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
	"math"
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
	highExportReservedKbps     uint64 = 64_000
	originalExportReservedKbps uint64 = 32_000
)

// ExportMode 标识导出任务是重新编码剪辑还是归档原画浏览包。
type ExportMode string

const (
	ExportModeClip   ExportMode = "clip"
	ExportModeSource ExportMode = "source"
)

// ExportOverlayMode 决定画面信息占用独立画布还是覆盖在录像内部。
type ExportOverlayMode string

const (
	ExportOverlayDashcam  ExportOverlayMode = "dashcam"
	ExportOverlayTopBar   ExportOverlayMode = "top_bar"
	ExportOverlayFloating ExportOverlayMode = "floating"
)

// ExportRequest 只接受结构化选项，避免把 FFmpeg 参数暴露给客户端。
type ExportRequest struct {
	Mode            ExportMode        `json:"mode,omitempty"`
	SessionID       string            `json:"session_id"`
	MomentIDs       []string          `json:"moment_ids,omitempty"`
	StartSeconds    float64           `json:"start_seconds"`
	DurationSeconds float64           `json:"duration_seconds"`
	Layout          string            `json:"layout"`
	Cameras         []string          `json:"cameras"`
	Quality         string            `json:"quality"`
	BurnTimestamp   bool              `json:"burn_timestamp"`
	BurnSEI         bool              `json:"burn_sei"`
	OverlayMode     ExportOverlayMode `json:"overlay_mode,omitempty"`
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

// ExportOverlayCue 是导出时间轴上的画面信息；后台在受保护快照内准备，避免创建任务接口被 SEI 解析阻塞。
type ExportOverlayCue struct {
	Start     time.Duration
	End       time.Duration
	Timestamp string
	Telemetry *ExportTelemetry
}

// ExportTelemetry 是与具体 SEI 解析实现解耦的导出仪表状态。
type ExportTelemetry struct {
	Gear                string
	SpeedKPH            float64
	AcceleratorPosition float64
	SteeringAngle       float64
	LeftBlinker         bool
	RightBlinker        bool
	BlinkerVisible      bool
	BrakeApplied        bool
	Autopilot           string
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
	OutputBytes     uint64        `json:"output_bytes,omitempty"`
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
	if config.QueueSize <= 0 || config.MaxDuration < 0 {
		return nil, errors.New("导出队列必须大于零，最大时长不能为负数")
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

	err := e.runClipJob(ctx, task, job)
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
			if info, statErr := os.Stat(e.outputPathForTask(task)); statErr == nil {
				task.OutputBytes = uint64(info.Size())
			} else {
				e.logger.Warn("读取导出文件实际大小失败", "task_id", id, "error", statErr)
			}
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
	if request.StartSeconds < 0 || request.DurationSeconds <= 0 {
		return &InputError{Message: "导出时长必须大于 0"}
	}
	if maximum > 0 && request.DurationSeconds > maximum.Seconds() {
		return &InputError{Message: fmt.Sprintf("导出时长不能超过 %.0f 分钟", maximum.Minutes())}
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
	availableCameras := 0
	for _, camera := range request.Cameras {
		if !slices.Contains(knownCameras(), camera) {
			return &InputError{Message: "包含未知摄像头视角"}
		}
		exportStart := source.SessionStart.Add(time.Duration(request.StartSeconds * float64(time.Second)))
		exportEnd := exportStart.Add(time.Duration(request.DurationSeconds * float64(time.Second)))
		if slices.ContainsFunc(source.Videos, func(video ExportVideo) bool {
			videoEnd := video.RecordedAt.Add(time.Duration(video.Duration * float64(time.Second)))
			return video.Camera == camera && video.RecordedAt.Before(exportEnd) && videoEnd.After(exportStart)
		}) {
			availableCameras++
		}
	}
	if availableCameras == 0 {
		return &InputError{Message: "选择的视角在导出范围内没有录像"}
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
	switch {
	case !request.BurnTimestamp && !request.BurnSEI:
		request.OverlayMode = ""
	case request.OverlayMode == "" && request.BurnTimestamp && !request.BurnSEI:
		request.OverlayMode = ExportOverlayDashcam
	case request.OverlayMode == "":
		request.OverlayMode = ExportOverlayTopBar
	}
	switch request.OverlayMode {
	case "", ExportOverlayTopBar, ExportOverlayFloating:
	case ExportOverlayDashcam:
		if !request.BurnTimestamp || request.BurnSEI {
			return ExportRequest{}, &InputError{Message: "行车记录仪模式只支持单独叠加录像时间"}
		}
	default:
		return ExportRequest{}, &InputError{Message: "不支持的画面信息显示位置"}
	}
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
	request.OverlayMode = ""
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

// EstimateExportBytes 按常见场景码率估算用户最终得到的文件大小。
func EstimateExportBytes(request ExportRequest) uint64 {
	bitrateKbps := expectedExportBitrateKbps(request)
	if bitrateKbps == 0 || request.DurationSeconds <= 0 {
		return 0
	}
	return estimateEncodedBytes(bitrateKbps, request.DurationSeconds, 1.08)
}

// EstimateExportWorkingBytes 包含最终文件和仍在使用的最大单段临时文件。
func EstimateExportWorkingBytes(request ExportRequest) uint64 {
	reservedBitrate := reservedExportBitrateKbps(request)
	finalBytes := estimateEncodedBytes(reservedBitrate, request.DurationSeconds, 1.15)
	segmentDuration := min(request.DurationSeconds, maxClipSegmentDuration.Seconds())
	segmentBytes := estimateEncodedBytes(reservedBitrate, segmentDuration, 1.15)
	if ^uint64(0)-finalBytes < segmentBytes {
		return ^uint64(0)
	}
	return finalBytes + segmentBytes
}

func expectedExportBitrateKbps(request ExportRequest) uint64 {
	switch request.Quality {
	case "smooth":
		return 1500
	case "standard":
		return 3000
	case "original":
		return 18_000
	case "high":
		switch request.Layout {
		case "composite6":
			return 32_000
		case "composite4a", "composite4b":
			return 24_000
		default:
			return 16_000
		}
	default:
		return 0
	}
}

func reservedExportBitrateKbps(request ExportRequest) uint64 {
	switch request.Quality {
	case "high":
		return highExportReservedKbps
	case "original":
		return originalExportReservedKbps
	default:
		return expectedExportBitrateKbps(request)
	}
}

func estimateEncodedBytes(bitrateKbps uint64, durationSeconds, multiplier float64) uint64 {
	if bitrateKbps == 0 || durationSeconds <= 0 {
		return 0
	}
	return uint64(float64(bitrateKbps*1000)/8*durationSeconds*multiplier + 0.5)
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
	} else if strings.HasPrefix(name, ".") && strings.Contains(name, ".tmp.part-") {
		id = strings.TrimPrefix(strings.SplitN(name, ".tmp.part-", 2)[0], ".")
	} else if strings.HasPrefix(name, ".") && strings.HasSuffix(name, ".tmp.concat.txt") {
		id = strings.TrimSuffix(strings.TrimPrefix(name, "."), ".tmp.concat.txt")
	}
	return isExportID(id)
}

func isExportID(id string) bool {
	decoded, err := hex.DecodeString(id)
	return err == nil && len(decoded) == 12
}

func exportTimeout(request ExportRequest, source ExportSource) time.Duration {
	if request.Mode != ExportModeSource {
		// 六路高保真在 ROCK 3A 上可能慢于实时速度，超时只防止失控进程，不能误杀正常长导出。
		timeout := time.Duration(request.DurationSeconds*6*float64(time.Second)) + 5*time.Minute
		return max(timeout, 10*time.Minute)
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
		"original": {width: 1280, height: 960, fps: 36},
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
	useHEVC := request.Quality == "high" || request.Quality == "original"
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
			} else if useHEVC {
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
		cameraLabel := fmt.Sprintf("camera%d", cameraIndex)
		if len(segments) == 0 {
			// 某一路整段缺失时必须保留绝对时间位置，不能让其他视角前移补位。
			filters = append(filters, fmt.Sprintf("color=c=black:s=%dx%d:r=%d:d=%.6f,format=nv12[%s]", tile.width, tile.height, profile.fps, request.DurationSeconds, cameraLabel))
			cameraLabels = append(cameraLabels, "["+cameraLabel+"]")
			continue
		}
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
		filters = append(filters, strings.Join(cameraLabels, "")+fmt.Sprintf("xstack=inputs=%d:layout=%s:fill=black:shortest=0[%s]", len(tiles), strings.Join(positions, "|"), outputLabel))
	}
	if len(subtitlePaths) > 0 && subtitlePaths[0] != "" {
		if effectiveExportOverlayMode(request) == ExportOverlayTopBar {
			_, videoHeight := exportTileCanvasSize(tiles)
			barHeight := exportInformationBarHeight(videoHeight)
			if !request.BurnSEI {
				barHeight = exportTimestampBarHeight(videoHeight)
			}
			filters = append(filters, fmt.Sprintf("[%s]pad=w=iw:h=ih+%d:x=0:y=%d:color=black[canvas]", outputLabel, barHeight, barHeight))
			outputLabel = "canvas"
		}
		filters = append(filters, fmt.Sprintf("[%s]ass=filename='%s':fontsdir='/opt/teslabox/media/share/fonts'[overlay]", outputLabel, escapeFilterPath(subtitlePaths[0])))
		outputLabel = "overlay"
	}
	encoder := "h264_rkmpp"
	if useHEVC {
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
	if useHEVC {
		// RK3568 的 H.264 硬编在高细节画面中会留下残块；HEVC 路径同时兼顾兼容性、功耗和画质。
		args = append(args, "-tag:v", "hvc1")
	}
	args = append(args, "-g", strconv.Itoa(profile.fps*2), "-fps_mode", "cfr", "-t", fmt.Sprintf("%.6f", request.DurationSeconds),
		"-movflags", "+faststart", "-progress", "pipe:1", "-nostats", filepath.ToSlash(filepath.Clean(output)))
	return args, nil
}

func exportInformationBarHeight(videoHeight int) int {
	preferred := 64
	switch {
	case videoHeight >= 1080:
		preferred = 96
	case videoHeight >= 720:
		preferred = 80
	}
	return alignedExportBarHeight(videoHeight, preferred)
}

func exportTimestampBarHeight(videoHeight int) int {
	preferred := 48
	if videoHeight >= 960 {
		preferred = 64
	}
	return alignedExportBarHeight(videoHeight, preferred)
}

func alignedExportBarHeight(videoHeight, preferred int) int {
	// 最终画布高度保持 16 对齐，避免硬编器把隐式步幅补齐区暴露成底部黑边。
	return preferred + (16-(videoHeight+preferred)%16)%16
}

func writeOverlaySubtitles(path string, request ExportRequest, cues []ExportOverlayCue) (bool, error) {
	if !request.BurnTimestamp && !request.BurnSEI {
		return false, nil
	}
	mode := effectiveExportOverlayMode(request)
	canvasWidth, canvasHeight, barHeight, separateBar, err := exportOverlayCanvas(request)
	if err != nil {
		return false, err
	}
	duration := time.Duration(request.DurationSeconds * float64(time.Second))
	if duration <= 0 {
		return false, nil
	}
	fontScale := 1.0
	if !separateBar {
		fontScale = max(0.55, min(1.2, min(float64(canvasHeight)/1080, float64(canvasWidth)/1280)))
	} else {
		videoHeight := canvasHeight - barHeight
		switch {
		case videoHeight >= 1080:
			fontScale = 1
		case videoHeight >= 720:
			fontScale = 0.82
		default:
			fontScale = 0.66
		}
	}
	dashboardSize := int(math.Round(29 * fontScale))
	timeSize := int(math.Round(24 * fontScale))
	labelSize := int(math.Round(14 * fontScale))
	if separateBar {
		timeSize = scaleASS(33, fontScale)
		labelSize = scaleASS(23, fontScale)
	}
	lines := make([]string, 0, len(cues)*2+3)
	panelTop, panelLeft, panelWidth := 0, 0, canvasWidth
	if mode == ExportOverlayFloating {
		barHeight = max(scaleASS(88, fontScale), 64)
		panelTop = max(scaleASS(6, fontScale), 4)
		preferredWidth := 560
		if !request.BurnSEI {
			preferredWidth = 460
		}
		panelWidth = min(canvasWidth-scaleASS(32, fontScale), scaleASS(preferredWidth, fontScale))
		panelLeft = (canvasWidth - panelWidth) / 2
	}
	if mode == ExportOverlayTopBar || mode == ExportOverlayFloating {
		alpha := "00"
		shape := fmt.Sprintf("m 0 0 l %d 0 l %d %d l 0 %d", panelWidth, panelWidth, barHeight, barHeight)
		if mode == ExportOverlayFloating {
			alpha = "A0"
			shape = assRoundedRectanglePath(panelWidth, barHeight, scaleASS(18, fontScale))
		}
		lines = append(lines, fmt.Sprintf("Dialogue: 0,%s,%s,Canvas,,0,0,0,,{\\an7\\pos(%d,%d)\\1c&H000000&\\1a&H%s&\\p1}%s{\\p0}",
			assTimestamp(0), assTimestamp(duration), panelLeft, panelTop, alpha, shape))
	}
	timeX, timeLabelY, timeValueY := panelLeft+scaleASS(20, fontScale), panelTop+scaleASS(9, fontScale), panelTop+scaleASS(39, fontScale)
	if request.BurnTimestamp && mode == ExportOverlayTopBar {
		lines = append(lines, fmt.Sprintf("Dialogue: 1,%s,%s,TimeLabel,,0,0,0,,{\\pos(%d,%d)}录像时间", assTimestamp(0), assTimestamp(duration), timeX, timeLabelY))
	}
	dashboardX := canvasWidth / 2
	if mode == ExportOverlayTopBar && request.BurnTimestamp && request.BurnSEI {
		dashboardX = min(panelLeft+panelWidth-scaleASS(340, fontScale), max(dashboardX, panelLeft+scaleASS(710, fontScale)))
	}
	for _, cue := range cues {
		if cue.End <= cue.Start {
			continue
		}
		start, end := assTimestamp(cue.Start), assTimestamp(cue.End)
		if request.BurnTimestamp && cue.Timestamp != "" {
			if mode == ExportOverlayDashcam || mode == ExportOverlayFloating {
				lines = append(lines, fmt.Sprintf("Dialogue: 2,%s,%s,DashcamTime,,0,0,0,,%s", start, end, escapeASS(cue.Timestamp)))
			} else {
				lines = append(lines, fmt.Sprintf("Dialogue: 2,%s,%s,TimeValue,,0,0,0,,{\\pos(%d,%d)}%s", start, end, timeX, timeValueY, escapeASS(cue.Timestamp)))
			}
		}
		if request.BurnSEI {
			lines = appendExportTelemetryDashboard(lines, start, end, dashboardX, panelTop, barHeight, fontScale, cue.Telemetry)
		}
	}
	if len(lines) == 0 || (mode != ExportOverlayDashcam && len(lines) <= 1) {
		return false, nil
	}
	content := fmt.Sprintf("[Script Info]\nScriptType: v4.00+\nPlayResX: %d\nPlayResY: %d\nScaledBorderAndShadow: yes\nWrapStyle: 2\n\n", canvasWidth, canvasHeight) +
		"[V4+ Styles]\nFormat: Name,Fontname,Fontsize,PrimaryColour,SecondaryColour,OutlineColour,BackColour,Bold,Italic,Underline,StrikeOut,ScaleX,ScaleY,Spacing,Angle,BorderStyle,Outline,Shadow,Alignment,MarginL,MarginR,MarginV,Encoding\n" +
		"Style: Canvas,Noto Sans CJK SC,10,&H00000000,&H00000000,&H00000000,&H00000000,0,0,0,0,100,100,0,0,1,0,0,7,0,0,0,1\n" +
		fmt.Sprintf("Style: DashcamTime,Noto Sans CJK SC,%d,&H00FFFFFF,&H00FFFFFF,&H00000000,&H80000000,1,0,0,0,100,100,0,0,1,3,1,7,%d,0,%d,1\n", scaleASS(46, fontScale), scaleASS(14, fontScale), scaleASS(10, fontScale)) +
		fmt.Sprintf("Style: TimeLabel,Noto Sans CJK SC,%d,&H007A7171,&H007A7171,&H00000000,&H00000000,1,0,0,0,100,100,0,0,1,0,0,7,0,0,0,1\n", labelSize) +
		fmt.Sprintf("Style: TimeValue,Noto Sans CJK SC,%d,&H00FFFFFF,&H00FFFFFF,&H00000000,&H00000000,1,0,0,0,100,100,0,0,1,0,0,7,0,0,0,1\n", timeSize) +
		fmt.Sprintf("Style: Dashboard,Noto Sans CJK SC,%d,&H00FFFFFF,&H00FFFFFF,&H00000000,&H00000000,1,0,0,0,100,100,1,0,1,0,0,5,0,0,0,1\n\n", dashboardSize) +
		"[Events]\nFormat: Layer,Start,End,Style,Name,MarginL,MarginR,MarginV,Effect,Text\n" + strings.Join(lines, "\n") + "\n"
	if err := os.WriteFile(path, []byte(content), 0o640); err != nil {
		return false, fmt.Errorf("写入导出字幕: %w", err)
	}
	return true, nil
}

func exportOverlayCanvas(request ExportRequest) (width, height, barHeight int, separateBar bool, err error) {
	profile, ok := map[string]compositeProfile{
		"smooth": {width: 854, height: 480, fps: 20, bitrate: 1500}, "standard": {width: 1280, height: 720, fps: 24, bitrate: 3000},
		"high": {width: 1920, height: 1080, fps: 30, bitrate: int(highExportReservedKbps)}, "original": {width: 1280, height: 960, fps: 36},
	}[request.Quality]
	if !ok {
		return 0, 0, 0, false, &InputError{Message: "导出画质无效"}
	}
	tiles, err := exportTiles(request, profile)
	if err != nil {
		return 0, 0, 0, false, err
	}
	width, height = exportTileCanvasSize(tiles)
	separateBar = effectiveExportOverlayMode(request) == ExportOverlayTopBar
	if separateBar {
		if request.BurnSEI {
			barHeight = exportInformationBarHeight(height)
		} else {
			barHeight = exportTimestampBarHeight(height)
		}
		height += barHeight
	}
	return width, height, barHeight, separateBar, nil
}

func exportTileCanvasSize(tiles []compositeTile) (width, height int) {
	for _, tile := range tiles {
		width = max(width, tile.x+tile.width)
		height = max(height, tile.y+tile.height)
	}
	return width, height
}

func effectiveExportOverlayMode(request ExportRequest) ExportOverlayMode {
	if request.OverlayMode != "" {
		return request.OverlayMode
	}
	if request.BurnTimestamp && !request.BurnSEI {
		return ExportOverlayDashcam
	}
	return ExportOverlayTopBar
}

func scaleASS(value int, scale float64) int {
	return int(math.Round(float64(value) * scale))
}

func assRoundedRectanglePath(width, height, radius int) string {
	radius = min(max(radius, 1), min(width, height)/2)
	return fmt.Sprintf("m %[1]d 0 l %[2]d 0 b %[3]d 0 %[3]d %[1]d %[3]d %[1]d l %[3]d %[4]d b %[3]d %[5]d %[2]d %[5]d %[2]d %[5]d l %[1]d %[5]d b 0 %[5]d 0 %[4]d 0 %[4]d l 0 %[1]d b 0 0 %[1]d 0 %[1]d 0",
		radius, width-radius, width, height-radius, height)
}

func exportTelemetryASS(telemetry *ExportTelemetry) string {
	const (
		white = "{\\c&H00FFFFFF&}"
		gray  = "{\\c&H007A7171&}"
		blue  = "{\\c&H00E16A3E&}"
		green = "{\\c&H0071BF2F&}"
		red   = "{\\c&H002721E8&}"
	)
	if telemetry == nil {
		return gray + "—  方向盘 —  ◀  — km/h  ▶  刹车  电门 —%"
	}
	gear := strings.TrimSpace(telemetry.Gear)
	if gear == "" {
		gear = "—"
	}
	autopilot, assisted := exportAutopilotLabel(telemetry.Autopilot)
	steeringColor := white
	if assisted {
		steeringColor = blue
	}
	leftColor, rightColor := gray, gray
	if telemetry.BlinkerVisible && telemetry.LeftBlinker {
		leftColor = green
	}
	if telemetry.BlinkerVisible && telemetry.RightBlinker {
		rightColor = green
	}
	brakeColor := gray
	if telemetry.BrakeApplied {
		brakeColor = red
	}
	acceleratorColor := gray
	if telemetry.AcceleratorPosition > 0 {
		acceleratorColor = green
	}
	steeringSign := " "
	if telemetry.SteeringAngle > 0 {
		steeringSign = "+"
	} else if telemetry.SteeringAngle < 0 {
		steeringSign = "−"
	}
	return fmt.Sprintf("%s%s%s\\h\\h%s◉ 方向盘 %s%.1f°\\h\\h%s◀\\h\\h%s%d km/h %s%s\\h\\h%s▶\\h\\h%s● 刹车\\h\\h%s▮ 电门 %d%%",
		blue, escapeASS(gear), white, steeringColor, steeringSign, math.Abs(telemetry.SteeringAngle), leftColor,
		white, int(math.Round(telemetry.SpeedKPH)), blue, autopilot, rightColor, brakeColor, acceleratorColor, int(math.Round(telemetry.AcceleratorPosition)))
}

func appendExportTelemetryDashboard(lines []string, start, end string, centerX, top, barHeight int, scale float64, telemetry *ExportTelemetry) []string {
	const (
		white = "&H00FFFFFF&"
		gray  = "&H007A7171&"
		muted = "&H00AAA1A1&"
		blue  = "&H00E16A3E&"
		green = "&H0071BF2F&"
		red   = "&H002721E8&"
		amber = "&H000BA5F5&"
	)
	gear, autopilot := "—", "手动"
	speed, steering, accelerator := 0, 0.0, 0
	steeringColor, brakeColor, acceleratorColor := white, gray, gray
	leftColor, rightColor := gray, gray
	if telemetry != nil {
		if value := strings.TrimSpace(telemetry.Gear); value != "" {
			gear = value
		}
		autopilot, _ = exportAutopilotLabel(telemetry.Autopilot)
		speed = int(math.Round(telemetry.SpeedKPH))
		steering = telemetry.SteeringAngle
		accelerator = int(math.Round(telemetry.AcceleratorPosition))
		_, assisted := exportAutopilotLabel(telemetry.Autopilot)
		if assisted {
			steeringColor = blue
		}
		if telemetry.BlinkerVisible && telemetry.LeftBlinker {
			leftColor = green
		}
		if telemetry.BlinkerVisible && telemetry.RightBlinker {
			rightColor = green
		}
		if telemetry.BrakeApplied {
			brakeColor = red
		}
		if telemetry.AcceleratorPosition > 0 {
			acceleratorColor = white
		}
	}
	gearColor := white
	switch strings.ToUpper(gear) {
	case "D":
		gearColor = blue
	case "R":
		gearColor = amber
	case "—":
		gearColor = gray
	}
	steeringSign := " "
	if steering > 0 {
		steeringSign = "+"
	} else if steering < 0 {
		steeringSign = "−"
	}
	assistanceColor := gray
	if autopilot != "手动" {
		assistanceColor = white
	}

	gearX := centerX - scaleASS(238, scale)
	steeringX := centerX - scaleASS(158, scale)
	leftX := centerX - scaleASS(72, scale)
	speedX := centerX
	rightX := centerX + scaleASS(72, scale)
	brakeX := centerX + scaleASS(158, scale)
	acceleratorX := centerX + scaleASS(238, scale)
	// 图标中心与“时速 + km/h”组合的视觉中心对齐，避免四个状态项悬在顶部。
	iconTop := top + scaleASS(15, scale)
	labelY := top + barHeight - scaleASS(27, scale)
	assistanceY := top + barHeight - scaleASS(22, scale)

	lines = append(lines,
		assDashboardText(start, end, gearX, top+scaleASS(10, scale), scaleASS(50, scale), gearColor, gear),
		assDashboardText(start, end, gearX, labelY, scaleASS(24, scale), white, "档位"),
		assDashboardText(start, end, steeringX, labelY, scaleASS(24, scale), white, fmt.Sprintf("方向盘 %s%.1f°", steeringSign, math.Abs(steering))),
		assDashboardText(start, end, speedX, top, scaleASS(60, scale), white, strconv.Itoa(speed)),
		assDashboardText(start, end, speedX, top+scaleASS(48, scale), scaleASS(22, scale), muted, "km/h"),
		assDashboardText(start, end, brakeX, labelY, scaleASS(24, scale), white, "刹车"),
		assDashboardText(start, end, acceleratorX, labelY, scaleASS(24, scale), white, fmt.Sprintf("电门 %d%%", accelerator)),
	)
	if autopilot != "手动" {
		badgeWidth := scaleASS(54, scale)
		if len(autopilot) >= 3 {
			badgeWidth = scaleASS(68, scale)
		}
		lines = append(lines, assDashboardBadge(start, end, speedX, assistanceY-scaleASS(2, scale), badgeWidth, scaleASS(24, scale), blue))
	}
	lines = append(lines, assDashboardText(start, end, speedX, assistanceY, scaleASS(20, scale), assistanceColor, autopilot))
	lines = appendSteeringWheelASS(lines, start, end, steeringX, iconTop, scale, steeringColor, steering)
	lines = appendBlinkerASS(lines, start, end, leftX, iconTop, scale, leftColor, true)
	lines = appendBlinkerASS(lines, start, end, rightX, iconTop, scale, rightColor, false)
	lines = appendPedalASS(lines, start, end, brakeX, iconTop, scale, brakeColor, true, 0)
	lines = appendPedalASS(lines, start, end, acceleratorX, iconTop, scale, acceleratorColor, false, accelerator)
	return lines
}

func assDashboardText(start, end string, x, y, size int, color, value string) string {
	return fmt.Sprintf("Dialogue: 4,%s,%s,Dashboard,,0,0,0,,{\\an8\\pos(%d,%d)\\fs%d\\1c%s\\b1}%s", start, end, x, y, size, color, escapeASS(value))
}

func assDashboardBadge(start, end string, centerX, top, width, height int, color string) string {
	left := centerX - width/2
	path := assRoundedRectanglePath(width, height, height/2)
	return fmt.Sprintf("Dialogue: 3,%s,%s,Canvas,,0,0,0,,{\\an7\\pos(%d,%d)\\1c%s\\1a&H20&\\3c&H00FFB96E&\\3a&H30&\\bord1\\shad0\\p1}%s{\\p0}",
		start, end, left, top, color, path)
}

func assDashboardVector(start, end string, layer, x, y int, scale float64, color, path, extra string) string {
	percent := int(math.Round(scale * 100))
	return fmt.Sprintf("Dialogue: %d,%s,%s,Canvas,,0,0,0,,{\\an7\\pos(%d,%d)\\fscx%d\\fscy%d\\1c%s%s\\p1}%s{\\p0}",
		layer, start, end, x-scaleASS(20, scale), y, percent, percent, color, extra, path)
}

func appendSteeringWheelASS(lines []string, start, end string, centerX, top int, scale float64, color string, angle float64) []string {
	path := "m 20.4 1.99 b 21.1 1.97 21.8 1.97 22.5 1.99 b 29.08 2.44 34.71 6.34 37.31 12.41 b 40 18.04 38.77 25.63 34.66 30.24 b 34.58 30.28 34.52 30.33 34.45 30.38 b 33.9 30.94 33.34 31.51 32.79 32.08 b 30.97 34.03 26.55 35.91 23.86 36.14 b 10.41 38.03 0 24.87 5.53 12.34 b 8.11 6.26 13.87 2.44 20.4 1.99 c m 8.59 14.96 b 8.63 17.41 12.3 15.67 13.62 15.51 b 20.04 14.02 25.1 14.28 31.42 16.05 b 32.8 16.47 34.63 16.27 33.96 14.33 b 30.98 6.48 21.43 3.91 14.32 7.85 b 11.81 9.22 9.3 12.19 8.59 14.96 c m 10.82 27.8 b 10.81 27.88 10.85 27.93 10.93 27.93 b 11.72 29.37 17.53 33.72 18.13 30.45 b 18.04 26.33 15.7 22.47 11.35 21.9 b 9.45 21.71 7.96 22.63 8.83 24.7 b 9.32 25.73 10.02 26.98 10.82 27.8 c m 26.65 24.44 b 26.57 24.45 26.53 24.5 26.53 24.58 b 25.41 25.95 23.32 30.17 25.05 31.56 b 26.28 32.35 29.32 30.16 30.32 29.37 b 30.56 29.37 30.76 29.27 30.9 29.07 b 31.11 28.76 31.83 28.33 31.76 27.93 b 32.81 26.7 35.69 22.61 32.72 21.96 b 30.77 21.55 27.75 22.81 26.65 24.44 c"
	angle = min(540, max(-540, angle))
	rotation := fmt.Sprintf("\\org(%d,%d)\\frz%.2f", centerX, top+scaleASS(20, scale), angle)
	// 直接复用在线播放方向盘的透明复合路径，保证造型和旋转中心一致。
	return append(lines, assDashboardVector(start, end, 5, centerX, top, scale, color, path, rotation))
}

func appendBlinkerASS(lines []string, start, end string, centerX, top int, scale float64, color string, left bool) []string {
	path := "m 4 20 b 4 18 5 17 7 15 l 17 6 b 20 3 24 5 24 9 l 24 14 l 34 14 b 38 14 40 17 40 20 b 40 23 38 26 34 26 l 24 26 l 24 31 b 24 35 20 37 17 34 l 7 25 b 5 23 4 22 4 20"
	if !left {
		path = "m 36 20 b 36 18 35 17 33 15 l 23 6 b 20 3 16 5 16 9 l 16 14 l 6 14 b 2 14 0 17 0 20 b 0 23 2 26 6 26 l 16 26 l 16 31 b 16 35 20 37 23 34 l 33 25 b 35 23 36 22 36 20"
	}
	return append(lines, assDashboardVector(start, end, 5, centerX, top, scale, color, path, ""))
}

func appendPedalASS(lines []string, start, end string, centerX, top int, scale float64, color string, brake bool, level int) []string {
	path := "m 20 1 b 9 1 1 9 1 20 b 1 31 9 39 20 39 b 31 39 39 31 39 20 b 39 9 31 1 20 1"
	if brake {
		path += " m 25 4 b 25 10 23 14 20 18 m 10 18 l 30 18 l 28 32 l 12 32 l 10 18 m 14 22 l 14 28 m 17 22 l 17 28 m 20 22 l 20 28 m 23 22 l 23 28 m 26 22 l 26 28"
	} else {
		path += " m 20 4 l 20 10 m 14 10 b 14 7 26 7 26 10 l 26 32 b 26 36 14 36 14 32 l 14 10 m 18 14 l 18 30 m 22 14 l 22 30"
	}
	outline := fmt.Sprintf("\\1a&HFF&\\3c%s\\bord%.1f\\shad0", color, max(1.5, 2.2*scale))
	lines = append(lines, assDashboardVector(start, end, 5, centerX, top, scale, color, path, outline))
	if brake || level <= 0 {
		return lines
	}
	level = min(100, max(0, level))
	iconLeft := centerX - scaleASS(20, scale)
	clipTop := top + scaleASS(40*(100-level)/100, scale)
	clipRight := iconLeft + scaleASS(40, scale)
	clipBottom := top + scaleASS(40, scale)
	clip := fmt.Sprintf("\\clip(%d,%d,%d,%d)\\1a&HFF&\\3c&H0071BF2F&\\bord%.1f\\shad0", iconLeft, clipTop, clipRight, clipBottom, max(1.5, 2.2*scale))
	return append(lines, assDashboardVector(start, end, 6, centerX, top, scale, "&H0071BF2F&", path, clip))
}

func exportAutopilotLabel(value string) (string, bool) {
	source := strings.TrimSpace(value)
	normalized := strings.ToUpper(source)
	switch {
	case strings.Contains(normalized, "FSD"):
		return "FSD", true
	case normalized == "EAP":
		return "EAP", true
	case normalized == "AP" || strings.Contains(normalized, "AUTOSTEER") || strings.Contains(source, "自动辅助转向"):
		return "AP", true
	case strings.Contains(normalized, "TACC"):
		return "TACC", false
	default:
		return "手动", false
	}
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
	if request.Quality == "high" {
		return highExportTiles(request.Layout)
	}
	return teslaLayoutTiles(request.Layout, profile.height)
}

func highExportTiles(layout string) ([]compositeTile, error) {
	// 整体画布必须直接满足 RKMPP 的 16 像素边界；不能在合成后补黑边，否则补齐区域会成为成片的可见像素。
	switch layout {
	case "composite6":
		return gridTiles(
			[]string{"left_pillar", "front", "right_pillar", "left_repeater", "back", "right_repeater"},
			[]int{842, 844, 842}, []int{544, 544},
		), nil
	case "composite4a":
		return gridTiles(
			[]string{"front", "back", "left_repeater", "right_repeater"},
			[]int{840, 840}, []int{544, 544},
		), nil
	case "composite4b":
		return []compositeTile{
			{camera: "front", width: 1264, height: 816},
			{camera: "right_repeater", y: 816, width: 422, height: 272},
			{camera: "back", x: 422, y: 816, width: 420, height: 272},
			{camera: "left_repeater", x: 842, y: 816, width: 422, height: 272},
		}, nil
	case "composite3":
		return []compositeTile{
			{camera: "front", width: 1120, height: 726},
			{camera: "right_repeater", y: 726, width: 560, height: 362},
			{camera: "left_repeater", x: 560, y: 726, width: 560, height: 362},
		}, nil
	default:
		return nil, &InputError{Message: "不支持的合成布局"}
	}
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
