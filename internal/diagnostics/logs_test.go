package diagnostics

import (
	"context"
	"strings"
	"testing"
)

type fakeRunner struct {
	name            string
	args            []string
	output          string
	systemctlOutput string
	calls           []string
}

func (r *fakeRunner) Run(_ context.Context, name string, args ...string) ([]byte, error) {
	r.name = name
	r.args = append([]string(nil), args...)
	r.calls = append(r.calls, strings.Join(append([]string{name}, args...), " "))
	if name == "systemctl" {
		return []byte(r.systemctlOutput), nil
	}
	return []byte(r.output), nil
}

func TestManagerLogsFiltersUSBAndRedactsSecrets(t *testing.T) {
	t.Parallel()
	runner := &fakeRunner{
		output: strings.Join([]string{
			"kernel: thermal zone ready",
			"kernel: dwc3 fcc00000.dwc3: new device",
			"kernel: usb gadget password=owner-secret",
			`kernel: usb gadget {"token":"owner-token"}`,
		}, "\n"),
		systemctlOutput: strings.Join([]string{
			"Id=radxa-adbd.service\nLoadState=masked\nActiveState=inactive\nSubState=dead\nUnitFileState=masked",
			"Id=radxa-usbnet.service\nLoadState=masked\nActiveState=inactive\nSubState=dead\nUnitFileState=masked",
		}, "\n\n"),
	}
	manager := New(runner)
	report, err := manager.Logs(t.Context(), SourceUSB, 20)
	if err != nil {
		t.Fatal(err)
	}
	if len(report.Lines) != 3 || strings.Contains(strings.Join(report.Lines, "\n"), "owner-secret") || strings.Contains(strings.Join(report.Lines, "\n"), "owner-token") {
		t.Fatalf("report = %#v", report)
	}
	if report.Redacted != 2 || !strings.Contains(report.Lines[1], "[已脱敏]") || !strings.Contains(report.Lines[2], "[已脱敏]") {
		t.Fatalf("redaction = %#v", report)
	}
	if len(runner.calls) != 2 || !strings.HasPrefix(runner.calls[0], "journalctl ") || !strings.HasPrefix(runner.calls[1], "systemctl show ") {
		t.Fatalf("calls = %#v", runner.calls)
	}
	if runner.name != "systemctl" || len(report.ServiceStates) != 2 || report.ServiceStates[0].UnitFileState != "masked" {
		t.Fatalf("service states = %#v, command = %s %v", report.ServiceStates, runner.name, runner.args)
	}
}

func TestManagerLogsUsesAllowlistedUnitsAndBoundsLimit(t *testing.T) {
	t.Parallel()
	runner := &fakeRunner{output: "one\ntwo\nthree\n"}
	manager := New(runner)
	report, err := manager.Logs(t.Context(), SourceApplication, 2)
	if err != nil {
		t.Fatal(err)
	}
	command := strings.Join(runner.args, " ")
	if !strings.Contains(command, "teslabox.service") || !strings.Contains(command, "teslabox-update.service") {
		t.Fatalf("args = %v", runner.args)
	}
	if !report.Limited || len(report.Lines) != 2 || report.Lines[0] != "two" {
		t.Fatalf("report = %#v", report)
	}
	if _, err := manager.Logs(t.Context(), "../../etc/passwd", 20); err == nil {
		t.Fatal("unsupported source error = nil")
	}
}
