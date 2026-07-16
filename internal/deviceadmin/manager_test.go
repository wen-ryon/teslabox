package deviceadmin

import (
	"context"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"
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
	output := "*:Home\\:5G:80:149:WPA2\n:Home\\:5G:50:149:WPA2\n:Cafe:92:6:--\n:hidden:bad:1:WPA2\n"
	items := parseAccessPoints(output, map[string]bool{"Home:5G": true})
	if len(items) != 2 {
		t.Fatalf("items = %#v", items)
	}
	if items[0].SSID != "Cafe" || items[0].Band != "2.4 GHz" || items[0].Security != "OPEN" {
		t.Fatalf("first item = %#v", items[0])
	}
	if items[1].SSID != "Home:5G" || items[1].Signal != 80 || !items[1].Saved || !items[1].Active {
		t.Fatalf("second item = %#v", items[1])
	}
}

func TestWiFiStatusSeparatesAPAndSavedNetworks(t *testing.T) {
	t.Parallel()
	runner := &fakeRunner{responses: map[string]string{
		"nmcli -t --escape yes -f NAME,UUID,TYPE,DEVICE,AUTOCONNECT connection show": "TeslaBox-5G:11111111-1111-1111-1111-111111111111:802-11-wireless:wlan0:yes\nHome:22222222-2222-2222-2222-222222222222:802-11-wireless::no\n",
		"nmcli -g 802-11-wireless.mode,802-11-wireless.ssid,802-11-wireless.band,802-11-wireless.channel,802-11-wireless-security.key-mgmt,ipv4.method,ipv4.addresses connection show uuid 11111111-1111-1111-1111-111111111111": "ap\nTeslaBox-5G\na\n149\nwpa-psk\nshared\n192.168.88.1/24\n",
		"nmcli -g 802-11-wireless.mode,802-11-wireless.ssid,802-11-wireless.band,802-11-wireless.channel,802-11-wireless-security.key-mgmt,ipv4.method,ipv4.addresses connection show uuid 22222222-2222-2222-2222-222222222222": "infrastructure\nHome\na\n0\nwpa-psk\nauto\n\n",
		"nmcli -t --escape yes -f IN-USE,SSID,SIGNAL,CHAN,SECURITY device wifi list ifname wlan0 --rescan no":                                                                                                                    ":Home:70:149:WPA2\n",
	}}
	manager := New(runner, slog.New(slog.NewTextHandler(io.Discard, nil)))
	status, err := manager.WiFiStatus(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !status.APActive || len(status.SavedNetworks) != 1 || status.SavedNetworks[0].SSID != "Home" {
		t.Fatalf("status = %#v", status)
	}
	if len(status.AvailableNetworks) != 1 || !status.AvailableNetworks[0].Saved {
		t.Fatalf("available = %#v", status.AvailableNetworks)
	}
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
