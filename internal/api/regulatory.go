package api

import (
	"context"
	"log/slog"
	"net/http"

	"github.com/wen-ryon/teslabox/internal/regulatory"
)

// WirelessRegionAdmin 作为可选能力，兼容尚未接入地区管理的本机自动化及测试适配器。
type WirelessRegionAdmin interface {
	WirelessRegion(context.Context) (regulatory.Status, error)
	SetWirelessRegion(context.Context, string) (regulatory.Status, error)
}

func registerRegulatoryRoutes(mux *http.ServeMux, admin WirelessRegionAdmin, logger *slog.Logger) {
	mux.HandleFunc("GET /api/v1/network/regulatory", func(w http.ResponseWriter, r *http.Request) {
		status, err := admin.WirelessRegion(r.Context())
		if err != nil {
			writeDeviceAdminError(w, logger, "读取无线地区失败", err)
			return
		}
		writeJSON(w, http.StatusOK, status)
	})
	mux.HandleFunc("POST /api/v1/network/regulatory", func(w http.ResponseWriter, r *http.Request) {
		var input struct {
			Country string `json:"country"`
			Confirm bool   `json:"confirm_network_change"`
		}
		if !decodeJSON(w, r, &input) {
			return
		}
		if !input.Confirm {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "修改无线地区可能断开 WiFi 和热点，请确认通过有线网络操作"})
			return
		}
		status, err := admin.SetWirelessRegion(r.Context(), input.Country)
		if err != nil {
			writeDeviceAdminError(w, logger, "保存无线地区失败", err)
			return
		}
		writeJSON(w, http.StatusOK, status)
	})
}
