package mediaassets

import (
	"bytes"
	"context"
	"image"
	"image/color"
	"image/png"
	"io"
	"os"
	"path/filepath"
	"testing"

	mediasnapshot "github.com/wen-ryon/teslabox/internal/snapshot"
	"github.com/wen-ryon/teslabox/internal/system"
)

type fakeMaintenance struct{ calls int }

func (m *fakeMaintenance) BeginMediaMaintenance(context.Context) (func(), error) {
	m.calls++
	return func() {}, nil
}

type unusedRunner struct{}

func (unusedRunner) Run(context.Context, string, ...string) ([]byte, error) {
	return nil, nil
}

var _ system.Runner = unusedRunner{}

type fakeSnapshotSource struct {
	view      *mediasnapshot.View
	scanErr   error
	scanCalls int
	released  int
}

func (s *fakeSnapshotSource) ActivateMedia(context.Context) error {
	s.scanCalls++
	return s.scanErr
}

func (s *fakeSnapshotSource) AcquireMediaSnapshotView() (*mediasnapshot.View, func()) {
	return s.view, func() { s.released++ }
}

type musicScanRunner struct{ root string }

func (r musicScanRunner) Run(_ context.Context, name string, args ...string) ([]byte, error) {
	switch name {
	case "mount":
		mountPath := args[len(args)-1]
		if err := os.WriteFile(filepath.Join(mountPath, "manual-song.mp3"), []byte("music"), 0o644); err != nil {
			return nil, err
		}
		nested := filepath.Join(mountPath, "热门歌曲", "精选")
		if err := os.MkdirAll(nested, 0o750); err != nil {
			return nil, err
		}
		if err := os.WriteFile(filepath.Join(nested, "nested-song.mp3"), []byte("music"), 0o644); err != nil {
			return nil, err
		}
		if err := os.WriteFile(filepath.Join(nested, "cover.txt"), []byte("not music"), 0o644); err != nil {
			return nil, err
		}
	}
	return nil, nil
}

type externalDeleteRunner struct{}

func (externalDeleteRunner) Run(_ context.Context, name string, args ...string) ([]byte, error) {
	switch name {
	case "losetup":
		if len(args) > 0 && args[0] == "--find" {
			return []byte("/dev/loop8\n"), nil
		}
	case "mount":
		mountPath := args[len(args)-1]
		nested := filepath.Join(mountPath, "合集", "第一辑")
		if err := os.MkdirAll(nested, 0o750); err != nil {
			return nil, err
		}
		if err := os.WriteFile(filepath.Join(nested, "manual-song.mp3"), []byte("music"), 0o644); err != nil {
			return nil, err
		}
	}
	return nil, nil
}

type recordingDeleteRunner struct{}

func (recordingDeleteRunner) Run(_ context.Context, name string, args ...string) ([]byte, error) {
	switch name {
	case "losetup":
		if len(args) > 0 && args[0] == "--find" {
			return []byte("/dev/loop9\n"), nil
		}
	case "mount":
		mountPath := args[len(args)-1]
		eventPath := filepath.Join(mountPath, "TeslaCam", "SavedClips", "2026-07-26_03-30-00")
		if err := os.MkdirAll(eventPath, 0o750); err != nil {
			return nil, err
		}
		for name, content := range map[string]string{
			"2026-07-26_03-30-00-front.mp4": "front",
			"2026-07-26_03-30-00-back.mp4":  "back!!",
			"notes.txt":                     "keep",
			"event.json":                    "{}",
			"thumb.png":                     "thumb",
		} {
			if err := os.WriteFile(filepath.Join(eventPath, name), []byte(content), 0o644); err != nil {
				return nil, err
			}
		}
		adjacentPath := filepath.Join(mountPath, "TeslaCam", "SavedClips", "2026-07-26_03-31-00")
		if err := os.MkdirAll(adjacentPath, 0o750); err != nil {
			return nil, err
		}
		if err := os.WriteFile(filepath.Join(adjacentPath, "event.json"), []byte("other"), 0o644); err != nil {
			return nil, err
		}
		recentPath := filepath.Join(mountPath, "TeslaCam", "RecentClips")
		if err := os.MkdirAll(recentPath, 0o750); err != nil {
			return nil, err
		}
		if err := os.WriteFile(filepath.Join(recentPath, "2026-07-26_03-32-00-front.mp4"), []byte("recent"), 0o644); err != nil {
			return nil, err
		}
		if err := os.WriteFile(filepath.Join(recentPath, "thumb.png"), []byte("recent-thumb"), 0o644); err != nil {
			return nil, err
		}
		sentryPath := filepath.Join(mountPath, "TeslaCam", "SentryClips", "2026-07-26_03-33-00")
		if err := os.MkdirAll(sentryPath, 0o750); err != nil {
			return nil, err
		}
		if err := os.WriteFile(filepath.Join(sentryPath, "2026-07-26_03-33-00-front.mp4"), []byte("sentry"), 0o644); err != nil {
			return nil, err
		}
		if err := os.WriteFile(filepath.Join(sentryPath, "event.json"), []byte("{}"), 0o644); err != nil {
			return nil, err
		}
	}
	return nil, nil
}

type recordingDeleteReclaimRunner struct {
	recordingDeleteRunner
	detached bool
}

func (r *recordingDeleteReclaimRunner) Run(ctx context.Context, name string, args ...string) ([]byte, error) {
	output, err := r.recordingDeleteRunner.Run(ctx, name, args...)
	if name == "losetup" && len(args) > 0 && args[0] == "--detach" {
		r.detached = true
	}
	return output, err
}

type capturedCommand struct {
	name string
	args []string
}

type captureRunner struct {
	commands    []capturedCommand
	detached    bool
	failDiscard bool
}

func (r *captureRunner) Run(_ context.Context, name string, args ...string) ([]byte, error) {
	r.commands = append(r.commands, capturedCommand{name: name, args: append([]string(nil), args...)})
	if name == "mount" && r.failDiscard && len(args) >= 2 && args[1] == "rw,nosuid,nodev,noexec,discard" {
		return nil, &system.CommandError{Name: "mount", Code: 32, Output: "unknown option discard"}
	}
	if name == "losetup" && len(args) > 0 && args[0] == "--find" {
		return []byte("/dev/loop10\n"), nil
	}
	if name == "losetup" && len(args) > 0 && args[0] == "--detach" {
		r.detached = true
	}
	return nil, nil
}

func TestManagerStagesValidWrapAndDoesNotExposeStagingPath(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	imagePath := filepath.Join(root, "images", "teslacam.img")
	if err := os.MkdirAll(filepath.Dir(imagePath), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(imagePath, []byte("image"), 0o600); err != nil {
		t.Fatal(err)
	}
	manager, err := New(Config{
		ImagePath: imagePath, StatePath: filepath.Join(root, "state"), StagingPath: filepath.Join(root, "state", "media-staging"), MountPath: filepath.Join(root, "mount"),
	}, unusedRunner{}, &fakeMaintenance{})
	if err != nil {
		t.Fatal(err)
	}
	assets, err := manager.Upload(t.Context(), CategoryWrap, []UploadFile{{FileName: "Blue Wrap.png", Content: validPNG(t, 512, 512)}})
	if err != nil {
		t.Fatalf("Upload() error = %v", err)
	}
	if len(assets) != 1 || !assets[0].Pending || !assets[0].PreviewAvailable || assets[0].Files[0].Staged != "" {
		t.Fatalf("public assets = %#v", assets)
	}
	preview, asset, err := manager.OpenPreview(t.Context(), assets[0].ID)
	if err != nil {
		t.Fatalf("OpenPreview() error = %v", err)
	}
	preview.Close()
	if asset.Files[0].Staged != "" {
		t.Fatalf("public preview asset leaked staging path: %#v", asset)
	}
	status, err := manager.Status(t.Context())
	if err != nil || !status.NeedsApply || len(status.Assets) != 1 {
		t.Fatalf("Status() = %#v, %v", status, err)
	}
}

func TestManagerRejectsInvalidWrapAndRemovesPartialStaging(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	manager, err := New(Config{
		ImagePath: filepath.Join(root, "images", "teslacam.img"), StatePath: filepath.Join(root, "state"), StagingPath: filepath.Join(root, "state", "media-staging"), MountPath: filepath.Join(root, "mount"),
	}, unusedRunner{}, &fakeMaintenance{})
	if err != nil {
		t.Fatal(err)
	}
	_, err = manager.Upload(t.Context(), CategoryWrap, []UploadFile{{FileName: "invalid.png", Content: validPNG(t, 511, 511)}})
	if err == nil {
		t.Fatal("expected invalid wrap dimensions to fail")
	}
	entries, readErr := os.ReadDir(filepath.Join(root, "state", "media-staging"))
	if readErr != nil && !os.IsNotExist(readErr) {
		t.Fatal(readErr)
	}
	if len(entries) != 0 {
		t.Fatalf("partial staging entries = %#v", entries)
	}
}

func TestManagerStagesPairedLightShowAndDeletesPendingAsset(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	manager, err := New(Config{
		ImagePath: filepath.Join(root, "images", "teslacam.img"), StatePath: filepath.Join(root, "state"), StagingPath: filepath.Join(root, "state", "media-staging"), MountPath: filepath.Join(root, "mount"),
	}, unusedRunner{}, &fakeMaintenance{})
	if err != nil {
		t.Fatal(err)
	}
	assets, err := manager.Upload(t.Context(), CategoryLightShow, []UploadFile{
		{Role: "sequence", FileName: "show-one.fseq", Content: bytes.NewBufferString("sequence")},
		{Role: "audio", FileName: "show-one.wav", Content: bytes.NewBufferString("audio")},
	})
	if err != nil || len(assets) != 1 || len(assets[0].Files) != 2 {
		t.Fatalf("Upload() = %#v, %v", assets, err)
	}
	status, err := manager.Delete(t.Context(), assets[0].ID)
	if err != nil || len(status.Assets) != 0 || status.NeedsApply {
		t.Fatalf("Delete() = %#v, %v", status, err)
	}
}

func TestNormalizeTeslaAssetNameAndEnsureRootLayout(t *testing.T) {
	t.Parallel()
	name, err := normalizeName(CategoryWrap, "我的 皮肤@01.png")
	if err != nil {
		t.Fatal(err)
	}
	if name != "01.png" {
		t.Fatalf("normalized wrap name = %q", name)
	}
	root := t.TempDir()
	if err := ensureTeslaMediaRootLayout(root); err != nil {
		t.Fatal(err)
	}
	for _, directory := range []string{"TeslaCam", "Boombox", "Wraps", "LightShow"} {
		info, err := os.Stat(filepath.Join(root, directory))
		if err != nil || !info.IsDir() {
			t.Fatalf("root layout %s = %#v, %v", directory, info, err)
		}
	}
}

func TestManagerStatusDiscoversDirectlyWrittenMediaFromSnapshot(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	imagePath := filepath.Join(root, "images", "teslacam.img")
	if err := os.MkdirAll(filepath.Dir(imagePath), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(imagePath, []byte("image"), 0o600); err != nil {
		t.Fatal(err)
	}
	viewRoot := filepath.Join(root, "snapshot")
	if err := os.MkdirAll(filepath.Join(viewRoot, "Boombox"), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(viewRoot, "Wraps"), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(viewRoot, "Boombox", "manual.wav"), []byte("audio"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(viewRoot, "Wraps", "manual.png"), []byte("png"), 0o644); err != nil {
		t.Fatal(err)
	}
	source := &fakeSnapshotSource{view: &mediasnapshot.View{ID: "snapshot", MountPath: viewRoot}}
	manager, err := New(Config{
		ImagePath: imagePath, StatePath: filepath.Join(root, "state"), StagingPath: filepath.Join(root, "state", "media-staging"), MountPath: filepath.Join(root, "mount"),
	}, unusedRunner{}, &fakeMaintenance{}, source)
	if err != nil {
		t.Fatal(err)
	}

	status, err := manager.Status(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if source.scanCalls != 1 || source.released != 1 || len(status.Assets) != 2 {
		t.Fatalf("snapshot calls = %d/%d, assets = %#v", source.scanCalls, source.released, status.Assets)
	}
	for _, asset := range status.Assets {
		if !asset.External || !asset.Applied || asset.Pending || asset.PendingDelete {
			t.Fatalf("direct asset = %#v", asset)
		}
	}
}

func TestManagerScansMusicPartitionRecursivelyFromReadOnlySnapshot(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	imagePath := filepath.Join(root, "image.img")
	if err := os.WriteFile(imagePath, []byte("image"), 0o600); err != nil {
		t.Fatal(err)
	}
	viewRoot := filepath.Join(root, "snapshot")
	if err := os.MkdirAll(viewRoot, 0o750); err != nil {
		t.Fatal(err)
	}
	source := &fakeSnapshotSource{view: &mediasnapshot.View{ID: "snapshot", MountPath: viewRoot, LoopDevice: "/dev/loop7"}}
	manager, err := New(Config{
		ImagePath: imagePath, StatePath: filepath.Join(root, "state"), StagingPath: filepath.Join(root, "state", "media-staging"), MountPath: filepath.Join(root, "mount", "p1"),
	}, musicScanRunner{root: root}, &fakeMaintenance{}, source)
	if err != nil {
		t.Fatal(err)
	}

	status, err := manager.Status(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if source.scanCalls != 1 || source.released != 1 || len(status.Assets) != 2 {
		t.Fatalf("snapshot calls = %d/%d, assets = %#v", source.scanCalls, source.released, status.Assets)
	}
	got := []string{status.Assets[0].Name, status.Assets[1].Name}
	if got[0] != "manual-song.mp3" || got[1] != "热门歌曲/精选/nested-song.mp3" {
		t.Fatalf("music names = %#v", got)
	}
}

func TestManagerDeleteExternalMediaUsesMaintenanceWindow(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	imagePath := filepath.Join(root, "image.img")
	if err := os.WriteFile(imagePath, []byte("image"), 0o600); err != nil {
		t.Fatal(err)
	}
	maintenance := &fakeMaintenance{}
	mountPath := filepath.Join(root, "mount")
	manager, err := New(Config{
		ImagePath: imagePath, StatePath: filepath.Join(root, "state"), StagingPath: filepath.Join(root, "state", "media-staging"), MountPath: mountPath,
	}, externalDeleteRunner{}, maintenance)
	if err != nil {
		t.Fatal(err)
	}
	file := File{Name: "合集/第一辑/manual-song.mp3", SizeBytes: 5}
	asset := Asset{
		ID: externalAssetID(CategoryMusic, []File{file}), Category: CategoryMusic, Files: []File{file}, Applied: true, External: true,
	}
	if _, err := manager.DeleteExternal(t.Context(), asset); err != nil {
		t.Fatal(err)
	}
	if maintenance.calls != 1 {
		t.Fatalf("maintenance calls = %d, want 1", maintenance.calls)
	}
	if _, err := os.Stat(filepath.Join(mountPath, "p2", "合集", "第一辑", "manual-song.mp3")); !os.IsNotExist(err) {
		t.Fatalf("direct media exists after deletion: %v", err)
	}
}

func TestManagerDeleteRecordingsUsesMaintenanceWindowAndServerPaths(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	imagePath := filepath.Join(root, "image.img")
	if err := os.WriteFile(imagePath, []byte("image"), 0o600); err != nil {
		t.Fatal(err)
	}
	maintenance := &fakeMaintenance{}
	mountPath := filepath.Join(root, "mount")
	manager, err := New(Config{
		ImagePath: imagePath, StatePath: filepath.Join(root, "state"), StagingPath: filepath.Join(root, "state", "media-staging"), MountPath: mountPath,
	}, recordingDeleteRunner{}, maintenance)
	if err != nil {
		t.Fatal(err)
	}
	result, err := manager.DeleteRecordings(t.Context(), RecordingDeleteRequest{Paths: []string{
		"TeslaCam/SavedClips/2026-07-26_03-30-00/2026-07-26_03-30-00-front.mp4",
		"TeslaCam/SavedClips/2026-07-26_03-30-00/2026-07-26_03-30-00-back.mp4",
	}})
	if err != nil {
		t.Fatal(err)
	}
	if maintenance.calls != 1 || result.DeletedFiles != 5 || result.DeletedBytes != 22 || result.DeletedEvents != 1 {
		t.Fatalf("DeleteRecordings() = %#v, maintenance calls = %d", result, maintenance.calls)
	}
	eventPath := filepath.Join(mountPath, "p1", "TeslaCam", "SavedClips", "2026-07-26_03-30-00")
	if _, err := os.Stat(eventPath); !os.IsNotExist(err) {
		t.Fatalf("event directory exists after deletion: %v", err)
	}
	adjacentFile := filepath.Join(mountPath, "p1", "TeslaCam", "SavedClips", "2026-07-26_03-31-00", "event.json")
	if content, err := os.ReadFile(adjacentFile); err != nil || string(content) != "other" {
		t.Fatalf("adjacent event changed: %q, %v", content, err)
	}
}

func TestManagerDeleteRecordingsAndReclaimUsesOneMaintenanceWindow(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	imagePath := filepath.Join(root, "image.img")
	if err := os.WriteFile(imagePath, []byte("image"), 0o600); err != nil {
		t.Fatal(err)
	}
	maintenance := &fakeMaintenance{}
	runner := &recordingDeleteReclaimRunner{}
	manager, err := New(Config{
		ImagePath: imagePath, StatePath: filepath.Join(root, "state"),
		StagingPath: filepath.Join(root, "state", "media-staging"), MountPath: filepath.Join(root, "mount"),
	}, runner, maintenance)
	if err != nil {
		t.Fatal(err)
	}
	manager.reclaim = func(_ context.Context, path string) (SpaceReclaimResult, error) {
		if path != imagePath || !runner.detached {
			t.Fatalf("reclaim path = %q, detached = %t", path, runner.detached)
		}
		return SpaceReclaimResult{CandidateBytes: 4096, ReclaimedBytes: 2048, ProcessedRanges: 2}, nil
	}
	result, err := manager.DeleteRecordingsAndReclaim(t.Context(), RecordingDeleteRequest{Paths: []string{
		"TeslaCam/SavedClips/2026-07-26_03-30-00/2026-07-26_03-30-00-front.mp4",
	}})
	if err != nil {
		t.Fatalf("DeleteRecordingsAndReclaim() error = %v", err)
	}
	if maintenance.calls != 1 || result.DeletedEvents != 1 || result.SpaceReclaim.ReclaimedBytes != 2048 || result.ReclaimError != "" {
		t.Fatalf("DeleteRecordingsAndReclaim() = %#v, maintenance calls = %d", result, maintenance.calls)
	}
}

func TestManagerReclaimSpaceUsesMaintenanceAndDetachesLoopBeforePunching(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	imagePath := filepath.Join(root, "image.img")
	if err := os.WriteFile(imagePath, []byte("image"), 0o600); err != nil {
		t.Fatal(err)
	}
	maintenance := &fakeMaintenance{}
	runner := &captureRunner{}
	manager, err := New(Config{
		ImagePath: imagePath, StatePath: filepath.Join(root, "state"), StagingPath: filepath.Join(root, "state", "media-staging"), MountPath: filepath.Join(root, "mount"),
	}, runner, maintenance)
	if err != nil {
		t.Fatal(err)
	}
	manager.reclaim = func(_ context.Context, path string) (SpaceReclaimResult, error) {
		if path != imagePath || !runner.detached {
			t.Fatalf("reclaim path = %q, detached = %t", path, runner.detached)
		}
		return SpaceReclaimResult{CandidateBytes: 4096, ReclaimedBytes: 2048, ProcessedRanges: 2}, nil
	}
	result, err := manager.ReclaimSpace(t.Context())
	if err != nil {
		t.Fatalf("ReclaimSpace() error = %v", err)
	}
	if maintenance.calls != 1 || result.ReclaimedBytes != 2048 {
		t.Fatalf("ReclaimSpace() = %#v, maintenance calls = %d", result, maintenance.calls)
	}
	var checked bool
	for _, command := range runner.commands {
		if command.name == "fsck.exfat" {
			checked = true
		}
	}
	if !checked {
		t.Fatal("ReclaimSpace() did not check exFAT before punching holes")
	}
}

func TestManagerMountPartitionEnablesDiscardOnlyForKernelExFAT(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name        string
		exFAT       bool
		wantOptions string
	}{
		{name: "exfat", exFAT: true, wantOptions: "rw,nosuid,nodev,noexec,discard"},
		{name: "fat32", exFAT: false, wantOptions: "rw,nosuid,nodev,noexec"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			root := t.TempDir()
			runner := &captureRunner{}
			manager, err := New(Config{
				ImagePath: filepath.Join(root, "image.img"), StatePath: filepath.Join(root, "state"),
				StagingPath: filepath.Join(root, "staging"), MountPath: filepath.Join(root, "mount"),
			}, runner, &fakeMaintenance{})
			if err != nil {
				t.Fatal(err)
			}
			if _, err := manager.mountPartition(t.Context(), "/dev/loop10p1", 1, tt.exFAT); err != nil {
				t.Fatalf("mountPartition() error = %v", err)
			}
			var options string
			for _, command := range runner.commands {
				if command.name == "mount" && len(command.args) >= 2 {
					options = command.args[1]
				}
			}
			if options != tt.wantOptions {
				t.Fatalf("mount options = %q, want %q", options, tt.wantOptions)
			}
		})
	}
}

func TestManagerMountPartitionFallsBackWhenDiscardIsUnsupported(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	runner := &captureRunner{failDiscard: true}
	manager, err := New(Config{
		ImagePath: filepath.Join(root, "image.img"), StatePath: filepath.Join(root, "state"),
		StagingPath: filepath.Join(root, "staging"), MountPath: filepath.Join(root, "mount"),
	}, runner, &fakeMaintenance{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := manager.mountPartition(t.Context(), "/dev/loop10p1", 1, true); err != nil {
		t.Fatalf("mountPartition() error = %v", err)
	}
	var mountOptions []string
	for _, command := range runner.commands {
		if command.name == "mount" && len(command.args) >= 2 {
			mountOptions = append(mountOptions, command.args[1])
		}
	}
	want := []string{"rw,nosuid,nodev,noexec,discard", "rw,nosuid,nodev,noexec"}
	if len(mountOptions) != len(want) || mountOptions[0] != want[0] || mountOptions[1] != want[1] {
		t.Fatalf("mount options = %#v, want %#v", mountOptions, want)
	}
}

func TestManagerDeleteRecordingsRejectsUntrustedPathsBeforeMaintenance(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	imagePath := filepath.Join(root, "image.img")
	if err := os.WriteFile(imagePath, []byte("image"), 0o600); err != nil {
		t.Fatal(err)
	}
	maintenance := &fakeMaintenance{}
	manager, err := New(Config{
		ImagePath: imagePath, StatePath: filepath.Join(root, "state"), StagingPath: filepath.Join(root, "state", "media-staging"), MountPath: filepath.Join(root, "mount"),
	}, unusedRunner{}, maintenance)
	if err != nil {
		t.Fatal(err)
	}
	for _, unsafe := range []string{
		"../etc/passwd",
		"TeslaCam/SavedClips/event/../../secret.mp4",
		"TeslaCam/RecentClips/not-a-video.txt",
		"TeslaTrackMode/2026-07-26_03-30-00-front.mp4",
	} {
		if _, err := manager.DeleteRecordings(t.Context(), RecordingDeleteRequest{Paths: []string{unsafe}}); err == nil {
			t.Fatalf("unsafe recording path accepted: %q", unsafe)
		}
	}
	if maintenance.calls != 0 {
		t.Fatalf("unsafe requests entered maintenance %d times", maintenance.calls)
	}
}

func TestManagerDeleteRecentRecordingPreservesSharedDirectoryMetadata(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	imagePath := filepath.Join(root, "image.img")
	if err := os.WriteFile(imagePath, []byte("image"), 0o600); err != nil {
		t.Fatal(err)
	}
	mountPath := filepath.Join(root, "mount")
	manager, err := New(Config{
		ImagePath: imagePath, StatePath: filepath.Join(root, "state"), StagingPath: filepath.Join(root, "state", "media-staging"), MountPath: mountPath,
	}, recordingDeleteRunner{}, &fakeMaintenance{})
	if err != nil {
		t.Fatal(err)
	}
	result, err := manager.DeleteRecordings(t.Context(), RecordingDeleteRequest{Paths: []string{
		"TeslaCam/RecentClips/2026-07-26_03-32-00-front.mp4",
	}})
	if err != nil {
		t.Fatal(err)
	}
	if result.DeletedFiles != 1 || result.DeletedBytes != 6 || result.DeletedEvents != 0 {
		t.Fatalf("DeleteRecordings() = %#v", result)
	}
	recentPath := filepath.Join(mountPath, "p1", "TeslaCam", "RecentClips")
	if _, err := os.Stat(filepath.Join(recentPath, "2026-07-26_03-32-00-front.mp4")); !os.IsNotExist(err) {
		t.Fatalf("recent recording exists after deletion: %v", err)
	}
	if content, err := os.ReadFile(filepath.Join(recentPath, "thumb.png")); err != nil || string(content) != "recent-thumb" {
		t.Fatalf("shared recent metadata changed: %q, %v", content, err)
	}
}

func TestManagerDeleteSentryRecordingRemovesWholeEventDirectory(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	imagePath := filepath.Join(root, "image.img")
	if err := os.WriteFile(imagePath, []byte("image"), 0o600); err != nil {
		t.Fatal(err)
	}
	mountPath := filepath.Join(root, "mount")
	manager, err := New(Config{
		ImagePath: imagePath, StatePath: filepath.Join(root, "state"), StagingPath: filepath.Join(root, "state", "media-staging"), MountPath: mountPath,
	}, recordingDeleteRunner{}, &fakeMaintenance{})
	if err != nil {
		t.Fatal(err)
	}
	result, err := manager.DeleteRecordings(t.Context(), RecordingDeleteRequest{Paths: []string{
		"TeslaCam/SentryClips/2026-07-26_03-33-00/2026-07-26_03-33-00-front.mp4",
	}})
	if err != nil {
		t.Fatal(err)
	}
	if result.DeletedFiles != 2 || result.DeletedBytes != 8 || result.DeletedEvents != 1 {
		t.Fatalf("DeleteRecordings() = %#v", result)
	}
	eventPath := filepath.Join(mountPath, "p1", "TeslaCam", "SentryClips", "2026-07-26_03-33-00")
	if _, err := os.Stat(eventPath); !os.IsNotExist(err) {
		t.Fatalf("sentry event directory exists after deletion: %v", err)
	}
}

func TestSafeMusicRelativePathRejectsTraversalAndUnsupportedFiles(t *testing.T) {
	t.Parallel()
	for _, name := range []string{"../song.mp3", "album/../song.mp3", "album/song.txt", "album\\song.mp3", ".hidden/song.mp3"} {
		if safeMusicRelativePath(name) {
			t.Fatalf("unsafe music path accepted: %q", name)
		}
	}
	if !safeMusicRelativePath("精选/album-song.MP3") {
		t.Fatal("valid nested music path was rejected")
	}
}

func validPNG(t *testing.T, width, height int) io.Reader {
	t.Helper()
	var output bytes.Buffer
	canvas := image.NewRGBA(image.Rect(0, 0, width, height))
	canvas.Set(0, 0, color.White)
	if err := png.Encode(&output, canvas); err != nil {
		t.Fatal(err)
	}
	return bytes.NewReader(output.Bytes())
}
