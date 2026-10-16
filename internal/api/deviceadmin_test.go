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
	statusErr         error
	scanErr           error
	autoConnectUpdate deviceadmin.AutoConnectUpdate
	ipv4Update        deviceadmin.IPv4Update
	apUpdate          deviceadmin.APUpdate
	repairRequested   bool
	powerAction       string
	disconnectedUUID  string
}

func (a *fakeDeviceAdmin) WiFiStatus(context.Context) (deviceadmin.WiFiStatus, error) {
	return a.status, a.statusErr
}
func (a *fakeDeviceAdmin) Scan(context.Context) ([]deviceadmin.AccessPoint, error) {
	return []deviceadmin.AccessPoint{}, a.scanErr
}
func (a *fakeDeviceAdmin) Connect(context.Context, deviceadmin.ConnectRequest) error { return nil }
func (a *fakeDeviceAdmin) SetAutoConnect(_ context.Context, update deviceadmin.AutoConnectUpdate) error {
	a.autoConnectUpdate = update
	return nil
}
func (a *fakeDeviceAdmin) UpdateIPv4(_ context.Context, update deviceadmin.IPv4Update) error {
	a.ipv4Update = update
	return nil
}
func (a *fakeDeviceAdmin) Forget(context.Context, string) error { return nil }

func (a *fakeDeviceAdmin) DisconnectWiFiProfile(_ context.Context, uuid string) error {
	a.disconnectedUUID = uuid
	return nil
}
func (a *fakeDeviceAdmin) APSettings(context.Context) (deviceadmin.APSettings, error) {
	return deviceadmin.APSettings{
		Available: true, Interface: "ap0", InterfaceRole: "AP 虚拟接口", MACAddress: "AA:BB:CC:DD:EE:20",
		SSID: "TeslaBox-5G", Address: "192.168.88.1/24", ActiveBand: "bg", ActiveChannel: 1,
		ConcurrencyMode: "same_channel", MaxConcurrentChannels: 1, CapabilityNotice: "热点必须与上游 WiFi 使用同一信道。",
		AllowedChannels: []int{1, 6, 11, 149, 161},
	}, nil
}
func (a *fakeDeviceAdmin) UpdateAP(_ context.Context, update deviceadmin.APUpdate) error {
	a.apUpdate = update
	return nil
}
func (a *fakeDeviceAdmin) RepairAPInterface(context.Context) error {
	a.repairRequested = true
	return nil
}
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
	if ap.Code != http.StatusOK || !strings.Contains(ap.Body.String(), `"interface":"ap0"`) ||
		!strings.Contains(ap.Body.String(), `"interface_role":"AP 虚拟接口"`) ||
		!strings.Contains(ap.Body.String(), `"active_channel":1`) ||
		!strings.Contains(ap.Body.String(), `"allowed_channels":[1,6,11,149,161]`) ||
		!strings.Contains(ap.Body.String(), `"concurrency_mode":"same_channel"`) ||
		!strings.Contains(ap.Body.String(), `"max_concurrent_channels":1`) {
		t.Fatalf("ap status = %d, body = %s", ap.Code, ap.Body.String())
	}

	repair := httptest.NewRecorder()
	handler.ServeHTTP(repair, httptest.NewRequest(http.MethodPost, "/api/v1/network/ap/repair", nil))
	if repair.Code != http.StatusAccepted || !admin.repairRequested {
		t.Fatalf("repair status = %d, requested = %t", repair.Code, admin.repairRequested)
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

	ipv4 := httptest.NewRecorder()
	ipv4Request := httptest.NewRequest(
		http.MethodPost,
		"/api/v1/network/wifi/ipv4",
		strings.NewReader(`{"uuid":"11111111-1111-1111-1111-111111111111","mode":"static","address":"192.0.2.20","subnet_mask":"255.255.255.0","gateway":"192.0.2.1","dns":["223.5.5.5","1.1.1.1"]}`),
	)
	handler.ServeHTTP(ipv4, ipv4Request)
	if ipv4.Code != http.StatusOK || admin.ipv4Update.Mode != "static" || len(admin.ipv4Update.DNS) != 2 {
		t.Fatalf("ipv4 status = %d, update = %#v", ipv4.Code, admin.ipv4Update)
	}

	power := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/api/v1/system/power", strings.NewReader(`{"action":"reboot"}`))
	handler.ServeHTTP(power, request)
	if power.Code != http.StatusAccepted || admin.powerAction != "reboot" {
		t.Fatalf("power status = %d, action = %q", power.Code, admin.powerAction)
	}
}

func TestDeviceAdminAcceptsAPSecuritySelection(t *testing.T) {
	t.Parallel()
	for _, mode := range []string{"", "wpa2", "wpa3"} {
		name := mode
		if name == "" {
			name = "old client"
		}
		t.Run(name, func(t *testing.T) {
			admin := &fakeDeviceAdmin{}
			handler := NewHandler(fakeController{}, slog.New(slog.NewTextHandler(io.Discard, nil)), WithDeviceAdmin(admin))
			body := `{"ssid":"TeslaBox","band":"a","channel":44`
			if mode != "" {
				body += `,"security_mode":"` + mode + `"`
			}
			body += `}`
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, httptest.NewRequest(http.MethodPost, "/api/v1/network/ap", strings.NewReader(body)))
			if response.Code != http.StatusOK || admin.apUpdate.SecurityMode != mode {
				t.Fatalf("status = %d, update = %+v, body = %s", response.Code, admin.apUpdate, response.Body.String())
			}
		})
	}
}

func TestWriteDeviceAdminErrorReturnsStructuredAPReason(t *testing.T) {
	t.Parallel()
	recorder := httptest.NewRecorder()
	writeDeviceAdminError(recorder, slog.New(slog.NewTextHandler(io.Discard, nil)), "开启热点失败", &deviceadmin.APOperationError{
		Code: "channel_requires_dfs", Message: "热点信道 44 当前不可用", Suggestion: "请选择 149 或 161 信道。",
	})
	if recorder.Code != http.StatusConflict ||
		!strings.Contains(recorder.Body.String(), `"reason_code":"channel_requires_dfs"`) ||
		!strings.Contains(recorder.Body.String(), `"suggestion":"请选择 149 或 161 信道。"`) {
		t.Fatalf("status = %d, body = %s", recorder.Code, recorder.Body.String())
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

func TestDeviceAdminKeepsCoreStatusUsableWithoutWiFi(t *testing.T) {
	t.Parallel()
	admin := &fakeDeviceAdmin{
		status: deviceadmin.WiFiStatus{
			Available:         false,
			SingleRadioNotice: "未检测到可用的无线网卡；热点和 WiFi 管理不可用，但 TeslaUSB、媒体与有线管理不受影响。",
		},
		scanErr: deviceadmin.ErrWiFiUnavailable,
	}
	handler := NewHandler(fakeController{}, slog.New(slog.NewTextHandler(io.Discard, nil)), WithDeviceAdmin(admin))

	status := httptest.NewRecorder()
	handler.ServeHTTP(status, httptest.NewRequest(http.MethodGet, "/api/v1/network/wifi", nil))
	if status.Code != http.StatusOK || !strings.Contains(status.Body.String(), `"available":false`) {
		t.Fatalf("wifi status = %d, body = %s", status.Code, status.Body.String())
	}

	scan := httptest.NewRecorder()
	handler.ServeHTTP(scan, httptest.NewRequest(http.MethodPost, "/api/v1/network/wifi/scan", nil))
	if scan.Code != http.StatusServiceUnavailable || !strings.Contains(scan.Body.String(), "有线管理不受影响") {
		t.Fatalf("wifi scan = %d, body = %s", scan.Code, scan.Body.String())
	}
}

func TestDeviceAdminAcceptsExplicitAPCarPlayConfirmation(t *testing.T) {
	admin := &fakeDeviceAdmin{}
	h := NewHandler(fakeController{}, slog.New(slog.NewTextHandler(io.Discard, nil)), WithDeviceAdmin(admin))
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest("POST", "/api/v1/network/ap", strings.NewReader(`{"ssid":"TeslaBox","band":"a","channel":161,"enabled":true,"confirm_carplay_reconnect":true}`)))
	if w.Code != http.StatusOK || !admin.apUpdate.ConfirmCarPlayReconnect || admin.apUpdate.SSID != "TeslaBox" {
		t.Fatalf("确认授权没有传入 AP 操作：%d %s %+v", w.Code, w.Body.String(), admin.apUpdate)
	}
}
