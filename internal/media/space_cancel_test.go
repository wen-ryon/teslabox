package media

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestExportCanceledMediaReleasesSnapshot(t *testing.T) {
	dir := externalExportTestDir(t)
	path := filepath.Join(dir, "front.mp4")
	if err := os.WriteFile(path, []byte("video"), 0o600); err != nil {
		t.Fatal(err)
	}
	e, err := NewExporter(&fakeExportRunner{}, ExportConfig{FFmpegPath: "/opt/ffmpeg", OutputDir: filepath.Join(dir, "exports"), QueueSize: 1, MaxDuration: time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	mediaCtx, cancelMedia := context.WithCancel(t.Context())
	cancelMedia()
	released := make(chan struct{})
	task, err := e.Create(ExportRequest{Mode: ExportModeSource, SessionID: "session", MomentIDs: []string{"minute"}, Cameras: []string{"front"}}, ExportSource{MediaContext: mediaCtx, SourceFiles: []SourceExportFile{{MomentID: "minute", Camera: "front", Name: "front.mp4", Path: path, RecordedAt: time.Now(), Size: 5}}}, func() { close(released) })
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	go e.Run(ctx)
	defer func() {
		cancel()
		if err := e.Wait(t.Context()); err != nil {
			t.Error(err)
		}
	}()
	select {
	case <-released:
	case <-time.After(3 * time.Second):
		t.Fatal("取消后未释放快照")
	}
	result, err := e.Task(task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if result.State != ExportFailed || result.DownloadReady {
		t.Fatalf("取消后不能发布完成文件: %+v", result)
	}
}
