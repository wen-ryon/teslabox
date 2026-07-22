package diagnostics

import (
	"context"
	"strings"
	"testing"
)

type fakeRunner struct {
	name   string
	args   []string
	output string
}

func (r *fakeRunner) Run(_ context.Context, name string, args ...string) ([]byte, error) {
	r.name = name
	r.args = append([]string(nil), args...)
	return []byte(r.output), nil
}

func TestManagerLogsFiltersUSBAndRedactsSecrets(t *testing.T) {
	t.Parallel()
	runner := &fakeRunner{output: strings.Join([]string{
		"kernel: thermal zone ready",
		"kernel: dwc3 fcc00000.dwc3: new device",
		"kernel: usb gadget password=owner-secret",
		`kernel: usb gadget {"token":"owner-token"}`,
	}, "\n")}
	manager := New(runner)
	report, err := manager.Logs(t.Context(), SourceUSB, 20)
	if err != nil {
		t.Fatal(err)
	}
	if runner.name != "journalctl" || !strings.Contains(strings.Join(runner.args, " "), "--dmesg") {
		t.Fatalf("command = %s %v", runner.name, runner.args)
	}
	if len(report.Lines) != 3 || strings.Contains(strings.Join(report.Lines, "\n"), "owner-secret") || strings.Contains(strings.Join(report.Lines, "\n"), "owner-token") {
		t.Fatalf("report = %#v", report)
	}
	if report.Redacted != 2 || !strings.Contains(report.Lines[1], "[已脱敏]") || !strings.Contains(report.Lines[2], "[已脱敏]") {
		t.Fatalf("redaction = %#v", report)
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
