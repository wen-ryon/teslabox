//go:build !windows

package automation

import (
	"os"
	"path/filepath"
	"testing"
)

func TestEnsureCLIShortcutCreatesAndKeepsExpectedLink(t *testing.T) {
	t.Parallel()
	directory := t.TempDir()
	target := filepath.Join(directory, "teslabox")
	link := filepath.Join(directory, "teslaboxctl")
	if err := os.WriteFile(target, []byte("binary"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := EnsureCLIShortcut(link, target); err != nil {
		t.Fatal(err)
	}
	if err := EnsureCLIShortcut(link, target); err != nil {
		t.Fatalf("重复检查正确链接失败: %v", err)
	}
	resolved, err := os.Readlink(link)
	if err != nil {
		t.Fatal(err)
	}
	if resolved != target {
		t.Fatalf("link target = %q, want %q", resolved, target)
	}
}

func TestEnsureCLIShortcutRefusesExistingFileOrDifferentLink(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name  string
		setup func(string) error
	}{
		{name: "普通文件", setup: func(path string) error { return os.WriteFile(path, []byte("keep"), 0o644) }},
		{name: "其他链接", setup: func(path string) error { return os.Symlink("/usr/bin/false", path) }},
	} {
		t.Run(test.name, func(t *testing.T) {
			directory := t.TempDir()
			link := filepath.Join(directory, "teslaboxctl")
			if err := test.setup(link); err != nil {
				t.Fatal(err)
			}
			if err := EnsureCLIShortcut(link, filepath.Join(directory, "teslabox")); err == nil {
				t.Fatal("EnsureCLIShortcut() 覆盖了非预期目标")
			}
		})
	}
}
