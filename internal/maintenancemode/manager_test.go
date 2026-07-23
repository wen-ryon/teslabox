package maintenancemode

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func TestManagerPersistsAutoRecovery(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "maintenance.json")
	manager, err := New(path)
	if err != nil {
		t.Fatal(err)
	}
	if manager.Settings().AutoRecovery || !manager.Settings().UpdateAutoRecovery {
		t.Fatalf("new manager settings = %#v", manager.Settings())
	}
	if _, err := manager.SetAutoRecovery(true); err != nil {
		t.Fatal(err)
	}
	reloaded, err := New(path)
	if err != nil {
		t.Fatal(err)
	}
	if !reloaded.Settings().AutoRecovery || !reloaded.Settings().UpdateAutoRecovery {
		t.Fatalf("automatic recovery was not persisted: %#v", reloaded.Settings())
	}
}

func TestManagerMigratesLegacyManualMediaSettingToAutomaticUpdateRecovery(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "maintenance.json")
	if err := os.WriteFile(path, []byte(`{"auto_recovery":false}`), 0o600); err != nil {
		t.Fatal(err)
	}
	manager, err := New(path)
	if err != nil {
		t.Fatal(err)
	}
	if manager.Settings().AutoRecovery || !manager.Settings().UpdateAutoRecovery {
		t.Fatalf("legacy settings = %#v", manager.Settings())
	}
}

func TestManagerRejectsSymbolicLink(t *testing.T) {
	t.Parallel()
	if runtime.GOOS == "windows" {
		t.Skip("Windows 创建符号链接需要额外开发者权限；目标 Debian 会执行此安全检查")
	}
	root := t.TempDir()
	target := filepath.Join(root, "target.json")
	if err := os.WriteFile(target, []byte(`{"auto_recovery":true}`), 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(root, "maintenance.json")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	if _, err := New(link); err == nil {
		t.Fatal("New() error = nil, want symbolic link rejection")
	}
}
