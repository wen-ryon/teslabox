package datamount

import (
	"context"
	"errors"
	"reflect"
	"testing"

	"github.com/wen-ryon/teslabox/internal/storage"
)

type command struct {
	name string
	args []string
}

type fakeRunner struct {
	commands []command
	err      error
}

func (r *fakeRunner) Run(_ context.Context, name string, args ...string) ([]byte, error) {
	r.commands = append(r.commands, command{name: name, args: args})
	return nil, r.err
}

func TestManagerMountUsesStableIdentifier(t *testing.T) {
	tests := []struct {
		name     string
		uuid     string
		label    string
		device   storage.Device
		wantArgs []string
	}{
		{
			name:   "uuid",
			uuid:   "data-uuid",
			label:  "TESLABOX",
			device: storage.Device{UUID: "data-uuid", Label: "TESLABOX", FSType: "xfs"},
			wantArgs: []string{
				"--options", "noatime,nosuid,nodev", "--uuid", "data-uuid",
			},
		},
		{
			name:   "label fallback",
			label:  "TESLABOX",
			device: storage.Device{Label: "TESLABOX", FSType: "xfs"},
			wantArgs: []string{
				"--options", "noatime,nosuid,nodev", "--label", "TESLABOX",
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			runner := &fakeRunner{}
			manager := New(runner, t.TempDir(), test.uuid, test.label)
			if err := manager.Mount(t.Context(), test.device); err != nil {
				t.Fatalf("Mount() error = %v", err)
			}
			wantArgs := append(append([]string{}, test.wantArgs...), manager.mountPath)
			if len(runner.commands) != 1 || runner.commands[0].name != "mount" || !reflect.DeepEqual(runner.commands[0].args, wantArgs) {
				t.Fatalf("commands = %#v, want mount %#v", runner.commands, wantArgs)
			}
		})
	}
}

func TestManagerMountRejectsUnsafeDevice(t *testing.T) {
	tests := []struct {
		name   string
		device storage.Device
	}{
		{name: "wrong filesystem", device: storage.Device{UUID: "data-uuid", FSType: "ext4"}},
		{name: "wrong uuid", device: storage.Device{UUID: "other", FSType: "xfs"}},
		{name: "already mounted", device: storage.Device{UUID: "data-uuid", FSType: "xfs", MountPoints: []string{"/media/other"}}},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			runner := &fakeRunner{}
			manager := New(runner, t.TempDir(), "data-uuid", "TESLABOX")
			if err := manager.Mount(t.Context(), test.device); err == nil {
				t.Fatal("Mount() error = nil, want validation error")
			}
			if len(runner.commands) != 0 {
				t.Fatalf("unsafe mount executed commands: %#v", runner.commands)
			}
		})
	}
}

func TestManagerMountWrapsCommandError(t *testing.T) {
	runner := &fakeRunner{err: errors.New("测试失败")}
	manager := New(runner, t.TempDir(), "data-uuid", "TESLABOX")
	err := manager.Mount(t.Context(), storage.Device{UUID: "data-uuid", FSType: "xfs"})
	if err == nil {
		t.Fatal("Mount() error = nil, want command error")
	}
}

type noActiveIdentity struct{}

func (noActiveIdentity) Active() (storage.DeviceRecord, bool, error) {
	return storage.DeviceRecord{}, false, nil
}

func TestRegisteredManagerRejectsMountWhileIdentityIsSuspended(t *testing.T) {
	t.Parallel()
	runner := &fakeRunner{}
	manager := NewRegistered(runner, t.TempDir(), noActiveIdentity{}, "legacy", "TESLABOX")
	err := manager.Mount(t.Context(), storage.Device{UUID: "legacy", Label: "TESLABOX", FSType: "xfs"})
	if err == nil {
		t.Fatal("Mount() error = nil")
	}
	if len(runner.commands) != 0 {
		t.Fatalf("commands = %#v", runner.commands)
	}
}
