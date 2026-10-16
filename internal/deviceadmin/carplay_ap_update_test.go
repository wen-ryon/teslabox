package deviceadmin

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

func apRestartFixture(t *testing.T) (*Manager, *carPlayReconnectRunner, APUpdate) {
	t.Helper()
	m, r, _ := reconnectFixture(t)
	uuid := m.carPlayNetworkHold.Plan.APUUID
	r.responses["nmcli -t --escape yes -f NAME,UUID,TYPE,DEVICE,AUTOCONNECT connection show"] = "TeslaBox:" + uuid + ":802-11-wireless::yes\n"
	r.responses["nmcli -g 802-11-wireless.mode,802-11-wireless.ssid,802-11-wireless.band,802-11-wireless.channel,802-11-wireless-security.key-mgmt,ipv4.method,ipv4.addresses connection show uuid "+uuid] = "ap\nTeslaBox\na\n161\nwpa-psk\nshared\n192.168.88.1/24\n"
	r.responses["nmcli -g connection.interface-name connection show uuid "+uuid] = "ap0\n"
	r.responses["nmcli -t --escape yes -f DEVICE,TYPE,STATE device status"] = "ap0:wifi:disconnected\nwlan0:wifi:disconnected\n"
	configureAllowedAP(m, r.fakeRunner, "ap0", "TeslaBox", 161)
	m.schedule = func(time.Duration, func()) { t.Fatal("确认事务不能留下延迟关闭任务") }
	return m, r, APUpdate{Enabled: true, SSID: "New-Hotspot", Band: "a", Channel: 161, AutoConnect: true, ConfirmCarPlayReconnect: true}
}

func TestAPUpdateRestartsCarPlayAndHandsOffTemplate(t *testing.T) {
	for _, enabled := range []bool{true, false} {
		t.Run(map[bool]string{true: "保存开启配置", false: "保存关闭配置"}[enabled], func(t *testing.T) {
			m, r, update := apRestartFixture(t)
			update.Enabled = enabled
			if err := m.UpdateAP(t.Context(), update); err != nil {
				t.Fatal(err)
			}
			calls := strings.Join(r.calls, "\n")
			stop := strings.Index(calls, "systemctl stop "+carPlayServiceUnit)
			modify := strings.Index(calls, "nmcli connection modify uuid ")
			down := strings.Index(calls, "nmcli --wait 15 connection down uuid ")
			start := strings.Index(calls, "systemctl --no-block start "+carPlayServiceUnit)
			if !(stop >= 0 && modify > stop && down > modify && start > down) {
				t.Fatalf("必须先释放、保存、关闭普通热点再恢复：%s", calls)
			}
			if strings.Contains(calls, "connection up uuid") || strings.Contains(calls, "teslabox.service") || strings.Contains(calls, "reboot") || m.carPlayWiFiReconnect {
				t.Fatalf("抢占了专用热点、影响录像或未清除事务：%s", calls)
			}
			if !enabled && !strings.Contains(calls, "connection.autoconnect no") {
				t.Fatal("关闭普通热点未同时关闭其自动连接")
			}
		})
	}
}

func TestAPUpdateDoesNotInterruptWithoutValidConfirmation(t *testing.T) {
	for _, scenario := range []string{"unconfirmed", "ssid", "password", "channel", "security", "unicode_password", "uncertain", "concurrent"} {
		t.Run(scenario, func(t *testing.T) {
			m, r, update := apRestartFixture(t)
			switch scenario {
			case "unconfirmed":
				update.ConfirmCarPlayReconnect = false
			case "ssid":
				update.SSID = ""
			case "password":
				update.Password = "short"
			case "channel":
				update.Channel = -1
			case "security":
				update.SecurityMode = "wpa3"
			case "unicode_password":
				update.Password = "密码abcdefgh"
			case "uncertain":
				m.carPlayRecoveryUncertain = true
			case "concurrent":
				m.carPlayWiFiReconnect = true
			}
			if err := m.UpdateAP(t.Context(), update); err == nil {
				t.Fatal("非法或未确认操作被接受")
			}
			for _, call := range r.calls {
				if strings.HasPrefix(call, "systemctl") || strings.Contains(call, "connection modify") {
					t.Fatalf("无效请求不能中断服务或修改配置：%s", call)
				}
			}
		})
	}
}

func TestAPUpdateFailureStillRequestsCarPlayRecovery(t *testing.T) {
	for _, scenario := range []string{"stop", "unreleased", "modify", "down", "restart", "identity"} {
		t.Run(scenario, func(t *testing.T) {
			m, r, update := apRestartFixture(t)
			r.failures = map[string][]error{}
			uuid := m.carPlayNetworkHold.Plan.APUUID
			switch scenario {
			case "stop":
				r.stop = func() error { return errors.New("停止失败") }
			case "unreleased":
				r.stop = func() error { return nil }
			case "modify":
				r.failures["nmcli connection modify uuid "+uuid+" 802-11-wireless.ssid New-Hotspot 802-11-wireless.band a 802-11-wireless.channel 161 connection.autoconnect yes connection.autoconnect-retries 1 ipv4.method shared"] = []error{errors.New("保存失败")}
			case "down":
				r.failures["nmcli --wait 15 connection down uuid "+uuid] = []error{errors.New("关闭失败")}
			case "restart":
				r.failures["systemctl --no-block start "+carPlayServiceUnit] = []error{errors.New("启动失败")}
			case "identity":
				r.stop = func() error {
					m.mu.Lock()
					m.carPlayNetworkHold = nil
					m.mu.Unlock()
					r.responses["nmcli -t --escape yes -f NAME,UUID,TYPE,DEVICE,AUTOCONNECT connection show"] = ""
					return nil
				}
			}
			err := m.UpdateAP(t.Context(), update)
			if err == nil || m.carPlayWiFiReconnect || !containsCall(r.calls, "systemctl --no-block start "+carPlayServiceUnit) {
				t.Fatalf("失败必须清理事务并尝试恢复：%v %v", err, r.calls)
			}
			if scenario == "restart" {
				var op *APOperationError
				if !errors.As(err, &op) || op.Code != "carplay_restart_failed" {
					t.Fatalf("恢复失败没有明确原因码：%v", err)
				}
			}
			if scenario == "unreleased" || scenario == "identity" || scenario == "stop" {
				if strings.Contains(strings.Join(r.calls, "\n"), "connection modify uuid") {
					t.Fatal("未安全释放或模板改变时修改了配置")
				}
			}
		})
	}
}

func TestAPUpdateSurvivesBrowserDisconnectAndRejectsConcurrentLease(t *testing.T) {
	m, r, update := apRestartFixture(t)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	r.stop = func() error {
		if _, err := m.AcquireCarPlayNetwork(t.Context(), "other", CarPlayNetworkRequest{}, time.Now().Add(time.Minute), func(*CarPlayNetworkLease) error { return nil }); err == nil {
			t.Fatal("新租约抢占了变更事务")
		}
		if err := m.UpdateAP(t.Context(), APUpdate{}); err == nil {
			t.Fatal("并发 AP 保存抢占了变更事务")
		}
		m.mu.Lock()
		m.carPlayNetworkHold = nil
		m.mu.Unlock()
		cancel()
		return nil
	}
	if err := m.UpdateAP(ctx, update); err != nil {
		t.Fatal(err)
	}
}

func TestAPUpdateRejectsUnavailableCarPlayServiceWithoutChangingNetwork(t *testing.T) {
	m, r, update := apRestartFixture(t)
	r.responses["systemctl show "+carPlayServiceUnit+" -p ActiveState --value"] = "inactive\n"
	if err := m.UpdateAP(t.Context(), update); err == nil || m.carPlayWiFiReconnect {
		t.Fatalf("服务不可用时必须拒绝并清理事务：%v", err)
	}
	calls := strings.Join(r.calls, "\n")
	if strings.Contains(calls, "systemctl stop") || strings.Contains(calls, "connection modify") || strings.Contains(calls, "--no-block start") {
		t.Fatal("没有可安全恢复的服务时修改了网络")
	}
}

func TestAPUpdateWithoutCarPlayKeepsRegularHotspotBehavior(t *testing.T) {
	m, r, update := apRestartFixture(t)
	m.carPlayNetworkHold = nil
	configureAllowedAP(m, r.fakeRunner, "ap0", update.SSID, 161)
	if err := m.UpdateAP(t.Context(), update); err != nil {
		t.Fatal(err)
	}
	calls := strings.Join(r.calls, "\n")
	if strings.Contains(calls, "systemctl") || !strings.Contains(calls, "nmcli --wait 15 connection up uuid") {
		t.Fatalf("无 CarPlay 租约时应走原普通热点流程：%s", calls)
	}
}
