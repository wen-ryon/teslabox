package updater

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestDirectorySyncFailureStopsAndRetriesEveryAncestor(t *testing.T) {
	root := t.TempDir()
	target := filepath.Join(root, "new", "nested", "backups")
	for _, failAt := range []string{root, filepath.Join(root, "new"), filepath.Join(root, "new", "nested"), target} {
		t.Run(filepath.Base(failAt), func(t *testing.T) {
			err := durableMkdirAllWith(target, 0o700, func(path string) error {
				if path == failAt {
					return errors.New("injected directory sync failure")
				}
				return nil
			})
			if err == nil {
				t.Fatal("目录同步失败必须向上传播")
			}
			seen := map[string]bool{}
			if err = durableMkdirAllWith(target, 0o700, func(path string) error { seen[path] = true; return nil }); err != nil {
				t.Fatal(err)
			}
			for p := target; filepath.Dir(p) != p; p = filepath.Dir(p) {
				if !seen[filepath.Dir(p)] {
					t.Fatalf("重试未同步 %s", filepath.Dir(p))
				}
			}
			if !seen[target] {
				t.Fatal("未同步叶目录")
			}
			if info, err := os.Stat(target); err != nil || !info.IsDir() {
				t.Fatal("目录未创建")
			}
		})
	}
}
