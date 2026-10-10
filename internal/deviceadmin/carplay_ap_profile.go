package deviceadmin

import (
	"context"
	"errors"
	"fmt"
	"regexp"

	"github.com/godbus/dbus/v5"
)

type carPlayNMSettings map[string]map[string]dbus.Variant

// createCarPlayAPProfile 保留模板的网卡适配属性，一次创建具有持久恢复身份的内存配置。
func createCarPlayAPProfile(ctx context.Context, lease CarPlayNetworkLease, mac, ssid, password string) error {
	conn, err := dbus.ConnectSystemBus()
	if err != nil {
		return errors.New("CarPlay 配置检查：系统 D-Bus 连接失败")
	}
	defer conn.Close()
	const settingsInterface = "org.freedesktop.NetworkManager.Settings"
	const connectionInterface = settingsInterface + ".Connection"
	settingsObject := conn.Object(networkManagerBusName, dbus.ObjectPath("/org/freedesktop/NetworkManager/Settings"))
	var path dbus.ObjectPath
	if err := settingsObject.CallWithContext(ctx, settingsInterface+".GetConnectionByUuid", 0, lease.Plan.APUUID).Store(&path); err != nil {
		return errors.New("CarPlay 配置检查：模板路径查询失败")
	}
	profile := conn.Object(networkManagerBusName, path)
	var settings, secrets carPlayNMSettings
	if err := profile.CallWithContext(ctx, connectionInterface+".GetSettings", 0).Store(&settings); err != nil {
		return errors.New("CarPlay 配置检查：模板属性读取失败")
	}
	if err := profile.CallWithContext(ctx, connectionInterface+".GetSecrets", 0, "802-11-wireless-security").Store(&secrets); err != nil {
		return errors.New("CarPlay 配置检查：模板凭据读取失败")
	}
	copy, err := prepareCarPlayAPProfile(settings, secrets, lease, mac, ssid, password)
	if err != nil {
		return err
	}
	// in-memory | block-autoconnect：不会写入正式配置，也不会在显式启动前抢占无线接口。
	if err := settingsObject.CallWithContext(ctx, settingsInterface+".AddConnection2", 0, copy,
		uint32(0x2|0x20), map[string]dbus.Variant{}).Err; err != nil {
		var busError dbus.Error
		name := ""
		if errors.As(err, &busError) && regexp.MustCompile(`^[A-Za-z0-9_.]+$`).MatchString(busError.Name) {
			name = busError.Name
		}
		field := regexp.MustCompile(`^(connection|802-11-wireless(?:-security)?|ipv[46])(?:\.[A-Za-z0-9-]+)?:`).FindString(err.Error())
		return fmt.Errorf("CarPlay 配置检查：内存配置提交失败，类型=%T，名称=%s，属性=%s", err, name, field)
	}
	return nil
}

func prepareCarPlayAPProfile(settings, secrets carPlayNMSettings, lease CarPlayNetworkLease, mac, ssid, password string) (carPlayNMSettings, error) {
	if !validCarPlayChannel(lease.Plan.Channel) {
		return nil, invalidInput("CarPlay 热点信道无效")
	}
	copy := make(carPlayNMSettings, len(settings))
	for name, properties := range settings {
		copy[name] = make(map[string]dbus.Variant, len(properties))
		for key, value := range properties {
			copy[name][key] = value
		}
	}
	for name, properties := range secrets {
		if copy[name] == nil {
			copy[name] = make(map[string]dbus.Variant)
		}
		for key, value := range properties {
			copy[name][key] = value
		}
	}
	connection, wifi, security, ipv4 := copy["connection"], copy["802-11-wireless"], copy["802-11-wireless-security"], copy["ipv4"]
	if connection == nil || wifi == nil || security == nil || ipv4 == nil ||
		connection["uuid"].Value() != lease.Plan.APUUID || wifi["mode"].Value() != "ap" ||
		security["key-mgmt"].Value() != "wpa-psk" || ipv4["method"].Value() != "shared" {
		return nil, errors.New("CarPlay 热点模板结构无效")
	}
	ssidBytes, ok := wifi["ssid"].Value().([]byte)
	if !ok || string(ssidBytes) != ssid || security["psk"].Value() != password {
		return nil, errors.New("CarPlay 热点模板在读取期间发生变化")
	}
	connection["id"] = dbus.MakeVariant(lease.RuntimeAPName)
	connection["uuid"] = dbus.MakeVariant(lease.RuntimeAPUUID)
	connection["interface-name"] = dbus.MakeVariant(lease.Plan.APInterface)
	connection["autoconnect"] = dbus.MakeVariant(false)
	band := "a"
	if lease.Plan.Channel <= 13 {
		band = "bg"
	}
	wifi["band"] = dbus.MakeVariant(band)
	wifi["channel"] = dbus.MakeVariant(uint32(lease.Plan.Channel))
	// D-Bus 的旧 cloned-mac-address 是字节数组；字符串必须使用 assigned-mac-address。
	delete(wifi, "cloned-mac-address")
	wifi["assigned-mac-address"] = dbus.MakeVariant(mac)
	if err := normalizeCarPlayIPProperties(copy); err != nil {
		return nil, err
	}
	return copy, nil
}

type carPlayIPv6Address struct {
	Address []byte
	Prefix  uint32
	Gateway []byte
}

type carPlayIPv6Route struct {
	Destination []byte
	Prefix      uint32
	NextHop     []byte
	Metric      uint32
}

func normalizeCarPlayIPProperties(settings carPlayNMSettings) error {
	// GetSettings 同时返回新旧 IP 属性时使用现代字典，避免旧结构数组在 Go 解码后丢失结构签名。
	for _, name := range []string{"ipv4", "ipv6"} {
		properties := settings[name]
		if _, exists := properties["address-data"]; exists {
			delete(properties, "addresses")
		}
		if _, exists := properties["route-data"]; exists {
			delete(properties, "routes")
		}
	}
	ipv6 := settings["ipv6"]
	if value, exists := ipv6["addresses"]; exists {
		var addresses []carPlayIPv6Address
		if err := dbus.Store([]any{value.Value()}, &addresses); err != nil {
			return errors.New("CarPlay 热点模板的旧 IPv6 地址结构无法转换")
		}
		ipv6["addresses"] = dbus.MakeVariant(addresses)
	}
	if value, exists := ipv6["routes"]; exists {
		var routes []carPlayIPv6Route
		if err := dbus.Store([]any{value.Value()}, &routes); err != nil {
			return errors.New("CarPlay 热点模板的旧 IPv6 路由结构无法转换")
		}
		ipv6["routes"] = dbus.MakeVariant(routes)
	}
	return nil
}
