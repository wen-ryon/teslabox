package backing

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/wen-ryon/teslabox/internal/storage"
)

func TestInspectorInspect(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	layout := Layout{
		Root: root, ImagePath: "images/teslacam.img", SnapshotPath: "snapshots", IndexPath: "index", StatePath: "state",
	}
	inspector := NewInspector(layout)

	tests := []struct {
		name          string
		device        storage.Device
		prepare       func(t *testing.T)
		expectedState State
		expectedReady bool
	}{
		{
			name:          "uninitialized exfat drive",
			device:        storage.Device{FSType: "exfat", MountPoints: []string{root}},
			expectedState: StateNeedsInitialization,
		},
		{
			name:          "unmounted xfs drive",
			device:        storage.Device{FSType: "xfs"},
			expectedState: StateNeedsMount,
		},
		{
			name:          "wrong mount point",
			device:        storage.Device{FSType: "xfs", MountPoints: []string{filepath.Join(root, "other")}},
			expectedState: StateWrongMount,
		},
		{
			name:          "missing layout",
			device:        storage.Device{FSType: "xfs", MountPoints: []string{root}},
			expectedState: StateIncomplete,
		},
		{
			name:   "ready layout",
			device: storage.Device{FSType: "xfs", MountPoints: []string{root}},
			prepare: func(t *testing.T) {
				t.Helper()
				for _, directory := range []string{"images", "snapshots", "index", "state"} {
					if err := os.MkdirAll(filepath.Join(root, directory), 0o755); err != nil {
						t.Fatal(err)
					}
				}
				if err := os.WriteFile(filepath.Join(root, "images", "teslacam.img"), []byte("image"), 0o600); err != nil {
					t.Fatal(err)
				}
			},
			expectedState: StateReady,
			expectedReady: true,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if test.prepare != nil {
				test.prepare(t)
			}
			status, err := inspector.Inspect(test.device)
			if err != nil {
				t.Fatalf("Inspect() error = %v", err)
			}
			if status.State != test.expectedState || status.Ready != test.expectedReady {
				t.Fatalf("status = %#v", status)
			}
		})
	}
}

func TestInspectorRejectsSymlinkedImage(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	for _, directory := range []string{"images", "snapshots", "index", "state"} {
		if err := os.MkdirAll(filepath.Join(root, directory), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	target := filepath.Join(root, "target.img")
	if err := os.WriteFile(target, []byte("image"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, filepath.Join(root, "images", "teslacam.img")); err != nil {
		t.Skipf("当前环境不允许创建符号链接: %v", err)
	}

	inspector := NewInspector(Layout{
		Root: root, ImagePath: "images/teslacam.img", SnapshotPath: "snapshots", IndexPath: "index", StatePath: "state",
	})
	_, err := inspector.Inspect(storage.Device{FSType: "xfs", MountPoints: []string{root}})
	if err == nil {
		t.Fatal("Inspect() error = nil, want symlink rejection")
	}
}
