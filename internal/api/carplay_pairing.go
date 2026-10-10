package api

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"time"

	"github.com/wen-ryon/teslabox/internal/carplaypairing"
)

// WithCarPlayPairing 使用应用生命周期启动任务，HTTP 返回不会中断配对和自有资源清理。
func WithCarPlayPairing(manager *carplaypairing.Manager, parent context.Context) Option {
	return func(options *handlerOptions) { options.carPlayPairing, options.carPlayPairingContext = manager, parent }
}

func pairingInput(w http.ResponseWriter, r *http.Request, fields ...string) (map[string]string, bool) {
	fail := func() (map[string]string, bool) {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "配对请求格式无效"})
		return nil, false
	}
	if r.URL.RawQuery != "" || r.URL.RawPath != "" {
		return fail()
	}
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, 512))
	token, err := decoder.Token()
	if err != nil || token != json.Delim('{') {
		return fail()
	}
	allowed := make(map[string]bool, len(fields))
	for _, field := range fields {
		allowed[field] = true
	}
	input := make(map[string]string, len(fields))
	for decoder.More() {
		field, err := decoder.Token()
		name, ok := field.(string)
		if err != nil || !ok || !allowed[name] {
			return fail()
		}
		if _, duplicate := input[name]; duplicate {
			return fail()
		}
		var value string
		if decoder.Decode(&value) != nil {
			return fail()
		}
		input[name] = value
	}
	if token, err := decoder.Token(); err != nil || token != json.Delim('}') {
		return fail()
	}
	if decoder.Decode(&struct{}{}) != io.EOF || len(input) != len(fields) {
		return fail()
	}
	return input, true
}

func registerCarPlayPairingRoutes(mux *http.ServeMux, manager *carplaypairing.Manager, parent context.Context) {
	mux.HandleFunc("GET /api/v1/carplay/pairing", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		if r.URL.RawQuery != "" || r.URL.RawPath != "" {
			http.NotFound(w, r)
			return
		}
		status := carplaypairing.TaskStatus{State: "idle"}
		available := manager != nil && parent != nil && parent.Err() == nil && manager.Ready()
		if manager != nil {
			status = manager.Status()
		}
		writeJSON(w, http.StatusOK, map[string]any{"available": available, "scanning": manager != nil && manager.Scanning(), "task": status})
	})
	ready := func(w http.ResponseWriter) bool {
		w.Header().Set("Cache-Control", "no-store")
		if manager == nil || parent == nil || parent.Err() != nil {
			writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "CarPlay 首次登记尚未就绪"})
			return false
		}
		return true
	}
	result := func(w http.ResponseWriter, err error) {
		if err != nil {
			message := "配对任务已结束或暂不可操作"
			if errors.Is(err, carplaypairing.ErrDigitsMismatch) {
				message = "配对数字不一致，请核对后重试"
			}
			if errors.Is(err, carplaypairing.ErrBusy) {
				message = "上一轮配对或清理尚未结束"
			}
			writeJSON(w, http.StatusConflict, map[string]string{"error": message})
			return
		}
		writeJSON(w, http.StatusOK, manager.Status())
	}
	mux.HandleFunc("POST /api/v1/carplay/pairing/scan", func(w http.ResponseWriter, r *http.Request) {
		if !ready(w) {
			return
		}
		if _, ok := pairingInput(w, r); !ok {
			return
		}
		if !manager.Ready() {
			writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "CarPlay 首次登记尚未就绪"})
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
		defer cancel()
		stop := context.AfterFunc(parent, cancel)
		defer stop()
		devices, err := manager.Scan(ctx)
		if err != nil {
			result(w, err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"devices": devices})
	})
	mux.HandleFunc("POST /api/v1/carplay/pairing/start", func(w http.ResponseWriter, r *http.Request) {
		if !ready(w) {
			return
		}
		input, ok := pairingInput(w, r, "phone_id")
		if !ok {
			return
		}
		if !manager.Ready() {
			writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "CarPlay 首次登记尚未就绪"})
			return
		}
		status, err := manager.Start(parent, input["phone_id"], 120*time.Second)
		if err != nil {
			result(w, err)
			return
		}
		writeJSON(w, http.StatusAccepted, status)
	})
	mux.HandleFunc("POST /api/v1/carplay/pairing/confirm", func(w http.ResponseWriter, r *http.Request) {
		if !ready(w) {
			return
		}
		input, ok := pairingInput(w, r, "request_id", "digits")
		if !ok {
			return
		}
		result(w, manager.Confirm(input["request_id"], input["digits"]))
	})
	mux.HandleFunc("POST /api/v1/carplay/pairing/cancel", func(w http.ResponseWriter, r *http.Request) {
		if !ready(w) {
			return
		}
		input, ok := pairingInput(w, r, "request_id")
		if !ok {
			return
		}
		result(w, manager.Cancel(input["request_id"]))
	})
	mux.HandleFunc("POST /api/v1/carplay/pairing/reconcile", func(w http.ResponseWriter, r *http.Request) {
		if !ready(w) {
			return
		}
		input, ok := pairingInput(w, r, "request_id")
		if !ok {
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
		defer cancel()
		result(w, manager.Reconcile(ctx, input["request_id"]))
	})
}
