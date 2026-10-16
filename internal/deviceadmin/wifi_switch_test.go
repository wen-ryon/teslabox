package deviceadmin

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/wen-ryon/teslabox/internal/system"
)

func TestResolveWiFiSwitchAccessPointUsesCompletedScan(t *testing.T) {
	t.Parallel()
	const fields = "-t --escape yes -f IN-USE,SSID,BSSID,SIGNAL,CHAN,SECURITY device wifi list ifname "
	const visible = ":Phone:AA\\:BB\\:CC\\:DD\\:EE\\:01:90:6:WPA2\n"
	tests := []struct {
		name      string
		peer      string
		fresh     string
		shared    bool
		cacheFail bool
		wantScan  bool
		wantError bool
	}{
		{name: "同PHY另一接口缓存命中不扫描", peer: visible, shared: true},
		{name: "所选接口缓存失败仍可用同PHY缓存", peer: visible, shared: true, cacheFail: true},
		{name: "空缓存等待完整扫描结果", fresh: visible, shared: true, wantScan: true},
		{name: "不能使用另一张网卡的缓存", peer: visible, shared: false, wantScan: true, wantError: true},
		{name: "完整扫描仍为空才报告不可见", shared: true, wantScan: true, wantError: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			r := &fakeRunner{responses: map[string]string{
				"nmcli -t --escape yes -f DEVICE,TYPE,STATE device status": "wlan0:wifi:connected\nwlan1:wifi:disconnected\nusb0:ethernet:connected\n",
				"nmcli " + fields + "wlan0 --rescan no":                    test.peer,
				"nmcli --wait 12 " + fields + "wlan1 --rescan yes":         test.fresh,
			}, failures: map[string][]error{}}
			if test.cacheFail {
				r.failures["nmcli "+fields+"wlan1 --rescan no"] = []error{errors.New("接口缓存暂不可读")}
			}
			m := New(r, slog.New(slog.NewTextHandler(io.Discard, nil)))
			m.sameWirelessPHY = func(_, _ string) (bool, bool) { return test.shared, true }
			target, err := m.resolveWiFiSwitchAccessPoint(t.Context(), "wlan1", ConnectRequest{Channel: 6}, "Phone")
			if (err != nil) != test.wantError {
				t.Fatalf("target=%#v, err=%v", target, err)
			}
			if err == nil && (target.SSID != "Phone" || target.Channel != 6) {
				t.Fatalf("错误目标：%#v", target)
			}
			freshCall := "nmcli --wait 12 " + fields + "wlan1 --rescan yes"
			if containsCall(r.calls, freshCall) != test.wantScan {
				t.Fatalf("完整扫描调用=%v, want=%v: %v", containsCall(r.calls, freshCall), test.wantScan, r.calls)
			}
			for _, call := range r.calls {
				if strings.Contains(call, "wifi rescan") || strings.Contains(call, "device disconnect") ||
					strings.Contains(call, "connection up") || strings.Contains(call, "usb0 --rescan") {
					t.Fatalf("解析目标不得提前断连、重复扫描或操作USB：%s", call)
				}
			}
		})
	}
}

func TestGuardReservedAPAutoConnectOnlyProtectsRelatedRadio(t *testing.T) {
	t.Parallel()
	const ap = "11111111-1111-1111-1111-111111111111"
	const station = "22222222-2222-2222-2222-222222222222"
	const peer = "33333333-3333-3333-3333-333333333333"
	tests := []struct {
		name       string
		shared     bool
		known      bool
		activePeer bool
		wantGuard  bool
	}{
		{name: "同PHY预留AP不自动当STA", shared: true, known: true, wantGuard: true},
		{name: "同PHY第二STA必须停止", shared: true, known: true, activePeer: true, wantGuard: true},
		{name: "不同PHY不干预", known: true, activePeer: true},
		{name: "PHY未知不猜测断连", shared: true, activePeer: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			device := ""
			if test.activePeer {
				device = "wlan0"
			}
			r := &fakeRunner{responses: map[string]string{
				"nmcli -t --escape yes -f NAME,UUID,TYPE,DEVICE,AUTOCONNECT connection show": fmt.Sprintf("AP:%s:802-11-wireless::no\nSTA:%s:802-11-wireless:wlan1:yes\nPeer:%s:802-11-wireless:%s:yes\n", ap, station, peer, device),
				"nmcli -t --escape yes -f DEVICE,TYPE,STATE device status":                   "wlan0:wifi:connected\nwlan1:wifi:connected\nusb0:ethernet:connected\n",
				"nmcli -g connection.interface-name connection show uuid " + ap:              "wlan0\n",
				"nmcli -g GENERAL.AUTOCONNECT device show wlan0":                             "yes\n",
			}}
			const detail = "nmcli -g 802-11-wireless.mode,802-11-wireless.ssid,802-11-wireless.band,802-11-wireless.channel,802-11-wireless-security.key-mgmt,ipv4.method,ipv4.addresses connection show uuid "
			r.responses[detail+ap] = "ap\nTeslaBox\na\n149\nwpa-psk\nshared\n192.168.88.1/24\n"
			r.responses[detail+station] = "infrastructure\nPhone\nbg\n0\nwpa-psk\nauto\n\n"
			r.responses[detail+peer] = "infrastructure\nOld\na\n0\nwpa-psk\nauto\n\n"
			m := New(r, slog.New(slog.NewTextHandler(io.Discard, nil)))
			m.sameWirelessPHY = func(_, _ string) (bool, bool) { return test.shared, test.known }
			if err := m.guardReservedAPAutoConnect(t.Context()); err != nil {
				t.Fatal(err)
			}
			if containsCall(r.calls, "nmcli device set wlan0 autoconnect no") != test.wantGuard ||
				containsCall(r.calls, "nmcli device disconnect wlan0") != (test.wantGuard && test.activePeer) {
				t.Fatalf("角色保护行为错误：%v", r.calls)
			}
			for _, call := range r.calls {
				if strings.Contains(call, "connection modify") || strings.Contains(call, "device disconnect wlan1") ||
					strings.Contains(call, "device set usb0") || strings.Contains(call, "connection delete") {
					t.Fatalf("角色保护改变了用户配置或管理通道：%s", call)
				}
			}
		})
	}
}

func TestMatchWiFiSwitchAccessPointHonorsExplicitBSSIDAndChannel(t *testing.T) {
	t.Parallel()
	output := ":Home:AA\\:BB\\:CC\\:DD\\:EE\\:01:95:149:WPA2\n" +
		":Home:AA\\:BB\\:CC\\:DD\\:EE\\:02:60:6:WPA2\n"
	for _, request := range []ConnectRequest{
		{BSSID: "aa:bb:cc:dd:ee:02"},
		{Channel: 6},
	} {
		target, found := matchWiFiSwitchAccessPoint(output, request, "Home")
		if !found || target.Channel != 6 || target.BSSID != "AA:BB:CC:DD:EE:02" {
			t.Fatalf("同名强信号覆盖了指定目标：%#v, %v", target, found)
		}
	}
	if _, found := matchWiFiSwitchAccessPoint(output, ConnectRequest{BSSID: "AA:BB:CC:DD:EE:02", Channel: 149}, "Home"); found {
		t.Fatal("BSSID和信道不匹配不能连接")
	}
}

func TestConnectSameSavedWiFiDoesNotClaimDHCPIsComplete(t *testing.T) {
	t.Parallel()
	const uuid = "22222222-2222-2222-2222-222222222222"
	tests := []struct {
		name      string
		states    []string
		wantError bool
	}{
		{name: "已连接的重复请求不重建", states: []string{"100 (connected)"}},
		{name: "正在DHCP等待同一激活", states: []string{"70 (ip-config)", "100 (connected)"}},
		{name: "认证失败不能返回已连接", states: []string{"60 (need-auth)", "120 (failed)"}, wantError: true},
		{name: "状态未知不能返回已连接", states: []string{""}, wantError: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			r := &fakeRunner{responses: map[string]string{
				"nmcli -t --escape yes -f NAME,UUID,TYPE,DEVICE,AUTOCONNECT connection show":                                                                                                                "Home:" + uuid + ":802-11-wireless:wlan1:yes\n",
				"nmcli -g 802-11-wireless.mode,802-11-wireless.ssid,802-11-wireless.band,802-11-wireless.channel,802-11-wireless-security.key-mgmt,ipv4.method,ipv4.addresses connection show uuid " + uuid: "infrastructure\nHome\nbg\n0\nwpa-psk\nauto\n\n",
				"nmcli -t --escape yes -f DEVICE,TYPE,STATE device status":                                                                                                                                  "wlan1:wifi:connected\n",
				"nmcli -g GENERAL.CON-UUID device show wlan1":                                                                                                                                               uuid,
			}, responseSequences: map[string][]string{
				"nmcli -g GENERAL.STATE device show wlan1": test.states,
			}}
			m := New(r, slog.New(slog.NewTextHandler(io.Discard, nil)))
			if err := m.Connect(t.Context(), ConnectRequest{UUID: uuid}); (err != nil) != test.wantError {
				t.Fatalf("激活中不能被误报成功：%v, calls=%v", err, r.calls)
			}
			for _, call := range r.calls {
				if strings.Contains(call, "device disconnect") || strings.Contains(call, "connection up") ||
					strings.Contains(call, "wifi connect") || strings.Contains(call, "--rescan yes") {
					t.Fatalf("相同请求重置了原激活：%s", call)
				}
			}
		})
	}
}

func TestActivateSavedWiFiWaitTimeoutDoesNotRestartDHCP(t *testing.T) {
	t.Parallel()
	const uuid = "22222222-2222-2222-2222-222222222222"
	args := []string{"--wait", "15", "connection", "up", "uuid", uuid, "ifname", "wlan1"}
	command := "nmcli " + strings.Join(args, " ")
	r := &fakeRunner{responses: map[string]string{
		"nmcli -g GENERAL.CON-UUID device show wlan1": uuid,
	}, responseSequences: map[string][]string{
		"nmcli -g GENERAL.CON-UUID device show wlan1": {"", uuid},
		"nmcli -g GENERAL.STATE device show wlan1":    {"70 (ip-config)", "100 (connected)"},
	}, failures: map[string][]error{command: {&system.CommandError{Code: 3, Output: "Timeout expired"}}}}
	m := New(r, slog.New(slog.NewTextHandler(io.Discard, nil)))
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	if err := m.activateSavedWiFi(ctx, uuid, "wlan1", args); err != nil {
		t.Fatalf("CLI等待超时不等于NM激活失败：%v", err)
	}
	count := 0
	for _, call := range r.calls {
		if call == command {
			count++
		}
	}
	if count != 1 {
		t.Fatalf("不应重新激活正在DHCP的连接：%v", r.calls)
	}
}

func TestActivateNewWiFiTimeoutKeepsTargetIdentity(t *testing.T) {
	t.Parallel()
	const uuid = "22222222-2222-2222-2222-222222222222"
	const other = "33333333-3333-3333-3333-333333333333"
	args := []string{"--wait", "15", "device", "wifi", "connect", "Phone", "ifname", "wlan1"}
	command := "nmcli " + strings.Join(args, " ")
	tests := []struct {
		name      string
		ssid      string
		finalUUID string
		state     string
		wantError bool
	}{
		{name: "同一新网络完成DHCP", ssid: "Phone", finalUUID: uuid, state: "100 (connected)"},
		{name: "另一SSID不能当成功", ssid: "Old", finalUUID: uuid, state: "100 (connected)", wantError: true},
		{name: "等待中配置身份变化不能当成功", ssid: "Phone", finalUUID: other, state: "100 (connected)", wantError: true},
		{name: "认证失败不能无限等待", ssid: "Phone", finalUUID: uuid, state: "120 (failed)", wantError: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			r := &fakeRunner{responses: map[string]string{
				"nmcli -g GENERAL.STATE device show wlan1":                   test.state,
				"nmcli -g 802-11-wireless.ssid connection show uuid " + uuid: test.ssid + "\n",
			}, responseSequences: map[string][]string{
				"nmcli -g GENERAL.CON-UUID device show wlan1": {uuid, test.finalUUID},
			}, failures: map[string][]error{command: {&system.CommandError{Code: 3, Output: "Timeout expired"}}}}
			m := New(r, slog.New(slog.NewTextHandler(io.Discard, nil)))
			if err := m.activateNewWiFi(t.Context(), "wlan1", "Phone", args); (err != nil) != test.wantError {
				t.Fatalf("激活结果身份核对失败：%v, calls=%v", err, r.calls)
			}
			count := 0
			for _, call := range r.calls {
				if call == command {
					count++
				}
				if strings.Contains(call, "device disconnect") {
					t.Fatalf("等待阶段不应重置连接：%s", call)
				}
			}
			if count != 1 {
				t.Fatalf("重复提交连接：%v", r.calls)
			}
		})
	}
}

func TestWaitWiFiUUIDActivationHonorsCanceledContext(t *testing.T) {
	t.Parallel()
	r := &fakeRunner{responses: map[string]string{
		"nmcli -g GENERAL.STATE device show wlan1": "70 (ip-config)",
	}}
	m := New(r, slog.New(slog.NewTextHandler(io.Discard, nil)))
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if err := m.waitWiFiUUIDActivation(ctx, "wlan1", "22222222-2222-2222-2222-222222222222"); !errors.Is(err, context.Canceled) {
		t.Fatalf("取消后仍等待或报告成功：%v", err)
	}
}

func TestPrepareWiFiSwitchDoesNotRestoreDuplicateSTA(t *testing.T) {
	t.Parallel()
	const ap = "11111111-1111-1111-1111-111111111111"
	const station = "22222222-2222-2222-2222-222222222222"
	const peer = "33333333-3333-3333-3333-333333333333"
	r := &fakeRunner{responses: map[string]string{
		"nmcli -t --escape yes -f NAME,UUID,TYPE,DEVICE,AUTOCONNECT connection show": fmt.Sprintf("AP:%s:802-11-wireless::no\nSTA:%s:802-11-wireless:wlan1:yes\nPeer:%s:802-11-wireless:wlan0:yes\n", ap, station, peer),
		"nmcli -t --escape yes -f DEVICE,TYPE,STATE device status":                   "wlan0:wifi:connected\nwlan1:wifi:connected\nusb0:ethernet:connected\n",
		"nmcli -g connection.interface-name connection show uuid " + ap:              "wlan0\n",
		"nmcli -g GENERAL.AUTOCONNECT device show wlan0":                             "yes\n",
	}}
	const detail = "nmcli -g 802-11-wireless.mode,802-11-wireless.ssid,802-11-wireless.band,802-11-wireless.channel,802-11-wireless-security.key-mgmt,ipv4.method,ipv4.addresses connection show uuid "
	r.responses[detail+ap] = "ap\nTeslaBox\na\n149\nwpa-psk\nshared\n192.168.88.1/24\n"
	r.responses[detail+station] = "infrastructure\nPhone\nbg\n0\nwpa-psk\nauto\n\n"
	r.responses[detail+peer] = "infrastructure\nOld\na\n0\nwpa-psk\nauto\n\n"
	m := New(r, slog.New(slog.NewTextHandler(io.Discard, nil)))
	m.sameWirelessPHY = func(_, _ string) (bool, bool) { return true, true }
	m.wirelessPHYInfo = func(context.Context, string) (string, error) {
		return "valid interface combinations:\n * #{ managed } <= 2, #{ AP } <= 1, total <= 2, #channels <= 1\n", nil
	}
	state, err := m.prepareWiFiSwitch(t.Context(), WiFiStatus{Available: true, Interface: "wlan1"}, 6)
	if err != nil {
		t.Fatal(err)
	}
	if err := state.rollback(m, "wlan1"); err != nil {
		t.Fatal(err)
	}
	if containsCall(r.calls, "nmcli --wait 15 connection up uuid "+peer+" ifname wlan0") {
		t.Fatalf("回滚把已纠正的跨信道重复STA再次恢复：%v", r.calls)
	}
	if !containsCall(r.calls, "nmcli --wait 15 connection up uuid "+station+" ifname wlan1") {
		t.Fatalf("回滚没有保留原正常上游：%v", r.calls)
	}
}

func TestConnectWiFiSwitchOrderAndRollback(t *testing.T) {
	t.Parallel()
	const (
		apUUID     = "11111111-1111-1111-1111-111111111111"
		oldUUID    = "22222222-2222-2222-2222-222222222222"
		targetUUID = "33333333-3333-3333-3333-333333333333"
	)
	tests := []struct {
		name          string
		apActive      bool
		oldDevice     string
		actualChannel int
		failure       string
		newNetwork    bool
	}{
		{name: "热点先停上游成功后再开", apActive: true, oldDevice: "wlan1", actualChannel: 11},
		{name: "热点关闭但旧上游占用预留接口", oldDevice: "wlan0", actualChannel: 11},
		{name: "按关联实际信道恢复而不是扫描信道", apActive: true, oldDevice: "wlan1", actualChannel: 1},
		{name: "目标连接失败恢复原上游和热点", apActive: true, oldDevice: "wlan1", failure: "connect"},
		{name: "热点恢复失败不能显示成功", apActive: true, oldDevice: "wlan1", actualChannel: 11, failure: "ap"},
		{name: "实际信道不明确必须回滚", apActive: true, oldDevice: "wlan1", failure: "channel"},
		{name: "回滚失败保留两种错误", apActive: true, oldDevice: "wlan1", failure: "rollback"},
		{name: "新网络也遵循先停后连顺序", apActive: true, oldDevice: "wlan1", actualChannel: 11, newNetwork: true},
		{name: "密码无效不改变现有网络", apActive: true, oldDevice: "wlan1", failure: "invalid", newNetwork: true},
		{name: "停止热点失败不启动目标连接", apActive: true, oldDevice: "wlan1", failure: "stop"},
		{name: "热点命令成功但未广播仍回滚", apActive: true, oldDevice: "wlan1", actualChannel: 11, failure: "broadcast"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			apDevice, auto := "", "no"
			if test.apActive {
				apDevice, auto = "wlan0", "yes"
			}
			profiles := fmt.Sprintf(
				"AP:%s:802-11-wireless:%s:%s\nOld:%s:802-11-wireless:%s:yes\nNew:%s:802-11-wireless::no\n",
				apUUID, apDevice, auto, oldUUID, test.oldDevice, targetUUID,
			)
			runner := &fakeRunner{responses: map[string]string{
				"nmcli -t --escape yes -f NAME,UUID,TYPE,DEVICE,AUTOCONNECT connection show":                                profiles,
				"nmcli -t --escape yes -f DEVICE,TYPE,STATE device status":                                                  "wlan0:wifi:connected\nwlan1:wifi:connected\n",
				"nmcli -g connection.interface-name connection show uuid " + apUUID:                                         "wlan0\n",
				"nmcli -t --escape yes -f IN-USE,SSID,BSSID,SIGNAL,CHAN,SECURITY device wifi list ifname wlan1 --rescan no": ":New:AA\\:BB\\:CC\\:DD\\:EE\\:01:90:11:WPA2\n",
				"/usr/sbin/iw dev wlan1 info": fmt.Sprintf("Interface wlan1\n\ttype managed\n\tchannel %d\n", test.actualChannel),
			}, failures: map[string][]error{}}
			const detailFields = "nmcli -g 802-11-wireless.mode,802-11-wireless.ssid,802-11-wireless.band,802-11-wireless.channel,802-11-wireless-security.key-mgmt,ipv4.method,ipv4.addresses connection show uuid "
			runner.responses[detailFields+apUUID] = "ap\nTeslaBox\na\n161\nwpa-psk\nshared\n192.168.88.1/24\n"
			runner.responses[detailFields+oldUUID] = "infrastructure\nOld\na\n0\nwpa-psk\nauto\n\n"
			runner.responses[detailFields+targetUUID] = "infrastructure\nNew\nbg\n0\nwpa-psk\nauto\n\n"
			apBefore := "Interface wlan0\n\tssid TeslaBox\n\ttype AP\n\tchannel 161\n"
			apAfter := fmt.Sprintf("Interface wlan0\n\tssid TeslaBox\n\ttype AP\n\tchannel %d\n", test.actualChannel)
			if test.failure == "broadcast" {
				apAfter = "Interface wlan0\n\ttype managed\n\tchannel 11\n"
			}
			runner.responseSequences = map[string][]string{
				"/usr/sbin/iw dev wlan0 info": {apBefore, apAfter},
			}
			targetUp := "nmcli --wait 15 connection up uuid " + targetUUID + " ifname wlan1 ap AA:BB:CC:DD:EE:01"
			request := ConnectRequest{UUID: targetUUID}
			if test.newNetwork {
				request = ConnectRequest{SSID: "New", Security: "wpa", Password: "correct-password"}
				targetUp = "nmcli --wait 15 device wifi connect New bssid AA:BB:CC:DD:EE:01 ifname wlan1 name TeslaBox-Upstream-New password correct-password"
			}
			if test.failure == "invalid" {
				request.Password = "short"
			}
			apUp := "nmcli --wait 20 connection up uuid " + apUUID + " ifname wlan0"
			oldUp := "nmcli --wait 15 connection up uuid " + oldUUID + " ifname " + test.oldDevice
			if test.failure == "connect" || test.failure == "rollback" {
				runner.failures[targetUp] = []error{errors.New("目标连接失败")}
			}
			if test.failure == "ap" {
				runner.failures[apUp] = []error{errors.New("热点恢复失败")}
			}
			if test.failure == "rollback" {
				runner.failures[oldUp] = []error{errors.New("原上游恢复失败")}
			}
			if test.failure == "stop" {
				runner.failures["nmcli device disconnect wlan0"] = []error{errors.New("停止热点失败")}
			}
			manager := New(runner, slog.New(slog.NewTextHandler(io.Discard, nil)))
			manager.sameWirelessPHY = func(_, _ string) (bool, bool) { return true, true }
			manager.wirelessPHYInfo = func(context.Context, string) (string, error) {
				return "valid interface combinations:\n * #{ managed } <= 2, #{ AP } <= 1, total <= 2, #channels <= 1\n" +
					" * 2412 MHz [1] (20.0 dBm)\n * 2462 MHz [11] (20.0 dBm)\n * 5805 MHz [161] (20.0 dBm)\n", nil
			}
			err := manager.Connect(context.Background(), request)
			if (err != nil) != (test.failure != "") {
				t.Fatalf("Connect() = %v, failure = %q", err, test.failure)
			}
			index := func(call string) int {
				for i, got := range runner.calls {
					if got == call {
						return i
					}
				}
				return -1
			}
			oldStop := index("nmcli device disconnect " + test.oldDevice)
			targetStart := index(targetUp)
			if test.failure == "invalid" {
				if oldStop >= 0 || index("nmcli device disconnect wlan0") >= 0 || targetStart >= 0 {
					t.Fatalf("无效输入改变了原连接: %#v", runner.calls)
				}
				return
			}
			if test.failure == "stop" {
				if targetStart >= 0 || index(apUp) < 0 {
					t.Fatalf("停止失败仍激活目标或未尝试恢复热点: %#v", runner.calls)
				}
				return
			}
			if oldStop < 0 || targetStart <= oldStop {
				t.Fatalf("旧上游未在目标激活之前停止: %#v", runner.calls)
			}
			if test.apActive {
				apStop := index("nmcli device disconnect wlan0")
				if apStop < 0 || apStop >= targetStart || index(apUp) <= targetStart {
					t.Fatalf("热点未遵循先停、上游激活后再恢复的顺序: %#v", runner.calls)
				}
			} else if index(apUp) >= 0 {
				t.Fatalf("用户关闭的热点不应被重新打开: %#v", runner.calls)
			}
			if test.failure == "" && test.apActive {
				want := fmt.Sprintf(
					"nmcli connection modify uuid %s 802-11-wireless.band bg 802-11-wireless.channel %d",
					apUUID, test.actualChannel,
				)
				if index(want) <= targetStart {
					t.Fatalf("没有在目标激活后同步实际信道: %#v", runner.calls)
				}
			}
			if test.failure != "" && index(oldUp) <= targetStart {
				t.Fatalf("失败没有恢复原上游: %#v", runner.calls)
			}
			if test.failure == "rollback" && (!strings.Contains(err.Error(), "目标连接失败") ||
				!strings.Contains(err.Error(), "原上游恢复失败")) {
				t.Fatalf("回滚错误丢失: %v", err)
			}
		})
	}
}
