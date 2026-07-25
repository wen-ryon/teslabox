package hostmigration

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

type recordingRunner struct {
	calls []string
	err   error
}

func (r *recordingRunner) Run(_ context.Context, name string, args ...string) ([]byte, error) {
	r.calls = append(r.calls, strings.Join(append([]string{name}, args...), " "))
	return nil, r.err
}

func TestRemoveLegacyUdevSignalPreservesUnrelatedRules(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "99-teslabox.rules")
	content := legacyUdevComment + "\r\n" +
		`ACTION=="add|remove|change", SUBSYSTEM=="block", KERNEL!="loop*", ` + legacyUdevSignal + "\r\n" +
		`ACTION=="add", SUBSYSTEM=="net", TAG+="systemd"` + "\r\n"
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	runner := &recordingRunner{}

	changed, err := RemoveLegacyUdevSignal(t.Context(), path, runner)
	if err != nil {
		t.Fatal(err)
	}
	if !changed {
		t.Fatal("RemoveLegacyUdevSignal() changed = false")
	}
	updated, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(updated), legacyUdevSignal) || strings.Contains(string(updated), "\r\n") {
		t.Fatalf("updated rule still contains legacy signal or CRLF: %q", updated)
	}
	if !strings.Contains(string(updated), `ACTION=="add", SUBSYSTEM=="net", TAG+="systemd"`) ||
		!strings.Contains(string(updated), "不要从 udev 向主进程发送信号") {
		t.Fatalf("updated rule did not preserve the unrelated rule: %q", updated)
	}
	if len(runner.calls) != 1 || runner.calls[0] != "udevadm control --reload-rules" {
		t.Fatalf("runner calls = %#v", runner.calls)
	}
}

func TestRemoveLegacyUdevSignalIsIdempotent(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "99-teslabox.rules")
	if err := os.WriteFile(path, []byte(replacementComment), 0o644); err != nil {
		t.Fatal(err)
	}
	runner := &recordingRunner{}

	changed, err := RemoveLegacyUdevSignal(t.Context(), path, runner)
	if err != nil {
		t.Fatal(err)
	}
	if changed || len(runner.calls) != 0 {
		t.Fatalf("changed = %t, calls = %#v", changed, runner.calls)
	}
}

func TestRemoveLegacyUdevSignalRejectsSymlink(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	target := filepath.Join(root, "target")
	link := filepath.Join(root, "99-teslabox.rules")
	if err := os.WriteFile(target, []byte(legacyUdevSignal), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, link); err != nil {
		t.Skipf("当前平台不能创建测试符号链接: %v", err)
	}

	if _, err := RemoveLegacyUdevSignal(t.Context(), link, &recordingRunner{}); err == nil {
		t.Fatal("RemoveLegacyUdevSignal() error = nil")
	}
}
