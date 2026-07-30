package media

import (
	"archive/zip"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
)

var (
	// ErrExportQueueFull 表示设备正在处理过多导出任务，调用方应稍后重试。
	ErrExportQueueFull = errors.New("导出队列已满")
	// ErrExportNotFound 表示任务不存在或已经被清理。
	ErrExportNotFound = errors.New("导出任务不存在")
)

// ExportState 是异步导出任务对外暴露的稳定状态。
type ExportState string

const (
	ExportQueued           ExportState = "queued"
	ExportRunning          ExportState = "running"
	ExportCompleted        ExportState = "completed"
	ExportFailed           ExportState = "failed"
	ExportCanceled         ExportState = "canceled"
	maxExportHistory                   = 8
	maxExportDecoderInputs             = 12
	maxSourceExportFiles               = 120
	maxSourceExportBytes   uint64      = 4 * 1024 * 1024 * 1024
	maxSourceExportHistory             = 2
)

// ExportMode 标识导出任务是重新编码剪辑还是归档原始片段。
type ExportMode string

const (
	ExportModeClip   ExportMode = "clip"
	ExportModeSource ExportMode = "source"
)

// ExportRequest 只接受结构化选项，避免把 FFmpeg 参数暴露给客户端。
type ExportRequest struct {
	Mode            ExportMode `json:"mode,omitempty"`
	SessionID       string     `json:"session_id"`
	MomentIDs       []string   `json:"moment_ids,omitempty"`
	StartSeconds    float64    `json:"start_seconds"`
	DurationSeconds float64    `json:"duration_seconds"`
	Layout          string     `json:"layout"`
	Cameras         []string   `json:"cameras"`
	Quality         string     `json:"quality"`
}

// ExportVideo 是已通过快照边界校验的单个输入片段。
type ExportVideo struct {
	Camera     string
	Path       string
	RecordedAt time.Time
	Duration   float64
}

// SourceExportFile 是已通过快照边界校验、可直接归档的原始录像文件。
type SourceExportFile struct {
	MomentID   string
	Camera     string
	Name       string
	Path       string
	RecordedAt time.Time
	Size       uint64
}

// ExportSource 固定任务创建时的只读快照和完整录像时间轴。
type ExportSource struct {
	SessionStart time.Time
	SessionEnd   time.Time
	Videos       []ExportVideo
	SourceFiles  []SourceExportFile
}

// ExportTask 是不包含板端文件路径的任务状态。
type ExportTask struct {
	ID              string        `json:"id"`
	State           ExportState   `json:"state"`
	Request         ExportRequest `json:"request"`
	ProgressPercent int           `json:"progress_percent"`
	CreatedAt       time.Time     `json:"created_at"`
	StartedAt       *time.Time    `json:"started_at,omitempty"`
	CompletedAt     *time.Time    `json:"completed_at,omitempty"`
	Error           string        `json:"error,omitempty"`
	DownloadReady   bool          `json:"download_ready"`
	EstimatedBytes  uint64        `json:"estimated_bytes"`
	FileCount       int           `json:"file_count,omitempty"`
}

// ExportConfig 限制导出路径、队列和单任务资源占用。
type ExportConfig struct {
	FFmpegPath  string
	OutputDir   string
	QueueSize   int
	MaxDuration time.Duration
	Logger      *slog.Logger
}

type exportRunner interface {
	RunProgress(context.Context, string, []string, func(time.Duration)) error
}

type exportJob struct {
	id      string
	request ExportRequest
	source  ExportSource
	release func()
}

// Exporter 用单工作进程串行化硬件编码，避免多个任务争抢 MPP/RGA。
type Exporter struct {
	runner exportRunner
	config ExportConfig
	logger *slog.Logger

	prepareMu sync.Mutex
	prepared  bool
	mu        sync.RWMutex
	tasks     map[string]ExportTask
	cancels   map[string]context.CancelFunc
	releases  map[string]func()
	queue     chan exportJob
	runOnce   sync.Once
	done      chan struct{}
	stopping  bool
}

// NewExporter 在启动阶段建立独立输出目录，并清理异常退出遗留的临时文件。
func NewExporter(runner exportRunner, config ExportConfig) (*Exporter, error) {
	if runner == nil {
		return nil, errors.New("导出执行器不能为空")
	}
	if !isLinuxAbsolutePath(config.FFmpegPath) || !isHostOrLinuxAbsolutePath(config.OutputDir) {
		return nil, errors.New("FFmpeg 和导出目录必须使用绝对路径")
	}
	if config.QueueSize <= 0 || config.MaxDuration <= 0 {
		return nil, errors.New("导出队列和最大时长必须大于零")
	}
	if config.Logger == nil {
		config.Logger = slog.Default()
	}
	return &Exporter{
		runner: runner, config: config, logger: config.Logger, tasks: make(map[string]ExportTask),
		cancels: make(map[string]context.CancelFunc), releases: make(map[string]func()),
		queue: make(chan exportJob, config.QueueSize), done: make(chan struct{}),
	}, nil
}

// Run 消费有界队列；服务上下文取消时，正在运行的 FFmpeg 会被同步终止。
func (e *Exporter) Run(ctx context.Context) {
	e.runOnce.Do(func() {
		defer close(e.done)
		e.run(ctx)
	})
}

func (e *Exporter) run(ctx context.Context) {
	for {
		if ctx.Err() != nil {
			e.stopQueued()
			return
		}
		select {
		case <-ctx.Done():
			e.stopQueued()
			return
		case job := <-e.queue:
			e.runJob(ctx, job)
		}
	}
}

// Wait 等待工作进程释放 FFmpeg 和快照租约，避免退出清理与仍在运行的导出并发。
func (e *Exporter) Wait(ctx context.Context) error {
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-e.done:
		return nil
	}
}

// Create 校验请求后接管快照释放函数，直到任务完成、失败或取消。
func (e *Exporter) Create(request ExportRequest, source ExportSource, release func()) (ExportTask, error) {
	e.pruneFinished()
	if release == nil {
		release = func() {}
	}
	normalized, err := normalizeExportRequest(request)
	if err != nil {
		release()
		return ExportTask{}, err
	}
	request = normalized
	if err := validateExport(request, source, e.config.MaxDuration); err != nil {
		release()
		return ExportTask{}, err
	}
	if err := e.prepareOutput(); err != nil {
		release()
		return ExportTask{}, err
	}
	id, err := exportID()
	if err != nil {
		release()
		return ExportTask{}, err
	}
	var releaseOnce sync.Once
	safeRelease := func() { releaseOnce.Do(release) }
	task := ExportTask{
		ID: id, State: ExportQueued, Request: request, CreatedAt: time.Now(),
		EstimatedBytes: estimateExportBytes(request, source), FileCount: len(source.SourceFiles),
	}
	job := exportJob{id: id, request: request, source: source, release: safeRelease}
	e.mu.Lock()
	if e.stopping {
		e.mu.Unlock()
		safeRelease()
		return ExportTask{}, errors.New("视频导出器正在停止")
	}
	e.tasks[id] = task
	e.releases[id] = safeRelease
	select {
	case e.queue <- job:
		e.mu.Unlock()
		return task, nil
	default:
		delete(e.tasks, id)
		delete(e.releases, id)
		e.mu.Unlock()
		safeRelease()
		return ExportTask{}, ErrExportQueueFull
	}
}

func (e *Exporter) prepareOutput() error {
	e.prepareMu.Lock()
	defer e.prepareMu.Unlock()
	if err := ensureOutputFilesystem(e.config.OutputDir); err != nil {
		return err
	}
	if err := os.MkdirAll(e.config.OutputDir, 0o750); err != nil {
		return fmt.Errorf("创建导出目录: %w", err)
	}
	if e.prepared {
		return nil
	}
	entries, err := os.ReadDir(e.config.OutputDir)
	if err != nil {
		return fmt.Errorf("读取导出目录: %w", err)
	}
	for _, entry := range entries {
		if !entry.IsDir() && isExportArtifact(entry.Name()) {
			_ = os.Remove(filepath.Join(e.config.OutputDir, entry.Name()))
		}
	}
	e.prepared = true
	return nil
}

// Task 返回防御性拷贝，避免 API 修改内部状态。
func (e *Exporter) Task(id string) (ExportTask, error) {
	e.mu.RLock()
	defer e.mu.RUnlock()
	task, ok := e.tasks[id]
	if !ok {
		return ExportTask{}, ErrExportNotFound
	}
	task.Request.Cameras = slices.Clone(task.Request.Cameras)
	task.Request.MomentIDs = slices.Clone(task.Request.MomentIDs)
	return task, nil
}

// Busy 在系统升级前检查是否仍有排队或运行中的导出，避免重启留下半成品。
func (e *Exporter) Busy() bool {
	e.mu.RLock()
	defer e.mu.RUnlock()
	for _, task := range e.tasks {
		if task.State == ExportQueued || task.State == ExportRunning {
			return true
		}
	}
	return false
}

// Cancel 取消排队或运行中的任务，并删除尚未交付的输出文件。
func (e *Exporter) Cancel(id string) (ExportTask, error) {
	e.mu.Lock()
	task, ok := e.tasks[id]
	if !ok {
		e.mu.Unlock()
		return ExportTask{}, ErrExportNotFound
	}
	if task.State == ExportCompleted || task.State == ExportFailed || task.State == ExportCanceled {
		e.mu.Unlock()
		return task, nil
	}
	wasQueued := task.State == ExportQueued
	if cancel := e.cancels[id]; cancel != nil {
		cancel()
	}
	var release func()
	if wasQueued {
		release = e.releases[id]
	}
	now := time.Now()
	task.State = ExportCanceled
	task.CompletedAt = &now
	task.Error = ""
	task.DownloadReady = false
	e.tasks[id] = task
	e.mu.Unlock()
	if release != nil {
		release()
	}
	_ = os.Remove(e.outputPathForTask(task))
	_ = os.Remove(e.tempPathForTask(task))
	return task, nil
}

// Open 只允许打开已完成任务对应的固定文件名。
func (e *Exporter) Open(id string) (*os.File, ExportTask, error) {
	task, err := e.Task(id)
	if err != nil {
		return nil, ExportTask{}, err
	}
	if task.State != ExportCompleted || !task.DownloadReady {
		return nil, task, errors.New("导出文件尚未就绪")
	}
	file, err := os.Open(e.outputPathForTask(task))
	return file, task, err
}

func (e *Exporter) runJob(parent context.Context, job exportJob) {
	defer func() {
		job.release()
		e.mu.Lock()
		delete(e.releases, job.id)
		e.mu.Unlock()
		e.pruneFinished()
	}()
	e.mu.Lock()
	task, ok := e.tasks[job.id]
	if !ok || task.State == ExportCanceled {
		e.mu.Unlock()
		return
	}
	timeout := exportTimeout(job.request, job.source)
	ctx, cancel := context.WithTimeout(parent, timeout)
	e.cancels[job.id] = cancel
	now := time.Now()
	task.State = ExportRunning
	task.StartedAt = &now
	e.tasks[job.id] = task
	e.mu.Unlock()

	defer func() {
		cancel()
		e.mu.Lock()
		delete(e.cancels, job.id)
		e.mu.Unlock()
	}()

	if job.request.Mode == ExportModeSource {
		err := writeSourceArchive(ctx, job.source, e.tempPathForTask(task), func(progress int) {
			e.updateProgress(job.id, progress)
		})
		if err != nil {
			e.finishJob(job.id, task, err)
			return
		}
		if err = os.Rename(e.tempPathForTask(task), e.outputPathForTask(task)); err != nil {
			e.finishJob(job.id, task, err)
			return
		}
		e.finishJob(job.id, task, nil)
		return
	}

	args, err := exportArgs(job.request, job.source, e.tempPathForTask(task))
	if err == nil {
		err = e.runner.RunProgress(ctx, e.config.FFmpegPath, args, func(position time.Duration) {
			progress := min(99, max(0, int(position.Seconds()/job.request.DurationSeconds*100)))
			e.updateProgress(job.id, progress)
		})
	}
	if err == nil {
		err = os.Rename(e.tempPathForTask(task), e.outputPathForTask(task))
	}
	e.finishJob(job.id, task, err)
}

func (e *Exporter) finishJob(id string, task ExportTask, err error) {
	if err != nil {
		_ = os.Remove(e.tempPathForTask(task))
		if !errors.Is(err, context.Canceled) {
			e.logger.Warn("视频导出任务失败", "task_id", id, "error", err)
		}
	}

	e.mu.Lock()
	task = e.tasks[id]
	finished := time.Now()
	if task.State != ExportCanceled {
		if err != nil {
			task.State = ExportFailed
			task.Error = "导出失败"
		} else {
			task.State = ExportCompleted
			task.ProgressPercent = 100
			task.DownloadReady = true
		}
		task.CompletedAt = &finished
		e.tasks[id] = task
	}
	e.mu.Unlock()
}

func (e *Exporter) updateProgress(id string, progress int) {
	e.mu.Lock()
	defer e.mu.Unlock()
	task, ok := e.tasks[id]
	if ok && task.State == ExportRunning && progress > task.ProgressPercent {
		task.ProgressPercent = progress
		e.tasks[id] = task
	}
}

func (e *Exporter) stopQueued() {
	e.mu.Lock()
	e.stopping = true
	for _, cancel := range e.cancels {
		cancel()
	}
	releases := make([]func(), 0, len(e.releases))
	for id, release := range e.releases {
		task := e.tasks[id]
		if task.State == ExportQueued || task.State == ExportRunning {
			wasQueued := task.State == ExportQueued
			now := time.Now()
			task.State = ExportCanceled
			task.CompletedAt = &now
			e.tasks[id] = task
			// 运行任务必须等 FFmpeg 真正退出后由 runJob 释放；排队任务不会再被消费，可立即归还租约。
			if wasQueued {
				releases = append(releases, release)
			}
		}
	}
	e.mu.Unlock()
	for _, release := range releases {
		release()
	}
}

func (e *Exporter) pruneFinished() {
	e.mu.Lock()
	finished := make([]ExportTask, 0, len(e.tasks))
	for id, task := range e.tasks {
		if _, retained := e.releases[id]; retained {
			continue
		}
		if task.State == ExportCompleted || task.State == ExportFailed || task.State == ExportCanceled {
			finished = append(finished, task)
		}
	}
	slices.SortFunc(finished, func(left, right ExportTask) int { return right.CreatedAt.Compare(left.CreatedAt) })
	staleByID := make(map[string]ExportTask)
	if len(finished) > maxExportHistory {
		for _, task := range finished[maxExportHistory:] {
			staleByID[task.ID] = task
		}
	}
	sourceKept := 0
	for _, task := range finished {
		if task.Request.Mode != ExportModeSource {
			continue
		}
		sourceKept++
		if sourceKept > maxSourceExportHistory {
			staleByID[task.ID] = task
		}
	}
	if len(staleByID) == 0 {
		e.mu.Unlock()
		return
	}
	stale := make([]ExportTask, 0, len(staleByID))
	for _, task := range staleByID {
		stale = append(stale, task)
	}
	for _, task := range stale {
		delete(e.tasks, task.ID)
	}
	e.mu.Unlock()
	for _, task := range stale {
		_ = os.Remove(e.outputPathForTask(task))
		_ = os.Remove(e.tempPathForTask(task))
	}
}

func (e *Exporter) outputPath(id string) string { return filepath.Join(e.config.OutputDir, id+".mp4") }
func (e *Exporter) tempPath(id string) string {
	return filepath.Join(e.config.OutputDir, "."+id+".tmp.mp4")
}

func (e *Exporter) outputPathForTask(task ExportTask) string {
	if task.Request.Mode == ExportModeSource {
		return filepath.Join(e.config.OutputDir, task.ID+".zip")
	}
	return e.outputPath(task.ID)
}

func (e *Exporter) tempPathForTask(task ExportTask) string {
	if task.Request.Mode == ExportModeSource {
		return filepath.Join(e.config.OutputDir, "."+task.ID+".tmp.zip")
	}
	return e.tempPath(task.ID)
}

func validateExport(request ExportRequest, source ExportSource, maximum time.Duration) error {
	if request.Mode == ExportModeSource {
		return validateSourceExport(request, source)
	}
	if strings.TrimSpace(request.SessionID) == "" || source.SessionStart.IsZero() || !source.SessionEnd.After(source.SessionStart) {
		return &InputError{Message: "导出录像不存在"}
	}
	if request.StartSeconds < 0 || request.DurationSeconds <= 0 || request.DurationSeconds > maximum.Seconds() {
		return &InputError{Message: fmt.Sprintf("导出时长必须大于 0 且不超过 %.0f 分钟", maximum.Minutes())}
	}
	if request.StartSeconds+request.DurationSeconds > source.SessionEnd.Sub(source.SessionStart).Seconds()+0.01 {
		return &InputError{Message: "导出范围超过完整录像时长"}
	}
	if request.Quality != "smooth" && request.Quality != "standard" && request.Quality != "high" && request.Quality != "original" {
		return &InputError{Message: "不支持的导出画质"}
	}
	if request.Quality == "original" && request.Layout != "single" {
		return &InputError{Message: "原画导出仅支持单路视角；多路合成最高支持 1080p"}
	}
	for _, camera := range request.Cameras {
		if !slices.Contains(knownCameras(), camera) {
			return &InputError{Message: "包含未知摄像头视角"}
		}
		exportStart := source.SessionStart.Add(time.Duration(request.StartSeconds * float64(time.Second)))
		exportEnd := exportStart.Add(time.Duration(request.DurationSeconds * float64(time.Second)))
		if !slices.ContainsFunc(source.Videos, func(video ExportVideo) bool {
			videoEnd := video.RecordedAt.Add(time.Duration(video.Duration * float64(time.Second)))
			return video.Camera == camera && video.RecordedAt.Before(exportEnd) && videoEnd.After(exportStart)
		}) {
			return &InputError{Message: "选择的视角在导出范围内没有录像"}
		}
	}
	if exportDecoderInputCount(request, source) > maxExportDecoderInputs {
		return &InputError{Message: "导出范围包含过多录像片段，请缩短时间范围后重试"}
	}
	return nil
}

func exportDecoderInputCount(request ExportRequest, source ExportSource) int {
	exportStart := source.SessionStart.Add(time.Duration(request.StartSeconds * float64(time.Second)))
	exportEnd := exportStart.Add(time.Duration(request.DurationSeconds * float64(time.Second)))
	count := 0
	for _, camera := range request.Cameras {
		videos := slices.DeleteFunc(slices.Clone(source.Videos), func(video ExportVideo) bool { return video.Camera != camera })
		slices.SortFunc(videos, func(left, right ExportVideo) int { return left.RecordedAt.Compare(right.RecordedAt) })
		cursor := exportStart
		for _, video := range videos {
			videoEnd := video.RecordedAt.Add(time.Duration(video.Duration * float64(time.Second)))
			start := maxTime(exportStart, video.RecordedAt, cursor)
			end := minTime(exportEnd, videoEnd)
			if !end.After(start) || !isLinuxAbsolutePath(video.Path) {
				continue
			}
			count++
			cursor = end
		}
	}
	return count
}

func normalizeExportRequest(request ExportRequest) (ExportRequest, error) {
	mode := request.Mode
	if mode == "" {
		mode = ExportModeClip
	}
	request.Mode = mode
	if mode == ExportModeSource {
		return normalizeSourceExportRequest(request)
	}
	if mode != ExportModeClip {
		return ExportRequest{}, &InputError{Message: "不支持的导出方式"}
	}
	var cameras []string
	switch request.Layout {
	case "composite6":
		cameras = []string{"left_pillar", "front", "right_pillar", "left_repeater", "back", "right_repeater"}
	case "composite4a":
		cameras = []string{"front", "back", "left_repeater", "right_repeater"}
	case "composite4b":
		cameras = []string{"front", "right_repeater", "back", "left_repeater"}
	case "composite3":
		cameras = []string{"front", "right_repeater", "left_repeater"}
	case "single":
		if len(request.Cameras) != 1 || !slices.Contains(knownCameras(), request.Cameras[0]) {
			return ExportRequest{}, &InputError{Message: "请选择一个有效的单路视角"}
		}
		cameras = []string{request.Cameras[0]}
	default:
		return ExportRequest{}, &InputError{Message: "不支持的导出视角布局"}
	}
	request.Cameras = cameras
	return request, nil
}

func normalizeSourceExportRequest(request ExportRequest) (ExportRequest, error) {
	seenMoments := make(map[string]struct{}, len(request.MomentIDs))
	momentIDs := make([]string, 0, len(request.MomentIDs))
	for _, momentID := range request.MomentIDs {
		momentID = strings.TrimSpace(momentID)
		if momentID == "" {
			continue
		}
		if _, found := seenMoments[momentID]; found {
			continue
		}
		seenMoments[momentID] = struct{}{}
		momentIDs = append(momentIDs, momentID)
	}
	seenCameras := make(map[string]struct{}, len(request.Cameras))
	cameras := make([]string, 0, len(request.Cameras))
	for _, camera := range request.Cameras {
		if !slices.Contains(knownCameras(), camera) {
			return ExportRequest{}, &InputError{Message: "包含未知摄像头视角"}
		}
		if _, found := seenCameras[camera]; found {
			continue
		}
		seenCameras[camera] = struct{}{}
		cameras = append(cameras, camera)
	}
	request.MomentIDs = momentIDs
	request.Cameras = cameras
	request.Layout = ""
	request.Quality = ""
	request.StartSeconds = 0
	request.DurationSeconds = 0
	return request, nil
}

func validateSourceExport(request ExportRequest, source ExportSource) error {
	if strings.TrimSpace(request.SessionID) == "" || len(request.MomentIDs) == 0 {
		return &InputError{Message: "请至少选择一个原始录像分钟"}
	}
	if len(request.Cameras) == 0 {
		return &InputError{Message: "请至少选择一个摄像头视角"}
	}
	if len(source.SourceFiles) == 0 {
		return &InputError{Message: "所选分钟和视角没有可用的原始录像"}
	}
	if len(source.SourceFiles) > maxSourceExportFiles {
		return &InputError{Message: "原始片段过多，请缩短选择范围后重试"}
	}
	var total uint64
	for _, file := range source.SourceFiles {
		if file.MomentID == "" || !slices.Contains(request.Cameras, file.Camera) ||
			!isHostOrLinuxAbsolutePath(file.Path) || filepath.Base(file.Name) != file.Name || !strings.HasSuffix(strings.ToLower(file.Name), ".mp4") {
			return &InputError{Message: "原始录像文件无效"}
		}
		if maxSourceExportBytes-total < file.Size {
			return &InputError{Message: "原始片段过大，请缩短选择范围后重试"}
		}
		total += file.Size
	}
	return nil
}

// EstimateExportBytes 按目标码率加封装余量估算空间，避免导出侵占车端录像保留容量。
func EstimateExportBytes(request ExportRequest) uint64 {
	bitrateKbps := map[string]uint64{"smooth": 1500, "standard": 3000, "high": 8000, "original": 20000}[request.Quality]
	if bitrateKbps == 0 || request.DurationSeconds <= 0 {
		return 0
	}
	return uint64(float64(bitrateKbps*1000)/8*request.DurationSeconds*1.15 + 0.5)
}

// EstimateSourceExportBytes 返回原始片段打包时需要预留的目标文件空间。
func EstimateSourceExportBytes(source ExportSource) uint64 {
	var total uint64
	for _, file := range source.SourceFiles {
		if ^uint64(0)-total < file.Size {
			return ^uint64(0)
		}
		total += file.Size
	}
	return total
}

func estimateExportBytes(request ExportRequest, source ExportSource) uint64 {
	if request.Mode == ExportModeSource {
		return EstimateSourceExportBytes(source)
	}
	return EstimateExportBytes(request)
}

func knownCameras() []string {
	return []string{"left_pillar", "front", "right_pillar", "left_repeater", "back", "right_repeater"}
}

func exportID() (string, error) {
	value := make([]byte, 12)
	if _, err := rand.Read(value); err != nil {
		return "", fmt.Errorf("生成导出任务编号: %w", err)
	}
	return hex.EncodeToString(value), nil
}

func isExportArtifact(name string) bool {
	id := ""
	if strings.HasPrefix(name, ".") && strings.HasSuffix(name, ".tmp.mp4") {
		id = strings.TrimSuffix(strings.TrimPrefix(name, "."), ".tmp.mp4")
	} else if strings.HasPrefix(name, ".") && strings.HasSuffix(name, ".tmp.zip") {
		id = strings.TrimSuffix(strings.TrimPrefix(name, "."), ".tmp.zip")
	} else if strings.HasSuffix(name, ".mp4") {
		id = strings.TrimSuffix(name, ".mp4")
	} else if strings.HasSuffix(name, ".zip") {
		id = strings.TrimSuffix(name, ".zip")
	}
	decoded, err := hex.DecodeString(id)
	return err == nil && len(decoded) == 12
}

func exportTimeout(request ExportRequest, source ExportSource) time.Duration {
	if request.Mode != ExportModeSource {
		timeout := time.Duration(request.DurationSeconds*3*float64(time.Second)) + time.Minute
		return max(timeout, 2*time.Minute)
	}
	const minimumSourceWriteRate = 5 * 1024 * 1024
	timeout := 2*time.Minute + time.Duration(EstimateSourceExportBytes(source)/minimumSourceWriteRate)*time.Second
	return min(timeout, 30*time.Minute)
}

type sourceArchiveManifest struct {
	Format      string                      `json:"format"`
	ExportedAt  time.Time                   `json:"exported_at"`
	SourceFiles []sourceArchiveManifestFile `json:"source_files"`
}

type sourceArchiveManifestFile struct {
	MomentID   string    `json:"moment_id"`
	Camera     string    `json:"camera"`
	Name       string    `json:"name"`
	RecordedAt time.Time `json:"recorded_at"`
	Size       uint64    `json:"size"`
}

func writeSourceArchive(ctx context.Context, source ExportSource, output string, updateProgress func(int)) error {
	file, err := os.OpenFile(output, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o640)
	if err != nil {
		return fmt.Errorf("创建原始录像压缩包: %w", err)
	}
	archive := zip.NewWriter(file)
	completed := false
	defer func() {
		if completed {
			return
		}
		_ = archive.Close()
		_ = file.Close()
	}()
	manifest := sourceArchiveManifest{Format: "teslabox-source-export/v1", ExportedAt: time.Now(), SourceFiles: make([]sourceArchiveManifestFile, 0, len(source.SourceFiles))}
	for _, sourceFile := range source.SourceFiles {
		manifest.SourceFiles = append(manifest.SourceFiles, sourceArchiveManifestFile{
			MomentID: sourceFile.MomentID, Camera: sourceFile.Camera, Name: sourceFile.Name,
			RecordedAt: sourceFile.RecordedAt, Size: sourceFile.Size,
		})
	}
	manifestBody, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		return fmt.Errorf("生成原始录像清单: %w", err)
	}
	manifestWriter, err := archive.CreateHeader(&zip.FileHeader{Name: "manifest.json", Method: zip.Store, Modified: manifest.ExportedAt})
	if err != nil {
		return fmt.Errorf("写入原始录像清单: %w", err)
	}
	if _, err := manifestWriter.Write(manifestBody); err != nil {
		return fmt.Errorf("写入原始录像清单: %w", err)
	}

	total := EstimateSourceExportBytes(source)
	var written uint64
	usedNames := map[string]int{"manifest.json": 1}
	buffer := make([]byte, 1024*1024)
	for _, sourceFile := range source.SourceFiles {
		if err := ctx.Err(); err != nil {
			return err
		}
		input, err := os.Open(sourceFile.Path)
		if err != nil {
			return fmt.Errorf("打开原始录像: %w", err)
		}
		name := uniqueArchiveName(sourceFile.Name, usedNames)
		header := &zip.FileHeader{Name: name, Method: zip.Store, Modified: sourceFile.RecordedAt}
		header.SetMode(0o640)
		entry, createErr := archive.CreateHeader(header)
		if createErr == nil {
			_, createErr = copySourceFile(ctx, entry, input, buffer, func(bytes int) {
				written += uint64(bytes)
				if total > 0 {
					updateProgress(min(99, int(written*100/total)))
				}
			})
		}
		closeErr := input.Close()
		if createErr != nil {
			return fmt.Errorf("写入原始录像: %w", createErr)
		}
		if closeErr != nil {
			return fmt.Errorf("关闭原始录像: %w", closeErr)
		}
	}
	if err := archive.Close(); err != nil {
		return fmt.Errorf("完成原始录像压缩包: %w", err)
	}
	if err := file.Close(); err != nil {
		return fmt.Errorf("关闭原始录像压缩包: %w", err)
	}
	completed = true
	return nil
}

func copySourceFile(ctx context.Context, destination io.Writer, source io.Reader, buffer []byte, onWrite func(int)) (int64, error) {
	var total int64
	for {
		if err := ctx.Err(); err != nil {
			return total, err
		}
		count, readErr := source.Read(buffer)
		if count > 0 {
			written, writeErr := destination.Write(buffer[:count])
			total += int64(written)
			if written > 0 {
				onWrite(written)
			}
			if writeErr != nil {
				return total, writeErr
			}
			if written != count {
				return total, io.ErrShortWrite
			}
		}
		if readErr == io.EOF {
			return total, nil
		}
		if readErr != nil {
			return total, readErr
		}
	}
}

func uniqueArchiveName(name string, used map[string]int) string {
	count := used[name]
	used[name] = count + 1
	if count == 0 {
		return name
	}
	extension := filepath.Ext(name)
	base := strings.TrimSuffix(name, extension)
	for {
		candidate := fmt.Sprintf("%s-%d%s", base, count+1, extension)
		if used[candidate] == 0 {
			used[candidate] = 1
			return candidate
		}
		count++
	}
}

func exportArgs(request ExportRequest, source ExportSource, output string) ([]string, error) {
	profile, ok := map[string]compositeProfile{
		"smooth":   {width: 854, height: 480, fps: 20, bitrate: 1500},
		"standard": {width: 1280, height: 720, fps: 24, bitrate: 3000},
		"high":     {width: 1920, height: 1080, fps: 30, bitrate: 8000},
		"original": {fps: 36},
	}[request.Quality]
	if !ok || !isHostOrLinuxAbsolutePath(output) {
		return nil, &InputError{Message: "导出参数无效"}
	}
	exportStart := source.SessionStart.Add(time.Duration(request.StartSeconds * float64(time.Second)))
	exportEnd := exportStart.Add(time.Duration(request.DurationSeconds * float64(time.Second)))
	tiles, err := exportTiles(request, profile)
	if err != nil {
		return nil, err
	}
	args := []string{"-hide_banner", "-loglevel", "warning", "-nostdin", "-y"}
	filters := make([]string, 0, len(source.Videos)*2)
	cameraLabels := make([]string, 0, len(tiles))
	inputIndex := 0
	for cameraIndex, tile := range tiles {
		videos := slices.DeleteFunc(slices.Clone(source.Videos), func(video ExportVideo) bool { return video.Camera != tile.camera })
		slices.SortFunc(videos, func(left, right ExportVideo) int { return left.RecordedAt.Compare(right.RecordedAt) })
		cursor := exportStart
		segments := make([]string, 0, len(videos))
		for _, video := range videos {
			videoEnd := video.RecordedAt.Add(time.Duration(video.Duration * float64(time.Second)))
			start := maxTime(exportStart, video.RecordedAt, cursor)
			end := minTime(exportEnd, videoEnd)
			if !end.After(start) || !isLinuxAbsolutePath(video.Path) {
				continue
			}
			if inputIndex >= maxExportDecoderInputs {
				return nil, &InputError{Message: "导出范围包含过多录像片段，请缩短时间范围后重试"}
			}
			args = append(args, "-hwaccel", "rkmpp", "-hwaccel_output_format", "drm_prime", "-c:v", "h264_rkmpp", "-i", filepath.ToSlash(filepath.Clean(video.Path)))
			label := fmt.Sprintf("c%ds%d", cameraIndex, len(segments))
			gap := max(0, start.Sub(cursor).Seconds())
			trimStart := max(0, start.Sub(video.RecordedAt).Seconds())
			duration := end.Sub(start).Seconds()
			filter := fmt.Sprintf("[%d:v]trim=start=%.6f:duration=%.6f,setpts=PTS-STARTPTS,fps=%d", inputIndex, trimStart, duration, profile.fps)
			if gap > 0 {
				filter += fmt.Sprintf(",tpad=start_mode=clone:start_duration=%.6f", gap)
			}
			if request.Quality == "original" {
				filter += fmt.Sprintf(",hwdownload,format=nv12[%s]", label)
			} else {
				filter += fmt.Sprintf(",scale_rkrga=w=%d:h=%d:format=nv12:force_original_aspect_ratio=disable,hwdownload,format=nv12[%s]", tile.width, tile.height, label)
			}
			filters = append(filters, filter)
			segments = append(segments, "["+label+"]")
			inputIndex++
			cursor = end
		}
		if len(segments) == 0 {
			return nil, &InputError{Message: "选择的视角在导出范围内没有可用片段"}
		}
		cameraLabel := fmt.Sprintf("camera%d", cameraIndex)
		chain := strings.Join(segments, "")
		if len(segments) > 1 {
			chain += fmt.Sprintf("concat=n=%d:v=1:a=0", len(segments))
		} else {
			chain += "null"
		}
		trailing := max(0, exportEnd.Sub(cursor).Seconds())
		if trailing > 0 {
			chain += fmt.Sprintf(",tpad=stop_mode=clone:stop_duration=%.6f", trailing)
		}
		chain += fmt.Sprintf(",trim=duration=%.6f,setpts=PTS-STARTPTS[%s]", request.DurationSeconds, cameraLabel)
		filters = append(filters, chain)
		cameraLabels = append(cameraLabels, "["+cameraLabel+"]")
	}

	outputLabel := "out"
	if len(tiles) == 1 {
		filters = append(filters, cameraLabels[0]+"null["+outputLabel+"]")
	} else {
		positions := make([]string, 0, len(tiles))
		for _, tile := range tiles {
			positions = append(positions, strconv.Itoa(tile.x)+"_"+strconv.Itoa(tile.y))
		}
		filters = append(filters, strings.Join(cameraLabels, "")+fmt.Sprintf("xstack=inputs=%d:layout=%s:fill=black:shortest=1[%s]", len(tiles), strings.Join(positions, "|"), outputLabel))
	}
	args = append(args, "-filter_complex", strings.Join(filters, ";"), "-map", "["+outputLabel+"]", "-an", "-c:v", "h264_rkmpp")
	if request.Quality == "original" {
		args = append(args, "-rc_mode", "CQP", "-qp_init", "18", "-qp_min", "16", "-qp_max", "24")
	} else {
		bitrate := strconv.Itoa(profile.bitrate) + "k"
		args = append(args, "-rc_mode", "CBR", "-b:v", bitrate, "-maxrate", bitrate, "-bufsize", strconv.Itoa(profile.bitrate*2)+"k")
	}
	args = append(args, "-g", strconv.Itoa(profile.fps*2), "-fps_mode", "cfr", "-t", fmt.Sprintf("%.6f", request.DurationSeconds),
		"-movflags", "+faststart", "-progress", "pipe:1", "-nostats", filepath.ToSlash(filepath.Clean(output)))
	return args, nil
}

func isHostOrLinuxAbsolutePath(value string) bool {
	return isLinuxAbsolutePath(value) || filepath.IsAbs(strings.TrimSpace(value))
}

func exportTiles(request ExportRequest, profile compositeProfile) ([]compositeTile, error) {
	if request.Layout == "single" {
		width := profile.width
		if request.Quality != "original" {
			width = teslaAspectWidth(profile.height)
		}
		return []compositeTile{{camera: request.Cameras[0], width: width, height: profile.height}}, nil
	}
	return teslaLayoutTiles(request.Layout, profile.height)
}

func maxTime(values ...time.Time) time.Time {
	result := values[0]
	for _, value := range values[1:] {
		if value.After(result) {
			result = value
		}
	}
	return result
}

func minTime(values ...time.Time) time.Time {
	result := values[0]
	for _, value := range values[1:] {
		if value.Before(result) {
			result = value
		}
	}
	return result
}
