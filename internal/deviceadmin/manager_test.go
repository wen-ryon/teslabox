package deviceadmin

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/wen-ryon/teslabox/internal/system"
)

type fakeRunner struct {
	responses map[string]string
	failures  map[string][]error
	calls     []string
}

func (r *fakeRunner) Run(_ context.Context, name string, args ...string) ([]byte, error) {
	call := strings.Join(append([]string{name}, args...), " ")
	r.calls = append(r.calls, call)
	if len(r.failures[call]) > 0 {
		err := r.failures[call][0]
		r.failures[call] = r.failures[call][1:]
		if err != nil {
			return nil, err
		}
	}
	return []byte(r.responses[call]), nil
}

func TestParseAccessPointsDeduplicatesAndSorts(t *testing.T) {
	t.Parallel()
	output := "*:Home\\:5G:AA\\:BB\\:CC\\:DD\\:EE\\:01:50:161:WPA2\n:Home\\:5G:AA\\:BB\\:CC\\:DD\\:EE\\:02:90:157:WPA2\n:Cafe:AA\\:BB\\:CC\\:DD\\:EE\\:03:92:6:--\n:hidden:AA\\:BB\\:CC\\:DD\\:EE\\:04:bad:1:WPA2\n"
	items := parseAccessPoints(output, map[string]bool{"Home:5G": true})
	if len(items) != 2 {
		t.Fatalf("items = %#v", items)
	}
	if items[0].SSID != "Cafe" || items[0].Band != "2.4 GHz" || items[0].Security != "OPEN" {
		t.Fatalf("first item = %#v", items[0])
	}
	if items[1].SSID != "Home:5G" || items[1].BSSID != "AA:BB:CC:DD:EE:01" || items[1].Signal != 50 || items[1].Channel != 161 || !items[1].Saved || !items[1].Active {
		t.Fatalf("second item = %#v", items[1])
	}
}

func TestWiFiStatusSeparatesAPAndSavedNetworks(t *testing.T) {
	t.Parallel()
	runner := &fakeRunner{responses: map[string]string{
		"nmcli -t --escape yes -f NAME,UUID,TYPE,DEVICE,AUTOCONNECT connection show": "TeslaBox-5G:11111111-1111-1111-1111-111111111111:802-11-wireless:wlan0:yes\nHome:22222222-2222-2222-2222-222222222222:802-11-wireless::no\n",
		"nmcli -g 802-11-wireless.mode,802-11-wireless.ssid,802-11-wireless.band,802-11-wireless.channel,802-11-wireless-security.key-mgmt,ipv4.method,ipv4.addresses connection show uuid 11111111-1111-1111-1111-111111111111": "ap\nTeslaBox-5G\na\n149\nwpa-psk\nshared\n192.168.88.1/24\n",
		"nmcli -g 802-11-wireless.mode,802-11-wireless.ssid,802-11-wireless.band,802-11-wireless.channel,802-11-wireless-security.key-mgmt,ipv4.method,ipv4.addresses connection show uuid 22222222-2222-2222-2222-222222222222": "infrastructure\nHome\na\n0\nwpa-psk\nauto\n\n",
		"nmcli -t --escape yes -f IN-USE,SSID,BSSID,SIGNAL,CHAN,SECURITY device wifi list ifname wlan0 --rescan no":                                                                                                              ":Home:AA\\:BB\\:CC\\:DD\\:EE\\:01:70:149:WPA2\n",
		"nmcli --escape no -g GENERAL.HWADDR,IP4.ADDRESS device show wlan0": "AA:BB:CC:DD:EE:10\n192.0.2.28/24\n",
	}}
	manager := New(runner, slog.New(slog.NewTextHandler(io.Discard, nil)))
	status, err := manager.WiFiStatus(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !status.APActive || len(status.SavedNetworks) != 1 || status.SavedNetworks[0].SSID != "Home" {
		t.Fatalf("status = %#v", status)
	}
	if status.ConcurrentMode {
		t.Fatalf("AP on wlan0 reported concurrent mode: %#v", status)
	}
	if len(status.AvailableNetworks) != 1 || !status.AvailableNetworks[0].Saved {
		t.Fatalf("available = %#v", status.AvailableNetworks)
	}
	if status.MACAddress != "AA:BB:CC:DD:EE:10" || status.IPAddress != "192.0.2.28/24" {
		t.Fatalf("interface details = %#v", status)
	}
}

func TestAPSettingsIncludesInterfaceDetails(t *testing.T) {
	t.Parallel()
	runner := &fakeRunner{responses: map[string]string{
		"nmcli -t --escape yes -f NAME,UUID,TYPE,DEVICE,AUTOCONNECT connection show": "TeslaBox-5G:11111111-1111-1111-1111-111111111111:802-11-wireless:ap0:yes\n",
		"nmcli -g 802-11-wireless.mode,802-11-wireless.ssid,802-11-wireless.band,802-11-wireless.channel,802-11-wireless-security.key-mgmt,ipv4.method,ipv4.addresses connection show uuid 11111111-1111-1111-1111-111111111111": "ap\nTeslaBox-5G\na\n161\nwpa-psk\nshared\n192.168.88.1/24\n",
		"nmcli --escape no -g GENERAL.HWADDR,IP4.ADDRESS device show ap0": "AA:BB:CC:DD:EE:20\n192.168.88.1/24\n",
	}}
	manager := New(runner, slog.New(slog.NewTextHandler(io.Discard, nil)))
	settings, err := manager.APSettings(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if settings.Interface != "ap0" || settings.MACAddress != "AA:BB:CC:DD:EE:20" || settings.Address != "192.168.88.1/24" {
		t.Fatalf("settings = %#v", settings)
	}
}

func TestAPSettingsReportsUnavailableWithoutWirelessDevice(t *testing.T) {
	t.Parallel()
	runner := &fakeRunner{responses: map[string]string{
		"nmcli -t --escape yes -f NAME,UUID,TYPE,DEVICE,AUTOCONNECT connection show": "TeslaBox-5G:11111111-1111-1111-1111-111111111111:802-11-wireless::yes\n",
		"nmcli -g 802-11-wireless.mode,802-11-wireless.ssid,802-11-wireless.band,802-11-wireless.channel,802-11-wireless-security.key-mgmt,ipv4.method,ipv4.addresses connection show uuid 11111111-1111-1111-1111-111111111111": "ap\nTeslaBox-5G\na\n161\nwpa-psk\nshared\n192.168.88.1/24\n",
		"nmcli -g connection.interface-name connection show uuid 11111111-1111-1111-1111-111111111111":                                                                                                                           "ap0\n",
		"nmcli -t --escape yes -f DEVICE,TYPE,STATE device status":                                                                                                                                                               "eth0:ethernet:connected\n",
	}}
	manager := New(runner, slog.New(slog.NewTextHandler(io.Discard, nil)))

	settings, err := manager.APSettings(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if settings.Available || settings.Enabled {
		t.Fatalf("settings = %#v", settings)
	}
}

func TestAPSettingsReportsUnavailableForWiFiOnlyInterface(t *testing.T) {
	t.Parallel()
	const apUUID = "11111111-1111-1111-1111-111111111111"
	runner := &fakeRunner{responses: map[string]string{
		"nmcli -t --escape yes -f NAME,UUID,TYPE,DEVICE,AUTOCONNECT connection show":                                                                                                                  "TeslaBox-5G:" + apUUID + ":802-11-wireless::yes\n",
		"nmcli -g 802-11-wireless.mode,802-11-wireless.ssid,802-11-wireless.band,802-11-wireless.channel,802-11-wireless-security.key-mgmt,ipv4.method,ipv4.addresses connection show uuid " + apUUID: "ap\nTeslaBox-5G\na\n161\nwpa-psk\nshared\n192.168.88.1/24\n",
		"nmcli -g connection.interface-name connection show uuid " + apUUID:                                                                                                                           "ap0\n",
		"nmcli -t --escape yes -f DEVICE,TYPE,STATE device status":                                                                                                                                    "wlan0:wifi:connected\neth0:ethernet:connected\n",
	}}
	manager := New(runner, slog.New(slog.NewTextHandler(io.Discard, nil)))

	settings, err := manager.APSettings(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if settings.Available || settings.Enabled || settings.Interface != "ap0" {
		t.Fatalf("settings = %#v", settings)
	}
	status, err := manager.WiFiStatus(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !status.Available || status.Interface != "wlan0" {
		t.Fatalf("WiFi-only status = %#v", status)
	}
}

func TestManagerDiscoversNonstandardWiFiAndAPInterfaces(t *testing.T) {
	t.Parallel()
	runner := &fakeRunner{responses: map[string]string{
		"nmcli -t --escape yes -f NAME,UUID,TYPE,DEVICE,AUTOCONNECT connection show": "TeslaBox-5G:11111111-1111-1111-1111-111111111111:802-11-wireless::yes\nHome:22222222-2222-2222-2222-222222222222:802-11-wireless::yes\n",
		"nmcli -g 802-11-wireless.mode,802-11-wireless.ssid,802-11-wireless.band,802-11-wireless.channel,802-11-wireless-security.key-mgmt,ipv4.method,ipv4.addresses connection show uuid 11111111-1111-1111-1111-111111111111": "ap\nTeslaBox-5G\na\n161\nwpa-psk\nshared\n192.168.88.1/24\n",
		"nmcli -g 802-11-wireless.mode,802-11-wireless.ssid,802-11-wireless.band,802-11-wireless.channel,802-11-wireless-security.key-mgmt,ipv4.method,ipv4.addresses connection show uuid 22222222-2222-2222-2222-222222222222": "infrastructure\nHome\na\n0\nwpa-psk\nauto\n\n",
		"nmcli -g connection.interface-name connection show uuid 11111111-1111-1111-1111-111111111111":                                                                                                                           "hotspot7\n",
		"nmcli -t --escape yes -f DEVICE,TYPE,STATE device status":                                                   "hotspot7:wifi:disconnected\nwlp2s0:wifi:connected\neth0:ethernet:connected\n",
		"nmcli -t --escape yes -f IN-USE,SSID,BSSID,SIGNAL,CHAN,SECURITY device wifi list ifname wlp2s0 --rescan no": ":Home:AA\\:BB\\:CC\\:DD\\:EE\\:01:70:161:WPA2\n",
		"nmcli --escape no -g GENERAL.HWADDR,IP4.ADDRESS device show wlp2s0":                                         "AA:BB:CC:DD:EE:10\n192.0.2.28/24\n",
		"nmcli --escape no -g GENERAL.HWADDR,IP4.ADDRESS device show hotspot7":                                       "AA:BB:CC:DD:EE:20\n192.168.88.1/24\n",
	}}
	manager := New(runner, slog.New(slog.NewTextHandler(io.Discard, nil)))
	status, err := manager.WiFiStatus(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if status.Interface != "wlp2s0" || status.MACAddress != "AA:BB:CC:DD:EE:10" {
		t.Fatalf("status = %#v", status)
	}
	settings, err := manager.APSettings(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if settings.Interface != "hotspot7" || settings.MACAddress != "AA:BB:CC:DD:EE:20" {
		t.Fatalf("settings = %#v", settings)
	}
}

func TestUpdateAPClosesActiveHotspotAfterResponseDelay(t *testing.T) {
	t.Parallel()
	const apUUID = "11111111-1111-1111-1111-111111111111"
	runner := &fakeRunner{responses: map[string]string{
		"nmcli -t --escape yes -f NAME,UUID,TYPE,DEVICE,AUTOCONNECT connection show":                                                                                                                  "TeslaBox-5G:" + apUUID + ":802-11-wireless:ap0:yes\n",
		"nmcli -g 802-11-wireless.mode,802-11-wireless.ssid,802-11-wireless.band,802-11-wireless.channel,802-11-wireless-security.key-mgmt,ipv4.method,ipv4.addresses connection show uuid " + apUUID: "ap\nTeslaBox-5G\na\n161\nwpa-psk\nshared\n192.168.88.1/24\n",
		"nmcli -g connection.interface-name connection show uuid " + apUUID:                                                                                                                           "ap0\n",
		"nmcli -t --escape yes -f DEVICE,TYPE,STATE device status":                                                                                                                                    "ap0:wifi:connected\n",
	}}
	manager := New(runner, slog.New(slog.NewTextHandler(io.Discard, nil)))
	var scheduled func()
	manager.schedule = func(delay time.Duration, operation func()) {
		if delay != apDisableDelay {
			t.Fatalf("delay = %s, want %s", delay, apDisableDelay)
		}
		scheduled = operation
	}

	err := manager.UpdateAP(context.Background(), APUpdate{
		Enabled: false, SSID: "TeslaBox-5G", Band: "a", Channel: 161, AutoConnect: false,
	})
	if err != nil {
		t.Fatalf("UpdateAP() error = %v", err)
	}
	downCall := "nmcli --wait 15 connection down uuid " + apUUID
	if containsCall(runner.calls, downCall) {
		t.Fatalf("AP was closed before response: calls = %v", runner.calls)
	}
	if scheduled == nil {
		t.Fatal("AP close operation was not scheduled")
	}
	scheduled()
	if !containsCall(runner.calls, downCall) {
		t.Fatalf("scheduled AP close was not executed: calls = %v", runner.calls)
	}
}

func TestUpdateAPDoesNotRunStaleDisableAfterReenable(t *testing.T) {
	t.Parallel()
	const apUUID = "11111111-1111-1111-1111-111111111111"
	runner := &fakeRunner{responses: map[string]string{
		"nmcli -t --escape yes -f NAME,UUID,TYPE,DEVICE,AUTOCONNECT connection show":                                                                                                                  "TeslaBox-5G:" + apUUID + ":802-11-wireless:ap0:yes\n",
		"nmcli -g 802-11-wireless.mode,802-11-wireless.ssid,802-11-wireless.band,802-11-wireless.channel,802-11-wireless-security.key-mgmt,ipv4.method,ipv4.addresses connection show uuid " + apUUID: "ap\nTeslaBox-5G\na\n161\nwpa-psk\nshared\n192.168.88.1/24\n",
		"nmcli -g connection.interface-name connection show uuid " + apUUID:                                                                                                                           "ap0\n",
		"nmcli -t --escape yes -f DEVICE,TYPE,STATE device status":                                                                                                                                    "ap0:wifi:connected\n",
	}}
	manager := New(runner, slog.New(slog.NewTextHandler(io.Discard, nil)))
	var scheduled func()
	manager.schedule = func(_ time.Duration, operation func()) {
		scheduled = operation
	}
	update := APUpdate{SSID: "TeslaBox-5G", Band: "a", Channel: 161}
	if err := manager.UpdateAP(context.Background(), update); err != nil {
		t.Fatalf("关闭 UpdateAP() error = %v", err)
	}
	update.Enabled = true
	if err := manager.UpdateAP(context.Background(), update); err != nil {
		t.Fatalf("重新启用 UpdateAP() error = %v", err)
	}
	if scheduled == nil {
		t.Fatal("AP close operation was not scheduled")
	}
	scheduled()
	downCall := "nmcli --wait 15 connection down uuid " + apUUID
	if containsCall(runner.calls, downCall) {
		t.Fatalf("旧关闭任务不应关闭已重新启用的 AP: calls = %v", runner.calls)
	}
	upCall := "nmcli --wait 15 connection up uuid " + apUUID + " ifname ap0"
	if !containsCall(runner.calls, upCall) {
		t.Fatalf("AP 未重新启用: calls = %v", runner.calls)
	}
}

func TestWiFiStatusReportsUnavailableWithoutWirelessDevice(t *testing.T) {
	t.Parallel()
	runner := &fakeRunner{responses: map[string]string{
		"nmcli -t --escape yes -f DEVICE,TYPE,STATE device status": "eth0:ethernet:connected\n",
	}}
	manager := New(runner, slog.New(slog.NewTextHandler(io.Discard, nil)))

	status, err := manager.WiFiStatus(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if status.Available || status.Interface != "" {
		t.Fatalf("status = %#v", status)
	}
	if !strings.Contains(status.SingleRadioNotice, "有线管理不受影响") {
		t.Fatalf("notice = %q", status.SingleRadioNotice)
	}
	for _, call := range runner.calls {
		if strings.Contains(call, "device wifi list ifname") {
			t.Fatalf("没有无线网卡时不应扫描: %#v", runner.calls)
		}
	}
}

func TestWiFiStatusUsesSingleRadioAPInterface(t *testing.T) {
	t.Parallel()
	runner := &fakeRunner{responses: map[string]string{
		"nmcli -t --escape yes -f NAME,UUID,TYPE,DEVICE,AUTOCONNECT connection show": "TeslaBox-5G:11111111-1111-1111-1111-111111111111:802-11-wireless:wlan7:yes\n",
		"nmcli -g 802-11-wireless.mode,802-11-wireless.ssid,802-11-wireless.band,802-11-wireless.channel,802-11-wireless-security.key-mgmt,ipv4.method,ipv4.addresses connection show uuid 11111111-1111-1111-1111-111111111111": "ap\nTeslaBox-5G\na\n149\nwpa-psk\nshared\n192.168.88.1/24\n",
		"nmcli -t --escape yes -f DEVICE,TYPE,STATE device status":                                                  "wlan7:wifi:connected\neth0:ethernet:connected\n",
		"nmcli -t --escape yes -f IN-USE,SSID,BSSID,SIGNAL,CHAN,SECURITY device wifi list ifname wlan7 --rescan no": "",
	}}
	manager := New(runner, slog.New(slog.NewTextHandler(io.Discard, nil)))

	status, err := manager.WiFiStatus(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !status.Available || status.Interface != "wlan7" || !status.APActive || status.ConcurrentMode {
		t.Fatalf("status = %#v", status)
	}
}

func TestParseInterfaceDetails(t *testing.T) {
	t.Parallel()
	macAddress, ipAddress := parseInterfaceDetails("AA:bb:CC:dd:EE:ff\nfe80::1/64\n192.168.8.2/24\n")
	if macAddress != "AA:BB:CC:DD:EE:FF" || ipAddress != "192.168.8.2/24" {
		t.Fatalf("mac = %q, ip = %q", macAddress, ipAddress)
	}
}

func TestWiFiStatusIncludesSavedStaticIPv4Settings(t *testing.T) {
	t.Parallel()
	runner := &fakeRunner{responses: map[string]string{
		"nmcli -t --escape yes -f NAME,UUID,TYPE,DEVICE,AUTOCONNECT connection show": "Home:22222222-2222-2222-2222-222222222222:802-11-wireless:wlan0:yes\n",
		"nmcli -g 802-11-wireless.mode,802-11-wireless.ssid,802-11-wireless.band,802-11-wireless.channel,802-11-wireless-security.key-mgmt,ipv4.method,ipv4.addresses connection show uuid 22222222-2222-2222-2222-222222222222": "infrastructure\nHome\na\n0\nwpa-psk\nmanual\n192.0.2.20/24\n",
		"nmcli -g ipv4.gateway,ipv4.dns connection show uuid 22222222-2222-2222-2222-222222222222":                                                                                                                               "192.0.2.1\n223.5.5.5\n1.1.1.1\n",
		"nmcli --escape no -g GENERAL.HWADDR,IP4.ADDRESS device show wlan0":                                                                                                                                                      "AA:BB:CC:DD:EE:10\n192.0.2.20/24\n",
	}}
	manager := New(runner, slog.New(slog.NewTextHandler(io.Discard, nil)))
	status, err := manager.WiFiStatus(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(status.SavedNetworks) != 1 {
		t.Fatalf("saved networks = %#v", status.SavedNetworks)
	}
	settings := status.SavedNetworks[0].IPv4
	if settings.Mode != "static" || settings.Address != "192.0.2.20" || settings.SubnetMask != "255.255.255.0" || settings.Gateway != "192.0.2.1" || len(settings.DNS) != 2 {
		t.Fatalf("static ipv4 settings = %#v", settings)
	}
}

func TestUpdateIPv4PersistsStaticProfileAndReactivatesCurrentWiFi(t *testing.T) {
	t.Parallel()
	runner := &fakeRunner{responses: map[string]string{
		"nmcli -t --escape yes -f NAME,UUID,TYPE,DEVICE,AUTOCONNECT connection show": "Home:22222222-2222-2222-2222-222222222222:802-11-wireless:wlan0:yes\n",
		"nmcli -g 802-11-wireless.mode,802-11-wireless.ssid,802-11-wireless.band,802-11-wireless.channel,802-11-wireless-security.key-mgmt,ipv4.method,ipv4.addresses connection show uuid 22222222-2222-2222-2222-222222222222": "infrastructure\nHome\na\n0\nwpa-psk\nauto\n\n",
	}}
	manager := New(runner, slog.New(slog.NewTextHandler(io.Discard, nil)))
	request := IPv4Update{
		UUID: "22222222-2222-2222-2222-222222222222", Mode: "static",
		Address: "192.0.2.20", SubnetMask: "255.255.255.0", Gateway: "192.0.2.1", DNS: []string{"223.5.5.5", "1.1.1.1"},
	}
	if err := manager.UpdateIPv4(context.Background(), request); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"nmcli connection modify uuid 22222222-2222-2222-2222-222222222222 ipv4.method manual ipv4.addresses 192.0.2.20/24 ipv4.gateway 192.0.2.1 ipv4.dns 223.5.5.5,1.1.1.1 ipv4.ignore-auto-dns yes",
		"nmcli --wait 15 connection up uuid 22222222-2222-2222-2222-222222222222 ifname wlan0",
	} {
		if !containsCall(runner.calls, want) {
			t.Fatalf("missing call %q in %#v", want, runner.calls)
		}
	}
}

func TestUpdateIPv4RestoresDHCPWithoutReconnectingInactiveWiFi(t *testing.T) {
	t.Parallel()
	runner := &fakeRunner{responses: map[string]string{
		"nmcli -t --escape yes -f NAME,UUID,TYPE,DEVICE,AUTOCONNECT connection show": "Home:22222222-2222-2222-2222-222222222222:802-11-wireless::yes\n",
		"nmcli -g 802-11-wireless.mode,802-11-wireless.ssid,802-11-wireless.band,802-11-wireless.channel,802-11-wireless-security.key-mgmt,ipv4.method,ipv4.addresses connection show uuid 22222222-2222-2222-2222-222222222222": "infrastructure\nHome\na\n0\nwpa-psk\nmanual\n192.0.2.20/24\n",
	}}
	manager := New(runner, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err := manager.UpdateIPv4(context.Background(), IPv4Update{
		UUID: "22222222-2222-2222-2222-222222222222", Mode: "dhcp",
	}); err != nil {
		t.Fatal(err)
	}
	const want = "nmcli connection modify uuid 22222222-2222-2222-2222-222222222222 ipv4.method auto ipv4.addresses  ipv4.gateway  ipv4.dns  ipv4.ignore-auto-dns no"
	if !containsCall(runner.calls, want) {
		t.Fatalf("missing call %q in %#v", want, runner.calls)
	}
	for _, call := range runner.calls {
		if strings.Contains(call, "connection up uuid") {
			t.Fatalf("inactive WiFi should not reconnect: %#v", runner.calls)
		}
	}
}

func TestNormalizeIPv4UpdateRejectsInvalidStaticNetwork(t *testing.T) {
	t.Parallel()
	base := IPv4Update{Mode: "static", Address: "192.0.2.20", SubnetMask: "255.255.255.0", Gateway: "192.0.2.1", DNS: []string{"223.5.5.5"}}
	for _, test := range []struct {
		name   string
		mutate func(*IPv4Update)
	}{
		{name: "non-contiguous mask", mutate: func(value *IPv4Update) { value.SubnetMask = "255.0.255.0" }},
		{name: "gateway outside subnet", mutate: func(value *IPv4Update) { value.Gateway = "192.168.121.1" }},
		{name: "network address", mutate: func(value *IPv4Update) { value.Address = "192.0.2.0" }},
		{name: "ipv6 dns", mutate: func(value *IPv4Update) { value.DNS = []string{"2001:4860:4860::8888"} }},
	} {
		t.Run(test.name, func(t *testing.T) {
			value := base
			value.DNS = append([]string{}, base.DNS...)
			test.mutate(&value)
			if _, err := normalizeIPv4Update(value); err == nil {
				t.Fatal("normalizeIPv4Update() error = nil")
			}
		})
	}
	dhcp, err := normalizeIPv4Update(IPv4Update{Mode: "dhcp", Address: "192.0.2.20", DNS: []string{"223.5.5.5"}})
	if err != nil || dhcp.Address != "" || len(dhcp.DNS) != 0 {
		t.Fatalf("dhcp normalization = %#v, %v", dhcp, err)
	}
}

func TestWiFiStatusReportsRealAPSTAConcurrency(t *testing.T) {
	t.Parallel()
	runner := &fakeRunner{responses: map[string]string{
		"nmcli -t --escape yes -f NAME,UUID,TYPE,DEVICE,AUTOCONNECT connection show": "Home:22222222-2222-2222-2222-222222222222:802-11-wireless:wlan0:yes\nTeslaBox-5G:11111111-1111-1111-1111-111111111111:802-11-wireless:ap0:yes\n",
		"nmcli -g 802-11-wireless.mode,802-11-wireless.ssid,802-11-wireless.band,802-11-wireless.channel,802-11-wireless-security.key-mgmt,ipv4.method,ipv4.addresses connection show uuid 11111111-1111-1111-1111-111111111111": "ap\nTeslaBox-5G\na\n161\nwpa-psk\nshared\n192.168.88.1/24\n",
		"nmcli -g 802-11-wireless.mode,802-11-wireless.ssid,802-11-wireless.band,802-11-wireless.channel,802-11-wireless-security.key-mgmt,ipv4.method,ipv4.addresses connection show uuid 22222222-2222-2222-2222-222222222222": "infrastructure\nHome\na\n0\nwpa-psk\nauto\n\n",
		"nmcli -t --escape yes -f IN-USE,SSID,BSSID,SIGNAL,CHAN,SECURITY device wifi list ifname wlan0 --rescan no":                                                                                                              "*:Home:AA\\:BB\\:CC\\:DD\\:EE\\:01:70:161:WPA2\n",
	}}
	manager := New(runner, slog.New(slog.NewTextHandler(io.Discard, nil)))
	status, err := manager.WiFiStatus(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !status.ConcurrentMode || !status.ChannelCoupled || status.CurrentConnection != "Home" {
		t.Fatalf("status = %#v", status)
	}
}

func TestConnectAlignsAPChannelAndLocksBSSID(t *testing.T) {
	t.Parallel()
	runner := &fakeRunner{responses: map[string]string{
		"nmcli -t --escape yes -f NAME,UUID,TYPE,DEVICE,AUTOCONNECT connection show": "TeslaBox-5G:11111111-1111-1111-1111-111111111111:802-11-wireless:ap0:yes\nHome:22222222-2222-2222-2222-222222222222:802-11-wireless::no\n",
		"nmcli -g 802-11-wireless.mode,802-11-wireless.ssid,802-11-wireless.band,802-11-wireless.channel,802-11-wireless-security.key-mgmt,ipv4.method,ipv4.addresses connection show uuid 11111111-1111-1111-1111-111111111111": "ap\nTeslaBox-5G\na\n149\nwpa-psk\nshared\n192.168.88.1/24\n",
		"nmcli -g 802-11-wireless.mode,802-11-wireless.ssid,802-11-wireless.band,802-11-wireless.channel,802-11-wireless-security.key-mgmt,ipv4.method,ipv4.addresses connection show uuid 22222222-2222-2222-2222-222222222222": "infrastructure\nHome\na\n0\nwpa-psk\nauto\n\n",
		"nmcli -t --escape yes -f DEVICE,TYPE,STATE device status":                                                  "ap0:wifi:connected\nwlan0:wifi:disconnected\n",
		"nmcli -t --escape yes -f IN-USE,SSID,BSSID,SIGNAL,CHAN,SECURITY device wifi list ifname wlan0 --rescan no": ":Home:AA\\:BB\\:CC\\:DD\\:EE\\:01:90:161:WPA2\n",
	}}
	manager := New(runner, slog.New(slog.NewTextHandler(io.Discard, nil)))
	request := ConnectRequest{UUID: "22222222-2222-2222-2222-222222222222"}
	if err := manager.Connect(context.Background(), request); err != nil {
		t.Fatal(err)
	}
	wantCalls := []string{
		"nmcli connection modify uuid 11111111-1111-1111-1111-111111111111 802-11-wireless.band a 802-11-wireless.channel 161",
		"nmcli --wait 20 connection up uuid 11111111-1111-1111-1111-111111111111 ifname ap0",
		"nmcli connection modify uuid 22222222-2222-2222-2222-222222222222 802-11-wireless.bssid AA:BB:CC:DD:EE:01",
		"nmcli --wait 15 connection up uuid 22222222-2222-2222-2222-222222222222 ifname wlan0",
	}
	for _, want := range wantCalls {
		if !containsCall(runner.calls, want) {
			t.Fatalf("missing call %q in %#v", want, runner.calls)
		}
	}
}

func TestConnectKeepsIndependentAPRadioOnItsOwnChannel(t *testing.T) {
	t.Parallel()
	const (
		apUUID       = "11111111-1111-1111-1111-111111111111"
		upstreamUUID = "22222222-2222-2222-2222-222222222222"
	)
	runner := &fakeRunner{responses: map[string]string{
		"nmcli -t --escape yes -f NAME,UUID,TYPE,DEVICE,AUTOCONNECT connection show":                                                                                                                        "TeslaBox-5G:" + apUUID + ":802-11-wireless:ap0:yes\nHome:" + upstreamUUID + ":802-11-wireless::no\n",
		"nmcli -g 802-11-wireless.mode,802-11-wireless.ssid,802-11-wireless.band,802-11-wireless.channel,802-11-wireless-security.key-mgmt,ipv4.method,ipv4.addresses connection show uuid " + apUUID:       "ap\nTeslaBox-5G\nbg\n6\nwpa-psk\nshared\n192.168.88.1/24\n",
		"nmcli -g 802-11-wireless.mode,802-11-wireless.ssid,802-11-wireless.band,802-11-wireless.channel,802-11-wireless-security.key-mgmt,ipv4.method,ipv4.addresses connection show uuid " + upstreamUUID: "infrastructure\nHome\na\n0\nwpa-psk\nauto\n\n",
		"nmcli -t --escape yes -f DEVICE,TYPE,STATE device status":                                                                                                                                          "ap0:wifi:connected\nwlan0:wifi:disconnected\n",
		"nmcli -t --escape yes -f IN-USE,SSID,BSSID,SIGNAL,CHAN,SECURITY device wifi list ifname wlan0 --rescan no":                                                                                         ":Home:AA\\:BB\\:CC\\:DD\\:EE\\:01:90:161:WPA2\n",
	}}
	manager := New(runner, slog.New(slog.NewTextHandler(io.Discard, nil)))
	manager.sameWirelessPHY = func(_, _ string) (bool, bool) { return false, true }

	if err := manager.Connect(context.Background(), ConnectRequest{UUID: upstreamUUID}); err != nil {
		t.Fatal(err)
	}
	for _, call := range runner.calls {
		if strings.Contains(call, "uuid "+apUUID+" 802-11-wireless.band") ||
			strings.Contains(call, "connection up uuid "+apUUID) {
			t.Fatalf("独立 AP 网卡不应跟随上游信道重启: calls = %#v", runner.calls)
		}
	}
	want := "nmcli --wait 15 connection up uuid " + upstreamUUID + " ifname wlan0"
	if !containsCall(runner.calls, want) {
		t.Fatalf("独立客户端网卡未连接上游: calls = %#v", runner.calls)
	}
	status, err := manager.WiFiStatus(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !status.ConcurrentMode || status.ChannelCoupled || !strings.Contains(status.SingleRadioNotice, "独立无线设备") {
		t.Fatalf("status = %#v", status)
	}
}

func TestAlignAPChannelFailureRestoresPreviousNetworkState(t *testing.T) {
	t.Parallel()
	const (
		apUUID       = "11111111-1111-1111-1111-111111111111"
		upstreamUUID = "22222222-2222-2222-2222-222222222222"
	)
	apUpCall := "nmcli --wait 20 connection up uuid " + apUUID + " ifname ap0"
	runner := &fakeRunner{
		responses: map[string]string{
			"nmcli -t --escape yes -f NAME,UUID,TYPE,DEVICE,AUTOCONNECT connection show":                                                                                                                        "TeslaBox-5G:" + apUUID + ":802-11-wireless:ap0:yes\nHome:" + upstreamUUID + ":802-11-wireless:wlan0:yes\n",
			"nmcli -g 802-11-wireless.mode,802-11-wireless.ssid,802-11-wireless.band,802-11-wireless.channel,802-11-wireless-security.key-mgmt,ipv4.method,ipv4.addresses connection show uuid " + apUUID:       "ap\nTeslaBox-5G\na\n149\nwpa-psk\nshared\n192.168.88.1/24\n",
			"nmcli -g 802-11-wireless.mode,802-11-wireless.ssid,802-11-wireless.band,802-11-wireless.channel,802-11-wireless-security.key-mgmt,ipv4.method,ipv4.addresses connection show uuid " + upstreamUUID: "infrastructure\nHome\na\n0\nwpa-psk\nauto\n\n",
			"nmcli -g connection.interface-name connection show uuid " + apUUID:                                                                                                                                 "ap0\n",
			"nmcli -t --escape yes -f DEVICE,TYPE,STATE device status":                                                                                                                                          "ap0:wifi:connected\nwlan0:wifi:connected\n",
		},
		failures: map[string][]error{
			apUpCall: {errors.New("热点启动失败")},
		},
	}
	manager := New(runner, slog.New(slog.NewTextHandler(io.Discard, nil)))
	status := WiFiStatus{
		Interface:         "wlan0",
		CurrentConnection: "Home",
		SavedNetworks: []SavedNetwork{{
			Name: "Home", UUID: upstreamUUID,
		}},
	}

	if err := manager.alignAPChannel(context.Background(), status, 161); err == nil {
		t.Fatal("alignAPChannel() error = nil")
	}
	wantCalls := []string{
		"nmcli connection modify uuid " + apUUID + " 802-11-wireless.band a 802-11-wireless.channel 149",
		apUpCall,
		"nmcli --wait 15 connection up uuid " + upstreamUUID + " ifname wlan0",
	}
	for _, want := range wantCalls {
		if !containsCall(runner.calls, want) {
			t.Fatalf("missing rollback call %q in %#v", want, runner.calls)
		}
	}
}

func TestReconcileAutoConnectUsesPriorityAndRealignsAP(t *testing.T) {
	t.Parallel()
	runner := &fakeRunner{responses: map[string]string{
		"nmcli -t --escape yes -f NAME,UUID,TYPE,DEVICE,AUTOCONNECT connection show": "TeslaBox-5G:11111111-1111-1111-1111-111111111111:802-11-wireless:ap0:yes\nHome:22222222-2222-2222-2222-222222222222:802-11-wireless::yes\nBackup:33333333-3333-3333-3333-333333333333:802-11-wireless::yes\n",
		"nmcli -g 802-11-wireless.mode,802-11-wireless.ssid,802-11-wireless.band,802-11-wireless.channel,802-11-wireless-security.key-mgmt,ipv4.method,ipv4.addresses connection show uuid 11111111-1111-1111-1111-111111111111": "ap\nTeslaBox-5G\na\n161\nwpa-psk\nshared\n192.168.88.1/24\n",
		"nmcli -g 802-11-wireless.mode,802-11-wireless.ssid,802-11-wireless.band,802-11-wireless.channel,802-11-wireless-security.key-mgmt,ipv4.method,ipv4.addresses connection show uuid 22222222-2222-2222-2222-222222222222": "infrastructure\nHome\na\n0\nwpa-psk\nauto\n\n",
		"nmcli -g 802-11-wireless.mode,802-11-wireless.ssid,802-11-wireless.band,802-11-wireless.channel,802-11-wireless-security.key-mgmt,ipv4.method,ipv4.addresses connection show uuid 33333333-3333-3333-3333-333333333333": "infrastructure\nBackup\na\n0\nwpa-psk\nauto\n\n",
		"nmcli -t --escape yes -f DEVICE,TYPE,STATE device status":                                                  "ap0:wifi:connected\nwlan0:wifi:disconnected\n",
		"nmcli -t --escape yes -f IN-USE,SSID,BSSID,SIGNAL,CHAN,SECURITY device wifi list ifname wlan0 --rescan no": ":Home:AA\\:BB\\:CC\\:DD\\:EE\\:01:80:165:WPA2\n:Backup:AA\\:BB\\:CC\\:DD\\:EE\\:02:90:161:WPA2\n",
		"nmcli -g connection.autoconnect-priority connection show uuid 22222222-2222-2222-2222-222222222222":        "90\n",
		"nmcli -g connection.autoconnect-priority connection show uuid 33333333-3333-3333-3333-333333333333":        "10\n",
	}}
	manager := New(runner, slog.New(slog.NewTextHandler(io.Discard, nil)))
	recovered, ssid, err := manager.reconcileAutoConnect(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !recovered || ssid != "Home" {
		t.Fatalf("recovered = %t, ssid = %q", recovered, ssid)
	}
	wantCalls := []string{
		"nmcli --wait 12 device wifi rescan ifname wlan0",
		"nmcli connection modify uuid 11111111-1111-1111-1111-111111111111 802-11-wireless.band a 802-11-wireless.channel 165",
		"nmcli --wait 20 connection up uuid 11111111-1111-1111-1111-111111111111 ifname ap0",
		"nmcli connection modify uuid 22222222-2222-2222-2222-222222222222 802-11-wireless.bssid AA:BB:CC:DD:EE:01",
		"nmcli --wait 15 connection up uuid 22222222-2222-2222-2222-222222222222 ifname wlan0",
	}
	for _, want := range wantCalls {
		if !containsCall(runner.calls, want) {
			t.Fatalf("missing call %q in %#v", want, runner.calls)
		}
	}
}

func TestReconcileAutoConnectFollowsActiveUpstreamChannel(t *testing.T) {
	t.Parallel()
	runner := &fakeRunner{responses: map[string]string{
		"nmcli -t --escape yes -f NAME,UUID,TYPE,DEVICE,AUTOCONNECT connection show": "Home:22222222-2222-2222-2222-222222222222:802-11-wireless:wlan0:yes\nTeslaBox-5G:11111111-1111-1111-1111-111111111111:802-11-wireless:ap0:yes\n",
		"nmcli -g 802-11-wireless.mode,802-11-wireless.ssid,802-11-wireless.band,802-11-wireless.channel,802-11-wireless-security.key-mgmt,ipv4.method,ipv4.addresses connection show uuid 11111111-1111-1111-1111-111111111111": "ap\nTeslaBox-5G\na\n149\nwpa-psk\nshared\n192.168.88.1/24\n",
		"nmcli -g 802-11-wireless.mode,802-11-wireless.ssid,802-11-wireless.band,802-11-wireless.channel,802-11-wireless-security.key-mgmt,ipv4.method,ipv4.addresses connection show uuid 22222222-2222-2222-2222-222222222222": "infrastructure\nHome\na\n0\nwpa-psk\nauto\n\n",
		"nmcli -t --escape yes -f IN-USE,SSID,BSSID,SIGNAL,CHAN,SECURITY device wifi list ifname wlan0 --rescan no":                                                                                                              "*:Home:AA\\:BB\\:CC\\:DD\\:EE\\:09:78:161:WPA2\n",
	}}
	manager := New(runner, slog.New(slog.NewTextHandler(io.Discard, nil)))
	recovered, ssid, err := manager.reconcileAutoConnect(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !recovered || ssid != "Home" {
		t.Fatalf("recovered = %t, ssid = %q", recovered, ssid)
	}
	wantCalls := []string{
		"nmcli device disconnect wlan0",
		"nmcli connection modify uuid 11111111-1111-1111-1111-111111111111 802-11-wireless.band a 802-11-wireless.channel 161",
		"nmcli --wait 20 connection up uuid 11111111-1111-1111-1111-111111111111 ifname ap0",
		"nmcli connection modify uuid 22222222-2222-2222-2222-222222222222 802-11-wireless.bssid AA:BB:CC:DD:EE:09",
		"nmcli --wait 15 connection up uuid 22222222-2222-2222-2222-222222222222 ifname wlan0",
	}
	for _, want := range wantCalls {
		if !containsCall(runner.calls, want) {
			t.Fatalf("missing call %q in %#v", want, runner.calls)
		}
	}
}

func TestReconcileAutoConnectIfNeededSkipsFullInventoryForHealthyLink(t *testing.T) {
	runner := &fakeRunner{responses: map[string]string{
		"nmcli -g GENERAL.STATE device show wlan0": "100 (connected)\n",
	}}
	manager := New(runner, slog.New(slog.NewTextHandler(io.Discard, nil)))
	manager.monitoredWiFiInterface = "wlan0"

	recovered, ssid, err := manager.reconcileAutoConnectIfNeeded(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if recovered || ssid != "" {
		t.Fatalf("healthy link recovery = (%t, %q), want no recovery", recovered, ssid)
	}
	if len(runner.calls) != 1 || runner.calls[0] != "nmcli -g GENERAL.STATE device show wlan0" {
		t.Fatalf("healthy link calls = %v, want only lightweight state query", runner.calls)
	}
}

func TestReconcileAutoConnectIfNeededRunsFullInventoryOnFirstCheck(t *testing.T) {
	runner := &fakeRunner{responses: map[string]string{
		"nmcli -t --escape yes -f NAME,UUID,TYPE,DEVICE,AUTOCONNECT connection show":                                "",
		"nmcli -t --escape yes -f DEVICE,TYPE,STATE device status":                                                  "wlan0:wifi:disconnected\n",
		"nmcli -t --escape yes -f IN-USE,SSID,BSSID,SIGNAL,CHAN,SECURITY device wifi list ifname wlan0 --rescan no": "",
	}}
	manager := New(runner, slog.New(slog.NewTextHandler(io.Discard, nil)))

	if _, _, err := manager.reconcileAutoConnectIfNeeded(context.Background()); err != nil {
		t.Fatal(err)
	}
	if manager.monitoredWiFiInterface != "wlan0" {
		t.Fatalf("monitored WiFi interface = %q, want wlan0", manager.monitoredWiFiInterface)
	}
	calls := strings.Join(runner.calls, "\n")
	if !strings.Contains(calls, "nmcli -t --escape yes -f NAME,UUID,TYPE,DEVICE,AUTOCONNECT connection show") ||
		!strings.Contains(calls, "nmcli -t --escape yes -f IN-USE,SSID,BSSID,SIGNAL,CHAN,SECURITY device wifi list ifname wlan0 --rescan no") {
		t.Fatalf("first check calls = %v, want full profile and cached access-point inventory", runner.calls)
	}
}

func containsCall(calls []string, expected string) bool {
	for _, call := range calls {
		if call == expected {
			return true
		}
	}
	return false
}

func TestValidationAndPowerScheduling(t *testing.T) {
	t.Parallel()
	if err := validateSSID(""); err == nil {
		t.Fatal("validateSSID() error = nil")
	}
	if err := validatePassword("short"); err == nil {
		t.Fatal("validatePassword() error = nil")
	}
	if !validBandChannel("a", 36) || !validBandChannel("a", 149) || !validBandChannel("a", 165) ||
		validBandChannel("a", 6) || !validBandChannel("bg", 13) || validBandChannel("bg", 14) {
		t.Fatal("validBandChannel() returned unexpected result")
	}
	if _, ok := concurrentBand(100); ok {
		t.Fatal("DFS 信道不应进入单射频 AP+STA 自动对齐")
	}
	runner := &fakeRunner{responses: map[string]string{}}
	manager := New(runner, slog.New(slog.NewTextHandler(io.Discard, nil)))
	manager.schedule = func(_ time.Duration, operation func()) { operation() }
	if err := manager.SchedulePower("reboot"); err != nil {
		t.Fatal(err)
	}
	if len(runner.calls) != 1 || runner.calls[0] != "systemctl reboot" {
		t.Fatalf("calls = %#v", runner.calls)
	}
	if err := manager.SchedulePower("invalid"); err == nil {
		t.Fatal("SchedulePower(invalid) error = nil")
	}
}

func TestIsNMDeviceInactive(t *testing.T) {
	t.Parallel()
	if !isNMDeviceInactive(&system.CommandError{Code: 6, Output: "Error: This device is not active"}) {
		t.Fatal("NetworkManager 非活动设备错误未被识别")
	}
	if isNMDeviceInactive(&system.CommandError{Code: 6, Output: "permission denied"}) {
		t.Fatal("无关的 NetworkManager 错误被误判")
	}
}
