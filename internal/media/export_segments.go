package media

import (
	"context"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/wen-ryon/teslabox/internal/teslasei"
)

const (
	maxClipSegmentDuration  = time.Minute
	segmentBoundaryMerge    = 500 * time.Millisecond
	exportSEISampleInterval = 100 * time.Millisecond
)

type clipExportSegment struct {
	index       int
	startOffset time.Duration
	request     ExportRequest
	source      ExportSource
}

func (e *Exporter) runClipJob(ctx context.Context, task ExportTask, job exportJob) error {
	if err := prepareExportOverlayCues(ctx, job.request, &job.source, e.logger); err != nil {
		return err
	}
	e.updateProgress(task.ID, 1)
	segments, err := planClipExport(job.request, job.source)
	if err != nil {
		return err
	}
	artifacts := make([]string, 0, len(segments)*2+1)
	defer func() {
		for _, path := range artifacts {
			_ = os.Remove(path)
		}
	}()

	segmentPaths := make([]string, 0, len(segments))
	completed := time.Duration(0)
	total := time.Duration(job.request.DurationSeconds * float64(time.Second))
	for _, segment := range segments {
		if err := ctx.Err(); err != nil {
			return err
		}
		outputPath := e.segmentPath(task.ID, segment.index)
		artifacts = append(artifacts, outputPath)
		subtitlePath := ""
		if segment.request.BurnTimestamp || segment.request.BurnSEI {
			subtitlePath = e.segmentSubtitlePath(task.ID, segment.index)
			artifacts = append(artifacts, subtitlePath)
			hasOverlay, err := writeOverlaySubtitles(subtitlePath, segment.request, segment.source.OverlayCues)
			if err != nil {
				return err
			}
			if !hasOverlay {
				subtitlePath = ""
			}
		}
		args, err := exportArgs(segment.request, segment.source, outputPath, subtitlePath)
		if err != nil {
			return err
		}
		segmentDuration := time.Duration(segment.request.DurationSeconds * float64(time.Second))
		e.logger.Info("视频导出分段开始",
			"task_id", task.ID,
			"segment", segment.index+1,
			"segment_count", len(segments),
			"duration_ms", segmentDuration.Milliseconds(),
			"decoder_inputs", exportDecoderInputCount(segment.request, segment.source),
		)
		err = e.runner.RunProgress(ctx, e.config.FFmpegPath, args, func(position time.Duration) {
			position = min(max(position, 0), segmentDuration)
			progress := int(float64(completed+position) / float64(total) * 98)
			e.updateProgress(task.ID, min(98, max(0, progress)))
		})
		if err != nil {
			return err
		}
		completed += segmentDuration
		segmentPaths = append(segmentPaths, outputPath)
	}

	finalTemporary := e.tempPathForTask(task)
	if len(segmentPaths) == 1 {
		return os.Rename(segmentPaths[0], finalTemporary)
	}
	concatPath := e.concatPath(task.ID)
	artifacts = append(artifacts, concatPath)
	if err := writeConcatManifest(concatPath, segmentPaths); err != nil {
		return err
	}
	e.updateProgress(task.ID, 99)
	consumedUntil := make([]time.Duration, len(segments))
	for index, segment := range segments {
		consumedUntil[index] = time.Duration(segment.request.DurationSeconds * float64(time.Second))
		if index > 0 {
			consumedUntil[index] += consumedUntil[index-1]
		}
	}
	removed := 0
	err = e.runner.RunProgress(ctx, e.config.FFmpegPath, concatExportArgs(concatPath, finalTemporary), func(position time.Duration) {
		// concat demuxer 已越过一个分段后即可删除该输入，让最终文件增长时不再额外保留整份分段副本。
		for removed < len(segmentPaths)-1 && position >= consumedUntil[removed]+time.Second {
			_ = os.Remove(segmentPaths[removed])
			removed++
		}
	})
	return err
}

func prepareExportOverlayCues(ctx context.Context, request ExportRequest, source *ExportSource, logger interface {
	Warn(string, ...any)
}) error {
	if source == nil || (!request.BurnTimestamp && !request.BurnSEI) {
		return nil
	}
	source.OverlayCues = nil
	duration := time.Duration(request.DurationSeconds * float64(time.Second))
	if duration <= 0 {
		return &InputError{Message: "导出时长无效"}
	}
	startAt := source.SessionStart.Add(time.Duration(request.StartSeconds * float64(time.Second)))
	preferredCamera := "front"
	if !slices.ContainsFunc(source.Videos, func(video ExportVideo) bool { return video.Camera == preferredCamera }) {
		preferredCamera = ""
		for _, camera := range request.Cameras {
			if slices.ContainsFunc(source.Videos, func(video ExportVideo) bool { return video.Camera == camera }) {
				preferredCamera = camera
				break
			}
		}
	}
	timestampVideos := make([]ExportVideo, 0)
	if preferredCamera != "" {
		for _, video := range source.Videos {
			if video.Camera == preferredCamera {
				timestampVideos = append(timestampVideos, video)
			}
		}
		slices.SortFunc(timestampVideos, func(left, right ExportVideo) int { return left.RecordedAt.Compare(right.RecordedAt) })
	}
	step := time.Second
	if request.BurnSEI {
		// 方向盘转角变化对延迟最敏感；10 Hz 在跟手性和 libass 事件数量之间保持平衡。
		step = exportSEISampleInterval
	}
	for offset := time.Duration(0); offset < duration; offset += step {
		if err := ctx.Err(); err != nil {
			return err
		}
		end := min(offset+step, duration)
		absolute := startAt.Add(offset)
		cue := ExportOverlayCue{Start: offset, End: end}
		if request.BurnTimestamp {
			cue.Timestamp = absolute.Format("2006-01-02 15:04:05")
			for _, video := range timestampVideos {
				videoEnd := video.RecordedAt.Add(time.Duration(video.Duration * float64(time.Second)))
				if absolute.Before(video.RecordedAt) || !absolute.Before(videoEnd) || video.WallClockAt.IsZero() {
					continue
				}
				cue.Timestamp = video.WallClockAt.Add(absolute.Sub(video.RecordedAt)).Format("2006-01-02 15:04:05")
				break
			}
		}
		if cue.Timestamp != "" || request.BurnSEI {
			source.OverlayCues = append(source.OverlayCues, cue)
		}
	}
	if !request.BurnSEI {
		return nil
	}
	for _, video := range timestampVideos {
		videoEnd := video.RecordedAt.Add(time.Duration(video.Duration * float64(time.Second)))
		if !videoEnd.After(startAt) || !video.RecordedAt.Before(startAt.Add(duration)) {
			continue
		}
		metadataItems, err := teslasei.Read(ctx, video.Path)
		if errors.Is(err, context.Canceled) {
			return err
		}
		if errors.Is(err, teslasei.ErrNoMetadata) {
			continue
		}
		if err != nil {
			logger.Warn("导出录像缺少可用 Tesla SEI", "path", video.Path, "error", err)
			continue
		}
		firstIndex := max(0, int(math.Floor(float64(video.RecordedAt.Sub(startAt))/float64(step))))
		for index := firstIndex; index < len(source.OverlayCues); index++ {
			cue := &source.OverlayCues[index]
			absolute := startAt.Add(cue.Start)
			if !absolute.Before(videoEnd) {
				break
			}
			if absolute.Before(video.RecordedAt) || cue.Telemetry != nil {
				continue
			}
			metadata, found := teslasei.At(metadataItems, time.Duration(video.Duration*float64(time.Second)), absolute.Sub(video.RecordedAt))
			if found {
				cue.Telemetry = exportTelemetryFromSEI(metadata, cue.Start)
			}
		}
	}
	return nil
}

func exportTelemetryFromSEI(metadata teslasei.Metadata, offset time.Duration) *ExportTelemetry {
	accelerator := float64(metadata.AcceleratorPosition)
	if accelerator >= -1.01 && accelerator <= 1.01 {
		accelerator *= 100
	}
	return &ExportTelemetry{
		Gear: strings.ToUpper(strings.TrimSpace(metadata.Gear)), SpeedKPH: math.Abs(float64(metadata.SpeedMPS)) * 3.6,
		AcceleratorPosition: min(100, max(0, accelerator)), SteeringAngle: float64(metadata.SteeringAngle),
		LeftBlinker: metadata.LeftBlinker, RightBlinker: metadata.RightBlinker,
		BlinkerVisible: (offset/(500*time.Millisecond))%2 == 0,
		BrakeApplied:   metadata.BrakeApplied, Autopilot: metadata.Autopilot,
	}
}

func planClipExport(request ExportRequest, source ExportSource) ([]clipExportSegment, error) {
	exportStart := source.SessionStart.Add(time.Duration(request.StartSeconds * float64(time.Second)))
	exportDuration := time.Duration(request.DurationSeconds * float64(time.Second))
	exportEnd := exportStart.Add(exportDuration)
	boundaries := clipSegmentBoundaries(request, source, exportStart, exportEnd)
	segments := make([]clipExportSegment, 0, len(boundaries)-1)
	for index := 0; index+1 < len(boundaries); index++ {
		start, end := boundaries[index], boundaries[index+1]
		if !end.After(start) {
			continue
		}
		segmentRequest := request
		segmentRequest.StartSeconds = 0
		segmentRequest.DurationSeconds = end.Sub(start).Seconds()
		segmentSource := ExportSource{SessionStart: start, SessionEnd: end}
		bestByCamera := make(map[string]ExportVideo, len(request.Cameras))
		bestOverlapByCamera := make(map[string]time.Duration, len(request.Cameras))
		for _, video := range source.Videos {
			videoEnd := video.RecordedAt.Add(time.Duration(video.Duration * float64(time.Second)))
			if slices.Contains(request.Cameras, video.Camera) && video.RecordedAt.Before(end) && videoEnd.After(start) {
				overlapStart, overlapEnd := video.RecordedAt, videoEnd
				if overlapStart.Before(start) {
					overlapStart = start
				}
				if overlapEnd.After(end) {
					overlapEnd = end
				}
				overlap := overlapEnd.Sub(overlapStart)
				// 多路录像的分钟边界通常相差数百毫秒。边界合并后若把前后两份文件都交给 FFmpeg，
				// 六路合成会瞬间创建十余个 MPP 解码器并让 RK3568 在后半程逐路掉帧。
				// 每个摄像头只保留覆盖当前分段最长的一份，边缘缺口由现有补帧逻辑保持时间轴连续。
				if overlap > bestOverlapByCamera[video.Camera] {
					bestByCamera[video.Camera] = video
					bestOverlapByCamera[video.Camera] = overlap
				}
			}
		}
		for _, camera := range request.Cameras {
			if video, ok := bestByCamera[camera]; ok {
				segmentSource.Videos = append(segmentSource.Videos, video)
			}
		}
		segmentStartOffset := start.Sub(exportStart)
		segmentEndOffset := end.Sub(exportStart)
		for _, cue := range source.OverlayCues {
			cueStart := max(cue.Start, segmentStartOffset)
			cueEnd := min(cue.End, segmentEndOffset)
			if cueEnd <= cueStart {
				continue
			}
			cue.Start = cueStart - segmentStartOffset
			cue.End = cueEnd - segmentStartOffset
			segmentSource.OverlayCues = append(segmentSource.OverlayCues, cue)
		}
		segments = append(segments, clipExportSegment{
			index: index, startOffset: segmentStartOffset, request: segmentRequest, source: segmentSource,
		})
	}
	if len(segments) == 0 {
		return nil, &InputError{Message: "导出范围内没有可处理的录像片段"}
	}
	return segments, nil
}

func clipSegmentBoundaries(request ExportRequest, source ExportSource, start, end time.Time) []time.Time {
	points := make([]time.Time, 0, len(source.Videos)*2+int(end.Sub(start)/maxClipSegmentDuration)+2)
	for cursor := start.Add(maxClipSegmentDuration); cursor.Before(end); cursor = cursor.Add(maxClipSegmentDuration) {
		points = append(points, cursor)
	}
	for _, video := range source.Videos {
		if !slices.Contains(request.Cameras, video.Camera) {
			continue
		}
		videoEnd := video.RecordedAt.Add(time.Duration(video.Duration * float64(time.Second)))
		if video.RecordedAt.After(start) && video.RecordedAt.Before(end) {
			points = append(points, video.RecordedAt)
		}
		if videoEnd.After(start) && videoEnd.Before(end) {
			points = append(points, videoEnd)
		}
	}
	slices.SortFunc(points, func(left, right time.Time) int { return left.Compare(right) })
	boundaries := []time.Time{start}
	for _, point := range points {
		if point.Sub(boundaries[len(boundaries)-1]) <= segmentBoundaryMerge || end.Sub(point) <= segmentBoundaryMerge {
			continue
		}
		boundaries = append(boundaries, point)
	}
	boundaries = append(boundaries, end)
	return boundaries
}

func (e *Exporter) segmentPath(id string, index int) string {
	return filepath.Join(e.config.OutputDir, fmt.Sprintf(".%s.tmp.part-%04d.mp4", id, index))
}

func (e *Exporter) segmentSubtitlePath(id string, index int) string {
	return filepath.Join(e.config.OutputDir, fmt.Sprintf(".%s.tmp.part-%04d.ass", id, index))
}

func (e *Exporter) concatPath(id string) string {
	return filepath.Join(e.config.OutputDir, "."+id+".tmp.concat.txt")
}

func writeConcatManifest(path string, segmentPaths []string) error {
	var content strings.Builder
	for _, segmentPath := range segmentPaths {
		cleaned := filepath.ToSlash(filepath.Clean(segmentPath))
		cleaned = strings.ReplaceAll(cleaned, "'", "'\\''")
		fmt.Fprintf(&content, "file '%s'\n", cleaned)
	}
	if err := os.WriteFile(path, []byte(content.String()), 0o640); err != nil {
		return fmt.Errorf("写入导出分段清单: %w", err)
	}
	return nil
}

func concatExportArgs(manifestPath, outputPath string) []string {
	return []string{
		"-hide_banner", "-loglevel", "warning", "-nostdin", "-y", "-f", "concat", "-safe", "0",
		"-i", filepath.ToSlash(filepath.Clean(manifestPath)), "-map", "0:v:0", "-an", "-c", "copy",
		"-movflags", "+faststart", "-progress", "pipe:1", "-nostats", filepath.ToSlash(filepath.Clean(outputPath)),
	}
}
