package api

import (
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/wen-ryon/teslabox/internal/carplaysettings"
)

func TestCarPlaySettingsRejectStaleBrowserAndApplySavedFrameRate(t *testing.T) {
	manager, err := carplaysettings.New(filepath.Join(t.TempDir(), "settings.json"))
	if err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	registerCarPlaySettingsRoutes(mux, manager)
	for _, test := range []struct {
		method, path, body string
		status             int
	}{
		{"PUT", "/api/v1/carplay/settings", `{"revision":1,"network":"wifi","quality":1080,"fps":60}`, 200},
		{"POST", "/api/v1/carplay/display", `{"revision":1,"width":1280,"height":720}`, 422},
		{"POST", "/api/v1/carplay/display", `{"revision":2,"width":1800,"height":1000}`, 200},
		{"PUT", "/api/v1/carplay/settings", `{"revision":1,"network":"auto","quality":720,"fps":30}`, 422},
	} {
		r := httptest.NewRecorder()
		mux.ServeHTTP(r, httptest.NewRequest(test.method, test.path, strings.NewReader(test.body)))
		if r.Code != test.status {
			t.Fatalf("%s %s: %d %s", test.method, test.path, r.Code, r.Body.String())
		}
		if test.path == "/api/v1/carplay/display" && r.Code == 200 && !strings.Contains(r.Body.String(), `"fps":60`) {
			t.Fatal("没有采用保存后的帧率")
		}
	}
}

func TestCarPlayControlAndRecoverySurviveLegacyPageSave(t *testing.T) {
	manager, err := carplaysettings.New(filepath.Join(t.TempDir(), "settings.json"))
	if err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	registerCarPlaySettingsRoutes(mux, manager)
	for _, test := range []struct{ name, body string }{
		{"关闭并保持画面优先", `{"revision":1,"phone_id":"02:00:00:00:00:01","enabled":false,"auto_play":true,"recovery_priority":"carplay","dashboard_layout":"vertical","network":"wifi","quality":720,"fps":30}`},
		{"旧页面不能重置新策略", `{"revision":2,"network":"wifi","quality":720,"fps":60}`},
	} {
		t.Run(test.name, func(t *testing.T) {
			reply := httptest.NewRecorder()
			mux.ServeHTTP(reply, httptest.NewRequest("PUT", "/api/v1/carplay/settings", strings.NewReader(test.body)))
			if reply.Code != 200 || manager.Settings().Enabled || !manager.Settings().AutoPlay || manager.Settings().RecoveryPriority != "carplay" || manager.Settings().PhoneID != "02:00:00:00:00:01" || manager.Settings().DashboardLayout != "vertical" {
				t.Fatalf("策略意外改变: %d %s", reply.Code, reply.Body.String())
			}
		})
	}
}
