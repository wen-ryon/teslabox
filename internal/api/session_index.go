package api

import (
	"sync"

	"github.com/wen-ryon/teslabox/internal/teslacam"
)

// sessionIndex 只保留一个快照的纯元数据，不借用磁盘快照；分页请求共用一次分组结果。
type sessionIndex struct {
	mu       sync.Mutex
	id       string
	sessions []videoSession
	recent   []videoSession
}

func (index *sessionIndex) get(id string, videos []teslacam.Video) []videoSession {
	return index.getList(id, videos, false)
}

func (index *sessionIndex) getList(id string, videos []teslacam.Video, windowed bool) []videoSession {
	index.mu.Lock()
	defer index.mu.Unlock()
	if id == "" {
		sessions := groupVideoSessions(groupVideos(videos, videoGroupTolerance), sessionContinuityGap)
		if windowed {
			return recentWindows(sessions)
		}
		return sessions
	}
	if id != index.id {
		index.sessions = groupVideoSessions(groupVideos(videos, videoGroupTolerance), sessionContinuityGap)
		index.recent = nil
		index.id = id
	}
	if windowed {
		if index.recent == nil {
			index.recent = recentWindows(index.sessions)
		}
		return index.recent
	}
	return index.sessions
}

func cloneSessionForDuration(session videoSession) videoSession {
	session.Moments = append([]videoGroup(nil), session.Moments...)
	for i := range session.Moments {
		session.Moments[i].Videos = append([]teslacam.Video(nil), session.Moments[i].Videos...)
	}
	return session
}
