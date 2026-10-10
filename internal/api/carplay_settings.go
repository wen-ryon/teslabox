package api

import (
	"net/http"

	"github.com/wen-ryon/teslabox/internal/carplaysettings"
)

// WithCarPlaySettings 在现有管理认证边界中保存独立 CarPlay 设置。
func WithCarPlaySettings(manager *carplaysettings.Manager) Option {
	return func(options *handlerOptions) { options.carPlaySettings = manager }
}

func registerCarPlaySettingsRoutes(mux *http.ServeMux, manager *carplaysettings.Manager) {
	mux.HandleFunc("POST /api/v1/carplay/display", func(w http.ResponseWriter, r *http.Request) {
		var input struct {
			Revision uint64 `json:"revision"`
			Width    int    `json:"width"`
			Height   int    `json:"height"`
		}
		if !decodeJSON(w, r, &input) {
			return
		}
		display, err := manager.ApplyDisplay(input.Revision, input.Width, input.Height)
		if err != nil {
			writeJSON(w, http.StatusUnprocessableEntity, map[string]string{"error": err.Error()})
			return
		}
		writeJSON(w, http.StatusOK, display)
	})
	mux.HandleFunc("GET /api/v1/carplay/settings", func(w http.ResponseWriter, r *http.Request) { writeJSON(w, http.StatusOK, manager.Settings()) })
	mux.HandleFunc("PUT /api/v1/carplay/settings", func(w http.ResponseWriter, r *http.Request) {
		// 旧页面没有新增字段时保留设备当前策略，不意外关闭或切换 CarPlay 布局。
		current := manager.Settings()
		settings := carplaysettings.Settings{Enabled: current.Enabled, AutoPlay: current.AutoPlay, RecoveryPriority: current.RecoveryPriority, PhoneID: current.PhoneID, DashboardLayout: current.DashboardLayout}
		if !decodeJSON(w, r, &settings) {
			return
		}
		saved, err := manager.Save(settings)
		if err != nil {
			writeJSON(w, http.StatusUnprocessableEntity, map[string]string{"error": err.Error()})
			return
		}
		writeJSON(w, http.StatusOK, saved)
	})
}
