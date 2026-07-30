package media

import (
	"archive/zip"
	"context"
	"crypto/rand"
	"encoding/base64"
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
	ExportQueued             ExportState = "queued"
	ExportRunning            ExportState = "running"
	ExportCompleted          ExportState = "completed"
	ExportFailed             ExportState = "failed"
	ExportCanceled           ExportState = "canceled"
	defaultExportRetainCount             = 8
	maxExportRetainCount                 = 20
	maxTerminalTaskHistory               = 24
	maxExportDecoderInputs               = 12
	maxSourceExportFiles                 = 120
	maxSourceExportBytes     uint64      = 4 * 1024 * 1024 * 1024
	maxSourceExportHistory               = 2
	// 高保真模式实际码率会随场景复杂度浮动；预留值必须覆盖高细节片段，不能沿用旧版 8 Mbps 的 CBR 估算。
	highExportReservedKbps uint64 = 64_000
)

// ExportMode 标识导出任务是重新编码剪辑还是归档原画浏览包。
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
	BurnTimestamp   bool       `json:"burn_timestamp"`
	BurnSEI         bool       `json:"burn_sei"`
}

// ExportSettings 是用户可调整的导出文件保留策略。
type ExportSettings struct {
	// DeleteAfterDownload 只在浏览器完整下载成功后删除文件，避免网络中断导致导出丢失。
	DeleteAfterDownload bool `json:"delete_after_download"`
	// RetainCount 限制可再次下载的已完成导出数，防止导出文件长期占满数据盘。
	RetainCount int `json:"retain_count"`
}

// ExportVideo 是已通过快照边界校验的单个输入片段。
type ExportVideo struct {
	Camera      string
	Path        string
	RecordedAt  time.Time
	WallClockAt time.Time
	Duration    float64
}

// ExportOverlayCue 是导出时间轴上的文字叠加内容；由 API 在受保护快照内准备。
type ExportOverlayCue struct {
	Start     time.Duration
	End       time.Duration
	Timestamp string
	SEIText   string
}

// SourceExportFile 是已通过快照边界校验、可直接归档到原画浏览包的录像文件。
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
	OverlayCues  []ExportOverlayCue
}

// ExportTask 是不包含板端文件路径的任务状态。
type ExportTask struct {
	ID              string        `json:"id"`
	State           ExportState   `json:"state"`
	Request         ExportRequest `json:"request"`
	ProgressPercent int           `json:"progress_percent"`
	CreatedAt       time.Time     `json:"created_at"`
	VideoStartAt    time.Time     `json:"-"`
	StartedAt       *time.Time    `json:"started_at,omitempty"`
	CompletedAt     *time.Time    `json:"completed_at,omitempty"`
	Error           string        `json:"error,omitempty"`
	DownloadReady   bool          `json:"download_ready"`
	EstimatedBytes  uint64        `json:"estimated_bytes"`
	FileCount       int           `json:"file_count,omitempty"`
}

type exportState struct {
	Version  int                   `json:"version"`
	Settings ExportSettings        `json:"settings"`
	Tasks    []persistedExportTask `json:"tasks"`
}

type persistedExportTask struct {
	ExportTask
	VideoStartAt time.Time `json:"video_start_at"`
}

// ExportConfig 限制导出路径、队列和单任务资源占用。
type ExportConfig struct {
	FFmpegPath  string
	OutputDir   string
	StatePath   string
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
	stateMu   sync.Mutex
	mu        sync.RWMutex
	settings  ExportSettings
	tasks     map[string]ExportTask
	cancels   map[string]context.CancelFunc
	releases  map[string]func()
	queue     chan exportJob
	runOnce   sync.Once
	done      chan struct{}
	stopping  bool
}

// NewExporter 在启动阶段读取导出策略和历史；数据盘未挂载时不触碰输出目录。
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
	if config.StatePath != "" && !isHostOrLinuxAbsolutePath(config.StatePath) {
		return nil, errors.New("导出状态路径必须使用绝对路径")
	}
	exporter := &Exporter{
		runner: runner, config: config, logger: config.Logger, tasks: make(map[string]ExportTask),
		cancels: make(map[string]context.CancelFunc), releases: make(map[string]func()),
		queue: make(chan exportJob, config.QueueSize), done: make(chan struct{}), settings: defaultExportSettings(),
	}
	if err := exporter.loadState(); err != nil {
		return nil, err
	}
	return exporter, nil
}

func defaultExportSettings() ExportSettings {
	return ExportSettings{RetainCount: defaultExportRetainCount}
}

func normalizeExportSettings(settings ExportSettings) (ExportSettings, error) {
	if settings.RetainCount == 0 {
		settings.RetainCount = defaultExportRetainCount
	}
	if settings.RetainCount < 1 || settings.RetainCount > maxExportRetainCount {
		return ExportSettings{}, &InputError{Message: fmt.Sprintf("历史导出保留数量必须在 1 到 %d 之间", maxExportRetainCount)}
	}
	return settings, nil
}

func (e *Exporter) loadState() error {
	if e.config.StatePath == "" {
		return nil
	}
	info, err := os.Lstat(e.config.StatePath)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("检查导出状态: %w", err)
	}
	if !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 {
		return errors.New("导出状态必须是普通文件")
	}
	if info.Size() > 1024*1024 {
		return errors.New("导出状态文件过大")
	}
	content, err := os.ReadFile(e.config.StatePath)
	if err != nil {
		return fmt.Errorf("读取导出状态: %w", err)
	}
	var persisted exportState
	decoder := json.NewDecoder(strings.NewReader(string(content)))
	if err := decoder.Decode(&persisted); err != nil {
		return fmt.Errorf("解析导出状态: %w", err)
	}
	if persisted.Version != 0 && persisted.Version != 1 {
		return fmt.Errorf("不支持的导出状态版本: %d", persisted.Version)
	}
	settings, err := normalizeExportSettings(persisted.Settings)
	if err != nil {
		return fmt.Errorf("导出状态中的保留策略无效: %w", err)
	}
	tasks := make(map[string]ExportTask, len(persisted.Tasks))
	for _, item := range persisted.Tasks {
		task := item.ExportTask
		if !isExportID(task.ID) || task.State != ExportCompleted || !task.DownloadReady || task.CreatedAt.IsZero() || task.Request.Mode != ExportModeClip && task.Request.Mode != ExportModeSource {
			return errors.New("导出状态包含无效的历史任务")
		}
		task.VideoStartAt = item.VideoStartAt
		tasks[task.ID] = cloneExportTask(task)
	}
	e.settings = settings
	e.tasks = tasks
	return nil
}

func (e *Exporter) persistState() error {
	if e.config.StatePath == "" {
		return nil
	}
	e.stateMu.Lock()
	defer e.stateMu.Unlock()
	e.mu.RLock()
	state := exportState{Version: 1, Settings: e.settings, Tasks: make([]persistedExportTask, 0, len(e.tasks))}
	for _, task := range e.tasks {
		if task.State != ExportCompleted || !task.DownloadReady {
			continue
		}
		state.Tasks = append(state.Tasks, persistedExportTask{ExportTask: cloneExportTask(task), VideoStartAt: task.VideoStartAt})
	}
	e.mu.RUnlock()
	slices.SortFunc(state.Tasks, func(left, right persistedExportTask) int {
		return right.CreatedAt.Compare(left.CreatedAt)
	})
	return saveExportState(e.config.StatePath, state)
}

func saveExportState(path string, state exportState) (resultErr error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		return fmt.Errorf("创建导出状态目录: %w", err)
	}
	temporary, err := os.CreateTemp(filepath.Dir(path), ".export-state-*")
	if err != nil {
		return fmt.Errorf("创建导出状态临时文件: %w", err)
	}
	temporaryName := temporary.Name()
	closed := false
	defer func() {
		if !closed {
			if closeErr := temporary.Close(); closeErr != nil && resultErr == nil {
				resultErr = closeErr
			}
		}
		if resultErr != nil {
			_ = os.Remove(temporaryName)
		}
	}()
	if err := temporary.Chmod(0o600); err != nil {
		return fmt.Errorf("设置导出状态权限: %w", err)
	}
	if err := json.NewEncoder(temporary).Encode(state); err != nil {
		return fmt.Errorf("编码导出状态: %w", err)
	}
	if err := temporary.Sync(); err != nil {
		return fmt.Errorf("同步导出状态: %w", err)
	}
	if err := temporary.Close(); err != nil {
		return fmt.Errorf("关闭导出状态: %w", err)
	}
	closed = true
	if err := os.Rename(temporaryName, path); err != nil {
		return fmt.Errorf("发布导出状态: %w", err)
	}
	return nil
}

func cloneExportTask(task ExportTask) ExportTask {
	task.Request.Cameras = slices.Clone(task.Request.Cameras)
	task.Request.MomentIDs = slices.Clone(task.Request.MomentIDs)
	if task.StartedAt != nil {
		started := *task.StartedAt
		task.StartedAt = &started
	}
	if task.CompletedAt != nil {
		completed := *task.CompletedAt
		task.CompletedAt = &completed
	}
	return task
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
	e.pruneFinished()
	id, err := exportID()
	if err != nil {
		release()
		return ExportTask{}, err
	}
	var releaseOnce sync.Once
	safeRelease := func() { releaseOnce.Do(release) }
	task := ExportTask{
		ID: id, State: ExportQueued, Request: request, CreatedAt: time.Now(),
		VideoStartAt:   source.SessionStart.Add(time.Duration(request.StartSeconds * float64(time.Second))),
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
		if !entry.IsDir() && isExportTemporaryArtifact(entry.Name()) {
			_ = os.Remove(filepath.Join(e.config.OutputDir, entry.Name()))
		}
	}
	e.prepared = true
	e.discardMissingHistory()
	return nil
}

func (e *Exporter) discardMissingHistory() {
	e.mu.RLock()
	history := make([]ExportTask, 0, len(e.tasks))
	for _, task := range e.tasks {
		if task.State == ExportCompleted && task.DownloadReady {
			history = append(history, task)
		}
	}
	e.mu.RUnlock()
	changed := false
	for _, task := range history {
		if _, err := os.Stat(e.outputPathForTask(task)); err == nil || !errors.Is(err, os.ErrNotExist) {
			continue
		}
		e.mu.Lock()
		if current, found := e.tasks[task.ID]; found && current.State == ExportCompleted && current.DownloadReady {
			delete(e.tasks, task.ID)
			changed = true
		}
		e.mu.Unlock()
	}
	if changed {
		if err := e.persistState(); err != nil {
			e.logger.Warn("清理缺失导出历史失败", "error", err)
		}
	}
}

// Settings 返回当前导出策略的副本。
func (e *Exporter) Settings() ExportSettings {
	e.mu.RLock()
	defer e.mu.RUnlock()
	return e.settings
}

// UpdateSettings 保存导出策略，并立即按新的保留数量回收最旧的已完成导出。
func (e *Exporter) UpdateSettings(settings ExportSettings) (ExportSettings, error) {
	normalized, err := normalizeExportSettings(settings)
	if err != nil {
		return ExportSettings{}, err
	}
	e.mu.Lock()
	previous := e.settings
	e.settings = normalized
	e.mu.Unlock()
	if err := e.persistState(); err != nil {
		e.mu.Lock()
		e.settings = previous
		e.mu.Unlock()
		return ExportSettings{}, err
	}
	e.prepareMu.Lock()
	prepared := e.prepared
	e.prepareMu.Unlock()
	if prepared {
		e.pruneFinished()
	}
	return normalized, nil
}

// History 返回仍可再次下载的已完成导出，按完成时间由新到旧排序。
func (e *Exporter) History() []ExportTask {
	e.mu.RLock()
	history := make([]ExportTask, 0, len(e.tasks))
	for _, task := range e.tasks {
		if task.State == ExportCompleted && task.DownloadReady {
			history = append(history, cloneExportTask(task))
		}
	}
	e.mu.RUnlock()
	slices.SortFunc(history, func(left, right ExportTask) int {
		leftAt, rightAt := left.CreatedAt, right.CreatedAt
		if left.CompletedAt != nil {
			leftAt = *left.CompletedAt
		}
		if right.CompletedAt != nil {
			rightAt = *right.CompletedAt
		}
		return rightAt.Compare(leftAt)
	})
	return history
}

// Task 返回防御性拷贝，避免 API 修改内部状态。
func (e *Exporter) Task(id string) (ExportTask, error) {
	e.mu.RLock()
	defer e.mu.RUnlock()
	task, ok := e.tasks[id]
	if !ok {
		return ExportTask{}, ErrExportNotFound
	}
	return cloneExportTask(task), nil
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

// Consume 删除已被浏览器完整接收的导出，并从历史中移除。
func (e *Exporter) Consume(id string) error {
	task, err := e.Task(id)
	if err != nil {
		return err
	}
	if task.State != ExportCompleted || !task.DownloadReady {
		return errors.New("导出文件尚未就绪")
	}
	if err := os.Remove(e.outputPathForTask(task)); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("删除已下载导出: %w", err)
	}
	e.mu.Lock()
	current, found := e.tasks[id]
	if found && current.State == ExportCompleted {
		delete(e.tasks, id)
	}
	e.mu.Unlock()
	return e.persistState()
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

	subtitlePath := ""
	var err error
	if job.request.BurnTimestamp || job.request.BurnSEI {
		var hasSubtitles bool
		subtitlePath = e.subtitlePath(task.ID)
		hasSubtitles, err = writeOverlaySubtitles(subtitlePath, job.source.OverlayCues, job.request.BurnTimestamp, job.request.BurnSEI)
		if err != nil {
			e.finishJob(job.id, task, err)
			return
		}
		if !hasSubtitles {
			subtitlePath = ""
		}
		defer func() {
			_ = os.Remove(e.subtitlePath(task.ID))
		}()
	}
	args, err := exportArgs(job.request, job.source, e.tempPathForTask(task), subtitlePath)
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
	if persistErr := e.persistState(); persistErr != nil {
		e.logger.Warn("保存导出历史失败", "task_id", id, "error", persistErr)
	}
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
	e.mu.RLock()
	completed := make([]ExportTask, 0, len(e.tasks))
	terminal := make([]ExportTask, 0, len(e.tasks))
	for id, task := range e.tasks {
		if _, retained := e.releases[id]; retained {
			continue
		}
		if task.State == ExportCompleted && task.DownloadReady {
			completed = append(completed, task)
		} else if task.State == ExportFailed || task.State == ExportCanceled {
			terminal = append(terminal, task)
		}
	}
	settings := e.settings
	e.mu.RUnlock()
	slices.SortFunc(completed, func(left, right ExportTask) int { return right.CreatedAt.Compare(left.CreatedAt) })
	slices.SortFunc(terminal, func(left, right ExportTask) int { return right.CreatedAt.Compare(left.CreatedAt) })
	stale := make([]ExportTask, 0)
	if len(completed) > settings.RetainCount {
		stale = append(stale, completed[settings.RetainCount:]...)
	}
	if len(terminal) > maxTerminalTaskHistory {
		stale = append(stale, terminal[maxTerminalTaskHistory:]...)
	}
	if len(stale) == 0 {
		return
	}
	removed := false
	for _, task := range stale {
		if err := os.Remove(e.outputPathForTask(task)); err != nil && !errors.Is(err, os.ErrNotExist) {
			e.logger.Warn("清理历史导出文件失败", "task_id", task.ID, "error", err)
			continue
		}
		if err := os.Remove(e.tempPathForTask(task)); err != nil && !errors.Is(err, os.ErrNotExist) {
			e.logger.Warn("清理历史导出临时文件失败", "task_id", task.ID, "error", err)
			continue
		}
		e.mu.Lock()
		if _, retained := e.releases[task.ID]; !retained {
			delete(e.tasks, task.ID)
			removed = true
		}
		e.mu.Unlock()
	}
	if removed {
		if err := e.persistState(); err != nil {
			e.logger.Warn("保存清理后的导出历史失败", "error", err)
		}
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

func (e *Exporter) subtitlePath(id string) string {
	return filepath.Join(e.config.OutputDir, "."+id+".tmp.ass")
}

// ExportDownloadFilename 返回浏览器下载时展示的文件名；板端仍使用任务 ID 保存，以维持清理和并发任务隔离。
func ExportDownloadFilename(task ExportTask) string {
	if task.Request.Mode == ExportModeSource {
		return "teslabox-source-" + task.ID + ".zip"
	}
	startAt := task.VideoStartAt
	if startAt.IsZero() {
		startAt = task.CreatedAt
	}
	return "export-" + exportViewLabel(task.Request) + "-" + startAt.Format("20060102-150405") + ".mp4"
}

func exportViewLabel(request ExportRequest) string {
	switch request.Layout {
	case "composite6":
		return "六路合成"
	case "composite4a":
		return "四路合成A"
	case "composite4b":
		return "四路合成B"
	case "composite3":
		return "三路合成"
	case "single":
		if len(request.Cameras) == 1 {
			switch request.Cameras[0] {
			case "left_pillar":
				return "左B柱"
			case "front":
				return "前视"
			case "right_pillar":
				return "右B柱"
			case "left_repeater":
				return "左翼子板"
			case "back":
				return "后视"
			case "right_repeater":
				return "右翼子板"
			}
		}
	}
	return "录像"
}

func validateExport(request ExportRequest, source ExportSource, maximum time.Duration) error {
	if request.Mode == ExportModeSource {
		if request.BurnTimestamp || request.BurnSEI {
			return &InputError{Message: "多路原画导出不支持叠加时间或 SEI 信息"}
		}
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
		return &InputError{Message: "原画 MP4 仅支持单路视角；多路请使用多路原画导出"}
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
	bitrateKbps := map[string]uint64{"smooth": 1500, "standard": 3000, "high": highExportReservedKbps, "original": 20000}[request.Quality]
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

func isExportTemporaryArtifact(name string) bool {
	id := ""
	if strings.HasPrefix(name, ".") && strings.HasSuffix(name, ".tmp.mp4") {
		id = strings.TrimSuffix(strings.TrimPrefix(name, "."), ".tmp.mp4")
	} else if strings.HasPrefix(name, ".") && strings.HasSuffix(name, ".tmp.zip") {
		id = strings.TrimSuffix(strings.TrimPrefix(name, "."), ".tmp.zip")
	} else if strings.HasPrefix(name, ".") && strings.HasSuffix(name, ".tmp.ass") {
		id = strings.TrimSuffix(strings.TrimPrefix(name, "."), ".tmp.ass")
	}
	return isExportID(id)
}

func isExportID(id string) bool {
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
	MomentID    string    `json:"moment_id"`
	Camera      string    `json:"camera"`
	Name        string    `json:"name"`
	ArchiveName string    `json:"archive_name"`
	RecordedAt  time.Time `json:"recorded_at"`
	Size        uint64    `json:"size"`
}

type sourceArchiveEntry struct {
	SourceFile  SourceExportFile
	ArchiveName string
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
	usedNames := map[string]int{"manifest.json": 1, "player.html": 1}
	entries := make([]sourceArchiveEntry, 0, len(source.SourceFiles))
	manifest := sourceArchiveManifest{Format: "teslabox-source-export/v2", ExportedAt: time.Now(), SourceFiles: make([]sourceArchiveManifestFile, 0, len(source.SourceFiles))}
	for _, sourceFile := range source.SourceFiles {
		archiveName := uniqueArchiveName(sourceFile.Name, usedNames)
		entries = append(entries, sourceArchiveEntry{SourceFile: sourceFile, ArchiveName: archiveName})
		manifest.SourceFiles = append(manifest.SourceFiles, sourceArchiveManifestFile{
			MomentID: sourceFile.MomentID, Camera: sourceFile.Camera, Name: sourceFile.Name, ArchiveName: archiveName,
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
	playerBody, err := sourceArchivePlayer(manifestBody)
	if err != nil {
		return err
	}
	playerWriter, err := archive.CreateHeader(&zip.FileHeader{Name: "player.html", Method: zip.Store, Modified: manifest.ExportedAt})
	if err != nil {
		return fmt.Errorf("写入原画浏览页面: %w", err)
	}
	if _, err := playerWriter.Write(playerBody); err != nil {
		return fmt.Errorf("写入原画浏览页面: %w", err)
	}

	total := EstimateSourceExportBytes(source)
	var written uint64
	buffer := make([]byte, 1024*1024)
	for _, archiveEntry := range entries {
		if err := ctx.Err(); err != nil {
			return err
		}
		sourceFile := archiveEntry.SourceFile
		input, err := os.Open(sourceFile.Path)
		if err != nil {
			return fmt.Errorf("打开原始录像: %w", err)
		}
		header := &zip.FileHeader{Name: archiveEntry.ArchiveName, Method: zip.Store, Modified: sourceFile.RecordedAt}
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

func sourceArchivePlayer(manifest []byte) ([]byte, error) {
	if !json.Valid(manifest) {
		return nil, errors.New("原画浏览包清单无效")
	}
	encodedManifest := base64.StdEncoding.EncodeToString(manifest)
	return []byte(strings.ReplaceAll(sourceArchivePlayerTemplate, "__TESLABOX_MANIFEST_BASE64__", encodedManifest)), nil
}

const sourceArchivePlayerTemplate = `<!doctype html>
<html lang="zh-CN">
<head>
  <meta charset="utf-8">
  <meta name="viewport" content="width=device-width, initial-scale=1">
  <title>TeslaBox 原画多路浏览包</title>
  <style>
    :root { color-scheme: dark; font-family: system-ui, sans-serif; background: #111; color: #f5f5f5; }
    body { margin: 0 auto; max-width: 1500px; padding: 20px; }
    h1 { margin: 0 0 8px; font-size: 22px; }
    p { color: #bbb; line-height: 1.5; }
    .toolbar { display: flex; flex-wrap: wrap; gap: 12px; align-items: center; margin: 18px 0; }
    select, button, input { font: inherit; }
    button { border: 0; border-radius: 8px; background: #e82127; color: white; padding: 9px 14px; cursor: pointer; }
    input[type=range] { flex: 1; min-width: 180px; }
    #time { min-width: 100px; font-variant-numeric: tabular-nums; }
    #grid { display: grid; grid-template-columns: repeat(3, minmax(0, 1fr)); gap: 8px; }
    figure { margin: 0; min-width: 0; border: 1px solid #303030; border-radius: 8px; overflow: hidden; background: #050505; }
    video { display: block; width: 100%; aspect-ratio: 724 / 469; background: #000; }
    figcaption { padding: 7px 9px; color: #ddd; font-size: 13px; }
    @media (max-width: 860px) { #grid { grid-template-columns: repeat(2, minmax(0, 1fr)); } }
    @media (max-width: 520px) { body { padding: 12px; } #grid { grid-template-columns: 1fr; } }
  </style>
</head>
<body>
  <h1>TeslaBox 原画多路浏览包</h1>
  <p>此包内 MP4 均为原始文件，未压缩、未转码。解压后直接打开本页即可同步查看所选分钟和视角；播放时需要浏览器能够解码原车端视频。</p>
  <div class="toolbar">
    <label>录像分钟 <select id="moment"></select></label>
    <button id="toggle" type="button">播放</button>
    <input id="seek" type="range" min="0" max="0" step="0.01" value="0" aria-label="播放进度">
    <span id="time">00:00 / 00:00</span>
  </div>
  <div id="grid"></div>
  <script>
  (() => {
    const manifest = JSON.parse(new TextDecoder().decode(Uint8Array.from(atob("__TESLABOX_MANIFEST_BASE64__"), char => char.charCodeAt(0))));
    const labels = {left_pillar:"左 B 柱", front:"前视", right_pillar:"右 B 柱", left_repeater:"左翼子板", back:"后视", right_repeater:"右翼子板"};
    const order = ["left_pillar", "front", "right_pillar", "left_repeater", "back", "right_repeater"];
    const moment = document.querySelector("#moment");
    const grid = document.querySelector("#grid");
    const toggle = document.querySelector("#toggle");
    const seek = document.querySelector("#seek");
    const time = document.querySelector("#time");
    const groups = new Map();
    for (const file of manifest.source_files || []) {
      const group = groups.get(file.moment_id) || [];
      group.push(file);
      groups.set(file.moment_id, group);
    }
    let tracks = [];
    let master = null;
    let synchronizing = false;
    const formatTime = seconds => {
      seconds = Math.max(0, Math.floor(Number(seconds) || 0));
      return String(Math.floor(seconds / 60)).padStart(2, "0") + ":" + String(seconds % 60).padStart(2, "0");
    };
    [...groups.entries()].sort((left, right) => String(left[0]).localeCompare(String(right[0]))).forEach(([id, files], index) => {
      const option = document.createElement("option");
      option.value = id;
      const recorded = files.map(file => file.recorded_at).filter(Boolean).sort()[0] || id;
      option.textContent = "第 " + (index + 1) + " 分钟 · " + recorded.replace("T", " ").replace(/\.\d+Z$/, "");
      moment.append(option);
    });
    function trackTarget(track) {
      return master.video.currentTime + (Date.parse(master.file.recorded_at) - Date.parse(track.file.recorded_at)) / 1000;
    }
    function synchronize(force) {
      if (!master || synchronizing) return;
      synchronizing = true;
      tracks.forEach(track => {
        if (track === master || !Number.isFinite(trackTarget(track))) return;
        const target = Math.max(0, trackTarget(track));
        if (force || Math.abs(track.video.currentTime - target) > 0.18) track.video.currentTime = target;
      });
      synchronizing = false;
    }
    function refreshTime() {
      const duration = Number.isFinite(master?.video.duration) ? master.video.duration : 0;
      seek.max = String(duration);
      seek.value = String(Math.min(duration, master?.video.currentTime || 0));
      time.textContent = formatTime(master?.video.currentTime) + " / " + formatTime(duration);
    }
    async function playTracks() {
      if (!master) return;
      synchronize(true);
      await Promise.allSettled(tracks.map(track => track.video.play()));
    }
    function loadMoment(autoplay) {
      const files = (groups.get(moment.value) || []).slice().sort((left, right) => order.indexOf(left.camera) - order.indexOf(right.camera));
      tracks.forEach(track => { track.video.pause(); track.video.removeAttribute("src"); track.video.load(); });
      tracks = [];
      grid.replaceChildren();
      files.forEach(file => {
        const figure = document.createElement("figure");
        const video = document.createElement("video");
        video.muted = true;
        video.playsInline = true;
        video.preload = "metadata";
        video.src = encodeURIComponent(file.archive_name);
        const caption = document.createElement("figcaption");
        caption.textContent = labels[file.camera] || file.camera;
        figure.append(video, caption);
        grid.append(figure);
        tracks.push({file, video});
      });
      master = tracks.find(track => track.file.camera === "front") || tracks[0] || null;
      if (!master) return;
      master.video.addEventListener("loadedmetadata", () => { synchronize(true); refreshTime(); if (autoplay) void playTracks(); }, {once: true});
      master.video.addEventListener("timeupdate", () => { synchronize(false); refreshTime(); });
      master.video.addEventListener("play", () => { toggle.textContent = "暂停"; });
      master.video.addEventListener("pause", () => { toggle.textContent = "播放"; });
      master.video.addEventListener("ended", () => {
        const next = moment.selectedIndex + 1;
        if (next < moment.options.length) {
          moment.selectedIndex = next;
          loadMoment(true);
          return;
        }
        tracks.forEach(track => track.video.pause());
        toggle.textContent = "播放";
      });
      refreshTime();
    }
    toggle.addEventListener("click", async () => {
      if (!master) return;
      if (!master.video.paused) { tracks.forEach(track => track.video.pause()); return; }
      await playTracks();
    });
    seek.addEventListener("input", () => {
      if (!master) return;
      master.video.currentTime = Number(seek.value);
      synchronize(true);
      refreshTime();
    });
    moment.addEventListener("change", () => loadMoment(false));
    if (moment.options.length) loadMoment(false);
  })();
  </script>
</body>
</html>`

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

func exportArgs(request ExportRequest, source ExportSource, output string, subtitlePaths ...string) ([]string, error) {
	profile, ok := map[string]compositeProfile{
		"smooth":   {width: 854, height: 480, fps: 20, bitrate: 1500},
		"standard": {width: 1280, height: 720, fps: 24, bitrate: 3000},
		"high":     {width: 1920, height: 1080, fps: 30, bitrate: int(highExportReservedKbps)},
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
	hevcComposite := request.Quality == "high" && len(tiles) > 1
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
			args = append(args, "-hwaccel", "rkmpp", "-hwaccel_output_format", "drm_prime", "-c:v", "h264_rkmpp")
			args = append(args, "-i", filepath.ToSlash(filepath.Clean(video.Path)))
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
			} else if hevcComposite {
				// 实测残块来自最终 h264_rkmpp 编码；保留硬解和 RGA 缩放以降低导出时间、CPU 占用和功耗，仅切换最终 HEVC 硬编。
				filter += fmt.Sprintf(",scale_rkrga=w=%d:h=%d:format=nv12:force_original_aspect_ratio=disable,hwdownload,format=nv12[%s]", tile.width, tile.height, label)
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
		stackLabel := outputLabel
		if request.Quality == "high" {
			stackLabel = "stack"
		}
		filters = append(filters, strings.Join(cameraLabels, "")+fmt.Sprintf("xstack=inputs=%d:layout=%s:fill=black:shortest=1[%s]", len(tiles), strings.Join(positions, "|"), stackLabel))
		if request.Quality == "high" {
			// RKMPP 编码路径以宏块边界处理超宽 NV12 帧；保留内容区域，仅补黑边以提供 16 对齐画布。
			filters = append(filters, "[stack]pad=w=ceil(iw/16)*16:h=ceil(ih/16)*16:x=0:y=0:color=black["+outputLabel+"]")
		}
	}
	if len(subtitlePaths) > 0 && subtitlePaths[0] != "" {
		filters = append(filters, fmt.Sprintf("[%s]subtitles=filename='%s':charenc=UTF-8[overlay]", outputLabel, escapeFilterPath(subtitlePaths[0])))
		outputLabel = "overlay"
	}
	encoder := "h264_rkmpp"
	if hevcComposite {
		encoder = "hevc_rkmpp"
	}
	args = append(args, "-filter_complex", strings.Join(filters, ";"), "-map", "["+outputLabel+"]", "-an", "-c:v", encoder)
	if request.Quality == "original" {
		args = append(args, "-rc_mode", "CQP", "-qp_init", "18", "-qp_min", "16", "-qp_max", "24")
	} else if request.Quality == "high" {
		// 多路画面共享一个码率池时，8 Mbps CBR 会把高细节片段过度压缩；把 QP 限制在 16–20 能避免复杂纹理被降到可见分层，任务仍由单工作进程串行化。
		args = append(args, "-rc_mode", "CQP", "-qp_init", "18", "-qp_min", "16", "-qp_max", "20")
	} else {
		bitrate := strconv.Itoa(profile.bitrate) + "k"
		args = append(args, "-rc_mode", "CBR", "-b:v", bitrate, "-maxrate", bitrate, "-bufsize", strconv.Itoa(profile.bitrate*2)+"k")
	}
	if hevcComposite {
		// hvc1 标记可让支持 HEVC 的常见本地播放器按 MP4 兼容路径识别视频流。
		args = append(args, "-tag:v", "hvc1")
	}
	args = append(args, "-g", strconv.Itoa(profile.fps*2), "-fps_mode", "cfr", "-t", fmt.Sprintf("%.6f", request.DurationSeconds),
		"-movflags", "+faststart", "-progress", "pipe:1", "-nostats", filepath.ToSlash(filepath.Clean(output)))
	return args, nil
}

func writeOverlaySubtitles(path string, cues []ExportOverlayCue, burnTimestamp, burnSEI bool) (bool, error) {
	if !burnTimestamp && !burnSEI {
		return false, nil
	}
	lines := make([]string, 0, len(cues)*2)
	for _, cue := range cues {
		if cue.End <= cue.Start {
			continue
		}
		start, end := assTimestamp(cue.Start), assTimestamp(cue.End)
		if burnTimestamp && cue.Timestamp != "" {
			lines = append(lines, fmt.Sprintf("Dialogue: 0,%s,%s,Timestamp,,0,0,0,,%s", start, end, escapeASS(cue.Timestamp)))
		}
		if burnSEI && cue.SEIText != "" {
			lines = append(lines, fmt.Sprintf("Dialogue: 0,%s,%s,Telemetry,,0,0,0,,%s", start, end, escapeASS(cue.SEIText)))
		}
	}
	if len(lines) == 0 {
		return false, nil
	}
	content := "[Script Info]\nScriptType: v4.00+\nPlayResX: 1920\nPlayResY: 1080\nScaledBorderAndShadow: yes\n\n" +
		"[V4+ Styles]\nFormat: Name,Fontname,Fontsize,PrimaryColour,SecondaryColour,OutlineColour,BackColour,Bold,Italic,Underline,StrikeOut,ScaleX,ScaleY,Spacing,Angle,BorderStyle,Outline,Shadow,Alignment,MarginL,MarginR,MarginV,Encoding\n" +
		"Style: Timestamp,Arial,28,&H00FFFFFF,&H000000FF,&H96000000,&H96000000,1,0,0,0,100,100,0,0,1,2,1,7,28,28,24,1\n" +
		"Style: Telemetry,Arial,22,&H00FFFFFF,&H000000FF,&H96000000,&H96000000,0,0,0,0,100,100,0,0,1,2,1,9,28,28,24,1\n\n" +
		"[Events]\nFormat: Layer,Start,End,Style,Name,MarginL,MarginR,MarginV,Effect,Text\n" + strings.Join(lines, "\n") + "\n"
	if err := os.WriteFile(path, []byte(content), 0o640); err != nil {
		return false, fmt.Errorf("写入导出字幕: %w", err)
	}
	return true, nil
}

func assTimestamp(value time.Duration) string {
	centiseconds := max(int64(0), value.Milliseconds()/10)
	hours := centiseconds / (60 * 60 * 100)
	minutes := centiseconds / (60 * 100) % 60
	seconds := centiseconds / 100 % 60
	return fmt.Sprintf("%d:%02d:%02d.%02d", hours, minutes, seconds, centiseconds%100)
}

func escapeASS(value string) string {
	value = strings.ReplaceAll(value, "\\", "\\\\")
	value = strings.ReplaceAll(value, "{", "\\{")
	value = strings.ReplaceAll(value, "}", "\\}")
	return strings.ReplaceAll(value, "\n", "\\N")
}

func escapeFilterPath(value string) string {
	value = filepath.ToSlash(filepath.Clean(value))
	return strings.NewReplacer("\\", "\\\\", "'", "\\'", ":", "\\:", ",", "\\,").Replace(value)
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
