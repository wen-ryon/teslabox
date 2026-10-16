package api

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http/httptest"
	"testing"

	"github.com/wen-ryon/teslabox/internal/deviceadmin"
)

type wifiDetailsTestAdmin struct {
	fakeDeviceAdmin
	seen deviceadmin.WiFiStatus
}

func (a *wifiDetailsTestAdmin) WiFiDetails(_ context.Context, status deviceadmin.WiFiStatus) deviceadmin.WiFiDetails {
	a.seen = status
	return deviceadmin.WiFiDetails{Available: true, BitrateKbps: 866700, Gateways: []string{"192.168.1.1"}}
}

func TestWiFiStatusDetailsPreservesStatusAndOptionalCapability(t *testing.T) {
	for _, supported := range []bool{false, true} {
		t.Run(map[bool]string{false: "兼容旧实现", true: "读取运行信息"}[supported], func(t *testing.T) {
			base := fakeDeviceAdmin{status: deviceadmin.WiFiStatus{Available: true, Interface: "sta0", CurrentConnection: "Home", ConnectionState: "connected", ConnectionUUID: "identity"}}
			a := &wifiDetailsTestAdmin{fakeDeviceAdmin: base}
			var admin DeviceAdmin = &base
			if supported {
				admin = a
			}
			h := NewHandler(fakeController{}, slog.New(slog.NewTextHandler(io.Discard, nil)), WithDeviceAdmin(admin))
			w := httptest.NewRecorder()
			h.ServeHTTP(w, httptest.NewRequest("GET", "/api/v1/network/wifi", nil))
			var body struct {
				deviceadmin.WiFiStatus
				Details deviceadmin.WiFiDetails `json:"details"`
			}
			if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil || w.Code != 200 {
				t.Fatalf("响应异常 %d %s: %v", w.Code, w.Body.String(), err)
			}
			if body.Interface != "sta0" || body.CurrentConnection != "Home" || body.Details.Available != supported {
				t.Fatalf("破坏旧状态或伪造详情：%+v", body)
			}
			if supported && (a.seen.ConnectionUUID != "identity" || body.Details.BitrateKbps != 866700) {
				t.Fatal("详情未关联到当前连接")
			}
		})
	}
}
