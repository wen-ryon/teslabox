package storagepolicy

import "testing"

func TestSnapshotReservePolicyRequiredBytes(t *testing.T) {
	t.Parallel()
	policy, err := NewSnapshotReservePolicy(8*1024*1024*1024, 5, 4*1024*1024*1024)
	if err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name           string
		total          uint64
		wantRuntime    uint64
		wantInitialize uint64
	}{
		{name: "fixed floor wins", total: 128_000_000_000, wantRuntime: 8 * 1024 * 1024 * 1024, wantInitialize: 12 * 1024 * 1024 * 1024},
		{name: "percentage wins", total: 1_000_000_000_000, wantRuntime: 50_000_000_000, wantInitialize: 50_000_000_000 + 4*1024*1024*1024},
		{name: "percentage rounds up", total: 101, wantRuntime: 8 * 1024 * 1024 * 1024, wantInitialize: 12 * 1024 * 1024 * 1024},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			runtime, runtimeErr := policy.RuntimeRequiredBytes(test.total)
			if runtimeErr != nil || runtime != test.wantRuntime {
				t.Fatalf("RuntimeRequiredBytes() = %d, %v, want %d", runtime, runtimeErr, test.wantRuntime)
			}
			initialization, initializationErr := policy.InitializationRequiredBytes(test.total)
			if initializationErr != nil || initialization != test.wantInitialize {
				t.Fatalf("InitializationRequiredBytes() = %d, %v, want %d", initialization, initializationErr, test.wantInitialize)
			}
		})
	}
}

func TestSnapshotReservePolicySeparatesRuntimeAndInitialization(t *testing.T) {
	t.Parallel()
	policy := DefaultSnapshotReservePolicy()
	total := uint64(128_000_000_000)
	free := uint64(10_000_000_000)
	runtime, err := policy.EvaluateRuntime(total, free)
	if err != nil || !runtime.Ready {
		t.Fatalf("runtime evaluation = %+v, %v", runtime, err)
	}
	initialization, err := policy.EvaluateInitialization(total, free)
	if err != nil || initialization.Ready || initialization.ReasonCode != ReasonInsufficientSpace {
		t.Fatalf("initialization evaluation = %+v, %v", initialization, err)
	}
}

func TestSnapshotReservePolicyReportsUnreachableDevice(t *testing.T) {
	t.Parallel()
	policy := DefaultSnapshotReservePolicy()
	evaluation, err := policy.EvaluateInitialization(10*1024*1024*1024, 9*1024*1024*1024)
	if err != nil {
		t.Fatal(err)
	}
	if evaluation.Ready || evaluation.ReasonCode != ReasonPolicyUnreachable {
		t.Fatalf("evaluation = %+v", evaluation)
	}
}

func TestNewSnapshotReservePolicyRejectsInvalidConfiguration(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name     string
		bytes    uint64
		percent  uint64
		headroom uint64
	}{
		{name: "zero bytes", percent: 5},
		{name: "zero percent", bytes: 1},
		{name: "percent too large", bytes: 1, percent: 100},
		{name: "overflow", bytes: ^uint64(0), percent: 5, headroom: 1},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if _, err := NewSnapshotReservePolicy(test.bytes, test.percent, test.headroom); err == nil {
				t.Fatal("NewSnapshotReservePolicy() error = nil")
			}
		})
	}
}
