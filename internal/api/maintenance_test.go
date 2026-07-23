package api

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/wen-ryon/teslabox/internal/maintenancemode"
	"github.com/wen-ryon/teslabox/internal/mediaassets"
	"github.com/wen-ryon/teslabox/internal/updater"
)

type fakeMaintenanceAutomation struct{ settings maintenancemode.Settings }

func (a *fakeMaintenanceAutomation) Settings() maintenancemode.Settings { return a.settings }

func (a *fakeMaintenanceAutomation) SetAutoRecovery(enabled bool) (maintenancemode.Settings, error) {
	a.settings.AutoRecovery = enabled
	return a.settings, nil
}

type fakeMediaAssetsAdmin struct{ applyCalls int }

func (a *fakeMediaAssetsAdmin) Status(context.Context) (mediaassets.Status, error) {
	return mediaassets.Status{}, nil
}

func (a *fakeMediaAssetsAdmin) UploadStream(context.Context, mediaassets.Category, func(func(mediaassets.UploadFile) error) error) ([]mediaassets.Asset, error) {
	return nil, nil
}

func (a *fakeMediaAssetsAdmin) Delete(context.Context, string) (mediaassets.Status, error) {
	return mediaassets.Status{}, nil
}

func (a *fakeMediaAssetsAdmin) DeleteExternal(context.Context, mediaassets.Asset) (mediaassets.Status, error) {
	return mediaassets.Status{}, nil
}

func (a *fakeMediaAssetsAdmin) Apply(context.Context) (mediaassets.Status, error) {
	a.applyCalls++
	return mediaassets.Status{}, nil
}

func (a *fakeMediaAssetsAdmin) OpenPreview(context.Context, string) (*os.File, mediaassets.Asset, error) {
	return nil, mediaassets.Asset{}, mediaassets.ErrPreviewUnavailable
}

func TestMaintenanceSettingRequiresRiskConfirmation(t *testing.T) {
	automation := &fakeMaintenanceAutomation{}
	handler := NewHandler(fakeController{}, slog.New(slog.NewTextHandler(io.Discard, nil)), WithMaintenanceAutomation(automation))

	missing := httptest.NewRecorder()
	handler.ServeHTTP(missing, httptest.NewRequest(http.MethodPost, "/api/v1/maintenance", strings.NewReader(`{"auto_recovery":true}`)))
	if missing.Code != http.StatusBadRequest || automation.Settings().AutoRecovery {
		t.Fatalf("unconfirmed setting = %d, %#v", missing.Code, automation.Settings())
	}

	confirmed := httptest.NewRecorder()
	handler.ServeHTTP(confirmed, httptest.NewRequest(http.MethodPost, "/api/v1/maintenance", strings.NewReader(`{"auto_recovery":true,"confirm_risk":true}`)))
	if confirmed.Code != http.StatusOK || !automation.Settings().AutoRecovery {
		t.Fatalf("confirmed setting = %d, %#v", confirmed.Code, automation.Settings())
	}
}

func TestAutomaticMaintenanceAppliesMediaAndSchedulesReboot(t *testing.T) {
	previousDelay := automaticMaintenanceDisconnectDelay
	automaticMaintenanceDisconnectDelay = 0
	t.Cleanup(func() { automaticMaintenanceDisconnectDelay = previousDelay })

	automation := &fakeMaintenanceAutomation{settings: maintenancemode.Settings{AutoRecovery: true}}
	assets := &fakeMediaAssetsAdmin{}
	gadget := &fakeGadgetAdmin{}
	power := &fakeDeviceAdmin{}
	handler := NewHandler(
		fakeController{}, slog.New(slog.NewTextHandler(io.Discard, nil)),
		WithMediaAssetsAdmin(assets), WithGadgetAdmin(gadget), WithDeviceAdmin(power), WithMaintenanceAutomation(automation),
	)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, httptest.NewRequest(http.MethodPost, "/api/v1/media-assets/apply", strings.NewReader(`{}`)))
	if response.Code != http.StatusOK || assets.applyCalls != 1 || gadget.pauseCalls != 1 || gadget.resumeCalls != 1 || power.powerAction != "reboot" {
		t.Fatalf("apply = %d, calls = apply:%d pause:%d resume:%d power:%q", response.Code, assets.applyCalls, gadget.pauseCalls, gadget.resumeCalls, power.powerAction)
	}
}

func TestAutomaticMaintenanceInstallsUpdateAndPreparesWarmReboot(t *testing.T) {
	previousDelay := automaticMaintenanceDisconnectDelay
	automaticMaintenanceDisconnectDelay = 0
	t.Cleanup(func() { automaticMaintenanceDisconnectDelay = previousDelay })

	automation := &fakeMaintenanceAutomation{settings: maintenancemode.Settings{UpdateAutoRecovery: true}}
	update := &fakeUpdateAdmin{status: updater.Status{Configured: true, State: updater.StateDownloaded}}
	gadget := &fakeGadgetAdmin{}
	power := &fakeDeviceAdmin{}
	handler := NewHandler(
		fakeController{}, slog.New(slog.NewTextHandler(io.Discard, nil)),
		WithUpdateAdmin(update), WithGadgetAdmin(gadget), WithDeviceAdmin(power), WithMaintenanceAutomation(automation),
	)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, httptest.NewRequest(http.MethodPost, "/api/v1/update/install", nil))
	if response.Code != http.StatusAccepted || gadget.pauseCalls != 1 || gadget.resumeCalls != 1 || power.powerAction != "" {
		t.Fatalf("install = %d, pause:%d resume:%d power:%q", response.Code, gadget.pauseCalls, gadget.resumeCalls, power.powerAction)
	}
}
