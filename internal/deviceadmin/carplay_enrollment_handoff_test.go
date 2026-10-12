package deviceadmin

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

type handoffRunner struct {
	path, config string
	auto, active bool
	failUp       bool
	writes       []string
}

func (r *handoffRunner) Run(_ context.Context, name string, args ...string) ([]byte, error) {
	call := name + " " + strings.Join(args, " ")
	if strings.Contains(call, "--show-secrets") {
		return []byte(r.config), nil
	}
	if strings.Contains(call, "-g connection.autoconnect") {
		return []byte(yesNo(r.auto)), nil
	}
	if strings.Contains(call, "-t -f UUID connection show --active") {
		if r.active {
			return []byte(testCarPlayLease().Plan.APUUID), nil
		}
		return nil, nil
	}
	if _, err := os.Stat(r.path); err != nil {
		return nil, errors.New("没有恢复记录就修改网络")
	}
	r.writes = append(r.writes, call)
	switch {
	case strings.Contains(call, "connection modify"):
		r.auto = args[len(args)-1] == "yes"
	case strings.Contains(call, "connection down"):
		r.active = false
	case strings.Contains(call, "connection up"):
		if r.failUp {
			return nil, errors.New("模拟热点恢复失败")
		}
		r.active = true
	default:
		return nil, errors.New("非预期命令")
	}
	return nil, nil
}

func handoffFixture(t *testing.T) (*Manager, *handoffRunner) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "settings.json")
	r := &handoffRunner{path: path + ".ap-handoff", config: "original-profile-and-secret", auto: true, active: true}
	m := New(r, nil, NetworkConfig{CarPlaySettingsPath: path})
	value := &carPlayEnrollmentHandoff{Version: 1, Operation: strings.Repeat("a", 32), Phone: "02:11:22:33:44:55", UUID: testCarPlayLease().Plan.APUUID, ConfigHash: handoffHash(r.config), AutoConnect: true, Active: true}
	if err := m.saveHandoff(value); err != nil {
		t.Fatal(err)
	}
	return m, r
}

func TestEnrollmentHandoffRestoresAfterInterruptedStages(t *testing.T) {
	for _, stage := range []string{"prepared", "autoconnect_disabled", "suspended", "restore_interrupted"} {
		t.Run(stage, func(t *testing.T) {
			m, r := handoffFixture(t)
			if stage == "autoconnect_disabled" {
				r.auto = false
			}
			if stage == "suspended" || stage == "restore_interrupted" {
				if err := m.suspendHandoffLocked(context.Background()); err != nil {
					t.Fatal(err)
				}
			}
			if stage == "restore_interrupted" {
				r.failUp = true
				if err := m.restoreHandoffLocked(context.Background()); err == nil {
					t.Fatal("恢复失败没有保留记录")
				}
				r.failUp = false
			}
			fresh := New(r, nil, NetworkConfig{CarPlaySettingsPath: m.carPlaySettingsPath})
			if fresh.carPlayRecoveryUncertain {
				t.Fatal("合法交接记录无法读取")
			}
			if err := fresh.RecoverCarPlayHandoff(context.Background()); err != nil {
				t.Fatal(err)
			}
			if !r.auto || !r.active || fresh.carPlayEnrollmentHandoff != nil {
				t.Fatal("原热点未恢复")
			}
			body, err := os.ReadFile(r.path)
			if err != nil || strings.Contains(string(body), "secret") {
				t.Fatal("记录泄露密码", err)
			}
		})
	}
}

func TestEnrollmentHandoffDoesNotOverwriteChangedProfileOrOwnedLease(t *testing.T) {
	for _, change := range []string{"config", "autoconnect", "active", "lease", "unknown_recovery"} {
		t.Run(change, func(t *testing.T) {
			m, r := handoffFixture(t)
			if err := m.suspendHandoffLocked(context.Background()); err != nil {
				t.Fatal(err)
			}
			switch change {
			case "config":
				r.config = "user-new-config"
			case "autoconnect":
				r.auto = true
			case "active":
				r.active = true
			case "unknown_recovery":
				m.carPlayRecoveryUncertain = true
			case "lease":
				lease := testCarPlayLease()
				m.carPlayNetworkHold = &lease
			}
			before := len(r.writes)
			if err := m.FinishCarPlayHandoff(context.Background(), strings.Repeat("a", 32), false); err == nil {
				t.Fatal("覆盖外部配置或未释放租约")
			}
			if len(r.writes) != before || m.carPlayEnrollmentHandoff == nil {
				t.Fatal("冲突后仍修改网络或丢失记录")
			}
		})
	}
}

func TestEnrollmentHandoffCommittedRecordSurvivesRestart(t *testing.T) {
	m, r := handoffFixture(t)
	if err := m.suspendHandoffLocked(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := m.FinishCarPlayHandoff(context.Background(), strings.Repeat("a", 32), true); err != nil {
		t.Fatal(err)
	}
	fresh := New(r, nil, NetworkConfig{CarPlaySettingsPath: m.carPlaySettingsPath})
	before := len(r.writes)
	if err := fresh.RecoverCarPlayHandoff(context.Background()); err != nil {
		t.Fatal(err)
	}
	if fresh.carPlayEnrollmentHandoff == nil || !fresh.carPlayEnrollmentHandoff.Committed || len(r.writes) != before {
		t.Fatal("成功授权在重启时被误回滚")
	}
}
