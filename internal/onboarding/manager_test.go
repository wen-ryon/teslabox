package onboarding

import (
	"path/filepath"
	"testing"
)

func TestManagerPersistsIndependentGuideState(t *testing.T) {
	path := filepath.Join(t.TempDir(), "onboarding.json")
	manager, err := New(path)
	if err != nil {
		t.Fatal(err)
	}
	if manager.State().PageGuideComplete {
		t.Fatal("new device unexpectedly completed page guide")
	}
	if _, err := manager.CompletePageGuide(); err != nil {
		t.Fatal(err)
	}
	stablePath := "/dev/disk/by-id/usb-test-device"
	if _, err := manager.DismissStorage(stablePath); err != nil {
		t.Fatal(err)
	}
	reloaded, err := New(path)
	if err != nil {
		t.Fatal(err)
	}
	state := reloaded.State()
	if !state.PageGuideComplete || len(state.DismissedStorageStablePath) != 1 || state.DismissedStorageStablePath[0] != stablePath {
		t.Fatalf("unexpected persisted state: %+v", state)
	}
}

func TestManagerRejectsUnstableStoragePath(t *testing.T) {
	manager, err := New(filepath.Join(t.TempDir(), "onboarding.json"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := manager.DismissStorage("/dev/sda"); err == nil {
		t.Fatal("DismissStorage() accepted unstable device path")
	}
}
