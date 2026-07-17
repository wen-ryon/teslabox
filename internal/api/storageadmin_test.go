package api

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/wen-ryon/teslabox/internal/storageadmin"
)

type fakeStorageAdmin struct {
	status  storageadmin.Status
	request storageadmin.InitializeRequest
}

func (a *fakeStorageAdmin) Status(context.Context) (storageadmin.Status, error) { return a.status, nil }
func (a *fakeStorageAdmin) Initialize(_ context.Context, request storageadmin.InitializeRequest) (storageadmin.Candidate, error) {
	a.request = request
	return storageadmin.Candidate{StablePath: request.StablePath, Initialized: true}, nil
}
func (a *fakeStorageAdmin) Cancel(context.Context, storageadmin.CancelRequest) error { return nil }

func TestStorageAdminRoutes(t *testing.T) {
	t.Parallel()
	admin := &fakeStorageAdmin{status: storageadmin.Status{Devices: []storageadmin.Candidate{{StablePath: "/dev/disk/by-id/usb-test", State: "uninitialized"}}}}
	handler := NewHandler(fakeController{}, slog.New(slog.NewTextHandler(io.Discard, nil)), WithStorageAdmin(admin))

	status := httptest.NewRecorder()
	handler.ServeHTTP(status, httptest.NewRequest(http.MethodGet, "/api/v1/storage/devices", nil))
	if status.Code != http.StatusOK || !strings.Contains(status.Body.String(), `"state":"uninitialized"`) {
		t.Fatalf("status = %d, body = %s", status.Code, status.Body.String())
	}

	initialize := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/api/v1/storage/initialize", strings.NewReader(`{"stable_path":"/dev/disk/by-id/usb-test","confirm_serial":"SERIAL","confirm_erase":true}`))
	handler.ServeHTTP(initialize, request)
	if initialize.Code != http.StatusOK || admin.request.ConfirmSerial != "SERIAL" || !admin.request.ConfirmErase {
		t.Fatalf("status = %d, request = %#v", initialize.Code, admin.request)
	}
}
