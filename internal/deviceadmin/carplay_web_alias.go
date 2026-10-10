package deviceadmin

import (
	"context"
	"errors"
	"net/netip"
	"strings"
)

func validateCarPlayWebAlias(lease CarPlayNetworkLease) error {
	if lease.WebAliasAddress == "" && lease.WebAliasTarget == "" {
		return nil
	}
	public, e1 := netip.ParseAddr(lease.WebAliasAddress)
	target, e2 := netip.ParseAddr(lease.WebAliasTarget)
	if e1 != nil || e2 != nil || !public.Is4() || !public.IsGlobalUnicast() || public.IsPrivate() || !target.Is4() || !target.IsPrivate() || !target.IsGlobalUnicast() {
		return invalidInput("CarPlay HTTPS 本地映射恢复地址无效")
	}
	return nil
}

func carPlayWebAliasRule(lease CarPlayNetworkLease) []string {
	return []string{"-i", lease.Plan.APInterface, "-d", lease.WebAliasAddress, "-p", "tcp", "--dport", "443", "-m", "comment", "--comment", "TeslaBox-CarPlay-HTTPS-" + lease.RuntimeAPUUID, "-j", "DNAT", "--to-destination", lease.WebAliasTarget + ":443"}
}

func carPlayProbeAliasRule(lease CarPlayNetworkLease) []string {
	return []string{"-i", lease.Plan.APInterface, "-d", lease.WebAliasAddress, "-p", "tcp", "--dport", "80", "-m", "comment", "--comment", "TeslaBox-CarPlay-Probe-" + lease.RuntimeAPUUID, "-j", "DNAT", "--to-destination", lease.WebAliasTarget + ":80"}
}

func (m *Manager) prepareCarPlayWebAlias(ctx context.Context, lease CarPlayNetworkLease) error {
	if lease.WebAliasAddress == "" {
		return nil
	}
	if err := validateCarPlayLease(lease); err != nil {
		return err
	}
	for _, rule := range [][]string{carPlayWebAliasRule(lease), carPlayProbeAliasRule(lease)} {
		if _, err := m.run(ctx, "iptables", append([]string{"-t", "nat", "-C", "PREROUTING"}, rule...)...); err == nil {
			continue
		}
		// 仅热点入站的指定地址与端口回到本机，保留普通公网和上游访问。
		if _, err := m.run(ctx, "iptables", append([]string{"-t", "nat", "-I", "PREROUTING", "1"}, rule...)...); err != nil {
			return errors.New("创建 CarPlay 本地网页映射失败")
		}
	}
	return nil
}

func (m *Manager) removeCarPlayWebAlias(ctx context.Context, lease CarPlayNetworkLease) error {
	if lease.WebAliasAddress == "" {
		return nil
	}
	if err := validateCarPlayLease(lease); err != nil {
		return err
	}
	rules, err := m.run(ctx, "iptables", "-t", "nat", "-S", "PREROUTING")
	if err != nil {
		return errors.New("读取 CarPlay HTTPS 本地映射失败")
	}
	var cleanupErrors []error
	for _, rule := range [][]string{carPlayWebAliasRule(lease), carPlayProbeAliasRule(lease)} {
		label := rule[11]
		if !strings.Contains(string(rules), label) {
			continue
		}
		// 精确删除完整规则；即使另一条清理失败，也不能遗留已确认属于自己的规则。
		if _, err := m.run(ctx, "iptables", append([]string{"-t", "nat", "-D", "PREROUTING"}, rule...)...); err != nil {
			cleanupErrors = append(cleanupErrors, errors.New("CarPlay 本地网页映射身份不符或清理失败"))
		}
	}
	return errors.Join(cleanupErrors...)
}

func (m *Manager) carPlayWebAliasTarget(ctx context.Context, uuid string) (string, error) {
	// 关闭的热点状态不会返回地址，必须在持久化恢复记录前读取正式模板。
	data, err := m.run(ctx, "nmcli", "-g", "ipv4.addresses", "connection", "show", "uuid", uuid)
	if err != nil {
		return "", errors.New("读取 CarPlay HTTPS 固定热点地址失败")
	}
	prefix, err := netip.ParsePrefix(strings.TrimSpace(string(data)))
	if err != nil || !prefix.Addr().Is4() || !prefix.Addr().IsPrivate() || !isUsableHostAddress(prefix.Addr(), prefix.Bits()) {
		return "", invalidInput("CarPlay 本地 HTTPS 映射需要固定热点地址")
	}
	return prefix.Addr().String(), nil
}
