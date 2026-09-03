package media

import (
	"archive/zip"
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type fakeExportRunner struct {
	block   bool
	started chan struct{}
	once    sync.Once
}

type shutdownExportRunner struct {
	started  chan struct{}
	canceled chan struct{}
	release  chan struct{}
}

type recordingExportRunner struct {
	mu    sync.Mutex
	calls [][]string
}

func (r *recordingExportRunner) RunProgress(_ context.Context, _ string, args []string, progress func(time.Duration)) error {
	r.mu.Lock()
	r.calls = append(r.calls, slices.Clone(args))
	r.mu.Unlock()
	progress(time.Minute)
	return os.WriteFile(args[len(args)-1], []byte("video"), 0o640)
}

func (r *shutdownExportRunner) RunProgress(ctx context.Context, _ string, _ []string, _ func(time.Duration)) error {
	close(r.started)
	<-ctx.Done()
	close(r.canceled)
	<-r.release
	return ctx.Err()
}

func (r *fakeExportRunner) RunProgress(ctx context.Context, _ string, args []string, progress func(time.Duration)) error {
	if r.started != nil {
		r.once.Do(func() { close(r.started) })
	}
	if r.block {
		<-ctx.Done()
		return ctx.Err()
	}
	progress(45 * time.Second)
	return os.WriteFile(args[len(args)-1], []byte("exported-video"), 0o640)
}

func TestExportArgsAlignsCameraGapsAndConcatenatesMinutes(t *testing.T) {
	t.Parallel()
	start := time.Date(2026, 7, 20, 12, 0, 0, 0, time.UTC)
	source := ExportSource{
		SessionStart: start,
		SessionEnd:   start.Add(2 * time.Minute),
		Videos: []ExportVideo{
			{Camera: "front", Path: "/snapshot/front-1.mp4", RecordedAt: start, Duration: 60},
			{Camera: "front", Path: "/snapshot/front-2.mp4", RecordedAt: start.Add(time.Minute), Duration: 60},
			{Camera: "right_repeater", Path: "/snapshot/right-1.mp4", RecordedAt: start.Add(11 * time.Second), Duration: 49},
			{Camera: "right_repeater", Path: "/snapshot/right-2.mp4", RecordedAt: start.Add(time.Minute), Duration: 60},
			{Camera: "left_repeater", Path: "/snapshot/left-1.mp4", RecordedAt: start, Duration: 60},
			{Camera: "left_repeater", Path: "/snapshot/left-2.mp4", RecordedAt: start.Add(time.Minute), Duration: 60},
		},
	}
	request := ExportRequest{
		SessionID: "session", StartSeconds: 10, DurationSeconds: 90,
		Layout: "composite3", Cameras: []string{"front", "right_repeater", "left_repeater"}, Quality: "standard",
	}
	args, err := exportArgs(request, source, "/exports/task.mp4")
	if err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(args, " ")
	for _, expected := range []string{
		"trim=start=10.000000", "tpad=start_mode=clone:start_duration=1.000000",
		"concat=n=2:v=1:a=0", "xstack=inputs=3", "-t 90.000000", "-progress pipe:1",
	} {
		if !strings.Contains(joined, expected) {
			t.Fatalf("args do not contain %q: %s", expected, joined)
		}
	}
}

func TestExporterCompletesTaskAndReleasesSnapshot(t *testing.T) {
	t.Parallel()
	runner := &fakeExportRunner{}
	exporter, err := NewExporter(runner, ExportConfig{
		FFmpegPath: "/opt/ffmpeg", OutputDir: t.TempDir(), QueueSize: 1, MaxDuration: 30 * time.Minute,
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	go exporter.Run(ctx)

	var releases atomic.Int32
	task, err := exporter.Create(validExportRequest(), validExportSource(), func() { releases.Add(1) })
	if err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(2 * time.Second)
	for {
		task, err = exporter.Task(task.ID)
		if err != nil {
			t.Fatal(err)
		}
		if task.State == ExportCompleted {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("task state = %s", task.State)
		}
		time.Sleep(5 * time.Millisecond)
	}
	file, _, err := exporter.Open(task.ID)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	content, err := os.ReadFile(filepath.Join(exporter.config.OutputDir, task.ID+".mp4"))
	if err != nil || string(content) != "exported-video" {
		t.Fatalf("content = %q, error = %v", content, err)
	}
	if releases.Load() != 1 || task.ProgressPercent != 100 || !task.DownloadReady || task.OutputBytes != uint64(len("exported-video")) {
		t.Fatalf("releases = %d, task = %+v", releases.Load(), task)
	}
	if got, want := ExportDownloadFilename(task), "前视-标清-20260720-120000-无叠加.mp4"; got != want {
		t.Fatalf("ExportDownloadFilename() = %q, want %q", got, want)
	}
}

func TestExportDownloadFilenameDescribesQualityAndOverlay(t *testing.T) {
	t.Parallel()
	startAt := time.Date(2026, 8, 22, 16, 7, 27, 0, time.FixedZone("CST", 8*60*60))
	tests := []struct {
		name    string
		request ExportRequest
		want    string
	}{
		{name: "timestamp", request: ExportRequest{Layout: "single", Cameras: []string{"front"}, Quality: "original", BurnTimestamp: true}, want: "前视-原画-20260822-160727-时间.mp4"},
		{name: "telemetry", request: ExportRequest{Layout: "composite6", Quality: "high", BurnSEI: true}, want: "六路合成-高保真-20260822-160727-仪表.mp4"},
		{name: "both", request: ExportRequest{Layout: "composite4a", Quality: "high", BurnTimestamp: true, BurnSEI: true}, want: "四路合成A-高保真-20260822-160727-仪表+时间.mp4"},
		{name: "none", request: ExportRequest{Layout: "single", Cameras: []string{"back"}, Quality: "standard"}, want: "后视-标清-20260822-160727-无叠加.mp4"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			got := ExportDownloadFilename(ExportTask{Request: test.request, VideoStartAt: startAt})
			if got != test.want {
				t.Fatalf("ExportDownloadFilename() = %q, want %q", got, test.want)
			}
		})
	}
}

func TestExporterPersistsHistoryAndConsumesCompletedDownload(t *testing.T) {
	t.Parallel()
	directory := t.TempDir()
	statePath := filepath.Join(t.TempDir(), "export-state.json")
	exporter, err := NewExporter(&fakeExportRunner{}, ExportConfig{
		FFmpegPath: "/opt/ffmpeg", OutputDir: directory, StatePath: statePath, QueueSize: 1, MaxDuration: time.Minute,
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	go exporter.Run(ctx)
	task, err := exporter.Create(validExportRequest(), validExportSource(), func() {})
	if err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(2 * time.Second)
	for task.State != ExportCompleted {
		if time.Now().After(deadline) {
			t.Fatalf("task state = %s", task.State)
		}
		time.Sleep(5 * time.Millisecond)
		task, err = exporter.Task(task.ID)
		if err != nil {
			t.Fatal(err)
		}
	}
	if _, err := exporter.UpdateSettings(ExportSettings{RetainCount: 3}); err != nil {
		t.Fatal(err)
	}
	reloaded, err := NewExporter(&fakeExportRunner{}, ExportConfig{
		FFmpegPath: "/opt/ffmpeg", OutputDir: directory, StatePath: statePath, QueueSize: 1, MaxDuration: time.Minute,
	})
	if err != nil {
		t.Fatal(err)
	}
	history := reloaded.History()
	if len(history) != 1 || history[0].ID != task.ID || history[0].OutputBytes != uint64(len("exported-video")) || reloaded.Settings().RetainCount != 3 {
		t.Fatalf("history = %#v, settings = %#v", history, reloaded.Settings())
	}
	if err := reloaded.Consume(task.ID); err != nil {
		t.Fatal(err)
	}
	if len(reloaded.History()) != 0 {
		t.Fatalf("history after consume = %#v", reloaded.History())
	}
	if _, err := os.Stat(filepath.Join(directory, task.ID+".mp4")); !os.IsNotExist(err) {
		t.Fatalf("completed output was not removed: %v", err)
	}
}

func TestExporterDeletesCompletedExportWithoutDownload(t *testing.T) {
	t.Parallel()
	directory := t.TempDir()
	statePath := filepath.Join(t.TempDir(), "export-state.json")
	exporter, err := NewExporter(&fakeExportRunner{}, ExportConfig{
		FFmpegPath: "/opt/ffmpeg", OutputDir: directory, StatePath: statePath, QueueSize: 1, MaxDuration: time.Minute,
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	go exporter.Run(ctx)
	task, err := exporter.Create(validExportRequest(), validExportSource(), func() {})
	if err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(2 * time.Second)
	for task.State != ExportCompleted {
		if time.Now().After(deadline) {
			t.Fatalf("task state = %s", task.State)
		}
		time.Sleep(5 * time.Millisecond)
		task, err = exporter.Task(task.ID)
		if err != nil {
			t.Fatal(err)
		}
	}
	if err := exporter.Delete(task.ID); err != nil {
		t.Fatal(err)
	}
	if len(exporter.History()) != 0 {
		t.Fatalf("history after delete = %#v", exporter.History())
	}
	if _, err := os.Stat(filepath.Join(directory, task.ID+".mp4")); !os.IsNotExist(err) {
		t.Fatalf("completed output was not removed: %v", err)
	}
	reloaded, err := NewExporter(&fakeExportRunner{}, ExportConfig{
		FFmpegPath: "/opt/ffmpeg", OutputDir: directory, StatePath: statePath, QueueSize: 1, MaxDuration: time.Minute,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(reloaded.History()) != 0 {
		t.Fatalf("persisted history after delete = %#v", reloaded.History())
	}
}

func TestExportOverlaySubtitlesAreEscapedAndMapped(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), ".task.tmp.ass")
	request := validExportRequest()
	request.BurnTimestamp = true
	request.BurnSEI = true
	hasSubtitles, err := writeOverlaySubtitles(path, request, []ExportOverlayCue{{
		Start: 0, End: time.Second, Timestamp: "2026-07-30 12:00:00",
		Telemetry: &ExportTelemetry{Gear: "D", SpeedKPH: 20, AcceleratorPosition: 45, SteeringAngle: -1.7, BrakeApplied: true, Autopilot: "AP"},
	}})
	if err != nil || !hasSubtitles {
		t.Fatalf("writeOverlaySubtitles() = %t, %v", hasSubtitles, err)
	}
	content, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(content), "0:00:00.00,0:00:01.00,TimeValue") || !strings.Contains(string(content), "km/h") || !strings.Contains(string(content), "电门 45%") || !strings.Contains(string(content), "\\p1") {
		t.Fatalf("subtitle content = %s", content)
	}
	args, err := exportArgs(validExportRequest(), validExportSource(), "/exports/task.mp4", path)
	if err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(args, " ")
	if !strings.Contains(joined, "ass=filename='") || !strings.Contains(joined, "-map [overlay]") {
		t.Fatalf("overlay args = %s", joined)
	}
}

func TestCompositeExportUsesSeparateInformationBarWithoutVisibleAlignmentPadding(t *testing.T) {
	t.Parallel()
	request, err := normalizeExportRequest(ExportRequest{
		SessionID: "session", DurationSeconds: 10, Layout: "composite6", Quality: "high", BurnTimestamp: true, BurnSEI: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "overlay.ass")
	_, err = writeOverlaySubtitles(path, request, []ExportOverlayCue{{
		Start: 0, End: 10 * time.Second, Timestamp: "2026-08-08 12:00:00", Telemetry: &ExportTelemetry{Gear: "D", SpeedKPH: 85, SteeringAngle: -1.7},
	}})
	if err != nil {
		t.Fatal(err)
	}
	content, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(content), "PlayResX: 2528\nPlayResY: 1184") {
		t.Fatalf("ASS canvas does not match aligned six-camera output: %s", content)
	}
	if !strings.Contains(string(content), "\\frz1.70") || strings.Contains(string(content), "◀") || strings.Contains(string(content), "▶") {
		t.Fatalf("dashboard vectors do not match online steering and blinker styles: %s", content)
	}
	if !strings.Contains(string(content), "Style: TimeLabel,Noto Sans CJK SC,23,") ||
		!strings.Contains(string(content), "Style: TimeValue,Noto Sans CJK SC,33,") ||
		!strings.Contains(string(content), "\\fs24") {
		t.Fatalf("dashboard typography does not use the enlarged export sizes: %s", content)
	}
	args, err := exportArgs(request, sourceForCameras(request.Cameras), "/exports/six.mp4", path)
	if err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(args, " ")
	if !strings.Contains(joined, "pad=w=iw:h=ih+96:x=0:y=96:color=black") || strings.Contains(joined, "ceil(iw/16)") || !strings.Contains(joined, "fontsdir='/opt/teslabox/media/share/fonts'") {
		t.Fatalf("information bar args = %s", joined)
	}
}

func TestExportOverlayModesControlCanvasAndPadding(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name        string
		request     ExportRequest
		wantCanvas  string
		wantContent string
		wantPad     string
		rejectPad   bool
	}{
		{
			name: "行车记录仪时间", request: ExportRequest{SessionID: "session", DurationSeconds: 10, Layout: "single", Cameras: []string{"front"}, Quality: "original", BurnTimestamp: true, OverlayMode: ExportOverlayDashcam},
			wantCanvas: "PlayResX: 1448\nPlayResY: 938", wantContent: "Style: DashcamTime,Noto Sans CJK SC,53,", rejectPad: true,
		},
		{
			name: "单路顶部时间栏", request: ExportRequest{SessionID: "session", DurationSeconds: 10, Layout: "single", Cameras: []string{"front"}, Quality: "original", BurnTimestamp: true, OverlayMode: ExportOverlayTopBar},
			wantCanvas: "PlayResX: 1448\nPlayResY: 1024", wantContent: "录像时间", wantPad: "pad=w=iw:h=ih+86:x=0:y=86:color=black",
		},
		{
			name: "悬浮仪表", request: ExportRequest{SessionID: "session", DurationSeconds: 10, Layout: "single", Cameras: []string{"front"}, Quality: "high", BurnTimestamp: true, BurnSEI: true, OverlayMode: ExportOverlayFloating},
			wantCanvas: "PlayResX: 1666\nPlayResY: 1080", wantContent: "\\1a&HA0", rejectPad: true,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "overlay.ass")
			_, err := writeOverlaySubtitles(path, test.request, []ExportOverlayCue{{Start: 0, End: 10 * time.Second, Timestamp: "2026-08-08 12:00:00", Telemetry: &ExportTelemetry{Gear: "D", SpeedKPH: 20}}})
			if err != nil {
				t.Fatal(err)
			}
			content, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(string(content), test.wantCanvas) || !strings.Contains(string(content), test.wantContent) {
				t.Fatalf("overlay content = %s", content)
			}
			args, err := exportArgs(test.request, validExportSource(), "/exports/overlay.mp4", path)
			if err != nil {
				t.Fatal(err)
			}
			joined := strings.Join(args, " ")
			if test.wantPad != "" && !strings.Contains(joined, test.wantPad) {
				t.Fatalf("overlay args do not contain %q: %s", test.wantPad, joined)
			}
			if test.rejectPad && strings.Contains(joined, "pad=w=iw:h=ih+") {
				t.Fatalf("overlay mode must not extend canvas: %s", joined)
			}
		})
	}
}

func TestOriginalExportUsesMeasuredSourceCanvasForTopBar(t *testing.T) {
	t.Parallel()
	request := ExportRequest{
		SessionID: "session", DurationSeconds: 10, Layout: "single", Cameras: []string{"front"}, Quality: "original",
		BurnTimestamp: true, BurnSEI: true, OverlayMode: ExportOverlayTopBar,
	}
	source := validExportSource()
	source.CanvasWidth, source.CanvasHeight = 1448, 938
	path := filepath.Join(t.TempDir(), "measured-original.ass")
	_, err := writeOverlaySubtitles(path, request, []ExportOverlayCue{{
		Start: 0, End: 10 * time.Second, Timestamp: "2026-09-02 12:00:00", Telemetry: &ExportTelemetry{Gear: "D", SpeedKPH: 64},
	}}, source)
	if err != nil {
		t.Fatal(err)
	}
	content, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, marker := range []string{"PlayResX: 1448\nPlayResY: 1056", "m 0 0 l 1448 0 l 1448 118 l 0 118"} {
		if !strings.Contains(string(content), marker) {
			t.Fatalf("原画顶部信息栏未覆盖实测画布：缺少 %q：%s", marker, content)
		}
	}
	args, err := exportArgs(request, source, "/exports/original-top-bar.mp4", path)
	if err != nil {
		t.Fatal(err)
	}
	if joined := strings.Join(args, " "); !strings.Contains(joined, "pad=w=iw:h=ih+118:x=0:y=118:color=black") {
		t.Fatalf("原画顶部信息栏未在视频上方扩展画布：%s", joined)
	}
}

func TestFrontOriginalSingleOverlayUsesDedicatedLargerScaleOnlyWithinScope(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name        string
		request     ExportRequest
		wantMarkers []string
	}{
		{
			name: "前视原画行车记录仪模式放大时间",
			request: ExportRequest{SessionID: "session", DurationSeconds: 10, Layout: "single", Cameras: []string{"front"}, Quality: "original",
				BurnTimestamp: true, OverlayMode: ExportOverlayDashcam},
			wantMarkers: []string{"PlayResX: 1448\nPlayResY: 938", "Style: DashcamTime,Noto Sans CJK SC,53,"},
		},
		{
			name: "前视原画悬浮信息栏整体放大",
			request: ExportRequest{SessionID: "session", DurationSeconds: 10, Layout: "single", Cameras: []string{"front"}, Quality: "original",
				BurnTimestamp: true, BurnSEI: true, OverlayMode: ExportOverlayFloating},
			wantMarkers: []string{
				"PlayResX: 1448\nPlayResY: 938", "Style: DashcamTime,Noto Sans CJK SC,53,", "Style: Dashboard,Noto Sans CJK SC,33,",
				"{\\an7\\pos(379,7)", "{\\an8\\pos(448,", "{\\an8\\pos(724,", "{\\an8\\pos(1000,",
			},
		},
		{
			name: "前视原画顶部信息栏整体放大",
			request: ExportRequest{SessionID: "session", DurationSeconds: 10, Layout: "single", Cameras: []string{"front"}, Quality: "original",
				BurnTimestamp: true, BurnSEI: true, OverlayMode: ExportOverlayTopBar},
			wantMarkers: []string{
				"PlayResX: 1448\nPlayResY: 1056", "Style: TimeLabel,Noto Sans CJK SC,26,", "Style: TimeValue,Noto Sans CJK SC,38,",
				"Style: Dashboard,Noto Sans CJK SC,33,", "m 0 0 l 1448 0 l 1448 118 l 0 118",
			},
		},
		{
			name: "前视原画纯时间顶部栏放大",
			request: ExportRequest{SessionID: "session", DurationSeconds: 10, Layout: "single", Cameras: []string{"front"}, Quality: "original",
				BurnTimestamp: true, OverlayMode: ExportOverlayTopBar},
			wantMarkers: []string{
				"PlayResX: 1448\nPlayResY: 1024", "Style: TimeLabel,Noto Sans CJK SC,26,", "Style: TimeValue,Noto Sans CJK SC,38,",
				"m 0 0 l 1448 0 l 1448 86 l 0 86",
			},
		},
		{
			name: "其他视角原画保持原缩放",
			request: ExportRequest{SessionID: "session", DurationSeconds: 10, Layout: "single", Cameras: []string{"back"}, Quality: "original",
				BurnTimestamp: true, BurnSEI: true, OverlayMode: ExportOverlayFloating},
			wantMarkers: []string{"Style: DashcamTime,Noto Sans CJK SC,40,", "Style: Dashboard,Noto Sans CJK SC,25,", "{\\an7\\pos(463,5)"},
		},
		{
			name: "前视高保真保持原缩放",
			request: ExportRequest{SessionID: "session", DurationSeconds: 10, Layout: "single", Cameras: []string{"front"}, Quality: "high",
				BurnTimestamp: true, BurnSEI: true, OverlayMode: ExportOverlayFloating},
			wantMarkers: []string{"Style: DashcamTime,Noto Sans CJK SC,46,", "Style: Dashboard,Noto Sans CJK SC,29,", "{\\an7\\pos(533,6)"},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "overlay.ass")
			if _, err := writeOverlaySubtitles(path, test.request, []ExportOverlayCue{{
				Start: 0, End: 10 * time.Second, Timestamp: "2026-09-03 12:00:00", Telemetry: &ExportTelemetry{Gear: "D", SpeedKPH: 64},
			}}); err != nil {
				t.Fatal(err)
			}
			contentBytes, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			content := string(contentBytes)
			for _, marker := range test.wantMarkers {
				if !strings.Contains(content, marker) {
					t.Errorf("%s 缺少 %q：%s", test.name, marker, content)
				}
			}
		})
	}
}

func TestFloatingExportKeepsTimestampOutsideCompactRoundedDashboard(t *testing.T) {
	t.Parallel()
	request := ExportRequest{
		SessionID: "session", DurationSeconds: 10, Layout: "single", Cameras: []string{"front"}, Quality: "high",
		BurnTimestamp: true, BurnSEI: true, OverlayMode: ExportOverlayFloating,
	}
	path := filepath.Join(t.TempDir(), "floating.ass")
	_, err := writeOverlaySubtitles(path, request, []ExportOverlayCue{{
		Start: 0, End: 10 * time.Second, Timestamp: "2026-08-08 12:00:00",
		Telemetry: &ExportTelemetry{Gear: "D", SpeedKPH: 85, SteeringAngle: -1.7, Autopilot: "AP"},
	}})
	if err != nil {
		t.Fatal(err)
	}
	contentBytes, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	content := string(contentBytes)
	for _, marker := range []string{
		"Style: DashcamTime,Noto Sans CJK SC,46,", "Dialogue: 2,0:00:00.00,0:00:10.00,DashcamTime", "\\1a&HA0", "{\\an7\\pos(533,6)",
		"{\\an7\\pos(733,21)", "{\\an7\\pos(893,21)", "{\\an8\\pos(833,72)\\fs20\\1c&H00FFFFFF&\\b1}AP",
		"{\\an8\\pos(593,67)\\fs24\\1c&H00FFFFFF&\\b1}档位", "{\\an8\\pos(993,67)\\fs24\\1c&H00FFFFFF&\\b1}刹车",
		"Dialogue: 3,0:00:00.00,0:00:10.00,Canvas", "\\1a&H20&\\3c&H00FFB96E&",
	} {
		if !strings.Contains(content, marker) {
			t.Errorf("悬浮信息栏缺少布局标记 %q：%s", marker, content)
		}
	}
	if strings.Contains(content, ",TimeLabel,,") || strings.Contains(content, ",TimeValue,,") || strings.Contains(content, "录像时间") {
		t.Fatalf("悬浮模式不应把时间放入仪表框：%s", content)
	}
	if strings.Contains(content, "\\1c&H00000000&\\org(") || strings.Count(content, "\\org(673,41)\\frz1.70") != 1 {
		t.Fatalf("方向盘必须使用单个随角度旋转的透明镂空路径：%s", content)
	}
}

func TestSteeringWheelASSReversesAngleToMatchCSSDirection(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name  string
		angle float64
		want  string
	}{
		{name: "左转", angle: -22.4, want: "\\frz22.40"},
		{name: "右转", angle: 22.4, want: "\\frz-22.40"},
	} {
		t.Run(test.name, func(t *testing.T) {
			lines := appendSteeringWheelASS(nil, "0:00:00.00", "0:00:01.00", 100, 20, 1, "&H00FFFFFF&", test.angle)
			if len(lines) != 1 || !strings.Contains(lines[0], test.want) {
				t.Fatalf("appendSteeringWheelASS(%v) = %#v, want %q", test.angle, lines, test.want)
			}
		})
	}
}

func TestExportOverlayUsesStrokeFreeTimestampAndReadableInactiveTelemetry(t *testing.T) {
	t.Parallel()
	request := ExportRequest{
		SessionID: "session", DurationSeconds: 10, Layout: "single", Cameras: []string{"front"}, Quality: "high",
		BurnTimestamp: true, BurnSEI: true, OverlayMode: ExportOverlayFloating,
	}
	path := filepath.Join(t.TempDir(), "readable-inactive.ass")
	_, err := writeOverlaySubtitles(path, request, []ExportOverlayCue{{
		Start: 0, End: 10 * time.Second, Timestamp: "2026-09-02 12:00:00",
		Telemetry: &ExportTelemetry{Gear: "P", BlinkerVisible: true},
	}})
	if err != nil {
		t.Fatal(err)
	}
	contentBytes, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	content := string(contentBytes)
	if !strings.Contains(content, "Style: DashcamTime,Noto Sans CJK SC,46,&H00FFFFFF,&H00FFFFFF,&H00000000,&H80000000,1,0,0,0,100,100,0,0,1,0,0,7,") {
		t.Fatalf("左上角时间仍包含描边或阴影：%s", content)
	}
	for _, marker := range []string{
		"{\\an8\\pos(833,72)\\fs20\\1c&H00FFFFFF&\\b1}手动",
		"{\\an7\\pos(733,21)\\fscx100\\fscy100\\1c&H00FFFFFF&",
		"{\\an7\\pos(893,21)\\fscx100\\fscy100\\1c&H00FFFFFF&",
		"{\\an7\\pos(973,21)\\fscx100\\fscy100\\1c&H00FFFFFF&",
	} {
		if !strings.Contains(content, marker) {
			t.Errorf("未激活状态缺少高对比白色标记 %q：%s", marker, content)
		}
	}
}

func TestFloatingExportCentersCompactDashboardForEveryLayout(t *testing.T) {
	t.Parallel()
	for _, layout := range []string{"single", "composite6", "composite4a", "composite4b", "composite3"} {
		t.Run(layout, func(t *testing.T) {
			request := ExportRequest{
				SessionID: "session", DurationSeconds: 10, Layout: layout, Cameras: []string{"front"}, Quality: "high",
				BurnSEI: true, OverlayMode: ExportOverlayFloating,
			}
			width, height, _, _, err := exportOverlayCanvas(request)
			if err != nil {
				t.Fatal(err)
			}
			scale := max(0.55, min(1.2, min(float64(height)/1080, float64(width)/1280)))
			panelWidth := scaleASS(600, scale)
			panelLeft := (width - panelWidth) / 2
			centerX := width / 2
			step := scaleASS(80, scale)
			path := filepath.Join(t.TempDir(), layout+".ass")
			_, err = writeOverlaySubtitles(path, request, []ExportOverlayCue{{
				Start: 0, End: 10 * time.Second, Telemetry: &ExportTelemetry{Gear: "D", SteeringAngle: -22.4},
			}})
			if err != nil {
				t.Fatal(err)
			}
			contentBytes, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			content := string(contentBytes)
			for _, marker := range []string{
				fmt.Sprintf("{\\an7\\pos(%d,%d)", panelLeft, max(scaleASS(6, scale), 4)),
				fmt.Sprintf("{\\an8\\pos(%d,", centerX-3*step),
				fmt.Sprintf("{\\an8\\pos(%d,", centerX),
				fmt.Sprintf("{\\an8\\pos(%d,", centerX+3*step),
				"\\frz22.40",
			} {
				if !strings.Contains(content, marker) {
					t.Errorf("%s 布局未按画布中心使用紧凑等距仪表：缺少 %q", layout, marker)
				}
			}
		})
	}
}

func TestWriteExportOverlayRenderFixtures(t *testing.T) {
	directory := os.Getenv("TESLABOX_RENDER_FIXTURE_DIR")
	if directory == "" {
		t.Skip("未请求生成导出叠加层渲染样本")
	}
	if err := os.MkdirAll(directory, 0o750); err != nil {
		t.Fatal(err)
	}
	cue := ExportOverlayCue{
		Start: 0, End: time.Second, Timestamp: "2026-08-30 12:02:13",
		Telemetry: &ExportTelemetry{Gear: "D", SpeedKPH: 64, AcceleratorPosition: 27, SteeringAngle: 4.2, BlinkerVisible: true},
	}
	tests := []struct {
		name    string
		request ExportRequest
		source  ExportSource
	}{
		{
			name: "single-original-dashcam-1448x938.ass",
			request: ExportRequest{SessionID: "session", DurationSeconds: 1, Layout: "single", Cameras: []string{"front"}, Quality: "original",
				BurnTimestamp: true, OverlayMode: ExportOverlayDashcam},
			source: ExportSource{CanvasWidth: 1448, CanvasHeight: 938},
		},
		{
			name: "single-original-floating-1448x938.ass",
			request: ExportRequest{SessionID: "session", DurationSeconds: 1, Layout: "single", Cameras: []string{"front"}, Quality: "original",
				BurnTimestamp: true, BurnSEI: true, OverlayMode: ExportOverlayFloating},
			source: ExportSource{CanvasWidth: 1448, CanvasHeight: 938},
		},
		{
			name: "single-high-floating-1666x1080.ass",
			request: ExportRequest{SessionID: "session", DurationSeconds: 1, Layout: "single", Cameras: []string{"front"}, Quality: "high",
				BurnTimestamp: true, BurnSEI: true, OverlayMode: ExportOverlayFloating},
		},
		{
			name: "single-original-topbar-1448x1056.ass",
			request: ExportRequest{SessionID: "session", DurationSeconds: 1, Layout: "single", Cameras: []string{"front"}, Quality: "original",
				BurnTimestamp: true, BurnSEI: true, OverlayMode: ExportOverlayTopBar},
			source: ExportSource{CanvasWidth: 1448, CanvasHeight: 938},
		},
		{
			name: "composite4b-high-floating-1264x1088.ass",
			request: ExportRequest{SessionID: "session", DurationSeconds: 1, Layout: "composite4b", Quality: "high",
				BurnTimestamp: true, BurnSEI: true, OverlayMode: ExportOverlayFloating},
		},
	}
	for _, test := range tests {
		if _, err := writeOverlaySubtitles(filepath.Join(directory, test.name), test.request, []ExportOverlayCue{cue}, test.source); err != nil {
			t.Fatalf("生成 %s：%v", test.name, err)
		}
	}
}

func TestNormalizeExportRequestRejectsDashcamTelemetry(t *testing.T) {
	t.Parallel()
	_, err := normalizeExportRequest(ExportRequest{
		SessionID: "session", DurationSeconds: 10, Layout: "single", Cameras: []string{"front"}, Quality: "high",
		BurnTimestamp: true, BurnSEI: true, OverlayMode: ExportOverlayDashcam,
	})
	if err == nil || !strings.Contains(err.Error(), "行车记录仪模式") {
		t.Fatalf("normalizeExportRequest() error = %v", err)
	}
}

func TestPrepareExportOverlayUsesSourceWallClock(t *testing.T) {
	t.Parallel()
	timelineStart := time.Date(2026, 7, 30, 8, 0, 0, 0, time.FixedZone("CST", 8*60*60))
	source := ExportSource{
		SessionStart: timelineStart, SessionEnd: timelineStart.Add(2 * time.Minute),
		Videos: []ExportVideo{{
			Camera: "front", Path: filepath.Join(t.TempDir(), "front.mp4"), RecordedAt: timelineStart,
			WallClockAt: timelineStart.Add(15 * time.Minute), Duration: 60,
		}},
	}
	err := prepareExportOverlayCues(t.Context(), ExportRequest{
		StartSeconds: 5, DurationSeconds: 2, Cameras: []string{"front"}, BurnTimestamp: true,
	}, &source, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	if len(source.OverlayCues) != 2 || source.OverlayCues[0].Timestamp != "2026-07-30 08:15:05" {
		t.Fatalf("overlay cues = %#v", source.OverlayCues)
	}
}

func TestPrepareExportOverlayUsesTenHertzSEISampling(t *testing.T) {
	t.Parallel()
	start := time.Date(2026, 8, 8, 12, 0, 0, 0, time.UTC)
	source := ExportSource{SessionStart: start, SessionEnd: start.Add(time.Second)}
	err := prepareExportOverlayCues(t.Context(), ExportRequest{
		DurationSeconds: 1, Cameras: []string{"front"}, BurnSEI: true,
	}, &source, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	if len(source.OverlayCues) != 10 || source.OverlayCues[0].End != 100*time.Millisecond || source.OverlayCues[9].End != time.Second {
		t.Fatalf("overlay cues = %#v", source.OverlayCues)
	}
}

func TestExporterPackagesSelectedSourceFilesWithoutTranscoding(t *testing.T) {
	t.Parallel()
	directory := t.TempDir()
	firstPath := filepath.Join(directory, "2026-07-20_10-00-00-front.mp4")
	secondPath := filepath.Join(directory, "2026-07-20_10-00-00-back.mp4")
	if err := os.WriteFile(firstPath, []byte("front-source"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(secondPath, []byte("back-source"), 0o600); err != nil {
		t.Fatal(err)
	}
	exporter, err := NewExporter(&fakeExportRunner{}, ExportConfig{
		FFmpegPath: "/opt/ffmpeg", OutputDir: filepath.Join(directory, "exports"), QueueSize: 1, MaxDuration: 30 * time.Minute,
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	go exporter.Run(ctx)
	recordedAt := time.Date(2026, 7, 20, 10, 0, 0, 0, time.UTC)
	source := ExportSource{SourceFiles: []SourceExportFile{
		{MomentID: "minute-1", Camera: "front", Name: filepath.Base(firstPath), Path: firstPath, RecordedAt: recordedAt, Size: uint64(len("front-source"))},
		{MomentID: "minute-1", Camera: "back", Name: filepath.Base(secondPath), Path: secondPath, RecordedAt: recordedAt, Size: uint64(len("back-source"))},
	}}
	var releases atomic.Int32
	task, err := exporter.Create(ExportRequest{
		Mode: ExportModeSource, SessionID: "session", MomentIDs: []string{"minute-1"}, Cameras: []string{"front", "back"},
	}, source, func() { releases.Add(1) })
	if err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(2 * time.Second)
	for {
		task, err = exporter.Task(task.ID)
		if err != nil {
			t.Fatal(err)
		}
		if task.State == ExportCompleted {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("task state = %s", task.State)
		}
		time.Sleep(5 * time.Millisecond)
	}
	if task.FileCount != 2 || task.EstimatedBytes != uint64(len("front-source")+len("back-source")) || releases.Load() != 1 {
		t.Fatalf("task = %#v, releases = %d", task, releases.Load())
	}
	archive, err := zip.OpenReader(filepath.Join(exporter.config.OutputDir, task.ID+".zip"))
	if err != nil {
		t.Fatal(err)
	}
	defer archive.Close()
	contents := make(map[string]string, len(archive.File))
	for _, item := range archive.File {
		if item.Method != zip.Store {
			t.Fatalf("archive entry %q method = %d, want store", item.Name, item.Method)
		}
		reader, err := item.Open()
		if err != nil {
			t.Fatal(err)
		}
		body, err := io.ReadAll(reader)
		closeErr := reader.Close()
		if err != nil || closeErr != nil {
			t.Fatalf("read %s: %v %v", item.Name, err, closeErr)
		}
		contents[item.Name] = string(body)
	}
	if contents[filepath.Base(firstPath)] != "front-source" || contents[filepath.Base(secondPath)] != "back-source" ||
		!strings.Contains(contents["manifest.json"], `"moment_id": "minute-1"`) ||
		!strings.Contains(contents["manifest.json"], `"archive_name": "2026-07-20_10-00-00-front.mp4"`) ||
		!strings.Contains(contents["player.html"], "TeslaBox 原画多路浏览包") ||
		!strings.Contains(contents["player.html"], "archive_name") {
		t.Fatalf("archive contents = %#v", contents)
	}
}

func TestExporterFirstCreateRemovesOnlyManagedTemporaryArtifacts(t *testing.T) {
	t.Parallel()
	directory := t.TempDir()
	temporary := ".0123456789abcdef01234567.tmp.mp4"
	completed := "0123456789abcdef01234567.mp4"
	for _, name := range []string{temporary, completed, "keep.mp4"} {
		if err := os.WriteFile(filepath.Join(directory, name), []byte("video"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	exporter, err := NewExporter(&fakeExportRunner{}, ExportConfig{
		FFmpegPath: "/opt/ffmpeg", OutputDir: directory, QueueSize: 1, MaxDuration: time.Minute,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := exporter.Create(validExportRequest(), validExportSource(), func() {}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(directory, temporary)); !os.IsNotExist(err) {
		t.Fatalf("temporary artifact %q was not removed: %v", temporary, err)
	}
	if _, err := os.Stat(filepath.Join(directory, completed)); err != nil {
		t.Fatalf("completed artifact %q was removed: %v", completed, err)
	}
	if _, err := os.Stat(filepath.Join(directory, "keep.mp4")); err != nil {
		t.Fatalf("unmanaged file was removed: %v", err)
	}
}

func TestExporterCancelQueuedTaskReleasesSnapshotImmediately(t *testing.T) {
	t.Parallel()
	runner := &fakeExportRunner{block: true, started: make(chan struct{})}
	exporter, err := NewExporter(runner, ExportConfig{
		FFmpegPath: "/opt/ffmpeg", OutputDir: t.TempDir(), QueueSize: 2, MaxDuration: 30 * time.Minute,
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	go exporter.Run(ctx)

	var firstRelease atomic.Int32
	if _, err := exporter.Create(validExportRequest(), validExportSource(), func() { firstRelease.Add(1) }); err != nil {
		t.Fatal(err)
	}
	select {
	case <-runner.started:
	case <-time.After(time.Second):
		t.Fatal("running task did not start")
	}
	var queuedRelease atomic.Int32
	queued, err := exporter.Create(validExportRequest(), validExportSource(), func() { queuedRelease.Add(1) })
	if err != nil {
		t.Fatal(err)
	}
	canceled, err := exporter.Cancel(queued.ID)
	if err != nil {
		t.Fatal(err)
	}
	if canceled.State != ExportCanceled || queuedRelease.Load() != 1 {
		t.Fatalf("state = %s, releases = %d", canceled.State, queuedRelease.Load())
	}
	cancel()
	waitCtx, waitCancel := context.WithTimeout(t.Context(), time.Second)
	defer waitCancel()
	if err := exporter.Wait(waitCtx); err != nil {
		t.Fatalf("Wait() error = %v", err)
	}
	if firstRelease.Load() != 1 {
		t.Fatalf("running snapshot releases = %d", firstRelease.Load())
	}
}

func TestExporterShutdownWaitsForRunnerBeforeReleasingSnapshot(t *testing.T) {
	t.Parallel()
	runner := &shutdownExportRunner{
		started: make(chan struct{}), canceled: make(chan struct{}), release: make(chan struct{}),
	}
	exporter, err := NewExporter(runner, ExportConfig{
		FFmpegPath: "/opt/ffmpeg", OutputDir: t.TempDir(), QueueSize: 1, MaxDuration: time.Minute,
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	go exporter.Run(ctx)
	var releases atomic.Int32
	if _, err := exporter.Create(validExportRequest(), validExportSource(), func() { releases.Add(1) }); err != nil {
		t.Fatal(err)
	}
	select {
	case <-runner.started:
	case <-time.After(time.Second):
		t.Fatal("running task did not start")
	}
	cancel()
	select {
	case <-runner.canceled:
	case <-time.After(time.Second):
		t.Fatal("runner did not receive cancellation")
	}
	if releases.Load() != 0 {
		t.Fatalf("snapshot released before runner exit: %d", releases.Load())
	}
	close(runner.release)
	waitCtx, waitCancel := context.WithTimeout(t.Context(), time.Second)
	defer waitCancel()
	if err := exporter.Wait(waitCtx); err != nil {
		t.Fatalf("Wait() error = %v", err)
	}
	if releases.Load() != 1 {
		t.Fatalf("snapshot releases = %d", releases.Load())
	}
}

func validExportRequest() ExportRequest {
	return ExportRequest{SessionID: "session", DurationSeconds: 60, Layout: "single", Cameras: []string{"front"}, Quality: "standard"}
}

func TestExportOriginalPreservesSingleCameraDimensions(t *testing.T) {
	t.Parallel()
	request := ExportRequest{SessionID: "session", DurationSeconds: 60, Layout: "single", Cameras: []string{"front"}, Quality: "original"}
	args, err := exportArgs(request, validExportSource(), "/exports/original.mp4")
	if err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(args, " ")
	for _, expected := range []string{"fps=36", "hwdownload,format=nv12", "-c:v hevc_rkmpp", "-tag:v hvc1", "-rc_mode CQP", "-qp_init 18"} {
		if !strings.Contains(joined, expected) {
			t.Fatalf("original args do not contain %q: %s", expected, joined)
		}
	}
	if strings.Contains(joined, "scale_rkrga") || strings.Contains(joined, "pad=") {
		t.Fatalf("原画单路不应缩放或补边: %s", joined)
	}
}

func TestNormalizeExportRequestUsesPlaybackLayouts(t *testing.T) {
	t.Parallel()
	request, err := normalizeExportRequest(ExportRequest{Layout: "composite4a", Cameras: []string{"front"}})
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"front", "back", "left_repeater", "right_repeater"}
	if !slices.Equal(request.Cameras, want) {
		t.Fatalf("cameras = %#v, want %#v", request.Cameras, want)
	}
}

func TestValidateExportRejectsOriginalComposite(t *testing.T) {
	t.Parallel()
	request, err := normalizeExportRequest(ExportRequest{SessionID: "session", DurationSeconds: 60, Layout: "composite3", Quality: "original"})
	if err != nil {
		t.Fatal(err)
	}
	if err := validateExport(request, validExportSource(), time.Minute); err == nil || !strings.Contains(err.Error(), "原画 MP4 仅支持单路") {
		t.Fatalf("error = %v", err)
	}
}

func TestPlanClipExportBoundsHardwareDecoderInputsPerSegment(t *testing.T) {
	t.Parallel()
	start := time.Date(2026, 7, 20, 12, 0, 0, 0, time.UTC)
	source := ExportSource{
		SessionStart: start,
		SessionEnd:   start.Add(13 * 10 * time.Second),
		Videos:       make([]ExportVideo, 0, 13),
	}
	for index := 0; index < 13; index++ {
		source.Videos = append(source.Videos, ExportVideo{
			Camera:     "front",
			Path:       fmt.Sprintf("/snapshot/front-%02d.mp4", index),
			RecordedAt: start.Add(time.Duration(index) * 10 * time.Second),
			Duration:   10,
		})
	}
	request := ExportRequest{
		SessionID: "session", DurationSeconds: 130, Layout: "single",
		Cameras: []string{"front"}, Quality: "standard",
	}
	if err := validateExport(request, source, 0); err != nil {
		t.Fatalf("validateExport() error = %v", err)
	}
	segments, err := planClipExport(request, source)
	if err != nil {
		t.Fatal(err)
	}
	if len(segments) != 13 {
		t.Fatalf("segment count = %d, want 13", len(segments))
	}
	for _, segment := range segments {
		if count := exportDecoderInputCount(segment.request, segment.source); count > 1 {
			t.Fatalf("segment %d decoder inputs = %d", segment.index, count)
		}
	}
}

func TestPlanClipExportBoundsStaggeredSixCameraMinuteBoundary(t *testing.T) {
	t.Parallel()
	start := time.Date(2026, 8, 8, 12, 0, 0, 0, time.UTC)
	cameras := []string{"front", "back", "left_repeater", "right_repeater", "left_pillar", "right_pillar"}
	source := ExportSource{SessionStart: start, SessionEnd: start.Add(72 * time.Second)}
	for index, camera := range cameras {
		boundary := 60*time.Second + time.Duration(index-2)*80*time.Millisecond
		source.Videos = append(source.Videos,
			ExportVideo{Camera: camera, Path: "/snapshot/" + camera + "-0.mp4", RecordedAt: start, Duration: boundary.Seconds()},
			ExportVideo{Camera: camera, Path: "/snapshot/" + camera + "-1.mp4", RecordedAt: start.Add(boundary), Duration: 12},
		)
	}
	request := ExportRequest{SessionID: "session", DurationSeconds: 72, Layout: "composite6", Cameras: cameras, Quality: "high"}
	segments, err := planClipExport(request, source)
	if err != nil {
		t.Fatal(err)
	}
	for _, segment := range segments {
		if count := exportDecoderInputCount(segment.request, segment.source); count > len(cameras) {
			t.Fatalf("segment %d decoder inputs = %d, want at most %d", segment.index, count, len(cameras))
		}
		seen := make(map[string]bool, len(cameras))
		for _, video := range segment.source.Videos {
			if seen[video.Camera] {
				t.Fatalf("segment %d contains duplicate camera %q", segment.index, video.Camera)
			}
			seen[video.Camera] = true
		}
	}
}

func TestExporterRunsLongClipSequentiallyAndRemuxesSegments(t *testing.T) {
	t.Parallel()
	start := time.Date(2026, 7, 20, 12, 0, 0, 0, time.UTC)
	source := ExportSource{SessionStart: start, SessionEnd: start.Add(130 * time.Second)}
	for index := 0; index < 13; index++ {
		source.Videos = append(source.Videos, ExportVideo{
			Camera: "front", Path: fmt.Sprintf("/snapshot/front-%02d.mp4", index),
			RecordedAt: start.Add(time.Duration(index) * 10 * time.Second), Duration: 10,
		})
	}
	runner := &recordingExportRunner{}
	exporter, err := NewExporter(runner, ExportConfig{FFmpegPath: "/opt/ffmpeg", OutputDir: t.TempDir(), QueueSize: 1})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	go exporter.Run(ctx)
	task, err := exporter.Create(ExportRequest{
		SessionID: "session", DurationSeconds: 130, Layout: "single", Cameras: []string{"front"}, Quality: "standard",
	}, source, func() {})
	if err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(2 * time.Second)
	for task.State != ExportCompleted {
		if time.Now().After(deadline) {
			t.Fatalf("task state = %s", task.State)
		}
		time.Sleep(5 * time.Millisecond)
		task, err = exporter.Task(task.ID)
		if err != nil {
			t.Fatal(err)
		}
	}
	runner.mu.Lock()
	calls := slices.Clone(runner.calls)
	runner.mu.Unlock()
	if len(calls) != 14 {
		t.Fatalf("runner calls = %d, want 13 segments and 1 remux", len(calls))
	}
	if joined := strings.Join(calls[len(calls)-1], " "); !strings.Contains(joined, "-f concat") || !strings.Contains(joined, "-c copy") {
		t.Fatalf("final remux args = %s", joined)
	}
}

func TestExportArgsFillsMissingCompositeCameraWithBlack(t *testing.T) {
	t.Parallel()
	source := validExportSource()
	request, err := normalizeExportRequest(ExportRequest{
		SessionID: "session", DurationSeconds: 60, Layout: "composite3", Quality: "standard",
	})
	if err != nil {
		t.Fatal(err)
	}
	args, err := exportArgs(request, source, "/exports/missing.mp4")
	if err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(args, " ")
	if count := strings.Count(joined, "color=c=black"); count != 2 {
		t.Fatalf("black placeholder count = %d, args = %s", count, joined)
	}
	if !strings.Contains(joined, "shortest=0") {
		t.Fatalf("composite must not stop at the shortest camera: %s", joined)
	}
}

func TestExportHighLayoutsUseHEVCEncoderWithHardwarePipeline(t *testing.T) {
	t.Parallel()
	tests := []struct {
		layout string
		want   []string
	}{
		{layout: "composite6", want: []string{"scale_rkrga=w=842:h=544", "scale_rkrga=w=844:h=544", "layout=0_0|842_0|1686_0|0_544|842_544|1686_544"}},
		{layout: "composite4a", want: []string{"scale_rkrga=w=840:h=544", "layout=0_0|840_0|0_544|840_544"}},
		{layout: "composite4b", want: []string{"scale_rkrga=w=1264:h=816", "scale_rkrga=w=422:h=272", "layout=0_0|0_816|422_816|842_816"}},
		{layout: "composite3", want: []string{"scale_rkrga=w=1120:h=726", "scale_rkrga=w=560:h=362", "layout=0_0|0_726|560_726"}},
	}
	for _, test := range tests {
		t.Run(test.layout, func(t *testing.T) {
			t.Parallel()
			request, err := normalizeExportRequest(ExportRequest{
				SessionID: "session", DurationSeconds: 10, Layout: test.layout, Quality: "high",
			})
			if err != nil {
				t.Fatal(err)
			}
			source := sourceForCameras(request.Cameras)
			args, err := exportArgs(request, source, "/exports/layout.mp4")
			if err != nil {
				t.Fatal(err)
			}
			joined := strings.Join(args, " ")
			for _, expected := range test.want {
				if !strings.Contains(joined, expected) {
					t.Fatalf("args do not contain %q: %s", expected, joined)
				}
			}
			if !strings.Contains(joined, "-hwaccel rkmpp") || !strings.Contains(joined, "-hwaccel_output_format drm_prime") || !strings.Contains(joined, "hwdownload,format=nv12") {
				t.Fatalf("高保真多路导出必须保持 MPP 硬件解码和 RGA 缩放: %s", joined)
			}
			if strings.Contains(joined, "ceil(iw/16)") || !strings.Contains(joined, "force_original_aspect_ratio=disable") {
				t.Fatalf("高保真 HEVC 导出必须直接生成对齐画布，不能把补边编码为可见像素: %s", joined)
			}
			if !strings.Contains(joined, "-c:v hevc_rkmpp") || !strings.Contains(joined, "-tag:v hvc1") {
				t.Fatalf("高保真多路导出必须改用 HEVC: %s", joined)
			}
		})
	}
}

func TestHighExportTilesProduceAlignedVisibleCanvas(t *testing.T) {
	t.Parallel()
	tests := []struct {
		layout     string
		wantWidth  int
		wantHeight int
	}{
		{layout: "composite6", wantWidth: 2528, wantHeight: 1088},
		{layout: "composite4a", wantWidth: 1680, wantHeight: 1088},
		{layout: "composite4b", wantWidth: 1264, wantHeight: 1088},
		{layout: "composite3", wantWidth: 1120, wantHeight: 1088},
	}
	for _, test := range tests {
		t.Run(test.layout, func(t *testing.T) {
			t.Parallel()
			tiles, err := highExportTiles(test.layout)
			if err != nil {
				t.Fatal(err)
			}
			width, height := 0, 0
			for _, tile := range tiles {
				width = max(width, tile.x+tile.width)
				height = max(height, tile.y+tile.height)
			}
			if width != test.wantWidth || height != test.wantHeight || width%16 != 0 || height%16 != 0 {
				t.Fatalf("visible canvas = %dx%d, want %dx%d with 16-pixel alignment", width, height, test.wantWidth, test.wantHeight)
			}
		})
	}
}

func TestExportHighUsesConstrainedQualityAndReservesSpace(t *testing.T) {
	t.Parallel()
	request, err := normalizeExportRequest(ExportRequest{
		SessionID: "session", DurationSeconds: 60, Layout: "composite6", Quality: "high",
	})
	if err != nil {
		t.Fatal(err)
	}
	args, err := exportArgs(request, sourceForCameras(request.Cameras), "/exports/high.mp4")
	if err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(args, " ")
	for _, expected := range []string{"-c:v hevc_rkmpp", "-tag:v hvc1", "-rc_mode CQP", "-qp_init 18", "-qp_min 16", "-qp_max 20"} {
		if !strings.Contains(joined, expected) {
			t.Fatalf("high-quality args do not contain %q: %s", expected, joined)
		}
	}
	if strings.Contains(joined, "-b:v") || strings.Contains(joined, "-maxrate") {
		t.Fatalf("高保真 HEVC 导出不应被 CBR 限制: %s", joined)
	}
	if got, want := EstimateExportBytes(request), uint64(259_200_000); got != want {
		t.Fatalf("EstimateExportBytes() = %d, want %d", got, want)
	}
}

func TestExportHighSingleUsesHEVCRKMPPPath(t *testing.T) {
	t.Parallel()
	request, err := normalizeExportRequest(ExportRequest{
		SessionID: "session", DurationSeconds: 10, Layout: "single", Cameras: []string{"front"}, Quality: "high",
	})
	if err != nil {
		t.Fatal(err)
	}
	args, err := exportArgs(request, sourceForCameras(request.Cameras), "/exports/high-single.mp4")
	if err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(args, " ")
	for _, expected := range []string{"-hwaccel rkmpp", "-c:v h264_rkmpp", "scale_rkrga", "-c:v hevc_rkmpp", "-tag:v hvc1", "-rc_mode CQP", "-qp_init 18"} {
		if !strings.Contains(joined, expected) {
			t.Fatalf("单路高保真参数缺少 %q: %s", expected, joined)
		}
	}
}

func sourceForCameras(cameras []string) ExportSource {
	start := time.Date(2026, 7, 20, 12, 0, 0, 0, time.UTC)
	videos := make([]ExportVideo, 0, len(cameras))
	for _, camera := range cameras {
		videos = append(videos, ExportVideo{Camera: camera, Path: "/snapshot/" + camera + ".mp4", RecordedAt: start, Duration: 60})
	}
	return ExportSource{SessionStart: start, SessionEnd: start.Add(time.Minute), Videos: videos}
}

func validExportSource() ExportSource {
	start := time.Date(2026, 7, 20, 12, 0, 0, 0, time.UTC)
	return ExportSource{
		SessionStart: start, SessionEnd: start.Add(time.Minute),
		Videos: []ExportVideo{{Camera: "front", Path: "/snapshot/front.mp4", RecordedAt: start, Duration: 60}},
	}
}
