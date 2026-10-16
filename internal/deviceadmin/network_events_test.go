package deviceadmin

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/godbus/dbus/v5"
)

func TestParseNetworkManagerEvent(t *testing.T) {
	t.Parallel()
	devicePath := dbus.ObjectPath("/org/freedesktop/NetworkManager/Devices/2")
	accessPointPath := dbus.ObjectPath("/org/freedesktop/NetworkManager/AccessPoint/3")
	otherAccessPointPath := dbus.ObjectPath("/org/freedesktop/NetworkManager/AccessPoint/4")

	tests := []struct {
		name       string
		signal     *dbus.Signal
		wantEvent  networkEvent
		wantPath   dbus.ObjectPath
		wantChange bool
	}{
		{
			name: "wireless device state change",
			signal: &dbus.Signal{
				Name: networkManagerDeviceInterface + ".StateChanged",
				Path: devicePath,
				Body: []interface{}{uint32(100), uint32(40), uint32(0)},
			},
			wantEvent:  networkEvent{Kind: networkEventDeviceState, State: 100},
			wantPath:   accessPointPath,
			wantChange: true,
		},
		{
			name: "active access point changed",
			signal: &dbus.Signal{
				Name: dbusPropertiesInterface + ".PropertiesChanged",
				Path: devicePath,
				Body: []interface{}{
					networkManagerWirelessInterface,
					map[string]dbus.Variant{"ActiveAccessPoint": dbus.MakeVariant(otherAccessPointPath)},
					map[string]dbus.Variant{},
				},
			},
			wantEvent:  networkEvent{Kind: networkEventAssociation},
			wantPath:   otherAccessPointPath,
			wantChange: true,
		},
		{
			name: "active access point frequency changed",
			signal: &dbus.Signal{
				Name: dbusPropertiesInterface + ".PropertiesChanged",
				Path: accessPointPath,
				Body: []interface{}{
					networkManagerAccessPointIface,
					map[string]dbus.Variant{"Frequency": dbus.MakeVariant(uint32(5805))},
					map[string]dbus.Variant{},
				},
			},
			wantEvent:  networkEvent{Kind: networkEventAssociation},
			wantPath:   accessPointPath,
			wantChange: true,
		},
		{
			name: "unrelated access point ignored",
			signal: &dbus.Signal{
				Name: dbusPropertiesInterface + ".PropertiesChanged",
				Path: otherAccessPointPath,
				Body: []interface{}{
					networkManagerAccessPointIface,
					map[string]dbus.Variant{"Frequency": dbus.MakeVariant(uint32(5805))},
					map[string]dbus.Variant{},
				},
			},
			wantPath: accessPointPath,
		},
		{
			name: "network device added",
			signal: &dbus.Signal{
				Name: networkManagerBusName + ".DeviceAdded",
				Path: networkManagerObjectPath,
				Body: []interface{}{dbus.ObjectPath("/org/freedesktop/NetworkManager/Devices/9")},
			},
			wantEvent:  networkEvent{Kind: networkEventInventory},
			wantPath:   accessPointPath,
			wantChange: true,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			event, path, changed := parseNetworkManagerEvent(test.signal, devicePath, accessPointPath)
			if changed != test.wantChange || event != test.wantEvent || path != test.wantPath {
				t.Fatalf("parseNetworkManagerEvent() = (%#v, %q, %t), want (%#v, %q, %t)", event, path, changed, test.wantEvent, test.wantPath, test.wantChange)
			}
		})
	}
}

func TestNetworkManagerMatchRulesTargetDeviceAndAccessPointChanges(t *testing.T) {
	t.Parallel()
	rules := networkManagerMatchRules(dbus.ObjectPath("/org/freedesktop/NetworkManager/Devices/2"))
	if len(rules) != 5 {
		t.Fatalf("rules = %#v, want 5 rules", rules)
	}
	for _, want := range []string{
		"member='StateChanged'",
		"path='/org/freedesktop/NetworkManager/Devices/2'",
		"arg0='org.freedesktop.NetworkManager.AccessPoint'",
		"member='DeviceAdded'",
		"member='DeviceRemoved'",
	} {
		found := false
		for _, rule := range rules {
			if strings.Contains(rule, want) {
				found = true
				break
			}
		}
		if !found {
			t.Fatalf("rules = %#v, missing %q", rules, want)
		}
	}
}

func TestNetworkManagerMatchRulesAlsoObservePeerDeviceState(t *testing.T) {
	t.Parallel()
	rules := networkManagerMatchRules(dbus.ObjectPath("/org/freedesktop/NetworkManager/Devices/2"))
	for _, rule := range rules {
		if strings.Contains(rule, "member='StateChanged'") {
			if strings.Contains(rule, "path=") {
				t.Fatalf("只监听STA会漏掉同PHY预留AP自动成为第二STA：%s", rule)
			}
			return
		}
	}
	t.Fatal("缺少设备状态监听规则")
}

func TestRelatedWirelessDeviceFiltersUSBAndOtherRadios(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name       string
		deviceType uint32
		iface      string
		shared     bool
		known      bool
		want       bool
	}{
		{name: "同PHY的AP虚拟接口", deviceType: 2, iface: "wlan0", shared: true, known: true, want: true},
		{name: "USB共享以太网不触发无线恢复", deviceType: 1, iface: "usb0", shared: true, known: true},
		{name: "另一张无线网卡不触发恢复", deviceType: 2, iface: "wlan2", known: true},
		{name: "未知PHY不猜测相关性", deviceType: 2, iface: "wlan0", shared: true},
		{name: "缺少接口名不触发恢复", deviceType: 2, shared: true, known: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			properties := map[string]dbus.Variant{
				"DeviceType": dbus.MakeVariant(test.deviceType),
				"Interface":  dbus.MakeVariant(test.iface),
			}
			got := relatedWirelessDevice(properties, "wlan1", func(left, right string) (bool, bool) {
				if left != "wlan1" || right != test.iface {
					t.Errorf("PHY查询接口错误：%s, %s", left, right)
				}
				return test.shared, test.known
			})
			if got != test.want {
				t.Fatalf("相关无线设备=%v, want=%v", got, test.want)
			}
		})
	}
	if relatedWirelessDevice(map[string]dbus.Variant{}, "wlan1", nil) {
		t.Fatal("缺少设备属性不能触发恢复")
	}
}

func TestStartNetworkWatcherForwardsEventWithoutPolling(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	manager := New(&fakeRunner{responses: map[string]string{}}, nil)
	manager.monitoredWiFiInterface = "wlan0"
	manager.watchNetworkEvents = func(ctx context.Context, interfaceName string, notify func(networkEvent)) error {
		if interfaceName != "wlan0" {
			t.Errorf("interfaceName = %q, want wlan0", interfaceName)
		}
		notify(networkEvent{Kind: networkEventAssociation})
		<-ctx.Done()
		return nil
	}
	events := make(chan networkEvent, 1)
	watchErrors := make(chan error, 1)
	if !manager.startNetworkWatcher(ctx, events, watchErrors) {
		t.Fatal("startNetworkWatcher() = false, want true")
	}
	select {
	case event := <-events:
		if event.Kind != networkEventAssociation {
			t.Fatalf("event = %#v, want association change", event)
		}
	case <-time.After(time.Second):
		t.Fatal("event watcher did not forward the NetworkManager event")
	}
	if len(watchErrors) != 0 {
		t.Fatalf("watch errors = %#v, want empty", watchErrors)
	}
}
