package gadget

import (
	"os"
	"path/filepath"
	"testing"
)

func TestFSProberProbe(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	configFSPath := filepath.Join(root, "usb_gadget")
	udcPath := filepath.Join(root, "udc")
	if err := os.MkdirAll(configFSPath, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(udcPath, "fcc00000.dwc3"), 0o755); err != nil {
		t.Fatal(err)
	}

	environment, err := NewFSProber(configFSPath, udcPath).Probe()
	if err != nil {
		t.Fatalf("Probe() error = %v", err)
	}
	if !environment.Ready || !environment.ConfigFSAvailable {
		t.Fatalf("environment = %#v", environment)
	}
	if len(environment.UDCNames) != 1 || environment.UDCNames[0] != "fcc00000.dwc3" {
		t.Fatalf("UDCNames = %#v", environment.UDCNames)
	}
}

func TestFSProberReportsMissingEnvironment(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	environment, err := NewFSProber(filepath.Join(root, "missing-configfs"), filepath.Join(root, "missing-udc")).Probe()
	if err != nil {
		t.Fatalf("Probe() error = %v", err)
	}
	if environment.Ready || environment.Reason == "" {
		t.Fatalf("environment = %#v", environment)
	}
}
