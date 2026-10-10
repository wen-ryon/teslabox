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

func TestCarPlayFirstAcquireCancelsActivationAfterEmptySnapshot(t *testing.T) {
	const apUUID = "11111111-1111-1111-1111-111111111111"
	const upstreamUUID = "22222222-2222-2222-2222-222222222222"
	for _, activation := range []string{"none", "saved", "unknown"} {
		pending, unknown := activation != "none", activation == "unknown"
		for _, journalFails := range []bool{false, true} {
			t.Run(strings.Join([]string{activation, yesNo(journalFails)}, "/"), func(t *testing.T) {
				r := &fakeRunner{responses: map[string]string{
					"nmcli -t --escape yes -f NAME,UUID,TYPE,DEVICE,AUTOCONNECT connection show":                                                                                                                        "TeslaBox-5G:" + apUUID + ":802-11-wireless::no\nHome:" + upstreamUUID + ":802-11-wireless::yes\n",
					"nmcli -g 802-11-wireless.mode,802-11-wireless.ssid,802-11-wireless.band,802-11-wireless.channel,802-11-wireless-security.key-mgmt,ipv4.method,ipv4.addresses connection show uuid " + apUUID:       "ap\nTeslaBox-5G\na\n149\nwpa-psk\nshared\n192.168.88.1/24\n",
					"nmcli -g 802-11-wireless.mode,802-11-wireless.ssid,802-11-wireless.band,802-11-wireless.channel,802-11-wireless-security.key-mgmt,ipv4.method,ipv4.addresses connection show uuid " + upstreamUUID: "infrastructure\nHome\na\n0\nwpa-psk\nauto\n\n",
					"nmcli -t --escape yes -f DEVICE,TYPE,STATE device status":                                                                                                                                          "wlan0:wifi:disconnected\nap0:wifi:disconnected\n",
					"nmcli -g GENERAL.AUTOCONNECT device show wlan0":                                                                                                                                                    "yes\n",
				}}
				if pending {
					r.responses["nmcli -t -f UUID,DEVICE connection show --active"] = upstreamUUID + ":wlan0\n"
					if unknown {
						r.responses["nmcli -t -f UUID,DEVICE connection show --active"] = "55555555-5555-4555-8555-555555555555:wlan0\n"
					}
				}
				m := New(r, nil)
				m.manualWiFiDisconnect = true
				m.sameWirelessPHY = func(string, string) (bool, bool) { return true, true }
				m.wirelessPHYInfo = func(context.Context, string) (string, error) {
					return "* 5745 MHz [149] (20.0 dBm)\nvalid interface combinations:\n * #{ managed } <= 1, #{ AP } <= 1, total <= 2, #channels <= 1\n", nil
				}
				disconnect := "nmcli --wait 15 device disconnect wlan0"
				if pending {
					disconnect = "nmcli --wait 15 connection down uuid " + upstreamUUID
				}
				written := false
				lease, err := m.AcquireCarPlayNetwork(context.Background(), "carplay", CarPlayNetworkRequest{Channels: []int{149}}, time.Now().Add(time.Minute), func(lease *CarPlayNetworkLease) error {
					if unknown {
						t.Fatal("首次接管认领了快照以外的未知配置")
					}
					if containsCall(r.calls, disconnect) || containsCall(r.calls, "nmcli device set wlan0 autoconnect no") {
						t.Fatal("保存恢复记录之前取消了连接")
					}
					if !lease.DeviceAutoConnectManaged || !lease.OriginalDeviceAutoConnect || pending && (lease.Plan.UpstreamUUID != upstreamUUID || !lease.Plan.PauseUpstream) {
						t.Fatal("没有保存原始设备策略或进行中的连接身份")
					}
					if journalFails {
						return errors.New("保存失败")
					}
					written = true
					return nil
				})
				if (err != nil) != (journalFails || unknown) || containsCall(r.calls, disconnect) != written {
					t.Fatalf("首次接管错误: %+v %v %v", lease, err, r.calls)
				}
				if unknown && strings.Contains(strings.Join(r.calls, "\n"), "autoconnect no") {
					t.Fatal("未知上游被拒绝后仍修改了设备策略")
				}
				if written {
					pending := m.carPlayStartupAdoption
					if pending == nil || pending.leaseUUID != lease.RuntimeAPUUID || !pending.candidates[upstreamUUID] || !pending.expiresAt.After(time.Now()) {
						t.Fatal("成功接管没有保留启动快照身份")
					}
				} else if m.carPlayStartupAdoption != nil {
					t.Fatal("恢复日志未保存便发布了启动认领状态")
				}
			})
		}
	}
}

func TestCarPlayRenewDoesNotAdoptUnknownUpstream(t *testing.T) {
	lease := testCarPlayLease()
	lease.Plan.UpstreamUUID, lease.Plan.PauseUpstream = "", false
	r := &fakeRunner{responses: map[string]string{
		"nmcli -t -f UUID,DEVICE connection show --active": "22222222-2222-2222-2222-222222222222:wlan0\n" + lease.RuntimeAPUUID + ":ap0\n",
	}}
	m := New(r, nil)
	if err := m.RestoreCarPlayNetworkHold(lease); err != nil {
		t.Fatal(err)
	}
	_, err := m.AcquireCarPlayNetwork(context.Background(), lease.Owner, CarPlayNetworkRequest{Channels: []int{lease.Plan.Channel}}, time.Now().Add(time.Minute), func(*CarPlayNetworkLease) error {
		t.Fatal("续租认领了未知连接")
		return nil
	})
	if err == nil || strings.Contains(strings.Join(r.calls, "\n"), "disconnect") {
		t.Fatal("续租修改了未知上游")
	}
}

func TestCarPlayBluetoothRenewAcceptsOnlyRecentKnownSameChannelActivation(t *testing.T) {
	for _, scenario := range []string{"same_channel", "unknown", "expired", "other_lease", "different_channel", "missing_ap", "already_promoted", "checkpoint_failure"} {
		t.Run(scenario, func(t *testing.T) {
			lease := testCarPlayLease()
			upstream := lease.Plan.UpstreamUUID
			lease.Plan.UpstreamUUID, lease.Plan.PauseUpstream = "", false
			r := &fakeRunner{responses: map[string]string{
				"nmcli -t -f UUID,DEVICE connection show --active": upstream + ":wlan0\n" + lease.RuntimeAPUUID + ":ap0\n",
				"/usr/sbin/iw dev wlan0 info":                      "type managed\nchannel 149 (5745 MHz)\n",
				"/usr/sbin/iw dev ap0 info":                        "type AP\nchannel 149 (5745 MHz)\n",
			}}
			m := New(r, nil)
			m.carPlaySettingsPath = filepath.Join(t.TempDir(), "settings.json")
			store, err := carplaysettings.New(m.carPlaySettingsPath)
			if err != nil {
				t.Fatal(err)
			}
			settings := store.Settings()
			settings.Network = "bluetooth"
			if _, err := store.Save(settings); err != nil {
				t.Fatal(err)
			}
			if scenario == "already_promoted" {
				lease.Plan.DisableWiFiUpstream = true
			}
			if err := m.RestoreCarPlayNetworkHold(lease); err != nil {
				t.Fatal(err)
			}
			m.carPlayStartupAdoption = &carPlayStartupAdoption{leaseUUID: lease.RuntimeAPUUID,
				expiresAt: time.Now().Add(time.Minute), candidates: map[string]bool{upstream: true}}
			switch scenario {
			case "unknown":
				m.carPlayStartupAdoption.candidates = nil
			case "expired":
				m.carPlayStartupAdoption.expiresAt = time.Now().Add(-time.Second)
			case "other_lease":
				m.carPlayStartupAdoption.leaseUUID = "different"
			case "different_channel":
				r.responses["/usr/sbin/iw dev wlan0 info"] = "type managed\nchannel 36\n"
			case "missing_ap":
				r.responses["nmcli -t -f UUID,DEVICE connection show --active"] = upstream + ":wlan0\n"
			}
			written := false
			got, err := m.AcquireCarPlayNetwork(context.Background(), lease.Owner, CarPlayNetworkRequest{Channels: []int{149}}, time.Now().Add(time.Minute), func(value *CarPlayNetworkLease) error {
				if scenario == "checkpoint_failure" {
					return errors.New("保存失败")
				}
				written = true
				return validateCarPlayLease(*value)
			})
			if scenario == "same_channel" {
				if err != nil || !written || got.Plan.UpstreamUUID != upstream || got.Plan.PauseUpstream {
					t.Fatalf("晚到同信道连接未保留: %+v %v", got, err)
				}
			} else if err == nil || written || m.carPlayNetworkHold.Plan.UpstreamUUID != "" {
				t.Fatalf("不安全认领或失败修改了租约: %+v %v", got, err)
			}
			for _, call := range r.calls {
				if strings.Contains(call, "connection down") || strings.Contains(call, "device disconnect") {
					t.Fatalf("蓝牙未验证便断开了 Wi-Fi: %s", call)
				}
			}
		})
	}
}

func TestCarPlayStartupRetainsConfiguredChannelWithoutWiFiConstraint(t *testing.T) {
	for _, test := range []struct {
		name                 string
		configured, upstream int
		active, expected     int
	}{
		{"保留用户的五 GHz 配置", 149, 0, 0, 149},
		{"保留用户的二点四 GHz 配置", 6, 0, 0, 6},
		{"未配置使用兼容列表", 0, 0, 0, 1},
		{"配置不兼容使用可用信道", 44, 0, 0, 1},
		{"同信道上游仍优先", 149, 6, 0, 6},
		{"活动热点不改变", 149, 0, 6, 6},
	} {
		t.Run(test.name, func(t *testing.T) {
			settings := APSettings{Available: true, Interface: "ap0", UUID: "11111111-1111-1111-1111-111111111111",
				ConcurrencyMode: "same_channel", Channel: test.configured, AllowedChannels: []int{1, 6, 149},
				Enabled: test.active != 0, ActiveChannel: test.active}
			status := WiFiStatus{Interface: "wlan0"}
			if test.upstream != 0 {
				status.CurrentConnection = "Home"
				status.currentConnectionUUID = "22222222-2222-2222-2222-222222222222"
			}
			plan, err := planCarPlayNetwork(settings, status, test.upstream, []int{1, 6, 149})
			if err != nil || plan.Channel != test.expected {
				t.Fatalf("配置没有按并发约束保留: %+v %v", plan, err)
			}
		})
	}
}
