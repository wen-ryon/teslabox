package deviceadmin

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"testing"
)

const (
	wifiStatusUUIDA = "11111111-1111-1111-1111-111111111111"
	wifiStatusUUIDB = "22222222-2222-2222-2222-222222222222"
)

func TestParseWiFiConnectionSnapshotRejectsMalformedOrPartialState(t *testing.T) {
	for _, input := range []string{"", "100 (connected)", "connected\n" + wifiStatusUUIDA, "100 (connected)\nnot-a-uuid", "999 (future-state)\n" + wifiStatusUUIDA} {
		if got := parseWiFiConnectionSnapshot(input); got.state != "unknown" {
			t.Fatalf("不完整或未知状态被认作连接: %q %+v", input, got)
		}
	}
	for _, input := range []string{"0\n--", "10 (unmanaged)\n--", "30 (disconnected)\n" + wifiStatusUUIDA, "120 (failed)\n" + wifiStatusUUIDA} {
		if got := parseWiFiConnectionSnapshot(input); got.state != "disconnected" || got.uuid != "" {
			t.Fatalf("未清除失效身份: %q %+v", input, got)
		}
	}
}

func wifiStatusFixture() (*Manager, *fakeRunner) {
	runner := &fakeRunner{responses: map[string]string{
		"nmcli -t --escape yes -f NAME,UUID,TYPE,DEVICE,AUTOCONNECT connection show": "Home:" + wifiStatusUUIDA + ":802-11-wireless:wlan0:yes\nOffice:" + wifiStatusUUIDB + ":802-11-wireless:wlan1:yes\n",
		"nmcli -t --escape yes -f DEVICE,TYPE,STATE device status":                   "wlan0:wifi:connected\nwlan1:wifi:connected\n",
		"nmcli -g 802-11-wireless.mode,802-11-wireless.ssid,802-11-wireless.band,802-11-wireless.channel,802-11-wireless-security.key-mgmt,ipv4.method,ipv4.addresses connection show uuid " + wifiStatusUUIDA: "infrastructure\nHome\na\n0\nwpa-psk\nauto\n\n",
		"nmcli -g 802-11-wireless.mode,802-11-wireless.ssid,802-11-wireless.band,802-11-wireless.channel,802-11-wireless-security.key-mgmt,ipv4.method,ipv4.addresses connection show uuid " + wifiStatusUUIDB: "infrastructure\nOffice\na\n0\nwpa-psk\nauto\n\n",
		"nmcli --escape no -g GENERAL.STATE,GENERAL.CON-UUID device show wlan0": "100 (connected)\n" + wifiStatusUUIDA + "\n",
	}}
	return New(runner, slog.New(slog.NewTextHandler(io.Discard, nil)), NetworkConfig{WiFiInterface: "wlan0", APInterface: "wlan1"}), runner
}

func TestWiFiStatusOnlyMarksSelectedStationConnected(t *testing.T) {
	t.Parallel()
	manager, _ := wifiStatusFixture()
	status, err := manager.WiFiStatus(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	active := 0
	for _, network := range status.SavedNetworks {
		if network.Active {
			active++
		}
	}
	if active != 1 || status.CurrentConnection != "Home" {
		t.Fatalf("不能将预留接口的普通客户端同时标成当前连接：%#v", status)
	}
}

func TestWiFiStatusDoesNotReportDHCPOrStaleProfileAsConnected(t *testing.T) {
	for _, test := range []struct{ name, runtime string }{
		{"仍在获取地址", "70 (ip-config)\n" + wifiStatusUUIDA + "\n"},
		{"设备已断开但配置残留", "30 (disconnected)\n--\n"},
		{"实际配置已变", "100 (connected)\n33333333-3333-3333-3333-333333333333\n"},
		{"状态无法确认", ""},
	} {
		t.Run(test.name, func(t *testing.T) {
			manager, runner := wifiStatusFixture()
			runner.responses["nmcli --escape no -g GENERAL.STATE,GENERAL.CON-UUID device show wlan0"] = test.runtime
			status, err := manager.WiFiStatus(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			if status.CurrentConnection != "" {
				t.Fatalf("误报已连接：%#v", status)
			}
			for _, network := range status.SavedNetworks {
				if network.Active {
					t.Fatalf("误报活动配置：%#v", network)
				}
			}
		})
	}
}

func TestForgetConnectedWiFiDeletesOnlyRequestedProfile(t *testing.T) {
	manager, runner := wifiStatusFixture()
	if err := manager.Forget(context.Background(), wifiStatusUUIDA); err != nil {
		t.Fatal(err)
	}
	if !containsCall(runner.calls, "nmcli connection delete uuid "+wifiStatusUUIDA) {
		t.Fatal("未删除指定连接")
	}
	if containsCall(runner.calls, "nmcli device disconnect wlan1") || containsCall(runner.calls, "nmcli connection delete uuid "+wifiStatusUUIDB) {
		t.Fatal("影响了其他接口或连接")
	}
	if !manager.manualWiFiDisconnect {
		t.Fatal("忘记当前连接后后台协调不应立刻将它重新接回")
	}
}

func TestForgetFailureDoesNotChangeManualDisconnectState(t *testing.T) {
	manager, runner := wifiStatusFixture()
	runner.failures = map[string][]error{"nmcli connection delete uuid " + wifiStatusUUIDA: {errors.New("删除失败")}}
	if err := manager.Forget(context.Background(), wifiStatusUUIDA); err == nil {
		t.Fatal("必须返回真实删除错误")
	}
	if manager.manualWiFiDisconnect {
		t.Fatal("删除失败时不应伪造成功状态")
	}
}

func TestDisconnectWiFiProfileRejectsStalePage(t *testing.T) {
	manager, runner := wifiStatusFixture()
	if err := manager.DisconnectWiFiProfile(context.Background(), wifiStatusUUIDB); err == nil {
		t.Fatal("旧页面不能断开另一网络")
	}
	if containsCall(runner.calls, "nmcli device disconnect wlan0") {
		t.Fatal("误断开当前网络")
	}
	if err := manager.DisconnectWiFiProfile(context.Background(), wifiStatusUUIDA); err != nil {
		t.Fatal(err)
	}
	if !containsCall(runner.calls, "nmcli device disconnect wlan0") {
		t.Fatal("未断开目标接口")
	}
}

func TestDisconnectWiFiProfileCanCancelDHCP(t *testing.T) {
	manager, runner := wifiStatusFixture()
	runner.responses["nmcli --escape no -g GENERAL.STATE,GENERAL.CON-UUID device show wlan0"] = "70 (ip-config)\n" + wifiStatusUUIDA + "\n"
	if err := manager.DisconnectWiFiProfile(context.Background(), wifiStatusUUIDA); err != nil {
		t.Fatal(err)
	}
	if !containsCall(runner.calls, "nmcli device disconnect wlan0") {
		t.Fatal("未取消正在获取地址的连接")
	}
}
