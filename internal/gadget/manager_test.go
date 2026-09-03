package gadget

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/wen-ryon/teslabox/internal/exfatreserve"
)

func testManager(t *testing.T) (*Manager, string) {
	t.Helper()
	root := t.TempDir()
	configFS := filepath.Join(root, "usb_gadget")
	udcPath := filepath.Join(root, "udc")
	imagePath := filepath.Join(root, "teslacam.img")
	if err := os.MkdirAll(configFS, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(udcPath, "fcc00000.dwc3"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(udcPath, "fcc00000.dwc3", "state"), []byte("configured\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(udcPath, "fcc00000.dwc3", "current_speed"), []byte("high-speed\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	suspendPath := filepath.Join(udcPath, "fcc00000.dwc3", "device", "gadget")
	if err := os.MkdirAll(suspendPath, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(suspendPath, "suspended"), []byte("0\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(imagePath, []byte("image"), 0o600); err != nil {
		t.Fatal(err)
	}
	manager := NewManager(Config{
		ConfigFSPath: configFS,
		UDCPath:      udcPath,
		GadgetName:   "teslabox",
		ImagePath:    imagePath,
		SerialNumber: "TESLABOX-TEST",
	})
	manager.symlink = func(oldname, newname string) error {
		return os.WriteFile(newname, []byte(oldname), 0o644)
	}
	manager.sleep = func(time.Duration) {}
	manager.verifyCapacityReserve = func(string) (exfatreserve.Status, error) {
		return exfatreserve.Status{Legacy: true}, nil
	}
	return manager, imagePath
}

func TestManagerReportsEnumerationWithoutClaimingPhysicalCableState(t *testing.T) {
	t.Parallel()
	manager, _ := testManager(t)

	status, err := manager.Inspect()
	if err != nil {
		t.Fatal(err)
	}
	if !status.EnumerationComplete || !status.HostConnected || status.UDCState != "configured" {
		t.Fatalf("status = %#v", status)
	}
}

func TestSamePathRecognizesFilesystemAliases(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	original := filepath.Join(root, "teslacam.img")
	alias := filepath.Join(root, "teslacam-alias.img")
	if err := os.WriteFile(original, []byte("image"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Link(original, alias); err != nil {
		t.Skipf("当前文件系统不支持硬链接: %v", err)
	}
	if !samePath(original, alias) {
		t.Fatalf("samePath(%q, %q) = false", original, alias)
	}
}

func TestManagerStartCreatesAndBindsMassStorage(t *testing.T) {
	manager, imagePath := testManager(t)
	status, err := manager.Start()
	if err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	if !status.Exists || !status.Bound || status.UDC != "fcc00000.dwc3" {
		t.Fatalf("status = %#v", status)
	}
	if status.UDCState != "configured" || status.CurrentSpeed != "high-speed" || !status.HostDetected || !status.HostConnected || !status.EnumerationComplete || !status.BusSuspendKnown || status.BusSuspended {
		t.Fatalf("UDC status = %#v", status)
	}
	if !samePath(status.ImagePath, imagePath) {
		t.Fatalf("image path = %q, want %q", status.ImagePath, imagePath)
	}
	if _, err := os.Lstat(filepath.Join(manager.gadgetPath(), "configs", "c.1", "mass_storage.0")); err != nil {
		t.Fatalf("mass storage link: %v", err)
	}
	nofua, err := os.ReadFile(manager.lunPath("nofua"))
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.TrimSpace(string(nofua)); got != "1" {
		t.Fatalf("nofua = %q, want %q", got, "1")
	}
	stall, err := os.ReadFile(filepath.Join(manager.gadgetPath(), "functions", "mass_storage.0", "stall"))
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.TrimSpace(string(stall)); got != "0" {
		t.Fatalf("stall = %q, want %q", got, "0")
	}
}

func TestManagerRefusesGadgetWhenCapacityReserveIsInvalid(t *testing.T) {
	t.Parallel()
	manager, _ := testManager(t)
	manager.verifyCapacityReserve = func(string) (exfatreserve.Status, error) {
		return exfatreserve.Status{}, errors.New("reserve missing")
	}

	_, err := manager.Start()
	if err == nil || !strings.Contains(err.Error(), "安全可写容量") {
		t.Fatalf("Start() error = %v", err)
	}
	if _, statErr := os.Stat(manager.gadgetPath()); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("gadget path must not be created, stat error = %v", statErr)
	}
}

func TestManagerReportsConfiguredSuspendedBusAsAmbiguous(t *testing.T) {
	t.Parallel()
	manager, _ := testManager(t)
	suspendedPath := filepath.Join(manager.config.UDCPath, "fcc00000.dwc3", "device", "gadget", "suspended")
	if err := os.WriteFile(suspendedPath, []byte("1\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	status, err := manager.Inspect()
	if err != nil {
		t.Fatal(err)
	}
	if !status.EnumerationComplete || !status.BusSuspendKnown || !status.BusSuspended {
		t.Fatalf("status = %#v", status)
	}
}

func TestManagerOnlyReportsConfiguredHostAsConnected(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name          string
		state         string
		wantDetected  bool
		wantConnected bool
	}{
		{name: "not attached", state: "not attached", wantDetected: false, wantConnected: false},
		{name: "powered", state: "powered", wantDetected: true, wantConnected: false},
		{name: "addressed", state: "addressed", wantDetected: true, wantConnected: false},
		{name: "configured", state: "configured", wantDetected: true, wantConnected: true},
		{name: "suspended after configured", state: "suspended", wantDetected: true, wantConnected: false},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			manager, _ := testManager(t)
			statePath := filepath.Join(manager.config.UDCPath, "fcc00000.dwc3", "state")
			if err := os.WriteFile(statePath, []byte(test.state+"\n"), 0o644); err != nil {
				t.Fatal(err)
			}
			status, err := manager.Inspect()
			if err != nil {
				t.Fatal(err)
			}
			if status.HostDetected != test.wantDetected || status.HostConnected != test.wantConnected {
				t.Fatalf("status = %#v", status)
			}
			if status.EnumerationComplete != test.wantConnected {
				t.Fatalf("EnumerationComplete = %t, want %t", status.EnumerationComplete, test.wantConnected)
			}
		})
	}
}

func TestManagerStartIsIdempotent(t *testing.T) {
	manager, _ := testManager(t)
	first, err := manager.Start()
	if err != nil {
		t.Fatalf("first Start() error = %v", err)
	}
	second, err := manager.Start()
	if err != nil {
		t.Fatalf("second Start() error = %v", err)
	}
	if first.UDC != second.UDC || !second.Bound {
		t.Fatalf("second status = %#v", second)
	}
}

func TestManagerStopOnlyRemovesTeslaBoxGadget(t *testing.T) {
	manager, _ := testManager(t)
	other := filepath.Join(manager.config.ConfigFSPath, "radxa-otgutils")
	if err := os.MkdirAll(other, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(other, "UDC"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := manager.Start(); err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	status, err := manager.Stop()
	if err != nil {
		t.Fatalf("Stop() error = %v", err)
	}
	if status.Exists || status.Bound {
		t.Fatalf("status = %#v", status)
	}
	if _, err := os.Stat(other); err != nil {
		t.Fatalf("other gadget was removed: %v", err)
	}
}

func TestManagerStopRemovesOwnedGadgetWhenUDCDisappears(t *testing.T) {
	manager, _ := testManager(t)
	if _, err := manager.Start(); err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	udcPath := filepath.Join(manager.config.UDCPath, "fcc00000.dwc3")
	if err := os.RemoveAll(udcPath); err != nil {
		t.Fatal(err)
	}

	status, err := manager.Stop()
	if err != nil {
		t.Fatalf("Stop() error = %v", err)
	}
	if status.Exists || status.Bound {
		t.Fatalf("status = %#v", status)
	}
	if _, err := os.Stat(manager.gadgetPath()); !os.IsNotExist(err) {
		t.Fatalf("TeslaBox gadget should be removed, stat error = %v", err)
	}
}

func TestManagerStopDoesNotDependOnReadableBackingAttribute(t *testing.T) {
	manager, _ := testManager(t)
	if _, err := manager.Start(); err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	filePath := manager.lunPath("file")
	if err := os.Remove(filePath); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filePath, 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := manager.Inspect(); err == nil {
		t.Fatal("Inspect() error = nil, want unreadable backing attribute")
	}
	status, err := manager.Stop()
	if err != nil {
		t.Fatalf("Stop() error = %v", err)
	}
	if status.Exists || status.Bound {
		t.Fatalf("status = %#v", status)
	}
}

func TestManagerRejectsOtherBinding(t *testing.T) {
	manager, _ := testManager(t)
	other := filepath.Join(manager.config.ConfigFSPath, "radxa-otgutils")
	if err := os.MkdirAll(other, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(other, "UDC"), []byte("fcc00000.dwc3\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := manager.Start(); err == nil {
		t.Fatal("Start() error = nil, want occupied UDC error")
	}
	if _, err := os.Stat(manager.gadgetPath()); !os.IsNotExist(err) {
		t.Fatalf("TeslaBox gadget should not exist, stat error = %v", err)
	}
}

func TestManagerExplicitlyReleasesButPreservesAllowedGadget(t *testing.T) {
	manager, _ := testManager(t)
	manager.config.ReleaseGadget = "radxa-otgutils"
	var settledFor time.Duration
	manager.sleep = func(duration time.Duration) { settledFor = duration }
	other := filepath.Join(manager.config.ConfigFSPath, "radxa-otgutils")
	if err := os.MkdirAll(other, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(other, "UDC"), []byte("fcc00000.dwc3\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	status, err := manager.Start()
	if err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	if !status.Bound {
		t.Fatalf("status = %#v", status)
	}
	value, err := os.ReadFile(filepath.Join(other, "UDC"))
	if err != nil {
		t.Fatalf("allowed gadget was removed: %v", err)
	}
	if string(value) != "\n" {
		t.Fatalf("allowed gadget UDC = %q, want unbound", value)
	}
	if settledFor != hostDisconnectSettleTime {
		t.Fatalf("settle delay = %s, want %s", settledFor, hostDisconnectSettleTime)
	}
}

func TestManagerReleasesAllowedBindingBeforeBackingStoreInitialization(t *testing.T) {
	manager, imagePath := testManager(t)
	manager.config.ReleaseGadget = "radxa-otgutils"
	if err := os.Remove(imagePath); err != nil {
		t.Fatal(err)
	}
	other := filepath.Join(manager.config.ConfigFSPath, "radxa-otgutils")
	if err := os.MkdirAll(other, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(other, "UDC"), []byte("fcc00000.dwc3\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	released, err := manager.ReleaseAllowedBinding()
	if err != nil {
		t.Fatal(err)
	}
	if !released {
		t.Fatal("released = false")
	}
	value, err := os.ReadFile(filepath.Join(other, "UDC"))
	if err != nil {
		t.Fatal(err)
	}
	if string(value) != "\n" {
		t.Fatalf("allowed gadget UDC = %q, want unbound", value)
	}
}

func TestValidateImageRejectsSymlink(t *testing.T) {
	root := t.TempDir()
	target := filepath.Join(root, "target.img")
	link := filepath.Join(root, "link.img")
	if err := os.WriteFile(target, []byte("image"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, link); err != nil {
		t.Skipf("当前 Windows 账户不能创建符号链接: %v", err)
	}
	if _, err := validateImage(link); err == nil {
		t.Fatal("validateImage() error = nil, want symlink error")
	}
}
