package api

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"slices"
	"strings"

	"github.com/wen-ryon/teslabox/internal/service"
	"github.com/wen-ryon/teslabox/internal/teslacam"
)

// sessionSourceRevision 绑定卷代次、完整时间轴和文件属性，不把临时挂载编号当成录像变化。
// 它是选择范围的元数据校验；实际暂存仍逐字节核验 SHA256，不能用它跳过内容校验。
func sessionSourceRevision(controller Controller, state service.Snapshot, session videoSession) (string, error) {
	provider, ok := controller.(interface {
		RecordingIdentity(service.Snapshot) (string, error)
	})
	if !ok || state.MediaSnapshot == nil || session.RecentWindow || session.ClipType == "recent" {
		return "", errors.New("当前录像不支持跨快照复用选择")
	}
	identity, err := provider.RecordingIdentity(state)
	if err != nil || identity == "" {
		return "", errors.New("无法确认录像盘身份")
	}
	hash := sha256.New()
	encoder := json.NewEncoder(hash)
	if err := encoder.Encode([]any{"session-source-v1", identity, session.ID, session.ClipType, session.StartAt, session.EndAt, session.Duration}); err != nil {
		return "", err
	}
	for _, moment := range session.Moments {
		if err := encoder.Encode([]any{moment.ID, moment.RecordedAt, moment.TimelineStart, moment.Duration}); err != nil {
			return "", err
		}
		videos := slices.Clone(moment.Videos)
		slices.SortFunc(videos, func(a, b teslacam.Video) int { return strings.Compare(a.Path, b.Path) })
		for _, video := range videos {
			path, err := snapshotVideoPath(state.MediaSnapshot.MountPath, video.Path)
			if err != nil {
				return "", err
			}
			info, err := os.Lstat(path)
			if err != nil {
				return "", err
			}
			if !info.Mode().IsRegular() || info.Size() != video.Size {
				return "", errors.New("录像文件与索引不一致")
			}
			if err := encoder.Encode([]any{video.ID, video.Path, video.Camera, video.RecordedAt, video.Duration, info.Size(), info.ModTime().UnixNano()}); err != nil {
				return "", err
			}
		}
	}
	return hex.EncodeToString(hash.Sum(nil)), nil
}

func unchangedSessionSelection(controller Controller, state service.Snapshot, id, revision string) bool {
	if len(revision) != sha256.Size*2 || isRecentWindow(id) || state.MediaSnapshot == nil {
		return false
	}
	session, found := findVideoSession(state.Videos, id)
	if !found {
		return false
	}
	refineVideoSessionDurations(state.MediaSnapshot.MountPath, &session)
	current, err := sessionSourceRevision(controller, state, session)
	return err == nil && current == revision
}
