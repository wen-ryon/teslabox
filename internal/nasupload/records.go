package nasupload

import "slices"

// UploadedFile 按已校验的远端文件统计，避免合成画面被误计为多个源文件。
type UploadedFile struct {
	FileResult
	ID              string  `json:"id"`
	TargetID        string  `json:"target_id"`
	TargetName      string  `json:"target_name"`
	IsDefaultTarget bool    `json:"is_default_target"`
	Quality         Quality `json:"quality"`
}

// destinationKey 忽略名称、密码和画质变更，但目录或服务器变更不能沿用旧落点的成功状态。
func destinationKey(t Target) string {
	return digest([]any{t.Protocol, t.Host, t.Port, t.URL, t.User, t.Domain, t.Path, t.HostKey})
}

// Records 从现有日志恢复文件记录，不迁移或丢弃旧任务；同一远端文件仅保留最近核验。
func (m *Manager) Records(identity string) []UploadedFile {
	m.mu.Lock()
	defer m.mu.Unlock()
	current := ""
	for _, target := range m.state.Targets {
		if target.ID == m.state.DefaultTargetID {
			current = destinationKey(target)
			break
		}
	}
	files := make(map[string]UploadedFile)
	for _, j := range m.state.Jobs {
		if j.Identity != identity {
			continue
		}
		location := destinationKey(j.Target)
		quality := j.Target.Quality
		if j.Task.Request.Quality != nil {
			quality = *j.Task.Request.Quality
		}
		for _, file := range j.Task.Files {
			if file.VerifiedAt.IsZero() || file.RemotePath == "" {
				continue
			}
			id := digest([]string{location, file.RemotePath})
			if previous, ok := files[id]; ok && !file.VerifiedAt.After(previous.VerifiedAt) {
				continue
			}
			file.Cameras = slices.Clone(file.Cameras)
			files[id] = UploadedFile{FileResult: file, ID: id, TargetID: j.Target.ID, TargetName: j.Task.TargetName, IsDefaultTarget: current != "" && location == current, Quality: quality}
		}
	}
	out := make([]UploadedFile, 0, len(files))
	for _, file := range files {
		out = append(out, file)
	}
	slices.SortFunc(out, func(a, b UploadedFile) int {
		if order := b.VerifiedAt.Compare(a.VerifiedAt); order != 0 {
			return order
		}
		if a.ID < b.ID {
			return -1
		}
		if a.ID > b.ID {
			return 1
		}
		return 0
	})
	return out
}
