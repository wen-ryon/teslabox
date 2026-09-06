// Package fancontrol 提供可选的 ROCK 3A PWM 风扇控制。
package fancontrol

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	ModeAutomatic = "automatic"
	ModeManual    = "manual"
)

// TemperatureLevel 描述一个温度到相对转速请求的控制点。
type TemperatureLevel struct {
	TemperatureC int `json:"temperature_c"`
	Percent      int `json:"percent"`
}

// ScenarioSettings 保存各工作负载相对于最大转速限制的请求比例。
type ScenarioSettings struct {
	Idle        int `json:"idle"`
	Single      int `json:"single"`
	Multi       int `json:"multi"`
	Export      int `json:"export"`
	Maintenance int `json:"maintenance"`
}

// Settings 是可持久化的风扇控制策略。
type Settings struct {
	Enabled           bool               `json:"enabled"`
	MaximumPercent    int                `json:"maximum_percent"`
	TemperatureMode   string             `json:"temperature_mode"`
	TemperatureLevels []TemperatureLevel `json:"temperature_levels"`
	Scenarios         ScenarioSettings   `json:"scenarios"`
}

// Status 是设置中心展示的实时状态。
type Status struct {
	Settings
	Available          bool   `json:"available"`
	Reason             string `json:"reason,omitempty"`
	TemperatureC       int    `json:"temperature_c"`
	Scenario           string `json:"scenario"`
	ScenarioRequest    int    `json:"scenario_request"`
	TemperatureRequest int    `json:"temperature_request"`
	OutputPercent      int    `json:"output_percent"`
}

// Config 隔离板级路径，便于单元测试和后续适配其他板卡。
type Config struct {
	StatePath       string
	PWMChipPath     string
	TemperaturePath string
	PeriodNS        int
	ActiveLow       bool
	PollInterval    time.Duration
	Logger          *slog.Logger
}

// Manager 合并温度和业务场景请求，并确保输出不突破最大转速限制。
type Manager struct {
	config      Config
	mu          sync.RWMutex
	settings    Settings
	available   bool
	reason      string
	temperature int
	output      int
	activities  map[string]int
}

// New 创建风扇控制器；硬件不可用不会返回错误，避免影响主服务。
func New(config Config) (*Manager, error) {
	if !filepath.IsAbs(config.StatePath) || !filepath.IsAbs(config.PWMChipPath) || !filepath.IsAbs(config.TemperaturePath) {
		return nil, errors.New("风扇状态和硬件路径必须使用绝对路径")
	}
	if config.PeriodNS <= 0 {
		config.PeriodNS = 10000
	}
	if config.PollInterval <= 0 {
		config.PollInterval = 5 * time.Second
	}
	if config.Logger == nil {
		config.Logger = slog.Default()
	}
	m := &Manager{config: config, settings: DefaultSettings(), activities: make(map[string]int)}
	if err := m.load(); err != nil {
		return nil, err
	}
	if err := m.initializePWM(); err != nil {
		m.reason = err.Error()
		config.Logger.Warn("PWM 风扇不可用，TeslaBox 主链路继续运行", "error", err)
	} else {
		m.available = true
	}
	return m, nil
}

// DefaultSettings 返回兼顾常驻散热与峰值功耗的默认策略。
func DefaultSettings() Settings {
	return Settings{Enabled: true, MaximumPercent: 50, TemperatureMode: ModeAutomatic,
		TemperatureLevels: []TemperatureLevel{{50, 35}, {60, 55}, {70, 80}, {78, 100}},
		Scenarios:         ScenarioSettings{Idle: 50, Single: 60, Multi: 85, Export: 100, Maintenance: 70}}
}

// Run 定期按温度和活动场景刷新PWM；退出时保留最后设置，不关闭风扇。
func (m *Manager) Run(ctx context.Context) {
	m.mu.RLock()
	startupPercent := m.settings.MaximumPercent
	available := m.available
	m.mu.RUnlock()
	// 两线风扇在较低占空比下可能无法自行起转，先以用户设定的绝对上限短暂启动。
	if available && startupPercent > 0 {
		if err := m.writePWM(startupPercent); err != nil {
			m.config.Logger.Warn("PWM 风扇启动预转失败", "error", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(time.Second):
		}
	}
	m.apply()
	ticker := time.NewTicker(m.config.PollInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			m.apply()
		}
	}
}

// Begin 登记一个活动场景并立即预升速，返回幂等的结束函数。
func (m *Manager) Begin(scene string) func() {
	m.mu.Lock()
	m.activities[scene]++
	m.mu.Unlock()
	m.apply()
	var once sync.Once
	return func() {
		once.Do(func() {
			m.mu.Lock()
			if m.activities[scene] > 1 {
				m.activities[scene]--
			} else {
				delete(m.activities, scene)
			}
			m.mu.Unlock()
			m.apply()
		})
	}
}

// Status 返回当前策略和实际计算结果。
func (m *Manager) Status() Status {
	m.mu.RLock()
	defer m.mu.RUnlock()
	scene, sr := m.sceneLocked()
	tr := temperatureRequest(m.settings, m.temperature)
	return Status{Settings: m.settings, Available: m.available, Reason: m.reason, TemperatureC: m.temperature,
		Scenario: scene, ScenarioRequest: sr, TemperatureRequest: tr, OutputPercent: m.output}
}

// Update 校验、持久化并立即应用新策略。
func (m *Manager) Update(settings Settings) (Status, error) {
	settings = normalize(settings)
	if err := validate(settings); err != nil {
		return Status{}, err
	}
	data, err := json.MarshalIndent(settings, "", "  ")
	if err != nil {
		return Status{}, err
	}
	if err := os.MkdirAll(filepath.Dir(m.config.StatePath), 0755); err != nil {
		return Status{}, fmt.Errorf("创建风扇配置目录: %w", err)
	}
	tmp := m.config.StatePath + ".tmp"
	if err := os.WriteFile(tmp, append(data, '\n'), 0644); err != nil {
		return Status{}, fmt.Errorf("写入风扇配置: %w", err)
	}
	if err := os.Rename(tmp, m.config.StatePath); err != nil {
		_ = os.Remove(tmp)
		return Status{}, fmt.Errorf("保存风扇配置: %w", err)
	}
	m.mu.Lock()
	m.settings = settings
	m.mu.Unlock()
	m.apply()
	return m.Status(), nil
}

func normalize(s Settings) Settings {
	s.TemperatureMode = strings.ToLower(strings.TrimSpace(s.TemperatureMode))
	if s.TemperatureMode == ModeAutomatic {
		s.TemperatureLevels = DefaultSettings().TemperatureLevels
	}
	sort.Slice(s.TemperatureLevels, func(i, j int) bool { return s.TemperatureLevels[i].TemperatureC < s.TemperatureLevels[j].TemperatureC })
	return s
}

func validate(s Settings) error {
	if s.MaximumPercent < 0 || s.MaximumPercent > 100 {
		return errors.New("最大转速限制必须在0到100之间")
	}
	if s.TemperatureMode != ModeAutomatic && s.TemperatureMode != ModeManual {
		return errors.New("温度调节模式无效")
	}
	if len(s.TemperatureLevels) != 4 {
		return errors.New("温度曲线必须包含四个档位")
	}
	last := -1
	for _, level := range s.TemperatureLevels {
		if level.TemperatureC < 20 || level.TemperatureC > 100 || level.TemperatureC <= last {
			return errors.New("四档温度必须在20到100℃之间并依次升高")
		}
		if level.Percent < 0 || level.Percent > 100 {
			return errors.New("温度转速必须在0到100之间")
		}
		last = level.TemperatureC
	}
	for _, value := range []int{s.Scenarios.Idle, s.Scenarios.Single, s.Scenarios.Multi, s.Scenarios.Export, s.Scenarios.Maintenance} {
		if value < 0 || value > 100 {
			return errors.New("场景转速必须在0到100之间")
		}
	}
	return nil
}

func (m *Manager) load() error {
	data, err := os.ReadFile(m.config.StatePath)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("读取风扇配置: %w", err)
	}
	var s Settings
	if err = json.Unmarshal(data, &s); err != nil {
		return fmt.Errorf("解析风扇配置: %w", err)
	}
	s = normalize(s)
	if err = validate(s); err != nil {
		return fmt.Errorf("校验风扇配置: %w", err)
	}
	m.settings = s
	return nil
}

func (m *Manager) initializePWM() error {
	if _, err := os.Stat(m.config.PWMChipPath); err != nil {
		return errors.New("未检测到 pwmchip8，请确认已启用风扇设备树 overlay")
	}
	pwm := filepath.Join(m.config.PWMChipPath, "pwm0")
	if _, err := os.Stat(pwm); errors.Is(err, os.ErrNotExist) {
		if err = os.WriteFile(filepath.Join(m.config.PWMChipPath, "export"), []byte("0"), 0200); err != nil && !strings.Contains(strings.ToLower(err.Error()), "busy") {
			return fmt.Errorf("导出PWM通道: %w", err)
		}
		time.Sleep(100 * time.Millisecond)
	}
	periodPath := filepath.Join(pwm, "period")
	if current, err := readInteger(periodPath); err == nil && current == m.config.PeriodNS {
		return nil
	}
	// 服务升级不会主动关闭风扇；只有周期不匹配时才短暂停用通道，避免对已启用 PWM 重写周期触发 EBUSY。
	if err := os.WriteFile(filepath.Join(pwm, "enable"), []byte("0"), 0200); err != nil && !strings.Contains(strings.ToLower(err.Error()), "invalid argument") {
		return fmt.Errorf("停用PWM以更新周期: %w", err)
	}
	if err := os.WriteFile(filepath.Join(pwm, "duty_cycle"), []byte("0"), 0200); err != nil {
		return fmt.Errorf("重置PWM占空比: %w", err)
	}
	if err := os.WriteFile(periodPath, []byte(strconv.Itoa(m.config.PeriodNS)), 0200); err != nil {
		return fmt.Errorf("设置PWM周期: %w", err)
	}
	return nil
}

func readInteger(path string) (int, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return 0, err
	}
	return strconv.Atoi(strings.TrimSpace(string(data)))
}

func (m *Manager) apply() {
	temp := readTemperature(m.config.TemperaturePath)
	m.mu.Lock()
	m.temperature = temp
	_, sr := m.sceneLocked()
	tr := temperatureRequest(m.settings, temp)
	req := sr
	if tr > req {
		req = tr
	}
	out := limitedOutput(m.settings.Enabled, req, m.settings.MaximumPercent)
	available := m.available
	m.output = out
	m.mu.Unlock()
	if !available {
		return
	}
	if err := m.writePWM(out); err != nil {
		m.mu.Lock()
		m.available = false
		m.reason = err.Error()
		m.mu.Unlock()
		m.config.Logger.Warn("写入PWM风扇失败，停止后续硬件操作", "error", err)
	}
}

func limitedOutput(enabled bool, request, maximum int) int {
	if !enabled {
		return 0
	}
	return request * maximum / 100
}

func (m *Manager) sceneLocked() (string, int) {
	s := m.settings.Scenarios
	scene, request := "idle", s.Idle
	candidates := []struct {
		name         string
		value, count int
	}{{"single", s.Single, m.activities["single"]}, {"multi", s.Multi, m.activities["multi"]}, {"export", s.Export, m.activities["export"]}, {"maintenance", s.Maintenance, m.activities["maintenance"]}}
	for _, c := range candidates {
		if c.count > 0 && c.value > request {
			scene, request = c.name, c.value
		}
	}
	return scene, request
}

func temperatureRequest(s Settings, temp int) int {
	levels := s.TemperatureLevels
	if len(levels) != 4 {
		return 0
	}
	if temp < levels[0].TemperatureC {
		return 0
	}
	for i := 1; i < len(levels); i++ {
		if temp < levels[i].TemperatureC {
			low, high := levels[i-1], levels[i]
			return low.Percent + (temp-low.TemperatureC)*(high.Percent-low.Percent)/(high.TemperatureC-low.TemperatureC)
		}
	}
	return levels[len(levels)-1].Percent
}

func readTemperature(path string) int {
	data, err := os.ReadFile(path)
	if err != nil {
		return 0
	}
	v, err := strconv.Atoi(strings.TrimSpace(string(data)))
	if err != nil {
		return 0
	}
	if v > 1000 {
		v /= 1000
	}
	return v
}

func (m *Manager) writePWM(percent int) error {
	pwm := filepath.Join(m.config.PWMChipPath, "pwm0")
	duty := m.config.PeriodNS * hardwareDutyPercent(percent, m.config.ActiveLow) / 100
	if err := os.WriteFile(filepath.Join(pwm, "duty_cycle"), []byte(strconv.Itoa(duty)), 0200); err != nil {
		return fmt.Errorf("设置PWM占空比: %w", err)
	}
	// ROCK 3A 风扇口为低电平有效；0% 仍保持 PWM 使能并输出持续高电平，避免禁用通道后引脚回落导致满速。
	if err := os.WriteFile(filepath.Join(pwm, "enable"), []byte("1"), 0200); err != nil && !strings.Contains(strings.ToLower(err.Error()), "busy") {
		return fmt.Errorf("启用PWM: %w", err)
	}
	return nil
}

func hardwareDutyPercent(logicalPercent int, activeLow bool) int {
	if activeLow {
		return 100 - logicalPercent
	}
	return logicalPercent
}
