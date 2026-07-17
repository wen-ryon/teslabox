package deviceadmin

import (
	"context"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/wen-ryon/teslabox/internal/system"
)

type fakeRunner struct {
	responses map[string]string
	calls     []string
}

func (r *fakeRunner) Run(_ context.Context, name string, args ...string) ([]byte, error) {
	call := strings.Join(append([]string{name}, args...), " ")
	r.calls = append(r.calls, call)
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
	if !status.ConcurrentMode || status.CurrentConnection != "Home" {
		t.Fatalf("status = %#v", status)
	}
}

func TestConnectAlignsAPChannelAndLocksBSSID(t *testing.T) {
	t.Parallel()
	runner := &fakeRunner{responses: map[string]string{
		"nmcli -t --escape yes -f NAME,UUID,TYPE,DEVICE,AUTOCONNECT connection show": "TeslaBox-5G:11111111-1111-1111-1111-111111111111:802-11-wireless:ap0:yes\nHome:22222222-2222-2222-2222-222222222222:802-11-wireless::no\n",
		"nmcli -g 802-11-wireless.mode,802-11-wireless.ssid,802-11-wireless.band,802-11-wireless.channel,802-11-wireless-security.key-mgmt,ipv4.method,ipv4.addresses connection show uuid 11111111-1111-1111-1111-111111111111": "ap\nTeslaBox-5G\na\n149\nwpa-psk\nshared\n192.168.88.1/24\n",
		"nmcli -g 802-11-wireless.mode,802-11-wireless.ssid,802-11-wireless.band,802-11-wireless.channel,802-11-wireless-security.key-mgmt,ipv4.method,ipv4.addresses connection show uuid 22222222-2222-2222-2222-222222222222": "infrastructure\nHome\na\n0\nwpa-psk\nauto\n\n",
		"nmcli -t --escape yes -f IN-USE,SSID,BSSID,SIGNAL,CHAN,SECURITY device wifi list ifname wlan0 --rescan no":                                                                                                              ":Home:AA\\:BB\\:CC\\:DD\\:EE\\:01:90:161:WPA2\n",
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

func TestReconcileAutoConnectUsesPriorityAndRealignsAP(t *testing.T) {
	t.Parallel()
	runner := &fakeRunner{responses: map[string]string{
		"nmcli -t --escape yes -f NAME,UUID,TYPE,DEVICE,AUTOCONNECT connection show": "TeslaBox-5G:11111111-1111-1111-1111-111111111111:802-11-wireless:ap0:yes\nHome:22222222-2222-2222-2222-222222222222:802-11-wireless::yes\nBackup:33333333-3333-3333-3333-333333333333:802-11-wireless::yes\n",
		"nmcli -g 802-11-wireless.mode,802-11-wireless.ssid,802-11-wireless.band,802-11-wireless.channel,802-11-wireless-security.key-mgmt,ipv4.method,ipv4.addresses connection show uuid 11111111-1111-1111-1111-111111111111": "ap\nTeslaBox-5G\na\n161\nwpa-psk\nshared\n192.168.88.1/24\n",
		"nmcli -g 802-11-wireless.mode,802-11-wireless.ssid,802-11-wireless.band,802-11-wireless.channel,802-11-wireless-security.key-mgmt,ipv4.method,ipv4.addresses connection show uuid 22222222-2222-2222-2222-222222222222": "infrastructure\nHome\na\n0\nwpa-psk\nauto\n\n",
		"nmcli -g 802-11-wireless.mode,802-11-wireless.ssid,802-11-wireless.band,802-11-wireless.channel,802-11-wireless-security.key-mgmt,ipv4.method,ipv4.addresses connection show uuid 33333333-3333-3333-3333-333333333333": "infrastructure\nBackup\na\n0\nwpa-psk\nauto\n\n",
		"nmcli -t --escape yes -f IN-USE,SSID,BSSID,SIGNAL,CHAN,SECURITY device wifi list ifname wlan0 --rescan no":                                                                                                              ":Home:AA\\:BB\\:CC\\:DD\\:EE\\:01:80:165:WPA2\n:Backup:AA\\:BB\\:CC\\:DD\\:EE\\:02:90:161:WPA2\n",
		"nmcli -g connection.autoconnect-priority connection show uuid 22222222-2222-2222-2222-222222222222":                                                                                                                     "90\n",
		"nmcli -g connection.autoconnect-priority connection show uuid 33333333-3333-3333-3333-333333333333":                                                                                                                     "10\n",
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
	if !validBandChannel("a", 149) || validBandChannel("a", 6) {
		t.Fatal("validBandChannel() returned unexpected result")
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
