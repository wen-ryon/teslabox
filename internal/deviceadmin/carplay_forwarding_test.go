package deviceadmin

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
)

func TestManualForwardingDenyPrecedesRuleAndRecordsOwnedIdentity(t *testing.T) {
	lease := testCarPlayLease()
	runner := &fakeRunner{responses: map[string]string{
		"ip -N -j -4 route show table all": "[]", "ip -N -j -4 rule show": "[]",
	}}
	m := New(runner, nil)
	saved := 0
	err := m.setCarPlayForwarding(context.Background(), &lease, "", func(value *CarPlayNetworkLease) error {
		saved++
		if value.ForwardTable < 20000 || value.ForwardTable >= 30000 {
			t.Fatal("优先级没有位于主路由之前")
		}
		return nil
	})
	if err != nil || saved < 1 {
		t.Fatalf("未创建专属离线路由: %v", err)
	}
	joined := strings.Join(runner.calls, "\n")
	if strings.Index(joined, "route replace unreachable") > strings.Index(joined, "rule add priority") {
		t.Fatal("规则提前开放导致主路由回退")
	}
	if m.carPlayNetworkHold.ForwardTable != lease.ForwardTable {
		t.Fatal("恢复身份未保留")
	}
}

func TestForwardingRepeatedHealthyCheckDoesNotInterruptAndForeignRulesAreProtected(t *testing.T) {
	lease := testCarPlayLease()
	lease.ForwardTable = 22001
	lease.ForwardInterface = "wlan0"
	lease.ForwardGateway = "10.0.0.1"
	routes := fmt.Sprintf(`[{"dst":"default","gateway":"10.0.0.1","dev":"wlan0","table":254,"protocol":16},{"dst":"default","gateway":"10.0.0.1","dev":"wlan0","table":%d,"protocol":242}]`, lease.ForwardTable)
	rules := fmt.Sprintf(`[{"priority":%d,"table":%d,"iif":"ap0","protocol":242}]`, lease.ForwardTable, lease.ForwardTable)
	runner := &fakeRunner{responses: map[string]string{"ip -N -j -4 route show table all": routes, "ip -N -j -4 rule show": rules}}
	m := New(runner, nil)
	if err := m.setCarPlayForwarding(context.Background(), &lease, "wlan0", func(*CarPlayNetworkLease) error { t.Fatal("健康检查不应重复保存路由"); return nil }); err != nil {
		t.Fatal(err)
	}
	for _, call := range runner.calls {
		if strings.Contains(call, "replace") || strings.Contains(call, " add ") {
			t.Fatal("健康检查改动了活动路由")
		}
	}
	runner.calls = nil
	runner.responses["ip -N -j -4 rule show"] = strings.ReplaceAll(rules, "ap0", "other0")
	if err := m.removeCarPlayForwarding(context.Background(), lease); err == nil {
		t.Fatal("接受了外部规则")
	}
	for _, call := range runner.calls {
		if strings.Contains(call, " del ") {
			t.Fatal("清理了外部路由")
		}
	}
	if numericRoutingField(json.RawMessage(`"main"`)) != 254 {
		t.Fatal("未兼容主路由表名称")
	}
}

func TestForwardingOwnsNumericUnreachableTypeReportedByBoard(t *testing.T) {
	lease := testCarPlayLease()
	lease.ForwardTable = 22001
	lease.ForwardSource = "wifi"
	route := carPlayForwardRoute{Destination: "default", Type: "7", Table: json.RawMessage(`"22001"`), Protocol: json.RawMessage(`"242"`)}
	if !forwardingRouteOwned(route, lease) {
		t.Fatal("未认领板端实际的不可达路由类型")
	}
	lease.ForwardGateway = "10.0.0.1"
	if validateCarPlayLease(lease) == nil {
		t.Fatal("接受了没有接口的恢复网关")
	}
}
