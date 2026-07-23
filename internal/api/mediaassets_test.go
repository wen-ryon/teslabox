package api

import (
	"bytes"
	"context"
	"image"
	"image/png"
	"io"
	"log/slog"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/wen-ryon/teslabox/internal/mediaassets"
)

type mediaAssetMaintenance struct{}

func (mediaAssetMaintenance) BeginMediaMaintenance(context.Context) (func(), error) {
	return func() {}, nil
}

type mediaAssetRunner struct{}

func (mediaAssetRunner) Run(context.Context, string, ...string) ([]byte, error) { return nil, nil }

func TestMediaAssetUploadRouteStreamsAndHidesStagingName(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	imagePath := filepath.Join(root, "images", "teslacam.img")
	if err := os.MkdirAll(filepath.Dir(imagePath), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(imagePath, []byte("image"), 0o600); err != nil {
		t.Fatal(err)
	}
	manager, err := mediaassets.New(mediaassets.Config{
		ImagePath: imagePath, StatePath: filepath.Join(root, "state"), StagingPath: filepath.Join(root, "state", "media-staging"), MountPath: filepath.Join(root, "mount"),
	}, mediaAssetRunner{}, mediaAssetMaintenance{})
	if err != nil {
		t.Fatal(err)
	}
	handler := NewHandler(fakeController{}, slog.New(slog.NewTextHandler(io.Discard, nil)), WithMediaAssetsAdmin(manager))

	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	part, err := writer.CreateFormFile("file", "Blue Wrap.png")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := part.Write(testPNG(t, 512, 512)); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodPost, "/api/v1/media-assets/upload?category=wrap", &body)
	request.Header.Set("Content-Type", writer.FormDataContentType())
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusCreated || strings.Contains(response.Body.String(), "staged") || !strings.Contains(response.Body.String(), "Blue Wrap.png") {
		t.Fatalf("upload = %d, %s", response.Code, response.Body.String())
	}

	status := httptest.NewRecorder()
	handler.ServeHTTP(status, httptest.NewRequest(http.MethodGet, "/api/v1/media-assets", nil))
	if status.Code != http.StatusOK || !strings.Contains(status.Body.String(), `"needs_apply":true`) {
		t.Fatalf("status = %d, %s", status.Code, status.Body.String())
	}

	directDelete := httptest.NewRecorder()
	directDeleteRequest := httptest.NewRequest(http.MethodPost, "/api/v1/media-assets/external-delete", strings.NewReader(`{"id":"0123456789abcdef0123456789abcdef","category":"music","files":[]}`))
	directDeleteRequest.Header.Set("Content-Type", "application/json")
	handler.ServeHTTP(directDelete, directDeleteRequest)
	if directDelete.Code != http.StatusBadRequest || !strings.Contains(directDelete.Body.String(), "OTG") {
		t.Fatalf("external delete without OTG confirmation = %d, %s", directDelete.Code, directDelete.Body.String())
	}
}

func testPNG(t *testing.T, width, height int) []byte {
	t.Helper()
	var output bytes.Buffer
	if err := png.Encode(&output, image.NewRGBA(image.Rect(0, 0, width, height))); err != nil {
		t.Fatal(err)
	}
	return output.Bytes()
}
