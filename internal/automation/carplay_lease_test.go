package automation

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/wen-ryon/teslabox/internal/deviceadmin"
)

type fakeCarPlayCoordinator struct {
	*fakeAPAdmin
	held            *deviceadmin.CarPlayNetworkLease
	changes         int
	restoreCalls    int
	releaseAttempts int
	releaseFailure  error
	recoveryGuarded bool
}

func (a *fakeCarPlayCoordinator) GuardCarPlayRecovery() {
	a.recoveryGuarded = true
}

func TestCarPlayCorruptRecoveryIsolatesNetworkAndPreservesEvidence(t *testing.T) {
	admin := &fakeCarPlayCoordinator{fakeAPAdmin: &fakeAPAdmin{}}
	service := newCarPlayLeaseService(t, admin, time.Now)
	content := []byte(`{"version":1,"lease": "private-corrupt-evidence"}`)
	if err := os.WriteFile(service.carPlayStatePath(), content, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := service.restoreCarPlayProtection(); err != nil {
		t.Fatal(err)
	}
	if !admin.recoveryGuarded || !service.carPlayRecoveryRequired {
		t.Fatal("损坏记录没有隔离网络")
	}
	if err := service.restore(context.Background()); err != nil {
		t.Fatal("旧 AP 恢复阻止服务启动", err)
	}
	service.expire(context.Background())
	if response := leaseRequest(service, http.MethodPut, "carplay", `{"channels":[149]}`); response.Code != http.StatusConflict {
		t.Fatal("损坏期间允许新租约")
	}
	if response := leaseRequest(service, http.MethodDelete, "carplay", ""); response.Code != http.StatusConflict {
		t.Fatal("损坏期间覆盖恢复状态")
	}
	if _, _, err := service.acquire(context.Background(), "other", time.Minute); err == nil {
		t.Fatal("损坏期间允许旧 AP 租约")
	}
	response := httptest.NewRecorder()
	service.Handler().ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/v1/network/carplay/state", nil))
	if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), `"recovery_required":true`) || strings.Contains(response.Body.String(), "private-corrupt-evidence") {
		t.Fatal("诊断状态不可读或泄露原内容")
	}
	retained, err := os.ReadFile(service.carPlayStatePath())
	if err != nil || string(retained) != string(content) || admin.changes != 0 || admin.releaseAttempts != 0 {
		t.Fatal("损坏记录被覆盖或执行了网络修改")
	}
}

func (a *fakeCarPlayCoordinator) RestoreCarPlayNetworkHold(lease deviceadmin.CarPlayNetworkLease) error {
	a.restoreCalls++
	a.held = &lease
	return nil
}

func (a *fakeCarPlayCoordinator) AcquireCarPlayNetwork(_ context.Context, owner string,
	_ deviceadmin.CarPlayNetworkRequest, expiry time.Time, checkpoint func(*deviceadmin.CarPlayNetworkLease) error,
) (deviceadmin.CarPlayNetworkLease, error) {
	if a.held != nil && a.held.Owner != owner {
		return *a.held, carPlayLeaseBusy()
	}
	lease := deviceadmin.CarPlayNetworkLease{Owner: owner, ExpiresAt: expiry,
		DeviceAutoConnectManaged: true, OriginalDeviceAutoConnect: true,
		RuntimeAPUUID: "33333333-3333-4333-8333-333333333333",
		RuntimeAPName: "TeslaBox-CarPlay-lease-33333333-3333-4333-8333-333333333333",
		Plan: deviceadmin.CarPlayNetworkPlan{Channel: 149, ConcurrencyMode: "same_channel",
			APInterface: "ap0", APUUID: "11111111-1111-1111-1111-111111111111",
			UpstreamInterface: "wlan0", UpstreamUUID: "22222222-2222-2222-2222-222222222222", PauseUpstream: true}}
	if err := checkpoint(&lease); err != nil {
		return lease, err
	}
	a.held = &lease
	a.changes++
	return lease, nil
}

func (a *fakeCarPlayCoordinator) ReleaseCarPlayNetwork(_ context.Context, owner string, checkpoint func(*deviceadmin.CarPlayNetworkLease) error) error {
	a.releaseAttempts++
	if a.held != nil && a.held.Owner != owner {
		return carPlayLeaseBusy()
	}
	if a.releaseFailure != nil {
		return a.releaseFailure
	}
	if err := checkpoint(nil); err != nil {
		return err
	}
	a.held = nil
	return nil
}

func newCarPlayLeaseService(t *testing.T, admin *fakeCarPlayCoordinator, now func() time.Time) *Service {
	t.Helper()
	directory := t.TempDir()
	return &Service{admin: admin, config: Config{StatePath: filepath.Join(directory, "ap.json"),
		CarPlayStatePath: filepath.Join(directory, "carplay.json"), Now: now,
		CarPlayAccessDirectory: filepath.Join(directory, "access"),
		Logger:                 slog.New(slog.NewTextHandler(io.Discard, nil))}, leases: make(map[string]time.Time)}
}

func leaseRequest(service *Service, method, owner, body string) *httptest.ResponseRecorder {
	response := httptest.NewRecorder()
	service.Handler().ServeHTTP(response, httptest.NewRequest(method, "/v1/network/carplay/leases/"+owner, strings.NewReader(body)))
	return response
}

func TestCarPlayLeaseJournalSurvivesRestartAndExpiresWithRetry(t *testing.T) {
	now := time.Now()
	admin := &fakeCarPlayCoordinator{fakeAPAdmin: &fakeAPAdmin{}}
	service := newCarPlayLeaseService(t, admin, func() time.Time { return now })
	response := leaseRequest(service, http.MethodPut, "carplay", `{"channels":[149,157],"ttl_seconds":15}`)
	if response.Code != http.StatusOK || admin.changes != 1 || service.carPlayLease == nil {
		t.Fatalf("申请失败: %d %s", response.Code, response.Body.String())
	}
	content, err := os.ReadFile(service.carPlayStatePath())
	if err != nil || !strings.Contains(string(content), "22222222-2222-2222-2222-222222222222") {
		t.Fatalf("恢复身份未落盘: %v, %s", err, content)
	}
	restartedAdmin := &fakeCarPlayCoordinator{fakeAPAdmin: &fakeAPAdmin{}}
	restarted := newCarPlayLeaseService(t, restartedAdmin, func() time.Time { return now })
	restarted.config.CarPlayStatePath = service.carPlayStatePath()
	if err := restarted.restoreCarPlayHold(); err != nil || restartedAdmin.restoreCalls != 1 || restarted.carPlayLease == nil {
		t.Fatalf("重启丢失保护: %v", err)
	}
	if restartedAdmin.changes != 0 || restartedAdmin.releaseAttempts != 0 {
		t.Fatal("读取恢复记录就修改了网络")
	}
	now = now.Add(15 * time.Second)
	restartedAdmin.releaseFailure = errors.New("upstream not ready")
	restarted.expire(context.Background())
	if restartedAdmin.releaseAttempts != 1 || restarted.carPlayLease == nil {
		t.Fatal("到期恢复失败丢失租约")
	}
	content, _ = os.ReadFile(service.carPlayStatePath())
	var state carPlayPersistedState
	if err := json.Unmarshal(content, &state); err != nil || state.Lease == nil {
		t.Fatalf("恢复失败丢失磁盘记录: %v", err)
	}
	restartedAdmin.releaseFailure = nil
	restarted.expire(context.Background())
	if restartedAdmin.releaseAttempts != 2 || restarted.carPlayLease != nil {
		t.Fatal("到期重试未完成回收")
	}
	content, _ = os.ReadFile(service.carPlayStatePath())
	if err := json.Unmarshal(content, &state); err != nil || state.Lease != nil {
		t.Fatalf("恢复成功未清除记录: %v", err)
	}
}

func TestCarPlayLeaseWriteFailureDoesNotChangeNetwork(t *testing.T) {
	admin := &fakeCarPlayCoordinator{fakeAPAdmin: &fakeAPAdmin{}}
	service := newCarPlayLeaseService(t, admin, time.Now)
	if err := os.Mkdir(service.carPlayStatePath(), 0o700); err != nil {
		t.Fatal(err)
	}
	response := leaseRequest(service, http.MethodPut, "carplay", `{"channels":[149],"ttl_seconds":15}`)
	if response.Code < 400 || admin.changes != 0 || service.carPlayLease != nil {
		t.Fatalf("日志写入失败仍变更网络: %d, changes=%d", response.Code, admin.changes)
	}
}

func TestCarPlayLeaseDoesNotPreemptOtherAutomation(t *testing.T) {
	now := time.Now()
	admin := &fakeCarPlayCoordinator{fakeAPAdmin: &fakeAPAdmin{}}
	service := newCarPlayLeaseService(t, admin, func() time.Time { return now })
	service.leases["gps"] = now.Add(time.Minute)
	response := leaseRequest(service, http.MethodPut, "carplay", `{"channels":[149]}`)
	if response.Code != http.StatusConflict || admin.changes != 0 {
		t.Fatalf("抢占了其他租约: %d", response.Code)
	}
	delete(service.leases, "gps")
	response = leaseRequest(service, http.MethodPut, "carplay", `{"channels":[149]}`)
	if response.Code != http.StatusOK {
		t.Fatalf("申请失败: %s", response.Body.String())
	}
	response = leaseRequest(service, http.MethodDelete, "other", "")
	if response.Code != http.StatusConflict || admin.releaseAttempts != 0 {
		t.Fatal("其他来源释放了租约")
	}
	if _, _, err := service.acquire(context.Background(), "gps", time.Minute); err == nil || len(admin.updates) != 0 {
		t.Fatal("CarPlay 占用期间允许其他 AP 租约")
	}
}

func TestCarPlayLeaseRejectsInvalidRequestsAndJournal(t *testing.T) {
	admin := &fakeCarPlayCoordinator{fakeAPAdmin: &fakeAPAdmin{}}
	service := newCarPlayLeaseService(t, admin, time.Now)
	for _, body := range []string{`{"channels":[149],"ttl_seconds":14}`, `{"channels":[149],"ttl_seconds":3601}`,
		`{"channels":[149],"ttl_seconds":9223372036854775807}`, `{"channels":[149],"command":"reboot"}`, `{"channels":[149]} {}`} {
		response := leaseRequest(service, http.MethodPut, "carplay", body)
		if response.Code < 400 || admin.changes != 0 {
			t.Fatalf("接受无效请求: %s", body)
		}
	}
	for _, content := range []string{`{"version":2,"lease":null}`, `{"version":1,"lease":null} {}`, `{"version":1,"lease":null,"command":"reboot"}`, strings.Repeat("x", 16385)} {
		if err := os.WriteFile(service.carPlayStatePath(), []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := service.restoreCarPlayHold(); err == nil {
			t.Fatalf("接受无效记录: %s", content[:min(len(content), 80)])
		}
	}
}
