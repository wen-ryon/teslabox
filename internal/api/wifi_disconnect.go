package api

import (
	"context"
	"log/slog"
	"net/http"
)

// WiFiDisconnectAdmin 将页面的指定连接断开与自动化的“断开当前上游”区分，避免旧页面误操作。
type WiFiDisconnectAdmin interface {
	DisconnectWiFiProfile(context.Context, string) error
}

func registerWiFiDisconnectRoute(mux *http.ServeMux, admin WiFiDisconnectAdmin, logger *slog.Logger) {
	mux.HandleFunc("POST /api/v1/network/wifi/disconnect", func(w http.ResponseWriter, r *http.Request) {
		var input struct {
			UUID string `json:"uuid"`
		}
		if !decodeJSON(w, r, &input) {
			return
		}
		if err := admin.DisconnectWiFiProfile(r.Context(), input.UUID); err != nil {
			writeDeviceAdminError(w, logger, "断开 WiFi 失败", err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]bool{"disconnected": true})
	})
}
