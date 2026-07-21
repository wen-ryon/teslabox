package media

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"
)

type runnerResult struct {
	output string
	err    error
}

type fakeRunner struct {
	results []runnerResult
	calls   [][]string
}

func (r *fakeRunner) Run(_ context.Context, name string, args ...string) ([]byte, error) {
	r.calls = append(r.calls, append([]string{name}, args...))
	result := r.results[0]
	r.results = r.results[1:]
	return []byte(result.output), result.err
}

func TestInspectorCapabilities(t *testing.T) {
	t.Parallel()
	runner := &fakeRunner{results: []runnerResult{
		{output: "ffmpeg version 388741a\n"},
		{output: "Hardware acceleration methods:\ndrm\nrkmpp\n"},
		{output: " V..... h264_rkmpp H264\n V..... hevc_rkmpp HEVC\n"},
		{output: " V..... h264_rkmpp H264\n"},
		{output: " .. overlay_rkrga VV->V\n .. scale_rkrga V->V\n .. vpp_rkrga V->V\n"},
	}}
	inspector, err := NewInspector(runner, Config{
		FFmpegPath: "/opt/teslabox/media/bin/ffmpeg", FFprobePath: "/opt/teslabox/media/bin/ffprobe", Timeout: time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}

	capabilities, err := inspector.Capabilities(t.Context())
	if err != nil {
		t.Fatalf("Capabilities() error = %v", err)
	}
	if !capabilities.RockchipReady || capabilities.FFmpegVersion != "ffmpeg version 388741a" {
		t.Fatalf("capabilities = %#v", capabilities)
	}
	if got, want := capabilities.Decoders, []string{"h264_rkmpp", "hevc_rkmpp"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("decoders = %#v, want %#v", got, want)
	}
	if len(runner.calls) != 5 {
		t.Fatalf("calls = %d, want 5", len(runner.calls))
	}
}

func TestInspectorProbe(t *testing.T) {
	t.Parallel()
	runner := &fakeRunner{results: []runnerResult{{output: `{
  "streams": [{
    "index": 0, "codec_name": "h264", "profile": "High", "codec_type": "video",
    "width": 2896, "height": 1876, "pix_fmt": "yuv420p", "has_b_frames": 0,
    "r_frame_rate": "36/1", "avg_frame_rate": "36/1", "time_base": "1/90000",
    "start_pts": 0, "start_time": "0.000000", "duration_ts": 5400000,
    "duration": "60.000000", "bit_rate": "12000000", "nb_frames": "2160"
  }],
  "format": {"format_name": "mov,mp4,m4a,3gp,3g2,mj2", "duration": "60.000000", "size": "90000000", "bit_rate": "12000000"}
}`}}}
	inspector, err := NewInspector(runner, Config{
		FFmpegPath: "/opt/teslabox/media/bin/ffmpeg", FFprobePath: "/opt/teslabox/media/bin/ffprobe", Timeout: time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}

	metadata, err := inspector.Probe(t.Context(), "/run/teslabox/snapshots/example/TeslaCam/front.mp4")
	if err != nil {
		t.Fatalf("Probe() error = %v", err)
	}
	if len(metadata.Streams) != 1 || metadata.Streams[0].Width != 2896 || metadata.Streams[0].FrameRate != "36/1" {
		t.Fatalf("metadata = %#v", metadata)
	}
	if metadata.Format.Duration != "60.000000" {
		t.Fatalf("format = %#v", metadata.Format)
	}
}

func TestNewInspectorRejectsInvalidConfig(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name   string
		runner commandRunner
		config Config
	}{
		{name: "nil runner", config: Config{FFmpegPath: "/ffmpeg", FFprobePath: "/ffprobe", Timeout: time.Second}},
		{name: "relative ffmpeg", runner: &fakeRunner{}, config: Config{FFmpegPath: "ffmpeg", FFprobePath: "/ffprobe", Timeout: time.Second}},
		{name: "zero timeout", runner: &fakeRunner{}, config: Config{FFmpegPath: "/ffmpeg", FFprobePath: "/ffprobe"}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			if _, err := NewInspector(test.runner, test.config); err == nil {
				t.Fatal("NewInspector() error = nil")
			}
		})
	}
}

func TestInspectorProbeReturnsRunnerError(t *testing.T) {
	t.Parallel()
	runner := &fakeRunner{results: []runnerResult{{err: errors.New("command failed")}}}
	inspector, err := NewInspector(runner, Config{FFmpegPath: "/ffmpeg", FFprobePath: "/ffprobe", Timeout: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := inspector.Probe(t.Context(), "/video.mp4"); err == nil {
		t.Fatal("Probe() error = nil")
	}
}
