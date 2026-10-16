package deviceadmin

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/godbus/dbus/v5"
)

// SharedNetworkDevice 只公开网络状态，不公开 SIM、配对密钥或连接配置中的密码。
type SharedNetworkDevice struct {
	Interface      string             `json:"interface"`
	Kind           string             `json:"kind"`
	Driver         string             `json:"driver"`
	State          string             `json:"state"`
	StateCode      uint32             `json:"state_code"`
	Managed        bool               `json:"managed"`
	MTU            uint32             `json:"mtu,omitempty"`
	Speed          uint32             `json:"speed,omitempty"`
	ConnectionName string             `json:"connection_name"`
	ConnectionUUID string             `json:"connection_uuid"`
	DefaultIPv4    bool               `json:"default_ipv4"`
	DefaultIPv6    bool               `json:"default_ipv6"`
	IPv4           EthernetIPSettings `json:"ipv4"`
	IPv6           EthernetIPSettings `json:"ipv6"`
	Owner          string             `json:"owner,omitempty"`
	Notice         string             `json:"notice,omitempty"`
}

// SharedNetworkStatus 包含已被系统枚举的移动与共享网络，不把配对设备当作网络。
type SharedNetworkStatus struct {
	Devices []SharedNetworkDevice `json:"devices"`
}

// SharedNetworks 查询当前网络对象；不拨号、不安装依赖，也不抢占 CarPlay 的 PAN。
func (m *Manager) SharedNetworks(ctx context.Context) (SharedNetworkStatus, error) {
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	open := m.openNetworkBus
	if open == nil {
		open = openEthernetBus
	}
	bus, err := open()
	if err != nil {
		return SharedNetworkStatus{}, err
	}
	defer bus.close()
	status, err := sharedNetworkInventory(ctx, bus)
	if err != nil {
		return status, err
	}
	// 状态查询不能在系统总线等待期间持有无线控制锁，避免拖延 CarPlay 续租。
	m.mu.Lock()
	defer m.mu.Unlock()
	for i := range status.Devices {
		d := &status.Devices[i]
		if d.Kind == "bluetooth" && m.carPlayNetworkHold != nil && d.ConnectionUUID != "" && d.ConnectionUUID == m.carPlayNetworkHold.RuntimePANUUID {
			d.Owner = "carplay"
			d.Notice = "此连接由 CarPlay 管理，请在 CarPlay 设置中调整网络来源"
		}
	}
	return status, nil
}

func sharedNetworkKind(deviceType uint32, driver string) string {
	switch deviceType {
	case 5:
		return "bluetooth"
	case 8:
		return "cellular"
	case 1:
		if driver == "bnep" {
			return "bluetooth"
		}
		// ECM/RNDIS/NCM 既可能来自手机，也可能来自蜂窝路由器，不能猜具体来源。
		switch driver {
		case "rndis_host", "cdc_ether", "cdc_ncm", "ipheth":
			return "usb"
		}
	}
	return ""
}

func isSharedUSBDriver(driver string) bool { return sharedNetworkKind(1, driver) == "usb" }

func sharedNetworkInventory(ctx context.Context, bus ethernetBus) (SharedNetworkStatus, error) {
	status := SharedNetworkStatus{Devices: []SharedNetworkDevice{}}
	body, err := bus.call(ctx, nmEthernetRoot, nmEthernetManager+".GetDevices")
	if err != nil {
		return status, err
	}
	var paths []dbus.ObjectPath
	if err := dbus.Store(body, &paths); err != nil {
		return status, err
	}
	if len(paths) > 128 {
		return status, fmt.Errorf("NetworkManager 设备清单异常")
	}
	for _, path := range paths {
		props, err := ethernetGetAll(ctx, bus, path, nmEthernetDevice)
		if err != nil {
			return status, err
		}
		kind := sharedNetworkKind(ethernetUint(props, "DeviceType"), ethernetString(props, "Driver"))
		if kind == "" {
			continue
		}
		d := SharedNetworkDevice{Interface: ethernetString(props, "Interface"), Kind: kind, Driver: ethernetString(props, "Driver"),
			StateCode: ethernetUint(props, "State"), Managed: ethernetBool(props, "Managed"), MTU: ethernetUint(props, "Mtu"),
			IPv4: EthernetIPSettings{Addresses: []string{}, DNS: []string{}}, IPv6: EthernetIPSettings{Addresses: []string{}, DNS: []string{}},
			Notice: "使用系统已有的共享或拨号配置；此处不修改 APN、SIM PIN 或蓝牙配对",
		}
		d.State = ethernetState(d.StateCode)
		// 仅 USB 以太网可能公开链路协商速率；不能用蓝牙版本或蜂窝理论速率冒充。
		if kind == "usb" && d.StateCode == 100 {
			if wired, err := ethernetGetAll(ctx, bus, path, nmEthernetDevice+".Wired"); err == nil {
				d.Speed = ethernetUint(wired, "Speed")
			}
		}
		if active := ethernetPath(props, "ActiveConnection"); active.IsValid() && active != "/" {
			p, err := ethernetGetAll(ctx, bus, active, nmEthernetManager+".Connection.Active")
			if err != nil {
				return status, err
			}
			d.ConnectionUUID, d.ConnectionName = ethernetString(p, "Uuid"), ethernetString(p, "Id")
			d.DefaultIPv4, d.DefaultIPv6 = ethernetBool(p, "Default"), ethernetBool(p, "Default6")
		}
		if d.StateCode == 100 && d.ConnectionUUID == "" {
			d.State = "unknown"
		}
		// 已配置不等于已联网；保留数字状态、实际地址和默认路由三个独立事实。
		for _, family := range []struct {
			key    string
			ipv6   bool
			target *EthernetIPSettings
		}{
			{"Ip4Config", false, &d.IPv4}, {"Ip6Config", true, &d.IPv6},
		} {
			if p := ethernetPath(props, family.key); p.IsValid() && p != "/" {
				iface := nmEthernetManager + ".IP4Config"
				if family.ipv6 {
					iface = nmEthernetManager + ".IP6Config"
				}
				values, err := ethernetGetAll(ctx, bus, p, iface)
				if err != nil {
					return status, err
				}
				*family.target = ethernetIP(values, true, family.ipv6)
			}
		}
		status.Devices = append(status.Devices, d)
	}
	sort.Slice(status.Devices, func(i, j int) bool { return status.Devices[i].Interface < status.Devices[j].Interface })
	return status, nil
}

// wirelessRoleName 只读取启动准备器发布的受限接口名，不接受网页指定文件路径。
func wirelessRoleName(key string) string {
	data, err := os.ReadFile(filepath.Join("/run/teslabox", "wireless-roles.env"))
	if err != nil || len(data) > 4096 {
		return ""
	}
	for _, line := range strings.Split(string(data), "\n") {
		if value, ok := strings.CutPrefix(line, key+"="); ok && ethernetInterfaceValid(value) {
			return value
		}
	}
	return ""
}
