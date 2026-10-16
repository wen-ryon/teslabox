package deviceadmin

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"strings"
	"testing"
)

func TestAPSecurityArguments(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name, mode, keyMgmt, pmf string
		invalid                  bool
	}{
		{name: "preserve"},
		{name: "WPA2", mode: "wpa2", keyMgmt: "wpa-psk", pmf: "1"},
		{name: "WPA3", mode: "wpa3", keyMgmt: "sae", pmf: "3"},
		{name: "transition unsupported", mode: "wpa2-wpa3", invalid: true},
		{name: "open rejected", mode: "open", invalid: true},
		{name: "invalid", mode: "wpa;reboot", invalid: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			args, err := apSecurityArguments(tc.mode)
			if (err != nil) != tc.invalid {
				t.Fatalf("error = %v", err)
			}
			if tc.invalid {
				return
			}
			if tc.mode == "" {
				if len(args) != 0 {
					t.Fatalf("omitted mode changed security: %v", args)
				}
				return
			}
			values := make(map[string]string)
			for i := 0; i < len(args); i += 2 {
				values[args[i]] = args[i+1]
			}
			for key, want := range map[string]string{
				"key-mgmt": tc.keyMgmt, "pmf": tc.pmf, "proto": "rsn",
				"pairwise": "ccmp", "group": "ccmp", "wps-method": "1", "auth-alg": "open",
			} {
				if got := values["802-11-wireless-security."+key]; got != want {
					t.Errorf("%s = %q, want %q", key, got, want)
				}
			}
			if _, ok := values["802-11-wireless-security.psk"]; ok {
				t.Fatal("security selection must not overwrite the password")
			}
		})
	}
}

func TestParseAPSecurityMode(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct{ name, text, want string }{
		{"WPA2 explicit", "wpa-psk\n1 (disable)\nrsn\nccmp\nccmp\n", "wpa2"},
		{"WPA3 explicit", "sae\n3 (required)\nrsn\nccmp\nccmp\n", "wpa3"},
		{"legacy default unknown", "wpa-psk\n0 (default)\n\n\n\n", ""},
		{"PSK optional not proof of mixed", "wpa-psk\n2\nrsn\nccmp\nccmp\n", ""},
		{"WPA1 allowed not WPA2 only", "wpa-psk\n1\nwpa,rsn\nccmp\nccmp\n", ""},
		{"empty", "", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := parseAPSecurityMode(tc.text); got != tc.want {
				t.Fatalf("mode = %q, want %q", got, tc.want)
			}
		})
	}
}

func apSecurityTestManager(t *testing.T) (*Manager, *fakeRunner) {
	t.Helper()
	const uuid = "11111111-1111-1111-1111-111111111111"
	runner := &fakeRunner{responses: map[string]string{
		"nmcli -t --escape yes -f NAME,UUID,TYPE,DEVICE,AUTOCONNECT connection show":                                                                                                                "TeslaBox-5G:" + uuid + ":802-11-wireless::no\n",
		"nmcli -g 802-11-wireless.mode,802-11-wireless.ssid,802-11-wireless.band,802-11-wireless.channel,802-11-wireless-security.key-mgmt,ipv4.method,ipv4.addresses connection show uuid " + uuid: "ap\nTeslaBox-5G\na\n161\nsae\nshared\n192.168.88.1/24\n",
		"nmcli -g connection.interface-name connection show uuid " + uuid:                                                                                                                           "ap0\n",
		"nmcli -t --escape yes -f DEVICE,TYPE,STATE device status":                                                                                                                                  "ap0:wifi:disconnected\n",
		"wpa_cli -i ap0 get_capability modes strict":                                                                                                                                                "AP STATION\n",
		"wpa_cli -i ap0 get_capability key_mgmt strict":                                                                                                                                             "WPA-PSK SAE\n",
		"wpa_cli -i ap0 get_capability pairwise strict":                                                                                                                                             "CCMP\n",
		"wpa_cli -i ap0 get_capability group_mgmt strict":                                                                                                                                           "AES-128-CMAC\n",
	}}
	manager := New(runner, slog.New(slog.NewTextHandler(io.Discard, nil)))
	configureAllowedAP(manager, runner, "ap0", "TeslaBox-5G", 161)
	return manager, runner
}

func TestAPSecurityCapabilityDoesNotInferAPFromSAE(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct{ name, key, value string }{
		{"no AP", "modes", "STATION"},
		{"no SAE", "key_mgmt", "WPA-PSK"},
		{"no PMF", "group_mgmt", ""},
		{"missing control socket", "modes", "Failed to connect to non-global ctrl_ifname: ap0 error: No such file or directory"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			manager, runner := apSecurityTestManager(t)
			runner.responses["wpa_cli -i ap0 get_capability "+tc.key+" strict"] = tc.value
			options := manager.apSecurityOptions(context.Background(), "ap0")
			if len(options) != 3 || !options[0].Available || options[1].Available || options[2].Available {
				t.Fatalf("options = %+v", options)
			}
			if options[2].Reason == "" {
				t.Fatal("unknown/unsupported capability must explain why WPA3 is unavailable")
			}
		})
	}
}

func TestAPSecurityRTL8852BEAllowlistKeepsOtherPreconditions(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name, driver, missing string
		available             bool
	}{
		{name: "vendor driver", driver: "rtl8852be", available: true},
		{name: "vendor module name", driver: "8852be", available: true},
		{name: "upstream driver", driver: "rtw89_8852be", available: true},
		{name: "unknown driver", driver: "other"},
		{name: "different chip", driver: "rtl8852bu"},
		{name: "no driver"},
		{name: "no AP mode", driver: "rtl8852be", missing: "modes"},
		{name: "no CCMP", driver: "rtl8852be", missing: "pairwise"},
		{name: "no PMF", driver: "rtl8852be", missing: "group_mgmt"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			manager, runner := apSecurityTestManager(t)
			runner.responses["wpa_cli -i ap0 get_capability key_mgmt strict"] = "WPA-PSK"
			runner.responses["readlink -f /sys/class/net/ap0/phy80211/device/driver"] = "/sys/bus/pci/drivers/" + tc.driver
			if tc.missing != "" {
				runner.responses["wpa_cli -i ap0 get_capability "+tc.missing+" strict"] = ""
			}
			option := manager.apSecurityOptions(context.Background(), "ap0")[2]
			if option.Available != tc.available {
				t.Fatalf("option = %+v", option)
			}
			if option.Available && !strings.Contains(option.Reason, "白名单") {
				t.Fatalf("compatibility allowance must be visible: %+v", option)
			}
		})
	}
}

func TestUpdateAPSecurity(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name, mode, password, want string
	}{
		{name: "old client preserves SAE password update", password: "new-password", want: "802-11-wireless-security.psk new-password"},
		{name: "WPA2 leaves password unchanged", mode: "wpa2", want: "802-11-wireless-security.key-mgmt wpa-psk"},
		{name: "WPA3 leaves password unchanged", mode: "wpa3", want: "802-11-wireless-security.key-mgmt sae"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			manager, runner := apSecurityTestManager(t)
			err := manager.UpdateAP(context.Background(), APUpdate{
				SSID: "TeslaBox-5G", Band: "a", Channel: 161, SecurityMode: tc.mode, Password: tc.password,
			})
			if err != nil {
				t.Fatal(err)
			}
			var modify string
			for _, call := range runner.calls {
				if strings.HasPrefix(call, "nmcli connection modify uuid ") {
					modify = call
				}
			}
			if !strings.Contains(modify, tc.want) {
				t.Fatalf("modify = %q", modify)
			}
			if tc.mode == "" && strings.Contains(modify, "key-mgmt") {
				t.Fatal("old client unexpectedly changed SAE to WPA2")
			}
			if tc.password == "" && strings.Contains(modify, ".psk ") {
				t.Fatal("blank password must not erase or fetch the saved secret")
			}
		})
	}
}

func TestUpdateAPRejectsUnavailableSecurityBeforeMutation(t *testing.T) {
	t.Parallel()
	manager, runner := apSecurityTestManager(t)
	runner.responses["wpa_cli -i ap0 get_capability key_mgmt strict"] = "WPA-PSK"
	err := manager.UpdateAP(context.Background(), APUpdate{
		SSID: "TeslaBox-5G", Band: "a", Channel: 161, SecurityMode: "wpa3",
	})
	var operationError *APOperationError
	if !errors.As(err, &operationError) || operationError.Code != "ap_security_unavailable" {
		t.Fatalf("error = %#v", err)
	}
	for _, call := range runner.calls {
		if strings.Contains(call, "connection modify") || strings.Contains(call, "connection up") {
			t.Fatalf("unsupported mode changed networking: %q", call)
		}
	}
}

func TestAPSecurityDoesNotReturnSecretsOrFakeEffectiveMode(t *testing.T) {
	t.Parallel()
	manager, runner := apSecurityTestManager(t)
	settings, err := manager.APSettings(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if settings.ActiveSecurity != "" || settings.SecurityMode != "" {
		t.Fatalf("unknown runtime/config must remain unknown: %+v", settings)
	}
	data, err := json.Marshal(settings)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), `"password":`) || strings.Contains(string(data), `"psk":`) {
		t.Fatalf("unexpected secret field in %s", data)
	}
	for _, call := range runner.calls {
		if strings.Contains(call, "--show-secrets") {
			t.Fatalf("AP security display read a secret: %q", call)
		}
	}
}

func TestUpdateAPSecurityActivationNeverFallsBack(t *testing.T) {
	t.Parallel()
	const uuid = "11111111-1111-1111-1111-111111111111"
	for _, tc := range []struct {
		name, status, wantCode string
		failure                bool
	}{
		{name: "success", status: "mode=AP\nwpa_state=COMPLETED\nkey_mgmt=SAE\n"},
		{name: "runtime unknown stays unknown"},
		{name: "start failure", failure: true, wantCode: "ap_security_activation_failed"},
		{name: "runtime mismatch", status: "mode=AP\nwpa_state=COMPLETED\nkey_mgmt=WPA2-PSK\n", wantCode: "ap_security_mismatch"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			manager, runner := apSecurityTestManager(t)
			runner.responses["wpa_cli -i ap0 status"] = tc.status
			if tc.failure {
				runner.failures = map[string][]error{
					"nmcli --wait 15 connection up uuid " + uuid + " ifname ap0": {errors.New("supplicant failed")},
				}
			}
			err := manager.UpdateAP(context.Background(), APUpdate{
				Enabled: true, SSID: "TeslaBox-5G", Band: "a", Channel: 161, SecurityMode: "wpa3",
			})
			if tc.wantCode == "" {
				if err != nil {
					t.Fatal(err)
				}
			} else {
				var operationError *APOperationError
				if !errors.As(err, &operationError) || operationError.Code != tc.wantCode {
					t.Fatalf("error = %#v, want %s", err, tc.wantCode)
				}
			}
			for _, call := range runner.calls {
				if strings.Contains(call, "key-mgmt wpa-psk") {
					t.Fatalf("WPA3 request was silently downgraded: %s", call)
				}
			}
		})
	}
}

func TestParseActiveAPSecurity(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct{ name, text, want string }{
		{"WPA2 AP", "mode=AP\nwpa_state=COMPLETED\nkey_mgmt=WPA2-PSK\n", "wpa2"},
		{"WPA3 AP", "mode=AP\nwpa_state=COMPLETED\nkey_mgmt=SAE\n", "wpa3"},
		{"STA is not AP", "mode=station\nwpa_state=COMPLETED\nkey_mgmt=SAE\n", ""},
		{"incomplete", "mode=AP\nwpa_state=DISCONNECTED\nkey_mgmt=SAE\n", ""},
		{"unknown mixed", "mode=AP\nwpa_state=COMPLETED\nkey_mgmt=WPA-PSK SAE\n", ""},
		{"no control socket", "FAIL\n", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := parseActiveAPSecurity(tc.text); got != tc.want {
				t.Fatalf("active security = %q, want %q", got, tc.want)
			}
		})
	}
}
