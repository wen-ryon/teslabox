package nasupload

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/wen-ryon/teslabox/internal/recordingcache"
)

func discoverAuto(t *testing.T, m *Manager, directory string, now time.Time) AutoEvent {
	t.Helper()
	if _, err := m.SaveAutomatic(AutoConfig{Enabled: true, IncludeExisting: true, Scope: "event"}); err != nil {
		t.Fatal(err)
	}
	events, err := m.ObserveAutomatic("disk", []string{directory}, []AutoEvent{{Directory: directory, Camera: "front", OccurredAt: now, Fingerprint: "files", LastVersion: "s1"}}, now)
	if err != nil || len(events) != 1 {
		t.Fatalf("events=%+v err=%v", events, err)
	}
	return events[0]
}

func TestPerOutputEvidenceSurvivesRestartAndRejectsSameSnapshot(t *testing.T) {
	m, cache, _ := fixture(t, &fakeTransport{})
	addTarget(t, m)
	now := time.Now()
	event := discoverAuto(t, m, "sentry/event", now)
	observe := func(manager *Manager, key, fingerprint, version string, at time.Time, want bool) {
		t.Helper()
		ready, err := manager.ObserveAutomaticInput(event.ID, key, fingerprint, version, at)
		if err != nil || ready != want {
			t.Fatalf("key=%s ready=%v err=%v", key, ready, err)
		}
	}
	observe(m, "event|minute", "main", "a", now, false)
	observe(m, "event|minute", "main", "a", now.Add(20*time.Second), false)
	observe(m, "supplement|minute", "extra-a", "a", now, false)
	if err := m.SetAutomaticResult(event.ID, "waiting_files", "等待稳定", nil); err != nil {
		t.Fatal(err)
	}
	recovered, err := New(Config{Root: m.cfg.Root, Cache: cache, Transport: &fakeTransport{}})
	if err != nil {
		t.Fatal(err)
	}
	observe(recovered, "event|minute", "main", "b", now.Add(30*time.Second), true)
	observe(recovered, "supplement|minute", "extra-b", "b", now.Add(30*time.Second), false)
	observe(recovered, "event|minute", "main", "c", now.Add(2*time.Minute), false)
	if err := recovered.CancelAutomatic(event.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := recovered.ObserveAutomaticInput(event.ID, "event|minute", "main", "d", now.Add(3*time.Minute)); err == nil {
		t.Fatal("canceled event observed")
	}
}

func TestAutoBaselineIndependentSnapshotsAndFixedPolicy(t *testing.T) {
	m, _, _ := fixture(t, &fakeTransport{})
	if m.Automatic().Enabled {
		t.Fatal("old installation enabled automatically")
	}
	if _, err := m.SaveAutomatic(AutoConfig{Enabled: true, Scope: "event"}); err == nil {
		t.Fatal("no default accepted")
	}
	target := addTarget(t, m)
	config := AutoConfig{Enabled: true, Scope: "surrounding"}
	if _, err := m.SaveAutomatic(config); err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	old := AutoEvent{Directory: "sentry/old", OccurredAt: now, Camera: "front", Fingerprint: "old", LastVersion: "a"}
	if events, err := m.ObserveAutomatic("disk", []string{old.Directory}, []AutoEvent{old}, now); err != nil || len(events) != 0 {
		t.Fatalf("historical=%+v %v", events, err)
	}
	newEvent := old
	newEvent.Directory = "saved/braking"
	newEvent.Fingerprint = "new"
	observe := func(version string, at time.Time) AutoEvent {
		newEvent.LastVersion = version
		events, err := m.ObserveAutomatic("disk", []string{old.Directory, newEvent.Directory}, []AutoEvent{newEvent}, at)
		if err != nil || len(events) != 1 {
			t.Fatalf("events=%+v %v", events, err)
		}
		return events[0]
	}
	e := observe("a", now)
	if e.Observations != 1 {
		t.Fatal(e)
	}
	if e = observe("a", now.Add(20*time.Second)); e.Observations != 1 {
		t.Fatal("same snapshot treated as independent")
	}
	target.Host = "changed"
	target.Path = "changed"
	if _, err := m.SaveTarget(context.Background(), target); err != nil {
		t.Fatal(err)
	}
	config.Scope = "directory"
	if _, err := m.SaveAutomatic(config); err != nil {
		t.Fatal(err)
	}
	e = observe("b", now.Add(40*time.Second))
	if e.Observations != 2 || e.Target.Host != "localhost" || e.Policy.Scope != "surrounding" {
		t.Fatal(e)
	}
	newEvent.OccurredAt = now.Add(time.Second)
	e = observe("c", now.Add(45*time.Second))
	if e.ID == "" || len(m.AutomaticEvents()) != 2 {
		t.Fatal("corrected metadata duplicated directory")
	}
	if e = observe("d", now.Add(2*time.Minute)); e.Observations != 1 {
		t.Fatal("stale observations survived restart gap")
	}
	public, _ := json.Marshal(m.AutomaticEvents())
	if strings.Contains(string(public), "obscured-") || strings.Contains(string(public), "private-password") {
		t.Fatal("event credentials leaked")
	}
	config.IncludeExisting = true
	if _, err := m.SaveAutomatic(config); err != nil {
		t.Fatal(err)
	}
	if events, err := m.ObserveAutomatic("disk", []string{old.Directory}, []AutoEvent{old}, now.Add(3*time.Minute)); err != nil || len(events) != 2 {
		t.Fatalf("explicit historical inclusion: %+v %v", events, err)
	}
}

func TestAutoInterruptedPreparationReusesIDAndCancellationPersists(t *testing.T) {
	m, cache, root := fixture(t, &fakeTransport{})
	addTarget(t, m)
	e := discoverAuto(t, m, "sentry/event", time.Now())
	files := inputs(t, root)
	req := Request{AutoEventID: e.ID, AutoKey: e.ID + "|minute|event", Role: "event"}
	var releases atomic.Int32
	task, err := m.Create(req, "disk", root, "a", files, nil, func() { releases.Add(1) })
	if err != nil {
		t.Fatal(err)
	}
	// 不运行暂存工作进程，模拟计划已持久化但复制尚未开始时进程消失。
	recovered, err := New(Config{Root: m.cfg.Root, Cache: cache, Transport: &fakeTransport{}})
	if err != nil {
		t.Fatal(err)
	}
	duplicate, err := recovered.Create(req, "disk", root, "b", files, nil, func() { releases.Add(1) })
	if err != nil || duplicate.ID != task.ID || len(recovered.History()) != 1 {
		t.Fatalf("recovery=%+v %v", duplicate, err)
	}
	runManager(t, recovered)
	waitState(t, recovered, task.ID, "completed")
	if _, err = recovered.Create(req, "disk", root, "c", files, nil, func() { releases.Add(1) }); err != nil {
		t.Fatal(err)
	}
	if releases.Load() != 2 {
		t.Fatal("duplicate lease not released")
	}
	if err = recovered.CancelAutomatic(e.ID); err != nil {
		t.Fatal(err)
	}
	if _, err = recovered.Create(req, "disk", root, "d", files, nil, func() { releases.Add(1) }); err == nil {
		t.Fatal("canceled event accepted duplicate")
	}
	if recovered.AutomaticEvents()[0].State != "canceled" {
		t.Fatal("cancel not durable")
	}
	// 原进程的快照租约在真实断电时随进程消失，测试主动释放其尚未消费的队列。
	p := <-m.prepare
	p.release()
}

type countProcessor struct{ calls atomic.Int32 }

func (p *countProcessor) Process(_ context.Context, _ Quality, _ []Input, path string) error {
	p.calls.Add(1)
	return os.WriteFile(path, []byte("finished encoded video"), 0o600)
}

func TestAutoRestartRetainsCompressedOutputWithoutSource(t *testing.T) {
	f := &fakeTransport{fail: 1}
	m, _, root := fixture(t, f)
	addTarget(t, m)
	e := discoverAuto(t, m, "sentry/event", time.Now())
	processor := &countProcessor{}
	m.cfg.Processor = processor
	q := Quality{Mode: "compressed", Height: 240, FPS: 24, BitrateKbps: 500, Layout: "separate"}
	files := inputs(t, root)[:1]
	cancel := runManager(t, m)
	task, err := m.Create(Request{AutoEventID: e.ID, AutoKey: e.ID + "|minute", Quality: &q}, "disk", root, "a", files, nil, func() {})
	if err != nil {
		t.Fatal(err)
	}
	waitState(t, m, task.ID, "waiting_retry")
	cancel()
	ctx, stop := context.WithTimeout(context.Background(), 3*time.Second)
	defer stop()
	if err = m.Wait(ctx); err != nil {
		t.Fatal(err)
	}
	if err = os.Remove(files[0].Path); err != nil {
		t.Fatal(err)
	}
	newCache, err := recordingcache.New(recordingcache.Config{Root: filepath.Join(filepath.Dir(m.cfg.Root), "recordings"), MediaRoot: filepath.Dir(m.cfg.Root), MaxBytes: 32 << 20, MaxTotalBytes: 64 << 20, MinFreeBytes: 1, IdleTTL: 0})
	if err != nil {
		t.Fatal(err)
	}
	recovered, err := New(Config{Root: m.cfg.Root, Cache: newCache, Transport: f, Processor: processor})
	if err != nil {
		t.Fatal(err)
	}
	if err = newCache.Cleanup(context.Background()); err != nil {
		t.Fatal(err)
	}
	runManager(t, recovered)
	waitState(t, recovered, task.ID, "completed")
	if processor.calls.Load() != 1 {
		t.Fatal("completed compressed output was re-encoded")
	}
}

func TestAutoTransientFailurePastFiveAttemptsAndPermanentStop(t *testing.T) {
	f := &fakeTransport{fail: 20}
	m, _, root := fixture(t, f)
	addTarget(t, m)
	e := discoverAuto(t, m, "sentry/event", time.Now())
	task, err := m.Create(Request{AutoEventID: e.ID, AutoKey: e.ID + "|minute"}, "disk", root, "s", inputs(t, root), nil, func() {})
	if err != nil {
		t.Fatal(err)
	}
	m.stage(context.Background(), <-m.prepare)
	for range 6 {
		m.transfer(context.Background(), task.ID)
	}
	done := waitState(t, m, task.ID, "waiting_retry")
	if done.Attempts != 6 || time.Until(done.NextRetryAt) > 15*time.Minute {
		t.Fatal(done)
	}
	for _, message := range []string{"authentication failed", "permission denied", "key mismatch", "certificate verify failed", "no space left"} {
		if !permanentUploadError(classifyCommandError(errors.New("command failed"), message)) {
			t.Fatal(message)
		}
	}
	if permanentUploadError(classifyCommandError(errors.New("timeout"), "i/o timeout")) {
		t.Fatal("network marked permanent")
	}
}

func TestManagerStoppedRejectsPreparationAndReleasesLease(t *testing.T) {
	m, _, root := fixture(t, &fakeTransport{})
	addTarget(t, m)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	m.Run(ctx)
	var released atomic.Int32
	if _, err := m.Create(Request{}, "disk", root, "a", inputs(t, root), nil, func() { released.Add(1) }); err == nil || released.Load() != 1 {
		t.Fatal("shutdown accepted orphan preparation")
	}
}

type secondFileFailure struct {
	fakeTransport
	calls map[string]int
}

func (f *secondFileFailure) Upload(ctx context.Context, target Target, local, remote, hash, id string) error {
	b, err := os.ReadFile(local)
	if err != nil {
		return err
	}
	name := string(b)
	f.calls[name]++
	if name == "sample back" && f.calls[name] == 1 {
		return errors.New("connection interrupted on second file")
	}
	return f.fakeTransport.Upload(ctx, target, local, remote, hash, id)
}

func TestAutoPartialSuccessSurvivesRestartAndSkipsVerifiedFile(t *testing.T) {
	m, _, root := fixture(t, &fakeTransport{})
	addTarget(t, m)
	f := &secondFileFailure{calls: map[string]int{}}
	m.cfg.Transport = f
	e := discoverAuto(t, m, "sentry/event", time.Now())
	files := inputs(t, root)
	task, err := m.Create(Request{AutoEventID: e.ID, AutoKey: e.ID + "|minute"}, "disk", root, "a", files, nil, func() {})
	if err != nil {
		t.Fatal(err)
	}
	m.stage(context.Background(), <-m.prepare)
	m.transfer(context.Background(), task.ID)
	partial := waitState(t, m, task.ID, "waiting_retry")
	if len(partial.Files) != 1 || partial.Files[0].Cameras[0] != "front" {
		t.Fatal(partial)
	}
	for _, file := range files {
		if err = os.Remove(file.Path); err != nil {
			t.Fatal(err)
		}
	}
	cache, err := recordingcache.New(recordingcache.Config{Root: filepath.Join(filepath.Dir(m.cfg.Root), "recordings"), MediaRoot: filepath.Dir(m.cfg.Root), MaxBytes: 32 << 20, MaxTotalBytes: 64 << 20, MinFreeBytes: 1, IdleTTL: 0})
	if err != nil {
		t.Fatal(err)
	}
	recovered, err := New(Config{Root: m.cfg.Root, Cache: cache, Transport: f})
	if err != nil {
		t.Fatal(err)
	}
	if err = cache.Cleanup(context.Background()); err != nil {
		t.Fatal(err)
	}
	recovered.transfer(context.Background(), task.ID)
	if done := waitState(t, recovered, task.ID, "completed"); len(done.Files) != 2 || f.calls["sample front"] != 1 || f.calls["sample back"] != 2 {
		t.Fatalf("re-uploaded completed file: %+v %+v", done, f.calls)
	}
}
