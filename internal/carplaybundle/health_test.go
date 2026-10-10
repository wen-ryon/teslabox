package carplaybundle

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

func TestHealthGateDoesNotBlockControlOrAcceptQueuedService(t *testing.T) {
	ready := false
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusOK) })
	handler := healthGate(next, func() bool { return ready })
	for _, test := range []struct {
		name, path string
		ready      bool
		code       int
	}{
		{"queued-service", "/healthz", false, http.StatusServiceUnavailable},
		{"management-still-available", "/api/v1/storage", false, http.StatusOK},
		{"local-service-ready", "/healthz", true, http.StatusOK},
		{"phone-leaving-is-not-ota-failure", "/healthz", false, http.StatusOK},
	} {
		t.Run(test.name, func(t *testing.T) {
			ready = test.ready
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, httptest.NewRequest("GET", test.path, nil))
			if response.Code != test.code {
				t.Fatalf("status=%d", response.Code)
			}
		})
	}
}

func TestLocalReadinessRejectsOldProcessOrWrongRuntime(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("Linux 私有权限和进程接口在目标平台验证")
	}
	root := t.TempDir()
	marker := filepath.Join(root, "ready.json")
	proc := filepath.Join(root, "proc")
	process := filepath.Join(proc, "123")
	if err := os.MkdirAll(process, 0700); err != nil {
		t.Fatal(err)
	}
	version := "/opt/teslabox/carplay/versions/a"
	for _, test := range []struct {
		name, runtime, command, state, recorded, actual string
		code                                            bool
	}{
		{"wrong-version", "/old", version + "/python/carplay-service.py", "S", "100", "100", false},
		{"unrelated-pid", version, "/usr/bin/other", "S", "100", "100", false},
		{"reused-pid-old-marker", version, version + "/python/carplay-service.py", "S", "100", "200", false},
		{"zombie", version, version + "/python/carplay-service.py", "Z", "100", "100", false},
		{"initialized-current-service", version, version + "/python/carplay-service.py", "S", "100", "100", true},
	} {
		t.Run(test.name, func(t *testing.T) {
			data, err := json.Marshal(map[string]any{"pid": 123, "root": test.runtime, "start_ticks": test.recorded, "ready_at_ms": -999999})
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(marker, data, 0600); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(process, "cmdline"), append([]byte("python3\x00"+test.command), 0), 0600); err != nil {
				t.Fatal(err)
			}
			fields := []string{test.state}
			for len(fields) < 19 {
				fields = append(fields, "0")
			}
			fields = append(fields, test.actual)
			statData := "123 (name with spaces) " + strings.Join(fields, " ")
			if err := os.WriteFile(filepath.Join(process, "stat"), []byte(statData), 0600); err != nil {
				t.Fatal(err)
			}
			if got := localServiceInstance(version, marker, proc) != ""; got != test.code {
				t.Fatalf("ready=%v", got)
			}
		})
	}
	if err := os.RemoveAll(process); err != nil {
		t.Fatal(err)
	}
	if localServiceInstance(version, marker, proc) != "" {
		t.Fatal("exited process accepted")
	}
}

func TestReadinessProbeObservesStableProcessAndResetsOnRestart(t *testing.T) {
	instance := "123:100"
	now := time.Now()
	probe := newReadinessProbe(func() string { return instance }, func() time.Time { return now })
	if probe() {
		t.Fatal("first observation accepted")
	}
	now = now.Add(time.Second)
	if probe() {
		t.Fatal("too early")
	}
	now = now.Add(time.Second)
	if !probe() {
		t.Fatal("stable process rejected")
	}
	instance = "123:200"
	if probe() {
		t.Fatal("reused pid accepted without new observation")
	}
	now = now.Add(2 * time.Second)
	if !probe() {
		t.Fatal("new stable generation rejected")
	}
	instance = ""
	if probe() {
		t.Fatal("dead process accepted")
	}
	instance = "123:200"
	if probe() {
		t.Fatal("missing interval was ignored")
	}
}
