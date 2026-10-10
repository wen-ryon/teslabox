package deviceadmin

import (
	"context"
	"errors"
	"net/netip"
	"os"
	"strings"
	"testing"
)

func TestCarPlayProbeRuleCleanupPreservesScopeAndContinuesAfterHTTPSFailure(t *testing.T) {
	lease := testCarPlayLease()
	lease.WebAliasAddress, lease.WebAliasTarget = "60.162.38.124", "192.168.88.1"
	https := strings.Join(carPlayWebAliasRule(lease), " ")
	probe := strings.Join(carPlayProbeAliasRule(lease), " ")
	if !strings.Contains(probe, "-i ap0 -d 60.162.38.124 -p tcp --dport 80") || !strings.HasSuffix(probe, "--to-destination 192.168.88.1:80") {
		t.Fatalf("probe scope=%s", probe)
	}
	runner := &fakeRunner{responses: map[string]string{"iptables -t nat -S PREROUTING": "-A PREROUTING " + https + "\n-A PREROUTING " + probe}, failures: map[string][]error{"iptables -t nat -D PREROUTING " + https: {errors.New("changed")}}}
	manager := &Manager{runner: runner}
	if err := manager.removeCarPlayWebAlias(context.Background(), lease); err == nil {
		t.Fatal("ignored HTTPS rule identity failure")
	}
	if !containsCall(runner.calls, "iptables -t nat -D PREROUTING "+probe) {
		t.Fatal("owned probe rule left behind after HTTPS failure")
	}
}

func TestCarPlayWebAliasDNSAndRecovery(t *testing.T) {
	lease := testCarPlayLease()
	lease.WebAliasAddress, lease.WebAliasTarget = "60.162.38.124", "192.168.88.1"
	directory := t.TempDir()
	if err := os.Chmod(directory, 0o755); err != nil {
		t.Fatal(err)
	}
	manager := &Manager{carPlayDomain: "carplay.teslabox.online", carPlayDNSDirectory: directory}
	if err := manager.prepareCarPlayDNS(lease, netip.MustParseAddr(lease.WebAliasAddress)); err != nil {
		t.Fatal(err)
	}
	foreign := lease
	foreign.WebAliasAddress = "60.162.38.125"
	if _, err := carPlayDNSContent(foreign, manager.carPlayDomain, netip.MustParseAddr(lease.WebAliasAddress)); err == nil {
		t.Fatal("接受了租约外的公网地址")
	}
	restarted := &Manager{carPlayDNSDirectory: directory}
	if err := restarted.removeCarPlayDNS(lease); err != nil {
		t.Fatal(err)
	}
	for _, addresses := range [][2]string{{"", "192.168.88.1"}, {"127.0.0.1", "192.168.88.1"}, {"192.168.0.1", "192.168.88.1"}, {"60.162.38.124", "60.162.38.125"}, {"60.162.38.124\n-A INPUT", "192.168.88.1"}} {
		invalid := lease
		invalid.WebAliasAddress, invalid.WebAliasTarget = addresses[0], addresses[1]
		if validateCarPlayLease(invalid) == nil {
			t.Fatalf("接受了无效恢复地址 %q", addresses)
		}
	}
}

func TestCarPlayWebAliasRuleOwnershipAndFailure(t *testing.T) {
	lease := testCarPlayLease()
	lease.WebAliasAddress, lease.WebAliasTarget = "60.162.38.124", "192.168.88.1"
	rule := strings.Join(carPlayWebAliasRule(lease), " ")
	check := "iptables -t nat -C PREROUTING " + rule
	remove := "iptables -t nat -D PREROUTING " + rule
	runner := &fakeRunner{failures: map[string][]error{check: {errors.New("missing")}}, responses: map[string]string{"iptables -t nat -S PREROUTING": "-A PREROUTING " + rule}}
	manager := &Manager{runner: runner}
	if err := manager.prepareCarPlayWebAlias(context.Background(), lease); err != nil {
		t.Fatal(err)
	}
	if !containsCall(runner.calls, "iptables -t nat -I PREROUTING 1 "+rule) {
		t.Fatal("没有创建限定接口、地址、端口和租约身份的规则")
	}
	if err := manager.removeCarPlayWebAlias(context.Background(), lease); err != nil {
		t.Fatal(err)
	}
	if !containsCall(runner.calls, remove) {
		t.Fatal("没有按完整身份删除规则")
	}
	runner.failures[remove] = []error{errors.New("foreign rule changed")}
	if err := manager.removeCarPlayWebAlias(context.Background(), lease); err == nil {
		t.Fatal("忽略了规则身份改变")
	}
	runner.responses["iptables -t nat -S PREROUTING"] = "unrelated rule"
	before := len(runner.calls)
	if err := manager.removeCarPlayWebAlias(context.Background(), lease); err != nil {
		t.Fatal(err)
	}
	if len(runner.calls) != before+1 {
		t.Fatal("清理不存在的规则修改了其他规则")
	}
}

func TestCarPlayWebAliasReadsInactiveTemplateAddress(t *testing.T) {
	runner := &fakeRunner{responses: map[string]string{"nmcli -g ipv4.addresses connection show uuid template": "192.168.88.1/24\n"}}
	manager := &Manager{runner: runner}
	target, err := manager.carPlayWebAliasTarget(context.Background(), "template")
	if err != nil || target != "192.168.88.1" {
		t.Fatalf("target=%q err=%v", target, err)
	}
	runner.responses["nmcli -g ipv4.addresses connection show uuid template"] = ""
	if _, err := manager.carPlayWebAliasTarget(context.Background(), "template"); err == nil {
		t.Fatal("猜测了缺失的热点地址")
	}
}
