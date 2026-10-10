package deviceadmin

import (
	"context"
	"encoding/json"
	"hash/fnv"
	"net/netip"
	"strconv"
	"strings"

	"github.com/wen-ryon/teslabox/internal/carplaysettings"
)

type carPlayForwardRoute struct {
	Destination string          `json:"dst"`
	Type        string          `json:"type"`
	Gateway     string          `json:"gateway"`
	Device      string          `json:"dev"`
	Table       json.RawMessage `json:"table"`
	Protocol    json.RawMessage `json:"protocol"`
}

type carPlayForwardRule struct {
	Priority      int             `json:"priority"`
	Table         json.RawMessage `json:"table"`
	Interface     string          `json:"iif"`
	InterfaceName string          `json:"iifname"`
	Protocol      json.RawMessage `json:"protocol"`
}

func numericRoutingField(value json.RawMessage) int {
	name := strings.Trim(string(value), "\"")
	if name == "main" {
		return 254
	}
	if name == "local" {
		return 255
	}
	if name == "default" {
		return 253
	}
	parsed, _ := strconv.Atoi(strings.Trim(string(value), "\""))
	return parsed
}

func (m *Manager) carPlayNetworkPreference() (string, string, error) {
	if m.carPlaySettingsPath == "" {
		return "auto", "internet", nil
	}
	settings, err := carplaysettings.New(m.carPlaySettingsPath)
	if err != nil {
		return "", "", err
	}
	value := settings.Settings()
	return value.Network, value.RecoveryPriority, nil
}

func (m *Manager) forwardingInventory(ctx context.Context) ([]carPlayForwardRoute, []carPlayForwardRule, error) {
	routesRaw, err := m.run(ctx, "ip", "-N", "-j", "-4", "route", "show", "table", "all")
	if err != nil {
		return nil, nil, err
	}
	rulesRaw, err := m.run(ctx, "ip", "-N", "-j", "-4", "rule", "show")
	if err != nil {
		return nil, nil, err
	}
	var routes []carPlayForwardRoute
	var rules []carPlayForwardRule
	if err = json.Unmarshal(routesRaw, &routes); err != nil {
		return nil, nil, err
	}
	if err = json.Unmarshal(rulesRaw, &rules); err != nil {
		return nil, nil, err
	}
	return routes, rules, nil
}

func forwardingRuleOwned(rule carPlayForwardRule, lease CarPlayNetworkLease) bool {
	iif := rule.Interface
	if iif == "" {
		iif = rule.InterfaceName
	}
	return rule.Priority == lease.ForwardTable && numericRoutingField(rule.Table) == lease.ForwardTable && iif == lease.Plan.APInterface && numericRoutingField(rule.Protocol) == 242
}

func forwardingRouteOwned(route carPlayForwardRoute, lease CarPlayNetworkLease) bool {
	if numericRoutingField(route.Table) != lease.ForwardTable || route.Destination != "default" || numericRoutingField(route.Protocol) != 242 {
		return false
	}
	return unreachableForwardRoute(route) && route.Device == "" && route.Gateway == "" ||
		(route.Type == "" || route.Type == "unicast" || route.Type == "1") && route.Device == lease.ForwardInterface && route.Gateway == lease.ForwardGateway
}

// ip -N 在板端将不可达路由类型输出为内核数值，恢复时必须同时兼容两种表示。
func unreachableForwardRoute(route carPlayForwardRoute) bool {
	return route.Type == "unreachable" || route.Type == "7"
}

func (m *Manager) removeCarPlayForwarding(ctx context.Context, lease CarPlayNetworkLease) error {
	if lease.ForwardTable == 0 {
		return nil
	}
	routes, rules, err := m.forwardingInventory(ctx)
	if err != nil {
		return err
	}
	for _, rule := range rules {
		if rule.Priority == lease.ForwardTable && !forwardingRuleOwned(rule, lease) {
			return invalidInput("CarPlay 出网规则身份已变化，拒绝清理")
		}
	}
	for _, route := range routes {
		if numericRoutingField(route.Table) == lease.ForwardTable && !forwardingRouteOwned(route, lease) {
			return invalidInput("CarPlay 出网路由身份已变化，拒绝清理")
		}
	}
	table := strconv.Itoa(lease.ForwardTable)
	for _, rule := range rules {
		if forwardingRuleOwned(rule, lease) {
			if _, err = m.run(ctx, "ip", "-4", "rule", "del", "priority", table, "iif", lease.Plan.APInterface, "table", table, "protocol", "242"); err != nil {
				return err
			}
		}
	}
	for _, route := range routes {
		if forwardingRouteOwned(route, lease) {
			args := []string{"-4", "route", "del"}
			if unreachableForwardRoute(route) {
				args = append(args, "unreachable")
			}
			args = append(args, "default", "table", table, "proto", "242")
			if route.Device != "" {
				args = append(args, "dev", route.Device)
			}
			if route.Gateway != "" {
				args = append(args, "via", route.Gateway)
			}
			if _, err = m.run(ctx, "ip", args...); err != nil {
				return err
			}
		}
	}
	return nil
}

// 先安装不可达路由，再打开热点专属策略规则；所选上游不可用时不会继续落回主路由。
func (m *Manager) setCarPlayForwarding(ctx context.Context, lease *CarPlayNetworkLease, device string, checkpoint func(*CarPlayNetworkLease) error) error {
	if lease.ForwardTable == 0 {
		hash := fnv.New32a()
		_, _ = hash.Write([]byte(lease.RuntimeAPUUID))
		candidate := 20000 + int(hash.Sum32()%10000)
		routes, rules, err := m.forwardingInventory(ctx)
		if err != nil {
			return err
		}
		for _, route := range routes {
			if numericRoutingField(route.Table) == candidate {
				return invalidInput("CarPlay 出网表已被其他功能占用")
			}
		}
		for _, rule := range rules {
			if rule.Priority == candidate {
				return invalidInput("CarPlay 出网优先级已被其他功能占用")
			}
		}
		lease.ForwardTable = candidate
		if err = checkpoint(lease); err != nil {
			return err
		}
		copy := *lease
		m.carPlayNetworkHold = &copy
	}
	routes, rules, err := m.forwardingInventory(ctx)
	if err != nil {
		return err
	}
	for _, route := range routes {
		if numericRoutingField(route.Table) == lease.ForwardTable && !forwardingRouteOwned(route, *lease) {
			return invalidInput("CarPlay 出网表身份已变化")
		}
	}
	ruleExists := false
	for _, rule := range rules {
		if rule.Priority == lease.ForwardTable {
			if !forwardingRuleOwned(rule, *lease) {
				return invalidInput("CarPlay 出网规则身份已变化")
			}
			ruleExists = true
		}
	}
	table := strconv.Itoa(lease.ForwardTable)
	if ruleExists {
		for _, route := range routes {
			if !forwardingRouteOwned(route, *lease) {
				continue
			}
			if device == "" && unreachableForwardRoute(route) {
				return nil
			}
			if device != "" && route.Device == device && lease.ForwardInterface == device {
				for _, upstream := range routes {
					if upstream.Destination == "default" && upstream.Device == device && (len(upstream.Table) == 0 || numericRoutingField(upstream.Table) == 254) && upstream.Gateway == lease.ForwardGateway {
						return nil
					}
				}
			}
		}
	}
	if _, err = m.run(ctx, "ip", "-4", "route", "replace", "unreachable", "default", "table", table, "proto", "242"); err != nil {
		return err
	}
	lease.ForwardInterface = ""
	lease.ForwardGateway = ""
	if err = checkpoint(lease); err != nil {
		return err
	}
	copy := *lease
	m.carPlayNetworkHold = &copy
	if !ruleExists {
		if _, err = m.run(ctx, "ip", "-4", "rule", "add", "priority", table, "iif", lease.Plan.APInterface, "table", table, "protocol", "242"); err != nil {
			return err
		}
	}
	if device == "" {
		return nil
	}
	if !carPlayInterfacePattern.MatchString(device) || device == lease.Plan.APInterface {
		return invalidInput("CarPlay 出网接口不合法")
	}
	var chosen *carPlayForwardRoute
	for index := range routes {
		route := &routes[index]
		if route.Destination == "default" && route.Device == device && (len(route.Table) == 0 || numericRoutingField(route.Table) == 254) {
			chosen = route
			break
		}
	}
	if chosen == nil {
		return invalidInput("所选 CarPlay 上游没有 IPv4 默认路由")
	}
	if chosen.Gateway != "" {
		address, parseErr := netip.ParseAddr(chosen.Gateway)
		if parseErr != nil || !address.Is4() || address.IsUnspecified() || address.IsMulticast() {
			return invalidInput("CarPlay 上游网关不合法")
		}
	}
	lease.ForwardInterface = device
	lease.ForwardGateway = chosen.Gateway
	// 记录目标路由后才创建；崩溃恢复同时接受专属不可达路由和这一条完整目标。
	if err = checkpoint(lease); err != nil {
		return err
	}
	copy = *lease
	m.carPlayNetworkHold = &copy
	args := []string{"-4", "route", "replace", "default", "table", table, "proto", "242", "dev", device}
	if chosen.Gateway != "" {
		args = append(args, "via", chosen.Gateway, "onlink")
	}
	_, err = m.run(ctx, "ip", args...)
	return err
}
