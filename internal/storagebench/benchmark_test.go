package storagebench

import (
	"os"
	"testing"
	"time"
)

func TestRunWritesBoundedSixCameraBurstsAndCleansUp(t *testing.T) {
	root := t.TempDir()
	benchmark := New(Config{
		CameraCount: 6, BurstCount: 2, BytesPerFile: 8 * 1024, ChunkBytes: 1024,
		StableMBps: 0, LimitedMBps: 0, StableStall: time.Minute, LimitedStall: time.Minute,
		StableFsync: time.Minute, LimitedFsync: time.Minute,
	})
	report, err := benchmark.Run(t.Context(), root)
	if err != nil {
		t.Fatal(err)
	}
	if report.BytesWritten != 6*2*8*1024 || len(report.Bursts) != 2 || report.Profile != ProfileStable {
		t.Fatalf("report = %#v", report)
	}
	entries, err := os.ReadDir(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("temporary entries = %#v", entries)
	}
}

func TestClassifyUnsafeStorage(t *testing.T) {
	report := Report{MinimumBurstMBps: 5, MaximumWriteStallMS: 9000, FsyncP95MS: 9000}
	classify(&report, DefaultConfig())
	if report.Profile != ProfileUnsafe {
		t.Fatalf("profile = %s", report.Profile)
	}
}
