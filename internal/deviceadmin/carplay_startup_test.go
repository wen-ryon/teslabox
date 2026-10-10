package deviceadmin

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/wen-ryon/teslabox/internal/carplaysettings"
)

func TestCarPlayStartupConnectsVisibleCompatibleSavedNetworkBeforeLease(t *testing.T) {
	const uuid = "22222222-2222-2222-2222-222222222222"
	for _, test := range []struct {
		name     string
		points   string
		cooldown bool
		connect  bool
	}{
		{"可见兼容上游", ":Home:AA\\:BB\\:CC\\:DD\\:EE\\:FF:80:149:WPA2\n", false, true},
		{"车机不支持信道", ":Home:AA\\:BB\\:CC\\:DD\\:EE\\:FF:80:44:WPA2\n", false, false},
		{"上游不在附近", "", false, false},
		{"失败目标仍在冷却", ":Home:AA\\:BB\\:CC\\:DD\\:EE\\:FF:80:149:WPA2\n", true, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			runner := &fakeRunner{responses: map[string]string{
				"nmcli -g GENERAL.STATE device show wlan0": "30 (disconnected)\n",
				"nmcli -t --escape yes -f IN-USE,SSID,BSSID,SIGNAL,CHAN,SECURITY device wifi list ifname wlan0 --rescan no": test.points,
			}}
			m := New(runner, nil)
			if test.cooldown {
				m.carPlayWiFiFailures = map[string]time.Time{uuid: time.Now().Add(time.Minute)}
			}
			status := WiFiStatus{Available: true, Interface: "wlan0", SavedNetworks: []SavedNetwork{
				{UUID: uuid, SSID: "Home", AutoConnect: true},
			}}
			settings := APSettings{ConcurrencyMode: "same_channel", AllowedChannels: []int{149}}
			changed := m.prepareCarPlayStartupUpstream(context.Background(), settings, status, []int{149})
			if changed != test.connect {
				t.Fatalf("启动连接决策错误: %v", changed)
			}
			joined := strings.Join(runner.calls, "\n")
			if strings.Contains(joined, "connection up uuid "+uuid) != test.connect {
				t.Fatal("没有按可见性和兼容信道连接")
			}
			for _, mutation := range []string{"device wifi rescan", "connection modify", "device set", "device disconnect"} {
				if strings.Contains(joined, mutation) {
					t.Fatalf("启动探测修改了不属于自己的网络策略: %s", mutation)
				}
			}
		})
	}
}

func TestCarPlayStartupFastPathsDoNotWaitForWiFi(t *testing.T) {
	for _, test := range []struct {
		name, mode, current, active string
		manual                      bool
	}{
		{"已有无线", "same_channel", "Home", "", false},
		{"独立网卡", "independent_radio", "", "", false},
		{"用户手动断开", "same_channel", "", "", true},
		{"已有有线", "same_channel", "", "x:802-3-ethernet:eth0\n", false},
	} {
		t.Run(test.name, func(t *testing.T) {
			runner := &fakeRunner{responses: map[string]string{
				"nmcli -t --escape yes -f UUID,TYPE,DEVICE connection show --active": test.active,
			}}
			m := New(runner, nil)
			m.manualWiFiDisconnect = test.manual
			if m.prepareCarPlayStartupUpstream(context.Background(), APSettings{ConcurrencyMode: test.mode},
				WiFiStatus{Available: true, Interface: "wlan0", CurrentConnection: test.current}, []int{149}) {
				t.Fatal("快速路径仍等待无线")
			}
			for _, call := range runner.calls {
				if strings.Contains(call, "device show wlan0") || strings.Contains(call, "connection up") {
					t.Fatalf("快速路径仍尝试无线激活: %s", call)
				}
			}
		})
	}
}

func TestSavedWiFiActivationDoesNotInterruptNetworkManagerDHCP(t *testing.T) {
	const uuid = "22222222-2222-2222-2222-222222222222"
	runner := &fakeRunner{responses: map[string]string{
		"nmcli -g GENERAL.CON-UUID device show wlan0": uuid,
	}, responseSequences: map[string][]string{
		"nmcli -g GENERAL.STATE device show wlan0": {"70 (ip-config)", "70 (ip-config)", "100 (connected)"},
	}}
	m := New(runner, nil)
	args := []string{"--wait", "15", "connection", "up", "uuid", uuid, "ifname", "wlan0"}
	if err := m.activateSavedWiFi(context.Background(), uuid, "wlan0", args); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(strings.Join(runner.calls, "\n"), "connection up") {
		t.Fatal("同一网络 DHCP 被重复激活打断")
	}
}

func TestSavedWiFiActivationIdentityAndFailure(t *testing.T) {
	const uuid = "22222222-2222-2222-2222-222222222222"
	for _, test := range []struct {
		name, current string
		states        []string
		activate      bool
		failed        bool
	}{
		{"同一网络已连接", uuid, []string{"100 (connected)"}, false, false},
		{"同一网络认证失败", uuid, []string{"60 (need-auth)", "120 (failed)"}, false, true},
		{"切换不同网络", "33333333-3333-4333-8333-333333333333", nil, true, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			runner := &fakeRunner{responses: map[string]string{
				"nmcli -g GENERAL.CON-UUID device show wlan0": test.current,
			}, responseSequences: map[string][]string{
				"nmcli -g GENERAL.STATE device show wlan0": test.states,
			}}
			m := New(runner, nil)
			err := m.activateSavedWiFi(context.Background(), uuid, "wlan0",
				[]string{"--wait", "15", "connection", "up", "uuid", uuid, "ifname", "wlan0"})
			if (err != nil) != test.failed || strings.Contains(strings.Join(runner.calls, "\n"), "connection up") != test.activate {
				t.Fatalf("没有按连接身份和失败状态处理: err=%v calls=%v", err, runner.calls)
			}
		})
	}
}

func TestCarPlayStartupWaitHonorsCancellation(t *testing.T) {
	runner := &fakeRunner{responses: map[string]string{"nmcli -g GENERAL.STATE device show wlan0": "70 (ip-config)"}}
	m := New(runner, nil)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()
	if err := m.waitWiFiActivation(ctx, "wlan0"); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("超时没有及时结束等待: %v", err)
	}
}

func TestCarPlayStartupHonorsSelectedSourceAndCarPlayPriority(t *testing.T) {
	for _, pair := range [][2]string{{"bluetooth", "internet"}, {"ethernet", "internet"}, {"wifi", "carplay"}} {
		runner := &fakeRunner{}
		m := New(runner, nil)
		m.carPlaySettingsPath = filepath.Join(t.TempDir(), "settings.json")
		settings, err := carplaysettings.New(m.carPlaySettingsPath)
		if err != nil {
			t.Fatal(err)
		}
		value := settings.Settings()
		value.Network, value.RecoveryPriority = pair[0], pair[1]
		if _, err := settings.Save(value); err != nil {
			t.Fatal(err)
		}
		if m.prepareCarPlayStartupUpstream(context.Background(), APSettings{ConcurrencyMode: "same_channel"},
			WiFiStatus{Available: true, Interface: "wlan0"}, []int{149}) || len(runner.calls) != 0 {
			t.Fatalf("绕过用户来源或优先级: %v", pair)
		}
	}
}

func TestCarPlayStartupLetsNetworkManagerActivateAfterScan(t *testing.T) {
	const uuid = "22222222-2222-2222-2222-222222222222"
	runner := &fakeRunner{responses: map[string]string{
		"nmcli -g GENERAL.AUTOCONNECT device show wlan0":                                                            "yes",
		"nmcli -t --escape yes -f IN-USE,SSID,BSSID,SIGNAL,CHAN,SECURITY device wifi list ifname wlan0 --rescan no": ":Home:AA\\:BB\\:CC\\:DD\\:EE\\:FF:80:149:WPA2\n",
	}, responseSequences: map[string][]string{
		"nmcli -g GENERAL.STATE device show wlan0": {"30 (disconnected)", "30 (disconnected)", "70 (ip-config)", "100 (connected)"},
	}}
	m := New(runner, nil)
	if !m.prepareCarPlayStartupUpstream(context.Background(), APSettings{ConcurrencyMode: "same_channel", AllowedChannels: []int{149}},
		WiFiStatus{Available: true, Interface: "wlan0", SavedNetworks: []SavedNetwork{{UUID: uuid, SSID: "Home", AutoConnect: true}}}, []int{149}) {
		t.Fatal("没有等待系统在首次扫描后激活")
	}
	if strings.Contains(strings.Join(runner.calls, "\n"), "connection up") {
		t.Fatal("状态查询后系统自动激活仍被重复请求打断")
	}
}
