package deviceadmin

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/wen-ryon/teslabox/internal/carplaysettings"
)

func TestSwitchToBluetoothDoesNotRestoreUnavailableOldWiFi(t *testing.T) {
	for _, originalBluetooth := range []bool{false, true} {
		lease := testCarPlayLease()
		lease.Plan.DisableWiFiUpstream = originalBluetooth
		activate := "nmcli --wait 15 connection up uuid " + lease.Plan.UpstreamUUID + " ifname wlan0"
		r := &fakeRunner{failures: map[string][]error{activate: {errors.New("旧热点已离开范围")}}}
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
		if err := m.RestoreCarPlayNetworkHold(lease); err != nil {
			t.Fatal(err)
		}
		cleared := false
		err = m.ReleaseCarPlayNetwork(context.Background(), lease.Owner, func(value *CarPlayNetworkLease) error { cleared = value == nil; return nil })
		if err != nil || !cleared || m.carPlayNetworkHold != nil || containsCall(r.calls, activate) {
			t.Fatalf("切到蓝牙仍被旧 Wi-Fi 恢复卡住: %v %v", err, r.calls)
		}
		if !containsCall(r.calls, "nmcli device set wlan0 autoconnect yes") {
			t.Fatal("释放租约丢失原设备策略")
		}
	}
}

func TestBluetoothNetworkPlanPausesSTAAndPrefersCompatible5GHz(t *testing.T) {
	for _, mode := range []string{"same_channel", "multi_channel", "independent_radio", "exclusive"} {
		for _, configured := range []int{1, 149} {
			settings := APSettings{Available: true, Interface: "ap0", UUID: "11111111-1111-1111-1111-111111111111",
				ConcurrencyMode: mode, Channel: configured, AllowedChannels: []int{1, 36, 149}}
			status := WiFiStatus{Interface: "wlan0", CurrentConnection: "Home", currentConnectionUUID: "22222222-2222-2222-2222-222222222222"}
			plan, err := planCarPlayBluetoothTarget(settings, status, []int{1, 36, 149})
			want := 36
			if configured == 149 {
				want = 149
			}
			if err != nil || plan.Channel != want || !plan.PauseUpstream || !plan.DisableWiFiUpstream || plan.UpstreamUUID != status.currentConnectionUUID {
				t.Fatalf("%s: %+v %v", mode, plan, err)
			}
			plan, err = planCarPlayBluetoothTarget(settings, status, []int{1})
			if err != nil || plan.Channel != 1 {
				t.Fatalf("不能使用车机声明之外的信道: %+v %v", plan, err)
			}
		}
	}
}

func TestBluetoothLeaseRenewalKeepsSTAOffAndRestoresOriginalPolicy(t *testing.T) {
	for _, mode := range []string{"same_channel", "independent_radio", "multi_channel"} {
		lease := testCarPlayLease()
		lease.Plan.ConcurrencyMode = mode
		lease.Plan.DisableWiFiUpstream = true
		lease.Plan.PauseUpstream = true
		lease.DeviceAutoConnectManaged = true
		lease.OriginalDeviceAutoConnect = true
		r := &fakeRunner{responses: map[string]string{
			"nmcli -t -f UUID,DEVICE connection show --active": lease.Plan.UpstreamUUID + ":" + lease.Plan.UpstreamInterface,
		}}
		m := New(r, nil)
		if err := m.RestoreCarPlayNetworkHold(lease); err != nil {
			t.Fatal(err)
		}
		written := false
		_, err := m.AcquireCarPlayNetwork(context.Background(), lease.Owner, CarPlayNetworkRequest{Channels: []int{lease.Plan.Channel}}, time.Now().Add(time.Minute), func(*CarPlayNetworkLease) error { written = true; return nil })
		if err != nil || !written || !containsCall(r.calls, "nmcli device set "+lease.Plan.UpstreamInterface+" autoconnect no") || !containsCall(r.calls, "nmcli --wait 15 connection down uuid "+lease.Plan.UpstreamUUID) {
			t.Fatalf("%s: %v %v", mode, err, r.calls)
		}
		for _, force := range []bool{false, true} {
			before := len(r.calls)
			if err := m.Connect(context.Background(), ConnectRequest{ConfirmCarPlayReconnect: force}); err == nil || len(r.calls) != before {
				t.Fatal("蓝牙供网被手动连接绕过")
			}
		}
		before := len(r.calls)
		if changed, _, err := m.reconcileAutoConnect(context.Background()); changed || err != nil || len(r.calls) != before {
			t.Fatal("蓝牙供网时触发了无线自动恢复")
		}
		r.responses["nmcli -t -f UUID,DEVICE connection show --active"] = ""
		if err := m.ReleaseCarPlayNetwork(context.Background(), lease.Owner, func(*CarPlayNetworkLease) error { return nil }); err != nil {
			t.Fatal(err)
		}
		if !containsCall(r.calls, "nmcli device set "+lease.Plan.UpstreamInterface+" autoconnect yes") || !containsCall(r.calls, "nmcli --wait 15 connection up uuid "+lease.Plan.UpstreamUUID+" ifname "+lease.Plan.UpstreamInterface) {
			t.Fatalf("退出后没有恢复原来的无线策略: %v", r.calls)
		}
	}
}

func TestBluetoothFirstAcquirePreservesCompatibleSTAUntilPANVerified(t *testing.T) {
	const ap = "11111111-1111-1111-1111-111111111111"
	const sta = "22222222-2222-2222-2222-222222222222"
	r := &fakeRunner{responses: map[string]string{
		"nmcli -t --escape yes -f NAME,UUID,TYPE,DEVICE,AUTOCONNECT connection show":                                                                                                               "TeslaBox:" + ap + ":802-11-wireless::no\nHome:" + sta + ":802-11-wireless:wlan0:yes\n",
		"nmcli -g 802-11-wireless.mode,802-11-wireless.ssid,802-11-wireless.band,802-11-wireless.channel,802-11-wireless-security.key-mgmt,ipv4.method,ipv4.addresses connection show uuid " + ap:  "ap\nTeslaBox\nbg\n1\nwpa-psk\nshared\n192.168.88.1/24\n",
		"nmcli -g 802-11-wireless.mode,802-11-wireless.ssid,802-11-wireless.band,802-11-wireless.channel,802-11-wireless-security.key-mgmt,ipv4.method,ipv4.addresses connection show uuid " + sta: "infrastructure\nHome\nbg\n1\nwpa-psk\nauto\n\n",
		"nmcli -t --escape yes -f DEVICE,TYPE,STATE device status":                                                                                                                                 "wlan0:wifi:connected\nap0:wifi:disconnected\n",
		"/usr/sbin/iw dev wlan0 info":                      "type managed\nchannel 1 (2412 MHz)\n",
		"nmcli -g GENERAL.AUTOCONNECT device show wlan0":   "yes",
		"nmcli -t -f UUID,DEVICE connection show --active": sta + ":wlan0\n",
	}}
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
	if _, err := store.Save(settings); err != nil {
		t.Fatal(err)
	}
	lease, err := m.AcquireCarPlayNetwork(context.Background(), "carplay", CarPlayNetworkRequest{Channels: []int{1, 36}}, time.Now().Add(time.Minute), func(value *CarPlayNetworkLease) error {
		if value.Plan.DisableWiFiUpstream || !value.DeviceAutoConnectManaged || !value.OriginalDeviceAutoConnect {
			t.Fatal("未保存恢复策略")
		}
		if containsCall(r.calls, "nmcli --wait 15 connection down uuid "+sta) {
			t.Fatal("保存前断开了网络")
		}
		return nil
	})
	if err != nil || lease.Plan.Channel != 1 || containsCall(r.calls, "nmcli --wait 15 connection down uuid "+sta) || !containsCall(r.calls, "nmcli device set wlan0 autoconnect no") {
		t.Fatalf("蓝牙验证前破坏了原 STA: %+v %v %v", lease, err, r.calls)
	}
}
