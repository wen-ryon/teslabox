package deviceadmin

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/wen-ryon/teslabox/internal/carplaysettings"
)

func testCarPlayLease() CarPlayNetworkLease {
	uuid := "33333333-3333-4333-8333-333333333333"
	return CarPlayNetworkLease{Owner: "carplay", ExpiresAt: time.Now().Add(time.Minute),
		DeviceAutoConnectManaged: true, OriginalDeviceAutoConnect: true,
		RuntimeAPUUID: uuid, RuntimeAPName: "TeslaBox-CarPlay-lease-" + uuid,
		Plan: CarPlayNetworkPlan{APUUID: "11111111-1111-1111-1111-111111111111", APInterface: "ap0",
			Channel: 149, ConcurrencyMode: "same_channel", PauseUpstream: true, UpstreamInterface: "wlan0",
			UpstreamUUID: "22222222-2222-2222-2222-222222222222"}}
}

func TestCarPlayRenewClearsCooldownOnlyForConfirmedSameChannelUpstream(t *testing.T) {
	for _, channel := range []int{6, 11, 0} {
		t.Run(fmt.Sprint(channel), func(t *testing.T) {
			lease := testCarPlayLease()
			lease.Plan.Channel, lease.Plan.PauseUpstream = 6, false
			info := "type managed\n"
			if channel != 0 {
				info += fmt.Sprintf("channel %d (2437 MHz)\n", channel)
			}
			runner := &fakeRunner{responses: map[string]string{
				"nmcli -t -f UUID,DEVICE connection show --active": lease.Plan.UpstreamUUID + ":wlan0\n" + lease.RuntimeAPUUID + ":ap0\n",
				"/usr/sbin/iw dev wlan0 info":                      info,
				"/usr/sbin/iw dev ap0 info":                        "type AP\nchannel 6 (2437 MHz)\n",
			}}
			m := New(runner, nil)
			if err := m.RestoreCarPlayNetworkHold(lease); err != nil {
				t.Fatal(err)
			}
			m.carPlayWiFiRecoveryAfter = time.Now().Add(3 * time.Minute)
			_, err := m.AcquireCarPlayNetwork(context.Background(), lease.Owner, CarPlayNetworkRequest{Channels: []int{6, 11}}, time.Now().Add(time.Minute), func(*CarPlayNetworkLease) error { return nil })
			if err != nil {
				t.Fatal(err)
			}
			if m.carPlayWiFiRecoveryAfter.IsZero() != (channel == 6) {
				t.Fatal("没有按真实同信道连接决定是否结束冷却")
			}
		})
	}
}

func TestCarPlayRenewPreservesOwnedAPWhenUpstreamChannelDisappears(t *testing.T) {
	for _, state := range []struct {
		name, apUUID, apInfo string
		accepted             bool
	}{
		{"上游消失但专属热点稳定", "33333333-3333-4333-8333-333333333333", "type AP\nchannel 6 (2437 MHz)\n", true},
		{"热点被替换", "55555555-5555-4555-8555-555555555555", "type AP\nchannel 6 (2437 MHz)\n", false},
		{"热点信道也改变", "33333333-3333-4333-8333-333333333333", "type AP\nchannel 1 (2412 MHz)\n", false},
		{"热点状态无法确认", "33333333-3333-4333-8333-333333333333", "type managed\n", false},
	} {
		t.Run(state.name, func(t *testing.T) {
			lease := testCarPlayLease()
			lease.Plan.Channel, lease.Plan.PauseUpstream = 6, false
			runner := &fakeRunner{responses: map[string]string{
				"nmcli -t -f UUID,DEVICE connection show --active": lease.Plan.UpstreamUUID + ":wlan0\n" + state.apUUID + ":ap0\n",
				"/usr/sbin/iw dev wlan0 info":                      "type managed\n",
				"/usr/sbin/iw dev ap0 info":                        state.apInfo,
			}}
			m := New(runner, nil)
			if err := m.RestoreCarPlayNetworkHold(lease); err != nil {
				t.Fatal(err)
			}
			saved := false
			renewed, err := m.AcquireCarPlayNetwork(context.Background(), lease.Owner, CarPlayNetworkRequest{Channels: []int{1, 6, 11}}, time.Now().Add(time.Minute), func(*CarPlayNetworkLease) error { saved = true; return nil })
			if (err == nil) != state.accepted || saved != state.accepted {
				t.Fatalf("续租结果错误: %+v %v", renewed, err)
			}
			if state.accepted && (renewed.Plan.PauseUpstream || containsCall(runner.calls, "nmcli --wait 15 connection down uuid "+lease.Plan.UpstreamUUID)) {
				t.Fatal("上游过渡窗口改写了暂停/恢复策略")
			}
		})
	}
}

func TestCarPlayHoldBlocksNetworkChangesAndRecovery(t *testing.T) {
	t.Parallel()
	runner := &fakeRunner{}
	manager := New(runner, nil)
	if err := manager.RestoreCarPlayNetworkHold(testCarPlayLease()); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	for _, change := range []func() error{
		func() error { return manager.Connect(ctx, ConnectRequest{}) },
		func() error { return manager.DisconnectWiFi(ctx) },
		func() error { _, err := manager.Scan(ctx); return err },
		func() error { return manager.SetAutoConnect(ctx, AutoConnectUpdate{}) },
		func() error { return manager.UpdateIPv4(ctx, IPv4Update{}) },
		func() error { return manager.Forget(ctx, testCarPlayLease().Plan.UpstreamUUID) },
		func() error { return manager.UpdateAP(ctx, APUpdate{}) },
		func() error { _, err := manager.ConfigureAPRadio(ctx, "a", 149); return err },
		func() error { return manager.SetAPRuntime(ctx, false) },
		func() error { return manager.RepairAPInterface(ctx) },
	} {
		var operationError *APOperationError
		if err := change(); !errors.As(err, &operationError) || operationError.Code != "carplay_network_reserved" {
			t.Fatalf("占用期间未阻止网络变更: %v", err)
		}
	}
	if changed, _, err := manager.reconcileAutoConnect(ctx); changed || err != nil {
		t.Fatalf("占用期间触发自动恢复: %v, %v", changed, err)
	}
	if len(runner.calls) != 0 {
		t.Fatalf("保护路径仍执行了系统命令: %v", runner.calls)
	}
}

func TestCarPlayRejectsInvalidRestoreRecord(t *testing.T) {
	t.Parallel()
	for _, alter := range []func(*CarPlayNetworkLease){
		func(lease *CarPlayNetworkLease) { lease.Owner = "../other" },
		func(lease *CarPlayNetworkLease) { lease.Plan.APInterface = "../wlan0" },
		func(lease *CarPlayNetworkLease) { lease.RuntimeAPUUID = lease.Plan.APUUID },
		func(lease *CarPlayNetworkLease) { lease.RuntimeAPName = "OtherAP" },
		func(lease *CarPlayNetworkLease) { lease.Plan.UpstreamUUID = "" },
		func(lease *CarPlayNetworkLease) { lease.Plan.ConcurrencyMode = "independent_radio" },
	} {
		lease := testCarPlayLease()
		alter(&lease)
		runner := &fakeRunner{}
		manager := New(runner, nil)
		if err := manager.RestoreCarPlayNetworkHold(lease); err == nil || manager.carPlayNetworkHold != nil || len(runner.calls) != 0 {
			t.Fatal("无效恢复记录产生了保护或系统命令")
		}
	}
}

func TestCarPlayUncertainRecoveryBlocksAllNetworkMutation(t *testing.T) {
	runner := &fakeRunner{}
	manager := New(runner, nil)
	manager.GuardCarPlayRecovery()
	ctx := context.Background()
	for _, change := range []func() error{
		func() error { return manager.Connect(ctx, ConnectRequest{}) },
		func() error { return manager.DisconnectWiFi(ctx) },
		func() error { _, err := manager.Scan(ctx); return err },
		func() error { return manager.SetAutoConnect(ctx, AutoConnectUpdate{}) },
		func() error { return manager.UpdateIPv4(ctx, IPv4Update{}) },
		func() error { return manager.Forget(ctx, testCarPlayLease().Plan.UpstreamUUID) },
		func() error { return manager.UpdateAP(ctx, APUpdate{}) },
		func() error { _, err := manager.ConfigureAPRadio(ctx, "a", 149); return err },
		func() error { return manager.RepairAPInterface(ctx) },
		func() error { return manager.SetAPRuntime(ctx, false) },
		func() error {
			_, err := manager.AcquireCarPlayNetwork(ctx, "carplay", CarPlayNetworkRequest{Channels: []int{149}}, time.Now().Add(time.Minute), func(*CarPlayNetworkLease) error { t.Fatal("损坏时写入恢复日志"); return nil })
			return err
		},
		func() error { return manager.ReleaseCarPlayNetwork(ctx, "carplay", nil) },
	} {
		if err := change(); err == nil {
			t.Fatal("损坏恢复记录后允许网络修改")
		}
	}
	if err := manager.clearManagedProfileBSSIDLocks(ctx); err != nil {
		t.Fatal(err)
	}
	if changed, _, err := manager.reconcileAutoConnect(ctx); err != nil || changed {
		t.Fatal("损坏状态触发了自愈")
	}
	if len(runner.calls) != 0 {
		t.Fatalf("保护路径执行了系统命令: %v", runner.calls)
	}
}

func TestCarPlayLeaseCheckpointPrecedesDisconnect(t *testing.T) {
	t.Parallel()
	lease := testCarPlayLease()
	activeCall := "nmcli -t -f UUID,DEVICE connection show --active"
	disconnectCall := "nmcli --wait 15 connection down uuid " + lease.Plan.UpstreamUUID
	runner := &fakeRunner{responses: map[string]string{activeCall: lease.Plan.UpstreamUUID + ":wlan0\n"}}
	manager := New(runner, nil)
	if err := manager.RestoreCarPlayNetworkHold(lease); err != nil {
		t.Fatal(err)
	}
	writeFailure := errors.New("journal unavailable")
	request := CarPlayNetworkRequest{Channels: []int{149, 157}}
	_, err := manager.AcquireCarPlayNetwork(context.Background(), lease.Owner, request, time.Now().Add(time.Minute),
		func(*CarPlayNetworkLease) error { return writeFailure })
	if !errors.Is(err, writeFailure) || containsCall(runner.calls, disconnectCall) {
		t.Fatalf("日志失败仍断开上游: %v, %v", err, runner.calls)
	}
	var checkpointValue *CarPlayNetworkLease
	_, err = manager.AcquireCarPlayNetwork(context.Background(), lease.Owner, request, time.Now().Add(time.Minute),
		func(value *CarPlayNetworkLease) error {
			if containsCall(runner.calls, disconnectCall) {
				t.Fatal("先断开后保存")
			}
			checkpointValue = value
			return nil
		})
	if err != nil || !containsCall(runner.calls, disconnectCall) {
		t.Fatalf("保存后未暂停: %v, %v", err, runner.calls)
	}
	checkpointValue.Owner = "unrelated"
	if manager.carPlayNetworkHold.Owner != lease.Owner {
		t.Fatal("日志回调的引用可改变网络所有权")
	}
}

func TestCarPlayFirstAcquireCapturesDevicePolicyBeforePause(t *testing.T) {
	t.Parallel()
	const apUUID = "11111111-1111-1111-1111-111111111111"
	const upstreamUUID = "22222222-2222-2222-2222-222222222222"
	runner := &fakeRunner{responses: map[string]string{
		"nmcli -t --escape yes -f NAME,UUID,TYPE,DEVICE,AUTOCONNECT connection show":                                                                                                                        "TeslaBox-5G:" + apUUID + ":802-11-wireless::no\nHome:" + upstreamUUID + ":802-11-wireless:wlan0:yes\n",
		"nmcli -g 802-11-wireless.mode,802-11-wireless.ssid,802-11-wireless.band,802-11-wireless.channel,802-11-wireless-security.key-mgmt,ipv4.method,ipv4.addresses connection show uuid " + apUUID:       "ap\nTeslaBox-5G\na\n149\nwpa-psk\nshared\n192.168.88.1/24\n",
		"nmcli -g 802-11-wireless.mode,802-11-wireless.ssid,802-11-wireless.band,802-11-wireless.channel,802-11-wireless-security.key-mgmt,ipv4.method,ipv4.addresses connection show uuid " + upstreamUUID: "infrastructure\nHome\na\n0\nwpa-psk\nauto\n\n",
		"nmcli -t --escape yes -f DEVICE,TYPE,STATE device status":                                                                                                                                          "wlan0:wifi:connected\nap0:wifi:disconnected\n",
		"/usr/sbin/iw dev wlan0 info":                      "Interface wlan0\n\ttype managed\n\tchannel 48 (5240 MHz)\n",
		"nmcli -g GENERAL.AUTOCONNECT device show wlan0":   "yes\n",
		"nmcli -t -f UUID,DEVICE connection show --active": upstreamUUID + ":wlan0\n",
	}}
	manager := New(runner, nil)
	manager.sameWirelessPHY = func(string, string) (bool, bool) { return true, true }
	manager.wirelessPHYInfo = func(context.Context, string) (string, error) {
		return "* 5745 MHz [149] (20.0 dBm)\nvalid interface combinations:\n * #{ managed } <= 1, #{ AP } <= 1, total <= 2, #channels <= 1\n", nil
	}
	lease, err := manager.AcquireCarPlayNetwork(context.Background(), "carplay", CarPlayNetworkRequest{Channels: []int{149}},
		time.Now().Add(time.Minute), func(value *CarPlayNetworkLease) error {
			if !value.DeviceAutoConnectManaged || !value.OriginalDeviceAutoConnect || !value.Plan.PauseUpstream {
				t.Fatalf("缺少原始状态: %+v", value)
			}
			if containsCall(runner.calls, "nmcli device set wlan0 autoconnect no") || containsCall(runner.calls, "nmcli --wait 15 connection down uuid "+upstreamUUID) {
				t.Fatal("保存前改变了设备或上游")
			}
			return nil
		})
	if err != nil || lease.Plan.UpstreamUUID != upstreamUUID || !validUUID(lease.RuntimeAPUUID) {
		t.Fatalf("首次申请失败: %+v, %v", lease, err)
	}
	if !containsCall(runner.calls, "nmcli device set wlan0 autoconnect no") || !containsCall(runner.calls, "nmcli --wait 15 connection down uuid "+upstreamUUID) {
		t.Fatalf("未按记录暂停设备: %v", runner.calls)
	}
}

func TestCarPlayExclusiveRenewDoesNotDisconnectOwnedAP(t *testing.T) {
	t.Parallel()
	lease := testCarPlayLease()
	lease.Plan.ConcurrencyMode = "exclusive"
	lease.Plan.APInterface = "wlan0"
	runner := &fakeRunner{responses: map[string]string{
		"nmcli -t -f UUID,DEVICE connection show --active": lease.RuntimeAPUUID + ":wlan0\n",
	}}
	manager := New(runner, nil)
	if err := manager.RestoreCarPlayNetworkHold(lease); err != nil {
		t.Fatal(err)
	}
	_, err := manager.AcquireCarPlayNetwork(context.Background(), lease.Owner, CarPlayNetworkRequest{Channels: []int{149}},
		time.Now().Add(time.Minute), func(*CarPlayNetworkLease) error { return nil })
	if err != nil || containsCall(runner.calls, "nmcli device disconnect wlan0") || containsCall(runner.calls, "nmcli --wait 15 connection down uuid "+lease.Plan.UpstreamUUID) {
		t.Fatalf("续租断开了活动 AP: %v, %v", err, runner.calls)
	}
}

func TestCarPlayReleaseHandsOffRecoveryInsteadOfRestoringLostHome(t *testing.T) {
	for _, priority := range []string{"internet", "carplay"} {
		lease := testCarPlayLease()
		r := &fakeRunner{}
		m := New(r, nil)
		if err := m.RestoreCarPlayNetworkHold(lease); err != nil {
			t.Fatal(err)
		}
		m.carPlayWiFiRecoveryUUID = "55555555-5555-4555-8555-555555555555"
		path := filepath.Join(t.TempDir(), "settings.json")
		settings, err := carplaysettings.New(path)
		if err != nil {
			t.Fatal(err)
		}
		value := settings.Settings()
		value.Network, value.RecoveryPriority = "wifi", priority
		if _, err = settings.Save(value); err != nil {
			t.Fatal(err)
		}
		m.carPlaySettingsPath = path
		if err := m.ReleaseCarPlayNetwork(context.Background(), lease.Owner, func(*CarPlayNetworkLease) error { return nil }); err != nil {
			t.Fatal(err)
		}
		called := containsCall(r.calls, "nmcli --wait 15 connection up uuid "+lease.Plan.UpstreamUUID+" ifname wlan0")
		if called != (priority == "carplay") {
			t.Fatal("恢复交接仍强连旧网络，或画面优先改变了恢复策略")
		}
		if !containsCall(r.calls, "nmcli device set wlan0 autoconnect yes") {
			t.Fatal("没有恢复设备自动连接")
		}
	}
}

func TestCarPlayRestoreFailureRetainsRecoveryRecord(t *testing.T) {
	t.Parallel()
	lease := testCarPlayLease()
	activate := "nmcli --wait 15 connection up uuid " + lease.Plan.UpstreamUUID + " ifname wlan0"
	runner := &fakeRunner{failures: map[string][]error{activate: {errors.New("temporarily unavailable"), nil}}}
	manager := New(runner, nil)
	if err := manager.RestoreCarPlayNetworkHold(lease); err != nil {
		t.Fatal(err)
	}
	cleared := false
	checkpoint := func(value *CarPlayNetworkLease) error {
		if value != nil {
			t.Fatal("释放应清除记录")
		}
		cleared = true
		return nil
	}
	if err := manager.ReleaseCarPlayNetwork(context.Background(), lease.Owner, checkpoint); err == nil || cleared || manager.carPlayNetworkHold == nil {
		t.Fatalf("恢复失败后丢失记录: %v", err)
	}
	if err := manager.ReleaseCarPlayNetwork(context.Background(), lease.Owner, checkpoint); err != nil || !cleared || manager.carPlayNetworkHold != nil {
		t.Fatalf("恢复成功后未清理: %v", err)
	}
}

func TestCarPlayRestoresOriginalDeviceAutoConnect(t *testing.T) {
	t.Parallel()
	for _, enabled := range []bool{false, true} {
		lease := testCarPlayLease()
		lease.OriginalDeviceAutoConnect = enabled
		lease.Plan.PauseUpstream = false
		runner := &fakeRunner{}
		manager := New(runner, nil)
		if err := manager.RestoreCarPlayNetworkHold(lease); err != nil {
			t.Fatal(err)
		}
		if err := manager.ReleaseCarPlayNetwork(context.Background(), lease.Owner, func(*CarPlayNetworkLease) error { return nil }); err != nil {
			t.Fatal(err)
		}
		want := "nmcli device set wlan0 autoconnect " + yesNo(enabled)
		if !containsCall(runner.calls, want) {
			t.Fatalf("没有恢复原有自动连接设置: %v", runner.calls)
		}
	}
}

func TestCarPlayCleanupRefusesChangedIdentity(t *testing.T) {
	t.Parallel()
	lease := testCarPlayLease()
	runner := &fakeRunner{responses: map[string]string{
		"nmcli -t --escape yes -f UUID,NAME connection show": lease.RuntimeAPUUID + ":OtherAP\n",
	}}
	manager := New(runner, nil)
	if err := manager.RestoreCarPlayNetworkHold(lease); err != nil {
		t.Fatal(err)
	}
	if err := manager.ReleaseCarPlayNetwork(context.Background(), lease.Owner, func(*CarPlayNetworkLease) error { return nil }); err == nil {
		t.Fatal("错误身份仍被清理")
	}
	for _, call := range runner.calls {
		if strings.Contains(call, "connection down") || strings.Contains(call, "connection delete") || strings.Contains(call, "connection up") {
			t.Fatalf("影响了其他连接: %s", call)
		}
	}
}
