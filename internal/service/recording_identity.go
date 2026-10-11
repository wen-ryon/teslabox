package service

import "path/filepath"

// RecordingIdentity 固定当前租约的录像卷身份，防止换盘或格式化后复用同名旧录像。
func (m *Manager) RecordingIdentity(state Snapshot) (string, error) {
	directory := m.cfg.IndexCacheDir
	if directory == "" {
		directory = filepath.Dir(m.cfg.ExportStatePath)
	}
	return persistentIndexIdentity(state, directory)
}
