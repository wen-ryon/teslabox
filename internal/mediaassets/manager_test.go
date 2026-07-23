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

func (s *fakeSnapshotSource) Scan(context.Context) error {
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
