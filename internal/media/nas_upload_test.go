package media

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/wen-ryon/teslabox/internal/nasupload"
	"github.com/wen-ryon/teslabox/internal/system"
)

func TestNASProcessorRealBoardQualityAndMergedViews(t *testing.T) {
	input := os.Getenv("TESLABOX_TEST_NAS_INPUT")
	if input == "" {
		t.Skip("set TESLABOX_TEST_NAS_INPUT to verify the board hardware encoder")
	}
	runner := system.NewOSRunner()
	ffmpeg := "/opt/teslabox/media/bin/ffmpeg"
	inspector, err := NewInspector(runner, Config{FFmpegPath: ffmpeg, FFprobePath: "/opt/teslabox/media/bin/ffprobe", Timeout: 15 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	dir := t.TempDir()
	sample := filepath.Join(dir, "sample.mp4")
	if _, err = runner.Run(ctx, ffmpeg, "-hide_banner", "-loglevel", "error", "-nostdin", "-y", "-i", input, "-t", "2", "-an", "-c:v", "copy", sample); err != nil {
		t.Fatal(err)
	}
	processor := NewNASProcessor(runner, inspector, ffmpeg)
	for _, test := range []struct {
		name    string
		quality nasupload.Quality
		cameras []string
	}{{"separate", nasupload.Quality{Mode: "compressed", Height: 480, FPS: 15, BitrateKbps: 1500, Layout: "separate"}, []string{"front"}}, {"merged", nasupload.Quality{Mode: "compressed", Height: 720, FPS: 24, BitrateKbps: 4000, Layout: "composite4a"}, []string{"front", "back", "left_repeater", "right_repeater"}}} {
		t.Run(test.name, func(t *testing.T) {
			inputs := []nasupload.Input{}
			for _, camera := range test.cameras {
				inputs = append(inputs, nasupload.Input{Camera: camera, Path: sample, RecordedAt: time.Date(2026, 10, 2, 12, 0, 0, 0, time.UTC)})
			}
			output := filepath.Join(dir, test.name+".mp4")
			if err = processor.Process(ctx, test.quality, inputs, output); err != nil {
				t.Fatal(err)
			}
			meta, e := inspector.Probe(ctx, output)
			if e != nil {
				t.Fatal(e)
			}
			if len(meta.Streams) == 0 {
				t.Fatal("missing output stream")
			}
			stream := meta.Streams[0]
			if stream.Height != test.quality.Height || stream.AverageFrameRate != strconv.Itoa(test.quality.FPS)+"/1" {
				t.Fatalf("quality mismatch: %+v", stream)
			}
			duration, e := strconv.ParseFloat(meta.Format.Duration, 64)
			if e != nil || duration < 1.9 || duration > 2.2 {
				t.Fatalf("timeline duration: %s", meta.Format.Duration)
			}
			t.Logf("height=%d fps=%s codec=%s duration=%s", stream.Height, stream.AverageFrameRate, stream.CodecName, meta.Format.Duration)
		})
	}
}
