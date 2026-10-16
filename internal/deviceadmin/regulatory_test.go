package deviceadmin

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/wen-ryon/teslabox/internal/regulatory"
)

func TestWirelessRegionCannotChangeWhileCarPlayOwnsNetwork(t *testing.T) {
	t.Parallel()
	r := &fakeRunner{}
	m := New(r, nil, NetworkConfig{Regulatory: regulatory.New(r, regulatory.Paths{})})
	if err := m.RestoreCarPlayNetworkHold(testCarPlayLease()); err != nil {
		t.Fatal(err)
	}
	_, err := m.SetWirelessRegion(t.Context(), "CN")
	var operationError *APOperationError
	if !errors.As(err, &operationError) || operationError.Code != "carplay_network_reserved" || len(r.calls) != 0 {
		t.Fatalf("error=%v commands=%v", err, r.calls)
	}
}

func TestRegulatoryPreflightAppliesBeforeMeasuringChannels(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	countryFile := filepath.Join(root, "country")
	if err := os.WriteFile(countryFile, []byte("CN\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	r := &fakeRunner{responses: map[string]string{"iw reg get": "global\ncountry CN: DFS-FCC\n"}}
	region := regulatory.New(r, regulatory.Paths{CountryFile: countryFile, LockFile: filepath.Join(root, "lock")})
	m := New(r, nil, NetworkConfig{Regulatory: region})
	m.wirelessPHYInfo = func(context.Context, string) (string, error) {
		if len(r.calls) == 0 || r.calls[0] != "iw reg set CN" {
			t.Fatal("channel measured before applying region")
		}
		return "* 5805 MHz [161] (20.0 dBm) (no IR)", nil
	}
	if err := m.apChannelPreflight(t.Context(), "ap0", 161); err == nil {
		t.Fatal("country code must not override actual no IR")
	}
}

func TestAPStatusOnlyReadsRestrictionsWithoutChangingRegion(t *testing.T) {
	t.Parallel()
	r := &fakeRunner{}
	m := New(r, nil, NetworkConfig{Regulatory: regulatory.New(r, regulatory.Paths{})})
	m.wirelessPHYInfo = func(context.Context, string) (string, error) {
		return "* 5805 MHz [161] (20.0 dBm) (no IR)", nil
	}
	settings := APSettings{ConcurrencyMode: "independent_radio"}
	m.applyAPChannelRestriction(t.Context(), &settings, "", "ap0", 161)
	if len(r.calls) != 0 || settings.State != "blocked" {
		t.Fatalf("commands=%v settings=%+v", r.calls, settings)
	}
}

func TestSetWirelessRegionRejectsUnsafeInput(t *testing.T) {
	t.Parallel()
	r := &fakeRunner{responses: map[string]string{}}
	m := New(r, nil, NetworkConfig{Regulatory: regulatory.New(r, regulatory.Paths{})})
	if _, err := m.SetWirelessRegion(t.Context(), "CN; reboot"); err == nil {
		t.Fatal("invalid country accepted")
	}
	for _, call := range r.calls {
		if strings.HasPrefix(call, "iw reg set") {
			t.Fatal("invalid input reached command runner")
		}
	}
}
