package storageadmin

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/wen-ryon/teslabox/internal/backing"
	"github.com/wen-ryon/teslabox/internal/driveinit"
	"github.com/wen-ryon/teslabox/internal/exfatreserve"
	"github.com/wen-ryon/teslabox/internal/gadget"
	"github.com/wen-ryon/teslabox/internal/imageinit"
	"github.com/wen-ryon/teslabox/internal/service"
	"github.com/wen-ryon/teslabox/internal/storage"
	"github.com/wen-ryon/teslabox/internal/storagebench"
	"github.com/wen-ryon/teslabox/internal/storagepolicy"
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

type fakeImageInitializer struct {
	calls   int
	options imageinit.Options
	onRun   func(imageinit.Options)
}

type fakeStorageBenchmark struct{ calls int }

func (f *fakeStorageBenchmark) Run(context.Context, string) (storagebench.Report, error) {
	f.calls++
	return storagebench.Report{Profile: storagebench.ProfileStable, MinimumBurstMBps: 42}, nil
}

func (f *fakeImageInitializer) Run(_ context.Context, options imageinit.Options) (imageinit.Result, error) {
	f.calls++
	f.options = options
	if f.onRun != nil {
		f.onRun(options)
	}
	if err := os.WriteFile(options.ImagePath, []byte("image"), 0o640); err != nil {
		return imageinit.Result{}, err
	}
	return imageinit.Result{WasExecuted: true}, nil
}

type fakeController struct {
	state            service.Snapshot
	triggered        bool
	maintenanceErr   error
	maintenanceCalls int
}

func (f *fakeController) Snapshot() service.Snapshot { return f.state }
func (f *fakeController) TriggerRefresh()            { f.triggered = true }
func (f *fakeController) BeginStorageMaintenance(context.Context) (func(), error) {
	f.maintenanceCalls++
	if f.maintenanceErr != nil {
		return nil, f.maintenanceErr
	}
	return func() {}, nil
}
func (f *fakeController) BeginMediaMaintenance(context.Context) (func(), error) {
	f.maintenanceCalls++
	if f.maintenanceErr != nil {
		return nil, f.maintenanceErr
	}
	return func() {}, nil
}

type fakeRunner struct {
	statOutput string
}

func (f fakeRunner) Run(_ context.Context, name string, _ ...string) ([]byte, error) {
	if name == "blkid" {
		return []byte("new-uuid\n"), nil
	}
	if name == "stat" {
		if f.statOutput != "" {
			return []byte(f.statOutput), nil
		}
		// 初始化单元测试默认模拟一个空间充足的数据盘；容量边界由专门的表驱动用例覆盖。
		return []byte("4096:488281250:488281250\n"), nil
	}
	return nil, nil
}

type simulatedInitializationRunner struct {
	totalBytes     uint64
	initialFree    uint64
	allocatedBytes uint64
}

func (r *simulatedInitializationRunner) Run(_ context.Context, name string, _ ...string) ([]byte, error) {
	switch name {
	case "blkid":
		return []byte("simulated-uuid\n"), nil
	case "stat":
		free := r.initialFree
		if r.allocatedBytes < free {
			free -= r.allocatedBytes
		} else {
			free = 0
		}
		return []byte(fmt.Sprintf("1:%d:%d\n", r.totalBytes, free)), nil
	default:
		return nil, nil
	}
}

type capacityRunner struct {
	size         int64
	partitionEnd int64
	commands     []string
}

func (r *capacityRunner) Run(_ context.Context, name string, args ...string) ([]byte, error) {
	r.commands = append(r.commands, name)
	switch name {
	case "parted":
		if slices.Contains(args, "--machine") {
			end := r.partitionEnd
			if end == 0 {
				end = 95_000_000_000
			}
			return []byte(fmt.Sprintf("BYT;\nimage:%dB:file:512:512:msdos:;\n1:1048576B:%dB:%dB:::;\n", r.size, end, end-1048575)), nil
		}
	case "stat":
		return []byte("4096:31250000:5000000\n"), nil
	case "truncate":
		size, err := strconv.ParseInt(args[1], 10, 64)
		if err != nil {
			return nil, err
		}
		r.size = size
	case "losetup":
		return []byte("/dev/loop8\n"), nil
	}
	return nil, nil
}

func TestManagerShrinksExistingCapacityReserveAfterPreallocation(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	imagePath := filepath.Join(root, "teslacam.img")
	if err := os.WriteFile(imagePath, []byte("image"), 0o640); err != nil {
		t.Fatal(err)
	}
	registry := storage.NewFileRegistry(filepath.Join(root, "registry.json"))
	record := storage.DeviceRecord{StablePath: "/dev/disk/by-id/usb-ready", Serial: "SERIAL", SizeBytes: 128_000_000_000, UUID: "data-uuid", Status: "ready"}
	if err := registry.Begin(record); err != nil {
		t.Fatal(err)
	}
	if err := registry.Complete(record); err != nil {
		t.Fatal(err)
	}
	device := driveinit.Device{
		RequestedPath: record.StablePath, ResolvedPath: "/dev/sda", Type: "disk", Transport: "usb",
		Serial: record.Serial, Size: record.SizeBytes,
		Children: []driveinit.Device{{ResolvedPath: "/dev/sda1", FileSystem: "xfs", UUID: record.UUID, Label: "TESLABOX", MountPoint: root}},
	}
	runner := &capacityRunner{size: 128_000_000_000, partitionEnd: 128_000_000_000}
	manager := New(Config{DataRoot: root, ImagePath: imagePath, DataLabel: "TESLABOX"}, runner, registry, fakeLister{devices: []driveinit.Device{device}}, &fakeDriveInitializer{}, &fakeImageInitializer{}, &fakeController{})
	manager.fileSize = func(string) (int64, error) { return 128_000_000_000, nil }
	manager.verifyReserve = func(string) (exfatreserve.Status, error) {
		return exfatreserve.Status{VolumeBytes: 128_000_000_000, ReservedBytes: 20_000_000_000}, nil
	}
	manager.resizeReserve = func(_ string, boundary int64) (exfatreserve.Status, error) {
		if boundary != 118_000_000_000 || !slices.Contains(runner.commands, "fallocate") {
			t.Fatalf("resize boundary = %d, commands = %v", boundary, runner.commands)
		}
		return exfatreserve.Status{VolumeBytes: 128_000_000_000, ReservedBytes: 10_000_000_000}, nil
	}
	result, err := manager.MigrateAdvertisedCapacity(t.Context(), CapacityMigrationRequest{ConfirmOTGDisconnected: true})
	if err != nil {
		t.Fatal(err)
	}
	if !result.Changed || !result.FileSystemChecked || !result.RequiresColdBoot || result.WritableBytes != 118_000_000_000 {
		t.Fatalf("result = %#v", result)
	}
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
	benchmark := &fakeStorageBenchmark{}
	controller := &fakeController{state: service.Snapshot{Phase: service.PhaseWaitingForDataDrive}}
	manager := New(Config{
		DataRoot: root, ImagePath: filepath.Join(root, "images", "teslacam.img"),
		DataLabel: "TESLABOX", ImageLabel: "TESLACAM",
	}, fakeRunner{}, registry, fakeLister{devices: []driveinit.Device{device}}, driveInitializer, imageInitializer, controller, benchmark)

	result, err := manager.Initialize(t.Context(), InitializeRequest{
		StablePath: device.RequestedPath, ConfirmSerial: device.Serial, ConfirmErase: true, ConfirmOTGDisconnected: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !result.Active || !result.Initialized || driveInitializer.calls != 1 || imageInitializer.calls != 1 || benchmark.calls != 1 || !controller.triggered {
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
	status, err := manager.Status(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if status.Benchmark == nil || status.Benchmark.Profile != storagebench.ProfileStable || status.Benchmark.MinimumBurstMBps != 42 {
		t.Fatalf("benchmark status = %#v", status.Benchmark)
	}
}

func TestManagerRejectsLegacyCapacityMigrationThatCannotExpandExFAT(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	imagePath := filepath.Join(root, "teslacam.img")
	if err := os.WriteFile(imagePath, []byte("image"), 0o640); err != nil {
		t.Fatal(err)
	}
	registry := storage.NewFileRegistry(filepath.Join(root, "registry.json"))
	record := storage.DeviceRecord{StablePath: "/dev/disk/by-id/usb-ready", Serial: "SERIAL", SizeBytes: 128_000_000_000, UUID: "data-uuid", Status: "ready"}
	if err := registry.Begin(record); err != nil {
		t.Fatal(err)
	}
	if err := registry.Complete(record); err != nil {
		t.Fatal(err)
	}
	device := driveinit.Device{
		RequestedPath: record.StablePath, ResolvedPath: "/dev/sda", Type: "disk", Transport: "usb",
		Model: "USB", Serial: record.Serial, Size: record.SizeBytes,
		Children: []driveinit.Device{{ResolvedPath: "/dev/sda1", FileSystem: "xfs", UUID: record.UUID, Label: "TESLABOX", MountPoint: root}},
	}
	controller := &fakeController{state: service.Snapshot{Backing: backing.Status{Ready: true}}}
	runner := &capacityRunner{size: 96_000_000_000}
	manager := New(Config{DataRoot: root, ImagePath: imagePath, DataLabel: "TESLABOX"}, runner, registry, fakeLister{devices: []driveinit.Device{device}}, &fakeDriveInitializer{}, &fakeImageInitializer{}, controller)
	manager.fileSize = func(string) (int64, error) { return runner.size, nil }
	result, err := manager.MigrateAdvertisedCapacity(t.Context(), CapacityMigrationRequest{ConfirmOTGDisconnected: true})
	var inputError *InputError
	if !errors.As(err, &inputError) || !strings.Contains(inputError.Error(), "重新初始化") {
		t.Fatalf("MigrateAdvertisedCapacity() result = %#v, error = %v", result, err)
	}
	if runner.size != 96_000_000_000 {
		t.Fatalf("legacy image size changed to %d", runner.size)
	}
}

func TestManagerInitializeRejectsActiveGadgetBeforeDiskCommands(t *testing.T) {
	t.Parallel()
	registry := storage.NewFileRegistry(filepath.Join(t.TempDir(), "registry.json"))
	driveInitializer := &fakeDriveInitializer{root: t.TempDir()}
	device := driveinit.Device{
		RequestedPath: "/dev/disk/by-id/usb-test", ResolvedPath: "/dev/sda", Type: "disk",
		Transport: "usb", Model: "USB SSD", Serial: "SERIAL", Size: 128_000_000_000,
	}
	manager := New(Config{}, fakeRunner{}, registry, fakeLister{devices: []driveinit.Device{device}}, driveInitializer, &fakeImageInitializer{}, &fakeController{
		state:          service.Snapshot{GadgetStatus: gadgetBoundStatus()},
		maintenanceErr: errors.New("录像仍在使用数据盘"),
	})
	_, err := manager.Initialize(t.Context(), InitializeRequest{StablePath: "/dev/disk/by-id/usb-test", ConfirmSerial: "SERIAL", ConfirmErase: true, ConfirmOTGDisconnected: true})
	if err == nil {
		t.Fatal("Initialize() error = nil")
	}
	if driveInitializer.calls != 0 {
		t.Fatalf("drive initializer calls = %d", driveInitializer.calls)
	}
}

func TestManagerInitializeUsesFullCapacityAndMusicLayout(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	registry := storage.NewFileRegistry(filepath.Join(t.TempDir(), "registry.json"))
	device := driveinit.Device{
		RequestedPath: "/dev/disk/by-id/usb-500g", ResolvedPath: "/dev/sda", Type: "disk",
		Transport: "usb", Model: "USB SSD", Serial: "SERIAL-500", Size: 500_000_000_000,
	}
	imageInitializer := &fakeImageInitializer{}
	manager := New(Config{
		DataRoot: root, ImagePath: filepath.Join(root, "images", "teslacam.img"),
		DataLabel: "TESLABOX", ImageLabel: "TESLACAM",
	}, fakeRunner{}, registry, fakeLister{devices: []driveinit.Device{device}}, &fakeDriveInitializer{root: root}, imageInitializer, &fakeController{})

	result, err := manager.Initialize(t.Context(), InitializeRequest{
		StablePath: device.RequestedPath, ConfirmSerial: device.Serial, ConfirmErase: true,
		ConfirmOTGDisconnected: true, MusicEnabled: true, TeslaCamPercent: 80,
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.PlannedImageSizeBytes != 500_000_000_000 || result.PlannedWritableBytes != 475_000_000_000 {
		t.Fatalf("planned capacities = %d/%d", result.PlannedImageSizeBytes, result.PlannedWritableBytes)
	}
	if imageInitializer.options.SizeBytes != 500_000_000_000 || imageInitializer.options.WritableBytes != 475_000_000_000 || !imageInitializer.options.MusicEnabled || imageInitializer.options.TeslaCamPercent != 80 || imageInitializer.options.MusicLabel != "MUSIC" {
		t.Fatalf("image options = %#v", imageInitializer.options)
	}
}

func TestManagerInitializePreservesSnapshotSpaceAcrossDriveSizes(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name        string
		deviceBytes int64
		xfsTotal    uint64
		xfsFree     uint64
	}{
		{
			name:        "512 GB drive",
			deviceBytes: 512_110_190_592,
			xfsTotal:    512_000_000_000,
			xfsFree:     508_419_558_400,
		},
		{
			name:        "1 TB drive",
			deviceBytes: 1_000_204_886_016,
			xfsTotal:    1_000_000_000_000,
			xfsFree:     995_000_000_000,
		},
		{
			name:        "2 TB drive",
			deviceBytes: maximumSupportedDeviceSize,
			xfsTotal:    2_000_000_000_000,
			xfsFree:     1_992_000_000_000,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			root := t.TempDir()
			device := driveinit.Device{
				RequestedPath: "/dev/disk/by-id/usb-simulated", ResolvedPath: "/dev/sda", Type: "disk",
				Transport: "usb", Model: test.name, Serial: "SIMULATED", Size: test.deviceBytes,
			}
			runner := &simulatedInitializationRunner{totalBytes: test.xfsTotal, initialFree: test.xfsFree}
			imageInitializer := &fakeImageInitializer{}
			imageInitializer.onRun = func(options imageinit.Options) {
				runner.allocatedBytes = uint64(options.WritableBytes)
			}
			policy := storagepolicy.DefaultSnapshotReservePolicy()
			manager := New(Config{
				DataRoot: root, ImagePath: filepath.Join(root, "images", "teslacam.img"),
				DataLabel: "TESLABOX", ImageLabel: "TESLACAM",
				Policy: policy,
			}, runner, storage.NewFileRegistry(filepath.Join(t.TempDir(), "registry.json")), fakeLister{devices: []driveinit.Device{device}}, &fakeDriveInitializer{root: root}, imageInitializer, &fakeController{})

			result, err := manager.Initialize(t.Context(), InitializeRequest{
				StablePath: device.RequestedPath, ConfirmSerial: device.Serial,
				ConfirmErase: true, ConfirmOTGDisconnected: true,
			})
			if err != nil {
				t.Fatalf("Initialize() error = %v", err)
			}
			remaining := runner.initialFree - runner.allocatedBytes
			required, requiredErr := policy.InitializationRequiredBytes(runner.totalBytes)
			if requiredErr != nil {
				t.Fatal(requiredErr)
			}
			if remaining < required {
				t.Fatalf("remaining bytes = %d, want at least %d", remaining, required)
			}
			legacyWritable, legacyErr := chooseImageSize(device.Size)
			if legacyErr != nil {
				t.Fatal(legacyErr)
			}
			legacyRemaining := runner.initialFree - uint64(legacyWritable)
			snapshotMinimum, minimumErr := policy.RuntimeRequiredBytes(runner.totalBytes)
			if minimumErr != nil {
				t.Fatal(minimumErr)
			}
			if legacyRemaining >= snapshotMinimum {
				t.Fatalf("simulation no longer reproduces old failure: legacy remaining %d, minimum %d", legacyRemaining, snapshotMinimum)
			}
			if result.PlannedWritableBytes != imageInitializer.options.WritableBytes || result.PlannedWritableBytes <= 0 {
				t.Fatalf("planned writable bytes = %d, image options = %d", result.PlannedWritableBytes, imageInitializer.options.WritableBytes)
			}
			if result.PlannedImageSizeBytes > maximumSupportedDeviceSize {
				t.Fatalf("advertised bytes = %d, exceeds MBR limit", result.PlannedImageSizeBytes)
			}
			t.Logf("advertised=%d writable=%d remaining=%d required=%d", result.PlannedImageSizeBytes, result.PlannedWritableBytes, remaining, required)
		})
	}
}

func TestManagerInitializeRejectsSpaceThatCannotPreserveSnapshotReserve(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	device := driveinit.Device{
		RequestedPath: "/dev/disk/by-id/usb-small-free", ResolvedPath: "/dev/sda", Type: "disk",
		Transport: "usb", Model: "Space constrained", Serial: "SMALL-FREE", Size: 128_000_000_000,
	}
	runner := &simulatedInitializationRunner{totalBytes: 128_000_000_000, initialFree: 44_000_000_000}
	imageInitializer := &fakeImageInitializer{}
	manager := New(Config{
		DataRoot: root, ImagePath: filepath.Join(root, "images", "teslacam.img"),
		DataLabel: "TESLABOX", ImageLabel: "TESLACAM",
	}, runner, storage.NewFileRegistry(filepath.Join(t.TempDir(), "registry.json")), fakeLister{devices: []driveinit.Device{device}}, &fakeDriveInitializer{root: root}, imageInitializer, &fakeController{})

	_, err := manager.Initialize(t.Context(), InitializeRequest{
		StablePath: device.RequestedPath, ConfirmSerial: device.Serial,
		ConfirmErase: true, ConfirmOTGDisconnected: true,
	})
	if err == nil || !strings.Contains(err.Error(), "保留快照安全空间") {
		t.Fatalf("Initialize() error = %v", err)
	}
	if imageInitializer.calls != 0 {
		t.Fatalf("image initializer calls = %d, want 0", imageInitializer.calls)
	}
}

func TestManagerInitializeRejectsPostAllocationSnapshotSpaceRegression(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	device := driveinit.Device{
		RequestedPath: "/dev/disk/by-id/usb-overallocated", ResolvedPath: "/dev/sda", Type: "disk",
		Transport: "usb", Model: "Overallocated", Serial: "OVERALLOCATED", Size: 512_110_190_592,
	}
	runner := &simulatedInitializationRunner{totalBytes: 512_000_000_000, initialFree: 508_419_558_400}
	imageInitializer := &fakeImageInitializer{}
	imageInitializer.onRun = func(options imageinit.Options) {
		// 模拟底层分配结果额外消耗安全余量，确保后置校验阻止错误登记为初始化完成。
		runner.allocatedBytes = uint64(options.WritableBytes) + storagepolicy.DefaultInitializationHeadroomBytes
	}
	registry := storage.NewFileRegistry(filepath.Join(t.TempDir(), "registry.json"))
	manager := New(Config{
		DataRoot: root, ImagePath: filepath.Join(root, "images", "teslacam.img"),
		DataLabel: "TESLABOX", ImageLabel: "TESLACAM",
	}, runner, registry, fakeLister{devices: []driveinit.Device{device}}, &fakeDriveInitializer{root: root}, imageInitializer, &fakeController{})

	_, err := manager.Initialize(t.Context(), InitializeRequest{
		StablePath: device.RequestedPath, ConfirmSerial: device.Serial,
		ConfirmErase: true, ConfirmOTGDisconnected: true,
	})
	if err == nil || !strings.Contains(err.Error(), "初始化后快照空间不足") {
		t.Fatalf("Initialize() error = %v", err)
	}
	if imageInitializer.calls != 1 {
		t.Fatalf("image initializer calls = %d, want 1", imageInitializer.calls)
	}
	if _, exists, activeErr := registry.Active(); activeErr != nil || exists {
		t.Fatalf("registry active exists = %t, error = %v", exists, activeErr)
	}
}

func TestManagerInitializeAcceptsNVMeCandidate(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	registry := storage.NewFileRegistry(filepath.Join(t.TempDir(), "registry.json"))
	device := driveinit.Device{
		RequestedPath: "/dev/disk/by-id/nvme-Samsung_SSD_990_PRO_S7ABC123", ResolvedPath: "/dev/nvme0n1", Type: "disk",
		Transport: "nvme", Model: "Samsung SSD 990 PRO", Serial: "S7ABC123", Size: 1_000_000_000_000,
	}
	driveInitializer := &fakeDriveInitializer{root: root}
	manager := New(Config{
		DataRoot: root, ImagePath: filepath.Join(root, "images", "teslacam.img"),
		DataLabel: "TESLABOX", ImageLabel: "TESLACAM",
	}, fakeRunner{}, registry, fakeLister{devices: []driveinit.Device{device}}, driveInitializer, &fakeImageInitializer{}, &fakeController{})

	result, err := manager.Initialize(t.Context(), InitializeRequest{
		StablePath: device.RequestedPath, ConfirmSerial: device.Serial, ConfirmErase: true, ConfirmOTGDisconnected: true,
	})
	if err != nil {
		t.Fatalf("Initialize() error = %v", err)
	}
	if !result.Initialized || result.Transport != "nvme" || driveInitializer.calls != 1 {
		t.Fatalf("result = %#v, drive calls = %d", result, driveInitializer.calls)
	}
}

func TestClassifyReadyManagedDeviceAllowsReinitialize(t *testing.T) {
	t.Parallel()
	device := driveinit.Device{
		RequestedPath: "/dev/disk/by-id/usb-ready", ResolvedPath: "/dev/sda", Type: "disk",
		Transport: "usb", Model: "USB SSD", Serial: "SERIAL-READY", Size: 500_000_000_000,
		Children: []driveinit.Device{{ResolvedPath: "/dev/sda1", FileSystem: "xfs", UUID: "ready-uuid", Label: "TESLABOX", MountPoint: "/var/lib/teslabox"}},
	}
	record := storage.DeviceRecord{StablePath: device.RequestedPath, Serial: device.Serial, SizeBytes: device.Size, UUID: "ready-uuid", Status: "ready"}
	candidate := classify(device, []storage.DeviceRecord{record}, record, true, "/var/lib/teslabox", "TESLABOX", storagepolicy.DefaultSnapshotReservePolicy())
	if !candidate.Initialized || !candidate.Active || !candidate.CanReinitialize || candidate.PlannedImageSizeBytes != 500_000_000_000 || candidate.PlannedWritableBytes != 475_000_000_000 {
		t.Fatalf("candidate = %#v", candidate)
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
		{name: "128 GB uses remaining capacity", deviceSize: 128_000_000_000, want: 118_000_000_000},
		{name: "500 GB keeps five percent reserve", deviceSize: 500_000_000_000, want: 475_000_000_000},
		{name: "64 GB reserve", deviceSize: 64_000_000_000, want: 54_000_000_000},
		{name: "exactly 2 TB remains supported", deviceSize: maximumSupportedDeviceSize, want: 1_900_000_000_000},
		{name: "one byte above 2 TB is rejected", deviceSize: maximumSupportedDeviceSize + 1, wantErr: true},
		{name: "40 GB is below usable layout", deviceSize: 40_000_000_000, wantErr: true},
		{name: "32 GB minimum cannot preserve writable layout", deviceSize: 32_000_000_000, wantErr: true},
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

func TestClassifyUsesSharedPolicyForSmallDrivePrecheck(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name          string
		size          int64
		wantSupported bool
	}{
		{name: "40 GB rejected before erase", size: 40_000_000_000},
		{name: "64 GB remains engineering compatible", size: 64_000_000_000, wantSupported: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			device := driveinit.Device{RequestedPath: "/dev/disk/by-id/usb-test", ResolvedPath: "/dev/sda", Type: "disk", Transport: "usb", Serial: "SERIAL", Size: test.size}
			candidate := classify(device, nil, storage.DeviceRecord{}, false, "/var/lib/teslabox", "TESLABOX", storagepolicy.DefaultSnapshotReservePolicy())
			if candidate.CanInitialize != test.wantSupported {
				t.Fatalf("candidate = %+v, want supported = %t", candidate, test.wantSupported)
			}
		})
	}
}

func TestClassifyRejectsDrivesAboveTwoTB(t *testing.T) {
	t.Parallel()
	device := driveinit.Device{
		RequestedPath: "/dev/disk/by-id/usb-4tb", ResolvedPath: "/dev/sda", Type: "disk",
		Transport: "usb", Serial: "SERIAL-4TB", Size: 4_000_000_000_000,
	}
	tests := []struct {
		name    string
		records []storage.DeviceRecord
		active  storage.DeviceRecord
	}{
		{name: "new device"},
		{
			name: "previously registered device",
			records: []storage.DeviceRecord{{
				StablePath: device.RequestedPath, Serial: device.Serial, SizeBytes: device.Size, Status: "ready",
			}},
			active: storage.DeviceRecord{StablePath: device.RequestedPath, Serial: device.Serial, SizeBytes: device.Size, Status: "ready"},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			candidate := classify(device, test.records, test.active, len(test.records) > 0, "/var/lib/teslabox", "TESLABOX", storagepolicy.DefaultSnapshotReservePolicy())
			if candidate.State != "unsupported" || candidate.CanInitialize || candidate.CanReinitialize || candidate.CanResume {
				t.Fatalf("candidate = %+v", candidate)
			}
			if !strings.Contains(candidate.Reason, "大于 2TB") {
				t.Fatalf("reason = %q", candidate.Reason)
			}
		})
	}
}

func TestManagerInitializeRejectsDriveAboveTwoTBBeforeMaintenance(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	device := driveinit.Device{
		RequestedPath: "/dev/disk/by-id/usb-4tb", ResolvedPath: "/dev/sda", Type: "disk",
		Transport: "usb", Serial: "SERIAL-4TB", Size: 4_000_000_000_000,
	}
	driveInitializer := &fakeDriveInitializer{root: root}
	imageInitializer := &fakeImageInitializer{}
	controller := &fakeController{}
	manager := New(Config{
		DataRoot: root, ImagePath: filepath.Join(root, "images", "teslacam.img"),
		DataLabel: "TESLABOX", ImageLabel: "TESLACAM",
	}, fakeRunner{}, storage.NewFileRegistry(filepath.Join(t.TempDir(), "registry.json")), fakeLister{devices: []driveinit.Device{device}}, driveInitializer, imageInitializer, controller)

	_, err := manager.Initialize(t.Context(), InitializeRequest{
		StablePath: device.RequestedPath, ConfirmSerial: device.Serial,
		ConfirmErase: true, ConfirmOTGDisconnected: true,
	})
	if err == nil || !strings.Contains(err.Error(), "大于 2TB") {
		t.Fatalf("Initialize() error = %v", err)
	}
	if driveInitializer.calls != 0 || imageInitializer.calls != 0 || controller.maintenanceCalls != 0 {
		t.Fatalf("destructive calls: drive=%d image=%d maintenance=%d", driveInitializer.calls, imageInitializer.calls, controller.maintenanceCalls)
	}
}

func TestChooseAdvertisedSizeUsesRealSectorAlignedCapacity(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name       string
		deviceSize int64
		want       int64
		wantErr    bool
	}{
		{name: "sector aligned real capacity", deviceSize: 123_924_905_999, want: 123_924_905_984},
		{name: "exactly 2 TB remains supported", deviceSize: maximumSupportedDeviceSize, want: maximumSupportedDeviceSize},
		{name: "one byte above 2 TB is rejected", deviceSize: maximumSupportedDeviceSize + 1, wantErr: true},
		{name: "4 TB is rejected instead of truncated", deviceSize: 4_000_000_000_000, wantErr: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got, err := chooseAdvertisedSize(test.deviceSize)
			if (err != nil) != test.wantErr || got != test.want {
				t.Fatalf("chooseAdvertisedSize() = %d, %v", got, err)
			}
			if err == nil && got%512 != 0 {
				t.Fatalf("advertised size %d is not sector aligned", got)
			}
		})
	}
}

type pathInspector struct{ devices map[string]driveinit.Device }

func (i pathInspector) Inspect(_ context.Context, path string) (driveinit.Device, error) {
	device, found := i.devices[path]
	if !found {
		return driveinit.Device{}, errors.New("not found")
	}
	return device, nil
}

func TestOSDeviceListerListsNVMeAndSkipsSystemDisk(t *testing.T) {
	t.Parallel()
	lister := NewOSDeviceLister(pathInspector{devices: map[string]driveinit.Device{
		"/dev/disk/by-id/mmc-boot": {
			RequestedPath: "/dev/disk/by-id/mmc-boot", ResolvedPath: "/dev/mmcblk0", Type: "disk", Serial: "BOOT", Size: 64_000_000_000,
			Children: []driveinit.Device{{ResolvedPath: "/dev/mmcblk0p2", MountPoint: "/"}},
		},
		"/dev/disk/by-id/nvme-Samsung_SSD_990_PRO_S7ABC123": {
			RequestedPath: "/dev/disk/by-id/nvme-Samsung_SSD_990_PRO_S7ABC123", ResolvedPath: "/dev/nvme0n1", Type: "disk", Transport: "nvme", Serial: "S7ABC123", Size: 1_000_000_000_000,
		},
		"/dev/disk/by-id/usb-TESLADRIVE_ABC-0:0": {
			RequestedPath: "/dev/disk/by-id/usb-TESLADRIVE_ABC-0:0", ResolvedPath: "/dev/sda", Type: "disk", Transport: "usb", Serial: "ABC", Size: 128_000_000_000,
		},
		"/dev/disk/by-id/wwn-0xABC": {
			RequestedPath: "/dev/disk/by-id/wwn-0xABC", ResolvedPath: "/dev/sda", Type: "disk", Transport: "usb", Serial: "ABC", Size: 128_000_000_000,
		},
		"/dev/disk/by-id/nvme-Samsung_SSD_990_PRO_S7ABC123-part1": {
			RequestedPath: "/dev/disk/by-id/nvme-Samsung_SSD_990_PRO_S7ABC123-part1", ResolvedPath: "/dev/nvme0n1p1", Type: "part",
		},
	}})
	lister.stablePaths = func() ([]string, error) {
		return []string{
			"/dev/disk/by-id/wwn-0xABC",
			"/dev/disk/by-id/mmc-boot",
			"/dev/disk/by-id/nvme-Samsung_SSD_990_PRO_S7ABC123-part1",
			"/dev/disk/by-id/nvme-Samsung_SSD_990_PRO_S7ABC123",
			"/dev/disk/by-id/usb-TESLADRIVE_ABC-0:0",
		}, nil
	}

	devices, err := lister.List(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if len(devices) != 2 {
		t.Fatalf("devices = %#v", devices)
	}
	if devices[0].RequestedPath != "/dev/disk/by-id/usb-TESLADRIVE_ABC-0:0" || devices[1].Transport != "nvme" {
		t.Fatalf("devices = %#v", devices)
	}
}
