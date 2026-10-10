//go:build linux

package carplaypairing

import (
	"strings"
	"testing"

	"github.com/godbus/dbus/v5"
)

func TestScanDevicesRejectsOtherAdapterRandomAddressAndBlockedDevices(t *testing.T) {
	path := dbus.ObjectPath("/org/bluez/hci0/dev_02_11_22_33_44_55")
	properties := map[string]dbus.Variant{"Address": dbus.MakeVariant(testPhone), "Adapter": dbus.MakeVariant(dbus.ObjectPath("/org/bluez/hci0")), "AddressType": dbus.MakeVariant("public"), "Alias": dbus.MakeVariant("iPhone"), "Paired": dbus.MakeVariant(false)}
	objects := bluezObjects{path: {"org.bluez.Device1": properties}}
	devices, err := scanDevices(objects)
	if err != nil || len(devices) != 1 || devices[0].ID != testPhone || devices[0].Paired {
		t.Fatal(devices, err)
	}
	for _, field := range []string{"Address", "Adapter", "AddressType", "Blocked"} {
		original := properties[field]
		switch field {
		case "Address":
			properties[field] = dbus.MakeVariant("00:00:00:00:00:00")
		case "Adapter":
			properties[field] = dbus.MakeVariant(dbus.ObjectPath("/org/bluez/hci1"))
		case "AddressType":
			properties[field] = dbus.MakeVariant("random")
		case "Blocked":
			properties[field] = dbus.MakeVariant(true)
		}
		devices, err = scanDevices(objects)
		if err != nil || len(devices) != 0 {
			t.Fatal(field, devices, err)
		}
		properties[field] = original
	}
	for _, name := range []string{"\u202EiPhone", strings.Repeat("x", 129), "iPhone\n"} {
		properties["Alias"] = dbus.MakeVariant(name)
		devices, err = scanDevices(objects)
		if err != nil || len(devices) != 1 || devices[0].Name != "" {
			t.Fatal("设备名称未清理", devices, err)
		}
	}
}
