package diagnostics

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"
)

func TestPlaybackProbeJournalSectionIsBoundedAndRedacted(t *testing.T) {
	t.Parallel()
	runner := &fakeRunner{output: "normal log\n播放诊断 probe_id=012345 password=private\n"}
	var report strings.Builder
	writePlaybackProbeJournalSection(t.Context(), runner, &report, "")
	if !strings.Contains(report.String(), "probe_id=012345") || strings.Contains(report.String(), "private") || strings.Contains(report.String(), "normal log") {
		t.Fatalf("section = %q", report.String())
	}
	if runner.name != "journalctl" || !strings.Contains(strings.Join(runner.args, " "), "teslabox.service") {
		t.Fatalf("command = %s %v", runner.name, runner.args)
	}
}

func TestPlaybackProbeJournalSectionFiltersSpecificRun(t *testing.T) {
	t.Parallel()
	id := "0123456789abcdef0123456789abcdef"
	runner := &fakeRunner{output: "播放诊断 probe_id=other\n播放诊断 probe_id=" + id + " probe_case=single-front-original password=private\n"}
	var report strings.Builder
	writePlaybackProbeJournalSection(t.Context(), runner, &report, id)
	if !strings.Contains(report.String(), "probe_case=single-front-original") || strings.Contains(report.String(), "probe_id=other") || strings.Contains(report.String(), "private") {
		t.Fatalf("section=%q", report.String())
	}
}

type lsblkFallbackRunner struct {
	calls []string
}

func (r *lsblkFallbackRunner) Run(_ context.Context, name string, args ...string) ([]byte, error) {
	r.calls = append(r.calls, strings.Join(append([]string{name}, args...), " "))
	if name == "lsblk" && strings.Contains(strings.Join(args, " "), "MOUNTPOINTS") {
		return nil, errors.New("lsblk: unknown column: MOUNTPOINTS")
	}
	if name == "lsblk" {
		return []byte("sda disk 116G xfs /var/lib/teslabox USB Disk usb"), nil
	}
	return nil, nil
}

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

func TestMediaLogsExcludeGadgetSnapshotLifecycle(t *testing.T) {
	t.Parallel()
	runner := &fakeRunner{output: strings.Join([]string{
		"teslabox: TeslaUSB 状态已切换 from=GADGET_ACTIVE to=SNAPSHOT_CREATING",
		"kernel: dwc3 fcc00000.dwc3: device reset",
		"teslabox: 视频导出任务失败 error=执行 ffmpeg",
	}, "\n")}
	report, err := New(runner).Logs(t.Context(), SourceMedia, 20)
	if err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(report.Lines, "\n")
	if len(report.Lines) != 1 || strings.Contains(joined, "SNAPSHOT_CREATING") || strings.Contains(joined, "dwc3") || !strings.Contains(joined, "ffmpeg") {
		t.Fatalf("media report = %#v", report)
	}
}

func TestManagerLogsAddsChineseDiagnosticSummaries(t *testing.T) {
	t.Parallel()
	runner := &fakeRunner{output: strings.Join([]string{
		"teslabox: ffmpeg failed: No such filter: subtitles",
		"kernel: Out of memory: Killed process 123 (ffmpeg)",
	}, "\n")}
	report, err := New(runner).Logs(t.Context(), SourceMedia, 20)
	if err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(report.Summaries, "\n")
	if !strings.Contains(joined, "缺少所需滤镜") || !strings.Contains(joined, "内存不足") {
		t.Fatalf("summaries = %#v", report.Summaries)
	}
}

func TestNormalizeLimitUsesCompactDefault(t *testing.T) {
	t.Parallel()
	if got := normalizeLimit(0); got != 50 {
		t.Fatalf("normalizeLimit(0) = %d, want 50", got)
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
	for _, expected := range []string{
		"TeslaBox 诊断报告",
		"TeslaBox 数据盘字节统计",
		"当前启动存储与 USB 关键故障日志",
		"TeslaBox 服务状态",
		"当前启动日志: media",
		"上一次启动关键故障日志",
		"Gadget 总线挂起状态",
		"DWC3 中断计数",
		"TeslaCam 镜像活动时间",
		"Mass Storage STALL 策略",
		"[已脱敏]",
	} {
		if !strings.Contains(content, expected) {
			t.Fatalf("bundle missing %q:\n%s", expected, content)
		}
	}
	if strings.Contains(content, "owner-token") || !strings.HasSuffix(bundle.Filename, ".txt") {
		t.Fatalf("bundle filename = %q, content = %s", bundle.Filename, content)
	}
}

func TestBundleBlockDevicesFallsBackForOldLsblk(t *testing.T) {
	t.Parallel()
	runner := &lsblkFallbackRunner{}
	var report strings.Builder
	writeBlockDevicesSection(t.Context(), runner, &report)
	if len(runner.calls) != 2 || !strings.Contains(runner.calls[0], "MOUNTPOINTS") ||
		!strings.Contains(runner.calls[1], "MOUNTPOINT,") {
		t.Fatalf("calls = %#v", runner.calls)
	}
	if !strings.Contains(report.String(), "/var/lib/teslabox") || strings.Contains(report.String(), "无法读取") {
		t.Fatalf("report = %q", report.String())
	}
}

func TestIncidentJournalIncludesStorageFailureKeywords(t *testing.T) {
	t.Parallel()
	runner := &fakeRunner{output: strings.Join([]string{
		"kernel: harmless message",
		"kernel: XFS (sda1): log I/O error -5",
		"kernel: blk_update_request: I/O error, dev sda",
		"teslabox: write failed: no space left on device",
	}, "\n")}
	var report strings.Builder
	writeIncidentJournalSection(t.Context(), runner, &report, "故障", "-1")
	content := report.String()
	if strings.Contains(content, "harmless") || !strings.Contains(content, "XFS") ||
		!strings.Contains(content, "blk_update_request") || !strings.Contains(content, "no space left") {
		t.Fatalf("report = %q", content)
	}
	if !strings.Contains(strings.Join(runner.args, " "), "--boot=-1") {
		t.Fatalf("args = %#v", runner.args)
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

func TestPlaybackTraceSectionDoesNotDisplaceFailures(t *testing.T) {
	t.Parallel()
	lines := []string{`{"msg":"客户端播放诊断","event":"load_failed","reason":"baseline_failure"}`}
	for i := 0; i < 650; i++ {
		lines = append(lines, `{"msg":"客户端播放诊断","event":"playback_trace","trace":{"phase":"seek","reason_code":"positioned"}}`)
	}
	var report strings.Builder
	writePlaybackProbeJournalSection(t.Context(), &fakeRunner{output: strings.Join(lines, "\n")}, &report, "")
	if !strings.Contains(report.String(), "baseline_failure") || strings.Count(report.String(), `"event":"playback_trace"`) != 650 {
		t.Fatal("stage records displaced failure or exceeded limit")
	}
}
