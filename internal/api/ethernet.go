package api

import (
	"context"
	"log/slog"
	"net/http"

	"github.com/wen-ryon/teslabox/internal/deviceadmin"
)

// EthernetAdmin 将普通有线设置与无线、企业组网及全局网络开关隔离。
type EthernetAdmin interface {
	EthernetStatus(context.Context) (deviceadmin.EthernetStatus, error)
	UpdateEthernet(context.Context, deviceadmin.EthernetUpdate) (*deviceadmin.EthernetTrial, error)
	ActEthernet(context.Context, deviceadmin.EthernetAction) (*deviceadmin.EthernetTrial, error)
	FinishEthernet(context.Context, string, bool) error
}

// WithEthernetAdmin 按能力注入，不改变现有无线管理接口或旧的测试实现。
func WithEthernetAdmin(admin EthernetAdmin) Option {
	return func(options *handlerOptions) { options.ethernetAdmin = admin }
}

func registerEthernetRoutes(mux *http.ServeMux, admin EthernetAdmin, logger *slog.Logger) {
	mux.HandleFunc("GET /api/v1/network/ethernet", func(w http.ResponseWriter, r *http.Request) {
		status, err := admin.EthernetStatus(r.Context())
		if err != nil {
			writeDeviceAdminError(w, logger, "读取有线网络失败", err)
			return
		}
		w.Header().Set("Cache-Control", "no-store")
		writeJSON(w, http.StatusOK, status)
	})
	mux.HandleFunc("POST /api/v1/network/ethernet", func(w http.ResponseWriter, r *http.Request) {
		var input deviceadmin.EthernetUpdate
		if !decodeJSON(w, r, &input) {
			return
		}
		trial, err := admin.UpdateEthernet(r.Context(), input)
		if err != nil {
			writeDeviceAdminError(w, logger, "保存有线网络失败", err)
			return
		}
		logger.Info("有线网络配置已提交", "interface", input.Interface, "uuid", input.UUID, "trial", trial != nil)
		writeEthernetResult(w, trial)
	})
	mux.HandleFunc("POST /api/v1/network/ethernet/action", func(w http.ResponseWriter, r *http.Request) {
		var input deviceadmin.EthernetAction
		if !decodeJSON(w, r, &input) {
			return
		}
		trial, err := admin.ActEthernet(r.Context(), input)
		if err != nil {
			writeDeviceAdminError(w, logger, "执行有线操作失败", err)
			return
		}
		logger.Info("有线网络操作已提交", "interface", input.Interface, "uuid", input.UUID, "action", input.Action)
		writeEthernetResult(w, trial)
	})
	mux.HandleFunc("POST /api/v1/network/ethernet/finish", func(w http.ResponseWriter, r *http.Request) {
		var input struct {
			Token string `json:"token"`
			Keep  *bool  `json:"keep"`
		}
		if !decodeJSON(w, r, &input) {
			return
		}
		if input.Token == "" || len(input.Token) > 128 || input.Keep == nil {
			writeJSON(w, http.StatusUnprocessableEntity, map[string]string{"error": "请提供试用标识及明确的确认／回退选择"})
			return
		}
		if err := admin.FinishEthernet(r.Context(), input.Token, *input.Keep); err != nil {
			writeDeviceAdminError(w, logger, "完成有线试用失败", err)
			return
		}
		logger.Info("有线网络试用已结束", "keep", *input.Keep)
		writeJSON(w, http.StatusOK, map[string]bool{"confirmed": *input.Keep, "rolled_back": !*input.Keep})
	})
}

func writeEthernetResult(w http.ResponseWriter, trial *deviceadmin.EthernetTrial) {
	code := http.StatusOK
	if trial != nil {
		code = http.StatusAccepted
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, code, struct {
		Saved bool                       `json:"saved"`
		Trial *deviceadmin.EthernetTrial `json:"trial,omitempty"`
	}{Saved: trial == nil, Trial: trial})
}
