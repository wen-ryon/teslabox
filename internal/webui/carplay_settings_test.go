package webui

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestCarPlaySettingsUseManagementSessionAndSameOrigin(t *testing.T) {
	handler := newTestHandler(t)
	unauthorized := httptest.NewRecorder()
	handler.ServeHTTP(unauthorized, httptest.NewRequest("PUT", "http://teslabox.local/api/v1/carplay/settings", strings.NewReader(`{}`)))
	if unauthorized.Code != http.StatusUnauthorized {
		t.Fatalf("未登录保存: %d", unauthorized.Code)
	}
	login := httptest.NewRecorder()
	handler.ServeHTTP(login, httptest.NewRequest("POST", "http://teslabox.local/api/v1/auth/login", strings.NewReader(`{"password":"owner-password"}`)))
	if login.Code != http.StatusOK {
		t.Fatalf("登录: %d %s", login.Code, login.Body.String())
	}
	for _, test := range []struct {
		origin string
		want   int
	}{{"http://teslabox.local", 200}, {"https://foreign.invalid", 403}} {
		request := httptest.NewRequest("PUT", "http://teslabox.local/api/v1/carplay/settings", strings.NewReader(`{}`))
		request.Header.Set("Origin", test.origin)
		for _, cookie := range login.Result().Cookies() {
			request.AddCookie(cookie)
		}
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, request)
		if response.Code != test.want {
			t.Fatalf("来源 %s: %d %s", test.origin, response.Code, response.Body.String())
		}
	}
}
