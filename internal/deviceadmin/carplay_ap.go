package deviceadmin

import (
	"context"
	"errors"
	"net/netip"
	"strings"
	"time"
)

// CarPlayAPAccess 只写入接收进程的私有交接文件，不能作为普通接口或日志的响应。
type CarPlayAPAccess struct {
	RuntimeAPUUID string   `json:"runtime_ap_uuid"`
	Interface     string   `json:"interface"`
	SSID          string   `json:"ssid"`
	Password      string   `json:"password"`
	Channel       int      `json:"channel"`
	MACAddress    string   `json:"mac_address"`
	Addresses     []string `json:"addresses"`
}

// CarPlayAPHealthy 只核对租约热点的实际身份与信道，不自动修复正在传输媒体的接口。
func (m *Manager) CarPlayAPHealthy(ctx context.Context, owner string) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.carPlayRecoveryUncertain || m.carPlayNetworkHold == nil || m.carPlayNetworkHold.Owner != owner {
		return false, carPlayNetworkBusy()
	}
	lease := *m.carPlayNetworkHold
	active, err := m.run(ctx, "nmcli", "-t", "-f", "UUID,DEVICE", "connection", "show", "--active")
	if err != nil {
		return false, err
	}
	for _, line := range strings.Split(strings.TrimSpace(string(active)), "\n") {
		fields := splitNMCLI(line)
		if len(fields) == 2 && fields[0] == lease.RuntimeAPUUID && fields[1] == lease.Plan.APInterface {
			runtime := m.activeAPRuntime(ctx, lease.Plan.APInterface)
			return runtime.Type == "AP" && runtime.Channel == lease.Plan.Channel, nil
		}
	}
	return false, nil
}

// StartCarPlayAP 使用租约中已持久化的 UUID 原子创建临时热点，失败时由同一租约回收。
func (m *Manager) StartCarPlayAP(ctx context.Context, owner string) (CarPlayAPAccess, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.startCarPlayAPLocked(ctx, owner)
}

func (m *Manager) startCarPlayAPLocked(ctx context.Context, owner string) (CarPlayAPAccess, error) {
	if m.carPlayRecoveryUncertain || m.carPlayNetworkHold == nil || m.carPlayNetworkHold.Owner != owner || !m.carPlayNetworkHold.ExpiresAt.After(time.Now()) {
		return CarPlayAPAccess{}, carPlayNetworkBusy()
	}
	lease := *m.carPlayNetworkHold
	active, err := m.run(ctx, "nmcli", "-t", "-f", "UUID,DEVICE", "connection", "show", "--active")
	if err != nil {
		return CarPlayAPAccess{}, err
	}
	for _, line := range strings.Split(strings.TrimSpace(string(active)), "\n") {
		fields := splitNMCLI(line)
		if len(fields) == 2 && fields[1] == lease.Plan.APInterface && fields[0] != lease.RuntimeAPUUID {
			return CarPlayAPAccess{}, carPlayNetworkBusy()
		}
	}
	profiles, err := m.run(ctx, "nmcli", "-t", "--escape", "yes", "-f", "UUID,NAME", "connection", "show")
	if err != nil {
		return CarPlayAPAccess{}, err
	}
	exists := false
	for _, line := range strings.Split(strings.TrimSpace(string(profiles)), "\n") {
		fields := splitNMCLI(line)
		if len(fields) != 2 || fields[0] != lease.RuntimeAPUUID {
			continue
		}
		if fields[1] != lease.RuntimeAPName {
			return CarPlayAPAccess{}, invalidInput("CarPlay 热点身份已改变")
		}
		exists = true
	}
	// 凭据查询与创建错误可能包含属性值，固定错误说明防止密码进入 journal。
	profile, err := m.run(ctx, "nmcli", "--escape", "no", "--show-secrets", "-g",
		"802-11-wireless.ssid,802-11-wireless-security.psk,802-11-wireless-security.key-mgmt,ipv4.method,ipv4.addresses",
		"connection", "show", "uuid", lease.Plan.APUUID)
	if err != nil {
		return CarPlayAPAccess{}, errors.New("读取 CarPlay 热点模板失败")
	}
	fields := strings.Split(strings.TrimSuffix(string(profile), "\n"), "\n")
	if len(fields) != 5 || validateSSID(fields[0]) != nil || validatePassword(fields[1]) != nil ||
		fields[2] != "wpa-psk" || fields[3] != "shared" {
		return CarPlayAPAccess{}, invalidInput("CarPlay 需要有效的 WPA2 共享热点模板")
	}
	for _, char := range fields[1] {
		if char > 127 || char == 0 || char == '\r' {
			return CarPlayAPAccess{}, invalidInput("CarPlay 热点密码必须是 8 到 63 个 ASCII 字符")
		}
	}
	if fields[4] != "" {
		prefix, parseErr := netip.ParsePrefix(fields[4])
		if parseErr != nil || !prefix.Addr().Is4() || !isUsableHostAddress(prefix.Addr(), prefix.Bits()) {
			return CarPlayAPAccess{}, invalidInput("CarPlay 热点模板的 IPv4 地址无效")
		}
	}
	mac, err := m.run(ctx, "nmcli", "--escape", "no", "-g", "GENERAL.HWADDR", "device", "show", lease.Plan.APInterface)
	macAddress := strings.TrimSpace(string(mac))
	if err != nil || !validBSSID(macAddress) {
		return CarPlayAPAccess{}, invalidInput("无法确认热点接口的稳定地址")
	}
	if !exists {
		if err := createCarPlayAPProfile(ctx, lease, macAddress, fields[0], fields[1]); err != nil {
			// 创建层只返回固定阶段和属性名称，保留诊断信息而不输出属性值。
			return CarPlayAPAccess{}, err
		}
	} else {
		identity, err := m.run(ctx, "nmcli", "-g", "connection.interface-name,802-11-wireless.mode", "connection", "show", "uuid", lease.RuntimeAPUUID)
		if err != nil || strings.TrimSpace(string(identity)) != lease.Plan.APInterface+"\nap" {
			return CarPlayAPAccess{}, invalidInput("CarPlay 热点接口已改变")
		}
	}
	runtime := m.activeAPRuntime(ctx, lease.Plan.APInterface)
	if m.carPlayDomain != "" {
		if fields[4] == "" {
			return CarPlayAPAccess{}, invalidInput("CarPlay 域名解析需要热点模板配置固定 IPv4 地址")
		}
		prefix, _ := netip.ParsePrefix(fields[4])
		address := prefix.Addr()
		if lease.WebAliasAddress != "" {
			if address.String() != lease.WebAliasTarget {
				return CarPlayAPAccess{}, invalidInput("CarPlay HTTPS 映射目标已改变")
			}
			if err := m.prepareCarPlayWebAlias(ctx, lease); err != nil {
				return CarPlayAPAccess{}, err
			}
			address = netip.MustParseAddr(lease.WebAliasAddress)
		}
		if err := m.prepareCarPlayDNS(lease, address); err != nil {
			return CarPlayAPAccess{}, err
		}
	}
	if runtime.Type != "AP" || runtime.Channel != lease.Plan.Channel || runtime.SSID != fields[0] {
		if _, err := m.run(ctx, "nmcli", "--wait", "20", "connection", "up", "uuid", lease.RuntimeAPUUID, "ifname", lease.Plan.APInterface); err != nil {
			return CarPlayAPAccess{}, errors.New("启动 CarPlay 临时热点失败")
		}
		runtime = m.activeAPRuntime(ctx, lease.Plan.APInterface)
	}
	if runtime.Type != "AP" || runtime.Channel != lease.Plan.Channel || runtime.SSID != fields[0] {
		return CarPlayAPAccess{}, invalidInput("CarPlay 热点没有按租约信道正常广播")
	}
	addresses, err := m.run(ctx, "nmcli", "-g", "IP4.ADDRESS", "device", "show", lease.Plan.APInterface)
	if err != nil {
		return CarPlayAPAccess{}, errors.New("读取 CarPlay 热点地址失败")
	}
	var ipv4 []string
	for _, value := range strings.Split(strings.TrimSpace(string(addresses)), "\n") {
		prefix, err := netip.ParsePrefix(value)
		if err == nil && prefix.Addr().Is4() {
			ipv4 = append(ipv4, value)
		}
	}
	if len(ipv4) == 0 {
		return CarPlayAPAccess{}, invalidInput("CarPlay 热点尚未获得 IPv4 地址")
	}
	return CarPlayAPAccess{RuntimeAPUUID: lease.RuntimeAPUUID, Interface: lease.Plan.APInterface,
		SSID: fields[0], Password: fields[1], Channel: lease.Plan.Channel, MACAddress: strings.ToUpper(macAddress), Addresses: ipv4}, nil
}
