package media

import (
	"archive/zip"
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/wen-ryon/teslabox/internal/recordingcache"
	"github.com/wen-ryon/teslabox/internal/system"
)

type stagedExportRunner struct {
	root    string
	started chan struct{}
	block   bool
	t       *testing.T
}

type lateCanceledOutputRunner struct {
	started, finished chan struct{}
}

func (r *lateCanceledOutputRunner) RunProgress(ctx context.Context, _ string, args []string, _ func(time.Duration)) error {
	close(r.started)
	<-ctx.Done()
	// 模拟编码器收到停止信号时最后一次输出晚于取消接口的第一次清理。
	err := os.WriteFile(args[len(args)-1], []byte("late-output"), 0o600)
	close(r.finished)
	return err
}

func (r *stagedExportRunner) RunProgress(ctx context.Context, _ string, args []string, progress func(time.Duration)) error {
	for index, arg := range args {
		if arg != "-i" {
			continue
		}
		path := filepath.FromSlash(args[index+1])
		root, err := filepath.EvalSymlinks(r.root)
		if err != nil {
			r.t.Fatal(err)
		}
		if !strings.HasPrefix(path, root+string(filepath.Separator)) {
			r.t.Errorf("导出仍在读取外部输入: path=%q root=%q", path, r.root)
		}
		if _, err := os.Stat(path); err != nil {
			r.t.Errorf("本地输入不可用: %v", err)
		}
	}
	if r.started != nil {
		close(r.started)
	}
	if r.block {
		<-ctx.Done()
		return ctx.Err()
	}
	progress(30 * time.Second)
	return os.WriteFile(args[len(args)-1], []byte("staged-export"), 0o600)
}

func stagedFixture(t *testing.T) (*recordingcache.Store, ExportConfig, ExportSource, ExportRequest, string) {
	t.Helper()
	root := t.TempDir()
	cache, err := recordingcache.New(recordingcache.Config{Root: filepath.Join(root, "recordings"), MediaRoot: root, MaxBytes: 8 << 20, MaxTotalBytes: 128 << 20, MinFreeBytes: 1, IdleTTL: 24 * time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	snapshot := t.TempDir()
	start := time.Date(2026, 10, 2, 8, 0, 0, 0, time.UTC)
	source := ExportSource{SessionStart: start, SessionEnd: start.Add(2 * time.Minute), SourceIdentity: "disk-volume-generation", SnapshotVersion: "snapshot-1", SourceRoot: snapshot}
	for _, spec := range []struct {
		camera, name string
		offset       time.Duration
	}{{"front", "front-1.mp4", 0}, {"front", "front-2.mp4", time.Minute}, {"back", "back-1.mp4", 0}} {
		path := filepath.Join(snapshot, spec.name)
		if err := os.WriteFile(path, []byte(spec.name), 0o600); err != nil {
			t.Fatal(err)
		}
		source.Videos = append(source.Videos, ExportVideo{Camera: spec.camera, Path: path, RecordedAt: start.Add(spec.offset), WallClockAt: start.Add(spec.offset), Duration: 60})
	}
	request := ExportRequest{SessionID: "event", DurationSeconds: 30, Layout: "single", Cameras: []string{"front"}, Quality: "standard"}
	cfg := ExportConfig{FFmpegPath: "/opt/ffmpeg", OutputDir: filepath.Join(root, "exports"), StatePath: filepath.Join(root, "state.json"), QueueSize: 2, Cache: cache}
	return cache, cfg, source, request, snapshot
}

func awaitExport(t *testing.T, e *Exporter, id string, state ExportState) ExportTask {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		task, err := e.Task(id)
		if err != nil {
			t.Fatal(err)
		}
		if task.State == state {
			return task
		}
		if task.State == ExportFailed && state != ExportFailed {
			t.Fatalf("任务失败: %+v", task)
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("任务 %s 未到达 %s", id, state)
	return ExportTask{}
}

func TestStagedExportReleasesSnapshotBeforeEncodingAndKeepsTimeline(t *testing.T) {
	_, cfg, source, request, _ := stagedFixture(t)
	runner := &stagedExportRunner{root: filepath.Dir(cfg.OutputDir), started: make(chan struct{}), block: true, t: t}
	e, err := NewExporter(runner, cfg)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	go e.Run(ctx)
	defer func() {
		cancel()
		if err := e.Wait(t.Context()); err != nil {
			t.Error(err)
		}
	}()
	mediaCtx, cancelMedia := context.WithCancel(t.Context())
	source.MediaContext = mediaCtx
	var released atomic.Int32
	task, err := e.Create(request, source, func() {
		released.Add(1)
		cancelMedia()
		for _, v := range source.Videos {
			_ = os.Remove(v.Path)
		}
	})
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-runner.started:
	case <-time.After(5 * time.Second):
		t.Fatal("未进入本地导出")
	}
	if released.Load() != 1 {
		t.Fatalf("编码开始时快照未释放: %d", released.Load())
	}
	e.mu.RLock()
	local := e.preparedSources[task.ID]
	e.mu.RUnlock()
	if local.SessionStart != source.SessionStart || local.SessionEnd != source.SessionEnd || len(local.Videos) != len(source.Videos) {
		t.Fatal("暂存改变了完整时间轴")
	}
	if local.Videos[0].Path == "" || local.Videos[1].Path != "" || local.Videos[2].Path != "" {
		t.Fatalf("复制了未选择的片段／视角: %+v", local.Videos)
	}
	if _, err := e.Cancel(task.ID); err != nil {
		t.Fatal(err)
	}
}

func TestPreparedExportRecoversAfterRestartWithoutSource(t *testing.T) {
	_, cfg, source, request, _ := stagedFixture(t)
	first := &stagedExportRunner{root: filepath.Dir(cfg.OutputDir), started: make(chan struct{}), block: true, t: t}
	e, err := NewExporter(first, cfg)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	go e.Run(ctx)
	task, err := e.Create(request, source, func() {
		for _, v := range source.Videos {
			_ = os.Remove(v.Path)
		}
	})
	if err != nil {
		cancel()
		t.Fatal(err)
	}
	select {
	case <-first.started:
	case <-time.After(5 * time.Second):
		cancel()
		t.Fatal("暂存任务未开始编码")
	}
	cancel()
	if err := e.Wait(t.Context()); err != nil {
		t.Fatal(err)
	}
	queued, err := e.Task(task.ID)
	if err != nil || queued.State != ExportQueued {
		t.Fatalf("停止后没有保留恢复计划: %+v %v", queued, err)
	}
	cfg.Cache, err = recordingcache.New(recordingcache.Config{Root: filepath.Join(filepath.Dir(cfg.OutputDir), "recordings"), MediaRoot: filepath.Dir(cfg.OutputDir), MaxBytes: 8 << 20, MaxTotalBytes: 128 << 20, MinFreeBytes: 1, IdleTTL: 24 * time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	recovered, err := NewExporter(&stagedExportRunner{root: filepath.Dir(cfg.OutputDir), t: t}, cfg)
	if err != nil {
		t.Fatal(err)
	}
	restartCtx, stop := context.WithCancel(t.Context())
	go recovered.Run(restartCtx)
	defer func() {
		stop()
		if err := recovered.Wait(t.Context()); err != nil {
			t.Error(err)
		}
	}()
	finished := awaitExport(t, recovered, task.ID, ExportCompleted)
	if !finished.DownloadReady {
		t.Fatal("恢复任务没有生成成品")
	}
	data, err := os.ReadFile(recovered.outputPathForTask(finished))
	if err != nil || string(data) != "staged-export" {
		t.Fatalf("成品 %q %v", data, err)
	}
}

func TestExportSelectionIncludesSEIDependency(t *testing.T) {
	_, _, source, request, _ := stagedFixture(t)
	request.Cameras = []string{"back"}
	request.BurnSEI = true
	paths, err := selectedExportPaths(request, source)
	if err != nil {
		t.Fatal(err)
	}
	if len(paths) != 2 {
		t.Fatalf("后视输出应保留前视元数据来源，不复制其他分钟: %v", paths)
	}
}

func TestPreparationReleasesNextSnapshotWhileEncoderBusy(t *testing.T) {
	_, cfg, source, request, _ := stagedFixture(t)
	runner := &stagedExportRunner{root: filepath.Dir(cfg.OutputDir), started: make(chan struct{}), block: true, t: t}
	e, err := NewExporter(runner, cfg)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	go e.Run(ctx)
	defer func() {
		cancel()
		if err := e.Wait(t.Context()); err != nil {
			t.Error(err)
		}
	}()
	first, err := e.Create(request, source, func() {})
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-runner.started:
	case <-time.After(5 * time.Second):
		t.Fatal("首任务未开始编码")
	}
	released := make(chan struct{})
	second, err := e.Create(request, source, func() { close(released) })
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-released:
	case <-time.After(5 * time.Second):
		t.Fatal("后续准备被长编码阻塞，仍持有快照")
	}
	queued := awaitExport(t, e, second.ID, ExportQueued)
	if queued.Phase != "prepared" {
		t.Fatalf("后续输入未准备: %+v", queued)
	}
	if _, err := e.Cancel(second.ID); err != nil {
		t.Fatal(err)
	}
	body, err := os.ReadFile(cfg.StatePath)
	if err != nil {
		t.Fatal(err)
	}
	var state exportState
	if err := json.Unmarshal(body, &state); err != nil {
		t.Fatal(err)
	}
	found := false
	for _, item := range state.Tasks {
		if item.ID == second.ID {
			found = true
			if item.State != ExportCanceled || item.Source != nil {
				t.Fatalf("取消任务仍可在重启后执行: %+v", item)
			}
		}
	}
	if !found {
		t.Fatal("取消状态未持久化")
	}
	_, _ = e.Cancel(first.ID)
}

func TestStagedSourceArchiveAndDownloadProtection(t *testing.T) {
	_, cfg, source, _, snapshot := stagedFixture(t)
	video := source.Videos[0]
	content, err := os.ReadFile(video.Path)
	if err != nil {
		t.Fatal(err)
	}
	source.SourceFiles = []SourceExportFile{{MomentID: "minute-1", Camera: "front", Name: filepath.Base(video.Path), Path: video.Path, RecordedAt: video.RecordedAt, Size: uint64(len(content))}}
	source.Videos = nil
	e, err := NewExporter(&fakeExportRunner{}, cfg)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	go e.Run(ctx)
	defer func() { cancel(); _ = e.Wait(t.Context()) }()
	task, err := e.Create(ExportRequest{Mode: ExportModeSource, SessionID: "event", MomentIDs: []string{"minute-1"}, Cameras: []string{"front"}}, source, func() {
		if err := os.RemoveAll(snapshot); err != nil {
			t.Error(err)
		}
	})
	if err != nil {
		t.Fatal(err)
	}
	task = awaitExport(t, e, task.ID, ExportCompleted)
	archive, err := zip.OpenReader(e.outputPathForTask(task))
	if err != nil {
		t.Fatal(err)
	}
	var actual []byte
	for _, item := range archive.File {
		if item.Name == filepath.Base(video.Path) {
			reader, err := item.Open()
			if err != nil {
				t.Fatal(err)
			}
			actual, err = io.ReadAll(reader)
			_ = reader.Close()
			if err != nil {
				t.Fatal(err)
			}
		}
	}
	_ = archive.Close()
	if string(actual) != string(content) {
		t.Fatalf("原画字节变化: %q", actual)
	}
	file, _, err := e.Open(task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := e.Delete(task.ID); err == nil {
		t.Fatal("下载期间允许删除成品")
	}
	_ = file.Close()
	e.CloseDownload(task.ID)
	if err := e.Delete(task.ID); err != nil {
		t.Fatal(err)
	}
}

func TestCacheSettingsPreserveOmittedAndAcceptZero(t *testing.T) {
	_, cfg, _, _, _ := stagedFixture(t)
	e, err := NewExporter(&fakeExportRunner{}, cfg)
	if err != nil {
		t.Fatal(err)
	}
	settings, err := e.UpdateSettings(ExportSettings{RetainCount: 3})
	if err != nil {
		t.Fatal(err)
	}
	if settings.CacheIdleHours == nil || *settings.CacheIdleHours != 24 {
		t.Fatalf("省略覆盖了留存: %+v", settings)
	}
	zero := 0
	settings, err = e.UpdateSettings(ExportSettings{RetainCount: 3, CacheIdleHours: &zero})
	if err != nil {
		t.Fatal(err)
	}
	if *settings.CacheIdleHours != 0 || cfg.Cache.Policy().IdleTTL != 0 {
		t.Fatal("零留存没有生效")
	}
	negative := -1
	if _, err = e.UpdateSettings(ExportSettings{RetainCount: 3, CacheIdleHours: &negative}); err == nil {
		t.Fatal("接受了负留存")
	}
	settings.CacheIdleHours = new(int)
	*settings.CacheIdleHours = 168
	if *e.Settings().CacheIdleHours != 0 {
		t.Fatal("调用者能修改服务内部设置")
	}
}

func TestSystemSpacePressureStopsEncodingWithActionableError(t *testing.T) {
	_, cfg, source, request, _ := stagedFixture(t)
	runner := &stagedExportRunner{root: filepath.Dir(cfg.OutputDir), started: make(chan struct{}), block: true, t: t}
	e, err := NewExporter(runner, cfg)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	go e.Run(ctx)
	defer func() { cancel(); _ = e.Wait(t.Context()) }()
	task, err := e.Create(request, source, func() {})
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-runner.started:
	case <-time.After(5 * time.Second):
		t.Fatal("编码未开始")
	}
	// 模拟其他媒体任务增长，仅在测试目录中用稀疏文件增加逻辑占用。
	file, err := os.Create(filepath.Join(filepath.Dir(cfg.OutputDir), "other-media.bin"))
	if err != nil {
		t.Fatal(err)
	}
	err = file.Truncate(129 << 20)
	_ = file.Close()
	if err != nil {
		t.Fatal(err)
	}
	task = awaitExport(t, e, task.ID, ExportFailed)
	if !strings.Contains(task.Error, "系统盘空间不足") {
		t.Fatalf("丢失空间保护原因: %+v", task)
	}
}

func TestRestartRejectsMissingOrTruncatedSystemDiskOutput(t *testing.T) {
	for _, name := range []string{"missing", "truncated"} {
		t.Run(name, func(t *testing.T) {
			_, cfg, source, request, _ := stagedFixture(t)
			e, err := NewExporter(&stagedExportRunner{root: filepath.Dir(cfg.OutputDir), t: t}, cfg)
			if err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithCancel(t.Context())
			go e.Run(ctx)
			task, err := e.Create(request, source, func() {})
			if err != nil {
				cancel()
				t.Fatal(err)
			}
			task = awaitExport(t, e, task.ID, ExportCompleted)
			cancel()
			if err := e.Wait(t.Context()); err != nil {
				t.Fatal(err)
			}
			if name == "missing" {
				err = os.Remove(e.outputPathForTask(task))
			} else {
				err = os.WriteFile(e.outputPathForTask(task), []byte("partial"), 0o600)
			}
			if err != nil {
				t.Fatal(err)
			}
			recovered, err := NewExporter(&fakeExportRunner{}, cfg)
			if err != nil {
				t.Fatal(err)
			}
			task, err = recovered.Task(task.ID)
			if err != nil {
				t.Fatal(err)
			}
			if task.State != ExportFailed || task.DownloadReady || !strings.Contains(task.Error, "不完整") {
				t.Fatalf("错误成品仍然可下载: %+v", task)
			}
		})
	}
}

func TestCanceledEncoderLateWriteLeavesNoOutput(t *testing.T) {
	_, cfg, source, request, _ := stagedFixture(t)
	runner := &lateCanceledOutputRunner{started: make(chan struct{}), finished: make(chan struct{})}
	e, err := NewExporter(runner, cfg)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	go e.Run(ctx)
	defer func() { cancel(); _ = e.Wait(t.Context()) }()
	task, err := e.Create(request, source, func() {})
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-runner.started:
	case <-time.After(5 * time.Second):
		t.Fatal("编码未开始")
	}
	if _, err := e.Cancel(task.ID); err != nil {
		t.Fatal(err)
	}
	select {
	case <-runner.finished:
	case <-time.After(5 * time.Second):
		t.Fatal("编码未停止")
	}
	cancel()
	if err := e.Wait(t.Context()); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{e.tempPathForTask(task), e.outputPathForTask(task)} {
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Fatalf("取消后残留输出 %s: %v", path, err)
		}
	}
	task, err = e.Task(task.ID)
	if err != nil || task.State != ExportCanceled || task.DownloadReady {
		t.Fatalf("取消终态错误: %+v %v", task, err)
	}
}

func TestRecoveredInputsArePinnedAtStartupAndVerifiedBeforeEncoding(t *testing.T) {
	_, cfg, source, request, snapshot := stagedFixture(t)
	first := &stagedExportRunner{root: filepath.Dir(cfg.OutputDir), started: make(chan struct{}), block: true, t: t}
	e, err := NewExporter(first, cfg)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	go e.Run(ctx)
	task, err := e.Create(request, source, func() { _ = os.RemoveAll(snapshot) })
	if err != nil {
		cancel()
		t.Fatal(err)
	}
	select {
	case <-first.started:
	case <-time.After(5 * time.Second):
		cancel()
		t.Fatal("首任务未准备")
	}
	cancel()
	if err := e.Wait(t.Context()); err != nil {
		t.Fatal(err)
	}
	body, err := os.ReadFile(cfg.StatePath)
	if err != nil {
		t.Fatal(err)
	}
	var state exportState
	if err := json.Unmarshal(body, &state); err != nil {
		t.Fatal(err)
	}
	var path string
	for _, item := range state.Tasks {
		if item.ID == task.ID && item.Source != nil {
			path = item.Source.Videos[0].Path
		}
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	// 保持长度不变，让启动的轻量检查通过；后台内容校验必须在启动编码之前拒绝它。
	if err := os.WriteFile(path, make([]byte, int(info.Size())), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg.Cache, err = recordingcache.New(recordingcache.Config{Root: filepath.Join(filepath.Dir(cfg.OutputDir), "recordings"), MediaRoot: filepath.Dir(cfg.OutputDir), MaxBytes: 8 << 20, MaxTotalBytes: 128 << 20, MinFreeBytes: 1, IdleTTL: 24 * time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	started := make(chan struct{})
	recovered, err := NewExporter(&stagedExportRunner{root: filepath.Dir(cfg.OutputDir), started: started, t: t}, cfg)
	if err != nil {
		t.Fatal(err)
	}
	queued, err := recovered.Task(task.ID)
	if err != nil || queued.State != ExportQueued {
		t.Fatalf("构造期间进行了全量内容校验: %+v %v", queued, err)
	}
	if err := cfg.Cache.SetIdleTTL(0); err != nil {
		t.Fatal(err)
	}
	if err := cfg.Cache.Cleanup(t.Context()); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("恢复输入在核验前被淘汰: %v", err)
	}
	restartCtx, stop := context.WithCancel(t.Context())
	go recovered.Run(restartCtx)
	defer func() { stop(); _ = recovered.Wait(t.Context()) }()
	failed := awaitExport(t, recovered, task.ID, ExportFailed)
	if failed.DownloadReady {
		t.Fatal("损坏输入仍生成可下载成品")
	}
	select {
	case <-started:
		t.Fatal("未核验的恢复输入进入编码器")
	default:
	}
}

func TestBoardRecordingSystemDiskExport(t *testing.T) {
	path := os.Getenv("TESLABOX_TEST_RECORDING")
	if path == "" {
		t.Skip("仅在板端指定只读录像后运行")
	}
	if !strings.HasPrefix(path, "/run/teslabox/snapshots/") {
		t.Fatal("板端样本必须来自只读媒体快照")
	}
	runner := system.NewOSRunner()
	inspector, err := NewInspector(runner, Config{FFmpegPath: "/opt/teslabox/media/bin/ffmpeg", FFprobePath: "/opt/teslabox/media/bin/ffprobe", Timeout: 15 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	metadata, err := inspector.Probe(t.Context(), path)
	if err != nil {
		t.Fatal(err)
	}
	duration, err := strconv.ParseFloat(metadata.Format.Duration, 64)
	if err != nil || duration < 3 {
		t.Fatalf("样本时长无效: %+v", metadata.Format)
	}
	root := t.TempDir()
	cache, err := recordingcache.New(recordingcache.Config{Root: filepath.Join(root, "recordings"), MediaRoot: root, IdleTTL: 24 * time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	cfg := ExportConfig{FFmpegPath: "/opt/teslabox/media/bin/ffmpeg", OutputDir: filepath.Join(root, "exports"), StatePath: filepath.Join(root, "state.json"), QueueSize: 2, Cache: cache}
	e, err := NewExporter(runner, cfg)
	if err != nil {
		t.Fatal(err)
	}
	start := time.Date(2026, 9, 29, 21, 37, 0, 0, time.Local)
	source := ExportSource{SourceIdentity: "board-validation", SnapshotVersion: filepath.Dir(path), SourceRoot: filepath.Dir(path), SessionStart: start, SessionEnd: start.Add(time.Duration(duration * float64(time.Second))), Videos: []ExportVideo{{Camera: "front", Path: path, RecordedAt: start, WallClockAt: start, Duration: duration}}}
	request := ExportRequest{SessionID: "board-validation", DurationSeconds: 3, Layout: "single", Cameras: []string{"front"}, Quality: "smooth"}
	ctx, cancel := context.WithCancel(t.Context())
	go e.Run(ctx)
	defer func() {
		cancel()
		if err := e.Wait(t.Context()); err != nil {
			t.Error(err)
		}
	}()
	var released atomic.Int32
	task, err := e.Create(request, source, func() { released.Add(1) })
	if err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(90 * time.Second)
	for time.Now().Before(deadline) {
		task, err = e.Task(task.ID)
		if err != nil {
			t.Fatal(err)
		}
		if task.State == ExportFailed {
			t.Fatalf("实机导出失败: %+v", task)
		}
		if task.State == ExportCompleted {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	if task.State != ExportCompleted || released.Load() != 1 {
		t.Fatalf("导出未完成或快照未释放: %+v release=%d", task, released.Load())
	}
	output, err := inspector.Probe(t.Context(), e.outputPathForTask(task))
	if err != nil {
		t.Fatal(err)
	}
	seconds, err := strconv.ParseFloat(output.Format.Duration, 64)
	if err != nil || seconds < 2.9 || seconds > 3.2 {
		t.Fatalf("成品时长 %q", output.Format.Duration)
	}
	t.Logf("真实录像 %s；系统盘成品 %d bytes，时长 %.3f 秒；快照释放 %d 次", filepath.Base(path), task.OutputBytes, seconds, released.Load())
}
