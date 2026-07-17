package storageadmin

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/wen-ryon/teslabox/internal/driveinit"
	"github.com/wen-ryon/teslabox/internal/gadget"
	"github.com/wen-ryon/teslabox/internal/imageinit"
	"github.com/wen-ryon/teslabox/internal/service"
	"github.com/wen-ryon/teslabox/internal/storage"
)

type fakeLister struct{ devices []driveinit.Device }

func (f fakeLister) List(context.Context) ([]driveinit.Device, error) { return f.devices, nil }

type fakeDriveInitializer struct {
	root  string
	calls int
}

func (f *fakeDriveInitializer) Run(_ context.Context, _ driveinit.Options) (driveinit.Result, error) {
	f.calls++
	for _, directory := range []string{"images", "snapshots", "index", "state"} {
		if err := os.MkdirAll(filepath.Join(f.root, directory), 0o750); err != nil {
			return driveinit.Result{}, err
		}
	}
	return driveinit.Result{Partition: "/dev/sda1", WasExecuted: true}, nil
}

type fakeImageInitializer struct{ calls int }

func (f *fakeImageInitializer) Run(_ context.Context, options imageinit.Options) (imageinit.Result, error) {
	f.calls++
	if err := os.WriteFile(options.ImagePath, []byte("image"), 0o640); err != nil {
		return imageinit.Result{}, err
	}
	return imageinit.Result{WasExecuted: true}, nil
}

type fakeController struct {
	state     service.Snapshot
	triggered bool
}

func (f *fakeController) Snapshot() service.Snapshot { return f.state }
func (f *fakeController) TriggerRefresh()            { f.triggered = true }

type fakeRunner struct{}

func (fakeRunner) Run(_ context.Context, name string, _ ...string) ([]byte, error) {
	if name == "blkid" {
		return []byte("new-uuid\n"), nil
	}
	return nil, nil
}

func TestManagerInitializeRegistersOnlyAfterCompleteLayout(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	registryPath := filepath.Join(t.TempDir(), "registry.json")
	registry := storage.NewFileRegistry(registryPath)
	device := driveinit.Device{
		RequestedPath: "/dev/disk/by-id/usb-test", ResolvedPath: "/dev/sda", Type: "disk",
		Transport: "usb", Model: "USB SSD", Serial: "SERIAL-1", Size: 128_000_000_000,
	}
	driveInitializer := &fakeDriveInitializer{root: root}
	imageInitializer := &fakeImageInitializer{}
	controller := &fakeController{state: service.Snapshot{Phase: service.PhaseWaitingForDataDrive}}
	manager := New(Config{
		DataRoot: root, ImagePath: filepath.Join(root, "images", "teslacam.img"),
		DataLabel: "TESLABOX", ImageLabel: "TESLACAM",
	}, fakeRunner{}, registry, fakeLister{devices: []driveinit.Device{device}}, driveInitializer, imageInitializer, controller)

	result, err := manager.Initialize(t.Context(), InitializeRequest{
		StablePath: device.RequestedPath, ConfirmSerial: device.Serial, ConfirmErase: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !result.Active || !result.Initialized || driveInitializer.calls != 1 || imageInitializer.calls != 1 || !controller.triggered {
		t.Fatalf("result = %#v, drive calls = %d, image calls = %d, triggered = %t", result, driveInitializer.calls, imageInitializer.calls, controller.triggered)
	}
	active, exists, err := registry.Active()
	if err != nil {
		t.Fatal(err)
	}
	if !exists || active.UUID != "new-uuid" || active.Serial != device.Serial {
		t.Fatalf("active = %#v, exists = %t", active, exists)
	}
	if _, err := os.Stat(filepath.Join(root, "state", "device.json")); err != nil {
		t.Fatalf("device marker: %v", err)
	}
}

func TestManagerInitializeRejectsActiveGadgetBeforeDiskCommands(t *testing.T) {
	t.Parallel()
	registry := storage.NewFileRegistry(filepath.Join(t.TempDir(), "registry.json"))
	driveInitializer := &fakeDriveInitializer{root: t.TempDir()}
	manager := New(Config{}, fakeRunner{}, registry, fakeLister{}, driveInitializer, &fakeImageInitializer{}, &fakeController{
		state: service.Snapshot{GadgetStatus: gadgetBoundStatus()},
	})
	_, err := manager.Initialize(t.Context(), InitializeRequest{StablePath: "/dev/disk/by-id/usb-test", ConfirmSerial: "SERIAL", ConfirmErase: true})
	if err == nil {
		t.Fatal("Initialize() error = nil")
	}
	if driveInitializer.calls != 0 {
		t.Fatalf("drive initializer calls = %d", driveInitializer.calls)
	}
}

func TestManagerCancelMissingPendingDeviceRestoresActiveRegistry(t *testing.T) {
	t.Parallel()
	registry := storage.NewFileRegistry(filepath.Join(t.TempDir(), "registry.json"))
	if err := registry.EnsureLegacy("old-uuid", "TESLABOX"); err != nil {
		t.Fatal(err)
	}
	pending := storage.DeviceRecord{
		StablePath: "/dev/disk/by-id/usb-new", Serial: "SERIAL-NEW",
		Model: "USB SSD", SizeBytes: 128_000_000_000, Label: "TESLABOX",
	}
	if err := registry.Begin(pending); err != nil {
		t.Fatal(err)
	}
	controller := &fakeController{}
	manager := New(Config{DataRoot: t.TempDir(), DataLabel: "TESLABOX"}, fakeRunner{}, registry, fakeLister{}, &fakeDriveInitializer{}, &fakeImageInitializer{}, controller)
	status, err := manager.Status(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if len(status.Devices) != 1 || !status.Devices[0].CanCancel || status.Devices[0].StablePath != pending.StablePath {
		t.Fatalf("status = %#v", status)
	}
	if err := manager.Cancel(t.Context(), CancelRequest{StablePath: pending.StablePath, ConfirmSerial: pending.Serial}); err != nil {
		t.Fatal(err)
	}
	active, exists, err := registry.Active()
	if err != nil {
		t.Fatal(err)
	}
	if !exists || active.UUID != "old-uuid" || !controller.triggered {
		t.Fatalf("active = %#v, exists = %t, triggered = %t", active, exists, controller.triggered)
	}
}

func gadgetBoundStatus() gadget.Status { return gadget.Status{Bound: true} }

func TestChooseImageSize(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name       string
		deviceSize int64
		want       int64
		wantErr    bool
	}{
		{name: "128 GB cap", deviceSize: 128_000_000_000, want: 96_000_000_000},
		{name: "64 GB reserve", deviceSize: 64_000_000_000, want: 44_000_000_000},
		{name: "too small", deviceSize: 40_000_000_000, wantErr: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got, err := chooseImageSize(test.deviceSize)
			if (err != nil) != test.wantErr || got != test.want {
				t.Fatalf("chooseImageSize() = %d, %v", got, err)
			}
		})
	}
}
