package deviceadmin

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/wen-ryon/teslabox/internal/system"
)

type fakeRunner struct {
	responses         map[string]string
	responseSequences map[string][]string
	failures          map[string][]error
	calls             []string
	argumentCalls     [][]string
}

func (r *fakeRunner) Run(_ context.Context, name string, args ...string) ([]byte, error) {
	arguments := append([]string{name}, args...)
	r.argumentCalls = append(r.argumentCalls, append([]string(nil), arguments...))
	call := strings.Join(arguments, " ")
	r.calls = append(r.calls, call)
	if len(r.failures[call]) > 0 {
		err := r.failures[call][0]
		r.failures[call] = r.failures[call][1:]
		if err != nil {
			return nil, err
		}
	}
	if len(r.responseSequences[call]) > 0 {
		response := r.responseSequences[call][0]
		r.responseSequences[call] = r.responseSequences[call][1:]
		return []byte(response), nil
	}
	return []byte(r.responses[call]), nil
}

func configureAllowedAP(manager *Manager, runner *fakeRunner, interfaceName, ssid string, channel int) {
	manager.wirelessPHYInfo = func(context.Context, string) (string, error) {
		return "\t* 2412 MHz [1] (20.0 dBm)\n\t* 5220 MHz [44] (20.0 dBm)\n\t* 5745 MHz [149] (20.0 dBm)\n\t* 5805 MHz [161] (20.0 dBm)\n\t* 5825 MHz [165] (20.0 dBm)\n", nil
	}
	if runner.responses == nil {
		runner.responses = make(map[string]string)
	}
	runner.responses["/usr/sbin/iw dev "+interfaceName+" info"] = fmt.Sprintf("Interface %s\n\tssid %s\n\ttype AP\n\tchannel %d\n", interfaceName, ssid, channel)
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

func TestParseAPSTAChannelLimit(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name   string
		output string
		limit  int
		found  bool
	}{
		{
			name: "single channel",
			output: "valid interface combinations:\n" +
				"\t * #{ managed } <= 1, #{ AP } <= 1,\n" +
				"\t   total <= 2, #channels <= 1\n",
			limit: 1,
			found: true,
		},
		{
			name: "multi channel",
			output: "valid interface combinations:\n" +
				"\t * #{ managed } <= 1, #{ AP } <= 1,\n" +
				"\t   total <= 2, #channels <= 2\n",
			limit: 2,
			found: true,
		},
		{
			name: "ignore unrelated combinations",
			output: "valid interface combinations:\n" +
				"\t * #{ managed } <= 2, total <= 2, #channels <= 2\n" +
				"\t * #{ AP } <= 2, total <= 2, #channels <= 2\n",
		},
		{
			name: "reject single total interface",
			output: "valid interface combinations:\n" +
				"\t * #{ managed } <= 1, #{ AP } <= 1, total <= 1, #channels <= 1\n",
		},
		{
			name: "shared group supports two interfaces",
			output: "valid interface combinations:\n" +
				"\t * #{ managed, AP, P2P-client } <= 2, total <= 2, #channels <= 1\n",
			limit: 1,
			found: true,
		},
		{
			name: "shared group permits only one role at a time",
			output: "valid interface combinations:\n" +
				"\t * #{ managed, AP } <= 1, #{ P2P-device } <= 1, total <= 2, #channels <= 2\n",
		},
		{
			name: "zero AP quota cannot run hotspot",
			output: "valid interface combinations:\n" +
				"\t * #{ managed } <= 2, #{ AP } <= 0, total <= 2, #channels <= 2\n",
		},
		{
			name: "grouped AP variants with separate station",
			output: "valid interface combinations:\n" +
				"\t * #{ AP, P2P-GO } <= 1, #{ managed, P2P-client } <= 1, total <= 2, #channels <= 2\n",
			limit: 2,
			found: true,
		},
		{
			name: "unsupported combination does not inflate channel count",
			output: "valid interface combinations:\n" +
				"\t * #{ managed, AP } <= 1, total <= 3, #channels <= 3\n" +
				"\t * #{ managed } <= 1, #{ AP } <= 1, total <= 2, #channels <= 1\n",
			limit: 1,
			found: true,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			limit, found := parseAPSTAChannelLimit(test.output)
			if limit != test.limit || found != test.found {
				t.Fatalf("parseAPSTAChannelLimit() = (%d, %t), want (%d, %t)", limit, found, test.limit, test.found)
			}
		})
	}
}

func TestParseIWAPRuntimeRequiresBroadcastIdentity(t *testing.T) {
	t.Parallel()
	runtime := parseIWAPRuntime("Interface wlan1\n\tssid TeslaBox-5G\n\ttype AP\n\tchannel 44 (5220 MHz), width: 20 MHz\n")
	if runtime.Type != "AP" || runtime.SSID != "TeslaBox-5G" || runtime.Band != "a" || runtime.Channel != 44 {
		t.Fatalf("runtime = %#v", runtime)
	}
	incomplete := parseIWAPRuntime("Interface wlan1\n\ttype AP\n")
	if incomplete.Channel != 0 || incomplete.SSID != "" {
		t.Fatalf("incomplete runtime = %#v", incomplete)
	}
}

func TestParseAPChannelCapabilitiesRejectsRestrictedChannels(t *testing.T) {
	t.Parallel()
	capabilities := parseAPChannelCapabilities("\t* 2412 MHz [1] (20.0 dBm)\n\t* 5220 MHz [44] (20.0 dBm) (radar detection)\n\t* 5745 MHz [149] (disabled)\n\t* 5765 MHz [153] (20.0 dBm) (no IR)\n")
	tests := []struct {
		name    string
		channel int
		code    string
	}{
		{name: "allowed", channel: 1},
		{name: "radar", channel: 44, code: "channel_requires_dfs"},
		{name: "disabled", channel: 149, code: "channel_disabled"},
		{name: "no ir", channel: 153, code: "channel_no_ir"},
		{name: "unknown", channel: 161, code: "channel_unknown"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			err := apChannelOperationError(test.channel, capabilities[test.channel])
			if test.code == "" {
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				return
			}
			if err == nil || err.Code != test.code {
				t.Fatalf("error = %#v, want code %q", err, test.code)
			}
		})
	}
}

func TestParseAPChannelCapabilitiesIgnoresDuplicate6GHzChannelNumber(t *testing.T) {
	t.Parallel()
	capabilities := parseAPChannelCapabilities("\t* 5785 MHz [157] (30.0 dBm)\n\t* 6735 MHz [157] (disabled)\n")
	capability := capabilities[157]
	if !capability.Found || capability.Disabled || capability.NoIR || capability.Radar {
		t.Fatalf("5 GHz channel 157 capability = %#v", capability)
	}
}

func TestAPConcurrencyCapabilityModes(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name         string
		wifi         string
		ap           string
		wifiExists   bool
		apExists     bool
		samePHY      bool
		topologyOK   bool
		channelLimit int
		mode         string
		role         string
	}{
		{name: "unavailable", wifi: "wlan0", ap: "ap0", wifiExists: true, mode: "unavailable", role: "预留 AP 接口"},
		{name: "exclusive", wifi: "wlan0", ap: "wlan0", wifiExists: true, apExists: true, mode: "exclusive", role: "共享物理接口"},
		{name: "independent radio", wifi: "wlan0", ap: "wlan1", wifiExists: true, apExists: true, topologyOK: true, mode: "independent_radio", role: "独立 AP 网卡"},
		{name: "same channel", wifi: "wlan0", ap: "wlan1", wifiExists: true, apExists: true, samePHY: true, topologyOK: true, channelLimit: 1, mode: "same_channel", role: "AP 虚拟接口"},
		{name: "multi channel", wifi: "wlan0", ap: "wlan1", wifiExists: true, apExists: true, samePHY: true, topologyOK: true, channelLimit: 2, mode: "multi_channel", role: "AP 虚拟接口"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			manager := New(&fakeRunner{responses: map[string]string{}}, slog.New(slog.NewTextHandler(io.Discard, nil)))
			manager.sameWirelessPHY = func(_, _ string) (bool, bool) { return test.samePHY, test.topologyOK }
			manager.wirelessPHYInfo = func(context.Context, string) (string, error) {
				return fmt.Sprintf("valid interface combinations:\n\t * #{ managed } <= 1, #{ AP } <= 1, total <= 2, #channels <= %d\n", test.channelLimit), nil
			}
			capability := manager.apConcurrencyCapability(context.Background(), test.wifi, test.ap, test.wifiExists, test.apExists)
			if capability.Mode != test.mode || capability.InterfaceRole != test.role {
				t.Fatalf("capability = %#v, want mode %q role %q", capability, test.mode, test.role)
			}
		})
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

func TestWiFiStatusReportsActiveSSIDInsteadOfProfileName(t *testing.T) {
	t.Parallel()
	const upstreamUUID = "22222222-2222-2222-2222-222222222222"
	runner := &fakeRunner{responses: map[string]string{
		"nmcli -t --escape yes -f NAME,UUID,TYPE,DEVICE,AUTOCONNECT connection show": "TeslaBox-Upstream:" + upstreamUUID + ":802-11-wireless:wlan0:yes\n",
		"nmcli -g 802-11-wireless.mode,802-11-wireless.ssid,802-11-wireless.band,802-11-wireless.channel,802-11-wireless-security.key-mgmt,ipv4.method,ipv4.addresses connection show uuid " + upstreamUUID: "infrastructure\nExampleWiFi-5G\na\n0\nwpa-psk\nauto\n\n",
		"nmcli -t --escape yes -f DEVICE,TYPE,STATE device status":                                                  "wlan0:wifi:connected\n",
		"nmcli -t --escape yes -f IN-USE,SSID,BSSID,SIGNAL,CHAN,SECURITY device wifi list ifname wlan0 --rescan no": "*:ExampleWiFi-5G:AA\\:BB\\:CC\\:DD\\:EE\\:01:80:161:WPA2\n",
	}}
	manager := New(runner, slog.New(slog.NewTextHandler(io.Discard, nil)))
	runner.responses["nmcli --escape no -g GENERAL.STATE,GENERAL.CON-UUID device show wlan0"] = "100 (connected)\n" + upstreamUUID + "\n"

	status, err := manager.WiFiStatus(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if status.CurrentConnection != "ExampleWiFi-5G" {
		t.Fatalf("current connection = %q, want active SSID", status.CurrentConnection)
	}
	if len(status.SavedNetworks) != 1 || status.SavedNetworks[0].Name != "TeslaBox-Upstream" || !status.SavedNetworks[0].Active {
		t.Fatalf("saved networks = %#v", status.SavedNetworks)
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

func TestAPSettingsDoesNotReportNetworkManagerZombieAsRunning(t *testing.T) {
	t.Parallel()
	const apUUID = "11111111-1111-1111-1111-111111111111"
	runner := &fakeRunner{responses: map[string]string{
		"nmcli -t --escape yes -f NAME,UUID,TYPE,DEVICE,AUTOCONNECT connection show":                                                                                                                  "TeslaBox-5G:" + apUUID + ":802-11-wireless:wlan1:yes\n",
		"nmcli -g 802-11-wireless.mode,802-11-wireless.ssid,802-11-wireless.band,802-11-wireless.channel,802-11-wireless-security.key-mgmt,ipv4.method,ipv4.addresses connection show uuid " + apUUID: "ap\nTeslaBox-5G\na\n44\nwpa-psk\nshared\n192.168.88.1/24\n",
		"nmcli -g connection.interface-name connection show uuid " + apUUID:                                                                                                                           "wlan1\n",
		"nmcli -t --escape yes -f DEVICE,TYPE,STATE device status":                                                                                                                                    "wlan1:wifi:connected\n",
		"/usr/sbin/iw dev wlan1 info": "Interface wlan1\n\ttype AP\n\ttxpower 3.00 dBm\n",
	}}
	manager := New(runner, slog.New(slog.NewTextHandler(io.Discard, nil)))
	manager.wirelessPHYInfo = func(context.Context, string) (string, error) {
		return "\t* 5220 MHz [44] (20.0 dBm)\n", nil
	}
	settings, err := manager.APSettings(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if settings.Enabled || settings.State != "degraded" || settings.ReasonCode != "ap_not_broadcasting" {
		t.Fatalf("settings = %#v", settings)
	}
}

func TestAPSettingsExplainsRadarBlockedChannel(t *testing.T) {
	t.Parallel()
	const apUUID = "11111111-1111-1111-1111-111111111111"
	runner := &fakeRunner{responses: map[string]string{
		"nmcli -t --escape yes -f NAME,UUID,TYPE,DEVICE,AUTOCONNECT connection show":                                                                                                                  "TeslaBox-5G:" + apUUID + ":802-11-wireless::yes\n",
		"nmcli -g 802-11-wireless.mode,802-11-wireless.ssid,802-11-wireless.band,802-11-wireless.channel,802-11-wireless-security.key-mgmt,ipv4.method,ipv4.addresses connection show uuid " + apUUID: "ap\nTeslaBox-5G\na\n44\nwpa-psk\nshared\n192.168.88.1/24\n",
		"nmcli -g connection.interface-name connection show uuid " + apUUID:                                                                                                                           "wlan1\n",
		"nmcli -t --escape yes -f DEVICE,TYPE,STATE device status":                                                                                                                                    "wlan1:wifi:disconnected\nwlan0:wifi:connected\n",
		"/usr/sbin/iw dev wlan0 info": "Interface wlan0\n\ttype managed\n\tchannel 44 (5220 MHz)\n",
	}}
	manager := New(runner, slog.New(slog.NewTextHandler(io.Discard, nil)))
	manager.sameWirelessPHY = func(_, _ string) (bool, bool) { return true, true }
	manager.wirelessPHYInfo = func(context.Context, string) (string, error) {
		return "\t* 5220 MHz [44] (20.0 dBm) (radar detection)\n\t* 5745 MHz [149] (20.0 dBm)\nvalid interface combinations:\n\t * #{ managed } <= 1, #{ AP } <= 1, total <= 2, #channels <= 1\n", nil
	}
	settings, err := manager.APSettings(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if settings.State != "blocked" || settings.ReasonCode != "channel_requires_dfs" || !strings.Contains(settings.StatusMessage, "44") {
		t.Fatalf("settings = %#v", settings)
	}
	if len(settings.AllowedChannels) != 1 || settings.AllowedChannels[0] != 149 {
		t.Fatalf("allowed channels = %#v", settings.AllowedChannels)
	}
}

func TestUpdateAPRejectsRadarChannelBeforeNetworkManagerActivation(t *testing.T) {
	t.Parallel()
	const apUUID = "11111111-1111-1111-1111-111111111111"
	runner := &fakeRunner{responses: map[string]string{
		"nmcli -t --escape yes -f NAME,UUID,TYPE,DEVICE,AUTOCONNECT connection show":                                                                                                                  "TeslaBox-5G:" + apUUID + ":802-11-wireless::yes\n",
		"nmcli -g 802-11-wireless.mode,802-11-wireless.ssid,802-11-wireless.band,802-11-wireless.channel,802-11-wireless-security.key-mgmt,ipv4.method,ipv4.addresses connection show uuid " + apUUID: "ap\nTeslaBox-5G\na\n44\nwpa-psk\nshared\n192.168.88.1/24\n",
		"nmcli -g connection.interface-name connection show uuid " + apUUID:                                                                                                                           "wlan1\n",
		"nmcli -t --escape yes -f DEVICE,TYPE,STATE device status":                                                                                                                                    "wlan1:wifi:disconnected\nwlan0:wifi:connected\n",
		"/usr/sbin/iw dev wlan0 info": "Interface wlan0\n\tssid Upstream\n\ttype managed\n\tchannel 44 (5220 MHz)\n",
	}}
	manager := New(runner, slog.New(slog.NewTextHandler(io.Discard, nil)))
	manager.sameWirelessPHY = func(_, _ string) (bool, bool) { return true, true }
	manager.wirelessPHYInfo = func(context.Context, string) (string, error) {
		return "\t* 5220 MHz [44] (20.0 dBm) (radar detection)\nvalid interface combinations:\n\t * #{ managed } <= 1, #{ AP } <= 1, total <= 2, #channels <= 1\n", nil
	}

	err := manager.UpdateAP(context.Background(), APUpdate{
		Enabled: true, SSID: "TeslaBox-5G", Band: "a", Channel: 44, AutoConnect: true,
	})
	var operationError *APOperationError
	if !errors.As(err, &operationError) || operationError.Code != "channel_requires_dfs" {
		t.Fatalf("UpdateAP() error = %#v", err)
	}
	for _, call := range runner.calls {
		if strings.Contains(call, "connection modify uuid "+apUUID) || strings.Contains(call, "connection up uuid "+apUUID) {
			t.Fatalf("受限信道不应交给 NetworkManager 尝试激活: calls = %#v", runner.calls)
		}
	}
}

func TestRepairAPInterfaceDisablesConflictWithoutDeletingSavedWiFi(t *testing.T) {
	t.Parallel()
	const (
		apUUID       = "11111111-1111-1111-1111-111111111111"
		upstreamUUID = "22222222-2222-2222-2222-222222222222"
		conflictUUID = "33333333-3333-3333-3333-333333333333"
	)
	runner := &fakeRunner{responses: map[string]string{
		"nmcli -t --escape yes -f NAME,UUID,TYPE,DEVICE,AUTOCONNECT connection show": "TeslaBox-5G:" + apUUID + ":802-11-wireless::yes\n" +
			"TeslaBox-Upstream-Home:" + upstreamUUID + ":802-11-wireless::yes\n" +
			"Desktop-WiFi:" + conflictUUID + ":802-11-wireless:wlan1:yes\n",
		"nmcli -g 802-11-wireless.mode,802-11-wireless.ssid,802-11-wireless.band,802-11-wireless.channel,802-11-wireless-security.key-mgmt,ipv4.method,ipv4.addresses connection show uuid " + apUUID:       "ap\nTeslaBox-5G\na\n161\nwpa-psk\nshared\n192.168.88.1/24\n",
		"nmcli -g 802-11-wireless.mode,802-11-wireless.ssid,802-11-wireless.band,802-11-wireless.channel,802-11-wireless-security.key-mgmt,ipv4.method,ipv4.addresses connection show uuid " + upstreamUUID: "infrastructure\nHome\na\n0\nwpa-psk\nauto\n\n",
		"nmcli -g 802-11-wireless.mode,802-11-wireless.ssid,802-11-wireless.band,802-11-wireless.channel,802-11-wireless-security.key-mgmt,ipv4.method,ipv4.addresses connection show uuid " + conflictUUID: "infrastructure\nOther\na\n0\nwpa-psk\nauto\n\n",
		"nmcli -g connection.interface-name connection show uuid " + apUUID:       "wlan1\n",
		"nmcli -g connection.interface-name connection show uuid " + upstreamUUID: "wlan0\n",
		"nmcli -g connection.interface-name connection show uuid " + conflictUUID: "wlan1\n",
		"nmcli -t --escape yes -f DEVICE,TYPE,STATE device status":                "wlan1:wifi:connected\nwlan0:wifi:disconnected\n",
	}}
	manager := New(runner, slog.New(slog.NewTextHandler(io.Discard, nil)))
	var scheduled func()
	manager.schedule = func(_ time.Duration, operation func()) { scheduled = operation }

	settings, err := manager.APSettings(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if settings.State != "blocked" || len(settings.ConflictingProfiles) != 1 || settings.ConflictingProfiles[0].UUID != conflictUUID {
		t.Fatalf("settings = %#v", settings)
	}
	if err := manager.RepairAPInterface(context.Background()); err != nil {
		t.Fatal(err)
	}
	if scheduled == nil {
		t.Fatal("RepairAPInterface() did not schedule repair")
	}
	scheduled()
	wantCalls := []string{
		"nmcli device disconnect wlan1",
		"nmcli connection modify uuid " + conflictUUID + " connection.autoconnect no connection.interface-name ",
		"nmcli connection modify uuid " + apUUID + " connection.interface-name wlan1",
	}
	for _, want := range wantCalls {
		if !containsCall(runner.calls, want) {
			t.Fatalf("missing repair call %q in %#v", want, runner.calls)
		}
	}
	for _, call := range runner.calls {
		if strings.Contains(call, "connection delete") {
			t.Fatalf("修复 AP 接口不得删除已保存 WiFi: calls = %#v", runner.calls)
		}
	}
}

func TestAPSettingsReportsSingleChannelCapabilityAndActiveChannel(t *testing.T) {
	t.Parallel()
	const (
		apUUID       = "11111111-1111-1111-1111-111111111111"
		upstreamUUID = "22222222-2222-2222-2222-222222222222"
	)
	runner := &fakeRunner{responses: map[string]string{
		"nmcli -t --escape yes -f NAME,UUID,TYPE,DEVICE,AUTOCONNECT connection show":                                                                                                                        "TeslaBox-5G:" + apUUID + ":802-11-wireless:wlan1:yes\nTeslaBox-Upstream-Home:" + upstreamUUID + ":802-11-wireless:wlan0:yes\n",
		"nmcli -g 802-11-wireless.mode,802-11-wireless.ssid,802-11-wireless.band,802-11-wireless.channel,802-11-wireless-security.key-mgmt,ipv4.method,ipv4.addresses connection show uuid " + apUUID:       "ap\nTeslaBox-5G\na\n0\nwpa-psk\nshared\n192.168.88.1/24\n",
		"nmcli -g 802-11-wireless.mode,802-11-wireless.ssid,802-11-wireless.band,802-11-wireless.channel,802-11-wireless-security.key-mgmt,ipv4.method,ipv4.addresses connection show uuid " + upstreamUUID: "infrastructure\nHome\nbg\n0\nwpa-psk\nauto\n\n",
		"nmcli -t --escape yes -f DEVICE,TYPE,STATE device status":                                                                                                                                          "wlan1:wifi:connected\nwlan0:wifi:connected\n",
		"nmcli --escape no -g GENERAL.HWADDR,IP4.ADDRESS device show wlan1":                                                                                                                                 "02:12:55:2B:BC:4C\n192.168.88.1/24\n",
		"/usr/sbin/iw dev wlan1 info": "Interface wlan1\n\ttype AP\n\tchannel 1 (2412 MHz), width: 20 MHz\n",
	}}
	manager := New(runner, slog.New(slog.NewTextHandler(io.Discard, nil)))
	manager.sameWirelessPHY = func(_, _ string) (bool, bool) { return true, true }
	manager.wirelessPHYInfo = func(context.Context, string) (string, error) {
		return "valid interface combinations:\n\t * #{ managed } <= 1, #{ AP } <= 1, total <= 2, #channels <= 1\n", nil
	}

	settings, err := manager.APSettings(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if settings.ConcurrencyMode != "same_channel" || settings.MaxConcurrentChannels != 1 {
		t.Fatalf("capability = %#v", settings)
	}
	if settings.ActiveBand != "bg" || settings.ActiveChannel != 1 {
		t.Fatalf("active channel = %#v", settings)
	}
	if !strings.Contains(settings.CapabilityNotice, "同一信道") || settings.InterfaceRole != "AP 虚拟接口" {
		t.Fatalf("capability notice = %#v", settings)
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
	if settings.Available || settings.Enabled || settings.Interface != "" || settings.MACAddress != "" || settings.Address != "" || settings.Sharing {
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
	if settings.Enabled || settings.Interface != "hotspot7" || settings.MACAddress != "" || settings.Address != "" || settings.Sharing {
		t.Fatalf("settings = %#v", settings)
	}
	for _, call := range runner.calls {
		if call == "nmcli --escape no -g GENERAL.HWADDR,IP4.ADDRESS device show hotspot7" {
			t.Fatalf("inactive AP queried runtime details: %q", call)
		}
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
	runner.responses["/usr/sbin/iw dev ap0 info"] = "Interface ap0\n\tssid TeslaBox-5G\n\ttype AP\n\tchannel 161\n"
	var scheduled func()
	manager.schedule = func(delay time.Duration, operation func()) {
		if delay != apDisableDelay {
			t.Fatalf("delay = %s, want %s", delay, apDisableDelay)
		}
		scheduled = operation
	}

	err := manager.UpdateAP(context.Background(), APUpdate{
		Enabled: false, SSID: "TeslaBox-5G", Band: "a", Channel: 161, AutoConnect: true,
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
	modifyCall := "nmcli connection modify uuid " + apUUID +
		" 802-11-wireless.ssid TeslaBox-5G 802-11-wireless.band a" +
		" 802-11-wireless.channel 161 connection.autoconnect no connection.autoconnect-retries 1 ipv4.method shared"
	if !containsCall(runner.calls, modifyCall) {
		t.Fatalf("关闭热点必须同步关闭开机自动连接: calls = %v", runner.calls)
	}
	scheduled()
	if !containsCall(runner.calls, downCall) {
		t.Fatalf("scheduled AP close was not executed: calls = %v", runner.calls)
	}
}

func TestUpdateAPClosesActiveHotspotWhenAutoConnectAlreadyDisabled(t *testing.T) {
	t.Parallel()
	const apUUID = "11111111-1111-1111-1111-111111111111"
	runner := &fakeRunner{responses: map[string]string{
		"nmcli -t --escape yes -f NAME,UUID,TYPE,DEVICE,AUTOCONNECT connection show":                                                                                                                  "TeslaBox-5G:" + apUUID + ":802-11-wireless:ap0:no\n",
		"nmcli -g 802-11-wireless.mode,802-11-wireless.ssid,802-11-wireless.band,802-11-wireless.channel,802-11-wireless-security.key-mgmt,ipv4.method,ipv4.addresses connection show uuid " + apUUID: "ap\nTeslaBox-5G\na\n161\nwpa-psk\nshared\n192.168.88.1/24\n",
		"nmcli -g connection.interface-name connection show uuid " + apUUID:                                                                                                                           "ap0\n",
		"nmcli -t --escape yes -f DEVICE,TYPE,STATE device status":                                                                                                                                    "ap0:wifi:connected\n",
		"/usr/sbin/iw dev ap0 info": "Interface ap0\n\tssid TeslaBox-5G\n\ttype AP\n\tchannel 161\n",
	}}
	manager := New(runner, slog.New(slog.NewTextHandler(io.Discard, nil)))
	var scheduled func()
	manager.schedule = func(_ time.Duration, operation func()) { scheduled = operation }

	if err := manager.UpdateAP(context.Background(), APUpdate{
		Enabled: false, SSID: "TeslaBox-5G", Band: "a", Channel: 161,
	}); err != nil {
		t.Fatalf("UpdateAP() error = %v", err)
	}
	if scheduled == nil {
		t.Fatal("临时热点关闭操作没有被调度")
	}
	scheduled()
	if !containsCall(runner.calls, "nmcli --wait 15 connection down uuid "+apUUID) {
		t.Fatalf("临时热点未被关闭: calls = %v", runner.calls)
	}
}

func TestSetAPRuntimeClosesTemporaryHotspotSynchronously(t *testing.T) {
	t.Parallel()
	const apUUID = "11111111-1111-1111-1111-111111111111"
	runner := &fakeRunner{responses: map[string]string{
		"nmcli -t --escape yes -f NAME,UUID,TYPE,DEVICE,AUTOCONNECT connection show":                                                                                                                  "TeslaBox-5G:" + apUUID + ":802-11-wireless:ap0:no\n",
		"nmcli -g 802-11-wireless.mode,802-11-wireless.ssid,802-11-wireless.band,802-11-wireless.channel,802-11-wireless-security.key-mgmt,ipv4.method,ipv4.addresses connection show uuid " + apUUID: "ap\nTeslaBox-5G\na\n161\nwpa-psk\nshared\n192.168.88.1/24\n",
		"nmcli -g connection.interface-name connection show uuid " + apUUID:                                                                                                                           "ap0\n",
		"nmcli -t --escape yes -f DEVICE,TYPE,STATE device status":                                                                                                                                    "ap0:wifi:connected\n",
		"/usr/sbin/iw dev ap0 info": "Interface ap0\n\tssid TeslaBox-5G\n\ttype AP\n\tchannel 161\n",
	}}
	manager := New(runner, slog.New(slog.NewTextHandler(io.Discard, nil)))
	manager.schedule = func(_ time.Duration, _ func()) {
		t.Fatal("SetAPRuntime(false) 不应调度延迟关闭")
	}

	if err := manager.SetAPRuntime(context.Background(), false); err != nil {
		t.Fatalf("SetAPRuntime() error = %v", err)
	}
	if !containsCall(runner.calls, "nmcli --wait 15 connection down uuid "+apUUID) {
		t.Fatalf("临时热点未被同步关闭: calls = %v", runner.calls)
	}
	for _, call := range runner.calls {
		if strings.Contains(call, "connection.autoconnect") && strings.Contains(call, "connection modify") {
			t.Fatalf("运行态关闭不得修改开机策略: calls = %v", runner.calls)
		}
	}
}

func TestUpdateAPClearsChannelForAutomaticSelection(t *testing.T) {
	t.Parallel()
	const apUUID = "11111111-1111-1111-1111-111111111111"
	runner := &fakeRunner{responses: map[string]string{
		"nmcli -t --escape yes -f NAME,UUID,TYPE,DEVICE,AUTOCONNECT connection show":                                                                                                                  "TeslaBox-5G:" + apUUID + ":802-11-wireless:ap0:yes\n",
		"nmcli -g 802-11-wireless.mode,802-11-wireless.ssid,802-11-wireless.band,802-11-wireless.channel,802-11-wireless-security.key-mgmt,ipv4.method,ipv4.addresses connection show uuid " + apUUID: "ap\nTeslaBox-5G\na\n161\nwpa-psk\nshared\n192.168.88.1/24\n",
		"nmcli -g connection.interface-name connection show uuid " + apUUID:                                                                                                                           "ap0\n",
		"nmcli -t --escape yes -f DEVICE,TYPE,STATE device status":                                                                                                                                    "ap0:wifi:connected\n",
	}}
	manager := New(runner, slog.New(slog.NewTextHandler(io.Discard, nil)))
	configureAllowedAP(manager, runner, "ap0", "TeslaBox-5G", 161)

	err := manager.UpdateAP(context.Background(), APUpdate{
		Enabled: true, SSID: "TeslaBox-5G", Band: "a", Channel: 0, AutoConnect: true,
	})
	if err != nil {
		t.Fatalf("UpdateAP() error = %v", err)
	}
	modifyCall := "nmcli connection modify uuid " + apUUID +
		" 802-11-wireless.ssid TeslaBox-5G 802-11-wireless.band a" +
		" 802-11-wireless.channel 161 connection.autoconnect yes connection.autoconnect-retries 1 ipv4.method shared"
	if !containsCall(runner.calls, modifyCall) {
		t.Fatalf("自动信道必须选择当前监管域允许的信道: calls = %v", runner.calls)
	}
	for _, call := range runner.calls {
		if strings.Contains(call, "802-11-wireless.channel 0") {
			t.Fatalf("自动信道不能把 0 写入 NetworkManager: calls = %v", runner.calls)
		}
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
	configureAllowedAP(manager, runner, "ap0", "TeslaBox-5G", 161)
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
	runner.responses["nmcli --escape no -g GENERAL.STATE,GENERAL.CON-UUID device show wlan0"] = "100 (connected)\n22222222-2222-2222-2222-222222222222\n"
	status, err := manager.WiFiStatus(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !status.ConcurrentMode || !status.ChannelCoupled || status.CurrentConnection != "Home" {
		t.Fatalf("status = %#v", status)
	}
}

func TestDisconnectWiFiOnlyDisconnectsCurrentClient(t *testing.T) {
	const upstreamUUID = "22222222-2222-2222-2222-222222222222"
	for _, test := range []struct {
		name       string
		upstream   string
		wantAction bool
	}{
		{name: "已连接", upstream: "Home:" + upstreamUUID + ":802-11-wireless:wlan0:yes\n", wantAction: true},
		{name: "已经断开", upstream: "Home:" + upstreamUUID + ":802-11-wireless::yes\n"},
	} {
		t.Run(test.name, func(t *testing.T) {
			runner := &fakeRunner{responses: map[string]string{
				"nmcli -t --escape yes -f NAME,UUID,TYPE,DEVICE,AUTOCONNECT connection show": "TeslaBox-5G:11111111-1111-1111-1111-111111111111:802-11-wireless:ap0:yes\n" + test.upstream,
				"nmcli -g 802-11-wireless.mode,802-11-wireless.ssid,802-11-wireless.band,802-11-wireless.channel,802-11-wireless-security.key-mgmt,ipv4.method,ipv4.addresses connection show uuid 11111111-1111-1111-1111-111111111111": "ap\nTeslaBox-5G\na\n161\nwpa-psk\nshared\n192.168.88.1/24\n",
				"nmcli -g 802-11-wireless.mode,802-11-wireless.ssid,802-11-wireless.band,802-11-wireless.channel,802-11-wireless-security.key-mgmt,ipv4.method,ipv4.addresses connection show uuid " + upstreamUUID:                      "infrastructure\nHome\na\n0\nwpa-psk\nauto\n\n",
				"nmcli -t --escape yes -f DEVICE,TYPE,STATE device status": "ap0:wifi:connected\nwlan0:wifi:connected\n",
			}}
			manager := New(runner, slog.New(slog.NewTextHandler(io.Discard, nil)))
			if test.wantAction {
				runner.responses["nmcli --escape no -g GENERAL.STATE,GENERAL.CON-UUID device show wlan0"] = "100 (connected)\n" + upstreamUUID + "\n"
			} else {
				runner.responses["nmcli --escape no -g GENERAL.STATE,GENERAL.CON-UUID device show wlan0"] = "30 (disconnected)\n--\n"
			}
			if err := manager.DisconnectWiFi(context.Background()); err != nil {
				t.Fatal(err)
			}
			if got := containsCall(runner.calls, "nmcli device disconnect wlan0"); got != test.wantAction {
				t.Fatalf("STA 断开命令存在 = %v，预期 %v；调用 = %#v", got, test.wantAction, runner.calls)
			}
			if containsCall(runner.calls, "nmcli device disconnect ap0") || manager.manualWiFiDisconnect != test.wantAction {
				t.Fatalf("AP 或手动断开状态异常：调用 = %#v，标志 = %v", runner.calls, manager.manualWiFiDisconnect)
			}
		})
	}
}

func TestManualWiFiDisconnectSuppressesRecovery(t *testing.T) {
	runner := &fakeRunner{responses: map[string]string{
		"nmcli -t --escape yes -f NAME,UUID,TYPE,DEVICE,AUTOCONNECT connection show": "Home:22222222-2222-2222-2222-222222222222:802-11-wireless::yes\n",
		"nmcli -g 802-11-wireless.mode,802-11-wireless.ssid,802-11-wireless.band,802-11-wireless.channel,802-11-wireless-security.key-mgmt,ipv4.method,ipv4.addresses connection show uuid 22222222-2222-2222-2222-222222222222": "infrastructure\nHome\na\n0\nwpa-psk\nauto\n\n",
		"nmcli -t --escape yes -f DEVICE,TYPE,STATE device status": "wlan0:wifi:disconnected\n",
	}}
	manager := New(runner, slog.New(slog.NewTextHandler(io.Discard, nil)))
	manager.manualWiFiDisconnect = true
	recovered, ssid, err := manager.reconcileAutoConnect(context.Background())
	if err != nil || recovered || ssid != "" || !manager.manualWiFiDisconnect {
		t.Fatalf("恢复结果 = %v, %q, %v；手动断开 = %v", recovered, ssid, err, manager.manualWiFiDisconnect)
	}
	for _, call := range runner.calls {
		if strings.Contains(call, "connection modify") || strings.Contains(call, "connection up") || strings.Contains(call, "wifi rescan") {
			t.Fatalf("手动断开后不应自动重连：%#v", runner.calls)
		}
	}
}

func TestConnectUsesBSSIDOnlyForCurrentActivation(t *testing.T) {
	t.Parallel()
	const upstreamUUID = "22222222-2222-2222-2222-222222222222"
	runner := &fakeRunner{responses: map[string]string{
		"nmcli -t --escape yes -f NAME,UUID,TYPE,DEVICE,AUTOCONNECT connection show": "TeslaBox-5G:11111111-1111-1111-1111-111111111111:802-11-wireless:ap0:yes\nHome:22222222-2222-2222-2222-222222222222:802-11-wireless::no\n",
		"nmcli -g 802-11-wireless.mode,802-11-wireless.ssid,802-11-wireless.band,802-11-wireless.channel,802-11-wireless-security.key-mgmt,ipv4.method,ipv4.addresses connection show uuid 11111111-1111-1111-1111-111111111111": "ap\nTeslaBox-5G\na\n149\nwpa-psk\nshared\n192.168.88.1/24\n",
		"nmcli -g 802-11-wireless.mode,802-11-wireless.ssid,802-11-wireless.band,802-11-wireless.channel,802-11-wireless-security.key-mgmt,ipv4.method,ipv4.addresses connection show uuid " + upstreamUUID:                      "infrastructure\nHome\na\n0\nwpa-psk\nauto\n\n",
		"nmcli -t --escape yes -f DEVICE,TYPE,STATE device status":                                                  "ap0:wifi:connected\nwlan0:wifi:disconnected\n",
		"nmcli -t --escape yes -f IN-USE,SSID,BSSID,SIGNAL,CHAN,SECURITY device wifi list ifname wlan0 --rescan no": ":Home:AA\\:BB\\:CC\\:DD\\:EE\\:01:90:161:WPA2\n",
	}}
	manager := New(runner, slog.New(slog.NewTextHandler(io.Discard, nil)))
	manager.manualWiFiDisconnect = true
	manager.sameWirelessPHY = func(_, _ string) (bool, bool) { return true, true }
	manager.wirelessPHYInfo = func(context.Context, string) (string, error) {
		return "valid interface combinations:\n\t * #{ managed } <= 1, #{ AP } <= 1, total <= 2, #channels <= 1\n\t* 5745 MHz [149] (20.0 dBm)\n\t* 5805 MHz [161] (20.0 dBm)\n", nil
	}
	runner.responseSequences = map[string][]string{
		"/usr/sbin/iw dev ap0 info": {
			"Interface ap0\n\tssid TeslaBox-5G\n\ttype AP\n\tchannel 149\n",
			"Interface ap0\n\tssid TeslaBox-5G\n\ttype AP\n\tchannel 161\n",
			"Interface ap0\n\tssid TeslaBox-5G\n\ttype AP\n\tchannel 161\n",
		},
	}
	runner.responses["/usr/sbin/iw dev wlan0 info"] = "Interface wlan0\n\ttype managed\n\tchannel 161\n"
	request := ConnectRequest{UUID: upstreamUUID}
	if err := manager.Connect(context.Background(), request); err != nil {
		t.Fatal(err)
	}
	if manager.manualWiFiDisconnect {
		t.Fatal("显式连接成功后仍保留手动断开标志")
	}
	wantCalls := []string{
		"nmcli connection modify uuid 11111111-1111-1111-1111-111111111111 802-11-wireless.band a 802-11-wireless.channel 161",
		"nmcli --wait 20 connection up uuid 11111111-1111-1111-1111-111111111111 ifname ap0",
		"nmcli --wait 15 connection up uuid " + upstreamUUID + " ifname wlan0 ap AA:BB:CC:DD:EE:01",
	}
	for _, want := range wantCalls {
		if !containsCall(runner.calls, want) {
			t.Fatalf("missing call %q in %#v", want, runner.calls)
		}
	}
	if !containsArgumentCall(
		runner.argumentCalls,
		"nmcli", "connection", "modify", "uuid", upstreamUUID,
		"802-11-wireless.bssid", "",
	) {
		t.Fatalf("已保存网络的永久 BSSID 锁定未清除: calls = %#v", runner.argumentCalls)
	}
}

func TestConnectClearsBSSIDAfterCreatingManagedProfile(t *testing.T) {
	t.Parallel()
	runner := &fakeRunner{responses: map[string]string{
		"nmcli -t --escape yes -f NAME,UUID,TYPE,DEVICE,AUTOCONNECT connection show":                                "",
		"nmcli -t --escape yes -f DEVICE,TYPE,STATE device status":                                                  "wlan0:wifi:disconnected\n",
		"nmcli -t --escape yes -f IN-USE,SSID,BSSID,SIGNAL,CHAN,SECURITY device wifi list ifname wlan0 --rescan no": ":Home:AA\\:BB\\:CC\\:DD\\:EE\\:01:90:161:WPA2\n",
	}}
	manager := New(runner, slog.New(slog.NewTextHandler(io.Discard, nil)))

	err := manager.Connect(context.Background(), ConnectRequest{
		SSID: "Home", BSSID: "AA:BB:CC:DD:EE:01", Channel: 161,
		Security: "wpa", Password: "correct-password",
	})
	if err != nil {
		t.Fatal(err)
	}
	if !containsArgumentCall(
		runner.argumentCalls,
		"nmcli", "connection", "modify", "id", "TeslaBox-Upstream-Home",
		"connection.autoconnect", "yes",
		"802-11-wireless.bssid", "",
		"802-11-wireless-security.psk-flags", "0",
		"802-11-wireless-security.psk", "correct-password",
	) {
		t.Fatalf("新建网络仍保留了永久 BSSID 锁定: calls = %#v", runner.argumentCalls)
	}
}

func TestClearManagedProfileBSSIDLocksOnlyMigratesTeslaBoxProfiles(t *testing.T) {
	t.Parallel()
	const (
		lockedUUID   = "22222222-2222-2222-2222-222222222222"
		unlockedUUID = "33333333-3333-3333-3333-333333333333"
		userUUID     = "44444444-4444-4444-4444-444444444444"
	)
	runner := &fakeRunner{responses: map[string]string{
		"nmcli -t --escape yes -f NAME,UUID,TYPE connection show": "TeslaBox-Upstream-Home:" + lockedUUID + ":802-11-wireless\n" +
			"TeslaBox-Upstream-Mobile:" + unlockedUUID + ":802-11-wireless\n" +
			"User-Locked:" + userUUID + ":802-11-wireless\n",
		"nmcli -g 802-11-wireless.bssid connection show uuid " + lockedUUID:   "AA:BB:CC:DD:EE:01\n",
		"nmcli -g 802-11-wireless.bssid connection show uuid " + unlockedUUID: "\n",
	}}
	manager := New(runner, slog.New(slog.NewTextHandler(io.Discard, nil)))

	if err := manager.clearManagedProfileBSSIDLocks(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !containsArgumentCall(
		runner.argumentCalls,
		"nmcli", "connection", "modify", "uuid", lockedUUID,
		"802-11-wireless.bssid", "",
	) {
		t.Fatalf("旧版 TeslaBox 配置的 BSSID 锁定未迁移: calls = %#v", runner.argumentCalls)
	}
	for _, call := range runner.calls {
		if strings.Contains(call, userUUID) {
			t.Fatalf("用户自行管理的 BSSID 配置不应被修改: calls = %#v", runner.calls)
		}
	}
}

func TestClearManagedProfileBSSIDLocksContinuesAfterProfileFailure(t *testing.T) {
	t.Parallel()
	const (
		failedUUID = "22222222-2222-2222-2222-222222222222"
		lockedUUID = "33333333-3333-3333-3333-333333333333"
	)
	failedReadCall := "nmcli -g 802-11-wireless.bssid connection show uuid " + failedUUID
	runner := &fakeRunner{
		responses: map[string]string{
			"nmcli -t --escape yes -f NAME,UUID,TYPE connection show": "TeslaBox-Upstream-Home:" + failedUUID + ":802-11-wireless\n" +
				"TeslaBox-Upstream-Mobile:" + lockedUUID + ":802-11-wireless\n",
			"nmcli -g 802-11-wireless.bssid connection show uuid " + lockedUUID: "AA:BB:CC:DD:EE:02\n",
		},
		failures: map[string][]error{
			failedReadCall: {errors.New("读取配置失败")},
		},
	}
	manager := New(runner, slog.New(slog.NewTextHandler(io.Discard, nil)))

	err := manager.clearManagedProfileBSSIDLocks(context.Background())
	if err == nil || !strings.Contains(err.Error(), "TeslaBox-Upstream-Home") {
		t.Fatalf("迁移错误 = %v，应该保留失败配置名称", err)
	}
	if !containsArgumentCall(
		runner.argumentCalls,
		"nmcli", "connection", "modify", "uuid", lockedUUID,
		"802-11-wireless.bssid", "",
	) {
		t.Fatalf("单个配置失败不应阻断其他配置迁移: calls = %#v", runner.argumentCalls)
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
	want := "nmcli --wait 15 connection up uuid " + upstreamUUID + " ifname wlan0 ap AA:BB:CC:DD:EE:01"
	if !containsCall(runner.calls, want) {
		t.Fatalf("独立客户端网卡未连接上游: calls = %#v", runner.calls)
	}
	status, err := manager.WiFiStatus(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !status.ConcurrentMode || status.ChannelCoupled || !strings.Contains(status.SingleRadioNotice, "保持各自信道") {
		t.Fatalf("status = %#v", status)
	}
}

func TestSavedWiFiMissingSystemSecretCanBeRepaired(t *testing.T) {
	t.Parallel()
	const upstreamUUID = "22222222-2222-2222-2222-222222222222"
	runner := &fakeRunner{responses: map[string]string{
		"nmcli -t --escape yes -f NAME,UUID,TYPE,DEVICE,AUTOCONNECT connection show": "ExampleVisible-5G:" + upstreamUUID + ":802-11-wireless::yes\n",
		"nmcli -g 802-11-wireless.mode,802-11-wireless.ssid,802-11-wireless.band,802-11-wireless.channel,802-11-wireless-security.key-mgmt,ipv4.method,ipv4.addresses connection show uuid " + upstreamUUID: "infrastructure\nExampleVisible-5G\na\n0\nwpa-psk\nauto\n\n",
		"nmcli --show-secrets -g 802-11-wireless-security.psk-flags,802-11-wireless-security.psk connection show uuid " + upstreamUUID:                                                                      "1 (agent-owned)\n\n",
		"nmcli -t --escape yes -f DEVICE,TYPE,STATE device status":                                                  "wlan0:wifi:disconnected\n",
		"nmcli -t --escape yes -f IN-USE,SSID,BSSID,SIGNAL,CHAN,SECURITY device wifi list ifname wlan0 --rescan no": ":ExampleVisible-5G:AA\\:BB\\:CC\\:DD\\:EE\\:01:90:161:WPA2\n",
	}}
	manager := New(runner, slog.New(slog.NewTextHandler(io.Discard, nil)))

	status, err := manager.WiFiStatus(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(status.SavedNetworks) != 1 || !status.SavedNetworks[0].RequiresPassword {
		t.Fatalf("saved networks = %#v", status.SavedNetworks)
	}
	if err := manager.Connect(context.Background(), ConnectRequest{
		UUID: upstreamUUID, Password: "correct-password",
	}); err != nil {
		t.Fatal(err)
	}
	want := "nmcli connection modify uuid " + upstreamUUID +
		" 802-11-wireless-security.psk-flags 0 802-11-wireless-security.psk correct-password"
	if !containsCall(runner.calls, want) {
		t.Fatalf("missing password repair call %q in %#v", want, runner.calls)
	}
	if !containsCall(runner.calls, "nmcli --wait 15 connection up uuid "+upstreamUUID+" ifname wlan0 ap AA:BB:CC:DD:EE:01") {
		t.Fatalf("saved network was not activated: %#v", runner.calls)
	}
}

func TestReconcileAutoConnectDoesNotRetryCoupledProfileWithoutSystemSecret(t *testing.T) {
	t.Parallel()
	const (
		apUUID       = "11111111-1111-1111-1111-111111111111"
		upstreamUUID = "22222222-2222-2222-2222-222222222222"
	)
	runner := &fakeRunner{responses: map[string]string{
		"nmcli -t --escape yes -f NAME,UUID,TYPE,DEVICE,AUTOCONNECT connection show":                                                                                                                        "TeslaBox-5G:" + apUUID + ":802-11-wireless:ap0:yes\nExampleVisible-5G:" + upstreamUUID + ":802-11-wireless::yes\n",
		"nmcli -g 802-11-wireless.mode,802-11-wireless.ssid,802-11-wireless.band,802-11-wireless.channel,802-11-wireless-security.key-mgmt,ipv4.method,ipv4.addresses connection show uuid " + apUUID:       "ap\nTeslaBox-5G\na\n161\nwpa-psk\nshared\n192.168.88.1/24\n",
		"nmcli -g 802-11-wireless.mode,802-11-wireless.ssid,802-11-wireless.band,802-11-wireless.channel,802-11-wireless-security.key-mgmt,ipv4.method,ipv4.addresses connection show uuid " + upstreamUUID: "infrastructure\nExampleVisible-5G\na\n0\nwpa-psk\nauto\n\n",
		"nmcli --show-secrets -g 802-11-wireless-security.psk-flags,802-11-wireless-security.psk connection show uuid " + upstreamUUID:                                                                      "2 (not-saved)\n\n",
		"nmcli -t --escape yes -f DEVICE,TYPE,STATE device status":                                                                                                                                          "ap0:wifi:connected\nwlan0:wifi:disconnected\n",
		"nmcli -t --escape yes -f IN-USE,SSID,BSSID,SIGNAL,CHAN,SECURITY device wifi list ifname wlan0 --rescan no":                                                                                         ":ExampleVisible-5G:AA\\:BB\\:CC\\:DD\\:EE\\:01:90:161:WPA2\n",
		"nmcli -g GENERAL.STATE device show wlan0":                                      "30 (disconnected)\n",
		"nmcli -g connection.autoconnect-priority connection show uuid " + upstreamUUID: "10\n",
	}}
	manager := New(runner, slog.New(slog.NewTextHandler(io.Discard, nil)))

	recovered, ssid, err := manager.reconcileAutoConnect(context.Background())
	if err == nil || !strings.Contains(err.Error(), errWiFiPasswordRequired.Error()) {
		t.Fatalf("reconcileAutoConnect() error = %v", err)
	}
	if recovered || ssid != "ExampleVisible-5G" {
		t.Fatalf("result = (%t, %q), want blocked ExampleVisible-5G", recovered, ssid)
	}
	for _, call := range runner.calls {
		if strings.Contains(call, "connection up uuid "+upstreamUUID) {
			t.Fatalf("missing-secret profile must not be activated repeatedly: %#v", runner.calls)
		}
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
			"nmcli -t --escape yes -f NAME,UUID,TYPE,DEVICE,AUTOCONNECT connection show":                                                                                                                        "TeslaBox-5G:" + apUUID + ":802-11-wireless:ap0:yes\nTeslaBox-Upstream-Home:" + upstreamUUID + ":802-11-wireless:wlan0:yes\n",
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
	manager.wirelessPHYInfo = func(context.Context, string) (string, error) {
		return "\t* 5745 MHz [149] (20.0 dBm)\n\t* 5805 MHz [161] (20.0 dBm)\n", nil
	}
	runner.responses["/usr/sbin/iw dev ap0 info"] = "Interface ap0\n\tssid TeslaBox-5G\n\ttype AP\n\tchannel 149\n"
	status := WiFiStatus{
		Interface:             "wlan0",
		CurrentConnection:     "Home",
		currentConnectionUUID: upstreamUUID,
		SavedNetworks: []SavedNetwork{{
			Name: "TeslaBox-Upstream-Home", UUID: upstreamUUID, SSID: "Home", Active: true,
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

func TestReconcileAutoConnectPreparesCoupledAPForNetworkManager(t *testing.T) {
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
	manager.wirelessPHYInfo = func(context.Context, string) (string, error) {
		return "\t* 5805 MHz [161] (20.0 dBm)\n\t* 5825 MHz [165] (20.0 dBm)\n", nil
	}
	runner.responseSequences = map[string][]string{
		"/usr/sbin/iw dev ap0 info": {
			"Interface ap0\n\tssid TeslaBox-5G\n\ttype AP\n\tchannel 161\n",
			"Interface ap0\n\tssid TeslaBox-5G\n\ttype AP\n\tchannel 165\n",
		},
	}
	recovered, ssid, err := manager.reconcileAutoConnect(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if recovered || ssid != "" {
		t.Fatalf("recovered = %t, ssid = %q, want NetworkManager to activate", recovered, ssid)
	}
	wantCalls := []string{
		"nmcli --wait 12 device wifi rescan ifname wlan0",
		"nmcli connection modify uuid 11111111-1111-1111-1111-111111111111 802-11-wireless.band a 802-11-wireless.channel 165",
		"nmcli --wait 20 connection up uuid 11111111-1111-1111-1111-111111111111 ifname ap0",
		"nmcli connection modify uuid 22222222-2222-2222-2222-222222222222 connection.autoconnect yes",
	}
	for _, want := range wantCalls {
		if !containsCall(runner.calls, want) {
			t.Fatalf("missing call %q in %#v", want, runner.calls)
		}
	}
	for _, call := range runner.calls {
		if strings.Contains(call, "connection up uuid 22222222-2222-2222-2222-222222222222") ||
			strings.Contains(call, "802-11-wireless.bssid") {
			t.Fatalf("coordination must leave upstream activation and BSSID selection to NetworkManager: %#v", runner.calls)
		}
	}
}

func TestReconcileAutoConnectLeavesOrdinaryReconnectToNetworkManager(t *testing.T) {
	t.Parallel()
	const (
		invisibleUUID = "22222222-2222-2222-2222-222222222222"
		visibleUUID   = "33333333-3333-3333-3333-333333333333"
	)
	runner := &fakeRunner{responses: map[string]string{
		"nmcli -t --escape yes -f NAME,UUID,TYPE,DEVICE,AUTOCONNECT connection show": "ExampleWiFi-5G:" + invisibleUUID + ":802-11-wireless::yes\nExampleVisible-5G:" + visibleUUID + ":802-11-wireless::yes\n",
		"nmcli -g 802-11-wireless.mode,802-11-wireless.ssid,802-11-wireless.band,802-11-wireless.channel,802-11-wireless-security.key-mgmt,ipv4.method,ipv4.addresses connection show uuid " + invisibleUUID: "infrastructure\nExampleWiFi-5G\na\n0\nwpa-psk\nauto\n\n",
		"nmcli -g 802-11-wireless.mode,802-11-wireless.ssid,802-11-wireless.band,802-11-wireless.channel,802-11-wireless-security.key-mgmt,ipv4.method,ipv4.addresses connection show uuid " + visibleUUID:   "infrastructure\nExampleVisible-5G\na\n0\nwpa-psk\nauto\n\n",
		"nmcli -t --escape yes -f DEVICE,TYPE,STATE device status":                                                  "wlan0:wifi:disconnected\n",
		"nmcli -t --escape yes -f IN-USE,SSID,BSSID,SIGNAL,CHAN,SECURITY device wifi list ifname wlan0 --rescan no": ":ExampleVisible-5G:AA\\:BB\\:CC\\:DD\\:EE\\:01:80:161:WPA2\n",
		"nmcli -g GENERAL.STATE device show wlan0":                                                                  "30 (disconnected)\n",
	}}
	manager := New(runner, slog.New(slog.NewTextHandler(io.Discard, nil)))

	recovered, ssid, err := manager.reconcileAutoConnect(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if recovered || ssid != "" {
		t.Fatalf("recovery = (%t, %q), want NetworkManager-owned reconnect", recovered, ssid)
	}
	for _, call := range runner.calls {
		if strings.Contains(call, "device wifi rescan") ||
			strings.Contains(call, "connection up") ||
			strings.Contains(call, "connection.autoconnect-priority") {
			t.Fatalf("ordinary reconnect issued TeslaBox recovery command %q", call)
		}
	}
}

func TestReconcileAutoConnectWaitsForNetworkManagerActivation(t *testing.T) {
	t.Parallel()
	const upstreamUUID = "22222222-2222-2222-2222-222222222222"
	runner := &fakeRunner{responses: map[string]string{
		"nmcli -t --escape yes -f NAME,UUID,TYPE,DEVICE,AUTOCONNECT connection show": "ExampleVisible-5G:" + upstreamUUID + ":802-11-wireless::yes\n",
		"nmcli -g 802-11-wireless.mode,802-11-wireless.ssid,802-11-wireless.band,802-11-wireless.channel,802-11-wireless-security.key-mgmt,ipv4.method,ipv4.addresses connection show uuid " + upstreamUUID: "infrastructure\nExampleVisible-5G\na\n0\nwpa-psk\nauto\n\n",
		"nmcli -t --escape yes -f DEVICE,TYPE,STATE device status":                                                  "wlan0:wifi:connecting\n",
		"nmcli -t --escape yes -f IN-USE,SSID,BSSID,SIGNAL,CHAN,SECURITY device wifi list ifname wlan0 --rescan no": ":ExampleVisible-5G:AA\\:BB\\:CC\\:DD\\:EE\\:01:80:161:WPA2\n",
		"nmcli -g GENERAL.STATE device show wlan0":                                                                  "70 (getting IP configuration)\n",
	}}
	manager := New(runner, slog.New(slog.NewTextHandler(io.Discard, nil)))

	recovered, ssid, err := manager.reconcileAutoConnect(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if recovered || ssid != "" {
		t.Fatalf("activation in progress = (%t, %q), want no duplicate recovery", recovered, ssid)
	}
	for _, call := range runner.calls {
		if strings.Contains(call, "connection up") || strings.Contains(call, "device wifi rescan") {
			t.Fatalf("activation in progress issued disruptive command %q", call)
		}
	}
}

func TestActivationStateAfterWaitTimeoutTreatsNetworkManagerActivationAsPending(t *testing.T) {
	t.Parallel()
	runner := &fakeRunner{responses: map[string]string{
		"nmcli -g GENERAL.STATE device show wlan0": "70 (getting IP configuration)\n",
	}}
	manager := New(runner, slog.New(slog.NewTextHandler(io.Discard, nil)))

	activated, pending := manager.activationStateAfterWaitTimeout(
		context.Background(),
		"wlan0",
		&system.CommandError{Code: 3, Output: "Error: Timeout expired (15 seconds)"},
	)
	if activated || !pending {
		t.Fatalf("activation state = (%t, %t), want pending", activated, pending)
	}
}

func TestReconcileForNetworkChangeRespectsRecoveryBackoff(t *testing.T) {
	t.Parallel()
	runner := &fakeRunner{responses: map[string]string{}}
	manager := New(runner, slog.New(slog.NewTextHandler(io.Discard, nil)))
	nextAttempt := time.Now().Add(time.Minute)
	manager.recovery = RecoveryStatus{
		Enabled:       true,
		Failures:      2,
		NextAttemptAt: &nextAttempt,
	}
	retryTimer := stoppedTimer()
	defer retryTimer.Stop()

	manager.reconcileForNetworkChange(context.Background(), retryTimer)

	if len(runner.calls) != 0 {
		t.Fatalf("backoff issued commands = %#v, want none", runner.calls)
	}
	status := manager.recoveryStatus()
	if status.Failures != 2 || status.NextAttemptAt != &nextAttempt {
		t.Fatalf("recovery status = %#v, want unchanged backoff", status)
	}
}

func TestReconcileAutoConnectFollowsActiveUpstreamChannel(t *testing.T) {
	t.Parallel()
	runner := &fakeRunner{responses: map[string]string{
		"nmcli -t --escape yes -f NAME,UUID,TYPE,DEVICE,AUTOCONNECT connection show": "TeslaBox-Upstream-Home:22222222-2222-2222-2222-222222222222:802-11-wireless:wlan0:yes\nTeslaBox-5G:11111111-1111-1111-1111-111111111111:802-11-wireless:ap0:yes\n",
		"nmcli -g 802-11-wireless.mode,802-11-wireless.ssid,802-11-wireless.band,802-11-wireless.channel,802-11-wireless-security.key-mgmt,ipv4.method,ipv4.addresses connection show uuid 11111111-1111-1111-1111-111111111111": "ap\nTeslaBox-5G\na\n149\nwpa-psk\nshared\n192.168.88.1/24\n",
		"nmcli -g 802-11-wireless.mode,802-11-wireless.ssid,802-11-wireless.band,802-11-wireless.channel,802-11-wireless-security.key-mgmt,ipv4.method,ipv4.addresses connection show uuid 22222222-2222-2222-2222-222222222222": "infrastructure\nHome\na\n0\nwpa-psk\nauto\n\n",
		"nmcli -t --escape yes -f IN-USE,SSID,BSSID,SIGNAL,CHAN,SECURITY device wifi list ifname wlan0 --rescan no":                                                                                                              "*:Home:AA\\:BB\\:CC\\:DD\\:EE\\:09:78:161:WPA2\n",
	}}
	manager := New(runner, slog.New(slog.NewTextHandler(io.Discard, nil)))
	runner.responses["nmcli --escape no -g GENERAL.STATE,GENERAL.CON-UUID device show wlan0"] = "100 (connected)\n22222222-2222-2222-2222-222222222222\n"
	manager.sameWirelessPHY = func(_, _ string) (bool, bool) { return true, true }
	manager.wirelessPHYInfo = func(context.Context, string) (string, error) {
		return "valid interface combinations:\n\t * #{ managed } <= 1, #{ AP } <= 1, total <= 2, #channels <= 1\n\t* 5745 MHz [149] (20.0 dBm)\n\t* 5805 MHz [161] (20.0 dBm)\n", nil
	}
	runner.responseSequences = map[string][]string{
		"/usr/sbin/iw dev ap0 info": {
			"Interface ap0\n\tssid TeslaBox-5G\n\ttype AP\n\tchannel 149\n",
			"Interface ap0\n\tssid TeslaBox-5G\n\ttype AP\n\tchannel 149\n",
			"Interface ap0\n\tssid TeslaBox-5G\n\ttype AP\n\tchannel 161\n",
		},
	}
	recovered, ssid, err := manager.reconcileAutoConnect(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !recovered || ssid != "Home" {
		t.Fatalf("recovered = %t, ssid = %q", recovered, ssid)
	}
	wantCalls := []string{
		"nmcli connection modify uuid 11111111-1111-1111-1111-111111111111 802-11-wireless.band a 802-11-wireless.channel 161 connection.autoconnect-retries 1",
		"nmcli --wait 20 connection up uuid 11111111-1111-1111-1111-111111111111 ifname ap0",
	}
	for _, want := range wantCalls {
		if !containsCall(runner.calls, want) {
			t.Fatalf("missing call %q in %#v", want, runner.calls)
		}
	}
}

func TestReconcileAutoConnectIfNeededProtectsRolesWithoutReactivatingHealthyLink(t *testing.T) {
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
	if !containsCall(runner.calls, "nmcli -g GENERAL.STATE device show wlan0") {
		t.Fatalf("健康连接没有检查实际状态：%v", runner.calls)
	}
	for _, call := range runner.calls {
		if strings.Contains(call, "wifi list") || strings.Contains(call, "wifi rescan") || strings.Contains(call, "connection up") {
			t.Fatalf("健康STA不应被后备检查扫描或重新激活：%s", call)
		}
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

func TestRefreshMonitoredInterfaceReplacesRemovedDevice(t *testing.T) {
	runner := &fakeRunner{responses: map[string]string{
		"nmcli -t --escape yes -f NAME,UUID,TYPE,DEVICE,AUTOCONNECT connection show":                                "",
		"nmcli -t --escape yes -f DEVICE,TYPE,STATE device status":                                                  "wlan1:wifi:disconnected\n",
		"nmcli --escape no -g GENERAL.HWADDR,IP4.ADDRESS device show wlan1":                                         "",
		"nmcli -t --escape yes -f IN-USE,SSID,BSSID,SIGNAL,CHAN,SECURITY device wifi list ifname wlan1 --rescan no": "",
	}}
	manager := New(runner, slog.New(slog.NewTextHandler(io.Discard, nil)))
	manager.monitoredWiFiInterface = "wlan0"

	if err := manager.refreshMonitoredInterface(context.Background()); err != nil {
		t.Fatal(err)
	}
	if manager.monitoredWiFiInterface != "wlan1" {
		t.Fatalf("monitored WiFi interface = %q, want wlan1", manager.monitoredWiFiInterface)
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

func containsArgumentCall(calls [][]string, expected ...string) bool {
	for _, call := range calls {
		if len(call) != len(expected) {
			continue
		}
		matches := true
		for index := range call {
			if call[index] != expected[index] {
				matches = false
				break
			}
		}
		if matches {
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
