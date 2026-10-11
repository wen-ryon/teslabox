package recordingcache

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

func testConfig(t *testing.T) Config {
	t.Helper()
	media := filepath.Join(t.TempDir(), "media")
	return Config{Root: filepath.Join(media, "recordings"), MediaRoot: media, MaxBytes: 4 << 20, MaxTotalBytes: 16 << 20, MinFreeBytes: 1, IdleTTL: 24 * time.Hour}
}

func newTestStore(t *testing.T, cfg Config) *Store {
	t.Helper()
	s, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func sourceFile(t *testing.T, content []byte) File {
	t.Helper()
	path := filepath.Join(t.TempDir(), "source.mp4")
	if err := os.WriteFile(path, content, 0o600); err != nil {
		t.Fatal(err)
	}
	return File{Identity: path, Path: path, Version: "snapshot-1"}
}

func ensure(t *testing.T, s *Store, files ...File) *Lease {
	t.Helper()
	lease, err := s.EnsureLocal(context.Background(), files, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(lease.Release)
	return lease
}

func requireBytes(t *testing.T, path string, want []byte) {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(data, want) {
		t.Fatalf("content at %s differs", path)
	}
}

func TestEnsureLocalOnDemandReuseAndExpand(t *testing.T) {
	t.Parallel()
	s := newTestStore(t, testConfig(t))
	front := sourceFile(t, []byte("front clip"))
	rear := sourceFile(t, []byte("rear clip"))
	first := ensure(t, s, front)
	firstLocal := first.Paths[front.Path]
	entries, err := os.ReadDir(s.objectDir())
	if err != nil || len(entries) != 1 {
		t.Fatalf("only the selected source should be copied: entries=%v err=%v", entries, err)
	}
	before, err := os.Stat(firstLocal)
	if err != nil {
		t.Fatal(err)
	}
	first.Release()
	front.Version = "snapshot-2"
	expanded := ensure(t, s, front, rear, front)
	if expanded.Paths[front.Path] != firstLocal || len(expanded.Paths) != 2 {
		t.Fatalf("existing front should be reused and rear added: %v", expanded.Paths)
	}
	after, err := os.Stat(firstLocal)
	if err != nil || !before.ModTime().Equal(after.ModTime()) || !os.SameFile(before, after) {
		t.Fatalf("cross-version reuse wrote a second copy: %v", err)
	}
	requireBytes(t, expanded.Paths[front.Path], []byte("front clip"))
	requireBytes(t, expanded.Paths[rear.Path], []byte("rear clip"))
}

func TestEnsureLocalNewVersionChecksContentDespiteSameMetadata(t *testing.T) {
	t.Parallel()
	s := newTestStore(t, testConfig(t))
	source := sourceFile(t, []byte("AAAA"))
	first := ensure(t, s, source)
	oldLocal := first.Paths[source.Path]
	first.Release()
	info, err := os.Stat(source.Path)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(source.Path, []byte("BBBB"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(source.Path, info.ModTime(), info.ModTime()); err != nil {
		t.Fatal(err)
	}
	source.Version = "snapshot-2"
	second := ensure(t, s, source)
	if second.Paths[source.Path] == oldLocal {
		t.Fatal("changed content reused the old cache despite a different immutable version")
	}
	requireBytes(t, second.Paths[source.Path], []byte("BBBB"))
	requireBytes(t, oldLocal, []byte("AAAA"))
}

func TestEnsureLocalCancelledCopyLeavesNoPartialReady(t *testing.T) {
	t.Parallel()
	s := newTestStore(t, testConfig(t))
	source := sourceFile(t, bytes.Repeat([]byte{0x47}, 2<<20))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	lease, err := s.EnsureLocal(ctx, []File{source}, func(done, _ uint64) {
		if done > 0 {
			cancel()
		}
	})
	if !errors.Is(err, context.Canceled) || lease != nil {
		t.Fatalf("cancelled copy: lease=%v err=%v", lease, err)
	}
	entries, err := os.ReadDir(s.objectDir())
	if err != nil || len(entries) != 0 {
		t.Fatalf("partial files survived cancellation: entries=%v err=%v", entries, err)
	}
	retry := ensure(t, s, source)
	requireBytes(t, retry.Paths[source.Path], bytes.Repeat([]byte{0x47}, 2<<20))
}

func TestEnsureLocalWaitingCanCancelAndConcurrentRequestsDeduplicate(t *testing.T) {
	t.Parallel()
	s := newTestStore(t, testConfig(t))
	source := sourceFile(t, []byte("same content"))
	entered := make(chan struct{})
	resume := make(chan struct{})
	finished := make(chan error, 1)
	go func() {
		var once sync.Once
		lease, err := s.EnsureLocal(context.Background(), []File{source}, func(_, _ uint64) {
			once.Do(func() { close(entered); <-resume })
		})
		if err == nil {
			lease.Release()
		}
		finished <- err
	}()
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("preparation did not start")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := s.EnsureLocal(ctx, []File{source}, nil)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled waiter: %v", err)
	}
	close(resume)
	if err := <-finished; err != nil {
		t.Fatal(err)
	}
	var group sync.WaitGroup
	errorsCh := make(chan error, 12)
	for range 12 {
		group.Go(func() {
			lease, err := s.EnsureLocal(context.Background(), []File{source}, nil)
			if err == nil {
				lease.Release()
			}
			errorsCh <- err
		})
	}
	group.Wait()
	close(errorsCh)
	for err := range errorsCh {
		if err != nil {
			t.Fatal(err)
		}
	}
	entries, err := os.ReadDir(s.objectDir())
	if err != nil || len(entries) != 1 {
		t.Fatalf("parallel requests duplicated source writes: entries=%v err=%v", entries, err)
	}
}

func TestRestoreIntegrityAndInterruptedAtomicFiles(t *testing.T) {
	t.Parallel()
	cfg := testConfig(t)
	s := newTestStore(t, cfg)
	source := sourceFile(t, []byte("good"))
	lease := ensure(t, s, source)
	local := lease.Paths[source.Path]
	lease.Release()
	if err := os.WriteFile(local, []byte("evil"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{filepath.Join(s.objectDir(), ".copy-crash.part"), filepath.Join(cfg.Root, ".manifest-crash.part")} {
		if err := os.WriteFile(path, []byte("partial"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	restarted := newTestStore(t, cfg)
	if _, err := restarted.AcquireLocal(context.Background(), []string{local}); err == nil {
		t.Fatal("corrupt cache was accepted after restart")
	}
	for _, path := range []string{filepath.Join(s.objectDir(), ".copy-crash.part"), filepath.Join(cfg.Root, ".manifest-crash.part")} {
		if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("damaged or interrupted data remains: %s err=%v", path, err)
		}
	}
	requireBytes(t, local, []byte("evil"))
	repaired := ensure(t, restarted, source)
	requireBytes(t, repaired.Paths[source.Path], []byte("good"))
}

func TestCleanupWithoutChangesDoesNotRewriteManifest(t *testing.T) {
	t.Parallel()
	s := newTestStore(t, testConfig(t))
	source := sourceFile(t, []byte("cached"))
	lease := ensure(t, s, source)
	lease.Release()
	path := filepath.Join(s.cfg.Root, "manifest.json")
	stamp := time.Unix(1700000000, 0)
	if err := os.Chtimes(path, stamp, stamp); err != nil {
		t.Fatal(err)
	}
	if err := s.Cleanup(context.Background()); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil || !info.ModTime().Equal(stamp) {
		t.Fatalf("unchanged cleanup rewrote manifest: info=%v err=%v", info, err)
	}
}

func TestAcquireLocalPinsBeforeCleanupAndRejectsArbitraryPath(t *testing.T) {
	t.Parallel()
	cfg := testConfig(t)
	cfg.IdleTTL = 0
	s := newTestStore(t, cfg)
	source := sourceFile(t, []byte("preserved"))
	lease := ensure(t, s, source)
	local := lease.Paths[source.Path]
	lease.Release()
	restarted := newTestStore(t, cfg)
	recovered, err := restarted.AcquireLocal(context.Background(), []string{local, local})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(recovered.Release)
	if _, err := restarted.AcquireLocal(context.Background(), []string{local, source.Path}); err == nil {
		t.Fatal("source path outside managed cache was accepted")
	}
	if err := restarted.Cleanup(context.Background()); err != nil {
		t.Fatal(err)
	}
	requireBytes(t, local, []byte("preserved"))
	recovered.Release()
	recovered.Release()
	if err := restarted.Cleanup(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(local); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("zero TTL should remove only after all pins released: %v", err)
	}
}

func TestPinLocalForRecoveryDefersHashAndProtectsFromCleanup(t *testing.T) {
	t.Parallel()
	cfg := testConfig(t)
	cfg.IdleTTL = 0
	s := newTestStore(t, cfg)
	source := sourceFile(t, []byte("good"))
	lease := ensure(t, s, source)
	local := lease.Paths[source.Path]
	lease.Release()
	if err := os.WriteFile(local, []byte("evil"), 0o600); err != nil {
		t.Fatal(err)
	}
	restarted := newTestStore(t, cfg)
	pin, err := restarted.PinLocalForRecovery(context.Background(), []string{local, local})
	if err != nil {
		t.Fatalf("recovery pin unexpectedly hashed same-length content: %v", err)
	}
	t.Cleanup(pin.Release)
	if err := restarted.Cleanup(context.Background()); err != nil {
		t.Fatal(err)
	}
	requireBytes(t, local, []byte("evil"))
	if _, err := restarted.AcquireLocal(context.Background(), []string{local}); err == nil {
		t.Fatal("background integrity verification accepted damaged content")
	}
	pin.Release()
	if err := restarted.Cleanup(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(local); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("failed verified acquire leaked a pin: %v", err)
	}
}

type cancellingPinContext struct {
	context.Context
	store  *Store
	cancel context.CancelFunc
}

func (c cancellingPinContext) Err() error {
	for _, obj := range c.store.state.Objects {
		if obj.Pins > 0 {
			c.cancel()
			break
		}
	}
	return c.Context.Err()
}

func TestPinLocalForRecoveryRollsBackInvalidAndCancelledInputs(t *testing.T) {
	for _, scenario := range []string{"invalid_path", "cancel_after_first_pin", "length_changed"} {
		t.Run(scenario, func(t *testing.T) {
			t.Parallel()
			cfg := testConfig(t)
			cfg.IdleTTL = 0
			s := newTestStore(t, cfg)
			source := sourceFile(t, []byte("source"))
			first := ensure(t, s, source)
			local := first.Paths[source.Path]
			first.Release()
			ctx := context.Background()
			paths := []string{local}
			switch scenario {
			case "invalid_path":
				paths = append(paths, source.Path)
			case "cancel_after_first_pin":
				parent, cancel := context.WithCancel(ctx)
				defer cancel()
				ctx = cancellingPinContext{Context: parent, store: s, cancel: cancel}
				paths = append(paths, local)
			case "length_changed":
				if err := os.WriteFile(local, []byte("x"), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			pin, err := s.PinLocalForRecovery(ctx, paths)
			if err == nil || pin != nil {
				t.Fatalf("invalid recovery pin accepted: lease=%v err=%v", pin, err)
			}
			if scenario == "cancel_after_first_pin" && !errors.Is(err, context.Canceled) {
				t.Fatalf("context cancellation not preserved: %v", err)
			}
			if err := s.Cleanup(context.Background()); err != nil {
				t.Fatal(err)
			}
			if _, err := os.Stat(local); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("failed recovery pin left inputs fixed: %v", err)
			}
		})
	}
}

func TestCleanupStartsTTLAfterLastLeaseRelease(t *testing.T) {
	t.Parallel()
	cfg := testConfig(t)
	cfg.IdleTTL = time.Hour
	s := newTestStore(t, cfg)
	now := time.Now()
	s.now = func() time.Time { return now }
	source := sourceFile(t, []byte("clip"))
	first := ensure(t, s, source)
	second := ensure(t, s, source)
	local := first.Paths[source.Path]
	now = now.Add(2 * time.Hour)
	first.Release()
	if err := s.Cleanup(context.Background()); err != nil {
		t.Fatal(err)
	}
	requireBytes(t, local, []byte("clip"))
	second.Release()
	now = now.Add(30 * time.Minute)
	if err := s.Cleanup(context.Background()); err != nil {
		t.Fatal(err)
	}
	requireBytes(t, local, []byte("clip"))
	now = now.Add(31 * time.Minute)
	if err := s.Cleanup(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(local); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("expired unpinned cache survived: %v", err)
	}
}

func TestCacheBudgetNeverEvictsPinnedInputs(t *testing.T) {
	t.Parallel()
	cfg := testConfig(t)
	cfg.MaxBytes = 8
	s := newTestStore(t, cfg)
	front := sourceFile(t, []byte("12345678"))
	rear := sourceFile(t, []byte("ABCDEFGH"))
	first := ensure(t, s, front)
	local := first.Paths[front.Path]
	if _, err := s.EnsureLocal(context.Background(), []File{rear}, nil); !errors.Is(err, ErrSpace) {
		t.Fatalf("expected space rejection rather than deletion of pinned input: %v", err)
	}
	requireBytes(t, local, []byte("12345678"))
	first.Release()
	second := ensure(t, s, rear)
	requireBytes(t, second.Paths[rear.Path], []byte("ABCDEFGH"))
	if _, err := os.Stat(local); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("LRU did not reclaim idle input: %v", err)
	}
}

func TestReserveIncludesOutputsAndSourceMissesAndCheckDoesNotDoubleCount(t *testing.T) {
	t.Parallel()
	s := newTestStore(t, testConfig(t))
	base, err := directoryBytes(context.Background(), s.cfg.MediaRoot)
	if err != nil {
		t.Fatal(err)
	}
	s.cfg.MaxTotalBytes = base + 3000
	release, err := s.Reserve(context.Background(), 2000)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(release)
	source := sourceFile(t, bytes.Repeat([]byte{0x12}, 1500))
	if _, err := s.EnsureLocal(context.Background(), []File{source}, nil); !errors.Is(err, ErrSpace) {
		t.Fatalf("source misses ignored output reservation: %v", err)
	}
	output := filepath.Join(s.cfg.MediaRoot, "export.mp4")
	if err := os.WriteFile(output, bytes.Repeat([]byte{0x45}, 2000), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := s.CheckSpace(context.Background()); err != nil {
		t.Fatalf("actual-output guard double-counted its reservation: %v", err)
	}
	if _, err := s.Reserve(context.Background(), 1); !errors.Is(err, ErrSpace) {
		t.Fatalf("next reservation ignored existing output plus active reservation: %v", err)
	}
	release()
	release()
	if err := os.WriteFile(output, bytes.Repeat([]byte{0x45}, 4000), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := s.CheckSpace(context.Background()); !errors.Is(err, ErrSpace) {
		t.Fatalf("over-budget actual output accepted: %v", err)
	}
}

func TestReserveHonorsActualFilesystemFreeReserve(t *testing.T) {
	t.Parallel()
	s := newTestStore(t, testConfig(t))
	s.cfg.MinFreeBytes = 10
	s.space = func(string) (uint64, uint64, error) { return 10000, 1000, nil }
	if _, err := s.Reserve(context.Background(), 991); !errors.Is(err, ErrSpace) {
		t.Fatalf("minimum free reserve was spent: %v", err)
	}
	release, err := s.Reserve(context.Background(), 990)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(release)
	source := sourceFile(t, []byte{0x12})
	if _, err := s.EnsureLocal(context.Background(), []File{source}, nil); !errors.Is(err, ErrSpace) {
		t.Fatalf("copy ignored another task reservation: %v", err)
	}
	if err := s.CheckSpace(context.Background()); err != nil {
		t.Fatalf("actual free-space check double-counted reservation: %v", err)
	}
	release()
	ensure(t, s, source)
}

func TestEnsureLocalReservedPinsHitsBeforeReclaimAndRollsBackOnFailure(t *testing.T) {
	t.Parallel()
	s := newTestStore(t, testConfig(t))
	selected := sourceFile(t, bytes.Repeat([]byte{1}, 1000))
	unused := sourceFile(t, bytes.Repeat([]byte{2}, 4000))
	first := ensure(t, s, selected, unused)
	selectedLocal := first.Paths[selected.Path]
	unusedLocal := first.Paths[unused.Path]
	first.Release()
	before, err := os.Stat(selectedLocal)
	if err != nil {
		t.Fatal(err)
	}
	used, err := directoryBytes(context.Background(), s.cfg.MediaRoot)
	if err != nil {
		t.Fatal(err)
	}
	s.cfg.MaxTotalBytes = used + 1000
	lease, release, err := s.EnsureLocalReserved(context.Background(), []File{selected}, 1500, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(lease.Release)
	t.Cleanup(release)
	after, err := os.Stat(selectedLocal)
	if err != nil || !os.SameFile(before, after) || !before.ModTime().Equal(after.ModTime()) {
		t.Fatalf("selected cache was evicted and copied again: %v", err)
	}
	if _, err := os.Stat(unusedLocal); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("unused cache was not reclaimed: %v", err)
	}
	release()
	lease.Release()
	s.cfg.MaxTotalBytes = 1 << 20
	ctx, cancel := context.WithCancel(context.Background())
	fresh := sourceFile(t, bytes.Repeat([]byte{3}, 512<<10))
	_, _, err = s.EnsureLocalReserved(ctx, []File{fresh}, 20000, func(done, _ uint64) {
		if done > 0 {
			cancel()
		}
	})
	cancel()
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("copy cancellation: %v", err)
	}
	if s.reserved != 0 {
		t.Fatalf("failed copy leaked reservation: %d", s.reserved)
	}
}

func TestPolicyAndSetIdleTTL(t *testing.T) {
	t.Parallel()
	cfg := testConfig(t)
	s := newTestStore(t, cfg)
	if got := s.Policy(); got.IdleTTL != 24*time.Hour {
		t.Fatalf("configured TTL not reflected: %+v", got)
	}
	for _, ttl := range []time.Duration{0, time.Hour, 168 * time.Hour} {
		if err := s.SetIdleTTL(ttl); err != nil || s.Policy().IdleTTL != ttl {
			t.Fatalf("valid policy update %v: %v", ttl, err)
		}
	}
	for _, ttl := range []time.Duration{-time.Second, 168*time.Hour + time.Second} {
		if err := s.SetIdleTTL(ttl); err == nil {
			t.Fatalf("invalid policy accepted: %v", ttl)
		}
	}
}
