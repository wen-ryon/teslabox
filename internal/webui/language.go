package webui

import (
	"encoding/json"
	"io"
	"io/fs"
	"net/http"
	"strings"

	"github.com/wen-ryon/teslabox/internal/uipreferences"
)

// LanguageSettings 隔离界面偏好与网络、录像及系统 locale 配置。
type LanguageSettings interface {
	Read() (uipreferences.Settings, error)
	Update(uipreferences.Settings) (uipreferences.Settings, error)
}

// WithLanguageSettings 接入设备共享存储，登录页仅公开语言，不公开其他设备状态。
func WithLanguageSettings(store LanguageSettings) HandlerOption {
	return func(settings *handlerSettings) { settings.language = store }
}

func registerLanguageRoutes(mux *http.ServeMux, store LanguageSettings, auth Authenticator) {
	// 登录前也必须能显示设备语言；公开读取没有写权限，也不返回其他配置。
	mux.HandleFunc("GET /api/v1/ui/language", func(w http.ResponseWriter, r *http.Request) {
		value, err := store.Read()
		if err != nil {
			writeJSON(w, 503, map[string]string{"error": "无法读取设备语言设置"})
			return
		}
		w.Header().Set("Cache-Control", "no-store")
		writeJSON(w, 200, value)
	})
	mux.Handle("POST /api/v1/ui/language", requireAuth(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var input uipreferences.Settings
		decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1024))
		decoder.DisallowUnknownFields()
		var extra any
		if decoder.Decode(&input) != nil || decoder.Decode(&extra) != io.EOF || !uipreferences.ValidLanguage(input.Language) {
			writeJSON(w, 400, map[string]string{"error": "界面语言只支持 zh-CN 或 en"})
			return
		}
		value, err := store.Update(input)
		if err != nil {
			writeJSON(w, 500, map[string]string{"error": "无法保存设备语言设置"})
			return
		}
		w.Header().Set("Cache-Control", "no-store")
		writeJSON(w, 200, value)
	}), auth))
}

func serveLanguagePage(files fs.FS, name string, store LanguageSettings) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		content, err := fs.ReadFile(files, name)
		if err != nil {
			http.Error(w, "页面资源不可用", 500)
			return
		}
		language := "zh-CN"
		if store != nil {
			if value, err := store.Read(); err == nil && uipreferences.ValidLanguage(value.Language) {
				language = value.Language
			}
		}
		// 值只能来自枚举，不能把持久文件或请求参数原样注入 HTML。
		page := strings.Replace(string(content), `<html lang="zh-CN"`, `<html lang="`+language+`" data-device-language="`+language+`"`, 1)
		writeLocalizedPage(w, page, language)
	}
}
