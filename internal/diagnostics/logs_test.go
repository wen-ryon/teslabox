package diagnostics

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"
)

type fakeRunner struct {
	name            string
	args            []string
	output          string
	systemctlOutput string
	calls           []string
}

type blockingBundleRunner struct {
	once    sync.Once
	started chan struct{}
	release chan struct{}
}

func (r *blockingBundleRunner) Run(ctx context.Context, _ string, _ ...string) ([]byte, error) {
	blocked := false
	r.once.Do(func() {
		blocked = true
		close(r.started)
	})
	if blocked {
		select {
		case <-r.release:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	return nil, nil
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
	if len(runner.calls) != 3 || !strings.HasPrefix(runner.calls[0], "journalctl ") ||
		!strings.HasPrefix(runner.calls[1], "journalctl ") || !strings.HasPrefix(runner.calls[2], "systemctl show ") {
		t.Fatalf("calls = %#v", runner.calls)
	}
	if runner.name != "systemctl" || len(report.ServiceStates) != 2 || report.ServiceStates[0].UnitFileState != "masked" {
		t.Fatalf("service states = %#v, command = %s %v", report.ServiceStates, runner.name, runner.args)
	}
}

func TestRedactLinePreservesNetworkManagerMissingPasswordExplanation(t *testing.T) {
	t.Parallel()
	line := "Warning: password for '802-11-wireless-security.psk' not given in 'passwd-file'"
	if redacted := redactLine(line); redacted != line {
		t.Fatalf("redactLine() = %q, want original diagnostic wording", redacted)
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

func TestManagerLogsCombinesMediaServiceAndKernelFailures(t *testing.T) {
	t.Parallel()
	runner := &fakeRunner{output: strings.Join([]string{
		"teslabox: 实时合成录像失败 error=执行 ffmpeg",
		"kernel: rga allocation failed",
		"teslabox: ordinary request",
	}, "\n")}
	report, err := New(runner).Logs(t.Context(), SourceMedia, 20)
	if err != nil {
		t.Fatal(err)
	}
	if len(runner.calls) != 2 || len(report.Lines) != 2 {
		t.Fatalf("calls = %#v, report = %#v", runner.calls, report)
	}
	if !strings.Contains(strings.Join(report.Lines, "\n"), "ffmpeg") ||
		!strings.Contains(strings.Join(report.Lines, "\n"), "rga") {
		t.Fatalf("report = %#v", report)
	}
}

func TestManagerBundleIncludesStateLogsAndRedaction(t *testing.T) {
	t.Parallel()
	runner := &fakeRunner{
		output: strings.Join([]string{
			"teslabox: 实时合成录像失败 token=owner-token",
			"kernel: dwc3 fcc00000.dwc3: device reset",
		}, "\n"),
		systemctlOutput: "ActiveState=active\nNRestarts=1\n",
	}
	bundle, err := New(runner).Bundle(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	content := string(bundle.Content)
	for _, expected := range []string{"TeslaBox 诊断报告", "TeslaBox 服务状态", "当前启动日志: media", "上一次启动关键故障日志", "[已脱敏]"} {
		if !strings.Contains(content, expected) {
			t.Fatalf("bundle missing %q:\n%s", expected, content)
		}
	}
	if strings.Contains(content, "owner-token") || !strings.HasSuffix(bundle.Filename, ".txt") {
		t.Fatalf("bundle filename = %q, content = %s", bundle.Filename, content)
	}
}

func TestManagerBundleSizeLimitPreservesUTF8(t *testing.T) {
	t.Parallel()
	line := strings.Repeat("中", 1400)
	runner := &fakeRunner{output: strings.Repeat(line+"\n", 500)}

	bundle, err := New(runner).Bundle(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if !utf8.Valid(bundle.Content) {
		t.Fatal("Bundle() returned invalid UTF-8 after applying the size limit")
	}
	if !strings.Contains(string(bundle.Content), "[报告已达到大小上限") {
		t.Fatalf("bundle length = %d, want truncated report", len(bundle.Content))
	}
}

func TestManagerBundleRejectsConcurrentGeneration(t *testing.T) {
	t.Parallel()
	runner := &blockingBundleRunner{started: make(chan struct{}), release: make(chan struct{})}
	manager := New(runner)
	firstDone := make(chan error, 1)
	go func() {
		_, err := manager.Bundle(t.Context())
		firstDone <- err
	}()

	select {
	case <-runner.started:
	case <-time.After(time.Second):
		t.Fatal("第一个诊断报告没有开始生成")
	}
	if _, err := manager.Bundle(t.Context()); err == nil {
		t.Fatal("并发生成诊断报告未被拒绝")
	}
	close(runner.release)
	if err := <-firstDone; err != nil {
		t.Fatal(err)
	}
}
