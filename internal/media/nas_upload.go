package media

import (
	"context"
	"errors"
	"math"
	"strconv"
	"time"

	"github.com/wen-ryon/teslabox/internal/nasupload"
)

// NASProcessor 复用导出的时间轴和硬件编码调度，避免上传另起编码进程抢占 RGA。
type NASProcessor struct {
	runner    exportRunner
	inspector *Inspector
	ffmpeg    string
}

// NewNASProcessor 让上传只依赖已暂存的源文件，不持有录像盘快照。
func NewNASProcessor(runner exportRunner, inspector *Inspector, ffmpeg string) *NASProcessor {
	return &NASProcessor{runner: runner, inspector: inspector, ffmpeg: ffmpeg}
}

// Process 每次仅处理一个录像分钟，避免多分钟、多视角超过硬解输入上限。
func (p *NASProcessor) Process(ctx context.Context, q nasupload.Quality, inputs []nasupload.Input, output string) error {
	if len(inputs) == 0 || q.Mode != "compressed" || q.Height < 240 || q.Height > 1080 || q.Height%2 != 0 || q.FPS < 1 || q.FPS > 36 || q.BitrateKbps < 256 || q.BitrateKbps > 20000 {
		return errors.New("上传压缩参数无效")
	}
	source := ExportSource{SessionStart: inputs[0].RecordedAt}
	for _, in := range inputs {
		if in.RecordedAt.Before(source.SessionStart) {
			source.SessionStart = in.RecordedAt
		}
	}
	var end time.Time
	cameras := make([]string, 0, len(inputs))
	for _, in := range inputs {
		meta, err := p.inspector.Probe(ctx, in.Path)
		if err != nil {
			return errors.New("无法确认暂存录像时长")
		}
		duration, err := strconv.ParseFloat(meta.Format.Duration, 64)
		if err != nil || math.IsNaN(duration) || math.IsInf(duration, 0) || duration <= 0 || duration > 120 {
			return errors.New("暂存录像时长无效")
		}
		source.Videos = append(source.Videos, ExportVideo{Camera: in.Camera, Path: in.Path, RecordedAt: in.RecordedAt, Duration: duration})
		cameras = append(cameras, in.Camera)
		videoEnd := in.RecordedAt.Add(time.Duration(duration * float64(time.Second)))
		if videoEnd.After(end) {
			end = videoEnd
		}
	}
	layout := q.Layout
	if layout == "separate" {
		layout = "single"
	}
	request := ExportRequest{Mode: ExportModeClip, Layout: layout, Cameras: cameras, Quality: "standard", DurationSeconds: end.Sub(source.SessionStart).Seconds(), uploadProfile: &compositeProfile{width: teslaAspectWidth(q.Height), height: q.Height, fps: q.FPS, bitrate: q.BitrateKbps}}
	args, err := exportArgs(request, source, output)
	if err != nil {
		return err
	}
	if err = p.runner.RunProgress(ctx, p.ffmpeg, args, nil); err != nil {
		return errors.New("上传录像压缩失败，请检查媒体工具链和系统盘空间")
	}
	return nil
}
