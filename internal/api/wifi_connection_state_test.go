package api

import (
	"errors"
	"io"
	"log/slog"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/wen-ryon/teslabox/internal/deviceadmin"
)

func TestWiFiConnectReplyUsesRuntimeStateAndRequestedIdentity(t *testing.T) {
	const uuid = "11111111-1111-4111-8111-111111111111"
	for _, item := range []struct {
		name, state, current, actual string
		statusErr                    error
		connected                    bool
		code                         int
	}{
		{name: "真实连接", state: "connected", current: "Home", actual: uuid, connected: true, code: 200},
		{name: "DHCP 进行中", state: "connecting", actual: uuid, code: 200},
		{name: "连接到了其他配置", state: "connected", current: "Office", actual: "22222222-2222-4222-8222-222222222222", code: 200},
		{name: "无法读取状态", statusErr: errors.New("读取失败"), code: 202},
	} {
		t.Run(item.name, func(t *testing.T) {
			admin := &fakeDeviceAdmin{status: deviceadmin.WiFiStatus{ConnectionState: item.state, CurrentConnection: item.current, ConnectionUUID: item.actual}, statusErr: item.statusErr}
			handler := NewHandler(fakeController{}, slog.New(slog.NewTextHandler(io.Discard, nil)), WithDeviceAdmin(admin))
			w := httptest.NewRecorder()
			handler.ServeHTTP(w, httptest.NewRequest("POST", "/api/v1/network/wifi/connect", strings.NewReader(`{"uuid":"`+uuid+`"}`)))
			want := `"connected":false`
			if item.connected {
				want = `"connected":true`
			}
			if w.Code != item.code || !strings.Contains(w.Body.String(), want) {
				t.Fatalf("连接请求被误报完成: %d %s", w.Code, w.Body.String())
			}
		})
	}
}
