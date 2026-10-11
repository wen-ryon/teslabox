package api

import (
	"context"
	"encoding/json"
	"os"
	"path"
	"path/filepath"
	"testing"
	"time"

	"github.com/wen-ryon/teslabox/internal/nasupload"
	"github.com/wen-ryon/teslabox/internal/recordingcache"
)

func TestAutoEventWorkflowWithConfiguredNAS(t *testing.T) {
	stateFile, executable := os.Getenv("TESLABOX_AUTO_TEST_TARGET_FILE"), os.Getenv("TESLABOX_TEST_RCLONE")
	if stateFile == "" || executable == "" {
		t.Skip("explicit test target state and rclone are required")
	}
	// 仅在明确指定的测试环境读取已保存凭据，绝不输出配置或原始响应。
	data, err := os.ReadFile(stateFile)
	if err != nil {
		t.Fatal("cannot read test target state")
	}
	var saved struct {
		DefaultTargetID string             `json:"default_target_id"`
		Targets         []nasupload.Target `json:"targets"`
	}
	if json.Unmarshal(data, &saved) != nil {
		t.Fatal("invalid test target state")
	}
	var target nasupload.Target
	for _, candidate := range saved.Targets {
		if candidate.ID == saved.DefaultTargetID {
			target = candidate
		}
	}
	if target.ID == "" || target.Credential == "" {
		t.Fatal("saved default test target is required")
	}
	_, controller, start := automaticFixture(t, "event", "")
	media := filepath.Join(t.TempDir(), "media")
	uploadRoot := filepath.Join(media, "uploads")
	cache, err := recordingcache.New(recordingcache.Config{Root: filepath.Join(media, "recordings"), MediaRoot: media, MaxBytes: 32 << 20, MaxTotalBytes: 64 << 20, MinFreeBytes: 1, IdleTTL: 0})
	if err != nil {
		t.Fatal(err)
	}
	if err = os.MkdirAll(uploadRoot, 0o700); err != nil {
		t.Fatal(err)
	}
	target.Path = path.Join(target.Path, ".teslabox-automatic-validation", time.Now().UTC().Format("20060102T150405.000000000"))
	target.Quality = nasupload.Quality{Mode: "original", Layout: "separate"}
	config := nasupload.AutoConfig{Enabled: true, IncludeExisting: true, Scope: "event"}
	state, err := json.Marshal(map[string]any{"version": 1, "default_target_id": target.ID, "targets": []nasupload.Target{target}, "automatic": config, "jobs": []any{}})
	if err != nil {
		t.Fatal("cannot create isolated test plan")
	}
	if err = os.WriteFile(filepath.Join(uploadRoot, "state.json"), state, 0o600); err != nil {
		t.Fatal(err)
	}
	m, err := nasupload.New(nasupload.Config{Root: uploadRoot, Cache: cache, Transport: &nasupload.Rclone{Executable: executable, WorkDir: uploadRoot}})
	if err != nil {
		t.Fatal("cannot initialize isolated real transport")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	go m.Run(ctx)
	t.Cleanup(func() {
		cancel()
		wait, stop := context.WithTimeout(context.Background(), 5*time.Second)
		defer stop()
		if err := m.Wait(wait); err != nil {
			t.Error(err)
		}
	})
	if err = eventUploadTick(ctx, controller, m, nil, start); err != nil {
		t.Fatal(err)
	}
	if len(m.History()) != 0 {
		t.Fatal("first snapshot triggered early")
	}
	controller.state.MediaSnapshot.ID = "b"
	if err = eventUploadTick(ctx, controller, m, nil, start.Add(20*time.Second)); err != nil {
		t.Fatal(err)
	}
	for ctx.Err() == nil {
		tasks := m.History()
		if len(tasks) == 1 && tasks[0].State == "completed" {
			if len(tasks[0].Files) != 1 || tasks[0].Request.Role != "event" || tasks[0].Files[0].Cameras[0] != "left_pillar" {
				t.Fatal("wrong event result")
			}
			controller.state.MediaSnapshot.ID = "c"
			if err = eventUploadTick(ctx, controller, m, nil, start.Add(40*time.Second)); err != nil {
				t.Fatal(err)
			}
			if len(m.History()) != 1 || controller.acquired.Load() != controller.released.Load() {
				t.Fatal("duplicate or source lease retained")
			}
			t.Log("real NAS event workflow passed: stable snapshots, system-disk staging, SHA256 readback, and repeated-scan deduplication")
			return
		}
		if len(tasks) > 0 && tasks[0].State == "failed" {
			t.Fatal(tasks[0].Error)
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatal("real NAS event workflow timed out")
}
