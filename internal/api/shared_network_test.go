package api

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/wen-ryon/teslabox/internal/deviceadmin"
)

type sharedAPIFake struct {
	fakeDeviceAdmin
	err error
}

func (f *sharedAPIFake) SharedNetworks(context.Context) (deviceadmin.SharedNetworkStatus, error) {
	return deviceadmin.SharedNetworkStatus{Devices: []deviceadmin.SharedNetworkDevice{{Interface: "bnep0", Kind: "bluetooth", State: "connecting", StateCode: 70}}}, f.err
}

func TestSharedNetworkRouteDoesNotOfferMutations(t *testing.T) {
	admin := &sharedAPIFake{}
	handler := NewHandler(fakeController{}, slog.New(slog.NewTextHandler(io.Discard, nil)), WithDeviceAdmin(admin))
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, httptest.NewRequest("GET", "/api/v1/network/shared", nil))
	if w.Code != 200 || w.Header().Get("Cache-Control") != "no-store" || !strings.Contains(w.Body.String(), `"state":"connecting"`) {
		t.Fatalf("错误响应 %d %s", w.Code, w.Body.String())
	}
	w = httptest.NewRecorder()
	handler.ServeHTTP(w, httptest.NewRequest("POST", "/api/v1/network/shared", nil))
	if w.Code < 400 {
		t.Fatal("不能经状态入口控制或配对")
	}
	admin.err = errors.New("系统总线不可用")
	w = httptest.NewRecorder()
	handler.ServeHTTP(w, httptest.NewRequest("GET", "/api/v1/network/shared", nil))
	if w.Code < 400 {
		t.Fatal("读取失败不能伪装成空设备成功")
	}
}
