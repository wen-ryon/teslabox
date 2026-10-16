package deviceadmin

import (
	"fmt"
	"net/netip"
	"strings"
	"time"
)

// EthernetIPSettings 区分持久配置与运行时租约，地址统一采用 CIDR，避免掩码换算歧义。
type EthernetIPSettings struct {
	Method        string   `json:"method"`
	Addresses     []string `json:"addresses"`
	Gateway       string   `json:"gateway"`
	DNS           []string `json:"dns"`
	IgnoreAutoDNS bool     `json:"ignore_auto_dns"`
	NeverDefault  bool     `json:"never_default"`
	RouteMetric   int64    `json:"route_metric"`
}

// EthernetLinkMode 仅列出驱动通过 ethtool 明确报告支持的模式。
type EthernetLinkMode struct {
	Speed  uint32 `json:"speed"`
	Duplex string `json:"duplex"`
}

// EthernetSettings 是普通独立以太网配置，不承载企业认证、网桥或绑定设置。
type EthernetSettings struct {
	Name          string             `json:"name"`
	AutoConnect   bool               `json:"auto_connect"`
	MTU           uint32             `json:"mtu"`
	IPv4          EthernetIPSettings `json:"ipv4"`
	IPv6          EthernetIPSettings `json:"ipv6"`
	AutoNegotiate bool               `json:"auto_negotiate"`
	Speed         uint32             `json:"speed"`
	Duplex        string             `json:"duplex"`
	WakeOnLAN     uint32             `json:"wake_on_lan"`
}

// EthernetProfile 保留配置 UUID，用于防止页面刷新前后的配置身份混淆。
type EthernetProfile struct {
	UUID     string           `json:"uuid"`
	Active   bool             `json:"active"`
	Editable bool             `json:"editable"`
	Reason   string           `json:"reason,omitempty"`
	Settings EthernetSettings `json:"settings"`
}

// EthernetDevice 把链路、实际 IP 与保存的配置分开，不以“已有配置”冒充已联网。
type EthernetDevice struct {
	Kind           string             `json:"kind"`
	Interface      string             `json:"interface"`
	Managed        bool               `json:"managed"`
	State          string             `json:"state"`
	StateCode      uint32             `json:"state_code"`
	Carrier        bool               `json:"carrier"`
	Speed          uint32             `json:"speed"`
	Duplex         string             `json:"duplex"`
	MAC            string             `json:"mac"`
	PermanentMAC   string             `json:"permanent_mac"`
	MTU            uint32             `json:"mtu"`
	Driver         string             `json:"driver"`
	ConnectionUUID string             `json:"connection_uuid"`
	IPv4           EthernetIPSettings `json:"ipv4"`
	IPv6           EthernetIPSettings `json:"ipv6"`
	Profiles       []EthernetProfile  `json:"profiles"`
	LinkModes      []EthernetLinkMode `json:"link_modes"`
	AutoNegotiate  *bool              `json:"auto_negotiate,omitempty"`
	WakeOnLANModes string             `json:"wake_on_lan_modes"`
	HardwareNotice string             `json:"hardware_notice,omitempty"`
}

// EthernetTrial 由 NetworkManager 持有回退计时器，网页或主程序退出不会取消回退。
type EthernetTrial struct {
	Token     string    `json:"token"`
	Interface string    `json:"interface"`
	UUID      string    `json:"uuid"`
	Action    string    `json:"action"`
	ExpiresAt time.Time `json:"expires_at"`
}

// EthernetStatus 同时支持板载和 USB 以太网，不把桥接、VLAN 或 WiFi 当成普通有线口。
type EthernetStatus struct {
	Devices []EthernetDevice `json:"devices"`
	Pending *EthernetTrial   `json:"pending,omitempty"`
}

// EthernetUpdate 保存指定配置；修改活动配置必须通过可回退的应用流程。
type EthernetUpdate struct {
	Interface string           `json:"interface"`
	UUID      string           `json:"uuid"`
	Apply     bool             `json:"apply"`
	Settings  EthernetSettings `json:"settings"`
}

// EthernetAction 将连接、断开限定到页面看到的设备与配置身份。
type EthernetAction struct {
	Interface string `json:"interface"`
	UUID      string `json:"uuid"`
	Action    string `json:"action"`
}

func validateEthernetSettings(value EthernetSettings) error {
	if strings.TrimSpace(value.Name) == "" || len(value.Name) > 128 || strings.ContainsAny(value.Name, "\x00\r\n") {
		return &InputError{Message: "有线连接名称必须为 1～128 字节，不能包含换行"}
	}
	if value.MTU != 0 && (value.MTU < 68 || value.MTU > 65535 || value.IPv6.Method != "disabled" && value.MTU < 1280) {
		return &InputError{Message: "MTU 应为 0（自动）或 68～65535；启用 IPv6 时至少为 1280"}
	}
	if value.Speed == 0 && value.Duplex != "" || value.Speed != 0 && value.Duplex != "full" && value.Duplex != "half" {
		return &InputError{Message: "速率和全双工／半双工必须同时设置，或同时留为自动"}
	}
	// 默认、忽略及硬件唤醒位不能混用；安全唤醒密码不属于普通设置范围。
	if value.WakeOnLAN != 1 && value.WakeOnLAN != 0x8000 && value.WakeOnLAN&^uint32(0x7e) != 0 {
		return &InputError{Message: "Wake-on-LAN 配置无效"}
	}
	if err := validateEthernetIP(value.IPv4, false); err != nil {
		return err
	}
	return validateEthernetIP(value.IPv6, true)
}

func validateEthernetIP(value EthernetIPSettings, ipv6 bool) error {
	family := "IPv4"
	if ipv6 {
		family = "IPv6"
	}
	if value.Method != "auto" && value.Method != "manual" && value.Method != "disabled" && !(ipv6 && value.Method == "dhcp") && value.Method != "link-local" {
		return &InputError{Message: family + " 地址获取方式无效"}
	}
	if len(value.Addresses) > 16 || len(value.DNS) > 8 || value.RouteMetric < -1 || value.RouteMetric > 4294967295 {
		return &InputError{Message: family + " 地址、DNS 数量或路由优先级无效"}
	}
	if value.Method == "manual" && len(value.Addresses) == 0 {
		return &InputError{Message: family + " 手动模式至少需要一个 CIDR 地址"}
	}
	if value.Method != "manual" && len(value.Addresses) != 0 || (value.Method == "disabled" || value.Method == "link-local") && (value.Gateway != "" || len(value.DNS) > 0) {
		return &InputError{Message: family + " 自动／停用模式不能保留手动地址，停用或仅本地链路模式不能保留网关或 DNS"}
	}
	seen := make(map[string]bool)
	for _, text := range value.Addresses {
		prefix, err := netip.ParsePrefix(text)
		if err != nil || !validEthernetAddress(prefix.Addr(), ipv6) || seen[text] {
			return &InputError{Message: family + " 地址必须是不同的合法单播 CIDR 地址"}
		}
		seen[text] = true
	}
	for _, text := range value.DNS {
		address, err := netip.ParseAddr(text)
		if err != nil || !validEthernetAddress(address, ipv6) {
			return &InputError{Message: family + " DNS 地址无效或地址族不匹配"}
		}
	}
	if value.Gateway != "" {
		gateway, err := netip.ParseAddr(value.Gateway)
		if err != nil || !validEthernetAddress(gateway, ipv6) {
			return &InputError{Message: family + " 网关地址无效"}
		}
		if value.Method == "manual" && !gateway.IsLinkLocalUnicast() {
			inside := false
			for _, text := range value.Addresses {
				prefix, _ := netip.ParsePrefix(text)
				inside = inside || prefix.Contains(gateway)
			}
			if !inside {
				return &InputError{Message: fmt.Sprintf("%s 网关必须位于配置的子网内", family)}
			}
		}
	}
	return nil
}

func validEthernetAddress(address netip.Addr, ipv6 bool) bool {
	return address.IsValid() && address.Is6() == ipv6 && !address.Is4In6() && address.Zone() == "" &&
		!address.IsUnspecified() && !address.IsMulticast() && !address.IsLoopback() && address.String() != "255.255.255.255"
}
