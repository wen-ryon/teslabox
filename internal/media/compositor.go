package media

import (
	"context"
	"errors"
	"fmt"
	"io"
	"path"
	"strconv"
	"strings"
	"sync"
	"time"
)

const streamAcquireTimeout = 5 * time.Second

// CompositeRequest 集中约束单分钟合成输入，避免客户端直接传递 FFmpeg 参数。
type CompositeRequest struct {
	Layout       string
	Quality      string
	Camera       string
	Inputs       map[string]string
	InputOffsets map[string]float64
}

// InputError 让 API 可以区分用户参数问题和媒体工具链故障。
type InputError struct {
	Message string
}

// Error 只暴露预先定义的安全描述，避免泄漏板端命令细节。
func (e *InputError) Error() string { return e.Message }

type streamingRunner interface {
	RunStreaming(context.Context, string, []string, io.Writer) error
}

// Compositor 隔离硬件合成命令，让 HTTP 层不依赖 FFmpeg 参数细节。
type Compositor struct {
	runner          streamingRunner
	ffmpegPath      string
	streamSlot      chan struct{}
	compatibilityMu sync.RWMutex
	compatibility   map[string]bool
	beginActivity   func() func()
}

// SetActivityHook 设置实时合成期间的可选活动通知。
func (c *Compositor) SetActivityHook(begin func() func()) { c.beginActivity = begin }

// NewCompositor 在启动阶段拒绝不安全路径，避免请求阶段执行任意程序。
func NewCompositor(runner streamingRunner, ffmpegPath string) (*Compositor, error) {
	if runner == nil {
		return nil, errors.New("媒体流执行器不能为空")
	}
	if !isLinuxAbsolutePath(ffmpegPath) {
		return nil, errors.New("FFmpeg 必须使用绝对路径")
	}
	return &Compositor{
		runner:        runner,
		ffmpegPath:    ffmpegPath,
		compatibility: make(map[string]bool),
		// RK356x 同时运行多个实时转码很容易放大峰值功耗和内存压力。
		streamSlot: make(chan struct{}, 1),
	}, nil
}

// Stream 沿用请求上下文，避免浏览器断开后遗留占用硬件的 FFmpeg 进程。
func (c *Compositor) Stream(ctx context.Context, request CompositeRequest, output io.Writer) error {
	if c.beginActivity != nil {
		end := c.beginActivity()
		defer end()
	}
	waitTimer := time.NewTimer(streamAcquireTimeout)
	defer waitTimer.Stop()
	select {
	case c.streamSlot <- struct{}{}:
		defer func() { <-c.streamSlot }()
	case <-ctx.Done():
		return fmt.Errorf("等待实时合成资源: %w", ctx.Err())
	case <-waitTimer.C:
		return errors.New("实时转码资源正在使用，请稍后重试")
	}

	compatibility := c.compatibilitySnapshot()
	for {
		args, tiles, err := compositeArgsWithCompatibility(request, compatibility)
		if err != nil {
			return err
		}
		countingOutput := &byteCountingWriter{output: output}
		if err := c.runner.RunStreaming(ctx, c.ffmpegPath, args, countingOutput); err != nil {
			inputIndex, retry := incompatibleDRMInput(err)
			if countingOutput.bytes > 0 || !retry || inputIndex < 0 || inputIndex >= len(tiles) {
				return fmt.Errorf("实时合成录像: %w", err)
			}
			camera := tiles[inputIndex].camera
			if compatibility[camera] {
				return fmt.Errorf("实时合成录像: %w", err)
			}
			// 仅在 FFmpeg 尚未输出任何 MP4 数据时回退，避免把两段媒体流拼给浏览器。
			compatibility[camera] = true
			c.markCompatibleFallback(camera)
			continue
		}
		return nil
	}
}

type byteCountingWriter struct {
	output io.Writer
	bytes  int64
}

func (w *byteCountingWriter) Write(data []byte) (int, error) {
	written, err := w.output.Write(data)
	w.bytes += int64(written)
	return written, err
}

func (c *Compositor) compatibilitySnapshot() map[string]bool {
	c.compatibilityMu.RLock()
	defer c.compatibilityMu.RUnlock()
	snapshot := make(map[string]bool, len(c.compatibility))
	for camera, enabled := range c.compatibility {
		snapshot[camera] = enabled
	}
	return snapshot
}

func (c *Compositor) markCompatibleFallback(camera string) {
	c.compatibilityMu.Lock()
	defer c.compatibilityMu.Unlock()
	c.compatibility[camera] = true
}

func incompatibleDRMInput(err error) (int, bool) {
	message := err.Error()
	if !strings.Contains(message, "Impossible to convert between the formats") || !strings.Contains(message, "drm_prime") {
		return 0, false
	}
	const marker = "input from stream "
	start := strings.Index(message, marker)
	if start < 0 {
		return 0, false
	}
	start += len(marker)
	end := start
	for end < len(message) && message[end] >= '0' && message[end] <= '9' {
		end++
	}
	if end == start || end >= len(message) || message[end] != ':' {
		return 0, false
	}
	inputIndex, parseErr := strconv.Atoi(message[start:end])
	return inputIndex, parseErr == nil
}

type compositeProfile struct {
	width   int
	height  int
	fps     int
	bitrate int
}

type compositeTile struct {
	camera string
	x      int
	y      int
	width  int
	height int
}

func compositeArgs(request CompositeRequest) ([]string, error) {
	args, _, err := compositeArgsWithCompatibility(request, nil)
	return args, err
}

func compositeArgsWithCompatibility(request CompositeRequest, compatibility map[string]bool) ([]string, []compositeTile, error) {
	profile, ok := map[string]compositeProfile{
		"smooth":   {width: 854, height: 480, fps: 20, bitrate: 1500},
		"standard": {width: 1280, height: 720, fps: 24, bitrate: 3000},
		"high":     {width: 1280, height: 720, fps: 30, bitrate: 5000},
	}[request.Quality]
	if !ok {
		return nil, nil, &InputError{Message: "不支持的播放画质"}
	}
	tiles, err := playbackTiles(request, profile.height)
	if err != nil {
		return nil, nil, err
	}

	args := []string{"-hide_banner", "-loglevel", "warning", "-nostdin"}
	for _, tile := range tiles {
		input := request.Inputs[tile.camera]
		if !isLinuxAbsolutePath(input) {
			return nil, nil, &InputError{Message: "当前片段缺少合成所需视角"}
		}
		args = append(args,
			"-hwaccel", "rkmpp", "-hwaccel_output_format", "drm_prime",
			"-c:v", "h264_rkmpp",
		)
		if offset := request.InputOffsets[tile.camera]; offset > 0 {
			args = append(args, "-ss", strconv.FormatFloat(offset, 'f', 3, 64))
		}
		args = append(args, "-i", path.Clean(input))
	}

	filters := make([]string, 0, len(tiles)+1)
	labels := make([]string, 0, len(tiles))
	layout := make([]string, 0, len(tiles))
	for index, tile := range tiles {
		label := fmt.Sprintf("v%d", index)
		if compatibility[tile.camera] {
			// 某一路实际触发 DRM 格式协商失败后，仅让该路下载并软件缩放。
			// 其余输入继续使用 RGA，兼顾不同固件和摄像头编码参数下的兼容性与性能。
			filters = append(filters, fmt.Sprintf(
				"[%d:v]hwdownload,format=nv12,scale=w=%d:h=%d:force_original_aspect_ratio=disable,fps=%d,setpts=PTS-STARTPTS[%s]",
				index, tile.width, tile.height, profile.fps, label,
			))
		} else {
			filters = append(filters, fmt.Sprintf(
				"[%d:v]scale_rkrga=w=%d:h=%d:format=nv12:force_original_aspect_ratio=disable,hwdownload,format=nv12,fps=%d,setpts=PTS-STARTPTS[%s]",
				index, tile.width, tile.height, profile.fps, label,
			))
		}
		labels = append(labels, "["+label+"]")
		layout = append(layout, strconv.Itoa(tile.x)+"_"+strconv.Itoa(tile.y))
	}
	if len(tiles) == 1 {
		filters = append(filters, labels[0]+"null[out]")
	} else {
		filters = append(filters, strings.Join(labels, "")+fmt.Sprintf(
			"xstack=inputs=%d:layout=%s:fill=black:shortest=0[out]", len(tiles), strings.Join(layout, "|"),
		))
	}

	bitrate := strconv.Itoa(profile.bitrate) + "k"
	args = append(args,
		"-filter_complex", strings.Join(filters, ";"), "-map", "[out]", "-an",
		"-c:v", "h264_rkmpp", "-rc_mode", "CBR", "-b:v", bitrate,
		"-maxrate", bitrate, "-bufsize", strconv.Itoa(profile.bitrate*2)+"k",
		"-g", strconv.Itoa(profile.fps*2), "-fps_mode", "cfr",
		"-movflags", "frag_keyframe+empty_moov+default_base_moof",
		"-f", "mp4", "pipe:1",
	)
	return args, tiles, nil
}

func playbackTiles(request CompositeRequest, height int) ([]compositeTile, error) {
	if request.Layout != "single" {
		return teslaLayoutTiles(request.Layout, height)
	}
	if request.Camera == "" {
		return nil, &InputError{Message: "单路播放缺少摄像头视角"}
	}
	return []compositeTile{{camera: request.Camera, width: teslaAspectWidth(height), height: height}}, nil
}

func gridTiles(cameras []string, columns, rows []int) []compositeTile {
	tiles := make([]compositeTile, 0, len(cameras))
	y := 0
	for row, rowHeight := range rows {
		x := 0
		for column, columnWidth := range columns {
			index := row*len(columns) + column
			if index >= len(cameras) {
				return tiles
			}
			tiles = append(tiles, compositeTile{camera: cameras[index], x: x, y: y, width: columnWidth, height: rowHeight})
			x += columnWidth
		}
		y += rowHeight
	}
	return tiles
}
