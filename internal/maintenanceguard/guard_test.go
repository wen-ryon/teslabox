package maintenanceguard

import (
	"os"
	"path/filepath"
	"testing"
)

func TestGuardRecoversInterruptedOperation(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	guard, err := New(filepath.Join(root, "run", "lock"), filepath.Join(root, "state.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err := guard.Save(State{Operation: "snapshot_reserve_repair", State: StateRunning, TargetBytes: 1024}); err != nil {
		t.Fatal(err)
	}
	state, exists, err := guard.RecoverInterrupted()
	if err != nil || !exists || state.State != StateInterrupted || state.TargetBytes != 1024 {
		t.Fatalf("RecoverInterrupted() = %+v, %t, %v", state, exists, err)
	}
	if _, err := guard.AcquireGadgetStart(t.Context()); err == nil {
		t.Fatal("AcquireGadgetStart() error = nil for interrupted operation")
	}
}

func TestGuardGadgetStartStateMatrix(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		state   OperationState
		blocked bool
	}{
		{name: "idle", state: StateIdle},
		{name: "completed", state: StateCompleted},
		{name: "cancelled safe", state: StateCancelledSafe},
		{name: "running", state: StateRunning, blocked: true},
		{name: "interrupted", state: StateInterrupted, blocked: true},
		{name: "cancelled unsafe", state: StateCancelledUnsafe, blocked: true},
		{name: "failed", state: StateFailed, blocked: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			root := t.TempDir()
			guard, err := New(filepath.Join(root, "run", "lock"), filepath.Join(root, "state.json"))
			if err != nil {
				t.Fatal(err)
			}
			if err := guard.Save(State{State: test.state}); err != nil {
				t.Fatal(err)
			}
			release, err := guard.AcquireGadgetStart(t.Context())
			if release != nil {
				release()
			}
			if (err != nil) != test.blocked {
				t.Fatalf("AcquireGadgetStart() error = %v, blocked = %t", err, test.blocked)
			}
		})
	}
}

func TestGuardIndependentCommandReadsPauseStateWithoutMainProcess(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	bootPath := filepath.Join(root, "boot-id")
	pausePath := filepath.Join(root, "gadget-paused")
	if err := os.WriteFile(bootPath, []byte("boot-current\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	guard, err := New(filepath.Join(root, "run", "lock"), filepath.Join(root, "state.json"), pausePath, bootPath)
	if err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name    string
		value   string
		blocked bool
	}{
		{name: "manual pause", value: "paused\n", blocked: true},
		{name: "same boot attempt", value: "attempt:boot-current\n", blocked: true},
		{name: "stale previous boot", value: "service:boot-old\n"},
		{name: "malformed", value: "unknown\n", blocked: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if err := os.WriteFile(pausePath, []byte(test.value), 0o600); err != nil {
				t.Fatal(err)
			}
			release, err := guard.AcquireGadgetStart(t.Context())
			if release != nil {
				release()
			}
			if (err != nil) != test.blocked {
				t.Fatalf("AcquireGadgetStart() error = %v, blocked = %t", err, test.blocked)
			}
		})
	}
}
