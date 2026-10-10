//go:build linux

package carplaypairing

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

func TestNativePairRunnerHelper(t *testing.T) {
	mode := os.Getenv("TESLABOX_PAIRING_TEST_CHILD")
	if mode == "" {
		return
	}
	args := os.Args
	value := func(flag string) string {
		for i := range args {
			if args[i] == flag && i+1 < len(args) {
				return args[i+1]
			}
		}
		return ""
	}
	if mode == "cancel" {
		term := make(chan os.Signal, 1)
		signal.Notify(term, syscall.SIGTERM)
		_ = os.WriteFile(os.Getenv("TESLABOX_PAIRING_TEST_MARKER"), []byte("ready"), 0600)
		<-term
		_ = os.WriteFile(os.Getenv("TESLABOX_PAIRING_TEST_MARKER"), []byte("cleaned"), 0600)
		os.Exit(1)
	}
	conn, err := net.Dial("unix", value("--confirmation-socket"))
	if err != nil {
		os.Exit(2)
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(3 * time.Second))
	id := value("--request-id")
	if err := json.NewEncoder(conn).Encode(map[string]any{"request_id": id, "passkey": 42}); err != nil {
		os.Exit(3)
	}
	var reply map[string]string
	if err := json.NewDecoder(conn).Decode(&reply); err != nil || reply["request_id"] != id || reply["digits"] != "000042" {
		os.Exit(4)
	}
	os.Exit(0)
}

func helperCommand(mode, marker string) func(context.Context, string, ...string) *exec.Cmd {
	return func(ctx context.Context, executable string, args ...string) *exec.Cmd {
		cmd := exec.CommandContext(ctx, executable, append([]string{"-test.run=^TestNativePairRunnerHelper$", "--"}, args...)...)
		cmd.Env = append(os.Environ(), "TESLABOX_PAIRING_TEST_CHILD="+mode, "TESLABOX_PAIRING_TEST_MARKER="+marker)
		return cmd
	}
}

func TestNativePairRunnerActualELFProcessAndUnixConfirmation(t *testing.T) {
	directory := privateDirectory(t)
	binary, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	body, err := os.ReadFile(binary)
	if err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(body)
	m, _ := NewManager(nativePairRunner(binary, directory, helperCommand("confirm", ""), hex.EncodeToString(digest[:])))
	s, err := m.Start(context.Background(), testPhone, 3*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	waitState(t, m.current.confirmation, "waiting_confirmation")
	if err := m.Confirm(s.RequestID, "000042"); err != nil {
		t.Fatal(err)
	}
	if err := m.Wait(context.Background(), s.RequestID); err != nil {
		t.Fatal(err)
	}
	if status := m.Status(); status.State != "bluetooth_paired" || status.Running {
		t.Fatal(status)
	}
	entries, err := os.ReadDir(directory)
	if err != nil || len(entries) != 0 {
		t.Fatal("本轮进程退出后应清理自有通道", entries, err)
	}
}

func TestNativePairRunnerDigestMismatchNeverStartsProcess(t *testing.T) {
	binary, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	started := false
	run := nativePairRunner(binary, privateDirectory(t), func(context.Context, string, ...string) *exec.Cmd { started = true; return nil }, "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa")
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	c, _ := NewConfirmation(ctx, time.Second)
	if err := run(ctx, testPhone, c); err == nil || started {
		t.Fatal("摘要不匹配时不能启动配对")
	}
}

func TestNativePairRunnerCancellationUsesTermAndWaitsForActualCleanup(t *testing.T) {
	directory := privateDirectory(t)
	marker := filepath.Join(directory, "child-marker")
	t.Cleanup(func() { _ = os.Remove(marker) })
	binary, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	m, _ := NewManager(nativePairRunner(binary, directory, helperCommand("cancel", marker)))
	s, err := m.Start(context.Background(), testPhone, 3*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(time.Second)
	for {
		body, _ := os.ReadFile(marker)
		if string(body) == "ready" {
			break
		}
		if time.Now().After(deadline) {
			_ = m.Cancel(s.RequestID)
			_ = m.Wait(context.Background(), s.RequestID)
			t.Fatal("测试进程未启动")
		}
		time.Sleep(time.Millisecond)
	}
	if err := m.Cancel(s.RequestID); err != nil {
		t.Fatal(err)
	}
	_ = m.Wait(context.Background(), s.RequestID)
	body, err := os.ReadFile(marker)
	if err != nil || string(body) != "cleaned" || m.Status().State != "canceled" {
		t.Fatal("需要等待 SIGTERM 清理，不能直接杀进程或误报成功", string(body), err, m.Status())
	}
}

func TestNativePairRunnerRejectsWritableOrNonELFProgram(t *testing.T) {
	directory := privateDirectory(t)
	binary := filepath.Join(directory, "invalid-program")
	t.Cleanup(func() { _ = os.Remove(binary) })
	for _, mode := range []os.FileMode{0700, 0722} {
		if err := os.WriteFile(binary, []byte("not ELF"), mode); err != nil {
			t.Fatal(err)
		}
		_ = os.Chmod(binary, mode)
		m, _ := NewManager(nativePairRunner(binary, directory, helperCommand("confirm", "")))
		s, err := m.Start(context.Background(), testPhone, time.Second)
		if err != nil {
			t.Fatal(err)
		}
		_ = m.Wait(context.Background(), s.RequestID)
		if m.Status().State != "failed" {
			t.Fatal(m.Status())
		}
	}
}
