// Package carplaysettings 保存独立 CarPlay 策略，设置变化不触碰录像 USB 生命周期。
package carplaysettings

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"sync"
)

// Settings 将设置版本与浏览器尺寸记忆关联，保存后才允许重新检测页面尺寸。
type Settings struct {
	PhoneID          string `json:"phone_id"`
	Revision         uint64 `json:"revision"`
	Network          string `json:"network"`
	RecoveryPriority string `json:"recovery_priority"`
	Enabled          bool   `json:"enabled"`
	AutoPlay         bool   `json:"auto_play"`
	Quality          int    `json:"quality"`
	FPS              int    `json:"fps"`
	DashboardLayout  string `json:"dashboard_layout"`
}

// Display 是浏览器完成尺寸检测后的接收端屏幕协商参数。
type Display struct {
	Revision uint64 `json:"revision"`
	Width    int    `json:"width"`
	Height   int    `json:"height"`
	FPS      int    `json:"fps"`
}

// ApplyDisplay 只接受当前设置版本的编码尺寸，不增加设置版本或触碰录像服务。
func (m *Manager) ApplyDisplay(revision uint64, width, height int) (Display, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if revision != m.data.Revision {
		return Display{}, errors.New("CarPlay 设置版本已变化")
	}
	maxWidth := map[int]int{1080: 1920, 720: 1280, 480: 854}[m.data.Quality]
	if width < 320 || height < 240 || width > maxWidth || height > m.data.Quality || width%2 != 0 || height%2 != 0 {
		return Display{}, errors.New("CarPlay 编码尺寸超出当前画质范围")
	}
	width = dashboardWidth(width, height, m.data.DashboardLayout)
	if width < 320 {
		return Display{}, errors.New("当前画面比例无法使用竖向地图布局")
	}
	display := Display{Revision: revision, Width: width, Height: height, FPS: m.data.FPS}
	if err := persist(m.path+".display", display); err != nil {
		return Display{}, err
	}
	return display, nil
}

// Manager 在系统盘保存策略，不调用系统重启或录像服务操作。
type Manager struct {
	mu   sync.Mutex
	path string
	data Settings
}

// New 读取设备策略，没有保存记录时保持现有 720p、30 帧及自动网络行为。
func New(path string) (*Manager, error) {
	if !filepath.IsAbs(path) {
		return nil, errors.New("CarPlay 设置路径必须是绝对路径")
	}
	m := &Manager{path: path, data: Settings{Revision: 1, Network: "auto", RecoveryPriority: "internet", Enabled: true, Quality: 720, FPS: 30, DashboardLayout: "auto"}}
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return m, nil
	}
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() || info.Size() > 4096 {
		return nil, errors.New("CarPlay 设置文件不合法")
	}
	content, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	if err = json.Unmarshal(content, &m.data); err != nil {
		return nil, err
	}
	if err = validate(m.data); err != nil {
		return nil, err
	}
	return m, nil
}

// Settings 返回副本，防止页面修改共享设置。
func (m *Manager) Settings() Settings { m.mu.Lock(); defer m.mu.Unlock(); return m.data }

// Save 拒绝旧版本覆盖，持久化成功后才更新内存中的设置版本。
func (m *Manager) Save(next Settings) (Settings, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if next.RecoveryPriority == "" {
		next.RecoveryPriority = m.data.RecoveryPriority
	}
	if next.DashboardLayout == "" {
		next.DashboardLayout = m.data.DashboardLayout
	}
	if next.Revision != m.data.Revision {
		return Settings{}, errors.New("CarPlay 设置已更新，请刷新后保存")
	}
	if err := validate(next); err != nil {
		return Settings{}, err
	}
	if next.Revision >= 9007199254740991 {
		return Settings{}, errors.New("CarPlay 设置版本超过浏览器安全范围")
	}
	next.Revision++
	if err := persist(m.path, next); err != nil {
		return Settings{}, err
	}
	m.data = next
	return next, nil
}

func persist(path string, data any) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		return err
	}
	file, err := os.CreateTemp(filepath.Dir(path), ".carplay-settings-*")
	if err != nil {
		return err
	}
	defer os.Remove(file.Name())
	if err = file.Chmod(0o600); err == nil {
		err = json.NewEncoder(file).Encode(data)
	}
	if err == nil {
		err = file.Sync()
	}
	closeErr := file.Close()
	if err == nil {
		err = closeErr
	}
	if err != nil {
		return err
	}
	return os.Rename(file.Name(), path)
}

var phoneIDPattern = regexp.MustCompile(`^(?:[A-F0-9]{2}:){5}[A-F0-9]{2}$`)

// IsPhoneID 保持设置和接收端使用同一蓝牙标识格式，不把 IP 地址作为手机身份。
func IsPhoneID(value string) bool { return phoneIDPattern.MatchString(value) }

func validate(s Settings) error {
	if s.DashboardLayout != "auto" && s.DashboardLayout != "vertical" {
		return errors.New("CarPlay 主页面布局不合法")
	}
	if s.PhoneID != "" && !IsPhoneID(s.PhoneID) {
		return errors.New("CarPlay 选择手机不合法")
	}

	if s.RecoveryPriority != "internet" && s.RecoveryPriority != "carplay" {
		return errors.New("CarPlay 恢复优先级不合法")
	}
	if s.Revision < 1 || s.Revision > 9007199254740991 {
		return errors.New("CarPlay 设置版本不合法")
	}
	if s.Network != "auto" && s.Network != "wifi" && s.Network != "ethernet" && s.Network != "bluetooth" {
		return errors.New("CarPlay 网络来源不合法")
	}
	if s.Quality != 1080 && s.Quality != 720 && s.Quality != 480 {
		return errors.New("CarPlay 画质不合法")
	}
	if s.FPS != 30 && s.FPS != 60 {
		return errors.New("CarPlay 帧率不合法")
	}
	return nil
}

func dashboardWidth(width, height int, layout string) int {
	if layout != "vertical" {
		return width
	}
	// 三档默认密度的实测边界为 W=1.3H-60；保持偶数编码尺寸，不放大已是竖向的画面。
	boundary := (13*height - 600) / 20 * 2
	if width > boundary {
		return boundary
	}
	return width
}
