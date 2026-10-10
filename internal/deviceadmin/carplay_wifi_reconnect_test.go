package deviceadmin

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

type carPlayReconnectRunner struct {
	*fakeRunner
	manager *Manager
	stop    func() error
}

func (r *carPlayReconnectRunner) Run(ctx context.Context, name string, args ...string) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if name == "systemctl" && len(args) > 0 && args[0] == "stop" {
		if !r.manager.mu.TryLock() {
			return nil, errors.New("停止服务时仍持有网络锁")
		}
		r.manager.mu.Unlock()
		if r.stop != nil {
			if err := r.stop(); err != nil {
				return nil, err
			}
		}
	}
	return r.fakeRunner.Run(ctx, name, args...)
}

func reconnectFixture(t *testing.T) (*Manager, *carPlayReconnectRunner, ConnectRequest) {
	t.Helper()
	lease := testCarPlayLease()
	runner := &carPlayReconnectRunner{fakeRunner: &fakeRunner{responses: map[string]string{
		"systemctl show " + carPlayServiceUnit + " -p ActiveState --value":           "active\n",
		"nmcli -t --escape yes -f NAME,UUID,TYPE,DEVICE,AUTOCONNECT connection show": "Phone:" + lease.Plan.UpstreamUUID + ":802-11-wireless::yes\n",
		"nmcli -g 802-11-wireless.mode,802-11-wireless.ssid,802-11-wireless.band,802-11-wireless.channel,802-11-wireless-security.key-mgmt,ipv4.method,ipv4.addresses connection show uuid " + lease.Plan.UpstreamUUID: "infrastructure\nPhone\na\n0\nnone\nauto\n\n",
		"nmcli -t --escape yes -f DEVICE,TYPE,STATE device status":                                                  "wlan0:wifi:disconnected\n",
		"nmcli -t --escape yes -f IN-USE,SSID,BSSID,SIGNAL,CHAN,SECURITY device wifi list ifname wlan0 --rescan no": ":Phone:AA\\:BB\\:CC\\:DD\\:EE\\:01:70:44:--\n",
	}}}
	m := New(runner, nil)
	runner.manager = m
	if err := m.RestoreCarPlayNetworkHold(lease); err != nil {
		t.Fatal(err)
	}
	runner.stop = func() error {
		m.mu.Lock()
		m.carPlayNetworkHold = nil
		m.mu.Unlock()
		return nil
	}
	return m, runner, ConnectRequest{UUID: lease.Plan.UpstreamUUID, ConfirmCarPlayReconnect: true}
}

func TestConfirmedWiFiReconnectResumesCarPlayAfterSuccessAndFailure(t *testing.T) {
	for _, failConnect := range []bool{false, true} {
		t.Run(map[bool]string{false: "success", true: "failure"}[failConnect], func(t *testing.T) {
			m, r, request := reconnectFixture(t)
			m.carPlayWiFiRecoveryAfter = time.Now().Add(3 * time.Minute)
			if failConnect {
				r.failures = map[string][]error{"nmcli --wait 15 connection up uuid " + request.UUID + " ifname wlan0 ap AA:BB:CC:DD:EE:01": {errors.New("连接失败")}}
			}
			err := m.Connect(context.Background(), request)
			if (err != nil) != failConnect {
				t.Fatalf("连接结果: %v", err)
			}
			if m.carPlayWiFiRecoveryAfter.IsZero() == failConnect {
				t.Fatal("连接成功未结束冷却，或失败错误清除了冷却")
			}
			calls := strings.Join(r.calls, "\n")
			if !strings.Contains(calls, "systemctl stop "+carPlayServiceUnit) || !strings.Contains(calls, "systemctl --no-block start "+carPlayServiceUnit) || m.carPlayWiFiReconnect {
				t.Fatalf("未恢复服务: %s", calls)
			}
			if strings.Contains(calls, "systemctl reboot") || strings.Contains(calls, "stop teslabox.service") {
				t.Fatal("重启了录像主程序")
			}
		})
	}
}

func TestWiFiReconnectRequiresConfirmationAndSafeRelease(t *testing.T) {
	for _, scenario := range []string{"unconfirmed", "invalid", "unreleased", "stop_failure", "restart_failure"} {
		t.Run(scenario, func(t *testing.T) {
			m, r, request := reconnectFixture(t)
			switch scenario {
			case "unconfirmed":
				request.ConfirmCarPlayReconnect = false
			case "invalid":
				request.UUID = "invalid"
			case "unreleased":
				r.stop = func() error { return nil }
			case "stop_failure":
				r.stop = func() error { return errors.New("停止失败") }
			case "restart_failure":
				r.failures = map[string][]error{"systemctl --no-block start " + carPlayServiceUnit: {errors.New("恢复失败")}}
			}
			if err := m.Connect(context.Background(), request); err == nil {
				t.Fatal("错误情况被报告为成功")
			}
			calls := strings.Join(r.calls, "\n")
			if scenario != "restart_failure" && strings.Contains(calls, "connection up uuid") {
				t.Fatal("未安全释放就修改上游")
			}
			if (scenario == "invalid" || scenario == "unconfirmed") && strings.Contains(calls, "systemctl") {
				t.Fatal("未确认或无效请求中断了服务")
			}
			if m.carPlayWiFiReconnect {
				t.Fatal("重连状态未清理")
			}
		})
	}
}

func TestScheduledWiFiFailureOnlyCoolsDownItsTarget(t *testing.T) {
	m, r, request := reconnectFixture(t)
	m.carPlayWiFiRecoveryUUID = request.UUID
	m.carPlayWiFiRecoveryAfter = time.Now().Add(3 * time.Minute)
	r.failures = map[string][]error{"nmcli --wait 15 connection up uuid " + request.UUID + " ifname wlan0 ap AA:BB:CC:DD:EE:01": {errors.New("旧网络已离开范围")}}
	if err := m.Connect(context.Background(), request); err == nil {
		t.Fatal("失败被报告为成功")
	}
	if !m.carPlayWiFiRecoveryAfter.IsZero() || m.carPlayWiFiRecoveryUUID != "" || !m.carPlayWiFiFailures[request.UUID].After(time.Now()) {
		t.Fatal("旧网络失败仍阻止其他网络接替")
	}
}

func TestConfirmedWiFiReconnectSurvivesBrowserDisconnect(t *testing.T) {
	m, r, request := reconnectFixture(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	r.stop = func() error {
		// 新租约不能在强制连接事务中抢占无线网卡。
		_, err := m.AcquireCarPlayNetwork(context.Background(), "other", CarPlayNetworkRequest{}, time.Now().Add(time.Minute), func(*CarPlayNetworkLease) error { return nil })
		if err == nil {
			t.Fatal("新租约抢占重连事务")
		}
		m.mu.Lock()
		m.carPlayNetworkHold = nil
		m.mu.Unlock()
		cancel()
		return nil
	}
	if err := m.Connect(ctx, request); err != nil {
		t.Fatal(err)
	}
}
