package api

import (
	"crypto/sha256"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/wen-ryon/teslabox/internal/service"
)

const recentWindowSpan = 10 * time.Minute
const recentWindowMomentLimit = 60

type recentCoverage struct {
	Start time.Time `json:"start"`
	End   time.Time `json:"end"`
}

func recentSessionCoverage(session videoSession) []recentCoverage {
	var ranges []recentCoverage
	for _, moment := range session.Moments {
		if len(ranges) > 0 && moment.RecordedAt.Sub(ranges[len(ranges)-1].End) <= time.Second {
			if moment.EndAt.After(ranges[len(ranges)-1].End) {
				ranges[len(ranges)-1].End = moment.EndAt
			}
		} else {
			ranges = append(ranges, recentCoverage{Start: moment.RecordedAt, End: moment.EndAt})
		}
	}
	return ranges
}

// recentWindows 以拍摄时间划分导航窗口，不以估算播放秒数切片；异常密集片段仍有数量上限。
func recentWindows(sessions []videoSession) []videoSession {
	var windows []videoSession
	for _, session := range sessions {
		if session.ClipType != "recent" {
			continue
		}
		for start := 0; start < len(session.Moments); {
			bucket := session.Moments[start].RecordedAt.Truncate(recentWindowSpan)
			end := start + 1
			for end < len(session.Moments) && end-start < recentWindowMomentLimit && session.Moments[end].RecordedAt.Truncate(recentWindowSpan).Equal(bucket) {
				end++
			}
			// 沿用已有真实时间轴算法，但只对所选窗口重新归零。
			window := groupVideoSessions(session.Moments[start:end], sessionContinuityGap)[0]
			hash := sha256.New()
			for _, moment := range window.Moments {
				for _, video := range moment.Videos {
					fmt.Fprintf(hash, "%q:%d:%d;", video.ID, video.Size, video.RecordedAt.UnixNano())
				}
			}
			window.ID = fmt.Sprintf("rw-%x", hash.Sum(nil))
			window.RecentWindow = true
			windows = append(windows, window)
			start = end
		}
	}
	slices.Reverse(windows)
	return windows
}

func isRecentWindow(id string) bool { return strings.HasPrefix(id, "rw-") }

// recentSnapshotMatches 拒绝把旧窗口内的相对秒数套到新的循环录像快照上。
func recentSnapshotMatches(id, requested, current string) bool {
	return !isRecentWindow(id) || requested != "" && requested == current
}

func checkRequestedSnapshot(w http.ResponseWriter, r *http.Request, state service.Snapshot) bool {
	requested := r.URL.Query().Get("snapshot_id")
	if requested != "" && (state.MediaSnapshot == nil || requested != state.MediaSnapshot.ID) {
		writeJSON(w, http.StatusConflict, map[string]string{"error": "录像快照已更新，请重新打开所选时间段"})
		return false
	}
	return true
}
