package api

import (
	"context"
	"io"
	"log/slog"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/wen-ryon/teslabox/internal/deviceadmin"
)

type ethernetAPIFake struct {
	update deviceadmin.EthernetUpdate
	action deviceadmin.EthernetAction
	token  string
	keep   bool
	trial  *deviceadmin.EthernetTrial
}

func (a *ethernetAPIFake) EthernetStatus(context.Context) (deviceadmin.EthernetStatus, error) {
	return deviceadmin.EthernetStatus{Devices: []deviceadmin.EthernetDevice{}, Pending: a.trial}, nil
}
func (a *ethernetAPIFake) UpdateEthernet(_ context.Context, value deviceadmin.EthernetUpdate) (*deviceadmin.EthernetTrial, error) {
	a.update = value
	return a.trial, nil
}
func (a *ethernetAPIFake) ActEthernet(_ context.Context, value deviceadmin.EthernetAction) (*deviceadmin.EthernetTrial, error) {
	a.action = value
	return a.trial, nil
}
func (a *ethernetAPIFake) FinishEthernet(_ context.Context, token string, keep bool) error {
	a.token = token
	a.keep = keep
	return nil
}

func TestEthernetRoutesDistinguishSavedTrialAndConfirmation(t *testing.T) {
	admin := &ethernetAPIFake{}
	handler := NewHandler(fakeController{}, slog.New(slog.NewTextHandler(io.Discard, nil)), WithEthernetAdmin(admin))
	call := func(method, path, body string) *httptest.ResponseRecorder {
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, httptest.NewRequest(method, path, strings.NewReader(body)))
		return w
	}
	if w := call("GET", "/api/v1/network/ethernet", ""); w.Code != 200 || w.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("状态读取失败: %d", w.Code)
	}
	if w := call("POST", "/api/v1/network/ethernet", `{"interface":"eth0","uuid":"a","apply":false,"settings":{"name":"wired"}}`); w.Code != 200 || !strings.Contains(w.Body.String(), `"saved":true`) || admin.update.Interface != "eth0" {
		t.Fatalf("保存结果错误: %d %s", w.Code, w.Body.String())
	}
	admin.trial = &deviceadmin.EthernetTrial{Token: "opaque-token", Interface: "eth0"}
	if w := call("POST", "/api/v1/network/ethernet/action", `{"interface":"eth0","uuid":"a","action":"connect"}`); w.Code != 202 || !strings.Contains(w.Body.String(), `"saved":false`) || admin.action.Action != "connect" {
		t.Fatalf("误报连接已完成: %d %s", w.Code, w.Body.String())
	}
	for _, body := range []string{`{"token":"opaque-token"}`, `{"token":"","keep":true}`, `{"token":"opaque-token","keep":false,"path":"/arbitrary"}`} {
		if w := call("POST", "/api/v1/network/ethernet/finish", body); w.Code < 400 {
			t.Fatalf("无效确认未拒绝: %d", w.Code)
		}
		if admin.token != "" {
			t.Fatal("校验失败仍调用后端")
		}
	}
	if w := call("POST", "/api/v1/network/ethernet/finish", `{"token":"opaque-token","keep":true}`); w.Code != 200 || admin.token != "opaque-token" || !admin.keep {
		t.Fatalf("确认失败: %d", w.Code)
	}
}
