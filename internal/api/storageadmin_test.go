package api

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/wen-ryon/teslabox/internal/fragmentation"
	"github.com/wen-ryon/teslabox/internal/storageadmin"
)

type fakeStorageAdmin struct {
	status  storageadmin.Status
	request storageadmin.InitializeRequest
}

func (a *fakeStorageAdmin) Status(context.Context) (storageadmin.Status, error) { return a.status, nil }
func (a *fakeStorageAdmin) Fragmentation(context.Context) (fragmentation.Report, error) {
	return fragmentation.Report{ExtentCount: 7598, Severity: fragmentation.SeverityCritical}, nil
}
func (a *fakeStorageAdmin) MigrateAdvertisedCapacity(context.Context, storageadmin.CapacityMigrationRequest) (storageadmin.CapacityMigrationResult, error) {
	return storageadmin.CapacityMigrationResult{Changed: true, FileSystemChecked: true, AdvertisedBytes: 128_000_000_000}, nil
}
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

	fragmentationStatus := httptest.NewRecorder()
	handler.ServeHTTP(fragmentationStatus, httptest.NewRequest(http.MethodGet, "/api/v1/storage/fragmentation", nil))
	if fragmentationStatus.Code != http.StatusOK || !strings.Contains(fragmentationStatus.Body.String(), `"extent_count":7598`) {
		t.Fatalf("fragmentation status = %d, body = %s", fragmentationStatus.Code, fragmentationStatus.Body.String())
	}

	defragment := httptest.NewRecorder()
	handler.ServeHTTP(defragment, httptest.NewRequest(http.MethodPost, "/api/v1/storage/defragment", nil))
	if defragment.Code != http.StatusNotFound {
		t.Fatalf("removed defragment route status = %d, body = %s", defragment.Code, defragment.Body.String())
	}

	migrate := httptest.NewRecorder()
	handler.ServeHTTP(migrate, httptest.NewRequest(http.MethodPost, "/api/v1/storage/migrate-capacity", strings.NewReader(`{"confirm_otg_disconnected":true}`)))
	if migrate.Code != http.StatusOK || !strings.Contains(migrate.Body.String(), `"advertised_bytes":128000000000`) {
		t.Fatalf("migrate status = %d, body = %s", migrate.Code, migrate.Body.String())
	}

	initialize := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/api/v1/storage/initialize", strings.NewReader(`{"stable_path":"/dev/disk/by-id/usb-test","confirm_serial":"SERIAL","confirm_erase":true,"confirm_otg_disconnected":true,"reinitialize":true,"music_enabled":true,"teslacam_percent":80}`))
	handler.ServeHTTP(initialize, request)
	if initialize.Code != http.StatusOK || admin.request.ConfirmSerial != "SERIAL" || !admin.request.ConfirmErase || !admin.request.ConfirmOTGDisconnected || !admin.request.Reinitialize || !admin.request.MusicEnabled || admin.request.TeslaCamPercent != 80 {
		t.Fatalf("status = %d, request = %#v", initialize.Code, admin.request)
	}
}
