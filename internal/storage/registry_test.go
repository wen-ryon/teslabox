package storage

import (
	"path/filepath"
	"testing"
)

func TestFileRegistrySuspendsActiveDeviceDuringIncompleteInitialization(t *testing.T) {
	t.Parallel()
	registry := NewFileRegistry(filepath.Join(t.TempDir(), "registry.json"))
	legacy := DeviceRecord{UUID: "legacy", Label: "TESLABOX", Status: "ready"}
	if err := registry.Complete(legacy); err != nil {
		t.Fatal(err)
	}
	pending := DeviceRecord{Serial: "USB-NEW", SizeBytes: 128_000_000_000, StablePath: "/dev/disk/by-id/usb-new"}
	if err := registry.Begin(pending); err != nil {
		t.Fatal(err)
	}
	active, exists, err := registry.Active()
	if err != nil {
		t.Fatal(err)
	}
	if exists || active.UUID != "" {
		t.Fatalf("active = %#v, exists = %t", active, exists)
	}
	if err := registry.Fail(pending, &InputErrorForTest{}); err != nil {
		t.Fatal(err)
	}
	records, err := registry.Records()
	if err != nil {
		t.Fatal(err)
	}
	if len(records) != 2 || records[1].Status != "failed" || records[1].LastError == "" {
		t.Fatalf("records = %#v", records)
	}
	if err := registry.CancelPending(pending.StablePath, pending.Serial); err != nil {
		t.Fatal(err)
	}
	active, exists, err = registry.Active()
	if err != nil || !exists || active.UUID != "legacy" {
		t.Fatalf("active after cancel = %#v, exists = %t, err = %v", active, exists, err)
	}
}

type InputErrorForTest struct{}

func (*InputErrorForTest) Error() string { return "测试失败" }

func TestFileRegistryEnsureLegacyIsIdempotent(t *testing.T) {
	t.Parallel()
	registry := NewFileRegistry(filepath.Join(t.TempDir(), "registry.json"))
	if err := registry.EnsureLegacy("old-uuid", "TESLABOX"); err != nil {
		t.Fatal(err)
	}
	if err := registry.EnsureLegacy("new-uuid", "OTHER"); err != nil {
		t.Fatal(err)
	}
	active, exists, err := registry.Active()
	if err != nil {
		t.Fatal(err)
	}
	if !exists || active.UUID != "old-uuid" {
		t.Fatalf("active = %#v, exists = %t", active, exists)
	}
}
