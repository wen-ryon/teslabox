package deviceadmin

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/godbus/dbus/v5"
)

func TestSharedNetworkKind(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		name         string
		typ          uint32
		driver, want string
	}{
		{"modem", 8, "cdc_mbim", "cellular"}, {"PAN", 5, "bluetooth", "bluetooth"},
		{"legacy_PAN", 1, "bnep", "bluetooth"},
		{"phone_or_router", 1, "rndis_host", "usb"}, {"ECM", 1, "cdc_ether", "usb"}, {"NCM", 1, "cdc_ncm", "usb"}, {"iPhone", 1, "ipheth", "usb"},
		{"USB_Ethernet", 1, "r8152", ""}, {"PCI_Ethernet", 1, "r8169", ""}, {"WiFi", 2, "8852be", ""}, {"name_not_evidence", 1, "usb0", ""},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if got := sharedNetworkKind(tt.typ, tt.driver); got != tt.want {
				t.Fatalf("got %s want %s", got, tt.want)
			}
		})
	}
}

type sharedTestBus struct {
	calls        []string
	usbConnected bool
	wiredMissing bool
}

func (b *sharedTestBus) close() {}
func (b *sharedTestBus) call(_ context.Context, path dbus.ObjectPath, method string, args ...any) ([]any, error) {
	b.calls = append(b.calls, method)
	if strings.HasSuffix(method, ".GetDevices") {
		return []any{[]dbus.ObjectPath{"/usb", "/pan", "/modem", "/eth", "/wifi"}}, nil
	}
	if !strings.HasSuffix(method, ".GetAll") {
		return nil, fmt.Errorf("不允许写方法 %s", method)
	}
	var p map[string]any
	if path == "/usb" && args[0] == nmEthernetDevice+".Wired" {
		if b.wiredMissing {
			return nil, fmt.Errorf("系统不提供协商速率")
		}
		return []any{ethernetTestProperties(map[string]any{"Speed": uint32(480)})}, nil
	}
	switch path {
	case "/usb":
		p = map[string]any{"Interface": "enx123", "Driver": "rndis_host", "DeviceType": uint32(1), "State": uint32(70), "Managed": true, "ActiveConnection": dbus.ObjectPath("/active")}
		if b.usbConnected {
			p["State"] = uint32(100)
		}
	case "/pan":
		p = map[string]any{"Interface": "bnep0", "DeviceType": uint32(5), "State": uint32(100), "Managed": true, "Mtu": uint32(1500), "ActiveConnection": dbus.ObjectPath("/active"), "Ip4Config": dbus.ObjectPath("/ip")}
	case "/modem":
		p = map[string]any{"Interface": "cdc-wdm0", "DeviceType": uint32(8), "State": uint32(30)}
	case "/eth":
		p = map[string]any{"Interface": "eth1", "Driver": "r8152", "DeviceType": uint32(1)}
	case "/wifi":
		p = map[string]any{"DeviceType": uint32(2)}
	case "/active":
		p = map[string]any{"Uuid": ethernetTestUUID, "Id": "<script>用户配置</script>", "Default": true}
	case "/ip":
		p = map[string]any{"Gateway": "172.20.10.1", "Nameservers": []uint32{0x01010101}, "AddressData": []map[string]dbus.Variant{ethernetTestProperties(map[string]any{"address": "172.20.10.2", "prefix": uint32(28)})}}
	default:
		return nil, fmt.Errorf("未知对象 %s", path)
	}
	return []any{ethernetTestProperties(p)}, nil
}

func TestSharedUSBReportsOnlySystemProvidedSpeed(t *testing.T) {
	for _, missing := range []bool{false, true} {
		t.Run(map[bool]string{false: "系统报告速率", true: "系统未报告速率"}[missing], func(t *testing.T) {
			status, err := sharedNetworkInventory(t.Context(), &sharedTestBus{usbConnected: true, wiredMissing: missing})
			if err != nil {
				t.Fatal(err)
			}
			for _, device := range status.Devices {
				if device.Kind == "usb" && (device.State != "connected" || (!missing && device.Speed != 480) || (missing && device.Speed != 0)) {
					t.Fatalf("速率缺失不能伪造或使设备列表失效：%+v", device)
				}
			}
		})
	}
}

func TestSharedNetworksAreReadOnlyAndRespectCarPlay(t *testing.T) {
	m := New(nil, nil)
	b := &sharedTestBus{}
	m.openNetworkBus = func() (ethernetBus, error) { return b, nil }
	m.carPlayNetworkHold = &CarPlayNetworkLease{RuntimePANUUID: ethernetTestUUID}
	status, err := m.SharedNetworks(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(status.Devices) != 3 {
		t.Fatalf("错误分类 %#v", status)
	}
	for _, d := range status.Devices {
		if d.Kind == "usb" && d.State != "connecting" {
			t.Fatal("获取地址中不能显示已连接")
		}
		if d.Kind == "bluetooth" && (d.Owner != "carplay" || d.IPv4.Addresses[0] != "172.20.10.2/28" || !d.DefaultIPv4) {
			t.Fatalf("PAN 状态错误 %#v", d)
		}
		if d.Kind == "bluetooth" && (d.MTU != 1500 || d.Speed != 0 || d.IPv4.Gateway != "172.20.10.1" || len(d.IPv4.DNS) != 1 || d.IPv4.DNS[0] != "1.1.1.1") {
			t.Fatalf("共享网络不能缺少实际地址信息或伪造蓝牙速率：%#v", d)
		}
		if d.Kind == "cellular" && d.State != "disconnected" {
			t.Fatal("保存配置不能冒充连接")
		}
	}
	for _, call := range b.calls {
		if !strings.HasSuffix(call, ".GetAll") && !strings.HasSuffix(call, ".GetDevices") {
			t.Fatalf("查询产生写入 %s", call)
		}
	}
}
