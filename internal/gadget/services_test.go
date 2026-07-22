package gadget

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
)

type serviceRunner struct {
	output     string
	failPrefix string
	calls      []string
}

func (r *serviceRunner) Run(_ context.Context, name string, args ...string) ([]byte, error) {
	call := strings.Join(append([]string{name}, args...), " ")
	r.calls = append(r.calls, call)
	if r.failPrefix != "" && strings.HasPrefix(call, r.failPrefix) {
		return nil, errors.New("command failed")
	}
	if len(args) > 0 && args[0] == "show" {
		return []byte(r.output), nil
	}
	return nil, nil
}

func TestInspectConflictingServicesParsesAllowlistedUnits(t *testing.T) {
	t.Parallel()
	runner := &serviceRunner{output: strings.Join([]string{
		"Id=radxa-adbd.service", "LoadState=loaded", "ActiveState=active", "SubState=exited", "UnitFileState=enabled",
		"", "Id=unrelated.service", "LoadState=loaded", "ActiveState=active",
	}, "\r\n")}
	states, err := InspectConflictingServices(t.Context(), runner)
	if err != nil {
		t.Fatal(err)
	}
	if len(states) != 2 || states[0].Name != "radxa-adbd.service" || states[0].ActiveState != "active" {
		t.Fatalf("states = %#v", states)
	}
	if states[1].Name != "radxa-usbnet.service" || states[1].LoadState != "unknown" {
		t.Fatalf("missing service = %#v", states[1])
	}
}

func TestDisableConflictingServicesOnlyChangesUnprotectedUnits(t *testing.T) {
	t.Parallel()
	runner := &serviceRunner{output: strings.Join([]string{
		"Id=radxa-adbd.service\nLoadState=loaded\nActiveState=active\nSubState=exited\nUnitFileState=enabled",
		"Id=radxa-usbnet.service\nLoadState=masked\nActiveState=inactive\nSubState=dead\nUnitFileState=masked",
	}, "\n\n")}
	protected, err := DisableConflictingServices(t.Context(), runner)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(protected, []string{"radxa-adbd.service"}) {
		t.Fatalf("protected = %v", protected)
	}
	wantCalls := []string{
		"systemctl show --property=Id --property=LoadState --property=ActiveState --property=SubState --property=UnitFileState radxa-adbd.service radxa-usbnet.service",
		"systemctl disable --now radxa-adbd.service",
		"systemctl mask radxa-adbd.service",
	}
	if !slices.Equal(runner.calls, wantCalls) {
		t.Fatalf("calls = %#v", runner.calls)
	}
}

func TestDisableConflictingServicesStopsAfterCommandFailure(t *testing.T) {
	t.Parallel()
	runner := &serviceRunner{
		output:     "Id=radxa-adbd.service\nLoadState=loaded\nActiveState=active\nUnitFileState=enabled\n",
		failPrefix: "systemctl disable --now radxa-adbd.service",
	}
	if _, err := DisableConflictingServices(t.Context(), runner); err == nil {
		t.Fatal("disable error = nil")
	}
	if len(runner.calls) != 2 {
		t.Fatalf("calls = %#v", runner.calls)
	}
}
