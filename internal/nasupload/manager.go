package nasupload

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/wen-ryon/teslabox/internal/recordingcache"
)

// Config 将任务日志和压缩成品固定到系统盘。
type Config struct {
	Logger    *slog.Logger
	Root      string
	Cache     *recordingcache.Store
	Transport Transport
	Processor Processor
}
type output struct {
	Inputs []Input `json:"inputs"`
	Path   string  `json:"path"`
	Name   string  `json:"name"`
	Ready  bool    `json:"ready,omitempty"`
	SHA256 string  `json:"sha256,omitempty"`
	Bytes  uint64  `json:"bytes,omitempty"`
}
type job struct {
	Task     Task     `json:"task"`
	Target   Target   `json:"target"`
	Identity string   `json:"identity"`
	Inputs   []Input  `json:"inputs"`
	Prepared bool     `json:"prepared"`
	Outputs  []output `json:"outputs"`
}
type diskState struct {
	DefaultTargetID string                     `json:"default_target_id,omitempty"`
	Version         int                        `json:"version"`
	Targets         []Target                   `json:"targets"`
	Jobs            []*job                     `json:"jobs"`
	Automatic       AutoConfig                 `json:"automatic"`
	AutoBaselines   map[string]map[string]bool `json:"auto_baselines,omitempty"`
	AutoEvents      []AutoEvent                `json:"auto_events,omitempty"`
}
type preparation struct {
	id            string
	root, version string
	mediaContext  context.Context
	release       func()
}

// Manager 分开暂存和网络工作进程，上传慢时也能尽早释放其他任务的录像盘快照。
type Manager struct {
	cfg      Config
	mu       sync.Mutex
	state    diskState
	leases   map[string]*recordingcache.Lease
	reserves map[string]func()
	cancels  map[string]context.CancelFunc
	prepare  chan preparation
	wake     chan struct{}
	done     chan struct{}
	stopping bool
	autoScan AutoScanStatus
}

// New 必须先于缓存定时清理恢复固定，避免重启时待上传输入被 TTL 删除。
func New(cfg Config) (*Manager, error) {
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	if cfg.Cache == nil || cfg.Transport == nil || cfg.Root == "" {
		return nil, errors.New("上传依赖不完整")
	}
	root, err := filepath.Abs(cfg.Root)
	if err != nil {
		return nil, err
	}
	cfg.Root = root
	if err = os.MkdirAll(root, 0o700); err != nil {
		return nil, err
	}
	if err = cfg.Cache.ValidateWorkspace(root); err != nil {
		return nil, err
	}
	m := &Manager{cfg: cfg, state: diskState{Version: 1}, leases: map[string]*recordingcache.Lease{}, reserves: map[string]func(){}, cancels: map[string]context.CancelFunc{}, prepare: make(chan preparation, 8), wake: make(chan struct{}, 1), done: make(chan struct{})}
	b, err := os.ReadFile(filepath.Join(root, "state.json"))
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	if len(b) > 0 {
		if err = json.Unmarshal(b, &m.state); err != nil || m.state.Version != 1 {
			return nil, errors.New("上传状态文件损坏，拒绝丢弃历史记录")
		}
	}
	for _, j := range m.state.Jobs {
		if j == nil || len(j.Task.ID) != 32 || j.Task.Request.Quality == nil || validateQuality(*j.Task.Request.Quality) != nil || validateTarget(j.Target) != nil {
			return nil, errors.New("上传计划结构无效，拒绝丢弃历史记录")
		}
		if _, err := hex.DecodeString(j.Task.ID); err != nil {
			return nil, errors.New("上传计划标识无效")
		}
		if terminal(j.Task.State) {
			continue
		}
		if !j.Prepared {
			j.Task.State = "failed"
			j.Task.Error = "暂存完成前设备重启，请重新选择录像"
			j.Inputs = nil
			continue
		}
		paths := make([]string, 0, len(j.Inputs))
		for _, in := range j.Inputs {
			paths = append(paths, in.Path)
		}
		lease, e := cfg.Cache.PinLocalForRecovery(context.Background(), paths)
		if e != nil {
			j.Task.State = "failed"
			j.Task.Error = "暂存录像缺失，请重新选择录像"
			j.Prepared = false
			continue
		}
		m.leases[j.Task.ID] = lease
		if j.Task.State != "failed" {
			j.Task.State = "ready"
			j.Task.NextRetryAt = time.Time{}
		}
	}
	if err = m.saveLocked(); err != nil {
		for _, l := range m.leases {
			l.Release()
		}
		return nil, err
	}
	return m, nil
}

func terminal(s string) bool { return s == "completed" || s == "canceled" }
func newID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic(err)
	}
	return hex.EncodeToString(b[:])
}
func copyTask(t Task) Task {
	b, _ := json.Marshal(t)
	var out Task
	_ = json.Unmarshal(b, &out)
	return out
}

// Targets 不将可逆的 rclone 凭据返回到浏览器。
func (m *Manager) Targets() []Target {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]Target, 0, len(m.state.Targets))
	for _, t := range m.state.Targets {
		t.IsDefault = t.ID == m.state.DefaultTargetID
		out = append(out, publicTarget(t))
	}
	return out
}

// SaveTarget 保留留空的密码，并给每次配置生成修订，待执行任务仍使用原来的落点。
func (m *Manager) SaveTarget(ctx context.Context, t Target) (Target, error) {
	return m.saveTarget(ctx, t, false)
}

// SaveServer 保留持锁时的已保存画质，避免服务器草稿覆盖另一次画质保存。
func (m *Manager) SaveServer(ctx context.Context, t Target) (Target, error) {
	return m.saveTarget(ctx, t, true)
}

func (m *Manager) saveTarget(ctx context.Context, t Target, serverOnly bool) (Target, error) {
	if serverOnly {
		t.Quality = Quality{Mode: "original", Height: 720, FPS: 24, BitrateKbps: 3000, Layout: "separate"}
	}
	t.IsDefault = false
	t.Credential = ""
	t.HasPassword = false
	t.Revision = ""
	if err := validateTarget(t); err != nil {
		return Target{}, err
	}
	if t.Password != "" {
		if len(t.Password) > 1024 {
			return Target{}, errors.New("密码过长")
		}
		credential, err := m.cfg.Transport.Obscure(ctx, t.Password)
		if err != nil {
			return Target{}, err
		}
		t.Credential = credential
	}
	t.Password = ""
	m.mu.Lock()
	defer m.mu.Unlock()
	index := -1
	for i, old := range m.state.Targets {
		if old.ID == t.ID && t.ID != "" {
			index = i
			if serverOnly {
				t.Quality = old.Quality
			}
			if t.Credential == "" {
				t.Credential = old.Credential
			}
			break
		}
	}
	if index < 0 {
		if t.ID != "" {
			return Target{}, errors.New("上传目标不存在")
		}
		if len(m.state.Targets) >= 20 {
			return Target{}, errors.New("最多配置 20 个上传目标")
		}
		t.ID = newID()
	}
	if t.Credential == "" {
		return Target{}, errors.New("请为专用上传账号设置密码")
	}
	t.Revision = digest(t)[:24]
	previous := slices.Clone(m.state.Targets)
	if index < 0 {
		m.state.Targets = append(m.state.Targets, t)
	} else {
		m.state.Targets[index] = t
	}
	if err := m.saveLocked(); err != nil {
		m.state.Targets = previous
		return Target{}, err
	}
	t.IsDefault = t.ID == m.state.DefaultTargetID
	return publicTarget(t), nil
}

// DeleteTarget 不影响已经排队且固定了配置的任务。
func (m *Manager) DeleteTarget(id string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	previous := slices.Clone(m.state.Targets)
	m.state.Targets = slices.DeleteFunc(slices.Clone(previous), func(t Target) bool { return t.ID == id })
	if len(previous) == len(m.state.Targets) {
		return errors.New("上传目标不存在")
	}
	previousDefault := m.state.DefaultTargetID
	if id == previousDefault {
		m.state.DefaultTargetID = ""
	}
	if err := m.saveLocked(); err != nil {
		m.state.Targets = previous
		m.state.DefaultTargetID = previousDefault
		return err
	}
	return nil
}

// TestTarget 在专用目录创建小样本、读回校验并删除，仅验证该账号的实际读写权限。
func (m *Manager) TestTarget(ctx context.Context, id string) error {
	m.mu.Lock()
	var target Target
	for _, t := range m.state.Targets {
		if t.ID == id {
			target = t
		}
	}
	m.mu.Unlock()
	if target.ID == "" {
		return errors.New("上传目标不存在")
	}
	return m.cfg.Transport.Test(ctx, target)
}

// Create 接管快照租约；无论排队是否成功都必须恰好释放一次。
func (m *Manager) Create(req Request, identity, root, version string, inputs []Input, mediaContext context.Context, release func()) (Task, error) {
	owned := false
	defer func() {
		if !owned && release != nil {
			release()
		}
	}()
	if identity == "" || len(inputs) == 0 || len(inputs) > 120 {
		return Task{}, errors.New("请选择 1–120 个有效录像文件")
	}
	var bytes uint64
	for _, in := range inputs {
		rel, err := filepath.Rel(root, in.Path)
		if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) || filepath.IsAbs(rel) || in.Size > 4<<30 || in.MomentID == "" {
			return Task{}, errors.New("录像文件超出快照范围")
		}
		bytes += in.Size
	}
	if bytes > 4<<30 {
		return Task{}, errors.New("一次上传最多选择 4 GiB 原始录像")
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.stopping {
		return Task{}, errors.New("上传服务正在停止，稍后重试")
	}
	if req.AutoEventID != "" {
		index := slices.IndexFunc(m.state.AutoEvents, func(e AutoEvent) bool { return e.ID == req.AutoEventID && e.Identity == identity })
		if index < 0 || m.state.AutoEvents[index].State == "canceled" {
			return Task{}, errors.New("自动事件不存在或已取消")
		}
		if !m.state.Automatic.Enabled {
			return Task{}, errors.New("自动上传已关闭")
		}
	}
	if req.AutoKey != "" {
		for _, previous := range m.state.Jobs {
			if previous.Task.Request.AutoKey != req.AutoKey {
				continue
			}
			if previous.Prepared || previous.Task.State != "failed" {
				return copyTask(previous.Task), nil
			}
			if len(m.prepare) >= cap(m.prepare) {
				return Task{}, errors.New("录像准备队列已满，自动事件稍后重试")
			}
			old := *previous
			previous.Inputs = slices.Clone(inputs)
			previous.Task.State = "copying"
			previous.Task.Error = ""
			if err := m.saveLocked(); err != nil {
				*previous = old
				return Task{}, err
			}
			m.prepare <- preparation{id: previous.Task.ID, root: root, version: version, mediaContext: mediaContext, release: release}
			owned = true
			return copyTask(previous.Task), nil
		}
	}
	var target Target
	if req.AutoEventID != "" {
		for _, event := range m.state.AutoEvents {
			if event.ID == req.AutoEventID && event.Identity == identity {
				if event.State == "canceled" {
					return Task{}, errors.New("自动事件已取消")
				}
				target = event.Target
				break
			}
		}
	} else {
		for _, t := range m.state.Targets {
			if t.ID == m.state.DefaultTargetID {
				target = t
			}
		}
	}
	if target.ID == "" {
		return Task{}, errors.New("请先在设置中指定默认上传目标")
	}
	if req.TargetID != "" && req.TargetID != target.ID {
		return Task{}, errors.New("默认上传目标已变更，请重新打开上传窗口")
	}
	req.TargetID = target.ID
	q := target.Quality
	if req.Quality != nil {
		q = *req.Quality
	}
	if err := validateQuality(q); err != nil {
		return Task{}, err
	}
	if q.Mode == "compressed" && q.Layout != "separate" {
		expected := map[string][]string{
			"composite6":  {"left_pillar", "front", "right_pillar", "left_repeater", "back", "right_repeater"},
			"composite4a": {"front", "back", "left_repeater", "right_repeater"},
			"composite4b": {"front", "back", "left_repeater", "right_repeater"},
			"composite3":  {"front", "left_repeater", "right_repeater"},
		}[q.Layout]
		if len(req.Cameras) != len(expected) {
			return Task{}, errors.New("合并布局需要选择对应视角")
		}
		for _, camera := range expected {
			if !slices.Contains(req.Cameras, camera) {
				return Task{}, errors.New("合并布局与所选视角不一致")
			}
		}
	}
	req.Quality = &q
	pending := 0
	for _, j := range m.state.Jobs {
		if !terminal(j.Task.State) && (j.Prepared || j.Task.State != "failed") {
			pending++
		}
	}
	if pending >= 8 || len(m.prepare) >= cap(m.prepare) {
		return Task{}, errors.New("上传队列已满，等待现有任务处理后重试")
	}
	task := Task{ID: newID(), Request: req, TargetName: target.Name, TargetRevision: target.Revision, State: "copying", CreatedAt: time.Now().UTC(), Files: []FileResult{}}
	j := &job{Task: task, Target: target, Identity: identity, Inputs: slices.Clone(inputs)}
	m.state.Jobs = append(m.state.Jobs, j)
	if err := m.saveLocked(); err != nil {
		m.state.Jobs = m.state.Jobs[:len(m.state.Jobs)-1]
		return Task{}, err
	}
	m.prepare <- preparation{id: task.ID, root: root, version: version, mediaContext: mediaContext, release: release}
	owned = true
	return copyTask(task), nil
}

// History 仅返回不含凭据和本地绝对路径的任务副本。
func (m *Manager) History() []Task {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]Task, 0, len(m.state.Jobs))
	for i := len(m.state.Jobs) - 1; i >= 0; i-- {
		out = append(out, copyTask(m.state.Jobs[i].Task))
	}
	return out
}

// Counts 同一录像分钟在多个视角、画质、目标上的成功记录只计一次。
func (m *Manager) Counts(identity string) map[string]int {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := map[string]int{}
	for _, j := range m.state.Jobs {
		if j.Identity != identity {
			continue
		}
		for _, f := range j.Task.Files {
			out[f.MomentID] = 1
			out[fmt.Sprintf("at:%d", f.RecordedAt.Unix()/60)] = 1
		}
	}
	return out
}

// Retry 只重用已经完整暂存的文件；部分成功结果不重复计数。
func (m *Manager) Retry(id string) (Task, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	j := m.findLocked(id)
	if j == nil || !j.Prepared || (j.Task.State != "failed" && j.Task.State != "waiting_retry") {
		return Task{}, errors.New("该任务不能重试，请重新选择录像")
	}
	old := j.Task
	j.Task.State = "ready"
	j.Task.Attempts = 0
	j.Task.Error = ""
	j.Task.NextRetryAt = time.Time{}
	if err := m.saveLocked(); err != nil {
		j.Task = old
		return Task{}, err
	}
	m.signal()
	return copyTask(j.Task), nil
}

// Cancel 先持久化取消状态，再释放输入固定，断电后也不会继续已取消的上传。
func (m *Manager) Cancel(id string) (Task, error) {
	m.mu.Lock()
	j := m.findLocked(id)
	if j == nil {
		m.mu.Unlock()
		return Task{}, errors.New("上传任务不存在")
	}
	if terminal(j.Task.State) {
		t := copyTask(j.Task)
		m.mu.Unlock()
		return t, nil
	}
	old := j.Task
	j.Task.State = "canceled"
	j.Task.Error = ""
	if err := m.saveLocked(); err != nil {
		j.Task = old
		m.mu.Unlock()
		return Task{}, err
	}
	if cancel := m.cancels[id]; cancel != nil {
		cancel()
	} else {
		m.releaseLocked(id)
		m.removeOutputsLocked(j)
	}
	t := copyTask(j.Task)
	m.mu.Unlock()
	m.signal()
	return t, nil
}

// Busy 阻止更新在暂存或传输过程中重启设备。
func (m *Manager) Busy() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, j := range m.state.Jobs {
		if !terminal(j.Task.State) && j.Task.State != "failed" {
			return true
		}
	}
	return false
}

// Run 退出前等待两个工作进程停止，避免停止服务后仍访问录像或网络。
func (m *Manager) Run(ctx context.Context) {
	defer close(m.done)
	var wg sync.WaitGroup
	wg.Add(1)
	go func() { defer wg.Done(); m.prepareLoop(ctx) }()
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for ctx.Err() == nil {
		m.mu.Lock()
		var selected *job
		for _, j := range m.state.Jobs {
			if j.Task.State == "ready" || j.Task.State == "waiting_retry" && !time.Now().Before(j.Task.NextRetryAt) {
				selected = j
				break
			}
		}
		m.mu.Unlock()
		if selected != nil {
			m.transfer(ctx, selected.Task.ID)
			continue
		}
		select {
		case <-ctx.Done():
		case <-ticker.C:
		case <-m.wake:
		}
	}
	m.mu.Lock()
	m.stopping = true
	m.mu.Unlock()
	wg.Wait()
	for {
		select {
		case p := <-m.prepare:
			if p.release != nil {
				p.release()
			}
		default:
			return
		}
	}
}

// Wait 配合主服务的有限关机窗口。
func (m *Manager) Wait(ctx context.Context) error {
	select {
	case <-m.done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}
func (m *Manager) signal() {
	select {
	case m.wake <- struct{}{}:
	default:
	}
}
func (m *Manager) findLocked(id string) *job {
	for _, j := range m.state.Jobs {
		if j.Task.ID == id {
			return j
		}
	}
	return nil
}

func (m *Manager) prepareLoop(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case p := <-m.prepare:
			m.stage(ctx, p)
		}
	}
}

func (m *Manager) stage(parent context.Context, p preparation) {
	if p.release != nil {
		defer p.release()
	}
	ctx, cancel := context.WithTimeout(parent, 5*time.Minute)
	defer cancel()
	stop := func() bool { return false }
	if p.mediaContext != nil {
		stop = context.AfterFunc(p.mediaContext, cancel)
	}
	defer stop()
	m.mu.Lock()
	j := m.findLocked(p.id)
	if j == nil || j.Task.State == "canceled" {
		m.mu.Unlock()
		return
	}
	inputs := slices.Clone(j.Inputs)
	identity := j.Identity
	m.cancels[p.id] = cancel
	m.mu.Unlock()
	files := make([]recordingcache.File, 0, len(inputs))
	for _, in := range inputs {
		rel, _ := filepath.Rel(p.root, in.Path)
		files = append(files, recordingcache.File{Identity: identity + "|" + filepath.ToSlash(rel), Version: p.version, Path: in.Path})
	}
	lease, err := m.cfg.Cache.EnsureLocal(ctx, files, nil)
	if err != nil {
		m.cfg.Logger.Warn("录像上传准备失败", "task_id", p.id, "snapshot_id", p.version, "source_count", len(files), "error", err, "context_cause", context.Cause(ctx))
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.cancels, p.id)
	if j.Task.State == "canceled" {
		if lease != nil {
			lease.Release()
		}
		return
	}
	if err != nil {
		j.Task.State = "failed"
		j.Task.Error = "准备系统盘录像失败，请刷新录像并重试"
		j.Inputs = nil
		_ = m.saveLocked()
		return
	}
	for i := range inputs {
		inputs[i].Path = lease.Paths[inputs[i].Path]
	}
	j.Inputs = inputs
	j.Prepared = true
	j.Task.State = "ready"
	if err = m.saveLocked(); err != nil {
		j.Prepared = false
		j.Task.State = "failed"
		j.Task.Error = "保存上传计划失败"
		lease.Release()
		return
	}
	m.leases[p.id] = lease
	m.signal()
}

func (m *Manager) transfer(parent context.Context, id string) {
	ctx, cancel := context.WithTimeout(parent, 2*time.Hour)
	defer cancel()
	m.mu.Lock()
	j := m.findLocked(id)
	if j == nil || terminal(j.Task.State) {
		m.mu.Unlock()
		return
	}
	j.Task.State = "uploading"
	j.Task.Attempts++
	j.Task.Error = ""
	m.cancels[id] = cancel
	if err := m.saveLocked(); err != nil {
		j.Task.State = "failed"
		j.Task.Error = "无法保存上传状态"
		delete(m.cancels, id)
		m.mu.Unlock()
		return
	}
	m.mu.Unlock()
	err := m.perform(ctx, j)
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.cancels, id)
	if release := m.reserves[id]; release != nil {
		release()
		delete(m.reserves, id)
	}
	if j.Task.State == "canceled" {
		m.releaseLocked(id)
		m.removeOutputsLocked(j)
		return
	}
	if parent.Err() != nil {
		j.Task.State = "ready"
		j.Task.Error = "设备停止，重新启动后继续"
		_ = m.saveLocked()
		return
	}
	if err != nil {
		j.Task.Error = err.Error()
		if permanentUploadError(err) || j.Task.Attempts >= 5 && j.Task.Request.AutoEventID == "" {
			j.Task.State = "failed"
		} else {
			j.Task.State = "waiting_retry"
			j.Task.NextRetryAt = time.Now().Add(min(time.Duration(1<<min(j.Task.Attempts, 6))*15*time.Second, 15*time.Minute))
		}
		_ = m.saveLocked()
		return
	}
	j.Task.State = "completed"
	j.Task.NextRetryAt = time.Time{}
	if err = m.saveLocked(); err != nil {
		j.Task.State = "failed"
		j.Task.Error = "结果保存失败，可重试核验远端文件"
		return
	}
	m.releaseLocked(id)
	m.removeOutputsLocked(j)
}

func (m *Manager) perform(ctx context.Context, j *job) error {
	paths := make([]string, 0, len(j.Inputs))
	for _, in := range j.Inputs {
		paths = append(paths, in.Path)
	}
	lease, err := m.cfg.Cache.AcquireLocal(ctx, paths)
	if err != nil {
		return errors.New("系统盘输入校验失败，请取消并重新选择录像")
	}
	defer lease.Release()
	q := *j.Task.Request.Quality
	groups := make([][]Input, 0, len(j.Inputs))
	if q.Mode == "compressed" && q.Layout != "separate" {
		for _, in := range j.Inputs {
			index := slices.IndexFunc(groups, func(g []Input) bool { return g[0].MomentID == in.MomentID })
			if index < 0 {
				groups = append(groups, []Input{in})
			} else {
				groups[index] = append(groups[index], in)
			}
		}
	} else {
		for _, in := range j.Inputs {
			groups = append(groups, []Input{in})
		}
	}
	m.mu.Lock()
	j.Task.TotalFiles = len(groups)
	m.mu.Unlock()
	for i, group := range groups {
		if err := ctx.Err(); err != nil {
			return errors.New("上传已停止或超时")
		}
		cameras := make([]string, 0, len(group))
		for _, in := range group {
			cameras = append(cameras, in.Camera)
		}
		m.mu.Lock()
		verified := slices.ContainsFunc(j.Task.Files, func(f FileResult) bool { return f.MomentID == group[0].MomentID && slices.Equal(f.Cameras, cameras) })
		m.mu.Unlock()
		if verified {
			continue
		}
		name := group[0].Name
		local := group[0].Path
		if q.Mode == "compressed" {
			name = strings.TrimSuffix(name, filepath.Ext(name)) + "-compressed.mp4"
			if q.Layout != "separate" {
				name = group[0].RecordedAt.Format("2006-01-02_15-04-05") + "-" + q.Layout + ".mp4"
			}
			local = filepath.Join(m.cfg.Root, j.Task.ID, fmt.Sprintf("%03d.mp4", i))
			m.mu.Lock()
			index := slices.IndexFunc(j.Outputs, func(o output) bool { return o.Path == local })
			var saved output
			if index >= 0 {
				saved = j.Outputs[index]
			}
			m.mu.Unlock()
			// 完成标记只在成品刷盘后保存；重启核对摘要，绝不续用半成品。
			hash, size, checkErr := fileHash(ctx, local)
			reusable := saved.Ready && checkErr == nil && hash == saved.SHA256 && size == saved.Bytes
			if !reusable {
				if m.cfg.Processor == nil {
					return errors.New("压缩工具不可用")
				}
				if err = os.MkdirAll(filepath.Dir(local), 0o700); err != nil {
					return errors.New("无法创建系统盘压缩目录")
				}
				// 编码峰值按所选码率的四倍预算，成品和临时文件也计入公共系统盘额度。
				reserve, e := m.cfg.Cache.Reserve(ctx, uint64(q.BitrateKbps)*1000/8*120*4)
				if e != nil {
					return errors.New("系统盘空间不足，无法压缩上传")
				}
				m.mu.Lock()
				m.reserves[j.Task.ID] = reserve
				m.mu.Unlock()
				m.mu.Lock()
				if index < 0 {
					j.Outputs = append(j.Outputs, output{Inputs: group, Path: local, Name: name})
					index = len(j.Outputs) - 1
				} else {
					j.Outputs[index].Ready = false
				}
				e = m.saveLocked()
				m.mu.Unlock()
				if e != nil {
					return errors.New("保存压缩成品计划失败")
				}
				err = m.cfg.Processor.Process(ctx, q, group, local)
				if err != nil {
					return err
				}
				f, e := os.OpenFile(local, os.O_RDWR, 0)
				if e != nil {
					return errors.New("无法刷盘压缩成品")
				}
				e = f.Sync()
				closeErr := f.Close()
				if e != nil || closeErr != nil {
					return errors.New("压缩成品刷盘失败")
				}
				hash, size, e := fileHash(ctx, local)
				if e != nil {
					return errors.New("无法校验压缩成品")
				}
				m.mu.Lock()
				j.Outputs[index].SHA256, j.Outputs[index].Bytes, j.Outputs[index].Ready = hash, size, true
				e = m.saveLocked()
				if e != nil {
					j.Outputs[index].Ready = false
				}
				m.mu.Unlock()
				if e != nil {
					return errors.New("保存压缩成品状态失败")
				}
			}
		}
		hash, size, e := fileHash(ctx, local)
		if e != nil {
			return errors.New("无法校验上传文件")
		}
		remote := remoteFile(j.Target, j.Identity, group, q, hash, name)
		if err = m.cfg.Transport.Upload(ctx, j.Target, local, remote, hash, j.Task.ID); err != nil {
			return err
		}
		result := FileResult{MomentID: group[0].MomentID, Cameras: cameras, Name: name, RemotePath: remote, SHA256: hash, Bytes: size, VerifiedAt: time.Now().UTC(), RecordedAt: group[0].RecordedAt}
		m.mu.Lock()
		if j.Task.State == "canceled" {
			m.mu.Unlock()
			return errors.New("上传已取消")
		}
		old := slices.Clone(j.Task.Files)
		index := slices.IndexFunc(j.Task.Files, func(f FileResult) bool { return f.RemotePath == remote })
		if index < 0 {
			j.Task.Files = append(j.Task.Files, result)
		} else {
			j.Task.Files[index] = result
		}
		e = m.saveLocked()
		if e != nil {
			j.Task.Files = old
		}
		m.mu.Unlock()
		if e != nil {
			return errors.New("上传成功但结果保存失败，请重试核验")
		}
		if q.Mode == "compressed" {
			_ = os.Remove(local)
			m.mu.Lock()
			if release := m.reserves[j.Task.ID]; release != nil {
				release()
				delete(m.reserves, j.Task.ID)
			}
			m.mu.Unlock()
		}
	}
	return nil
}

func fileHash(ctx context.Context, name string) (string, uint64, error) {
	f, err := os.Open(name)
	if err != nil {
		return "", 0, err
	}
	defer f.Close()
	h := sha256.New()
	buf := make([]byte, 128<<10)
	var n uint64
	for {
		if err = ctx.Err(); err != nil {
			return "", 0, err
		}
		count, e := f.Read(buf)
		if count > 0 {
			_, _ = h.Write(buf[:count])
			n += uint64(count)
		}
		if e == io.EOF {
			break
		}
		if e != nil {
			return "", 0, e
		}
	}
	return hex.EncodeToString(h.Sum(nil)), n, nil
}

func (m *Manager) releaseLocked(id string) {
	if l := m.leases[id]; l != nil {
		l.Release()
		delete(m.leases, id)
	}
	if release := m.reserves[id]; release != nil {
		release()
		delete(m.reserves, id)
	}
}
func (m *Manager) removeOutputsLocked(j *job) {
	for _, o := range j.Outputs {
		if filepath.Dir(o.Path) == filepath.Join(m.cfg.Root, j.Task.ID) {
			_ = os.Remove(o.Path)
		}
	}
	_ = os.Remove(filepath.Join(m.cfg.Root, j.Task.ID))
}
func (m *Manager) saveLocked() error {
	b, err := json.Marshal(m.state)
	if err != nil {
		return err
	}
	f, err := os.CreateTemp(m.cfg.Root, ".state-*")
	if err != nil {
		return err
	}
	name := f.Name()
	defer os.Remove(name)
	if err = f.Chmod(0o600); err == nil {
		_, err = f.Write(b)
	}
	if err == nil {
		err = f.Sync()
	}
	closeErr := f.Close()
	if err != nil {
		return err
	}
	if closeErr != nil {
		return closeErr
	}
	if err = os.Rename(name, filepath.Join(m.cfg.Root, "state.json")); err != nil {
		return err
	}
	if runtime.GOOS != "windows" {
		dir, e := os.Open(m.cfg.Root)
		if e != nil {
			return e
		}
		defer dir.Close()
		return dir.Sync()
	}
	return nil
}
