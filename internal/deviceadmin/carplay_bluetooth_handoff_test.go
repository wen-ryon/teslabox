package deviceadmin

import (
	"context"
	"errors"
	"github.com/wen-ryon/teslabox/internal/carplaysettings"
	"path/filepath"
	"strings"
	"testing"
)

func TestBluetoothVerifiedHandoffAndRollback(t *testing.T) {
	for _, phase := range []string{"offline", "success", "same_5ghz", "ap_failure", "checkpoint_failure"} {
		t.Run(phase, func(t *testing.T) {
			lease := testCarPlayLease()
			lease.Plan.Channel, lease.Plan.PauseUpstream = 1, false
			lease.RuntimePANUUID, lease.RuntimePANAddress = "44444444-4444-4444-4444-444444444444", "AA:BB:CC:DD:EE:FF"
			ap, sta, pan, runtime := lease.Plan.APUUID, lease.Plan.UpstreamUUID, lease.RuntimePANUUID, lease.RuntimeAPUUID
			r := &fakeRunner{responses: map[string]string{
				"nmcli -t --escape yes -f NAME,UUID,TYPE,DEVICE,AUTOCONNECT connection show":                                                                                                               "TeslaBox:" + ap + ":802-11-wireless::no\nHome:" + sta + ":802-11-wireless:wlan0:yes\n",
				"nmcli -g 802-11-wireless.mode,802-11-wireless.ssid,802-11-wireless.band,802-11-wireless.channel,802-11-wireless-security.key-mgmt,ipv4.method,ipv4.addresses connection show uuid " + ap:  "ap\nTeslaBox\nbg\n1\nwpa-psk\nshared\n192.168.88.1/24\n",
				"nmcli -g 802-11-wireless.mode,802-11-wireless.ssid,802-11-wireless.band,802-11-wireless.channel,802-11-wireless-security.key-mgmt,ipv4.method,ipv4.addresses connection show uuid " + sta: "infrastructure\nHome\nbg\n1\nwpa-psk\nauto\n\n",
				"nmcli -t --escape yes -f DEVICE,TYPE,STATE device status":                                                                                                                                 "wlan0:wifi:connected\nap0:wifi:connected\n",
				"nmcli -t -f UUID,DEVICE connection show --active":                                                                                                                                         sta + ":wlan0\n" + runtime + ":ap0\n",
				"nmcli -t --escape yes -f UUID,NAME connection show":                                                                                                                                       pan + ":" + carPlayPANName(lease) + "\n" + runtime + ":" + lease.RuntimeAPName,
				"nmcli --escape no -g connection.type,bluetooth.type,bluetooth.bdaddr connection show uuid " + pan:                                                                                         "bluetooth\npanu\n" + lease.RuntimePANAddress,
				"nmcli --escape no -g bluetooth.bdaddr,bluetooth.type connection show uuid " + pan:                                                                                                         lease.RuntimePANAddress + "\npanu",
				"nmcli -g GENERAL.DEVICES connection show uuid " + pan:                                                                                                                                     "bnep0",
				"nmcli -g IP4.DNS connection show uuid " + pan:                                                                                                                                             "172.20.10.1",
				"nmcli -g connection.id,connection.interface-name,802-11-wireless.mode connection show uuid " + runtime:                                                                                    lease.RuntimeAPName + "\nap0\nap",
				"nmcli -g connection.interface-name,802-11-wireless.mode connection show uuid " + runtime:                                                                                                  "ap0\nap",
				"nmcli --escape no --show-secrets -g 802-11-wireless.ssid,802-11-wireless-security.psk,802-11-wireless-security.key-mgmt,ipv4.method,ipv4.addresses connection show uuid " + ap:            "TeslaBox\ntest-password\nwpa-psk\nshared\n192.168.88.1/24",
				"nmcli --escape no -g GENERAL.HWADDR device show ap0":                                                                                                                                      "AA:BB:CC:DD:EE:01",
				"nmcli -g IP4.ADDRESS device show ap0":                                                                                                                                                     "192.168.88.1/24",
				"/usr/sbin/iw dev wlan0 info":                                                                                                                                                              "type managed\nchannel 1 (2412 MHz)\n",
				"/usr/sbin/iw dev ap0 info":                                                                                                                                                                "type AP\nssid TeslaBox\nchannel 1 (2412 MHz)\n",
				"ip -N -j -4 route show table all":                                                                                                                                                         `[{"dst":"default","dev":"bnep0","gateway":"172.20.10.1","table":254}]`,
				"ip -N -j -4 rule show":                                                                                                                                                                    `[]`,
			}, failures: map[string][]error{}, responseSequences: map[string][]string{}}
			m := New(r, nil)
			m.sameWirelessPHY = func(string, string) (bool, bool) { return true, true }
			m.wirelessPHYInfo = func(context.Context, string) (string, error) {
				return "* 2412 MHz [1] (20.0 dBm)\n* 5180 MHz [36] (20.0 dBm)\nvalid interface combinations:\n * #{ managed } <= 1, #{ AP } <= 1, total <= 2, #channels <= 1\n", nil
			}
			m.carPlaySettingsPath = filepath.Join(t.TempDir(), "settings.json")
			store, err := carplaysettings.New(m.carPlaySettingsPath)
			if err != nil {
				t.Fatal(err)
			}
			settings := store.Settings()
			settings.Network = "bluetooth"
			if _, err = store.Save(settings); err != nil {
				t.Fatal(err)
			}
			if err = m.RestoreCarPlayNetworkHold(lease); err != nil {
				t.Fatal(err)
			}
			m.carPlayVehicleChannels = []int{1, 36}
			if phase == "same_5ghz" {
				lease.Plan.Channel = 157
				m.carPlayNetworkHold.Plan.Channel = 157
				m.carPlayVehicleChannels = []int{1, 36, 157, 161}
				m.wirelessPHYInfo = func(context.Context, string) (string, error) {
					return "* 2412 MHz [1] (20.0 dBm)\n* 5180 MHz [36] (20.0 dBm)\n* 5785 MHz [157] (20.0 dBm)\n* 5805 MHz [161] (20.0 dBm)\nvalid interface combinations:\n * #{ managed } <= 1, #{ AP } <= 1, total <= 2, #channels <= 1\n", nil
				}
				r.responses["/usr/sbin/iw dev ap0 info"] = "type AP\nssid TeslaBox\nchannel 157 (5785 MHz)\n"
				r.responses["/usr/sbin/iw dev wlan0 info"] = "type managed\nchannel 157 (5785 MHz)\n"
			}
			m.carPlayPANInterface = func(context.Context, string) (string, error) { return "bnep0", nil }
			verified := false
			down := "nmcli --wait 15 connection down uuid " + sta
			m.carPlayInternetProbe = func(_ context.Context, device string, dns []string) bool {
				if device != "bnep0" || containsCall(r.calls, down) {
					t.Fatal("验证未绑定蓝牙接口或断网早于验证")
				}
				verified = true
				return phase != "offline"
			}
			if phase == "success" {
				r.responseSequences["/usr/sbin/iw dev ap0 info"] = []string{"type AP\nssid TeslaBox\nchannel 1\n", "type AP\nssid TeslaBox\nchannel 36\n"}
			}
			if phase == "ap_failure" {
				r.failures["nmcli --wait 20 connection up uuid "+runtime+" ifname ap0"] = []error{errors.New("激活失败")}
			}
			state, err := m.EnsureCarPlayUpstream(context.Background(), lease.Owner, lease.RuntimePANAddress, func(value *CarPlayNetworkLease) error {
				if !verified {
					t.Fatal("验证前修改了租约或路由")
				}
				if phase == "checkpoint_failure" {
					return errors.New("持久化失败")
				}
				return validateCarPlayLease(*value)
			})
			if !verified {
				t.Fatal("未探测蓝牙互联网")
			}
			for _, call := range r.calls {
				if strings.Contains(call, "connection delete") || strings.Contains(call, "connection down uuid "+pan) {
					t.Fatalf("切换删除或断开了 PAN: %s", call)
				}
			}
			if phase == "success" || phase == "same_5ghz" {
				want := 36
				if phase == "same_5ghz" {
					want = 157
					for _, call := range r.calls {
						if strings.Contains(call, "connection modify uuid "+runtime) || strings.Contains(call, "connection up uuid "+runtime) || strings.Contains(call, "connection down uuid "+runtime) {
							t.Fatalf("已经在线的五 GHz 热点被重新配置: %s", call)
						}
					}
				}
				if err != nil || !state.WiFiPaused || state.APChannel != want || !containsCall(r.calls, down) {
					t.Fatalf("切换未完成: %+v %v %v", state, err, r.calls)
				}
			} else {
				if m.carPlayNetworkHold.Plan.DisableWiFiUpstream || m.carPlayNetworkHold.Plan.Channel != 1 {
					t.Fatalf("失败未保持旧网络: %+v %v", m.carPlayNetworkHold, err)
				}
				if phase == "offline" && (err != nil || state.Internet || containsCall(r.calls, down)) {
					t.Fatalf("蓝牙无互联网仍切换: %+v %v", state, err)
				}
				if phase == "checkpoint_failure" && (err == nil || containsCall(r.calls, down)) {
					t.Fatal("未持久化就断开 Wi-Fi")
				}
				if phase == "ap_failure" && (err == nil || !containsCall(r.calls, "nmcli --wait 15 connection up uuid "+sta+" ifname wlan0")) {
					t.Fatalf("AP 失败没有恢复 STA: %v %v", err, r.calls)
				}
			}
		})
	}
}
