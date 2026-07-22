package mediaassets

import (
	"bytes"
	"context"
	"image"
	"image/color"
	"image/png"
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/wen-ryon/teslabox/internal/system"
)

type fakeMaintenance struct{ calls int }

func (m *fakeMaintenance) BeginMediaMaintenance(context.Context) (func(), error) {
	m.calls++
	return func() {}, nil
}

type unusedRunner struct{}

func (unusedRunner) Run(context.Context, string, ...string) ([]byte, error) {
	return nil, nil
}

var _ system.Runner = unusedRunner{}

func TestManagerStagesValidWrapAndDoesNotExposeStagingPath(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	imagePath := filepath.Join(root, "images", "teslacam.img")
	if err := os.MkdirAll(filepath.Dir(imagePath), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(imagePath, []byte("image"), 0o600); err != nil {
		t.Fatal(err)
	}
	manager, err := New(Config{
		ImagePath: imagePath, StatePath: filepath.Join(root, "state"), StagingPath: filepath.Join(root, "state", "media-staging"), MountPath: filepath.Join(root, "mount"),
	}, unusedRunner{}, &fakeMaintenance{})
	if err != nil {
		t.Fatal(err)
	}
	assets, err := manager.Upload(t.Context(), CategoryWrap, []UploadFile{{FileName: "Blue Wrap.png", Content: validPNG(t, 512, 512)}})
	if err != nil {
		t.Fatalf("Upload() error = %v", err)
	}
	if len(assets) != 1 || !assets[0].Pending || !assets[0].PreviewAvailable || assets[0].Files[0].Staged != "" {
		t.Fatalf("public assets = %#v", assets)
	}
	preview, asset, err := manager.OpenPreview(t.Context(), assets[0].ID)
	if err != nil {
		t.Fatalf("OpenPreview() error = %v", err)
	}
	preview.Close()
	if asset.Files[0].Staged != "" {
		t.Fatalf("public preview asset leaked staging path: %#v", asset)
	}
	status, err := manager.Status(t.Context())
	if err != nil || !status.NeedsApply || len(status.Assets) != 1 {
		t.Fatalf("Status() = %#v, %v", status, err)
	}
}

func TestManagerRejectsInvalidWrapAndRemovesPartialStaging(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	manager, err := New(Config{
		ImagePath: filepath.Join(root, "images", "teslacam.img"), StatePath: filepath.Join(root, "state"), StagingPath: filepath.Join(root, "state", "media-staging"), MountPath: filepath.Join(root, "mount"),
	}, unusedRunner{}, &fakeMaintenance{})
	if err != nil {
		t.Fatal(err)
	}
	_, err = manager.Upload(t.Context(), CategoryWrap, []UploadFile{{FileName: "invalid.png", Content: validPNG(t, 511, 511)}})
	if err == nil {
		t.Fatal("expected invalid wrap dimensions to fail")
	}
	entries, readErr := os.ReadDir(filepath.Join(root, "state", "media-staging"))
	if readErr != nil && !os.IsNotExist(readErr) {
		t.Fatal(readErr)
	}
	if len(entries) != 0 {
		t.Fatalf("partial staging entries = %#v", entries)
	}
}

func TestManagerStagesPairedLightShowAndDeletesPendingAsset(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	manager, err := New(Config{
		ImagePath: filepath.Join(root, "images", "teslacam.img"), StatePath: filepath.Join(root, "state"), StagingPath: filepath.Join(root, "state", "media-staging"), MountPath: filepath.Join(root, "mount"),
	}, unusedRunner{}, &fakeMaintenance{})
	if err != nil {
		t.Fatal(err)
	}
	assets, err := manager.Upload(t.Context(), CategoryLightShow, []UploadFile{
		{Role: "sequence", FileName: "show-one.fseq", Content: bytes.NewBufferString("sequence")},
		{Role: "audio", FileName: "show-one.wav", Content: bytes.NewBufferString("audio")},
	})
	if err != nil || len(assets) != 1 || len(assets[0].Files) != 2 {
		t.Fatalf("Upload() = %#v, %v", assets, err)
	}
	status, err := manager.Delete(t.Context(), assets[0].ID)
	if err != nil || len(status.Assets) != 0 || status.NeedsApply {
		t.Fatalf("Delete() = %#v, %v", status, err)
	}
}

func TestNormalizeTeslaAssetNameAndEnsureRootLayout(t *testing.T) {
	t.Parallel()
	name, err := normalizeName(CategoryWrap, "我的 皮肤@01.png")
	if err != nil {
		t.Fatal(err)
	}
	if name != "01.png" {
		t.Fatalf("normalized wrap name = %q", name)
	}
	root := t.TempDir()
	if err := ensureTeslaMediaRootLayout(root); err != nil {
		t.Fatal(err)
	}
	for _, directory := range []string{"TeslaCam", "Boombox", "Wraps", "LightShow"} {
		info, err := os.Stat(filepath.Join(root, directory))
		if err != nil || !info.IsDir() {
			t.Fatalf("root layout %s = %#v, %v", directory, info, err)
		}
	}
}

func validPNG(t *testing.T, width, height int) io.Reader {
	t.Helper()
	var output bytes.Buffer
	canvas := image.NewRGBA(image.Rect(0, 0, width, height))
	canvas.Set(0, 0, color.White)
	if err := png.Encode(&output, canvas); err != nil {
		t.Fatal(err)
	}
	return bytes.NewReader(output.Bytes())
}
