package snapshot

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestVolumeReadFailureCleansMountBeforeLoop(t *testing.T) {
	for _, failUnmount := range []bool{false, true} {
		name := "cleanup succeeds"
		if failUnmount {
			name = "unmount failure preserves image"
		}
		t.Run(name, func(t *testing.T) {
			root := t.TempDir()
			image := filepath.Join(root, "source.img")
			if err := os.WriteFile(image, []byte("source"), 0o600); err != nil {
				t.Fatal(err)
			}
			snaps, mounts := filepath.Join(root, "snapshots"), filepath.Join(root, "mounts")
			for _, p := range []string{snaps, mounts} {
				if err := os.Mkdir(p, 0o750); err != nil {
					t.Fatal(err)
				}
			}
			runner := &fakeRunner{}
			if failUnmount {
				runner.failName = "umount"
			}
			m := New(runner, Config{ImagePath: image, SnapshotPath: snaps, MountRoot: mounts, Policy: testSnapshotPolicy(t)})
			m.space = func(string) (uint64, uint64, error) { return 100 << 30, 30 << 30, nil }
			cause := errors.New("volume statfs failed")
			m.volumeSpace = func(string) (uint64, uint64, error) { return 0, 0, cause }
			_, err := m.Create(t.Context())
			if !errors.Is(err, cause) {
				t.Fatalf("丢失原始错误: %v", err)
			}
			unmountAt, detachAt := -1, -1
			for i, c := range runner.commands {
				if c.name == "umount" {
					unmountAt = i
				}
				if c.name == "losetup" && c.args[0] == "--detach" {
					detachAt = i
				}
			}
			entries, err := os.ReadDir(snaps)
			if err != nil {
				t.Fatal(err)
			}
			if failUnmount {
				if detachAt != -1 || len(entries) != 1 {
					t.Fatal("卸载失败仍删除了可恢复资源")
				}
			} else if unmountAt < 0 || detachAt <= unmountAt || len(entries) != 0 {
				t.Fatalf("清理顺序错误: %v", runner.commands)
			}
		})
	}
}
