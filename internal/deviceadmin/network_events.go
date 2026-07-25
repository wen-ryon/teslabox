package deviceadmin

import (
	"context"
	"fmt"

	"github.com/godbus/dbus/v5"
)

const (
	networkManagerBusName             = "org.freedesktop.NetworkManager"
	networkManagerObjectPath          = dbus.ObjectPath("/org/freedesktop/NetworkManager")
	networkManagerDeviceInterface     = "org.freedesktop.NetworkManager.Device"
	networkManagerWirelessInterface   = "org.freedesktop.NetworkManager.Device.Wireless"
	networkManagerAccessPointIface    = "org.freedesktop.NetworkManager.AccessPoint"
	dbusPropertiesInterface           = "org.freedesktop.DBus.Properties"
	dbusAddMatchMethod                = "org.freedesktop.DBus.AddMatch"
	dbusRemoveMatchMethod             = "org.freedesktop.DBus.RemoveMatch"
	networkManagerGetDeviceByIPMethod = "org.freedesktop.NetworkManager.GetDeviceByIpIface"
	networkEventBufferSize            = 16
)

type networkEventKind uint8

const (
	networkEventDeviceState networkEventKind = iota + 1
	networkEventAssociation
	networkEventInventory
)

// networkEvent 表示需要重新核对上游 WiFi 与热点信道的一次 NetworkManager 变化。
// 使用事件而不是轮询可避免车载待机期间反复唤醒无线芯片。
type networkEvent struct {
	Kind  networkEventKind
	State uint32
}

type networkEventWatch func(context.Context, string, func(networkEvent)) error

// watchNetworkManagerEvents 订阅 NetworkManager 的 D-Bus 信号。
// 信号由驱动、wpa_supplicant 和 NetworkManager 主动推送，不会触发 WiFi 扫描。
func watchNetworkManagerEvents(ctx context.Context, interfaceName string, notify func(networkEvent)) error {
	if interfaceName == "" {
		return fmt.Errorf("监听 NetworkManager 事件: 无线接口为空")
	}
	if notify == nil {
		return fmt.Errorf("监听 NetworkManager 事件: 回调为空")
	}

	conn, err := dbus.ConnectSystemBus()
	if err != nil {
		return fmt.Errorf("连接系统 D-Bus: %w", err)
	}
	defer conn.Close()

	devicePath, err := networkManagerDevicePath(conn, interfaceName)
	if err != nil {
		return err
	}
	activeAccessPoint, _ := networkManagerActiveAccessPoint(conn, devicePath)
	rules := networkManagerMatchRules(devicePath)
	for _, rule := range rules {
		if err := networkManagerMatch(conn, dbusAddMatchMethod, rule); err != nil {
			return err
		}
	}
	defer func() {
		for _, rule := range rules {
			_ = networkManagerMatch(conn, dbusRemoveMatchMethod, rule)
		}
	}()

	signals := make(chan *dbus.Signal, networkEventBufferSize)
	conn.Signal(signals)
	defer conn.RemoveSignal(signals)

	for {
		select {
		case <-ctx.Done():
			return nil
		case signal, open := <-signals:
			if !open {
				if ctx.Err() != nil {
					return nil
				}
				return fmt.Errorf("NetworkManager D-Bus 信号通道已关闭")
			}
			event, nextAccessPoint, changed := parseNetworkManagerEvent(signal, devicePath, activeAccessPoint)
			if !changed {
				continue
			}
			activeAccessPoint = nextAccessPoint
			notify(event)
			if event.Kind == networkEventInventory {
				// 监听目标与 NetworkManager 的设备对象绑定；设备增删后退出并由上层
				// 按新发现的接口重建监听，避免热插拔后永远停留在旧对象路径。
				return errNetworkInventoryChanged
			}
		}
	}
}

func networkManagerDevicePath(conn *dbus.Conn, interfaceName string) (dbus.ObjectPath, error) {
	var devicePath dbus.ObjectPath
	call := conn.Object(networkManagerBusName, networkManagerObjectPath).Call(
		networkManagerGetDeviceByIPMethod, 0, interfaceName,
	)
	if err := call.Store(&devicePath); err != nil {
		return "", fmt.Errorf("查找无线接口 %s 的 NetworkManager 设备: %w", interfaceName, err)
	}
	return devicePath, nil
}

func networkManagerActiveAccessPoint(conn *dbus.Conn, devicePath dbus.ObjectPath) (dbus.ObjectPath, error) {
	var value dbus.Variant
	call := conn.Object(networkManagerBusName, devicePath).Call(
		dbusPropertiesInterface+".Get", 0, networkManagerWirelessInterface, "ActiveAccessPoint",
	)
	if err := call.Store(&value); err != nil {
		return "", fmt.Errorf("读取当前接入点: %w", err)
	}
	path, ok := value.Value().(dbus.ObjectPath)
	if !ok {
		return "", fmt.Errorf("读取当前接入点: 返回类型无效")
	}
	return path, nil
}

func networkManagerMatchRules(devicePath dbus.ObjectPath) []string {
	deviceRule := fmt.Sprintf(
		"type='signal',sender='%s',path='%s',interface='%s'",
		networkManagerBusName, devicePath, networkManagerDeviceInterface,
	)
	propertiesRule := fmt.Sprintf(
		"type='signal',sender='%s',path='%s',interface='%s',member='PropertiesChanged'",
		networkManagerBusName, devicePath, dbusPropertiesInterface,
	)
	accessPointRule := fmt.Sprintf(
		"type='signal',sender='%s',interface='%s',member='PropertiesChanged',arg0='%s'",
		networkManagerBusName, dbusPropertiesInterface, networkManagerAccessPointIface,
	)
	return []string{
		deviceRule + ",member='StateChanged'",
		propertiesRule,
		accessPointRule,
		fmt.Sprintf(
			"type='signal',sender='%s',path='%s',interface='%s',member='DeviceAdded'",
			networkManagerBusName, networkManagerObjectPath, networkManagerBusName,
		),
		fmt.Sprintf(
			"type='signal',sender='%s',path='%s',interface='%s',member='DeviceRemoved'",
			networkManagerBusName, networkManagerObjectPath, networkManagerBusName,
		),
	}
}

func networkManagerMatch(conn *dbus.Conn, method string, rule string) error {
	if err := conn.BusObject().Call(method, 0, rule).Err; err != nil {
		return fmt.Errorf("订阅 NetworkManager 事件: %w", err)
	}
	return nil
}

func parseNetworkManagerEvent(signal *dbus.Signal, devicePath, activeAccessPoint dbus.ObjectPath) (networkEvent, dbus.ObjectPath, bool) {
	if signal == nil {
		return networkEvent{}, activeAccessPoint, false
	}
	if signal.Path == networkManagerObjectPath &&
		(signal.Name == networkManagerBusName+".DeviceAdded" || signal.Name == networkManagerBusName+".DeviceRemoved") {
		return networkEvent{Kind: networkEventInventory}, activeAccessPoint, true
	}
	if signal.Name == networkManagerDeviceInterface+".StateChanged" && signal.Path == devicePath {
		if len(signal.Body) == 0 {
			return networkEvent{}, activeAccessPoint, false
		}
		state, ok := signal.Body[0].(uint32)
		if !ok {
			return networkEvent{}, activeAccessPoint, false
		}
		return networkEvent{Kind: networkEventDeviceState, State: state}, activeAccessPoint, true
	}
	if signal.Name != dbusPropertiesInterface+".PropertiesChanged" || len(signal.Body) < 2 {
		return networkEvent{}, activeAccessPoint, false
	}
	interfaceName, ok := signal.Body[0].(string)
	if !ok {
		return networkEvent{}, activeAccessPoint, false
	}
	changed, ok := signal.Body[1].(map[string]dbus.Variant)
	if !ok {
		return networkEvent{}, activeAccessPoint, false
	}
	if signal.Path == devicePath && interfaceName == networkManagerWirelessInterface {
		value, found := changed["ActiveAccessPoint"]
		if !found {
			return networkEvent{}, activeAccessPoint, false
		}
		next, ok := value.Value().(dbus.ObjectPath)
		if !ok {
			return networkEvent{}, activeAccessPoint, false
		}
		return networkEvent{Kind: networkEventAssociation}, next, true
	}
	if signal.Path == activeAccessPoint && interfaceName == networkManagerAccessPointIface {
		if _, frequencyChanged := changed["Frequency"]; frequencyChanged {
			return networkEvent{Kind: networkEventAssociation}, activeAccessPoint, true
		}
		if _, bssidChanged := changed["HwAddress"]; bssidChanged {
			return networkEvent{Kind: networkEventAssociation}, activeAccessPoint, true
		}
	}
	return networkEvent{}, activeAccessPoint, false
}
