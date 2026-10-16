package deviceadmin

import (
	"context"
	"fmt"
	"net/netip"
	"reflect"
	"testing"

	"github.com/godbus/dbus/v5"
)

type wifiDetailsBus struct {
	props   map[string]ethernetProperties
	reads   int
	changed bool
	fail    string
}

func (b *wifiDetailsBus) close() {}
func (b *wifiDetailsBus) call(_ context.Context, path dbus.ObjectPath, method string, args ...any) ([]any, error) {
	if method == nmEthernetManager+".GetDeviceByIpIface" {
		if args[0] != "sta0" {
			return nil, fmt.Errorf("接口错误")
		}
		return []any{dbus.ObjectPath("/device")}, nil
	}
	if method != "org.freedesktop.DBus.Properties.GetAll" {
		return nil, fmt.Errorf("不允许的写方法 %s", method)
	}
	key := string(path) + ":" + args[0].(string)
	if key == b.fail {
		return nil, fmt.Errorf("读取失败")
	}
	if key == "/device:"+nmEthernetDevice {
		b.reads++
		if b.changed && b.reads > 1 {
			return []any{ethernetTestProperties(map[string]any{"State": uint32(100), "ActiveConnection": dbus.ObjectPath("/other")})}, nil
		}
	}
	values, ok := b.props[key]
	if !ok {
		return nil, fmt.Errorf("未提供属性 %s", key)
	}
	return []any{values}, nil
}

func wifiDetailsFixture() *wifiDetailsBus {
	v6 := netip.MustParseAddr("2001:4860:4860::8888").As16()
	return &wifiDetailsBus{props: map[string]ethernetProperties{
		"/device:" + nmEthernetDevice: ethernetTestProperties(map[string]any{
			"DeviceType": uint32(2), "Interface": "sta0", "State": uint32(100), "Mtu": uint32(1500),
			"ActiveConnection": dbus.ObjectPath("/active"), "Ip4Config": dbus.ObjectPath("/ip4"), "Ip6Config": dbus.ObjectPath("/ip6"),
		}),
		"/active:" + nmEthernetManager + ".Connection.Active": ethernetTestProperties(map[string]any{"Uuid": wifiStatusUUIDA}),
		"/device:" + nmEthernetDevice + ".Wireless": ethernetTestProperties(map[string]any{
			"Mode": uint32(2), "Bitrate": uint32(866700), "ActiveAccessPoint": dbus.ObjectPath("/ap"),
		}),
		"/ap:" + nmEthernetManager + ".AccessPoint": ethernetTestProperties(map[string]any{
			"Frequency": uint32(5220), "Strength": uint8(0), "MaxBitrate": uint32(9999999),
		}),
		"/ip4:" + nmEthernetManager + ".IP4Config": ethernetTestProperties(map[string]any{
			"Gateway": "192.168.1.1", "Nameservers": []uint32{0x01010101},
			"AddressData": []map[string]dbus.Variant{ethernetTestProperties(map[string]any{"address": "192.168.1.2", "prefix": uint32(24)})},
		}),
		"/ip6:" + nmEthernetManager + ".IP6Config": ethernetTestProperties(map[string]any{
			"Gateway": "fe80::1", "Nameservers": [][]byte{v6[:]},
			"AddressData": []map[string]dbus.Variant{ethernetTestProperties(map[string]any{"address": "2001:db8::2", "prefix": uint32(64)})},
		}),
	}}
}

func TestReadWiFiDetails(t *testing.T) {
	t.Parallel()
	got := readWiFiDetails(t.Context(), wifiDetailsFixture(), "sta0", wifiStatusUUIDA)
	if !got.Available || got.BitrateKbps != 866700 || got.FrequencyMHz != 5220 || got.Signal == nil || *got.Signal != 0 || got.MTU != 1500 {
		t.Fatalf("无线实际指标错误：%+v", got)
	}
	if !reflect.DeepEqual(got.Gateways, []string{"192.168.1.1", "fe80::1"}) ||
		!reflect.DeepEqual(got.DNS, []string{"1.1.1.1", "2001:4860:4860::8888"}) || len(got.Addresses) != 2 {
		t.Fatalf("必须使用当前连接的双栈地址：%+v", got)
	}
}

func TestReadWiFiDetailsRejectsStaleIdentityAndReadFailures(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		name   string
		change func(*wifiDetailsBus)
	}{
		{"切换期间", func(b *wifiDetailsBus) { b.changed = true }},
		{"其他配置", func(b *wifiDetailsBus) {
			b.props["/active:"+nmEthernetManager+".Connection.Active"]["Uuid"] = dbus.MakeVariant(wifiStatusUUIDB)
		}},
		{"非客户端", func(b *wifiDetailsBus) {
			b.props["/device:"+nmEthernetDevice+".Wireless"]["Mode"] = dbus.MakeVariant(uint32(3))
		}},
		{"地址读取失败", func(b *wifiDetailsBus) { b.fail = "/ip4:" + nmEthernetManager + ".IP4Config" }},
		{"断开中", func(b *wifiDetailsBus) { b.props["/device:"+nmEthernetDevice]["State"] = dbus.MakeVariant(uint32(110)) }},
	} {
		t.Run(tt.name, func(t *testing.T) {
			b := wifiDetailsFixture()
			tt.change(b)
			if got := readWiFiDetails(t.Context(), b, "sta0", wifiStatusUUIDA); !reflect.DeepEqual(got, WiFiDetails{}) {
				t.Fatalf("不能返回不属于当前连接的信息：%+v", got)
			}
		})
	}
}

func TestReadWiFiDetailsMissingRadioMetricsStillReportsAddresses(t *testing.T) {
	b := wifiDetailsFixture()
	b.fail = "/device:" + nmEthernetDevice + ".Wireless"
	got := readWiFiDetails(t.Context(), b, "sta0", wifiStatusUUIDA)
	if !got.Available || got.Signal != nil || got.BitrateKbps != 0 || len(got.Gateways) != 2 {
		t.Fatalf("可选指标缺失不应丢弃有效地址或捏造速率：%+v", got)
	}
}

func TestWiFiDetailsDoesNotQueryUnconfirmedConnection(t *testing.T) {
	m := New(nil, nil)
	m.openNetworkBus = func() (ethernetBus, error) { t.Fatal("未确认连接不应查询总线"); return nil, nil }
	for _, state := range []string{"connecting", "unknown", "disconnected"} {
		m.WiFiDetails(t.Context(), WiFiStatus{Available: true, Interface: "sta0", ConnectionUUID: wifiStatusUUIDA, ConnectionState: state})
	}
}
