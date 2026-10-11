package nasupload

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/wen-ryon/teslabox/internal/recordingcache"
)

type fakeTransport struct {
	mu      sync.Mutex
	fail    int
	block   bool
	entered chan string
	targets []Target
}

func (f *fakeTransport) Obscure(_ context.Context, s string) (string, error) {
	return "obscured-" + s, nil
}
func (f *fakeTransport) Test(context.Context, Target) error { return nil }
func (f *fakeTransport) Upload(ctx context.Context, target Target, local, remote, hash, id string) error {
	got, _, err := fileHash(ctx, local)
	if err != nil {
		return err
	}
	if got != hash {
		return errors.New("wrong upload digest")
	}
	f.mu.Lock()
	f.targets = append(f.targets, target)
	fail := f.fail > 0
	if fail {
		f.fail--
	}
	block := f.block
	f.mu.Unlock()
	if f.entered != nil {
		select {
		case f.entered <- local:
		default:
		}
	}
	if block {
		<-ctx.Done()
		return ctx.Err()
	}
	if fail {
		return errors.New("test connection interrupted")
	}
	return nil
}

func fixture(t *testing.T, f *fakeTransport) (*Manager, *recordingcache.Store, string) {
	t.Helper()
	root := t.TempDir()
	cache, err := recordingcache.New(recordingcache.Config{Root: filepath.Join(root, "media", "recordings"), MediaRoot: filepath.Join(root, "media"), MaxBytes: 32 << 20, MaxTotalBytes: 64 << 20, MinFreeBytes: 1, IdleTTL: 0})
	if err != nil {
		t.Fatal(err)
	}
	m, err := New(Config{Root: filepath.Join(root, "media", "uploads"), Cache: cache, Transport: f})
	if err != nil {
		t.Fatal(err)
	}
	source := filepath.Join(root, "source")
	if err = os.MkdirAll(source, 0o700); err != nil {
		t.Fatal(err)
	}
	return m, cache, source
}
func addTarget(t *testing.T, m *Manager) Target {
	t.Helper()
	target, err := m.SaveTarget(context.Background(), Target{Name: "test NAS", Protocol: "smb", Host: "localhost", User: "test", Password: "private-password", Path: "share/test", Quality: Quality{Mode: "original"}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = m.SetDefaultTarget(target.ID); err != nil {
		t.Fatal(err)
	}
	return target
}
func inputs(t *testing.T, root string) []Input {
	t.Helper()
	out := []Input{}
	for _, camera := range []string{"front", "back"} {
		p := filepath.Join(root, camera+".mp4")
		if err := os.WriteFile(p, []byte("sample "+camera), 0o600); err != nil {
			t.Fatal(err)
		}
		out = append(out, Input{MomentID: "minute-1", Camera: camera, Name: camera + ".mp4", Path: p, RecordedAt: time.Date(2026, 10, 2, 12, 0, 0, 0, time.UTC), Size: uint64(len("sample " + camera))})
	}
	return out
}
func runManager(t *testing.T, m *Manager) context.CancelFunc {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	go m.Run(ctx)
	t.Cleanup(func() {
		cancel()
		wait, stop := context.WithTimeout(context.Background(), 3*time.Second)
		defer stop()
		if err := m.Wait(wait); err != nil {
			t.Error(err)
		}
	})
	return cancel
}
func waitState(t *testing.T, m *Manager, id, state string) Task {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		for _, task := range m.History() {
			if task.ID == id && task.State == state {
				return task
			}
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("state %s not reached: %+v", state, m.History())
	return Task{}
}

func TestManualUploadStagesReusesAndCountsMinutes(t *testing.T) {
	f := &fakeTransport{}
	m, cache, root := fixture(t, f)
	target := addTarget(t, m)
	files := inputs(t, root)
	var releases atomic.Int32
	runManager(t, m)
	for i := 0; i < 2; i++ {
		task, err := m.Create(Request{TargetID: target.ID, SessionID: "session", Cameras: []string{"front", "back"}}, "disk-a", root, "snapshot", files, nil, func() { releases.Add(1) })
		if err != nil {
			t.Fatal(err)
		}
		done := waitState(t, m, task.ID, "completed")
		if len(done.Files) != 2 {
			t.Fatalf("files: %+v", done)
		}
	}
	if releases.Load() != 2 {
		t.Fatalf("snapshot leases: %d", releases.Load())
	}
	if m.Counts("disk-a")["minute-1"] != 1 || len(m.Counts("disk-b")) != 0 {
		t.Fatal("count must deduplicate views and exclude another disk")
	}
	if _, err := os.Stat(files[0].Path); err != nil {
		t.Fatal("source was deleted")
	}
	f.mu.Lock()
	for _, target := range f.targets {
		if target.Password != "" || target.Credential == "" {
			t.Fatal("transport credential invalid")
		}
	}
	f.mu.Unlock()
	if err := cache.Cleanup(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestNetworkFailureDoesNotCountAndRetryUsesSystemDisk(t *testing.T) {
	f := &fakeTransport{fail: 1}
	m, cache, root := fixture(t, f)
	target := addTarget(t, m)
	files := inputs(t, root)
	runManager(t, m)
	task, err := m.Create(Request{TargetID: target.ID}, "disk-a", root, "s1", files, nil, func() {})
	if err != nil {
		t.Fatal(err)
	}
	waitState(t, m, task.ID, "waiting_retry")
	if len(m.Counts("disk-a")) != 0 {
		t.Fatal("failure counted as success")
	}
	if err = cache.Cleanup(context.Background()); err != nil {
		t.Fatal(err)
	}
	for _, in := range files {
		if err = os.Remove(in.Path); err != nil {
			t.Fatal(err)
		}
	}
	if _, err = m.Retry(task.ID); err != nil {
		t.Fatal(err)
	}
	if done := waitState(t, m, task.ID, "completed"); len(done.Files) != 2 {
		t.Fatal(done)
	}
}

func TestTargetEditCannotReroutePendingUploadOrExposePassword(t *testing.T) {
	f := &fakeTransport{}
	m, _, root := fixture(t, f)
	target := addTarget(t, m)
	files := inputs(t, root)
	task, err := m.Create(Request{TargetID: target.ID}, "disk", root, "s", files, nil, func() {})
	if err != nil {
		t.Fatal(err)
	}
	target.Path = "another-share"
	target.Host = "another-host"
	edited, err := m.SaveTarget(context.Background(), target)
	if err != nil {
		t.Fatal(err)
	}
	if target.Revision == edited.Revision {
		t.Fatal("revision not updated")
	}
	public, _ := json.Marshal(m.Targets())
	if strings.Contains(string(public), "private-password") || strings.Contains(string(public), "credential") {
		t.Fatal("credential leaked")
	}
	runManager(t, m)
	waitState(t, m, task.ID, "completed")
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, seen := range f.targets {
		if seen.Host != "localhost" || seen.Path != "share/test" {
			t.Fatal("pending task rerouted")
		}
	}
}

func TestRestartPinsBeforeCleanupAndCancellationStopsWorker(t *testing.T) {
	f := &fakeTransport{block: true, entered: make(chan string, 1)}
	m, _, root := fixture(t, f)
	target := addTarget(t, m)
	files := inputs(t, root)
	cancel := runManager(t, m)
	task, err := m.Create(Request{TargetID: target.ID}, "disk", root, "s", files, nil, func() {})
	if err != nil {
		t.Fatal(err)
	}
	var local string
	select {
	case local = <-f.entered:
	case <-time.After(3 * time.Second):
		t.Fatal("not uploading")
	}
	cancel()
	ctx, stop := context.WithTimeout(context.Background(), 3*time.Second)
	defer stop()
	if err = m.Wait(ctx); err != nil {
		t.Fatal(err)
	}
	// 模拟进程丢失后重新构造缓存，不继承旧进程的内存固定。
	newCache, err := recordingcache.New(recordingcache.Config{Root: filepath.Join(filepath.Dir(m.cfg.Root), "recordings"), MediaRoot: filepath.Dir(m.cfg.Root), MaxBytes: 32 << 20, MaxTotalBytes: 64 << 20, MinFreeBytes: 1, IdleTTL: 0})
	if err != nil {
		t.Fatal(err)
	}
	recovered, err := New(Config{Root: m.cfg.Root, Cache: newCache, Transport: f})
	if err != nil {
		t.Fatal(err)
	}
	if err = newCache.Cleanup(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err = os.Stat(local); err != nil {
		t.Fatal("recovered input was cleaned")
	}
	runManager(t, recovered)
	waitState(t, recovered, task.ID, "uploading")
	if _, err = recovered.Cancel(task.ID); err != nil {
		t.Fatal(err)
	}
	waitState(t, recovered, task.ID, "canceled")
}

func TestRejectedCreateReleasesSnapshotAndInvalidConfigDoesNotPersist(t *testing.T) {
	m, _, root := fixture(t, &fakeTransport{})
	var releases atomic.Int32
	if _, err := m.Create(Request{TargetID: "missing"}, "disk", root, "s", inputs(t, root), nil, func() { releases.Add(1) }); err == nil {
		t.Fatal("missing target accepted")
	}
	if releases.Load() != 1 {
		t.Fatal("lease leaked")
	}
	for _, q := range []Quality{{Mode: "compressed", Height: 720, FPS: 0, BitrateKbps: 3000, Layout: "separate"}, {Mode: "compressed", Height: 1000, FPS: 24, BitrateKbps: 3000, Layout: "composite6"}} {
		if validateQuality(q) == nil {
			t.Fatal("invalid quality accepted")
		}
	}
	for _, target := range []Target{{Name: "bad", Protocol: "webdav", URL: "http://host/dav", User: "x", Quality: Quality{Mode: "original"}}, {Name: "bad", Protocol: "sftp", Host: "host", User: "x", HostKey: "SHA256:unverified", Quality: Quality{Mode: "original"}}} {
		if validateTarget(target) == nil {
			t.Fatal("unsafe target accepted")
		}
	}
}
