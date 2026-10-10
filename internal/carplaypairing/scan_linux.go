//go:build linux

package carplaypairing

import (
	"context"
	"errors"
	"sort"
	"strings"
	"time"
	"unicode"

	"github.com/godbus/dbus/v5"
)

type bluezObjects map[dbus.ObjectPath]map[string]map[string]dbus.Variant

func scanDevices(objects bluezObjects) ([]Device, error) {
	if len(objects) > 256 {
		return nil, errors.New("蓝牙设备数量超过预算")
	}
	devices := make([]Device, 0)
	for path, interfaces := range objects {
		p := interfaces["org.bluez.Device1"]
		id, _ := p["Address"].Value().(string)
		adapter, _ := p["Adapter"].Value().(dbus.ObjectPath)
		kind, _ := p["AddressType"].Value().(string)
		blocked, _ := p["Blocked"].Value().(bool)
		if !phoneAddress.MatchString(id) || id == "00:00:00:00:00:00" || path != dbus.ObjectPath("/org/bluez/hci0/dev_"+strings.ReplaceAll(id, ":", "_")) || adapter != "/org/bluez/hci0" || kind != "public" || blocked {
			continue
		}
		name, _ := p["Alias"].Value().(string)
		if name == "" {
			name, _ = p["Name"].Value().(string)
		}
		if len(name) > 128 || strings.IndexFunc(name, func(r rune) bool { return unicode.IsControl(r) || unicode.Is(unicode.Cf, r) }) >= 0 {
			name = ""
		}
		paired, _ := p["Paired"].Value().(bool)
		devices = append(devices, Device{ID: id, Name: name, Paired: paired})
		if len(devices) > 32 {
			return nil, errors.New("蓝牙候选数量超过预算")
		}
	}
	sort.Slice(devices, func(i, j int) bool { return devices[i].ID < devices[j].ID })
	return devices, nil
}

func scanBlueZ(ctx context.Context) ([]Device, error) {
	// 独立总线连接持有自己的发现会话；断开只释放这一连接，不停止其他客户端。
	conn, err := dbus.ConnectSystemBus(dbus.WithContext(ctx))
	if err != nil {
		return nil, err
	}
	defer conn.Close()
	return scanOn(ctx, conn, 7*time.Second)
}

func scanOn(ctx context.Context, conn *dbus.Conn, window time.Duration) ([]Device, error) {
	var owner string
	if err := conn.BusObject().CallWithContext(ctx, "org.freedesktop.DBus.GetNameOwner", 0, "org.bluez").Store(&owner); err != nil {
		return nil, err
	}
	adapter := conn.Object(owner, "/org/bluez/hci0")
	var powered dbus.Variant
	if err := adapter.CallWithContext(ctx, "org.freedesktop.DBus.Properties.Get", 0, "org.bluez.Adapter1", "Powered").Store(&powered); err != nil {
		return nil, err
	}
	if powered.Value() != true {
		return nil, errors.New("蓝牙适配器尚未开启")
	}
	// 过滤器只属于本连接；首次 CarPlay 配对需要 Classic，不把随机 LE 广播作为目标。
	if err := adapter.CallWithContext(ctx, "org.bluez.Adapter1.SetDiscoveryFilter", 0, map[string]dbus.Variant{"Transport": dbus.MakeVariant("bredr")}).Err; err != nil {
		return nil, err
	}
	if err := adapter.CallWithContext(ctx, "org.bluez.Adapter1.StartDiscovery", 0).Err; err != nil {
		return nil, err
	}
	defer func() {
		cleanup, cancel := context.WithTimeout(context.WithoutCancel(ctx), time.Second)
		defer cancel()
		_ = adapter.CallWithContext(cleanup, "org.bluez.Adapter1.StopDiscovery", 0).Err
	}()
	timer := time.NewTimer(window)
	defer timer.Stop()
	select {
	case <-timer.C:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	var objects bluezObjects
	if err := conn.Object(owner, "/").CallWithContext(ctx, "org.freedesktop.DBus.ObjectManager.GetManagedObjects", 0).Store(&objects); err != nil {
		return nil, err
	}
	return scanDevices(objects)
}
