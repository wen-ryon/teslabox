package deviceadmin

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

func TestCarPlayStartupWaitsForAssociatingUpstreamChannel(t *testing.T) {
	calls := 0
	channel := carPlayStartupChannel(context.Background(), "same_channel", true, func(context.Context) int {
		calls++
		if calls == 1 {
			return 0
		}
		return 149
	})
	if channel != 149 || calls != 2 {
		t.Fatalf("没有等待真实关联信道: channel=%d calls=%d", channel, calls)
	}
}

func TestCarPlayStartupChannelCancellationAndUnrelatedModes(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()
	if got := carPlayStartupChannel(ctx, "same_channel", true, func(context.Context) int { return 0 }); got != 0 || ctx.Err() == nil {
		t.Fatal("取消后仍等待或猜测了信道")
	}
	for _, test := range []struct {
		mode      string
		connected bool
		channel   int
	}{{"same_channel", false, 0}, {"independent_radio", true, 0}, {"same_channel", true, 149}} {
		calls := 0
		got := carPlayStartupChannel(context.Background(), test.mode, test.connected, func(context.Context) int {
			calls++
			return test.channel
		})
		if got != test.channel || calls != 1 {
			t.Fatalf("不需要等待的网络被延迟: %+v calls=%d", test, calls)
		}
	}
}

func TestCarPlayNetworkPolicy(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name, mode, code string
		upstream, active int
		connected        bool
		channel          int
		pause            bool
	}{
		{"独立网卡上游 2.4 GHz", "independent_radio", "", 1, 0, true, 149, false},
		{"多信道保持热点", "multi_channel", "", 44, 157, true, 157, false},
		{"同信道优先兼容上游", "same_channel", "", 157, 0, true, 157, false},
		{"同信道上游不兼容", "same_channel", "", 48, 0, true, 149, true},
		{"同信道上游 2.4 GHz", "same_channel", "", 1, 0, true, 149, true},
		{"同信道保护活动热点", "same_channel", "", 149, 157, true, 157, true},
		{"互斥接口暂停上游", "exclusive", "", 149, 0, true, 149, true},
		{"未联网不要求暂停", "exclusive", "", 0, 0, false, 149, false},
		{"活动热点不兼容", "multi_channel", "active_ap_channel_incompatible", 1, 44, true, 0, false},
		{"能力未知拒绝", "unknown", "capability_unavailable", 149, 0, true, 0, false},
		{"共享上游信道未知", "same_channel", "upstream_channel_unavailable", 0, 0, true, 0, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			settings := APSettings{Available: true, Interface: "ap0", UUID: "11111111-1111-1111-1111-111111111111",
				ConcurrencyMode: test.mode, AllowedChannels: []int{36, 44, 149, 157},
				Enabled: test.active != 0, ActiveChannel: test.active}
			status := WiFiStatus{Interface: "wlan0"}
			if test.connected {
				status.CurrentConnection = "Home"
				status.currentConnectionUUID = "22222222-2222-2222-2222-222222222222"
			}
			plan, err := planCarPlayNetwork(settings, status, test.upstream, []int{149, 153, 157, 161})
			if test.code != "" {
				var operationError *APOperationError
				if !errors.As(err, &operationError) || operationError.Code != test.code {
					t.Fatalf("错误 = %v，预期 %s", err, test.code)
				}
				return
			}
			if err != nil || plan.Channel != test.channel || plan.PauseUpstream != test.pause {
				t.Fatalf("规划 = %+v，错误 = %v", plan, err)
			}
			if plan.UpstreamUUID != status.currentConnectionUUID || plan.APInterface != "ap0" {
				t.Fatalf("恢复身份或热点接口错误: %+v", plan)
			}
		})
	}
}

func TestCarPlayNetworkRejectsMissingIntersectionAndIdentity(t *testing.T) {
	t.Parallel()
	settings := APSettings{Available: true, Interface: "ap0", UUID: "11111111-1111-1111-1111-111111111111",
		ConcurrencyMode: "same_channel", AllowedChannels: []int{44}}
	status := WiFiStatus{Interface: "wlan0", CurrentConnection: "Home"}
	_, err := planCarPlayNetwork(settings, status, 149, []int{149, 157})
	var operationError *APOperationError
	if !errors.As(err, &operationError) || operationError.Code != "no_carplay_channel" {
		t.Fatalf("没有可用信道时仍允许规划: %v", err)
	}
	settings.AllowedChannels = []int{149}
	_, err = planCarPlayNetwork(settings, status, 149, []int{149})
	if !errors.As(err, &operationError) || operationError.Code != "upstream_identity_unavailable" {
		t.Fatalf("丢失恢复身份时仍允许规划: %v", err)
	}
}

func TestCarPlayNetworkPlanningDoesNotChangeNetwork(t *testing.T) {
	t.Parallel()
	const apUUID = "11111111-1111-1111-1111-111111111111"
	runner := &fakeRunner{responses: map[string]string{
		"nmcli -t --escape yes -f NAME,UUID,TYPE,DEVICE,AUTOCONNECT connection show":                                                                                                                  "TeslaBox-5G:" + apUUID + ":802-11-wireless::no\n",
		"nmcli -g 802-11-wireless.mode,802-11-wireless.ssid,802-11-wireless.band,802-11-wireless.channel,802-11-wireless-security.key-mgmt,ipv4.method,ipv4.addresses connection show uuid " + apUUID: "ap\nTeslaBox-5G\na\n149\nwpa-psk\nshared\n192.168.88.1/24\n",
		"nmcli -t --escape yes -f DEVICE,TYPE,STATE device status":                                                                                                                                    "ap0:wifi:disconnected\nwlan0:wifi:disconnected\n",
	}}
	manager := New(runner, nil)
	manager.sameWirelessPHY = func(string, string) (bool, bool) { return false, true }
	configureAllowedAP(manager, runner, "ap0", "TeslaBox-5G", 149)
	plan, err := manager.PlanCarPlayNetwork(context.Background(), CarPlayNetworkRequest{Channels: []int{149, 157}})
	if err != nil || plan.Channel != 149 || plan.PauseUpstream {
		t.Fatalf("规划 = %+v，错误 = %v", plan, err)
	}
	enumerations := 0
	for _, call := range runner.calls {
		if call == "nmcli -t --escape yes -f NAME,UUID,TYPE,DEVICE,AUTOCONNECT connection show" {
			enumerations++
		}
		if strings.Contains(call, "device wifi list") {
			t.Fatalf("固定信道启动仍读取管理页扫描列表: %s", call)
		}
		for _, mutation := range []string{" modify ", " disconnect ", " rescan ", " connection up ", " connection down "} {
			if strings.Contains(" "+call+" ", mutation) {
				t.Fatalf("只读规划修改了网络: %s", call)
			}
		}
	}
	if enumerations != 1 {
		t.Fatalf("固定信道规划重复枚举配置: %d", enumerations)
	}
}

func TestCarPlayChannelsRequireExplicitVehicleSelection(t *testing.T) {
	t.Parallel()
	for _, channels := range [][]int{nil, {}, {0}, {14}, {35}, {178}, {6, 6}, {149, 149}, {196}, make([]int, 65)} {
		if err := validateCarPlayChannels(channels); err == nil {
			t.Fatalf("接受了无效车机信道: %v", channels)
		}
	}
}

func TestCarPlaySameChannelSupports24GHzWithoutPausingUpstream(t *testing.T) {
	settings := APSettings{Available: true, Interface: "ap0", UUID: "11111111-1111-1111-1111-111111111111", ConcurrencyMode: "same_channel", AllowedChannels: []int{1, 6, 11, 149}}
	status := WiFiStatus{Interface: "wlan0", CurrentConnection: "Phone", currentConnectionUUID: "22222222-2222-2222-2222-222222222222"}
	for _, upstream := range []int{6, 149} {
		plan, err := planCarPlayNetwork(settings, status, upstream, []int{1, 6, 11})
		if err != nil {
			t.Fatal(err)
		}
		if upstream == 6 && (plan.Channel != 6 || plan.PauseUpstream) {
			t.Fatalf("同信道上游被中断: %+v", plan)
		}
		if upstream == 149 && (plan.Channel != 1 || !plan.PauseUpstream) {
			t.Fatalf("跨频段单 PHY 未隔离: %+v", plan)
		}
	}
	if err := validateCarPlayChannels([]int{1, 6, 11}); err != nil {
		t.Fatal(err)
	}
}
