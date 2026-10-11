package nasupload

import (
	"encoding/json"
	"errors"
	"maps"
	"slices"
	"time"
)

// AutoConfig 与服务器、画质分开保存，附加合成画质不改变事件主视频。
type AutoConfig struct {
	Enabled           bool    `json:"enabled"`
	IncludeExisting   bool    `json:"include_existing"`
	Scope             string  `json:"scope"`
	SupplementLayout  string  `json:"supplement_layout"`
	SupplementQuality Quality `json:"supplement_quality"`
}

// AutoScanStatus 区分已开启与实际扫描成功，源盘或快照不可用时向用户说明等待原因。
type AutoScanStatus struct {
	CheckedAt time.Time `json:"checked_at"`
	Error     string    `json:"error,omitempty"`
}

// RecordAutomaticScan 仅记录运行状态，不在每次轮询额外写盘。
func (m *Manager) RecordAutomaticScan(now time.Time, err error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.autoScan = AutoScanStatus{CheckedAt: now}
	if err != nil {
		m.autoScan.Error = err.Error()
	}
}

// AutomaticScan 返回最近一次后台快照核对结果。
func (m *Manager) AutomaticScan() AutoScanStatus {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.autoScan
}

// AutoEvent 保留发现时的目标和策略，停电、切换配置或元数据晚到都不重建已排队任务。
type AutoEvent struct {
	DiscoveryOnly       bool                         `json:"-"`
	MetadataFingerprint string                       `json:"metadata_fingerprint,omitempty"`
	InputEvidence       map[string]AutoInputEvidence `json:"input_evidence,omitempty"`
	ID                  string                       `json:"id"`
	Identity            string                       `json:"identity"`
	Directory           string                       `json:"directory"`
	Camera              string                       `json:"camera"`
	OccurredAt          time.Time                    `json:"occurred_at"`
	DetectedAt          time.Time                    `json:"detected_at"`
	UpdatedAt           time.Time                    `json:"updated_at"`
	State               string                       `json:"state"`
	Error               string                       `json:"error,omitempty"`
	Policy              AutoConfig                   `json:"policy"`
	Target              Target                       `json:"target"`
	Fingerprint         string                       `json:"fingerprint"`
	LastVersion         string                       `json:"last_version"`
	StableSince         time.Time                    `json:"stable_since"`
	Observations        int                          `json:"observations"`
	TaskIDs             []string                     `json:"task_ids"`
	PlanFingerprint     string                       `json:"plan_fingerprint"`
	CoverageNote        string                       `json:"coverage_note,omitempty"`
}

// AutoInputEvidence 独立保存每个分钟、输出角色的稳定证据，附加视角写入不阻塞主视角。
type AutoInputEvidence struct {
	Fingerprint  string    `json:"fingerprint"`
	Version      string    `json:"version"`
	StableSince  time.Time `json:"stable_since"`
	CheckedAt    time.Time `json:"checked_at"`
	Observations int       `json:"observations"`
}

func defaultAutoConfig() AutoConfig {
	return AutoConfig{Scope: "event", SupplementQuality: Quality{Mode: "compressed", Height: 720, FPS: 24, BitrateKbps: 3000, Layout: "composite6"}}
}

// Automatic 返回无凭据的自动上传设置，旧状态默认为关闭。
func (m *Manager) Automatic() AutoConfig {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.state.Automatic.Scope == "" {
		return defaultAutoConfig()
	}
	return m.state.Automatic
}

// SaveAutomatic 仅影响以后发现的事件，已有上传任务保留原策略且不会被开关隐式取消。
func (m *Manager) SaveAutomatic(config AutoConfig) (AutoConfig, error) {
	switch config.Scope {
	case "event", "surrounding", "directory":
	default:
		return AutoConfig{}, errors.New("请选择事件分钟、前后一分钟或整个事件目录")
	}
	if config.SupplementLayout != "" {
		if len(layoutCameras(config.SupplementLayout)) == 0 {
			return AutoConfig{}, errors.New("请选择 6 路、4 路 A、4 路 B 或 3 路附加合成")
		}
		config.SupplementQuality.Mode = "compressed"
		config.SupplementQuality.Layout = config.SupplementLayout
		if err := validateQuality(config.SupplementQuality); err != nil {
			return AutoConfig{}, err
		}
	} else {
		config.SupplementQuality = defaultAutoConfig().SupplementQuality
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if config.Enabled && m.state.DefaultTargetID == "" {
		return AutoConfig{}, errors.New("请先指定默认上传目标，再开启自动上传")
	}
	old, baseline, oldEvents := m.state.Automatic, m.state.AutoBaselines, m.state.AutoEvents
	if config.Enabled && (!old.Enabled || config.IncludeExisting && !old.IncludeExisting) {
		m.state.AutoBaselines = nil
		if config.IncludeExisting {
			m.state.AutoEvents = slices.DeleteFunc(slices.Clone(m.state.AutoEvents), func(e AutoEvent) bool { return e.State == "historical" })
		}
	}
	m.state.Automatic = config
	if err := m.saveLocked(); err != nil {
		m.state.Automatic, m.state.AutoBaselines, m.state.AutoEvents = old, baseline, oldEvents
		return AutoConfig{}, err
	}
	m.signal()
	return config, nil
}

func layoutCameras(layout string) []string {
	return map[string][]string{
		"composite6":  {"left_pillar", "front", "right_pillar", "left_repeater", "back", "right_repeater"},
		"composite4a": {"front", "back", "left_repeater", "right_repeater"},
		"composite4b": {"front", "back", "left_repeater", "right_repeater"},
		"composite3":  {"front", "left_repeater", "right_repeater"},
	}[layout]
}

// ObserveAutomatic 先持久发现，再等待两次独立快照的稳定证据；首轮目录作为可选历史基线。
func (m *Manager) ObserveAutomatic(identity string, directories []string, candidates []AutoEvent, now time.Time) ([]AutoEvent, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if !m.state.Automatic.Enabled {
		return nil, nil
	}
	_, baselineExists := m.state.AutoBaselines[identity]
	needsChange := !baselineExists
	for _, candidate := range candidates {
		index := slices.IndexFunc(m.state.AutoEvents, func(e AutoEvent) bool { return e.Identity == identity && e.Directory == candidate.Directory })
		if index < 0 {
			needsChange = true
			break
		}
		event := m.state.AutoEvents[index]
		if event.State == "historical" || event.State == "canceled" {
			continue
		}
		if !candidate.DiscoveryOnly || event.MetadataFingerprint != candidate.MetadataFingerprint || event.Camera != candidate.Camera || !event.OccurredAt.Equal(candidate.OccurredAt) {
			needsChange = true
			break
		}
	}
	// 平静期不复制整份持久队列，也不重新写入完全相同的历史状态。
	if !needsChange {
		return m.automaticEventsForIdentityLocked(identity), nil
	}
	// JSON 复制用于事务回滚，不能因写盘失败遗留只存在内存中的去重记录。
	previous, err := cloneAutoState(m.state)
	if err != nil {
		return nil, err
	}
	if m.state.AutoBaselines == nil {
		m.state.AutoBaselines = make(map[string]map[string]bool)
	}
	baseline, initialized := m.state.AutoBaselines[identity]
	changed := !initialized
	if !initialized {
		baseline = make(map[string]bool)
		if !m.state.Automatic.IncludeExisting {
			for _, directory := range directories {
				baseline[directory] = true
			}
		}
		m.state.AutoBaselines[identity] = baseline
	}
	var target Target
	for _, t := range m.state.Targets {
		if t.ID == m.state.DefaultTargetID {
			target = t
		}
	}
	for _, candidate := range candidates {
		candidate.ID = digest([]string{identity, candidate.Directory})
		index := slices.IndexFunc(m.state.AutoEvents, func(e AutoEvent) bool { return e.ID == candidate.ID })
		if index < 0 {
			changed = true
			if len(m.state.AutoEvents) >= 10000 {
				m.state = previous
				return nil, errors.New("自动事件记录已满，请处理历史记录后重试")
			}
			candidate.Identity, candidate.Target, candidate.Policy = identity, target, m.state.Automatic
			candidate.DetectedAt, candidate.UpdatedAt, candidate.StableSince = now, now, now
			candidate.Observations = 1
			if candidate.DiscoveryOnly {
				candidate.Observations = 0
			}
			candidate.State = "waiting_files"
			if baseline[candidate.Directory] {
				candidate.State, candidate.Error = "historical", "启用前已有目录，未自动补传"
			}
			m.state.AutoEvents = append(m.state.AutoEvents, candidate)
			continue
		}
		event := &m.state.AutoEvents[index]
		if event.State == "historical" || event.State == "canceled" {
			continue
		}
		if candidate.DiscoveryOnly {
			if event.MetadataFingerprint != candidate.MetadataFingerprint || event.Camera != candidate.Camera || !event.OccurredAt.Equal(candidate.OccurredAt) {
				changed = true
				event.MetadataFingerprint, event.Camera, event.OccurredAt = candidate.MetadataFingerprint, candidate.Camera, candidate.OccurredAt
				event.InputEvidence = nil
				event.Fingerprint, event.PlanFingerprint = "", ""
			}
			continue
		}
		changed = true
		if candidate.Fingerprint != event.Fingerprint || candidate.Camera != event.Camera || now.Sub(event.UpdatedAt) > time.Minute {
			event.StableSince, event.Observations = now, 1
		} else if candidate.LastVersion != event.LastVersion {
			event.Observations++
		}
		event.Fingerprint, event.LastVersion, event.Camera, event.UpdatedAt = candidate.Fingerprint, candidate.LastVersion, candidate.Camera, now
		event.OccurredAt = candidate.OccurredAt
		if event.Target.ID == "" && target.ID != "" {
			event.Target = target
		}
	}
	if changed {
		if err := m.saveLocked(); err != nil {
			m.state = previous
			return nil, err
		}
	}
	return m.automaticEventsForIdentityLocked(identity), nil
}

func (m *Manager) automaticEventsForIdentityLocked(identity string) []AutoEvent {
	out := make([]AutoEvent, 0)
	for _, event := range m.state.AutoEvents {
		if event.Identity == identity && event.State != "historical" && event.State != "canceled" {
			event.TaskIDs = slices.Clone(event.TaskIDs)
			event.InputEvidence = maps.Clone(event.InputEvidence)
			out = append(out, event)
		}
	}
	return out
}

// ObserveAutomaticInput 只把不同快照的相同输入算作稳定；证据随本轮事件结果一起持久化，避免每个视角刷盘。
func (m *Manager) ObserveAutomaticInput(id, key, fingerprint, version string, now time.Time) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	index := slices.IndexFunc(m.state.AutoEvents, func(e AutoEvent) bool { return e.ID == id })
	if index < 0 || !m.state.Automatic.Enabled || m.state.AutoEvents[index].State == "canceled" {
		return false, errors.New("事件自动上传已关闭或取消")
	}
	event := &m.state.AutoEvents[index]
	old, exists := event.InputEvidence[key]
	evidence := old
	if !exists || evidence.Fingerprint != fingerprint || now.Sub(evidence.CheckedAt) > time.Minute {
		evidence = AutoInputEvidence{Fingerprint: fingerprint, Version: version, StableSince: now, CheckedAt: now, Observations: 1}
	} else {
		if evidence.Version != version {
			evidence.Observations++
			evidence.Version = version
		}
		evidence.CheckedAt = now
	}
	if event.InputEvidence == nil {
		event.InputEvidence = make(map[string]AutoInputEvidence)
	}
	event.InputEvidence[key] = evidence
	return evidence.Observations >= 2 && now.Sub(evidence.StableSince) >= 15*time.Second, nil
}

// SetAutomaticResult 保留事件准备状态及子任务，主视频与附加视频的失败分别可见。
func (m *Manager) SetAutomaticResult(id, state, message string, taskIDs []string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	for i, event := range m.state.AutoEvents {
		if event.ID != id {
			continue
		}
		old := event
		if event.State == "canceled" {
			return nil
		}
		event.State, event.Error = state, message
		if state == "queued" {
			event.PlanFingerprint = event.Fingerprint
			event.CoverageNote = message
			event.Error = ""
		}
		for _, taskID := range taskIDs {
			if !slices.Contains(event.TaskIDs, taskID) {
				event.TaskIDs = append(slices.Clone(event.TaskIDs), taskID)
			}
		}
		m.state.AutoEvents[i] = event
		if err := m.saveLocked(); err != nil {
			m.state.AutoEvents[i] = old
			return err
		}
		return nil
	}
	return errors.New("自动事件不存在")
}

// CancelAutomatic 先固定事件取消状态，再取消所有子任务，避免扫描与点击取消并发时补建新任务。
func (m *Manager) CancelAutomatic(id string) error {
	m.mu.Lock()
	index := slices.IndexFunc(m.state.AutoEvents, func(e AutoEvent) bool { return e.ID == id })
	if index < 0 {
		m.mu.Unlock()
		return errors.New("自动事件不存在")
	}
	old := m.state.AutoEvents[index]
	m.state.AutoEvents[index].State = "canceled"
	m.state.AutoEvents[index].Error = "用户已取消自动事件"
	if err := m.saveLocked(); err != nil {
		m.state.AutoEvents[index] = old
		m.mu.Unlock()
		return err
	}
	var ids []string
	for _, j := range m.state.Jobs {
		if j.Task.Request.AutoEventID == id {
			ids = append(ids, j.Task.ID)
		}
	}
	m.mu.Unlock()
	for _, taskID := range ids {
		if _, err := m.Cancel(taskID); err != nil {
			return err
		}
	}
	return nil
}

func cloneAutoState(state diskState) (diskState, error) {
	data, err := json.Marshal(state)
	if err != nil {
		return diskState{}, err
	}
	var copy diskState
	if err := json.Unmarshal(data, &copy); err != nil {
		return diskState{}, err
	}
	// 观察事件不能替换正在被工作进程使用的任务指针。
	copy.Jobs = state.Jobs
	return copy, nil
}

// AutomaticNeedsPreparation 让复制中断的任务重新取得快照，完整暂存的任务则由传输队列恢复。
func (m *Manager) AutomaticNeedsPreparation(event AutoEvent) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	if event.PlanFingerprint != event.Fingerprint || len(event.TaskIDs) == 0 {
		return true
	}
	for _, id := range event.TaskIDs {
		j := m.findLocked(id)
		if j == nil || !j.Prepared && j.Task.State == "failed" {
			return true
		}
	}
	return false
}

// AutomaticPolicy 查询已固定的策略，尚未发现的目录使用当前设置。
func (m *Manager) AutomaticPolicy(identity, directory string) AutoConfig {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, event := range m.state.AutoEvents {
		if event.Identity == identity && event.Directory == directory {
			return event.Policy
		}
	}
	return m.state.Automatic
}

// AutomaticEvents 不暴露记录中的固定凭据，浏览器只能看到事件进度和子任务标识。
func (m *Manager) AutomaticEvents() []AutoEvent {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]AutoEvent, 0, len(m.state.AutoEvents))
	for i := len(m.state.AutoEvents) - 1; i >= 0; i-- {
		event := m.state.AutoEvents[i]
		event.InputEvidence, event.MetadataFingerprint = nil, ""
		event.Target = publicTarget(event.Target)
		event.Identity, event.Fingerprint, event.LastVersion = "", "", ""
		event.TaskIDs = slices.Clone(event.TaskIDs)
		if len(event.TaskIDs) > 0 && event.State == "queued" {
			complete, failed, canceled := 0, 0, 0
			for _, id := range event.TaskIDs {
				if j := m.findLocked(id); j != nil {
					switch j.Task.State {
					case "completed":
						complete++
					case "failed":
						failed++
					case "canceled":
						canceled++
					}
				}
			}
			if complete == len(event.TaskIDs) {
				event.State = "completed"
			} else if canceled == len(event.TaskIDs) {
				event.State = "canceled"
			} else if failed > 0 {
				event.State = "failed"
			}
		}
		out = append(out, event)
	}
	return out
}
