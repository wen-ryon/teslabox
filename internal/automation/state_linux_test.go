//go:build linux

package automation

import (
	"os"
	"path/filepath"
	"testing"
)

func TestCarPlayStateRequiresPrivatePermissions(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	if err := writeRuntimeState(path, []byte(`{"version":1,"lease":null}`)); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := validatePrivateStateFile(info); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, 0o644); err != nil {
		t.Fatal(err)
	}
	info, err = os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := validatePrivateStateFile(info); err == nil {
		t.Fatal("接受了非私有的恢复记录")
	}
}
