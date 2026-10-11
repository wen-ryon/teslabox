package media

import (
	"context"
	"errors"
	"io"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

type fakeStreamingRunner struct {
	name string
	args []string
}

func TestCompositorHLSKeepsRawTransportSeparate(t *testing.T) {
	t.Parallel()
	runner := &fakeStreamingRunner{}
	compositor, err := NewCompositor(runner, "/opt/teslabox/media/bin/ffmpeg")
	if err != nil {
		t.Fatal(err)
	}
	directory := t.TempDir()
	request := CompositeRequest{Layout: "single", Camera: "front", Quality: "smooth", Inputs: map[string]string{"front": "/snapshot/front.mp4"}, HLSDirectory: directory}
	if err := compositor.Stream(t.Context(), request, io.Discard); err != nil {
		t.Fatal(err)
	}
	args := strings.Join(runner.args, " ")
	for _, value := range []string{"-f hls", "-hls_time 1", "-hls_segment_type fmp4", "independent_segments+temp_file", filepath.Join(directory, "index.m3u8"), "-g 10"} {
		if !strings.Contains(args, value) {
			t.Fatalf("missing %q in %s", value, args)
		}
	}
	if strings.Contains(args, "pipe:1") {
		t.Fatal("HLS must not use the unbounded HTTP stream")
	}
	request.HLSDirectory = ""
	if err := compositor.Stream(t.Context(), request, io.Discard); err != nil {
		t.Fatal(err)
	}
	args = strings.Join(runner.args, " ")
	if !strings.Contains(args, "pipe:1") || strings.Contains(args, "-f hls") {
		t.Fatalf("raw transport changed: %s", args)
	}
}

func (r *fakeStreamingRunner) RunStreaming(_ context.Context, name string, args []string, output io.Writer) error {
	r.name = name
	r.args = append([]string(nil), args...)
	_, err := io.WriteString(output, "fragmented-mp4")
	return err
}

type blockingStreamingRunner struct {
	started atomic.Int32
	release chan struct{}
}

type drmFallbackRunner struct {
	calls       [][]string
	failures    int
	partialData bool
}

func (r *drmFallbackRunner) RunStreaming(_ context.Context, _ string, args []string, output io.Writer) error {
	r.calls = append(r.calls, append([]string(nil), args...))
	if r.failures > 0 {
		r.failures--
		if r.partialData {
			_, _ = io.WriteString(output, "partial-mp4")
		}
		return errors.New("Impossible to convert between the formats supported by filter 'graph 0 input from stream 4:0' and 'auto_scale_0'; src: drm_prime")
	}
	_, err := io.WriteString(output, "fragmented-mp4")
	return err
}

func (r *blockingStreamingRunner) RunStreaming(ctx context.Context, _ string, _ []string, _ io.Writer) error {
	r.started.Add(1)
	select {
	case <-r.release:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func TestCompositorStreamBuildsSixCameraPipeline(t *testing.T) {
	t.Parallel()
	runner := &fakeStreamingRunner{}
	compositor, err := NewCompositor(runner, "/opt/teslabox/media/bin/ffmpeg")
	if err != nil {
		t.Fatal(err)
	}
	inputs := map[string]string{}
	for _, camera := range []string{"left_pillar", "front", "right_pillar", "left_repeater", "back", "right_repeater"} {
		inputs[camera] = "/snapshot/" + camera + ".mp4"
	}
	var output strings.Builder
	if err := compositor.Stream(t.Context(), CompositeRequest{
		Layout: "composite6", Quality: "standard", Inputs: inputs,
		InputOffsets: map[string]float64{"front": 50},
	}, &output); err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(runner.args, " ")
	for _, expected := range []string{
		"-ss 50.000", "fps=24", "setpts=PTS-STARTPTS", "scale_rkrga=w=556:h=360", "xstack=inputs=6", "shortest=0",
		"layout=0_0|556_0|1112_0|0_360|556_360|1112_360", "-b:v 3000k", "-g 12", "frag_keyframe+empty_moov+default_base_moof",
	} {
		if !strings.Contains(joined, expected) {
			t.Fatalf("args do not contain %q: %s", expected, joined)
		}
	}
	if strings.Contains(joined, ",pad=") || !strings.Contains(joined, "force_original_aspect_ratio=disable") {
		t.Fatalf("实时六路合成不应补黑边: %s", joined)
	}
	if runner.name != "/opt/teslabox/media/bin/ffmpeg" || output.String() != "fragmented-mp4" {
		t.Fatalf("runner = %q, output = %q", runner.name, output.String())
	}
}

func TestCompositorSerializesStreamingRequests(t *testing.T) {
	t.Parallel()
	runner := &blockingStreamingRunner{release: make(chan struct{}, 2)}
	compositor, err := NewCompositor(runner, "/opt/teslabox/media/bin/ffmpeg")
	if err != nil {
		t.Fatal(err)
	}
	request := CompositeRequest{
		Layout: "single", Quality: "smooth", Camera: "front",
		Inputs: map[string]string{"front": "/snapshot/front.mp4"},
	}
	firstDone := make(chan error, 1)
	go func() {
		firstDone <- compositor.Stream(t.Context(), request, io.Discard)
	}()
	waitForStreamingCalls(t, runner, 1)

	secondDone := make(chan error, 1)
	go func() {
		secondDone <- compositor.Stream(t.Context(), request, io.Discard)
	}()
	time.Sleep(50 * time.Millisecond)
	if got := runner.started.Load(); got != 1 {
		t.Fatalf("并发实时转码数 = %d，期望第二个请求等待", got)
	}

	runner.release <- struct{}{}
	if err := <-firstDone; err != nil {
		t.Fatal(err)
	}
	waitForStreamingCalls(t, runner, 2)
	runner.release <- struct{}{}
	if err := <-secondDone; err != nil {
		t.Fatal(err)
	}
}

func TestCompositorWaitingRequestHonorsContext(t *testing.T) {
	t.Parallel()
	runner := &blockingStreamingRunner{release: make(chan struct{}, 1)}
	compositor, err := NewCompositor(runner, "/opt/teslabox/media/bin/ffmpeg")
	if err != nil {
		t.Fatal(err)
	}
	request := CompositeRequest{
		Layout: "single", Quality: "smooth", Camera: "front",
		Inputs: map[string]string{"front": "/snapshot/front.mp4"},
	}
	firstDone := make(chan error, 1)
	go func() {
		firstDone <- compositor.Stream(t.Context(), request, io.Discard)
	}()
	waitForStreamingCalls(t, runner, 1)

	waitContext, cancel := context.WithCancel(t.Context())
	cancel()
	if err := compositor.Stream(waitContext, request, io.Discard); !errors.Is(err, context.Canceled) {
		t.Fatalf("等待中的请求错误 = %v，期望 context.Canceled", err)
	}
	if got := runner.started.Load(); got != 1 {
		t.Fatalf("已启动实时转码数 = %d，期望仍为 1", got)
	}
	runner.release <- struct{}{}
	if err := <-firstDone; err != nil {
		t.Fatal(err)
	}
}

func waitForStreamingCalls(t *testing.T, runner *blockingStreamingRunner, want int32) {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		if runner.started.Load() >= want {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("实时转码调用数 = %d，期望至少 %d", runner.started.Load(), want)
}

func TestCompositeArgsScalesHardwareFramesBeforeSoftwareFPS(t *testing.T) {
	t.Parallel()
	inputs := map[string]string{
		"front":          "/snapshot/front.mp4",
		"right_repeater": "/snapshot/right_repeater.mp4",
		"left_repeater":  "/snapshot/left_repeater.mp4",
	}
	args, err := compositeArgs(CompositeRequest{Layout: "composite3", Quality: "smooth", Inputs: inputs})
	if err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(args, " ")
	want := "scale_rkrga=w=492:h=320:format=nv12:force_original_aspect_ratio=disable,hwdownload,format=nv12,fps=20,setpts=PTS-STARTPTS"
	if !strings.Contains(joined, want) {
		t.Fatalf("滤镜必须先处理 DRM 硬件帧，再下载并执行软件 FPS 滤镜: %s", joined)
	}
	if strings.Contains(joined, "[0:v]fps=") || strings.Contains(joined, ",pad=") || strings.Contains(joined, "hwmap=mode=read") {
		t.Fatalf("滤镜不应在只读 DRM 映射上补黑边: %s", joined)
	}
}

func TestCompositeArgsUsesCompatibilityFallbackOnlyForDetectedCamera(t *testing.T) {
	t.Parallel()
	inputs := map[string]string{
		"front":          "/snapshot/front.mp4",
		"back":           "/snapshot/back.mp4",
		"left_repeater":  "/snapshot/left_repeater.mp4",
		"right_repeater": "/snapshot/right_repeater.mp4",
	}
	args, _, err := compositeArgsWithCompatibility(
		CompositeRequest{Layout: "composite4a", Quality: "standard", Inputs: inputs},
		map[string]bool{"left_repeater": true},
	)
	if err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(args, " ")
	fallbackFilter := "[2:v]hwdownload,format=nv12,scale=w=556:h=360:force_original_aspect_ratio=disable,fps=24,setpts=PTS-STARTPTS[v2]"
	if !strings.Contains(joined, fallbackFilter) {
		t.Fatalf("检测到不兼容的视角必须在下载 DRM 帧后使用兼容缩放链: %s", joined)
	}
	if strings.Contains(joined, "[2:v]scale_rkrga") {
		t.Fatalf("检测到不兼容的视角不应继续进入 scale_rkrga: %s", joined)
	}
	if !strings.Contains(joined, "[0:v]scale_rkrga") || !strings.Contains(joined, "[1:v]scale_rkrga") || !strings.Contains(joined, "[3:v]scale_rkrga") {
		t.Fatalf("单路兼容回退不应关闭其他视角的 RGA 缩放: %s", joined)
	}
}

func TestCompositorRetriesDetectedDRMInputAndCachesFallback(t *testing.T) {
	t.Parallel()
	runner := &drmFallbackRunner{failures: 1}
	compositor, err := NewCompositor(runner, "/opt/teslabox/media/bin/ffmpeg")
	if err != nil {
		t.Fatal(err)
	}
	inputs := map[string]string{}
	for _, camera := range []string{"left_pillar", "front", "right_pillar", "left_repeater", "back", "right_repeater"} {
		inputs[camera] = "/snapshot/" + camera + ".mp4"
	}
	request := CompositeRequest{Layout: "composite6", Quality: "standard", Inputs: inputs}
	var output strings.Builder
	if err := compositor.Stream(t.Context(), request, &output); err != nil {
		t.Fatal(err)
	}
	if len(runner.calls) != 2 || output.String() != "fragmented-mp4" {
		t.Fatalf("调用次数 = %d，输出 = %q", len(runner.calls), output.String())
	}
	first := strings.Join(runner.calls[0], " ")
	second := strings.Join(runner.calls[1], " ")
	if !strings.Contains(first, "[4:v]scale_rkrga") || !strings.Contains(second, "[4:v]hwdownload,format=nv12,scale=") {
		t.Fatalf("未按报错输入编号切换兼容链：首次=%s；重试=%s", first, second)
	}
	if !strings.Contains(second, "[0:v]scale_rkrga") || !strings.Contains(second, "[5:v]scale_rkrga") {
		t.Fatalf("兼容回退不应影响正常视角：%s", second)
	}

	output.Reset()
	if err := compositor.Stream(t.Context(), request, &output); err != nil {
		t.Fatal(err)
	}
	if len(runner.calls) != 3 || !strings.Contains(strings.Join(runner.calls[2], " "), "[4:v]hwdownload,format=nv12,scale=") {
		t.Fatalf("后续请求未复用已探测的兼容链，调用次数 = %d", len(runner.calls))
	}
}

func TestCompositorDoesNotRetryDRMFailureAfterWritingOutput(t *testing.T) {
	t.Parallel()
	runner := &drmFallbackRunner{failures: 1, partialData: true}
	compositor, err := NewCompositor(runner, "/opt/teslabox/media/bin/ffmpeg")
	if err != nil {
		t.Fatal(err)
	}
	inputs := map[string]string{}
	for _, camera := range []string{"left_pillar", "front", "right_pillar", "left_repeater", "back", "right_repeater"} {
		inputs[camera] = "/snapshot/" + camera + ".mp4"
	}
	var output strings.Builder
	err = compositor.Stream(t.Context(), CompositeRequest{Layout: "composite6", Quality: "standard", Inputs: inputs}, &output)
	if err == nil || len(runner.calls) != 1 || output.String() != "partial-mp4" {
		t.Fatalf("已输出数据后不应重试：error=%v calls=%d output=%q", err, len(runner.calls), output.String())
	}
}

func TestCompositeArgsRejectsMissingCamera(t *testing.T) {
	t.Parallel()
	_, err := compositeArgs(CompositeRequest{Layout: "composite3", Quality: "smooth", Inputs: map[string]string{"front": "/front.mp4"}})
	var inputError *InputError
	if err == nil || !errors.As(err, &inputError) {
		t.Fatalf("error = %v, want InputError", err)
	}
}

func TestCompositeArgsBuildsSingleCameraTranscode(t *testing.T) {
	t.Parallel()
	args, err := compositeArgs(CompositeRequest{
		Layout: "single", Quality: "standard", Camera: "front",
		Inputs: map[string]string{"front": "/snapshot/front.mp4"},
	})
	if err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(args, " ")
	for _, expected := range []string{"scale_rkrga=w=1110:h=720", "[v0]null[out]", "-b:v 3000k"} {
		if !strings.Contains(joined, expected) {
			t.Fatalf("args do not contain %q: %s", expected, joined)
		}
	}
	if strings.Contains(joined, "xstack=inputs=1") {
		t.Fatalf("单路转码不应走 xstack: %s", joined)
	}
}

func TestCompositeFourAPlacesLeftRepeaterBeforeRightRepeater(t *testing.T) {
	t.Parallel()
	tiles, err := teslaLayoutTiles("composite4a", 1080)
	if err != nil {
		t.Fatal(err)
	}
	if len(tiles) != 4 || tiles[2].camera != "left_repeater" || tiles[3].camera != "right_repeater" {
		t.Fatalf("tiles = %#v", tiles)
	}
}

func TestCompositeLayoutsUseTeslaAspectWithoutPadding(t *testing.T) {
	t.Parallel()
	tests := []struct {
		layout string
		want   []string
	}{
		{layout: "composite6", want: []string{"scale_rkrga=w=556:h=360", "layout=0_0|556_0|1112_0|0_360|556_360|1112_360"}},
		{layout: "composite4a", want: []string{"scale_rkrga=w=556:h=360", "layout=0_0|556_0|0_360|556_360"}},
		{layout: "composite4b", want: []string{"scale_rkrga=w=834:h=540", "layout=0_0|0_540|278_540|556_540"}},
		{layout: "composite3", want: []string{"scale_rkrga=w=740:h=480", "layout=0_0|0_480|370_480"}},
	}
	inputs := map[string]string{
		"left_pillar": "/snapshot/left_pillar.mp4", "front": "/snapshot/front.mp4", "right_pillar": "/snapshot/right_pillar.mp4",
		"left_repeater": "/snapshot/left_repeater.mp4", "back": "/snapshot/back.mp4", "right_repeater": "/snapshot/right_repeater.mp4",
	}
	for _, test := range tests {
		t.Run(test.layout, func(t *testing.T) {
			t.Parallel()
			args, err := compositeArgs(CompositeRequest{Layout: test.layout, Quality: "standard", Inputs: inputs})
			if err != nil {
				t.Fatal(err)
			}
			joined := strings.Join(args, " ")
			for _, expected := range test.want {
				if !strings.Contains(joined, expected) {
					t.Fatalf("args do not contain %q: %s", expected, joined)
				}
			}
			if strings.Contains(joined, ",pad=") || !strings.Contains(joined, "force_original_aspect_ratio=disable") {
				t.Fatalf("实时合成布局不应补黑边: %s", joined)
			}
		})
	}
}
