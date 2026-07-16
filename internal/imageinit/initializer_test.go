package imageinit

import (
	"context"
	"errors"
	"reflect"
	"testing"
)

type fakeInspector struct {
	environment Environment
	err         error
}

func (f fakeInspector) Inspect(context.Context, Options) (Environment, error) {
	return f.environment, f.err
}

type fakeRunner struct {
	commands []Command
	output   map[string][]byte
	failName string
}

func (f *fakeRunner) Run(_ context.Context, name string, args ...string) ([]byte, error) {
	f.commands = append(f.commands, Command{Name: name, Args: args})
	if name == f.failName {
		return nil, errors.New("测试失败")
	}
	return f.output[name], nil
}

func validOptions() Options {
	return Options{
		DataRoot:  "/var/lib/teslabox",
		ImagePath: "/var/lib/teslabox/images/teslacam.img",
		SizeBytes: 96_000_000_000,
		Label:     "TESLACAM",
	}
}

func validEnvironment() Environment {
	return Environment{
		DataRoot:   "/var/lib/teslabox",
		ImagePath:  "/var/lib/teslabox/images/teslacam.img",
		FileSystem: "xfs",
		Reflink:    true,
		TotalBytes: 123_922_808_832,
		FreeBytes:  123_000_000_000,
	}
}

func TestRunDryRunDoesNotExecute(t *testing.T) {
	runner := &fakeRunner{}
	result, err := New(fakeInspector{environment: validEnvironment()}, runner).Run(t.Context(), validOptions())
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if result.WasExecuted {
		t.Fatal("Run() WasExecuted = true, want false")
	}
	if len(runner.commands) != 0 {
		t.Fatalf("dry-run executed %d commands", len(runner.commands))
	}
	want := Command{Name: "truncate", Args: []string{"--size", "96000000000", "/var/lib/teslabox/images/teslacam.img"}}
	if !reflect.DeepEqual(result.Commands[0], want) {
		t.Fatalf("first command = %#v, want %#v", result.Commands[0], want)
	}
	partition := Command{
		Name: "parted",
		Args: []string{"--script", "--align", "optimal", "/var/lib/teslabox/images/teslacam.img", "mkpart", "primary", "ntfs", "1MiB", "100%"},
	}
	if !reflect.DeepEqual(result.Commands[2], partition) {
		t.Fatalf("partition command = %#v, want %#v", result.Commands[2], partition)
	}
}

func TestRunRejectsUnsafeEnvironment(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*Environment)
	}{
		{name: "not xfs", mutate: func(e *Environment) { e.FileSystem = "ext4" }},
		{name: "no reflink", mutate: func(e *Environment) { e.Reflink = false }},
		{name: "image exists", mutate: func(e *Environment) { e.ImageExists = true }},
		{name: "insufficient reserve", mutate: func(e *Environment) { e.TotalBytes = 100_000_000_000 }},
		{name: "insufficient free", mutate: func(e *Environment) { e.FreeBytes = 1_000_000_000 }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			environment := validEnvironment()
			tt.mutate(&environment)
			_, err := New(fakeInspector{environment: environment}, &fakeRunner{}).Run(t.Context(), validOptions())
			if err == nil {
				t.Fatal("Run() error = nil, want validation error")
			}
		})
	}
}

func TestRunRejectsPathOutsideDataRoot(t *testing.T) {
	options := validOptions()
	options.ImagePath = "/var/lib/teslabox-other/teslacam.img"
	_, err := New(fakeInspector{}, &fakeRunner{}).Run(t.Context(), options)
	if err == nil {
		t.Fatal("Run() error = nil, want path boundary error")
	}
}

func TestParseStatFS(t *testing.T) {
	blockSize, totalBlocks, freeBlocks, err := parseStatFS("4096:30254592:30000000\n")
	if err != nil {
		t.Fatalf("parseStatFS() error = %v", err)
	}
	if blockSize != 4096 || totalBlocks != 30254592 || freeBlocks != 30000000 {
		t.Fatalf("parseStatFS() = %d, %d, %d", blockSize, totalBlocks, freeBlocks)
	}
}
