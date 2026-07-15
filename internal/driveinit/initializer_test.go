package driveinit

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"testing"
)

const (
	testStablePath = "/dev/disk/by-id/usb-_TESLADRIVE_071C4AAFBA8BEB80-0:0"
	testSerial     = "071C4AAFBA8BEB80"
)

type fakeInspector struct {
	device Device
	err    error
}

func (f fakeInspector) Inspect(context.Context, string) (Device, error) {
	return f.device, f.err
}

type fakeRunner struct {
	commands []Command
	failName string
	output   []byte
}

func (f *fakeRunner) Run(_ context.Context, name string, args ...string) ([]byte, error) {
	f.commands = append(f.commands, Command{Name: name, Args: args})
	if name == f.failName {
		return nil, errors.New("测试失败")
	}
	return f.output, nil
}

func validDevice() Device {
	return Device{
		RequestedPath: testStablePath,
		ResolvedPath:  "/dev/sda",
		Type:          "disk",
		Transport:     "usb",
		Model:         "TESLADRIVE",
		Serial:        testSerial,
		Size:          123_924_905_984,
	}
}

func validOptions() Options {
	return Options{
		Device:        testStablePath,
		ConfirmSerial: testSerial,
		Label:         "TESLABOX",
		MountPath:     "/var/lib/teslabox",
	}
}

func TestRunDryRunDoesNotExecuteCommands(t *testing.T) {
	runner := &fakeRunner{}
	initializer := New(fakeInspector{device: validDevice()}, runner)

	result, err := initializer.Run(context.Background(), validOptions())
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if result.WasExecuted {
		t.Fatal("Run() WasExecuted = true, want false")
	}
	if len(runner.commands) != 0 {
		t.Fatalf("dry-run executed %d commands", len(runner.commands))
	}
	if got, want := result.Partition, "/dev/sda1"; got != want {
		t.Fatalf("partition = %q, want %q", got, want)
	}
	if got, want := result.Commands[0], (Command{Name: "wipefs", Args: []string{"--all", "/dev/sda"}}); !reflect.DeepEqual(got, want) {
		t.Fatalf("first command = %#v, want %#v", got, want)
	}
}

func TestRunExecuteUsesArgumentVectors(t *testing.T) {
	runner := &fakeRunner{}
	options := validOptions()
	options.Execute = true

	result, err := New(fakeInspector{device: validDevice()}, runner).Run(context.Background(), options)
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if !result.WasExecuted {
		t.Fatal("Run() WasExecuted = false, want true")
	}
	if !reflect.DeepEqual(runner.commands, result.Commands) {
		t.Fatalf("executed commands = %#v, want %#v", runner.commands, result.Commands)
	}
}

func TestRunRejectsUnsafeDevices(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*Device)
	}{
		{name: "not disk", mutate: func(d *Device) { d.Type = "part" }},
		{name: "not usb", mutate: func(d *Device) { d.Transport = "nvme" }},
		{name: "serial mismatch", mutate: func(d *Device) { d.Serial = "other" }},
		{name: "too small", mutate: func(d *Device) { d.Size = 1024 }},
		{name: "mounted child", mutate: func(d *Device) {
			d.Children = []Device{{ResolvedPath: "/dev/sda1", MountPoint: "/media/data"}}
		}},
		{name: "swap child", mutate: func(d *Device) {
			d.Children = []Device{{ResolvedPath: "/dev/sda1", FileSystem: "swap"}}
		}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			device := validDevice()
			tt.mutate(&device)
			runner := &fakeRunner{}
			options := validOptions()
			options.Execute = true

			_, err := New(fakeInspector{device: device}, runner).Run(context.Background(), options)
			if err == nil {
				t.Fatal("Run() error = nil, want validation error")
			}
			if len(runner.commands) != 0 {
				t.Fatalf("unsafe device executed %d commands", len(runner.commands))
			}
		})
	}
}

func TestRunRejectsPartitionStablePath(t *testing.T) {
	options := validOptions()
	options.Device += "-part1"
	_, err := New(fakeInspector{device: validDevice()}, &fakeRunner{}).Run(context.Background(), options)
	if err == nil {
		t.Fatal("Run() error = nil, want stable whole-disk path error")
	}
}

func TestRunRejectsOversizedXFSLabel(t *testing.T) {
	options := validOptions()
	options.Label = "TESLABOX_DATA"
	_, err := New(fakeInspector{device: validDevice()}, &fakeRunner{}).Run(context.Background(), options)
	if err == nil {
		t.Fatal("Run() error = nil, want XFS label length error")
	}
}

func TestLSBLKInspectorParsesDeviceTree(t *testing.T) {
	runner := &fakeRunner{output: []byte(`{
  "blockdevices": [{
    "path": "/dev/sda", "type": "disk", "size": 123924905984,
    "tran": "usb", "model": "TESLADRIVE", "serial": "071C4AAFBA8BEB80",
    "fstype": null, "mountpoint": null,
    "children": [{"path": "/dev/sda1", "type": "part", "size": 123923857408,
      "tran": null, "model": null, "serial": null, "fstype": "exfat", "mountpoint": null}]
  }]
}`)}
	// Windows 无法解析 Linux 设备符号链接，因此只验证真实 lsblk JSON 的内部映射。
	var response struct {
		BlockDevices []lsblkDevice `json:"blockdevices"`
	}
	if err := json.Unmarshal(runner.output, &response); err != nil {
		t.Fatalf("Unmarshal() error = %v", err)
	}
	device := response.BlockDevices[0].device()
	if got, want := len(device.Children), 1; got != want {
		t.Fatalf("children = %d, want %d", got, want)
	}
	if got, want := device.Children[0].FileSystem, "exfat"; got != want {
		t.Fatalf("child filesystem = %q, want %q", got, want)
	}
}
