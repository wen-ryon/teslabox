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
	for _, expected := range []string{
		"fps=24", "scale_rkrga=w=556:h=360", "xstack=inputs=6",
		"layout=0_0|556_0|1112_0|0_360|556_360|1112_360", "-b:v 3000k", "frag_keyframe+empty_moov+default_base_moof",
	} {
		if !strings.Contains(joined, expected) {
			t.Fatalf("args do not contain %q: %s", expected, joined)
		}
	}
	if strings.Contains(joined, "pad=") || !strings.Contains(joined, "force_original_aspect_ratio=disable") {
		t.Fatalf("实时六路合成不应补黑边: %s", joined)
	}
	if runner.name != "/opt/teslabox/media/bin/ffmpeg" || output.String() != "fragmented-mp4" {
		t.Fatalf("runner = %q, output = %q", runner.name, output.String())
	}
}

func TestCompositeArgsDownloadsFramesBeforeSoftwareScaling(t *testing.T) {
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
	if !strings.Contains(joined, "scale_rkrga=w=492:h=320:format=nv12:force_original_aspect_ratio=disable,hwdownload,format=nv12") {
		t.Fatalf("滤镜必须在硬件缩放后下载帧，且直接填满 Tesla 比例画布: %s", joined)
	}
	if strings.Contains(joined, "pad=") || strings.Contains(joined, "hwmap=mode=read") {
		t.Fatalf("滤镜不应在只读 DRM 映射上补黑边: %s", joined)
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
			if strings.Contains(joined, "pad=") || !strings.Contains(joined, "force_original_aspect_ratio=disable") {
				t.Fatalf("实时合成布局不应补黑边: %s", joined)
			}
		})
	}
}
