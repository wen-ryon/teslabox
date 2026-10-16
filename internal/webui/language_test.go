package webui

import (
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/wen-ryon/teslabox/internal/uipreferences"
)

func TestLanguageSettingsAreSharedAndProtected(t *testing.T) {
	store := uipreferences.New(filepath.Join(t.TempDir(), "ui.json"))
	auth := &carplayAuth{}
	handler := NewHandler(http.NotFoundHandler(), auth, slog.New(slog.NewTextHandler(io.Discard, nil)), WithLanguageSettings(store))
	request := func(method, path, body, origin string) *httptest.ResponseRecorder {
		r := httptest.NewRequest(method, "http://teslabox"+path, strings.NewReader(body))
		if origin != "" {
			r.Header.Set("Origin", origin)
		}
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, r)
		return w
	}
	if w := request("GET", "/api/v1/ui/language", "", ""); w.Code != 200 || !strings.Contains(w.Body.String(), `"zh-CN"`) {
		t.Fatalf("登录页不能读取默认语言: %d %s", w.Code, w.Body.String())
	}
	if w := request("POST", "/api/v1/ui/language", `{"language":"en"}`, ""); w.Code != 401 {
		t.Fatal("未登录可以修改共享语言")
	}
	auth.authenticated = true
	auth.change = true
	if w := request("POST", "/api/v1/ui/language", `{"language":"en"}`, ""); w.Code != 428 {
		t.Fatal("未改默认密码可以修改共享语言")
	}
	auth.change = false
	if w := request("POST", "/api/v1/ui/language", `{"language":"en"}`, "http://evil.example"); w.Code != 403 {
		t.Fatal("跨来源可以修改共享语言")
	}
	for _, body := range []string{`{"language":"CN"}`, `{"language":"en","region":"US"}`, `{"language":"en"}{}`, `{"language":"<script>"}`, strings.Repeat(" ", 1024) + `{"language":"en"}`} {
		if w := request("POST", "/api/v1/ui/language", body, "http://teslabox"); w.Code != 400 {
			t.Fatalf("无效输入未拒绝: %d", w.Code)
		}
	}
	if w := request("POST", "/api/v1/ui/language", `{"language":"en"}`, "http://teslabox"); w.Code != 200 || w.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("保存失败: %d %s", w.Code, w.Body.String())
	}
	// 新请求重新读取板端文件，不使用单客户端 localStorage 作为共享状态。
	if w := request("GET", "/settings", "", ""); !strings.Contains(w.Body.String(), `<html lang="en" data-device-language="en" data-ui-font="medium">`) {
		t.Fatal("设备语言没有注入真实 HTML 根节点")
	}
	value, err := store.Read()
	if err != nil || value.Language != "en" {
		t.Fatalf("持久化丢失: %+v %v", value, err)
	}
}

func TestCarPlayUsesTheSameBoardLanguage(t *testing.T) {
	store := uipreferences.New(filepath.Join(t.TempDir(), "ui.json"))
	if _, err := store.Update(uipreferences.Settings{Language: "en"}); err != nil {
		t.Fatal(err)
	}
	handler := NewHandler(http.NotFoundHandler(), &carplayAuth{}, slog.New(slog.NewTextHandler(io.Discard, nil)), WithCarPlay(http.NotFoundHandler()), WithCarPlayHome("carplay.teslabox.online"), WithLanguageSettings(store))
	for _, path := range []string{"http://teslabox/carplay", "https://carplay.teslabox.online/"} {
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, httptest.NewRequest("GET", path, nil))
		if w.Code != 200 || !strings.Contains(w.Body.String(), `<html lang="en" data-device-language="en">`) || !strings.Contains(w.Body.String(), `/i18n.js`) {
			t.Fatalf("CarPlay 没有使用共享语言: %s %d", path, w.Code)
		}
	}
	for _, path := range []string{"/i18n.js", "/i18n-extra.js"} {
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, httptest.NewRequest("GET", path, nil))
		if w.Code != 200 || !strings.Contains(w.Header().Get("Content-Type"), "javascript") {
			t.Fatalf("翻译资源缺失: %s %d", path, w.Code)
		}
	}
}
