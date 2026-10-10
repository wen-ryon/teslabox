package carplaybundle

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

var installedRuntime string

// HealthHandler 仅在升级验收阶段等待本机 CarPlay 初始化，不阻塞控制接口或公网下载。
func HealthHandler(next http.Handler) http.Handler {
	if installedRuntime == "" {
		return next
	}
	root := installedRuntime
	ready := newReadinessProbe(func() string {
		return localServiceInstance(root, "/run/teslabox-carplay/installation-ready.json", "/proc")
	}, time.Now)
	return healthGate(next, ready)
}

func newReadinessProbe(instance func() string, now func() time.Time) func() bool {
	var mu sync.Mutex
	var observed string
	var since time.Time
	return func() bool {
		mu.Lock()
		defer mu.Unlock()
		current := instance()
		if current == "" {
			observed = ""
			since = time.Time{}
			return false
		}
		if current != observed {
			observed = current
			since = now()
			return false
		}
		// time.Now 自带单调时钟；不使用服务标记的墙钟，避免 NTP 跳变误报稳定。
		return now().Sub(since) >= 2*time.Second
	}
}

func healthGate(next http.Handler, ready func() bool) http.Handler {
	var accepted atomic.Bool
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/healthz" && !accepted.Load() {
			if !ready() {
				w.Header().Set("Content-Type", "application/json")
				w.Header().Set("Cache-Control", "no-store")
				w.WriteHeader(http.StatusServiceUnavailable)
				_, _ = w.Write([]byte("{\"status\":\"carplay_install_pending\"}\n"))
				return
			}
			accepted.Store(true)
		}
		next.ServeHTTP(w, r)
	})
}

func localServiceInstance(root, marker, proc string) string {
	info, err := os.Lstat(marker)
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0077 != 0 || info.Size() > 4096 {
		return ""
	}
	file, err := os.Open(marker)
	if err != nil {
		return ""
	}
	data, err := io.ReadAll(io.LimitReader(file, 4097))
	closeErr := file.Close()
	if err != nil || closeErr != nil || len(data) > 4096 {
		return ""
	}
	var status struct {
		PID        int    `json:"pid"`
		Root       string `json:"root"`
		StartTicks string `json:"start_ticks"`
	}
	if json.Unmarshal(data, &status) != nil || status.PID <= 1 || status.Root != root || status.StartTicks == "" {
		return ""
	}
	statData, err := os.ReadFile(filepath.Join(proc, strconv.Itoa(status.PID), "stat"))
	if err != nil {
		return ""
	}
	end := bytes.LastIndexByte(statData, ')')
	if end < 0 {
		return ""
	}
	fields := strings.Fields(string(statData[end+1:]))
	if len(fields) <= 19 || fields[0] == "Z" || fields[0] == "X" || fields[19] != status.StartTicks {
		return ""
	}
	command, err := os.ReadFile(filepath.Join(proc, strconv.Itoa(status.PID), "cmdline"))
	if err != nil {
		return ""
	}
	arguments := bytes.Split(command, []byte{0})
	wanted := []byte(filepath.Join(root, "python/carplay-service.py"))
	for _, argument := range arguments {
		if bytes.Equal(argument, wanted) {
			return strconv.Itoa(status.PID) + ":" + status.StartTicks
		}
	}
	return ""
}
