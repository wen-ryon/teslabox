// Package media 封装 TeslaBox 对 FFmpeg 和 ffprobe 的受控调用。
package media

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path"
	"slices"
	"strings"
	"time"
)

// Config 固定媒体工具路径和单次探测时限，避免请求无限占用外部进程。
type Config struct {
	FFmpegPath  string
	FFprobePath string
	Timeout     time.Duration
}

// Capabilities 描述板端媒体工具链实际暴露的 Rockchip 硬件能力。
type Capabilities struct {
	FFmpegVersion        string   `json:"ffmpeg_version"`
	HardwareAcceleration []string `json:"hardware_acceleration"`
	Decoders             []string `json:"decoders"`
	Encoders             []string `json:"encoders"`
	Filters              []string `json:"filters"`
	RockchipReady        bool     `json:"rockchip_ready"`
}

// StreamMetadata 保存视频流中决定转码和同步策略的原始技术参数。
type StreamMetadata struct {
	Index            int    `json:"index"`
	CodecName        string `json:"codec_name"`
	Profile          string `json:"profile,omitempty"`
	CodecType        string `json:"codec_type"`
	Width            int    `json:"width"`
	Height           int    `json:"height"`
	PixelFormat      string `json:"pix_fmt,omitempty"`
	HasBFrames       int    `json:"has_b_frames"`
	FrameRate        string `json:"r_frame_rate"`
	AverageFrameRate string `json:"avg_frame_rate"`
	TimeBase         string `json:"time_base"`
	StartPTS         *int64 `json:"start_pts,omitempty"`
	StartTime        string `json:"start_time,omitempty"`
	DurationTS       *int64 `json:"duration_ts,omitempty"`
	Duration         string `json:"duration,omitempty"`
	BitRate          string `json:"bit_rate,omitempty"`
	FrameCount       string `json:"nb_frames,omitempty"`
}

// FormatMetadata 保存容器层时长、大小和总码率。
type FormatMetadata struct {
	FormatName string `json:"format_name"`
	Duration   string `json:"duration,omitempty"`
	Size       string `json:"size,omitempty"`
	BitRate    string `json:"bit_rate,omitempty"`
}

// Metadata 是 ffprobe 对单个录像文件返回的有限字段集合。
type Metadata struct {
	Streams []StreamMetadata `json:"streams"`
	Format  FormatMetadata   `json:"format"`
}

type commandRunner interface {
	Run(ctx context.Context, name string, args ...string) ([]byte, error)
}

// Inspector 使用可注入命令执行器隔离 Linux 外部进程边界。
type Inspector struct {
	runner commandRunner
	config Config
}

// NewInspector 在启动阶段拒绝不安全路径和无界超时配置。
func NewInspector(runner commandRunner, config Config) (*Inspector, error) {
	if runner == nil {
		return nil, errors.New("媒体命令执行器不能为空")
	}
	if !isLinuxAbsolutePath(config.FFmpegPath) || !isLinuxAbsolutePath(config.FFprobePath) {
		return nil, errors.New("FFmpeg 和 ffprobe 必须使用绝对路径")
	}
	if config.Timeout <= 0 {
		return nil, errors.New("媒体探测超时必须大于零")
	}
	return &Inspector{runner: runner, config: config}, nil
}

// Capabilities 从真实二进制读取能力，避免仅根据配置推断硬件可用性。
func (i *Inspector) Capabilities(parent context.Context) (Capabilities, error) {
	ctx, cancel := context.WithTimeout(parent, i.config.Timeout)
	defer cancel()

	versionOutput, err := i.run(ctx, i.config.FFmpegPath, "-hide_banner", "-version")
	if err != nil {
		return Capabilities{}, fmt.Errorf("读取 FFmpeg 版本: %w", err)
	}
	hardwareOutput, err := i.run(ctx, i.config.FFmpegPath, "-hide_banner", "-hwaccels")
	if err != nil {
		return Capabilities{}, fmt.Errorf("读取 FFmpeg 硬件加速能力: %w", err)
	}
	decoderOutput, err := i.run(ctx, i.config.FFmpegPath, "-hide_banner", "-decoders")
	if err != nil {
		return Capabilities{}, fmt.Errorf("读取 FFmpeg 解码器: %w", err)
	}
	encoderOutput, err := i.run(ctx, i.config.FFmpegPath, "-hide_banner", "-encoders")
	if err != nil {
		return Capabilities{}, fmt.Errorf("读取 FFmpeg 编码器: %w", err)
	}
	filterOutput, err := i.run(ctx, i.config.FFmpegPath, "-hide_banner", "-filters")
	if err != nil {
		return Capabilities{}, fmt.Errorf("读取 FFmpeg 滤镜: %w", err)
	}

	capabilities := Capabilities{
		FFmpegVersion:        firstLine(string(versionOutput)),
		HardwareAcceleration: parseHardwareAcceleration(string(hardwareOutput)),
		Decoders:             parseComponents(string(decoderOutput), "_rkmpp"),
		Encoders:             parseComponents(string(encoderOutput), "_rkmpp"),
		Filters:              parseComponents(string(filterOutput), "_rkrga"),
	}
	capabilities.RockchipReady = slices.Contains(capabilities.HardwareAcceleration, "rkmpp") &&
		slices.Contains(capabilities.Decoders, "h264_rkmpp") &&
		slices.Contains(capabilities.Encoders, "h264_rkmpp") &&
		slices.Contains(capabilities.Filters, "scale_rkrga") &&
		slices.Contains(capabilities.Filters, "overlay_rkrga")
	return capabilities, nil
}

// Probe 只读取转码决策需要的字段，避免把完整 ffprobe 输出和内部路径暴露给 API。
func (i *Inspector) Probe(parent context.Context, videoPath string) (Metadata, error) {
	if !isLinuxAbsolutePath(videoPath) {
		return Metadata{}, errors.New("视频探测路径必须是绝对路径")
	}
	ctx, cancel := context.WithTimeout(parent, i.config.Timeout)
	defer cancel()
	output, err := i.run(ctx, i.config.FFprobePath,
		"-v", "error",
		"-select_streams", "v",
		"-show_entries", "stream=index,codec_name,profile,codec_type,width,height,pix_fmt,has_b_frames,r_frame_rate,avg_frame_rate,time_base,start_pts,start_time,duration_ts,duration,bit_rate,nb_frames:format=format_name,duration,size,bit_rate",
		"-of", "json",
		videoPath,
	)
	if err != nil {
		return Metadata{}, fmt.Errorf("探测视频元数据: %w", err)
	}
	var metadata Metadata
	if err := json.Unmarshal(output, &metadata); err != nil {
		return Metadata{}, fmt.Errorf("解析 ffprobe 输出: %w", err)
	}
	if len(metadata.Streams) == 0 {
		return Metadata{}, errors.New("ffprobe 没有返回视频流")
	}
	return metadata, nil
}

func (i *Inspector) run(ctx context.Context, name string, args ...string) ([]byte, error) {
	output, err := i.runner.Run(ctx, name, args...)
	if err == nil {
		return output, nil
	}
	if ctxErr := ctx.Err(); ctxErr != nil {
		return nil, ctxErr
	}
	return nil, err
}

func firstLine(output string) string {
	line, _, _ := strings.Cut(strings.TrimSpace(output), "\n")
	return strings.TrimSpace(line)
}

func parseHardwareAcceleration(output string) []string {
	result := make([]string, 0, 4)
	for _, line := range strings.Split(output, "\n") {
		value := strings.TrimSpace(line)
		if value == "drm" || value == "rkmpp" {
			result = append(result, value)
		}
	}
	return result
}

func parseComponents(output, suffix string) []string {
	result := make([]string, 0, 16)
	for _, line := range strings.Split(output, "\n") {
		for _, field := range strings.Fields(line) {
			if strings.HasSuffix(field, suffix) && !slices.Contains(result, field) {
				result = append(result, field)
				break
			}
		}
	}
	return result
}

func isLinuxAbsolutePath(value string) bool {
	cleaned := path.Clean(strings.ReplaceAll(strings.TrimSpace(value), "\\", "/"))
	return path.IsAbs(cleaned) && cleaned != "/"
}
