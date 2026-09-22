package automation

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/wen-ryon/teslabox/internal/deviceadmin"
)

type fakeAPAdmin struct {
	mu       sync.Mutex
	settings deviceadmin.APSettings
	updates  []deviceadmin.APUpdate
}

func (a *fakeAPAdmin) APSettings(context.Context) (deviceadmin.APSettings, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.settings, nil
}

func (a *fakeAPAdmin) SetAPRuntime(_ context.Context, enabled bool) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.updates = append(a.updates, deviceadmin.APUpdate{Enabled: enabled, AutoConnect: a.settings.AutoConnect})
	a.settings.Enabled = enabled
	a.settings.DesiredEnabled = enabled || a.settings.AutoConnect
	if enabled {
		a.settings.State = "running"
	} else {
		a.settings.State = "stopped"
	}
	return nil
}

func (a *fakeAPAdmin) ConfigureAPRadio(_ context.Context, band string, channel int) (deviceadmin.APSettings, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.settings.Band = band
	a.settings.Channel = channel
	return a.settings, nil
}

func TestAPConfigReadAndUpdate(t *testing.T) {
	admin := &fakeAPAdmin{settings: defaultAPSettings(false, false)}
	admin.settings.ConcurrencyMode = "same_channel"
	admin.settings.AllowedChannels = []int{1, 6, 11}
	service := newTestService(t, admin, time.Now)

	response := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPut, "/v1/network/ap/config", strings.NewReader(`{"band":"bg","channel":6}`))
	service.Handler().ServeHTTP(response, request)
	if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), `"channel":6`) {
		t.Fatalf("config update status = %d, body = %s", response.Code, response.Body.String())
	}
	response = httptest.NewRecorder()
	request = httptest.NewRequest(http.MethodGet, "/v1/network/ap/config", nil)
	service.Handler().ServeHTTP(response, request)
	if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), `"concurrency_mode":"same_channel"`) {
		t.Fatalf("config read status = %d, body = %s", response.Code, response.Body.String())
	}
}

func newTestService(t *testing.T, admin *fakeAPAdmin, now func() time.Time) *Service {
	t.Helper()
	service, err := New(admin, Config{
		SocketPath: "/run/teslabox/automation/test.sock",
		StatePath:  "/run/teslabox/automation/test-state.json",
		Logger:     slog.New(slog.NewTextHandler(io.Discard, nil)),
		Now:        now,
	})
	if err != nil {
		t.Fatal(err)
	}
	// 单元测试使用临时目录保存状态，不经过仅约束生产配置的 New 路径检查。
	service.config.StatePath = filepath.Join(t.TempDir(), "automation-ap.json")
	return service
}

func defaultAPSettings(enabled, desired bool) deviceadmin.APSettings {
	return deviceadmin.APSettings{
		Available: true, Enabled: enabled, DesiredEnabled: desired, State: "stopped",
		SSID: "TeslaBox-5G", Band: "a", Channel: 36, AutoConnect: desired,
	}
}

func TestLeaseStartsAndStopsOnlyManagedAP(t *testing.T) {
	now := time.Date(2026, 9, 21, 12, 0, 0, 0, time.UTC)
	admin := &fakeAPAdmin{settings: defaultAPSettings(false, false)}
	service := newTestService(t, admin, func() time.Time { return now })

	response := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPut, "/v1/network/ap/leases/gps-geofence", strings.NewReader(`{"ttl_seconds":60}`))
	service.Handler().ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("acquire status = %d, body = %s", response.Code, response.Body.String())
	}
	if len(admin.updates) != 1 || !admin.updates[0].Enabled || admin.updates[0].AutoConnect {
		t.Fatalf("acquire update = %+v", admin.updates)
	}

	response = httptest.NewRecorder()
	request = httptest.NewRequest(http.MethodDelete, "/v1/network/ap/leases/gps-geofence", nil)
	service.Handler().ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("release status = %d, body = %s", response.Code, response.Body.String())
	}
	if len(admin.updates) != 2 || admin.updates[1].Enabled {
		t.Fatalf("release updates = %+v", admin.updates)
	}
}

func TestLeaseDoesNotStopManuallyEnabledAP(t *testing.T) {
	now := time.Date(2026, 9, 21, 12, 0, 0, 0, time.UTC)
	admin := &fakeAPAdmin{settings: defaultAPSettings(true, true)}
	service := newTestService(t, admin, func() time.Time { return now })

	for _, operation := range []struct {
		method string
		body   string
	}{
		{method: http.MethodPut, body: `{"ttl_seconds":60}`},
		{method: http.MethodDelete},
	} {
		response := httptest.NewRecorder()
		request := httptest.NewRequest(operation.method, "/v1/network/ap/leases/gps-geofence", strings.NewReader(operation.body))
		service.Handler().ServeHTTP(response, request)
		if response.Code != http.StatusOK {
			t.Fatalf("%s status = %d, body = %s", operation.method, response.Code, response.Body.String())
		}
	}
	if len(admin.updates) != 0 {
		t.Fatalf("manual AP received updates = %+v", admin.updates)
	}
}

func TestExpiredLeaseStopsAPAndPersistsState(t *testing.T) {
	now := time.Date(2026, 9, 21, 12, 0, 0, 0, time.UTC)
	admin := &fakeAPAdmin{settings: defaultAPSettings(false, false)}
	service := newTestService(t, admin, func() time.Time { return now })
	if _, _, err := service.acquire(context.Background(), "gps-geofence", minimumLeaseTTL); err != nil {
		t.Fatal(err)
	}
	now = now.Add(minimumLeaseTTL + time.Second)
	service.expire(context.Background())
	if len(admin.updates) != 2 || admin.updates[1].Enabled {
		t.Fatalf("expiry updates = %+v", admin.updates)
	}
	content, err := io.ReadAll(mustOpen(t, service.config.StatePath))
	if err != nil {
		t.Fatal(err)
	}
	var state persistedState
	if err := json.Unmarshal(content, &state); err != nil {
		t.Fatal(err)
	}
	if state.ManagedActivation || len(state.Leases) != 0 {
		t.Fatalf("persisted state = %+v", state)
	}
}

func TestRestoreReactivatesUnexpiredManagedLease(t *testing.T) {
	now := time.Date(2026, 9, 21, 12, 0, 0, 0, time.UTC)
	admin := &fakeAPAdmin{settings: defaultAPSettings(false, false)}
	service := newTestService(t, admin, func() time.Time { return now })
	state := persistedState{
		ManagedActivation: true,
		Leases:            []lease{{Owner: "gps-geofence", ExpiresAt: now.Add(time.Minute)}},
	}
	content, err := json.Marshal(state)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(service.config.StatePath, content, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := service.restore(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(admin.updates) != 1 || !admin.updates[0].Enabled || admin.updates[0].AutoConnect {
		t.Fatalf("restore updates = %+v", admin.updates)
	}
}

func TestLeaseInputIsBounded(t *testing.T) {
	admin := &fakeAPAdmin{settings: defaultAPSettings(false, false)}
	service := newTestService(t, admin, time.Now)
	tests := []struct {
		path string
		body string
		want int
	}{
		{path: "/v1/network/ap/leases/bad%20owner", body: `{}`, want: http.StatusBadRequest},
		{path: "/v1/network/ap/leases/gps", body: `{"ttl_seconds":2}`, want: http.StatusUnprocessableEntity},
		{path: "/v1/network/ap/leases/gps", body: `{"ttl_seconds":60,"command":"reboot"}`, want: http.StatusBadRequest},
	}
	for _, test := range tests {
		response := httptest.NewRecorder()
		request := httptest.NewRequest(http.MethodPut, test.path, strings.NewReader(test.body))
		service.Handler().ServeHTTP(response, request)
		if response.Code != test.want {
			t.Errorf("%s => %d, want %d; body=%s", test.path, response.Code, test.want, response.Body.String())
		}
	}
}

func mustOpen(t *testing.T, path string) io.Reader {
	t.Helper()
	file, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = file.Close() })
	return file
}
