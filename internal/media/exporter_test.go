package media

import (
	"archive/zip"
	"context"
	"fmt"
	"io"
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
	if releases.Load() != 1 || task.ProgressPercent != 100 || !task.DownloadReady {
		t.Fatalf("releases = %d, task = %+v", releases.Load(), task)
	}
	if got, want := ExportDownloadFilename(task), "export-前视-20260720-120000.mp4"; got != want {
		t.Fatalf("ExportDownloadFilename() = %q, want %q", got, want)
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
	if len(history) != 1 || history[0].ID != task.ID || reloaded.Settings().RetainCount != 3 {
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

func TestExportOverlaySubtitlesAreEscapedAndMapped(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), ".task.tmp.ass")
	hasSubtitles, err := writeOverlaySubtitles(path, []ExportOverlayCue{{
		Start: 0, End: time.Second, Timestamp: "2026-07-30 12:00:00", SEIText: "档位 D\n时速 20.0 km/h {test}",
	}}, true, true)
	if err != nil || !hasSubtitles {
		t.Fatalf("writeOverlaySubtitles() = %t, %v", hasSubtitles, err)
	}
	content, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(content), "0:00:00.00,0:00:01.00,Timestamp") || !strings.Contains(string(content), "时速 20.0 km/h \\{test\\}") || !strings.Contains(string(content), "档位 D\\N") {
		t.Fatalf("subtitle content = %s", content)
	}
	args, err := exportArgs(validExportRequest(), validExportSource(), "/exports/task.mp4", path)
	if err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(args, " ")
	if !strings.Contains(joined, "subtitles=filename='") || !strings.Contains(joined, "-map [overlay]") {
		t.Fatalf("overlay args = %s", joined)
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
	for _, expected := range []string{"fps=36", "hwdownload,format=nv12", "-rc_mode CQP", "-qp_init 18"} {
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

func TestValidateExportRejectsTooManyHardwareDecoderInputs(t *testing.T) {
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
	if err := validateExport(request, source, 30*time.Minute); err == nil || !strings.Contains(err.Error(), "过多录像片段") {
		t.Fatalf("validateExport() error = %v", err)
	}
	if _, err := exportArgs(request, source, "/exports/too-many.mp4"); err == nil || !strings.Contains(err.Error(), "过多录像片段") {
		t.Fatalf("exportArgs() error = %v", err)
	}
}

func TestExportHighLayoutsUseHEVCEncoderWithHardwarePipeline(t *testing.T) {
	t.Parallel()
	tests := []struct {
		layout string
		want   []string
	}{
		{layout: "composite6", want: []string{"scale_rkrga=w=834:h=540", "layout=0_0|834_0|1668_0|0_540|834_540|1668_540"}},
		{layout: "composite4a", want: []string{"scale_rkrga=w=834:h=540", "layout=0_0|834_0|0_540|834_540"}},
		{layout: "composite4b", want: []string{"scale_rkrga=w=1248:h=810", "layout=0_0|0_810|416_810|832_810"}},
		{layout: "composite3", want: []string{"scale_rkrga=w=1112:h=720", "layout=0_0|0_720|556_720"}},
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
			if !strings.Contains(joined, "pad=w=ceil(iw/16)*16:h=ceil(ih/16)*16:x=0:y=0:color=black") || !strings.Contains(joined, "force_original_aspect_ratio=disable") {
				t.Fatalf("高保真 HEVC 导出必须保持内容画布并按 MPP 边界补齐: %s", joined)
			}
			if !strings.Contains(joined, "-c:v hevc_rkmpp") || !strings.Contains(joined, "-tag:v hvc1") {
				t.Fatalf("高保真多路导出必须改用 HEVC: %s", joined)
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
	if got, want := EstimateExportBytes(request), uint64(552_000_000); got != want {
		t.Fatalf("EstimateExportBytes() = %d, want %d", got, want)
	}
}

func TestExportHighSingleKeepsRKMPPPath(t *testing.T) {
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
	for _, expected := range []string{"-hwaccel rkmpp", "-c:v h264_rkmpp", "scale_rkrga", "-rc_mode CQP", "-qp_init 18"} {
		if !strings.Contains(joined, expected) {
			t.Fatalf("单路高保真参数缺少 %q: %s", expected, joined)
		}
	}
	if strings.Contains(joined, "hevc_rkmpp") {
		t.Fatalf("单路高保真不应被多路 HEVC 修复路径影响: %s", joined)
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
