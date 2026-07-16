package api

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/wen-ryon/teslabox/internal/deviceadmin"
)

type fakeDeviceAdmin struct {
	status      deviceadmin.WiFiStatus
	powerAction string
}

func (a *fakeDeviceAdmin) WiFiStatus(context.Context) (deviceadmin.WiFiStatus, error) {
	return a.status, nil
}
func (a *fakeDeviceAdmin) Scan(context.Context) ([]deviceadmin.AccessPoint, error) {
	return []deviceadmin.AccessPoint{}, nil
}
func (a *fakeDeviceAdmin) Connect(context.Context, deviceadmin.ConnectRequest) error { return nil }
func (a *fakeDeviceAdmin) Forget(context.Context, string) error                      { return nil }
func (a *fakeDeviceAdmin) APSettings(context.Context) (deviceadmin.APSettings, error) {
	return deviceadmin.APSettings{Available: true, SSID: "TeslaBox-5G"}, nil
}
func (a *fakeDeviceAdmin) UpdateAP(context.Context, deviceadmin.APUpdate) error { return nil }
func (a *fakeDeviceAdmin) SchedulePower(action string) error {
	a.powerAction = action
	return nil
}

func TestDeviceAdminRoutes(t *testing.T) {
	t.Parallel()
	admin := &fakeDeviceAdmin{status: deviceadmin.WiFiStatus{Available: true, Interface: "wlan0"}}
	handler := NewHandler(fakeController{}, slog.New(slog.NewTextHandler(io.Discard, nil)), WithDeviceAdmin(admin))

	wifi := httptest.NewRecorder()
	handler.ServeHTTP(wifi, httptest.NewRequest(http.MethodGet, "/api/v1/network/wifi", nil))
	if wifi.Code != http.StatusOK || !strings.Contains(wifi.Body.String(), `"interface":"wlan0"`) {
		t.Fatalf("wifi status = %d, body = %s", wifi.Code, wifi.Body.String())
	}

	power := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/api/v1/system/power", strings.NewReader(`{"action":"reboot"}`))
	handler.ServeHTTP(power, request)
	if power.Code != http.StatusAccepted || admin.powerAction != "reboot" {
		t.Fatalf("power status = %d, action = %q", power.Code, admin.powerAction)
	}
}

func TestDeviceAdminRejectsUnknownJSONField(t *testing.T) {
	t.Parallel()
	admin := &fakeDeviceAdmin{}
	handler := NewHandler(fakeController{}, slog.New(slog.NewTextHandler(io.Discard, nil)), WithDeviceAdmin(admin))
	response := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/api/v1/system/power", strings.NewReader(`{"action":"reboot","extra":true}`))
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, body = %s", response.Code, response.Body.String())
	}
}
