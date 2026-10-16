package deviceadmin

import (
	"context"
	"time"

	"github.com/godbus/dbus/v5"
)

// WiFiDetails 仅包含当前连接的运行信息，不使用接入点理论最大速率或保存的地址配置。
type WiFiDetails struct {
	Available    bool     `json:"available"`
	BitrateKbps  uint32   `json:"bitrate_kbps,omitempty"`
	FrequencyMHz uint32   `json:"frequency_mhz,omitempty"`
	Signal       *uint8   `json:"signal,omitempty"`
	MTU          uint32   `json:"mtu,omitempty"`
	Addresses    []string `json:"addresses,omitempty"`
	Gateways     []string `json:"gateways,omitempty"`
	DNS          []string `json:"dns,omitempty"`
}

// WiFiDetails 在页面查询时补充信息，不让自动连接和 CarPlay 协调路径承担额外总线查询。
func (m *Manager) WiFiDetails(ctx context.Context, status WiFiStatus) WiFiDetails {
	if !status.Available || status.ConnectionState != "connected" || !validUUID(status.ConnectionUUID) || !ethernetInterfaceValid(status.Interface) {
		return WiFiDetails{}
	}
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	open := m.openNetworkBus
	if open == nil {
		open = openEthernetBus
	}
	bus, err := open()
	if err != nil {
		return WiFiDetails{}
	}
	defer bus.close()
	return readWiFiDetails(ctx, bus, status.Interface, status.ConnectionUUID)
}

func readWiFiDetails(ctx context.Context, bus ethernetBus, iface, uuid string) WiFiDetails {
	body, err := bus.call(ctx, nmEthernetRoot, nmEthernetManager+".GetDeviceByIpIface", iface)
	var path dbus.ObjectPath
	if err != nil || dbus.Store(body, &path) != nil || !path.IsValid() || path == "/" {
		return WiFiDetails{}
	}
	props, err := ethernetGetAll(ctx, bus, path, nmEthernetDevice)
	if err != nil || ethernetUint(props, "DeviceType") != 2 || ethernetString(props, "Interface") != iface || ethernetUint(props, "State") != 100 {
		return WiFiDetails{}
	}
	active := ethernetPath(props, "ActiveConnection")
	if !active.IsValid() || active == "/" {
		return WiFiDetails{}
	}
	conn, err := ethernetGetAll(ctx, bus, active, nmEthernetManager+".Connection.Active")
	if err != nil || ethernetString(conn, "Uuid") != uuid {
		return WiFiDetails{}
	}
	details := WiFiDetails{MTU: ethernetUint(props, "Mtu")}
	for _, family := range []struct {
		key, name string
		ipv6      bool
	}{{"Ip4Config", "IP4Config", false}, {"Ip6Config", "IP6Config", true}} {
		ipPath := ethernetPath(props, family.key)
		if !ipPath.IsValid() || ipPath == "/" {
			continue
		}
		values, err := ethernetGetAll(ctx, bus, ipPath, nmEthernetManager+"."+family.name)
		if err != nil {
			return WiFiDetails{}
		}
		ip := ethernetIP(values, true, family.ipv6)
		details.Addresses = append(details.Addresses, ip.Addresses...)
		if ip.Gateway != "" {
			details.Gateways = append(details.Gateways, ip.Gateway)
		}
		details.DNS = append(details.DNS, ip.DNS...)
	}
	// 无线指标是可选信息；驱动不提供时仍保留已读取的地址信息。
	if wireless, err := ethernetGetAll(ctx, bus, path, nmEthernetDevice+".Wireless"); err == nil {
		if ethernetUint(wireless, "Mode") != 2 {
			return WiFiDetails{}
		}
		details.BitrateKbps = ethernetUint(wireless, "Bitrate")
		if ap := ethernetPath(wireless, "ActiveAccessPoint"); ap.IsValid() && ap != "/" {
			if values, err := ethernetGetAll(ctx, bus, ap, nmEthernetManager+".AccessPoint"); err == nil {
				details.FrequencyMHz = ethernetUint(values, "Frequency")
				if signal, ok := values["Strength"].Value().(uint8); ok && signal <= 100 {
					details.Signal = &signal
				}
			}
		}
	}
	// 查询过程中若发生切换或重新激活，整组指标作废，不能混入上一连接的数据。
	latest, err := ethernetGetAll(ctx, bus, path, nmEthernetDevice)
	if err != nil || ethernetString(latest, "Interface") != iface || ethernetUint(latest, "State") != 100 || ethernetPath(latest, "ActiveConnection") != active ||
		ethernetPath(latest, "Ip4Config") != ethernetPath(props, "Ip4Config") || ethernetPath(latest, "Ip6Config") != ethernetPath(props, "Ip6Config") {
		return WiFiDetails{}
	}
	details.Available = true
	return details
}
