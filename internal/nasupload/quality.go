package nasupload

import "errors"

// SaveQuality 只更新画质，保留服务器、目录和凭据；已创建的任务仍使用原画质快照。
func (m *Manager) SaveQuality(id string, quality Quality) (Target, error) {
	if err := validateQuality(quality); err != nil {
		return Target{}, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	for i, target := range m.state.Targets {
		if target.ID != id {
			continue
		}
		previous := target
		target.Quality = quality
		target.Revision = ""
		target.Revision = digest(target)[:24]
		m.state.Targets[i] = target
		if err := m.saveLocked(); err != nil {
			m.state.Targets[i] = previous
			return Target{}, err
		}
		target.IsDefault = id == m.state.DefaultTargetID
		return publicTarget(target), nil
	}
	return Target{}, errors.New("请先保存服务器配置，再保存画质设置")
}
