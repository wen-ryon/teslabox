package writeback

import (
	"os"
	"path/filepath"
	"testing"
)

func TestApplyWritesByteThresholds(t *testing.T) {
	root := t.TempDir()
	for _, name := range []string{"dirty_background_bytes", "dirty_bytes"} {
		if err := os.WriteFile(filepath.Join(root, name), nil, 0o644); err != nil {
			t.Fatal(err)
		}
	}

	err := Apply(Config{
		Root:            root,
		BackgroundBytes: 64 * 1024 * 1024,
		DirtyBytes:      256 * 1024 * 1024,
	})
	if err != nil {
		t.Fatal(err)
	}

	background, err := os.ReadFile(filepath.Join(root, "dirty_background_bytes"))
	if err != nil {
		t.Fatal(err)
	}
	if got, want := string(background), "67108864\n"; got != want {
		t.Fatalf("dirty_background_bytes = %q, want %q", got, want)
	}
	dirty, err := os.ReadFile(filepath.Join(root, "dirty_bytes"))
	if err != nil {
		t.Fatal(err)
	}
	if got, want := string(dirty), "268435456\n"; got != want {
		t.Fatalf("dirty_bytes = %q, want %q", got, want)
	}
}

func TestApplyRejectsInvalidThresholds(t *testing.T) {
	tests := []struct {
		name   string
		config Config
	}{
		{name: "zero background", config: Config{Root: t.TempDir(), DirtyBytes: 1}},
		{name: "zero dirty", config: Config{Root: t.TempDir(), BackgroundBytes: 1}},
		{name: "equal thresholds", config: Config{Root: t.TempDir(), BackgroundBytes: 1, DirtyBytes: 1}},
		{name: "background exceeds dirty", config: Config{Root: t.TempDir(), BackgroundBytes: 2, DirtyBytes: 1}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if err := Apply(test.config); err == nil {
				t.Fatal("Apply() error = nil, want validation failure")
			}
		})
	}
}

func TestApplyReportsWriteFailure(t *testing.T) {
	err := Apply(Config{
		Root:            filepath.Join(t.TempDir(), "missing"),
		BackgroundBytes: 1,
		DirtyBytes:      2,
	})
	if err == nil {
		t.Fatal("Apply() error = nil, want write failure")
	}
}
