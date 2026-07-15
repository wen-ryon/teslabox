package service

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"testing"

	"github.com/wen-ryon/teslabox/internal/backing"
	"github.com/wen-ryon/teslabox/internal/config"
	"github.com/wen-ryon/teslabox/internal/gadget"
	"github.com/wen-ryon/teslabox/internal/storage"
)

type fakeDetector struct {
	device storage.Device
	err    error
}

func (d fakeDetector) Detect(context.Context) (storage.Device, error) {
	return d.device, d.err
}

type fakeInspector struct {
	status backing.Status
	err    error
}

func (i fakeInspector) Inspect(storage.Device) (backing.Status, error) {
	return i.status, i.err
}

type fakeProber struct {
	environment gadget.Environment
	err         error
}

func (p fakeProber) Probe() (gadget.Environment, error) {
	return p.environment, p.err
}

func TestManagerRefreshPhases(t *testing.T) {
	t.Parallel()
	device := storage.Device{Path: "/dev/sda1", UUID: "data-uuid", FSType: "xfs"}
	tests := []struct {
		name              string
		detector          fakeDetector
		inspector         fakeInspector
		prober            fakeProber
		expectedPhase     Phase
		expectedErrorKind error
	}{
		{
			name:              "waiting for data drive",
			detector:          fakeDetector{err: storage.ErrDeviceNotFound},
			expectedPhase:     PhaseWaitingForDataDrive,
			expectedErrorKind: storage.ErrDeviceNotFound,
		},
		{
			name:          "preparing backing store",
			detector:      fakeDetector{device: device},
			inspector:     fakeInspector{status: backing.Status{State: backing.StateIncomplete}},
			prober:        fakeProber{environment: gadget.Environment{Ready: true}},
			expectedPhase: PhasePreparingBackingStore,
		},
		{
			name:          "gadget environment degraded",
			detector:      fakeDetector{device: device},
			inspector:     fakeInspector{status: backing.Status{State: backing.StateReady, Ready: true}},
			prober:        fakeProber{environment: gadget.Environment{Reason: "未发现 UDC"}},
			expectedPhase: PhaseDegraded,
		},
		{
			name:          "ready for gadget",
			detector:      fakeDetector{device: device},
			inspector:     fakeInspector{status: backing.Status{State: backing.StateReady, Ready: true}},
			prober:        fakeProber{environment: gadget.Environment{Ready: true, UDCNames: []string{"fcc00000.dwc3"}}},
			expectedPhase: PhaseReadyForGadget,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			manager := NewManager(
				config.Config{},
				test.detector,
				test.inspector,
				test.prober,
				slog.New(slog.NewTextHandler(io.Discard, nil)),
			)
			err := manager.Refresh(t.Context())
			if test.expectedErrorKind != nil && !errors.Is(err, test.expectedErrorKind) {
				t.Fatalf("Refresh() error = %v", err)
			}
			if test.expectedErrorKind == nil && err != nil {
				t.Fatalf("Refresh() error = %v", err)
			}
			state := manager.Snapshot()
			if state.Phase != test.expectedPhase {
				t.Fatalf("Phase = %q, want %q", state.Phase, test.expectedPhase)
			}
		})
	}
}

func TestManagerSnapshotReturnsDefensiveCopies(t *testing.T) {
	t.Parallel()
	device := storage.Device{Path: "/dev/sda1", UUID: "data-uuid", MountPoints: []string{"/var/lib/teslabox"}}
	manager := NewManager(
		config.Config{},
		fakeDetector{device: device},
		fakeInspector{status: backing.Status{Ready: true, Missing: []string{"one"}}},
		fakeProber{environment: gadget.Environment{Ready: true, UDCNames: []string{"udc0"}}},
		slog.New(slog.NewTextHandler(io.Discard, nil)),
	)
	if err := manager.Refresh(t.Context()); err != nil {
		t.Fatal(err)
	}

	first := manager.Snapshot()
	first.DataDrive.MountPoints[0] = "changed"
	first.Backing.Missing[0] = "changed"
	first.Gadget.UDCNames[0] = "changed"
	second := manager.Snapshot()
	if second.DataDrive.MountPoints[0] != "/var/lib/teslabox" || second.Backing.Missing[0] != "one" || second.Gadget.UDCNames[0] != "udc0" {
		t.Fatalf("Snapshot() returned shared state: %#v", second)
	}
}
