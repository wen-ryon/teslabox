package snapshot

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"testing"
	"time"

	"github.com/wen-ryon/teslabox/internal/system"
)

type command struct {
	name string
	args []string
}

type fakeRunner struct {
	commands        []command
	failName        string
	failErr         error
	loopCalls       int
	failKernelMount bool
	mountFailures   int
	notMounted      bool
	loopList        string
}

func (r *fakeRunner) Run(_ context.Context, name string, args ...string) ([]byte, error) {
	r.commands = append(r.commands, command{name: name, args: append([]string(nil), args...)})
	if name == r.failName {
		if r.failErr != nil {
			return nil, r.failErr
		}
		return nil, errors.New("测试失败")
	}
	if name == "mount" && r.failKernelMount {
		return nil, &system.CommandError{Name: "mount", Code: 32, Output: "unknown filesystem type 'exfat'", Internal: errors.New("exit status 32")}
	}
	if name == "mount" && r.mountFailures > 0 {
		r.mountFailures--
		return nil, &system.CommandError{Name: "mount", Code: 32, Output: "Structure needs cleaning", Internal: errors.New("exit status 32")}
	}
	if name == "umount" && r.notMounted {
		return nil, &system.CommandError{Name: "umount", Code: 32, Output: "not mounted", Internal: errors.New("exit status 32")}
	}
	if name == "cp" {
		source := args[len(args)-2]
		target := args[len(args)-1]
		content, err := os.ReadFile(source)
		if err != nil {
			return nil, err
		}
		if err := os.WriteFile(target, content, 0o600); err != nil {
			return nil, err
		}
	}
	if name == "losetup" && len(args) > 0 && args[0] == "--find" {
		r.loopCalls++
		return []byte("/dev/loop7\n"), nil
	}
	if name == "losetup" && len(args) > 0 && args[0] == "--list" {
		return []byte(r.loopList), nil
	}
	return nil, nil
}

func TestManagerCreateStopsBeforeCopyWhenSpaceIsLow(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	imagePath := filepath.Join(root, "teslacam.img")
	snapshotPath := filepath.Join(root, "snapshots")
	mountRoot := filepath.Join(root, "mounts")
	if err := os.WriteFile(imagePath, []byte("teslacam"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(snapshotPath, 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(mountRoot, 0o750); err != nil {
		t.Fatal(err)
	}

	runner := &fakeRunner{}
	manager := New(runner, Config{ImagePath: imagePath, SnapshotPath: snapshotPath, MountRoot: mountRoot, MinFreeBytes: 20, MinFreePercent: 10})
	manager.space = func(string) (uint64, uint64, error) { return 100, 5, nil }
	_, err := manager.Create(t.Context())
	if !errors.Is(err, ErrInsufficientSpace) {
		t.Fatalf("Create() error = %v, want insufficient space", err)
	}
	if len(runner.commands) != 0 {
		t.Fatalf("commands = %#v, want none", runner.commands)
	}
}

func TestManagerRecoverOnlyRemovesManagedResources(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	snapshotPath := filepath.Join(root, "snapshots")
	mountRoot := filepath.Join(root, "mounts")
	if err := os.Mkdir(snapshotPath, 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(mountRoot, 0o750); err != nil {
		t.Fatal(err)
	}
	id := "20260716T080910.000000123Z"
	imagePath := filepath.Join(snapshotPath, "teslacam-"+id+".img")
	partialPath := filepath.Join(snapshotPath, "teslacam-"+id+".img.partial")
	unrelatedPath := filepath.Join(snapshotPath, "keep-me.img")
	for _, name := range []string{imagePath, partialPath, unrelatedPath} {
		if err := os.WriteFile(name, []byte("snapshot"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Mkdir(filepath.Join(mountRoot, id), 0o750); err != nil {
		t.Fatal(err)
	}

	runner := &fakeRunner{notMounted: true, loopList: "/dev/loop7 " + imagePath + "\n/dev/loop8 /var/lib/other.img\n"}
	manager := New(runner, Config{SnapshotPath: snapshotPath, MountRoot: mountRoot})
	if err := manager.Recover(t.Context()); err != nil {
		t.Fatalf("Recover() error = %v", err)
	}
	if _, err := os.Stat(imagePath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("managed image still exists: %v", err)
	}
	if _, err := os.Stat(partialPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("partial image still exists: %v", err)
	}
	if _, err := os.Stat(unrelatedPath); err != nil {
		t.Fatalf("unrelated image was removed: %v", err)
	}
	detached := make([]string, 0)
	for _, command := range runner.commands {
		if command.name == "losetup" && len(command.args) > 1 && command.args[0] == "--detach" {
			detached = append(detached, command.args[1])
		}
	}
	if !reflect.DeepEqual(detached, []string{"/dev/loop7"}) {
		t.Fatalf("detached = %v, want only managed loop", detached)
	}
}

func TestManagerRemoveDetachesLoopWhenFUSEAlreadyUnmounted(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	snapshotPath := filepath.Join(root, "snapshots")
	mountRoot := filepath.Join(root, "mounts")
	if err := os.Mkdir(snapshotPath, 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(mountRoot, 0o750); err != nil {
		t.Fatal(err)
	}
	imagePath := filepath.Join(snapshotPath, "teslacam-test.img")
	mountPath := filepath.Join(mountRoot, "test")
	if err := os.WriteFile(imagePath, []byte("snapshot"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(mountPath, 0o750); err != nil {
		t.Fatal(err)
	}

	runner := &fakeRunner{notMounted: true}
	manager := New(runner, Config{SnapshotPath: snapshotPath, MountRoot: mountRoot})
	err := manager.Remove(t.Context(), View{ID: "test", ImagePath: imagePath, MountPath: mountPath, LoopDevice: "/dev/loop7"})
	if err != nil {
		t.Fatalf("Remove() error = %v", err)
	}
	if len(runner.commands) != 2 || runner.commands[1].name != "losetup" {
		t.Fatalf("commands = %#v", runner.commands)
	}
}

func TestManagerCreateFallsBackToExFATFUSE(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	imagePath := filepath.Join(root, "teslacam.img")
	snapshotPath := filepath.Join(root, "snapshots")
	mountRoot := filepath.Join(root, "mounts")
	if err := os.WriteFile(imagePath, []byte("teslacam"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(snapshotPath, 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(mountRoot, 0o750); err != nil {
		t.Fatal(err)
	}

	runner := &fakeRunner{failKernelMount: true}
	manager := New(runner, Config{ImagePath: imagePath, SnapshotPath: snapshotPath, MountRoot: mountRoot})
	if _, err := manager.Create(t.Context()); err != nil {
		t.Fatalf("Create() error = %v", err)
	}
	if runner.commands[len(runner.commands)-1].name != "mount.exfat-fuse" {
		t.Fatalf("last command = %#v", runner.commands[len(runner.commands)-1])
	}
}

func TestManagerCreateAcceptsRepairedFilesystem(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	imagePath := filepath.Join(root, "teslacam.img")
	snapshotPath := filepath.Join(root, "snapshots")
	mountRoot := filepath.Join(root, "mounts")
	if err := os.WriteFile(imagePath, []byte("teslacam"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(snapshotPath, 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(mountRoot, 0o750); err != nil {
		t.Fatal(err)
	}

	runner := &fakeRunner{
		failName:      "fsck.exfat",
		failErr:       &system.CommandError{Name: "fsck.exfat", Code: 1, Internal: errors.New("已修复")},
		mountFailures: 1,
	}
	manager := New(runner, Config{ImagePath: imagePath, SnapshotPath: snapshotPath, MountRoot: mountRoot})
	view, err := manager.Create(t.Context())
	if err != nil {
		t.Fatalf("Create() error = %v", err)
	}
	if !view.RepairApplied {
		t.Fatal("Create() did not report the repair fallback")
	}
	if !slices.ContainsFunc(runner.commands, func(item command) bool { return item.name == "fsck.exfat" }) {
		t.Fatal("Create() did not repair the snapshot after mount failure")
	}
}

func TestManagerCreateUsesReflinkAndReadOnlyMount(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	imagePath := filepath.Join(root, "teslacam.img")
	snapshotPath := filepath.Join(root, "snapshots")
	mountRoot := filepath.Join(root, "mounts")
	if err := os.WriteFile(imagePath, []byte("teslacam"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(snapshotPath, 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(mountRoot, 0o750); err != nil {
		t.Fatal(err)
	}

	runner := &fakeRunner{}
	manager := New(runner, Config{ImagePath: imagePath, SnapshotPath: snapshotPath, MountRoot: mountRoot})
	manager.now = func() time.Time { return time.Date(2026, 7, 16, 8, 9, 10, 123, time.UTC) }
	view, err := manager.Create(t.Context())
	if err != nil {
		t.Fatalf("Create() error = %v", err)
	}
	if view.ID != "20260716T080910.000000123Z" || view.MountPath != filepath.Join(mountRoot, view.ID) {
		t.Fatalf("view = %#v", view)
	}
	if view.VolumeSpace.TotalBytes == 0 || view.VolumeSpace.FreeBytes == 0 {
		t.Fatalf("view volume space = %#v", view.VolumeSpace)
	}
	if view.RepairApplied {
		t.Fatal("Create() reported a repair for a directly mounted snapshot")
	}
	wantNames := []string{"cp", "losetup", "mount"}
	gotNames := make([]string, 0, len(runner.commands))
	for _, item := range runner.commands {
		gotNames = append(gotNames, item.name)
	}
	if !reflect.DeepEqual(gotNames, wantNames) {
		t.Fatalf("commands = %v, want %v", gotNames, wantNames)
	}
	if got := runner.commands[0].args[:2]; !reflect.DeepEqual(got, []string{"--reflink=always", "--sparse=auto"}) {
		t.Fatalf("cp args = %v", runner.commands[0].args)
	}
	if got := runner.commands[2].args[1]; got != "ro,nosuid,nodev,noexec" {
		t.Fatalf("mount options = %q", got)
	}
	if slices.ContainsFunc(runner.commands, func(item command) bool { return item.name == "fsck.exfat" }) {
		t.Fatal("Create() ran fsck for a snapshot that mounted read-only")
	}
}

func TestManagerCreateFailureDoesNotPublishSnapshot(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	imagePath := filepath.Join(root, "teslacam.img")
	snapshotPath := filepath.Join(root, "snapshots")
	mountRoot := filepath.Join(root, "mounts")
	if err := os.WriteFile(imagePath, []byte("teslacam"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(snapshotPath, 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(mountRoot, 0o750); err != nil {
		t.Fatal(err)
	}

	manager := New(&fakeRunner{failName: "fsck.exfat", mountFailures: 1}, Config{ImagePath: imagePath, SnapshotPath: snapshotPath, MountRoot: mountRoot})
	if _, err := manager.Create(t.Context()); err == nil {
		t.Fatal("Create() error = nil, want fsck error")
	}
	entries, err := os.ReadDir(snapshotPath)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("snapshot directory contains %v", entries)
	}
}

func TestManagerRemoveRejectsPathsOutsideManagedRoots(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	manager := New(&fakeRunner{}, Config{
		SnapshotPath: filepath.Join(root, "snapshots"),
		MountRoot:    filepath.Join(root, "mounts"),
	})
	err := manager.Remove(t.Context(), View{ID: "unsafe", ImagePath: filepath.Join(root, "outside.img"), MountPath: filepath.Join(root, "outside"), LoopDevice: "/dev/loop7"})
	if err == nil {
		t.Fatal("Remove() error = nil, want path validation error")
	}
}
