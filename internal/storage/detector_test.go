package storage

import (
	"context"
	"errors"
	"testing"
)

type fakeRunner struct {
	output string
	err    error
}

func (r fakeRunner) Run(context.Context, string, ...string) ([]byte, error) {
	return []byte(r.output), r.err
}

func TestLSBLKDetectorDetectByUUID(t *testing.T) {
	t.Parallel()
	payload := `{
  "blockdevices": [
    {"path":"/dev/mmcblk0","type":"disk","rm":0,"children":[
      {"path":"/dev/mmcblk0p3","uuid":"system-uuid","label":"TESLABOX","fstype":"ext4","type":"part","mountpoint":"/"}
    ]},
    {"path":"/dev/sda","type":"disk","rm":1,"tran":"usb","model":"Tesla Drive","serial":"USB123","children":[
      {"path":"/dev/sda1","uuid":"data-uuid","label":"TESLABOX","fstype":"xfs","type":"part","mountpoint":"/var/lib/teslabox","size":128000000000}
    ]}
  ]
}`
	detector := NewLSBLKDetector(fakeRunner{output: payload}, "data-uuid", "TESLABOX")
	device, err := detector.Detect(t.Context())
	if err != nil {
		t.Fatalf("Detect() error = %v", err)
	}
	if device.Path != "/dev/sda1" || device.UUID != "data-uuid" {
		t.Fatalf("device = %#v", device)
	}
	if device.Transport != "usb" || !device.Removable || device.Model != "Tesla Drive" || device.Serial != "USB123" {
		t.Errorf("inherited fields = %#v", device)
	}
	if device.SizeBytes != 128000000000 {
		t.Errorf("SizeBytes = %d", device.SizeBytes)
	}
}

func TestLSBLKDetectorDoesNotFallbackFromUUIDToLabel(t *testing.T) {
	t.Parallel()
	payload := `{"blockdevices":[{"path":"/dev/sda","type":"disk","tran":"usb","children":[{"path":"/dev/sda1","uuid":"other-uuid","label":"TESLABOX","type":"part"}]}]}`
	detector := NewLSBLKDetector(fakeRunner{output: payload}, "expected-uuid", "TESLABOX")
	_, err := detector.Detect(t.Context())
	if !errors.Is(err, ErrDeviceNotFound) {
		t.Fatalf("Detect() error = %v, want ErrDeviceNotFound", err)
	}
}

func TestLSBLKDetectorRejectsDuplicateLabel(t *testing.T) {
	t.Parallel()
	payload := `{"blockdevices":[
      {"path":"/dev/sda","type":"disk","children":[{"path":"/dev/sda1","label":"TESLABOX","type":"part"}]},
      {"path":"/dev/sdb","type":"disk","children":[{"path":"/dev/sdb1","label":"TESLABOX","type":"part"}]}
    ]}`
	detector := NewLSBLKDetector(fakeRunner{output: payload}, "", "TESLABOX")
	_, err := detector.Detect(t.Context())
	if !errors.Is(err, ErrAmbiguousDevice) {
		t.Fatalf("Detect() error = %v, want ErrAmbiguousDevice", err)
	}
}

func TestLSBLKDetectorDetectNotFound(t *testing.T) {
	t.Parallel()
	detector := NewLSBLKDetector(fakeRunner{output: `{"blockdevices":[]}`}, "", "TESLABOX")
	_, err := detector.Detect(t.Context())
	if !errors.Is(err, ErrDeviceNotFound) {
		t.Fatalf("Detect() error = %v, want ErrDeviceNotFound", err)
	}
}
