// Package maintenancemode 保存媒体写入和在线更新的自动恢复策略。
package maintenancemode

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
)

// Settings 是保存在系统盘上的设备级维护策略。
type Settings struct {
	// AutoRecovery 仅控制媒体写入和删除的自动恢复，避免用户上传或应用媒体时意外重启。
	AutoRecovery bool `json:"auto_recovery"`
	// UpdateAutoRecovery 控制在线更新后的自动重启恢复，默认开启以避免更新后 Gadget 长期断开。
	UpdateAutoRecovery bool `json:"update_auto_recovery"`
}

// Manager 用原子文件保存设置，避免浏览器切换或刷新后丢失设备策略。
type Manager struct {
	path string
	mu   sync.RWMutex
	data Settings
}

// New 读取已有策略；媒体写入默认手动恢复，在线更新默认自动重启恢复。
func New(path string) (*Manager, error) {
	if !filepath.IsAbs(path) || filepath.Clean(path) == string(filepath.Separator) {
		return nil, errors.New("维护策略路径必须是非根目录绝对路径")
	}
	manager := &Manager{path: path, data: defaultSettings()}
	if err := manager.load(); err != nil {
		return nil, err
	}
	return manager, nil
}

// Settings 返回当前策略的值副本。
func (m *Manager) Settings() Settings {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.data
}

// SetAutoRecovery 更新自动维护恢复开关。
func (m *Manager) SetAutoRecovery(enabled bool) (Settings, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	updated := m.data
	updated.AutoRecovery = enabled
	if err := save(m.path, updated); err != nil {
		return Settings{}, err
	}
	m.data = updated
	return updated, nil
}

func (m *Manager) load() error {
	info, err := os.Lstat(m.path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("检查维护策略: %w", err)
	}
	if !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 {
		return errors.New("维护策略必须是普通文件")
	}
	content, err := os.ReadFile(m.path)
	if err != nil {
		return fmt.Errorf("读取维护策略: %w", err)
	}
	var persisted struct {
		AutoRecovery       *bool `json:"auto_recovery"`
		UpdateAutoRecovery *bool `json:"update_auto_recovery"`
	}
	if err := json.Unmarshal(content, &persisted); err != nil {
		return fmt.Errorf("解析维护策略: %w", err)
	}
	data := defaultSettings()
	if persisted.AutoRecovery != nil {
		data.AutoRecovery = *persisted.AutoRecovery
	}
	if persisted.UpdateAutoRecovery != nil {
		data.UpdateAutoRecovery = *persisted.UpdateAutoRecovery
	}
	m.data = data
	return nil
}

func defaultSettings() Settings {
	return Settings{UpdateAutoRecovery: true}
}

func save(path string, data Settings) (resultErr error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		return fmt.Errorf("创建维护策略目录: %w", err)
	}
	temporary, err := os.CreateTemp(filepath.Dir(path), ".maintenance-*")
	if err != nil {
		return fmt.Errorf("创建维护策略临时文件: %w", err)
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
		return err
	}
	if err := json.NewEncoder(temporary).Encode(data); err != nil {
		return fmt.Errorf("编码维护策略: %w", err)
	}
	if err := temporary.Sync(); err != nil {
		return fmt.Errorf("同步维护策略: %w", err)
	}
	if err := temporary.Close(); err != nil {
		return fmt.Errorf("关闭维护策略: %w", err)
	}
	closed = true
	if err := os.Rename(temporaryName, path); err != nil {
		return fmt.Errorf("发布维护策略: %w", err)
	}
	return nil
}
