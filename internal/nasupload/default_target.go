package nasupload

import "errors"

// DefaultTarget 要求明确配置默认目标，不以列表顺序决定录像落点。
func (m *Manager) DefaultTarget() (Target, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, target := range m.state.Targets {
		if target.ID == m.state.DefaultTargetID {
			target.IsDefault = true
			return publicTarget(target), nil
		}
	}
	return Target{}, errors.New("请先在设置中指定默认上传目标")
}

// SetDefaultTarget 只改变后续新任务的目标，已排队任务继续使用原有目标快照。
func (m *Manager) SetDefaultTarget(id string) (Target, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, target := range m.state.Targets {
		if target.ID != id {
			continue
		}
		previous := m.state.DefaultTargetID
		m.state.DefaultTargetID = id
		if err := m.saveLocked(); err != nil {
			m.state.DefaultTargetID = previous
			return Target{}, err
		}
		target.IsDefault = true
		return publicTarget(target), nil
	}
	return Target{}, errors.New("请选择一个已保存的上传目标")
}
