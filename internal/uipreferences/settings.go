// Package uipreferences 保存设备共享的界面偏好，不修改无线地区或操作系统语言。
package uipreferences

import (
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"sync"
)

// DefaultPath 位于系统盘，OTA 替换程序和访问地址变化都不应重置语言。
const DefaultPath = "/var/lib/teslabox-system/ui-preferences.json"

// Settings 只包含明确支持的界面语言。
type Settings struct {
	Language string `json:"language"`
}

// Store 串行保存共享偏好，每次读取磁盘以保留重启及外部维护后的真实值。
type Store struct {
	path string
	mu   sync.RWMutex
}

// New 不在构造时写文件，旧设备首次访问默认中文，不覆盖已有状态。
func New(path string) *Store { return &Store{path: path} }

// ValidLanguage 禁止把地区代码、路径或任意 locale 当作语言配置。
func ValidLanguage(language string) bool { return language == "zh-CN" || language == "en" }

// Read 保留无效配置为可诊断错误，不能静默改写成默认中文。
func (s *Store) Read() (Settings, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.read()
}

func (s *Store) read() (Settings, error) {
	if !filepath.IsAbs(s.path) {
		return Settings{}, errors.New("界面偏好路径无效")
	}
	if err := safeParents(filepath.Dir(s.path)); err != nil {
		return Settings{}, err
	}
	info, err := os.Lstat(s.path)
	if errors.Is(err, os.ErrNotExist) {
		return Settings{Language: "zh-CN"}, nil
	}
	if err != nil {
		return Settings{}, err
	}
	if !info.Mode().IsRegular() || info.Size() > 4096 {
		return Settings{}, errors.New("界面偏好不是有界普通文件")
	}
	file, err := os.Open(s.path)
	if err != nil {
		return Settings{}, err
	}
	defer file.Close()
	opened, err := file.Stat()
	if err != nil {
		return Settings{}, err
	}
	if !os.SameFile(info, opened) {
		return Settings{}, errors.New("界面偏好在读取期间发生变化")
	}
	decoder := json.NewDecoder(io.LimitReader(file, 4097))
	decoder.DisallowUnknownFields()
	var result Settings
	if err := decoder.Decode(&result); err != nil {
		return Settings{}, err
	}
	var extra any
	if decoder.Decode(&extra) != io.EOF || !ValidLanguage(result.Language) {
		return Settings{}, errors.New("界面语言配置无效")
	}
	return result, nil
}

// Update 原子替换配置，写入成功后才通知客户端应用，不改变任何网络参数。
func (s *Store) Update(value Settings) (Settings, error) {
	if !ValidLanguage(value.Language) {
		return Settings{}, errors.New("界面语言只支持 zh-CN 或 en")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if !filepath.IsAbs(s.path) {
		return Settings{}, errors.New("界面偏好路径无效")
	}
	if err := safeParents(filepath.Dir(s.path)); err != nil {
		return Settings{}, err
	}
	if info, err := os.Lstat(s.path); err == nil {
		if !info.Mode().IsRegular() {
			return Settings{}, errors.New("界面偏好必须是普通文件")
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return Settings{}, err
	}
	dir := filepath.Dir(s.path)
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return Settings{}, err
	}
	if err := safeParents(dir); err != nil {
		return Settings{}, err
	}
	file, err := os.CreateTemp(dir, ".ui-preferences-*")
	if err != nil {
		return Settings{}, err
	}
	defer os.Remove(file.Name())
	defer file.Close()
	if err := file.Chmod(0o600); err != nil {
		return Settings{}, err
	}
	if err := json.NewEncoder(file).Encode(value); err != nil {
		return Settings{}, err
	}
	if err := file.Sync(); err != nil {
		return Settings{}, err
	}
	if err := file.Close(); err != nil {
		return Settings{}, err
	}
	if err := os.Rename(file.Name(), s.path); err != nil {
		return Settings{}, err
	}
	if err := syncDirectory(dir); err != nil {
		return Settings{}, err
	}
	return value, nil
}

func safeParents(path string) error {
	for {
		info, err := os.Lstat(path)
		if err == nil && !info.IsDir() {
			return errors.New("界面偏好父目录不是普通目录")
		}
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
		parent := filepath.Dir(path)
		if parent == path {
			return nil
		}
		path = parent
	}
}
