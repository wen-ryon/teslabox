// Package onboarding 保存设备级页面引导和外接存储引导状态。
package onboarding

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strings"
	"sync"
)

const currentVersion = 1

// State 是跨访问地址共享的设备级引导状态。
type State struct {
	Version                    int      `json:"version"`
	PageGuideComplete          bool     `json:"page_guide_complete"`
	DismissedStorageStablePath []string `json:"dismissed_storage_stable_paths"`
}

// Manager 用原子文件保存引导状态，避免 IP 或域名变化后重复提示。
type Manager struct {
	path string
	mu   sync.RWMutex
	data State
}

// New 读取已有状态；文件不存在时保留首次引导待完成状态。
func New(path string) (*Manager, error) {
	if !filepath.IsAbs(path) || filepath.Clean(path) == string(filepath.Separator) {
		return nil, errors.New("引导状态路径必须是非根目录绝对路径")
	}
	manager := &Manager{path: path, data: State{Version: currentVersion}}
	if err := manager.load(); err != nil {
		return nil, err
	}
	return manager, nil
}

// State 返回当前状态的深拷贝。
func (m *Manager) State() State {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return clone(m.data)
}

// CompletePageGuide 将设备级页面引导标记为完成。
func (m *Manager) CompletePageGuide() (State, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	updated := clone(m.data)
	updated.PageGuideComplete = true
	if err := save(m.path, updated); err != nil {
		return State{}, err
	}
	m.data = updated
	return clone(updated), nil
}

// DismissStorage 记录用户暂不处理的未初始化外接设备。
func (m *Manager) DismissStorage(stablePath string) (State, error) {
	stablePath = path.Clean(strings.TrimSpace(stablePath))
	if !strings.HasPrefix(stablePath, "/dev/disk/by-id/") || len(stablePath) > 1024 || strings.ContainsAny(stablePath, "\r\n") {
		return State{}, errors.New("外接存储稳定路径无效")
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	updated := clone(m.data)
	if !slices.Contains(updated.DismissedStorageStablePath, stablePath) {
		updated.DismissedStorageStablePath = append(updated.DismissedStorageStablePath, stablePath)
		slices.Sort(updated.DismissedStorageStablePath)
	}
	if err := save(m.path, updated); err != nil {
		return State{}, err
	}
	m.data = updated
	return clone(updated), nil
}

func (m *Manager) load() error {
	info, err := os.Lstat(m.path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("检查引导状态: %w", err)
	}
	if !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 {
		return errors.New("引导状态必须是普通文件")
	}
	content, err := os.ReadFile(m.path)
	if err != nil {
		return fmt.Errorf("读取引导状态: %w", err)
	}
	var state State
	if err := json.Unmarshal(content, &state); err != nil {
		return fmt.Errorf("解析引导状态: %w", err)
	}
	state.Version = currentVersion
	state.DismissedStorageStablePath = normalizePaths(state.DismissedStorageStablePath)
	m.data = state
	return nil
}

func normalizePaths(paths []string) []string {
	result := make([]string, 0, len(paths))
	for _, path := range paths {
		path = pathpkgClean(path)
		if strings.HasPrefix(path, "/dev/disk/by-id/") && len(path) <= 1024 && !strings.ContainsAny(path, "\r\n") && !slices.Contains(result, path) {
			result = append(result, path)
		}
	}
	slices.Sort(result)
	return result
}

func pathpkgClean(value string) string {
	return path.Clean(strings.TrimSpace(value))
}

func clone(state State) State {
	state.DismissedStorageStablePath = slices.Clone(state.DismissedStorageStablePath)
	return state
}

func save(path string, state State) (resultErr error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		return fmt.Errorf("创建引导状态目录: %w", err)
	}
	temporary, err := os.CreateTemp(filepath.Dir(path), ".onboarding-*")
	if err != nil {
		return fmt.Errorf("创建引导状态临时文件: %w", err)
	}
	temporaryName := temporary.Name()
	closed := false
	defer func() {
		if !closed {
			_ = temporary.Close()
		}
		if resultErr != nil {
			_ = os.Remove(temporaryName)
		}
	}()
	if err := temporary.Chmod(0o600); err != nil {
		return err
	}
	if err := json.NewEncoder(temporary).Encode(state); err != nil {
		return fmt.Errorf("编码引导状态: %w", err)
	}
	if err := temporary.Sync(); err != nil {
		return fmt.Errorf("同步引导状态: %w", err)
	}
	if err := temporary.Close(); err != nil {
		return fmt.Errorf("关闭引导状态: %w", err)
	}
	closed = true
	if err := os.Rename(temporaryName, path); err != nil {
		return fmt.Errorf("发布引导状态: %w", err)
	}
	return nil
}
