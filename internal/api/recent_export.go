package api

import (
	"context"
	"slices"
	"time"

	"github.com/wen-ryon/teslabox/internal/media"
	"github.com/wen-ryon/teslabox/internal/teslacam"
)

// recentRangeSource 以拍摄时间定位起点附近及范围内的片段，不累计范围之前全天的播放秒数。
func recentRangeSource(ctx context.Context, root string, videos []teslacam.Video, request *media.ExportRequest) (media.ExportSource, error) {
	fail := func(message string) (media.ExportSource, error) {
		return media.ExportSource{}, &media.InputError{Message: message}
	}
	if request.RangeStartAt == nil || request.RangeEndAt == nil || !request.RangeEndAt.After(*request.RangeStartAt) || request.Mode == media.ExportModeSource {
		return fail("请选择有效的录像开始和结束时间，时间范围仅支持剪辑导出")
	}
	start, end := *request.RangeStartAt, *request.RangeEndAt
	if len(request.Cameras) == 0 {
		return fail("请明确选择导出视角")
	}
	var recent []teslacam.Video
	for _, video := range videos {
		if video.ClipType == "recent" {
			recent = append(recent, video)
		}
	}
	groups := groupVideos(recent, videoGroupTolerance)
	slices.SortFunc(groups, func(a, b videoGroup) int { return a.RecordedAt.Compare(b.RecordedAt) })
	// 首段采用起点之前最近的同步片段，随后只遍历用户选择范围；历史长文件不改变后续片段的定位。
	first := 0
	for i := range groups {
		if !groups[i].RecordedAt.After(start) {
			first = i
		} else {
			break
		}
	}
	source := media.ExportSource{SessionStart: start, SessionEnd: end}
	for i := first; i < len(groups) && groups[i].RecordedAt.Before(end); i++ {
		if err := ctx.Err(); err != nil {
			return media.ExportSource{}, err
		}
		group := &groups[i]
		for _, video := range group.Videos {
			if !slices.Contains(request.Cameras, video.Camera) {
				continue
			}
			selected := []teslacam.Video{video}
			teslacam.RefineVideoDurations(root, selected)
			video = selected[0]
			if video.DurationState != "verified" {
				return fail("所选时间范围包含无法确认时长的录像，请调整范围或刷新后重试")
			}
			name, err := snapshotVideoPath(root, video.Path)
			if err != nil {
				return media.ExportSource{}, err
			}
			source.Videos = append(source.Videos, media.ExportVideo{Camera: video.Camera, Path: name, RecordedAt: video.RecordedAt, WallClockAt: video.RecordedAt, Duration: video.Duration})
		}
	}
	// 不把长缺口冻结成看似连续的录像；保留原导出器对亚秒级文件切换间隙的处理。
	for _, camera := range request.Cameras {
		cursor := start
		covered := false
		for _, video := range source.Videos {
			if video.Camera != camera {
				continue
			}
			finish := video.RecordedAt.Add(time.Duration(video.Duration * float64(time.Second)))
			if !finish.After(cursor) {
				continue
			}
			if video.RecordedAt.Sub(cursor) > time.Second {
				return fail("所选视角的时间范围存在录像缺口，请缩小范围后导出")
			}
			if finish.After(cursor) {
				cursor = finish
				covered = true
			}
		}
		if !covered || end.Sub(cursor) > time.Second {
			return fail("所选时间范围超出可用录像，或所选视角存在缺失")
		}
	}
	request.StartSeconds = 0
	request.DurationSeconds = end.Sub(start).Seconds()
	return source, nil
}
