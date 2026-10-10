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

func TestResetCarPlayPANPreservesAPAndRejectsChangedOwnership(t *testing.T) {
	for _, scenario := range []string{"success", "other_owner", "expired", "other_phone", "changed_profile", "delete_failed", "no_pan", "forward_failed", "forward_success", "checkpoint_failed"} {
		t.Run(scenario, func(t *testing.T) {
			lease := testCarPlayLease()
			lease.RuntimePANUUID = "44444444-4444-4444-8444-444444444444"
			lease.RuntimePANAddress = "AA:BB:CC:DD:EE:FF"
			profile := lease.RuntimePANUUID + ":" + carPlayPANName(lease) + "\n"
			if scenario == "changed_profile" {
				profile = lease.RuntimePANUUID + ":Other\n"
			}
			if scenario == "no_pan" {
				lease.RuntimePANUUID, lease.RuntimePANAddress = "", ""
			}
			if scenario == "forward_failed" || scenario == "forward_success" || scenario == "checkpoint_failed" {
				lease.ForwardTable, lease.ForwardSource = 22000, "bluetooth"
				lease.ForwardInterface, lease.ForwardGateway = "bnep0", "172.20.10.1"
			}
			runner := &fakeRunner{responses: map[string]string{
				"nmcli -t --escape yes -f UUID,NAME connection show":                                                                profile,
				"nmcli --escape no -g connection.type,bluetooth.type,bluetooth.bdaddr connection show uuid " + lease.RuntimePANUUID: "bluetooth\npanu\nAA:BB:CC:DD:EE:FF",
				"ip -N -j -4 route show table all":                                                                                  "[]", "ip -N -j -4 rule show": "[]",
			}, failures: map[string][]error{}}
			deleteCall := "nmcli connection delete uuid " + lease.RuntimePANUUID
			if scenario == "delete_failed" {
				runner.failures[deleteCall] = []error{errors.New("delete failed")}
			}
			if scenario == "forward_failed" {
				runner.failures["ip -4 route replace unreachable default table 22000 proto 242"] = []error{errors.New("route failed")}
			}
			m := New(runner, nil)
			if err := m.RestoreCarPlayNetworkHold(lease); err != nil {
				t.Fatal(err)
			}
			owner, phone := lease.Owner, lease.RuntimePANAddress
			if scenario == "no_pan" {
				phone = "AA:BB:CC:DD:EE:FF"
			}
			if scenario == "other_owner" {
				owner = "other"
			}
			if scenario == "other_phone" {
				phone = "11:22:33:44:55:66"
			}
			if scenario == "expired" {
				m.carPlayNetworkHold.ExpiresAt = time.Now().Add(-time.Second)
			}
			err := m.ResetCarPlayPAN(context.Background(), owner, phone, func(*CarPlayNetworkLease) error {
				if scenario == "checkpoint_failed" {
					return errors.New("checkpoint failed")
				}
				return nil
			})
			wantSuccess := scenario == "success" || scenario == "no_pan" || scenario == "forward_success"
			if (err == nil) != wantSuccess {
				t.Fatalf("result: %v", err)
			}
			deleted := false
			for _, call := range runner.calls {
				if call == deleteCall {
					deleted = true
					continue
				}
				if strings.Contains(call, " delete ") || strings.Contains(call, " disconnect") || strings.Contains(call, " connection down ") || strings.Contains(call, " connection up ") {
					t.Fatalf("恢复操作越界: %s", call)
				}
			}
			if deleted != (scenario == "success" || scenario == "delete_failed" || scenario == "forward_success") {
				t.Fatalf("unexpected deletion: %v", runner.calls)
			}
			if m.carPlayNetworkHold.RuntimeAPUUID != lease.RuntimeAPUUID || m.carPlayNetworkHold.Plan != lease.Plan {
				t.Fatal("恢复改变了热点租约")
			}
			if scenario == "forward_success" && m.carPlayNetworkHold.ForwardInterface != "" {
				t.Fatal("删除 PAN 后仍指向旧接口")
			}
		})
	}
}

func TestCarPlayConnectionInternetParsesDNSList(t *testing.T) {
	for _, tc := range []struct {
		name   string
		output string
		want   string
	}{
		{"single", "223.5.5.5\n", "223.5.5.5"},
		{"nmcli_pipe", "223.5.5.5 | 119.29.29.29\n", "223.5.5.5,119.29.29.29"},
		{"lines", "223.5.5.5\n119.29.29.29\n", "223.5.5.5,119.29.29.29"},
		{"invalid_and_ipv6", "-- | 2001:4860:4860::8888 | invalid | 223.5.5.5", "223.5.5.5"},
		{"limit", "223.5.5.5 | 119.29.29.29 | 1.1.1.1", "223.5.5.5,119.29.29.29"},
		{"empty", "--\n", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			runner := &fakeRunner{responses: map[string]string{"nmcli -g IP4.DNS connection show uuid test": tc.output}}
			m := New(runner, nil)
			called := false
			m.carPlayInternetProbe = func(_ context.Context, device string, servers []string) bool {
				called = true
				if device != "eth0" || strings.Join(servers, ",") != tc.want {
					t.Fatalf("unexpected probe: %s %v", device, servers)
				}
				return true
			}
			got := m.carPlayConnectionInternet(context.Background(), "test", "eth0")
			if got != (tc.want != "") || called != (tc.want != "") {
				t.Fatalf("internet=%v probe=%v want DNS=%q", got, called, tc.want)
			}
		})
	}
}

func TestCarPlayUpstreamPreservesWorkingEthernet(t *testing.T) {
	lease := testCarPlayLease()
	runner := &fakeRunner{responses: map[string]string{
		"nmcli -t --escape yes -f UUID,TYPE,DEVICE connection show --active":         lease.RuntimeAPUUID + ":802-11-wireless:ap0\n55555555-5555-4555-8555-555555555555:802-3-ethernet:eth0\n",
		"nmcli -g IP4.DNS connection show uuid 55555555-5555-4555-8555-555555555555": "192.168.0.1\n",
	}}
	m := New(runner, nil)
	m.carPlayWiFiRecoveryAfter = time.Now().Add(3 * time.Minute)
	if err := m.RestoreCarPlayNetworkHold(lease); err != nil {
		t.Fatal(err)
	}
	m.carPlayInternetProbe = func(_ context.Context, name string, dns []string) bool {
		return name == "eth0" && dns[0] == "192.168.0.1"
	}
	m.carPlayPANProfileCreate = func(context.Context, string, []byte) error { t.Fatal("已有互联网仍创建 PAN"); return nil }
	state, err := m.EnsureCarPlayUpstream(context.Background(), lease.Owner, "AA:BB:CC:DD:EE:FF", func(*CarPlayNetworkLease) error { t.Fatal("已有互联网仍修改租约"); return nil })
	if err != nil || !state.Internet || state.Source != "existing_upstream" {
		t.Fatalf("优先级错误: %+v %v", state, err)
	}
	if !m.carPlayWiFiRecoveryAfter.IsZero() {
		t.Fatal("已恢复互联网仍保留旧恢复冷却")
	}
}

func TestCarPlayChannelRecoveryReportsTargetBeforeChangingAP(t *testing.T) {
	lease := testCarPlayLease()
	lease.Plan.Channel, lease.Plan.PauseUpstream = 6, false
	runner := &fakeRunner{responses: map[string]string{
		"nmcli -t --escape yes -f NAME,UUID,TYPE,DEVICE,AUTOCONNECT connection show": "Phone:" + lease.Plan.UpstreamUUID + ":802-11-wireless::yes\n",
		"nmcli -g 802-11-wireless.mode,802-11-wireless.ssid,802-11-wireless.band,802-11-wireless.channel,802-11-wireless-security.key-mgmt,ipv4.method,ipv4.addresses connection show uuid " + lease.Plan.UpstreamUUID: "infrastructure\nPhone\nbg\n0\nnone\nauto\n\n",
		"nmcli -t --escape yes -f DEVICE,TYPE,STATE device status":                                                  "wlan0:wifi:disconnected\nap0:wifi:connected\n",
		"nmcli -t --escape yes -f IN-USE,SSID,BSSID,SIGNAL,CHAN,SECURITY device wifi list ifname wlan0 --rescan no": ":Phone:AA\\:BB\\:CC\\:DD\\:EE\\:01:70:11:--\n",
	}}
	m := New(runner, nil)
	if err := m.RestoreCarPlayNetworkHold(lease); err != nil {
		t.Fatal(err)
	}
	state, err := m.EnsureCarPlayUpstream(context.Background(), lease.Owner, "AA:BB:CC:DD:EE:FF", func(*CarPlayNetworkLease) error { t.Fatal("只报告目标时不能修改租约"); return nil })
	if err != nil || state.Source != "wifi_recovery_required" || state.RecoveryUUID != lease.Plan.UpstreamUUID || state.RecoveryChannel != 11 {
		t.Fatalf("没有报告新信道: %+v %v", state, err)
	}
	if m.carPlayNetworkHold.Plan.Channel != 6 {
		t.Fatal("媒体仍运行时改变了信道")
	}
	// 切换为画面优先后，即使上游已发现新信道，也不得请求重建或扫描。
	settingsPath := filepath.Join(t.TempDir(), "settings.json")
	settings, err := carplaysettings.New(settingsPath)
	if err != nil {
		t.Fatal(err)
	}
	value := settings.Settings()
	value.Network, value.RecoveryPriority = "wifi", "carplay"
	if _, err = settings.Save(value); err != nil {
		t.Fatal(err)
	}
	m.carPlaySettingsPath = settingsPath
	runner.responses["ip -N -j -4 route show table all"] = "[]"
	runner.responses["ip -N -j -4 rule show"] = "[]"
	m.carPlayWiFiRecoveryAfter = time.Time{}
	before := len(runner.calls)
	state, err = m.EnsureCarPlayUpstream(context.Background(), lease.Owner, "AA:BB:CC:DD:EE:FF", func(*CarPlayNetworkLease) error { return nil })
	if err != nil || state.RecoveryUUID != "" || state.Internet {
		t.Fatalf("画面优先仍请求迁移: %+v %v", state, err)
	}
	for _, call := range runner.calls[before:] {
		if strings.Contains(call, "device wifi rescan") || strings.Contains(call, "connection up") || strings.Contains(call, "device disconnect") {
			t.Fatalf("画面优先改动无线连接: %s", call)
		}
	}
	m.carPlaySettingsPath = ""
	m.carPlayNetworkHold = &lease
	m.carPlayWiFiRecoveryAfter = time.Now().Add(3 * time.Minute)
	for _, call := range runner.calls {
		if strings.Contains(call, "connection down") || strings.Contains(call, "connection up") || strings.Contains(call, "device disconnect") {
			t.Fatalf("报告阶段破坏活动会话: %s", call)
		}
	}
	m.carPlayPANProfileCreate = func(context.Context, string, []byte) error { return errors.New("隔离测试不创建 PAN") }
	_, _ = m.EnsureCarPlayUpstream(context.Background(), lease.Owner, "AA:BB:CC:DD:EE:FF", func(*CarPlayNetworkLease) error { return nil })
	scans := 0
	for _, call := range runner.calls {
		if strings.Contains(call, "device wifi rescan") {
			scans++
		}
	}
	if scans != 1 {
		t.Fatalf("恢复请求冷却期内反复扫描: %d", scans)
	}
}

func TestCarPlayRecoveryAcceptsReturnedSameChannelHotspot(t *testing.T) {
	lease := testCarPlayLease()
	lease.Plan.Channel = 44
	profiles := []SavedNetwork{{UUID: lease.Plan.UpstreamUUID, SSID: "Phone", AutoConnect: true}}
	points := []AccessPoint{{SSID: "Phone", Channel: 44, Signal: 65}}
	if identity, channel := carPlayWiFiRecoveryTarget(lease, profiles, points); identity != lease.Plan.UpstreamUUID || channel != 44 {
		t.Fatal("同信道热点重新出现后未安排恢复")
	}
	points[0].Active = true
	if identity, _ := carPlayWiFiRecoveryTarget(lease, profiles, points); identity != "" {
		t.Fatal("仍连接同信道热点时不应重建 CarPlay")
	}
}

func TestCarPlayRecoveryUsesOrderedSavedNetworksAndSkipsDisabled(t *testing.T) {
	lease := testCarPlayLease()
	lease.Plan.Channel = 6
	profiles := []SavedNetwork{{UUID: lease.Plan.UpstreamUUID, SSID: "Phone", AutoConnect: true},
		{UUID: "55555555-5555-4555-8555-555555555555", SSID: "Other", AutoConnect: true}}
	points := []AccessPoint{{SSID: "Phone", Channel: 11, Signal: 20}, {SSID: "Other", Channel: 1, Signal: 99}}
	if target, channel := carPlayWiFiRecoveryTarget(lease, profiles, points); target != lease.Plan.UpstreamUUID || channel != 11 {
		t.Fatal("切换到了无关网络")
	}
	profiles[0].AutoConnect = false
	if target, _ := carPlayWiFiRecoveryTarget(lease, profiles, points); target != profiles[1].UUID {
		t.Fatal("旧网络禁用时没有选择其他允许自动连接的网络")
	}
	profiles[1].AutoConnect = false
	profiles[0].AutoConnect, profiles[0].RequiresPassword = true, true
	if target, _ := carPlayWiFiRecoveryTarget(lease, profiles, points); target != "" {
		t.Fatal("恢复了缺少密码的网络")
	}
}

func TestCarPlayRecoveryFindsOtherSavedUpstreamAfterLeavingHome(t *testing.T) {
	for _, noPrevious := range []bool{false, true} {
		lease := testCarPlayLease()
		lease.Plan.Channel, lease.Plan.PauseUpstream = 149, false
		if noPrevious {
			lease.Plan.UpstreamUUID = ""
		}
		home, phone := "22222222-2222-2222-2222-222222222222", "55555555-5555-4555-8555-555555555555"
		r := &fakeRunner{responses: map[string]string{
			"nmcli -t --escape yes -f NAME,UUID,TYPE,DEVICE,AUTOCONNECT connection show":                                "Home:" + home + ":802-11-wireless::yes\nPhone:" + phone + ":802-11-wireless::yes\n",
			"nmcli -t --escape yes -f DEVICE,TYPE,STATE device status":                                                  "wlan0:wifi:disconnected\nap0:wifi:connected\n",
			"nmcli -t --escape yes -f IN-USE,SSID,BSSID,SIGNAL,CHAN,SECURITY device wifi list ifname wlan0 --rescan no": ":Phone:AA\\:BB\\:CC\\:DD\\:EE\\:01:70:149:--\n",
		}}
		for _, n := range []struct{ uuid, ssid string }{{home, "Home"}, {phone, "Phone"}} {
			r.responses["nmcli -g 802-11-wireless.mode,802-11-wireless.ssid,802-11-wireless.band,802-11-wireless.channel,802-11-wireless-security.key-mgmt,ipv4.method,ipv4.addresses connection show uuid "+n.uuid] = "infrastructure\n" + n.ssid + "\na\n0\nnone\nauto\n\n"
		}
		m := New(r, nil)
		m.carPlayVehicleChannels = []int{149}
		if err := m.RestoreCarPlayNetworkHold(lease); err != nil {
			t.Fatal(err)
		}
		state, err := m.EnsureCarPlayUpstream(context.Background(), lease.Owner, "AA:BB:CC:DD:EE:FF", func(*CarPlayNetworkLease) error { return nil })
		if err != nil || state.RecoveryUUID != phone || state.RecoveryChannel != 149 {
			t.Fatalf("没有选择手机热点（无原上游=%v）: %+v %v", noPrevious, state, err)
		}
		m.carPlayWiFiRecoveryAfter = time.Time{}
		m.carPlayWiFiFailures = map[string]time.Time{phone: time.Now().Add(3 * time.Minute)}
		m.carPlayPANProfileCreate = func(context.Context, string, []byte) error { return errors.New("测试不拨蓝牙") }
		state, _ = m.EnsureCarPlayUpstream(context.Background(), lease.Owner, "AA:BB:CC:DD:EE:FF", func(*CarPlayNetworkLease) error { return nil })
		if state.RecoveryUUID != "" {
			t.Fatal("冷却期又选择了失败网络")
		}
		m.carPlayWiFiRecoveryAfter = time.Time{}
		m.carPlayWiFiFailures[phone] = time.Now().Add(-time.Second)
		m.carPlayVehicleChannels = []int{1, 6, 11}
		state, _ = m.EnsureCarPlayUpstream(context.Background(), lease.Owner, "AA:BB:CC:DD:EE:FF", func(*CarPlayNetworkLease) error { return nil })
		if state.RecoveryUUID != "" {
			t.Fatal("选择了不兼容车机的信道")
		}
		for _, call := range r.calls {
			if strings.Contains(call, "connection up") || strings.Contains(call, "connection down") {
				t.Fatalf("仍有媒体租约就切换网络: %s", call)
			}
		}
	}
}

func TestCarPlayRecoveryFreshScanFailureKeepsAPAndDoesNotUseStaleHome(t *testing.T) {
	lease := testCarPlayLease()
	r := &fakeRunner{responses: map[string]string{
		"nmcli -t --escape yes -f NAME,UUID,TYPE,DEVICE,AUTOCONNECT connection show": "Home:" + lease.Plan.UpstreamUUID + ":802-11-wireless::yes\n",
		"nmcli -g 802-11-wireless.mode,802-11-wireless.ssid,802-11-wireless.band,802-11-wireless.channel,802-11-wireless-security.key-mgmt,ipv4.method,ipv4.addresses connection show uuid " + lease.Plan.UpstreamUUID: "infrastructure\nHome\na\n0\nnone\nauto\n\n",
		"nmcli -t --escape yes -f DEVICE,TYPE,STATE device status":                                                  "wlan0:wifi:disconnected\nap0:wifi:connected\n",
		"nmcli -t --escape yes -f IN-USE,SSID,BSSID,SIGNAL,CHAN,SECURITY device wifi list ifname wlan0 --rescan no": ":Home:AA\\:BB\\:CC\\:DD\\:EE\\:01:70:149:--\n",
	}, failures: map[string][]error{"nmcli --wait 12 device wifi rescan ifname wlan0": {errors.New("扫描失败")}}}
	m := New(r, nil)
	if err := m.RestoreCarPlayNetworkHold(lease); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "settings.json")
	settings, err := carplaysettings.New(path)
	if err != nil {
		t.Fatal(err)
	}
	value := settings.Settings()
	value.Network = "wifi"
	if _, err = settings.Save(value); err != nil {
		t.Fatal(err)
	}
	m.carPlaySettingsPath = path
	r.responses["ip -N -j -4 route show table all"] = "[]"
	r.responses["ip -N -j -4 rule show"] = "[]"
	state, err := m.EnsureCarPlayUpstream(context.Background(), lease.Owner, "AA:BB:CC:DD:EE:FF", func(*CarPlayNetworkLease) error { return nil })
	if err != nil || state.RecoveryUUID != "" || m.carPlayNetworkHold == nil {
		t.Fatalf("扫描失败仍按旧缓存切网: %+v %v", state, err)
	}
	if !m.carPlayWiFiRecoveryAfter.After(time.Now()) {
		t.Fatal("失败没有设置冷却")
	}
}

func TestCarPlayPANCheckpointPrecedesCreationAndRetainsRecoveryOnFailure(t *testing.T) {
	lease := testCarPlayLease()
	runner := &fakeRunner{responses: map[string]string{}}
	m := New(runner, nil)
	if err := m.RestoreCarPlayNetworkHold(lease); err != nil {
		t.Fatal(err)
	}
	var saved *CarPlayNetworkLease
	m.carPlayPANProfileCreate = func(_ context.Context, uuid string, address []byte) error {
		if saved == nil || saved.RuntimePANUUID != uuid || len(address) != 6 {
			t.Fatal("创建前未保存恢复身份")
		}
		return errors.New("测试创建失败")
	}
	_, err := m.EnsureCarPlayUpstream(context.Background(), lease.Owner, "AA:BB:CC:DD:EE:FF", func(value *CarPlayNetworkLease) error { copy := *value; saved = &copy; return nil })
	if err == nil || saved == nil || saved.RuntimePANUUID == "" || m.carPlayNetworkHold.RuntimePANUUID != saved.RuntimePANUUID {
		t.Fatalf("丢失恢复身份: %v", err)
	}
	if err := validateCarPlayLease(*saved); err != nil {
		t.Fatal(err)
	}
}

func TestCarPlayPANRejectsForeignProfileDuringCleanup(t *testing.T) {
	lease := testCarPlayLease()
	lease.RuntimePANUUID, lease.RuntimePANAddress = "44444444-4444-4444-4444-444444444444", "AA:BB:CC:DD:EE:FF"
	for _, identity := range []string{"bluetooth\npanu\n00:11:22:33:44:55", "802-3-ethernet\npanu\nAA:BB:CC:DD:EE:FF"} {
		runner := &fakeRunner{responses: map[string]string{
			"nmcli -t --escape yes -f UUID,NAME connection show":                                                                lease.RuntimePANUUID + ":" + carPlayPANName(lease),
			"nmcli --escape no -g connection.type,bluetooth.type,bluetooth.bdaddr connection show uuid " + lease.RuntimePANUUID: identity,
		}}
		if err := New(runner, nil).removeCarPlayPAN(context.Background(), lease); err == nil {
			t.Fatal("接受了被修改的供网配置")
		}
		for _, call := range runner.calls {
			if strings.Contains(call, "delete") {
				t.Fatal("删除了身份不符的配置")
			}
		}
	}
}

func TestCarPlayPANProbesItsIPInterfaceAndRetainsEstablishedLink(t *testing.T) {
	for _, tc := range []struct {
		name   string
		online bool
	}{{"互联网可用", true}, {"互联网暂时不可用", false}} {
		t.Run(tc.name, func(t *testing.T) {
			online := tc.online
			lease := testCarPlayLease()
			lease.RuntimePANUUID, lease.RuntimePANAddress = "44444444-4444-4444-4444-444444444444", "AA:BB:CC:DD:EE:FF"
			runner := &fakeRunner{responses: map[string]string{
				"nmcli -t --escape yes -f UUID,NAME connection show":                                                                lease.RuntimePANUUID + ":" + carPlayPANName(lease),
				"nmcli --escape no -g connection.type,bluetooth.type,bluetooth.bdaddr connection show uuid " + lease.RuntimePANUUID: "bluetooth\npanu\n" + lease.RuntimePANAddress,
				"nmcli --escape no -g bluetooth.bdaddr,bluetooth.type connection show uuid " + lease.RuntimePANUUID:                 lease.RuntimePANAddress + "\npanu",
				"nmcli -g GENERAL.DEVICES connection show uuid " + lease.RuntimePANUUID:                                             "phone-device",
				"nmcli -g IP4.DNS connection show uuid " + lease.RuntimePANUUID:                                                     "172.20.10.1",
			}}
			m := New(runner, nil)
			if err := m.RestoreCarPlayNetworkHold(lease); err != nil {
				t.Fatal(err)
			}
			m.carPlayPANInterface = func(context.Context, string) (string, error) { return "bnep0", nil }
			m.carPlayInternetProbe = func(_ context.Context, name string, dns []string) bool {
				if name != "bnep0" || dns[0] != "172.20.10.1" {
					t.Fatal("探测未绑定蓝牙 IP 接口")
				}
				return online
			}
			state, err := m.EnsureCarPlayUpstream(context.Background(), lease.Owner, lease.RuntimePANAddress, func(*CarPlayNetworkLease) error { t.Fatal("活动 PAN 不应改写身份"); return nil })
			if err != nil || state.Internet != online || state.Source != "bluetooth" {
				t.Fatalf("供网状态错误: %+v %v", state, err)
			}
			deleted := false
			for _, call := range runner.calls {
				deleted = deleted || strings.Contains(call, "connection delete uuid "+lease.RuntimePANUUID)
			}
			if deleted {
				t.Fatalf("互联网探测结果不能删除已建立的 PAN: %v", runner.calls)
			}
		})
	}
}

func TestCarPlayPANRecoversInternetWithoutReconnectingEstablishedLink(t *testing.T) {
	lease := testCarPlayLease()
	lease.RuntimePANUUID, lease.RuntimePANAddress = "44444444-4444-4444-4444-444444444444", "AA:BB:CC:DD:EE:FF"
	runner := &fakeRunner{responses: map[string]string{
		"nmcli -t --escape yes -f UUID,NAME connection show":                                                                lease.RuntimePANUUID + ":" + carPlayPANName(lease),
		"nmcli --escape no -g connection.type,bluetooth.type,bluetooth.bdaddr connection show uuid " + lease.RuntimePANUUID: "bluetooth\npanu\n" + lease.RuntimePANAddress,
		"nmcli --escape no -g bluetooth.bdaddr,bluetooth.type connection show uuid " + lease.RuntimePANUUID:                 lease.RuntimePANAddress + "\npanu",
		"nmcli -g GENERAL.DEVICES connection show uuid " + lease.RuntimePANUUID:                                             "phone-device",
		"nmcli -g IP4.DNS connection show uuid " + lease.RuntimePANUUID:                                                     "172.20.10.1",
	}}
	m := New(runner, nil)
	if err := m.RestoreCarPlayNetworkHold(lease); err != nil {
		t.Fatal(err)
	}
	interfaceValid := true
	m.carPlayPANInterface = func(context.Context, string) (string, error) {
		if !interfaceValid {
			return "", errors.New("蓝牙 IP 接口已失效")
		}
		return "bnep0", nil
	}
	online := false
	m.carPlayInternetProbe = func(context.Context, string, []string) bool { return online }
	checkpoint := func(*CarPlayNetworkLease) error { t.Fatal("不应重建活动 PAN 的身份"); return nil }
	for _, phase := range []struct {
		name   string
		online bool
	}{{"暂时未联网", false}, {"蜂窝网络恢复", true}} {
		t.Run(phase.name, func(t *testing.T) {
			online = phase.online
			state, err := m.EnsureCarPlayUpstream(context.Background(), lease.Owner, lease.RuntimePANAddress, checkpoint)
			if err != nil || state.Source != "bluetooth" || state.Interface != "bnep0" || state.Internet != online {
				t.Fatalf("供网状态错误: %+v %v", state, err)
			}
		})
	}
	for _, call := range runner.calls {
		if strings.Contains(call, "connection delete") || strings.Contains(call, "connection up") || strings.Contains(call, "device disconnect") {
			t.Fatalf("短暂断网导致蓝牙连接被拆除或重建: %s", call)
		}
	}
	interfaceValid = false
	_, err := m.EnsureCarPlayUpstream(context.Background(), lease.Owner, lease.RuntimePANAddress, checkpoint)
	if err == nil {
		t.Fatal("蓝牙接口实际失效时必须报告失败")
	}
	deleted := false
	for _, call := range runner.calls {
		deleted = deleted || call == "nmcli connection delete uuid "+lease.RuntimePANUUID
		if strings.Contains(call, "device disconnect") {
			t.Fatal("不能断开手机的整个蓝牙连接")
		}
	}
	if !deleted {
		t.Fatal("接口实际失效后未清理专属 PAN")
	}
}

func TestCarPlayPANAcceptsRenamedInterfaceWithoutDeletingConnection(t *testing.T) {
	lease := testCarPlayLease()
	lease.RuntimePANUUID, lease.RuntimePANAddress = "44444444-4444-4444-4444-444444444444", "AA:BB:CC:DD:EE:FF"
	runner := &fakeRunner{responses: map[string]string{
		"nmcli -t --escape yes -f UUID,NAME connection show":                                                                lease.RuntimePANUUID + ":" + carPlayPANName(lease),
		"nmcli --escape no -g connection.type,bluetooth.type,bluetooth.bdaddr connection show uuid " + lease.RuntimePANUUID: "bluetooth\npanu\n" + lease.RuntimePANAddress,
		"nmcli --escape no -g bluetooth.bdaddr,bluetooth.type connection show uuid " + lease.RuntimePANUUID:                 lease.RuntimePANAddress + "\npanu",
		"nmcli -g GENERAL.DEVICES connection show uuid " + lease.RuntimePANUUID:                                             "phone-device",
		"nmcli -g IP4.DNS connection show uuid " + lease.RuntimePANUUID:                                                     "172.20.10.1",
	}}
	m := New(runner, nil)
	if err := m.RestoreCarPlayNetworkHold(lease); err != nil {
		t.Fatal(err)
	}
	interfaceValid := true
	m.carPlayPANInterface = func(context.Context, string) (string, error) {
		if !interfaceValid {
			return "", errors.New("蓝牙 IP 接口已失效")
		}
		return "enxc41375dedfde", nil
	}
	online := false
	m.carPlayInternetProbe = func(context.Context, string, []string) bool { return online }
	checkpoint := func(*CarPlayNetworkLease) error { t.Fatal("不应重建活动 PAN 的身份"); return nil }
	for _, phase := range []struct {
		name   string
		online bool
	}{{"暂时未联网", false}, {"蜂窝网络恢复", true}} {
		t.Run(phase.name, func(t *testing.T) {
			online = phase.online
			state, err := m.EnsureCarPlayUpstream(context.Background(), lease.Owner, lease.RuntimePANAddress, checkpoint)
			if err != nil || state.Source != "bluetooth" || state.Interface != "enxc41375dedfde" || state.Internet != online {
				t.Fatalf("供网状态错误: %+v %v", state, err)
			}
		})
	}
	for _, call := range runner.calls {
		if strings.Contains(call, "connection delete") || strings.Contains(call, "connection up") || strings.Contains(call, "device disconnect") {
			t.Fatalf("短暂断网导致蓝牙连接被拆除或重建: %s", call)
		}
	}
	interfaceValid = false
	_, err := m.EnsureCarPlayUpstream(context.Background(), lease.Owner, lease.RuntimePANAddress, checkpoint)
	if err == nil {
		t.Fatal("蓝牙接口实际失效时必须报告失败")
	}
	deleted := false
	for _, call := range runner.calls {
		deleted = deleted || call == "nmcli connection delete uuid "+lease.RuntimePANUUID
		if strings.Contains(call, "device disconnect") {
			t.Fatal("不能断开手机的整个蓝牙连接")
		}
	}
	if !deleted {
		t.Fatal("接口实际失效后未清理专属 PAN")
	}
}
