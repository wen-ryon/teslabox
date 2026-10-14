//go:build linux

package updater

import (
	"context"
	"os"
	"path/filepath"
	"syscall"
	"testing"
)

func TestForwardBackupRejectsUnsafeExistingPrevious(t *testing.T) {
	kinds := []string{"symlink", "hardlink", "writable", "writable_parent"}
	// Linux CI 的隔离 root 步骤必须实际覆盖外来 UID，非 root 全量测试不伪造 chown 成功。
	if os.Geteuid() == 0 {
		kinds = append(kinds, "foreign_owner")
	}
	for _, kind := range kinds {
		t.Run(kind, func(t *testing.T) {
			cfg, request, pkg := installFixture(t)
			previous := filepath.Join(cfg.BackupDir, latestBackupName)
			other := filepath.Join(filepath.Dir(cfg.BackupDir), "other-program")
			if err := os.WriteFile(other, []byte("other"), 0o600); err != nil {
				t.Fatal(err)
			}
			var err error
			switch kind {
			case "symlink":
				err = os.Symlink(other, previous)
			case "hardlink":
				err = os.Link(other, previous)
			default:
				err = os.WriteFile(previous, []byte("previous"), 0o600)
			}
			if err != nil {
				t.Fatal(err)
			}
			if kind == "writable" {
				if err := os.Chmod(previous, 0o666); err != nil {
					t.Fatal(err)
				}
			}
			if kind == "writable_parent" {
				if err := os.Chmod(cfg.BackupDir, 0o777); err != nil {
					t.Fatal(err)
				}
			}
			if kind == "foreign_owner" {
				if err := os.Chown(previous, 1, -1); err != nil {
					t.Fatal("隔离 root 所有者测试未生效", err)
				}
			}
			starts := 0
			err = transactionalInstall(t.Context(), cfg, request, pkg, installOperations{stop: func(context.Context) error { return nil }, start: func(context.Context) error { starts++; return nil }})
			if err == nil {
				t.Fatal("不安全previous被覆盖")
			}
			value, _ := os.ReadFile(other)
			if string(value) != "other" {
				t.Fatal("第三方链接目标被改写")
			}
			main, _ := os.ReadFile(cfg.TargetPath)
			if string(main) != "old executable" {
				t.Fatal("拒绝后仍安装了候选")
			}
		})
	}
}

type foreignBackupInfo struct{ os.FileInfo }

func (f foreignBackupInfo) Sys() any {
	stat := *f.FileInfo.Sys().(*syscall.Stat_t)
	stat.Uid = uint32(os.Geteuid() + 1)
	return &stat
}

func TestForwardBackupOwnerGuardRejectsForeignMetadata(t *testing.T) {
	cfg, _, _ := installFixture(t)
	info, err := os.Stat(cfg.TargetPath)
	if err != nil {
		t.Fatal(err)
	}
	if safeFileMetadata(foreignBackupInfo{info}) == nil {
		t.Fatal("其他用户文件元数据通过了共享snapshot防线")
	}
}
