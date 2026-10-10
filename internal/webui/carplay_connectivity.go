package webui

import (
	"log/slog"
	"net/http"
	"strconv"
	"sync"
	"time"
)

const carPlayOnlineBody = "<html>\n<head>\n</head>\n<body>\n</body>\n</html>\n\n"

func carPlayConnectivityHandler(logger *slog.Logger) http.HandlerFunc {
	var mu sync.Mutex
	var nextLog time.Time
	return func(w http.ResponseWriter, r *http.Request) {
		serveCarPlayConnectivity(w, r)
		if logger == nil || r.TLS != nil {
			return
		}
		// 检测入口没有认证，限频且只记录固定响应字段，不收集 URL、Cookie 或请求正文。
		mu.Lock()
		defer mu.Unlock()
		if now := time.Now(); !now.Before(nextLog) {
			nextLog = now.Add(30 * time.Second)
			logger.Info("CarPlay 车机联网检测已处理", "http_status", http.StatusOK,
				"online_header", true, "content_length", len(carPlayOnlineBody))
		}
	}
}

func serveCarPlayConnectivity(w http.ResponseWriter, r *http.Request) {
	// 只响应实车测得的明文检测；不伪造该主机的 TLS 或真实互联网状态。
	if r.TLS != nil {
		http.NotFound(w, r)
		return
	}
	// 旧版 ConnMan 按大小写精确查询此头；Header.Set 会把 Man 改成 man。
	w.Header()["X-ConnMan-Status"] = []string{"online"}
	w.Header().Set("Content-Type", "text/html")
	w.Header().Set("Content-Length", strconv.Itoa(len(carPlayOnlineBody)))
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusOK)
	if r.Method != http.MethodHead {
		_, _ = w.Write([]byte(carPlayOnlineBody))
	}
}
