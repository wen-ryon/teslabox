package deviceadmin

import (
	"context"
	"encoding/binary"
	"fmt"
	"net"
	"net/netip"
	"sort"
	"strconv"
	"strings"

	"github.com/godbus/dbus/v5"
)

const (
	nmEthernetManager  = "org.freedesktop.NetworkManager"
	nmEthernetRoot     = dbus.ObjectPath("/org/freedesktop/NetworkManager")
	nmEthernetSettings = "org.freedesktop.NetworkManager.Settings.Connection"
	nmEthernetDevice   = "org.freedesktop.NetworkManager.Device"
)

type ethernetProperties = map[string]dbus.Variant
type ethernetConfig = map[string]map[string]dbus.Variant

type ethernetBus interface {
	call(context.Context, dbus.ObjectPath, string, ...any) ([]any, error)
	close()
}

type ethernetDBus struct{ conn *dbus.Conn }

func openEthernetBus() (ethernetBus, error) {
	conn, err := dbus.ConnectSystemBus()
	if err != nil {
		return nil, err
	}
	return &ethernetDBus{conn: conn}, nil
}

func (b *ethernetDBus) close() { _ = b.conn.Close() }
func (b *ethernetDBus) call(ctx context.Context, path dbus.ObjectPath, method string, args ...any) ([]any, error) {
	call := b.conn.Object(nmEthernetManager, path).CallWithContext(ctx, method, 0, args...)
	return call.Body, call.Err
}

func ethernetGetAll(ctx context.Context, bus ethernetBus, path dbus.ObjectPath, iface string) (ethernetProperties, error) {
	body, err := bus.call(ctx, path, "org.freedesktop.DBus.Properties.GetAll", iface)
	if err != nil {
		return nil, err
	}
	var props ethernetProperties
	if err := dbus.Store(body, &props); err != nil {
		return nil, err
	}
	return props, nil
}

func ethernetSettings(ctx context.Context, bus ethernetBus, path dbus.ObjectPath) (ethernetConfig, error) {
	body, err := bus.call(ctx, path, nmEthernetSettings+".GetSettings")
	if err != nil {
		return nil, err
	}
	var config ethernetConfig
	if err := dbus.Store(body, &config); err != nil {
		return nil, err
	}
	return config, nil
}

func ethernetString(props ethernetProperties, key string) string {
	value, _ := props[key].Value().(string)
	return value
}
func ethernetUint(props ethernetProperties, key string) uint32 {
	value, _ := props[key].Value().(uint32)
	return value
}
func ethernetBool(props ethernetProperties, key string) bool {
	value, _ := props[key].Value().(bool)
	return value
}
func ethernetPath(props ethernetProperties, key string) dbus.ObjectPath {
	value, _ := props[key].Value().(dbus.ObjectPath)
	return value
}

func ethernetIP(props ethernetProperties, runtime bool, ipv6 bool) EthernetIPSettings {
	value := EthernetIPSettings{Method: "auto", Addresses: []string{}, DNS: []string{}, RouteMetric: -1}
	addressKey, gatewayKey, dnsKey := "address-data", "gateway", "dns"
	if runtime {
		addressKey, gatewayKey, dnsKey = "AddressData", "Gateway", "Nameservers"
	}
	if method := ethernetString(props, "method"); method != "" {
		value.Method = method
	}
	if metric, ok := props["route-metric"].Value().(int64); ok {
		value.RouteMetric = metric
	}
	value.Gateway = ethernetString(props, gatewayKey)
	value.IgnoreAutoDNS = ethernetBool(props, "ignore-auto-dns")
	value.NeverDefault = ethernetBool(props, "never-default")
	if addresses, ok := props[addressKey].Value().([]map[string]dbus.Variant); ok {
		for _, address := range addresses {
			value.Addresses = append(value.Addresses, ethernetString(address, "address")+"/"+strconv.FormatUint(uint64(ethernetUint(address, "prefix")), 10))
		}
	}
	if ipv6 {
		if addresses, ok := props[dnsKey].Value().([][]byte); ok {
			for _, address := range addresses {
				if ip, ok := netip.AddrFromSlice(address); ok {
					value.DNS = append(value.DNS, ip.String())
				}
			}
		}
	} else if addresses, ok := props[dnsKey].Value().([]uint32); ok {
		for _, address := range addresses {
			var data [4]byte
			// NM 的旧版 IPv4 D-Bus 字段承载 in_addr 原生整数，不是大端文本整数。
			binary.NativeEndian.PutUint32(data[:], address)
			value.DNS = append(value.DNS, netip.AddrFrom4(data).String())
		}
	}
	return value
}

func ethernetProfile(config ethernetConfig) EthernetProfile {
	conn, wired := config["connection"], config["802-3-ethernet"]
	value := EthernetProfile{UUID: ethernetString(conn, "uuid"), Editable: true, Settings: EthernetSettings{
		Name: ethernetString(conn, "id"), AutoConnect: true, MTU: ethernetUint(wired, "mtu"),
		IPv4: ethernetIP(config["ipv4"], false, false), IPv6: ethernetIP(config["ipv6"], false, true),
		AutoNegotiate: ethernetBool(wired, "auto-negotiate"), Speed: ethernetUint(wired, "speed"), Duplex: ethernetString(wired, "duplex"), WakeOnLAN: 1,
	}}
	if flag, ok := conn["autoconnect"].Value().(bool); ok {
		value.Settings.AutoConnect = flag
	}
	if flag, ok := wired["wake-on-lan"].Value().(uint32); ok {
		value.Settings.WakeOnLAN = flag
	}
	if ethernetString(conn, "type") != "802-3-ethernet" || len(config["802-1x"]) > 0 || ethernetString(conn, "master") != "" || ethernetString(conn, "slave-type") != "" || ethernetString(conn, "controller") != "" {
		value.Editable = false
		value.Reason = "该配置含企业认证、网桥或绑定设置，请使用系统网络管理工具维护"
	} else if err := validateEthernetSettings(value.Settings); err != nil {
		value.Editable = false
		value.Reason = "该配置含当前页面不支持的地址或链路选项，已保留原配置"
	}
	return value
}

func ethernetPatchConfig(original ethernetConfig, iface, uuid string, value EthernetSettings) ethernetConfig {
	config := make(ethernetConfig, len(original)+3)
	for group, props := range original {
		config[group] = make(ethernetProperties, len(props))
		for key, prop := range props {
			config[group][key] = prop
		}
	}
	set := func(group, key string, value any) {
		if config[group] == nil {
			config[group] = make(ethernetProperties)
		}
		config[group][key] = dbus.MakeVariant(value)
	}
	set("connection", "id", value.Name)
	set("connection", "uuid", uuid)
	set("connection", "type", "802-3-ethernet")
	set("connection", "interface-name", iface)
	set("connection", "autoconnect", value.AutoConnect)
	set("802-3-ethernet", "mtu", value.MTU)
	set("802-3-ethernet", "auto-negotiate", value.AutoNegotiate)
	set("802-3-ethernet", "speed", value.Speed)
	set("802-3-ethernet", "duplex", value.Duplex)
	set("802-3-ethernet", "wake-on-lan", value.WakeOnLAN)
	for _, family := range []struct {
		name  string
		value EthernetIPSettings
		ipv6  bool
	}{{"ipv4", value.IPv4, false}, {"ipv6", value.IPv6, true}} {
		set(family.name, "method", family.value.Method)
		set(family.name, "gateway", family.value.Gateway)
		set(family.name, "ignore-auto-dns", family.value.IgnoreAutoDNS)
		set(family.name, "never-default", family.value.NeverDefault)
		set(family.name, "route-metric", family.value.RouteMetric)
		addresses := make([]map[string]dbus.Variant, 0, len(family.value.Addresses))
		for _, text := range family.value.Addresses {
			prefix, _ := netip.ParsePrefix(text)
			addresses = append(addresses, map[string]dbus.Variant{"address": dbus.MakeVariant(prefix.Addr().String()), "prefix": dbus.MakeVariant(uint32(prefix.Bits()))})
		}
		set(family.name, "address-data", addresses)
		delete(config[family.name], "addresses")
		if family.ipv6 {
			dns := make([][]byte, 0, len(family.value.DNS))
			for _, text := range family.value.DNS {
				address, _ := netip.ParseAddr(text)
				dns = append(dns, address.AsSlice())
			}
			set(family.name, "dns", dns)
		} else {
			dns := make([]uint32, 0, len(family.value.DNS))
			for _, text := range family.value.DNS {
				address, _ := netip.ParseAddr(text)
				data := address.As4()
				dns = append(dns, binary.NativeEndian.Uint32(data[:]))
			}
			set(family.name, "dns", dns)
		}
	}
	return config
}

type ethernetRecord struct {
	device   EthernetDevice
	path     dbus.ObjectPath
	profiles map[string]dbus.ObjectPath
	configs  map[string]ethernetConfig
}

func ethernetSavedProfiles(ctx context.Context, bus ethernetBus) (map[dbus.ObjectPath]ethernetConfig, error) {
	body, err := bus.call(ctx, dbus.ObjectPath(string(nmEthernetRoot)+"/Settings"), nmEthernetManager+".Settings.ListConnections")
	if err != nil {
		return nil, err
	}
	var paths []dbus.ObjectPath
	if err := dbus.Store(body, &paths); err != nil {
		return nil, err
	}
	if len(paths) > 2048 {
		return nil, fmt.Errorf("NetworkManager 已保存配置清单异常")
	}
	profiles := make(map[dbus.ObjectPath]ethernetConfig)
	for _, path := range paths {
		config, err := ethernetSettings(ctx, bus, path)
		if err != nil {
			return nil, err
		}
		// GetSettings 不读取凭据；无线、蓝牙等配置不进入有线口清单或响应。
		if ethernetString(config["connection"], "type") == "802-3-ethernet" {
			profiles[path] = config
		}
	}
	return profiles, nil
}

func ethernetSavedProfileMatches(config ethernetConfig, device EthernetDevice) bool {
	conn := config["connection"]
	if ethernetString(conn, "type") != "802-3-ethernet" {
		return false
	}
	if iface := ethernetString(conn, "interface-name"); iface != "" && iface != device.Interface {
		return false
	}
	binding, bound := config["802-3-ethernet"]["mac-address"]
	if !bound {
		return true
	}
	var address net.HardwareAddr
	switch value := binding.Value().(type) {
	case []byte:
		address = net.HardwareAddr(value)
	case string:
		if value == "" {
			return true
		}
		parsed, err := net.ParseMAC(value)
		if err != nil {
			return false
		}
		address = parsed
	default:
		return false
	}
	if len(address) == 0 {
		return true
	}
	if len(address) != 6 {
		return false
	}
	for _, known := range []string{device.MAC, device.PermanentMAC} {
		if parsed, err := net.ParseMAC(known); err == nil && parsed.String() == address.String() {
			return true
		}
	}
	// 硬件 MAC 未知时不能猜测归属，更不能借另一网口改写受限配置。
	return false
}

func ethernetInventory(ctx context.Context, bus ethernetBus) ([]ethernetRecord, error) {
	body, err := bus.call(ctx, nmEthernetRoot, nmEthernetManager+".GetDevices")
	if err != nil {
		return nil, err
	}
	var paths []dbus.ObjectPath
	if err := dbus.Store(body, &paths); err != nil {
		return nil, err
	}
	if len(paths) > 128 {
		return nil, fmt.Errorf("NetworkManager 设备清单异常")
	}
	saved, err := ethernetSavedProfiles(ctx, bus)
	if err != nil {
		return nil, err
	}
	records := []ethernetRecord{}
	for _, path := range paths {
		props, err := ethernetGetAll(ctx, bus, path, nmEthernetDevice)
		if err != nil {
			return nil, err
		}
		if ethernetUint(props, "DeviceType") != 1 {
			continue
		}
		if sharedNetworkKind(1, ethernetString(props, "Driver")) == "bluetooth" {
			continue
		}
		wired, err := ethernetGetAll(ctx, bus, path, nmEthernetDevice+".Wired")
		if err != nil {
			return nil, err
		}
		device := EthernetDevice{Interface: ethernetString(props, "Interface"), Managed: ethernetBool(props, "Managed"), StateCode: ethernetUint(props, "State"),
			Carrier: ethernetBool(wired, "Carrier"), Speed: ethernetUint(wired, "Speed"), MAC: ethernetString(wired, "HwAddress"), PermanentMAC: ethernetString(wired, "PermHwAddress"),
			MTU: ethernetUint(props, "Mtu"), Driver: ethernetString(props, "Driver"), Profiles: []EthernetProfile{}, LinkModes: []EthernetLinkMode{},
			IPv4: EthernetIPSettings{Addresses: []string{}, DNS: []string{}}, IPv6: EthernetIPSettings{Addresses: []string{}, DNS: []string{}},
		}
		device.State = ethernetState(device.StateCode)
		device.Kind = "ethernet"
		if isSharedUSBDriver(device.Driver) {
			device.Kind = "usb"
		}
		activeProfile := dbus.ObjectPath("")
		if active := ethernetPath(props, "ActiveConnection"); active.IsValid() && active != "/" {
			activeProps, err := ethernetGetAll(ctx, bus, active, nmEthernetManager+".Connection.Active")
			if err != nil {
				return nil, err
			}
			device.ConnectionUUID = ethernetString(activeProps, "Uuid")
			activeProfile = ethernetPath(activeProps, "Connection")
		}
		for _, family := range []struct {
			key    string
			target *EthernetIPSettings
			ipv6   bool
		}{{"Ip4Config", &device.IPv4, false}, {"Ip6Config", &device.IPv6, true}} {
			if configPath := ethernetPath(props, family.key); configPath.IsValid() && configPath != "/" {
				config, err := ethernetGetAll(ctx, bus, configPath, nmEthernetManager+map[bool]string{false: ".IP4Config", true: ".IP6Config"}[family.ipv6])
				if err != nil {
					return nil, err
				}
				*family.target = ethernetIP(config, true, family.ipv6)
			}
		}
		available, _ := props["AvailableConnections"].Value().([]dbus.ObjectPath)
		// 未插网线时 NM 的 AvailableConnections 可能为空，仍需允许维护本接口的已保存配置。
		available = append([]dbus.ObjectPath{}, available...)
		for candidate, config := range saved {
			if ethernetSavedProfileMatches(config, device) {
				available = append(available, candidate)
			}
		}
		if activeProfile.IsValid() && activeProfile != "/" {
			available = append(append([]dbus.ObjectPath{}, available...), activeProfile)
		}
		record := ethernetRecord{path: path, profiles: map[string]dbus.ObjectPath{}, configs: map[string]ethernetConfig{}}
		seen := map[dbus.ObjectPath]bool{}
		for _, candidate := range available {
			if seen[candidate] {
				continue
			}
			seen[candidate] = true
			config := saved[candidate]
			if config == nil {
				var err error
				config, err = ethernetSettings(ctx, bus, candidate)
				if err != nil {
					return nil, err
				}
			}
			if ethernetString(config["connection"], "type") != "802-3-ethernet" {
				continue
			}
			profile := ethernetProfile(config)
			if !validUUID(profile.UUID) {
				continue
			}
			profile.Active = candidate == activeProfile && device.StateCode == 100
			device.Profiles = append(device.Profiles, profile)
			record.profiles[profile.UUID], record.configs[profile.UUID] = candidate, config
		}
		sort.SliceStable(device.Profiles, func(i, j int) bool {
			if device.Profiles[i].Active != device.Profiles[j].Active {
				return device.Profiles[i].Active
			}
			return device.Profiles[i].Settings.Name < device.Profiles[j].Settings.Name
		})
		record.device = device
		records = append(records, record)
	}
	// 一个未绑定接口的 NM 配置可能同时列在多个网口下，不能经另一张网卡修改正在使用的配置。
	owners := map[string]map[string]bool{}
	for _, record := range records {
		if uuid := record.device.ConnectionUUID; uuid != "" {
			if owners[uuid] == nil {
				owners[uuid] = map[string]bool{}
			}
			owners[uuid][record.device.Interface] = true
		}
	}
	for i := range records {
		for j := range records[i].device.Profiles {
			profile := &records[i].device.Profiles[j]
			for iface := range owners[profile.UUID] {
				if iface != records[i].device.Interface {
					profile.Editable = false
					profile.Reason = "该配置正在被其他有线网口使用，请先在所属网口维护，或新建本接口专用配置"
				}
			}
		}
	}
	sort.Slice(records, func(i, j int) bool { return records[i].device.Interface < records[j].device.Interface })
	return records, nil
}

func ethernetState(state uint32) string {
	switch {
	case state == 100:
		return "connected"
	case state >= 40 && state < 100:
		return "connecting"
	case state == 110:
		return "disconnecting"
	case state == 10:
		return "unmanaged"
	case state == 20:
		return "unavailable"
	case state == 30:
		return "disconnected"
	case state == 120:
		return "failed"
	default:
		return "unknown"
	}
}

func ethernetInterfaceValid(name string) bool {
	return len(name) > 0 && len(name) <= 15 && name != "." && name != ".." && !strings.HasPrefix(name, "-") && !strings.ContainsAny(name, "/\\\x00\t\r\n :")
}
