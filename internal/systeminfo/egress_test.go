package systeminfo

import (
	"context"
	"errors"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestMatchEgressInterface(t *testing.T) {
	for _, tc := range []struct {
		name                      string
		local                     net.Addr
		interfaces                []egressInterface
		wantInterface, wantFamily string
	}{
		{"ipv4", &net.TCPAddr{IP: net.ParseIP("192.168.1.2")}, []egressInterface{{"sta0", []net.Addr{&net.IPNet{IP: net.ParseIP("192.168.1.2"), Mask: net.CIDRMask(24, 32)}}}}, "sta0", "ipv4"},
		{"ipv6", &net.TCPAddr{IP: net.ParseIP("2001:db8::2")}, []egressInterface{{"eth0", []net.Addr{&net.IPNet{IP: net.ParseIP("2001:db8::2"), Mask: net.CIDRMask(64, 128)}}}}, "eth0", "ipv6"},
		{"duplicate_address", &net.TCPAddr{IP: net.ParseIP("192.168.1.2")}, []egressInterface{{"eth0", []net.Addr{&net.IPNet{IP: net.ParseIP("192.168.1.2"), Mask: net.CIDRMask(24, 32)}}}, {"sta0", []net.Addr{&net.IPNet{IP: net.ParseIP("192.168.1.2"), Mask: net.CIDRMask(24, 32)}}}}, "", "ipv4"},
		{"missing_interface", &net.TCPAddr{IP: net.ParseIP("192.168.1.2")}, nil, "", "ipv4"},
		{"loopback", &net.TCPAddr{IP: net.ParseIP("127.0.0.1")}, nil, "", ""},
		{"unspecified", &net.TCPAddr{IP: net.IPv4zero}, nil, "", ""},
		{"unknown_address", nil, nil, "", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := matchEgressInterface(tc.local, tc.interfaces)
			if got.Interface != tc.wantInterface || got.Family != tc.wantFamily || got.Kind != "unknown" {
				t.Fatalf("结果=%+v", got)
			}
		})
	}
}

func TestEgressClassificationDoesNotGuessFromWirelessName(t *testing.T) {
	for _, tc := range []struct{ name, driver, iface, want string }{
		{"ethernet", "rk_gmac-dwmac", "eth0", "ethernet"},
		{"usb_phone", "ipheth", "enx123", "usb"},
		{"usb_router", "cdc_ncm", "usb0", "usb"},
		{"cellular", "qmi_wwan", "wwan0", "cellular"},
		{"bluetooth", "", "bnep0", "bluetooth"},
		{"renamed_unknown", "custom", "wlan0", "unknown"},
		{"tunnel", "", "tun0", "unknown"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := classifyEgressDriver(tc.driver, tc.iface); got != tc.want {
				t.Fatal(got)
			}
		})
	}
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "class/net/sta0/phy80211"), 0o700); err != nil {
		t.Fatal(err)
	}
	if got := egressKind(root, "sta0"); got != "wifi" {
		t.Fatal(got)
	}
	if got := egressKind(root, "../sta0"); got != "unknown" {
		t.Fatal(got)
	}
}

func TestCollectorEgressCacheIsScopedToSuccessfulProbe(t *testing.T) {
	c := NewCollector()
	now := time.Date(2026, 10, 16, 0, 0, 0, 0, time.UTC)
	c.now = func() time.Time { return now }
	c.describeEgress = func(net.Addr) InternetEgress {
		return InternetEgress{Kind: "wifi", Interface: "sta0", LocalIP: "192.168.1.2", Family: "ipv4"}
	}
	var calls int
	c.dialContext = func(context.Context, string, string) (net.Conn, error) {
		calls++
		client, peer := net.Pipe()
		t.Cleanup(func() { _ = peer.Close() })
		return client, nil
	}
	first := c.Collect(t.Context())
	if !first.InternetAvailable || first.InternetEgress == nil || first.InternetEgress.Interface != "sta0" {
		t.Fatalf("%+v", first)
	}
	first.InternetEgress.Interface = "tampered"
	second := c.Collect(t.Context())
	if calls != 1 || second.InternetEgress.Interface != "sta0" {
		t.Fatal("展示请求重发了探测或污染了缓存")
	}
	now = now.Add(internetCacheDuration)
	c.dialContext = func(context.Context, string, string) (net.Conn, error) { return nil, errors.New("offline") }
	failed := c.Collect(t.Context())
	if failed.InternetAvailable || failed.InternetEgress != nil {
		t.Fatal("失败后仍显示上次成功出口")
	}
}
