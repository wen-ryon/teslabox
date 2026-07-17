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
	status            deviceadmin.WiFiStatus
	autoConnectUpdate deviceadmin.AutoConnectUpdate
	powerAction       string
}

func (a *fakeDeviceAdmin) WiFiStatus(context.Context) (deviceadmin.WiFiStatus, error) {
	return a.status, nil
}
func (a *fakeDeviceAdmin) Scan(context.Context) ([]deviceadmin.AccessPoint, error) {
	return []deviceadmin.AccessPoint{}, nil
}
func (a *fakeDeviceAdmin) Connect(context.Context, deviceadmin.ConnectRequest) error { return nil }
func (a *fakeDeviceAdmin) SetAutoConnect(_ context.Context, update deviceadmin.AutoConnectUpdate) error {
	a.autoConnectUpdate = update
	return nil
}
func (a *fakeDeviceAdmin) Forget(context.Context, string) error { return nil }
func (a *fakeDeviceAdmin) APSettings(context.Context) (deviceadmin.APSettings, error) {
	return deviceadmin.APSettings{Available: true, Interface: "ap0", MACAddress: "AA:BB:CC:DD:EE:20", SSID: "TeslaBox-5G", Address: "192.168.88.1/24"}, nil
}
func (a *fakeDeviceAdmin) UpdateAP(context.Context, deviceadmin.APUpdate) error { return nil }
func (a *fakeDeviceAdmin) SchedulePower(action string) error {
	a.powerAction = action
	return nil
}

func TestDeviceAdminRoutes(t *testing.T) {
	t.Parallel()
	admin := &fakeDeviceAdmin{status: deviceadmin.WiFiStatus{Available: true, Interface: "wlan0", MACAddress: "AA:BB:CC:DD:EE:10", IPAddress: "192.0.2.28/24"}}
	handler := NewHandler(fakeController{}, slog.New(slog.NewTextHandler(io.Discard, nil)), WithDeviceAdmin(admin))

	wifi := httptest.NewRecorder()
	handler.ServeHTTP(wifi, httptest.NewRequest(http.MethodGet, "/api/v1/network/wifi", nil))
	if wifi.Code != http.StatusOK || !strings.Contains(wifi.Body.String(), `"mac_address":"AA:BB:CC:DD:EE:10"`) || !strings.Contains(wifi.Body.String(), `"ip_address":"192.0.2.28/24"`) {
		t.Fatalf("wifi status = %d, body = %s", wifi.Code, wifi.Body.String())
	}

	ap := httptest.NewRecorder()
	handler.ServeHTTP(ap, httptest.NewRequest(http.MethodGet, "/api/v1/network/ap", nil))
	if ap.Code != http.StatusOK || !strings.Contains(ap.Body.String(), `"interface":"ap0"`) || !strings.Contains(ap.Body.String(), `"mac_address":"AA:BB:CC:DD:EE:20"`) {
		t.Fatalf("ap status = %d, body = %s", ap.Code, ap.Body.String())
	}

	autoConnect := httptest.NewRecorder()
	autoConnectRequest := httptest.NewRequest(
		http.MethodPost,
		"/api/v1/network/wifi/autoconnect",
		strings.NewReader(`{"uuid":"11111111-1111-1111-1111-111111111111","enabled":true}`),
	)
	handler.ServeHTTP(autoConnect, autoConnectRequest)
	if autoConnect.Code != http.StatusOK || !admin.autoConnectUpdate.Enabled {
		t.Fatalf("autoconnect status = %d, update = %#v", autoConnect.Code, admin.autoConnectUpdate)
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
