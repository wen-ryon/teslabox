package media

import (
	"context"
	"errors"
	"io"
	"strings"
	"testing"
)

type fakeStreamingRunner struct {
	name string
	args []string
}

func (r *fakeStreamingRunner) RunStreaming(_ context.Context, name string, args []string, output io.Writer) error {
	r.name = name
	r.args = append([]string(nil), args...)
	_, err := io.WriteString(output, "fragmented-mp4")
	return err
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
	if err := compositor.Stream(t.Context(), CompositeRequest{Layout: "composite6", Quality: "standard", Inputs: inputs}, &output); err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(runner.args, " ")
	for _, expected := range []string{"fps=24", "xstack=inputs=6", "pad=", "-b:v 3000k", "frag_keyframe+empty_moov+default_base_moof"} {
		if !strings.Contains(joined, expected) {
			t.Fatalf("args do not contain %q: %s", expected, joined)
		}
	}
	if runner.name != "/opt/teslabox/media/bin/ffmpeg" || output.String() != "fragmented-mp4" {
		t.Fatalf("runner = %q, output = %q", runner.name, output.String())
	}
}

func TestCompositeArgsDownloadsFramesBeforeSoftwarePadding(t *testing.T) {
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
	if !strings.Contains(joined, "hwdownload,format=nv12,pad=") {
		t.Fatalf("滤镜必须先下载硬件帧再执行软件 pad，避免写入只读 DRM 映射: %s", joined)
	}
	if strings.Contains(joined, "hwmap=mode=read,format=nv12,pad=") {
		t.Fatalf("滤镜不应在只读 DRM 映射上执行 pad: %s", joined)
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

func TestCompositeFourAPlacesLeftRepeaterBeforeRightRepeater(t *testing.T) {
	t.Parallel()
	tiles, err := layoutTiles("composite4a", 1920, 1080)
	if err != nil {
		t.Fatal(err)
	}
	if len(tiles) != 4 || tiles[2].camera != "left_repeater" || tiles[3].camera != "right_repeater" {
		t.Fatalf("tiles = %#v", tiles)
	}
}
