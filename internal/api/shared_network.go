package api

import (
	"context"
	"log/slog"
	"net/http"

	"github.com/wen-ryon/teslabox/internal/deviceadmin"
)

// SharedNetworkAdmin 将设备发现与拨号、蓝牙配对及 CarPlay 的控制权分开。
type SharedNetworkAdmin interface {
	SharedNetworks(context.Context) (deviceadmin.SharedNetworkStatus, error)
}

func registerSharedNetworkRoutes(mux *http.ServeMux, admin SharedNetworkAdmin, logger *slog.Logger) {
	mux.HandleFunc("GET /api/v1/network/shared", func(w http.ResponseWriter, r *http.Request) {
		status, err := admin.SharedNetworks(r.Context())
		if err != nil {
			writeDeviceAdminError(w, logger, "读取移动与共享网络失败", err)
			return
		}
		w.Header().Set("Cache-Control", "no-store")
		writeJSON(w, http.StatusOK, status)
	})
}
