package carplaypairing

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"testing"
	"time"
)

const testPhone = "02:11:22:33:44:55"

func TestManagerCloseKeepsOwnershipUntilActualCleanup(t *testing.T) {
	entered, cleanup := make(chan struct{}), make(chan struct{})
	m, _ := NewManager(func(ctx context.Context, _ string, _ *Confirmation) error {
		close(entered)
		<-ctx.Done()
		<-cleanup
		return ctx.Err()
	})
	s, err := m.Start(context.Background(), testPhone, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	<-entered
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()
	if err := m.Close(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatal(err)
	}
	if m.Ready() || !m.Status().Running {
		t.Fatal("退出超时不能伪造清理完成")
	}
	if _, err := m.Start(context.Background(), testPhone, time.Second); err == nil {
		t.Fatal("关闭后不能接受新任务")
	}
	close(cleanup)
	if err := m.Wait(context.Background(), s.RequestID); err != nil {
		t.Fatal(err)
	}
	if err := m.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestManagerCloseReportsCleanupUncertainty(t *testing.T) {
	m, _ := NewManager(func(context.Context, string, *Confirmation) error { return ErrCleanupPending })
	s, _ := m.Start(context.Background(), testPhone, time.Second)
	_ = m.Wait(context.Background(), s.RequestID)
	if err := m.Close(context.Background()); !errors.Is(err, ErrCleanupPending) {
		t.Fatal(err)
	}
}

func TestManagerCleanupUncertaintyBlocksNewTaskAndReconcilesSameTaskOnly(t *testing.T) {
	runs := 0
	m, _ := NewManager(func(context.Context, string, *Confirmation) error { runs++; return ErrCleanupPending })
	s, err := m.Start(context.Background(), testPhone, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	_ = m.Wait(context.Background(), s.RequestID)
	if m.Status().State != "cleanup_pending" {
		t.Fatal(m.Status())
	}
	if _, err := m.Start(context.Background(), testPhone, time.Second); !errors.Is(err, ErrBusy) {
		t.Fatal(err)
	}
	if err := m.Reconcile(context.Background(), "old"); !errors.Is(err, ErrNotPending) {
		t.Fatal(err)
	}
	m.reconcile = func(context.Context, string, string, time.Time) error { return errors.New("still unknown") }
	if err := m.Reconcile(context.Background(), s.RequestID); err == nil {
		t.Fatal("清理未确认不能解除互斥")
	}
	if _, err := m.Start(context.Background(), testPhone, time.Second); !errors.Is(err, ErrBusy) {
		t.Fatal(err)
	}
	entered, finish := make(chan struct{}), make(chan struct{})
	m.reconcile = func(_ context.Context, id, phone string, _ time.Time) error {
		if id != s.RequestID || phone != testPhone {
			return ErrNotPending
		}
		close(entered)
		<-finish
		return nil
	}
	done := make(chan error, 1)
	go func() { done <- m.Reconcile(context.Background(), s.RequestID) }()
	<-entered
	if err := m.Cancel(s.RequestID); !errors.Is(err, ErrNotPending) {
		t.Fatal("不能把核对清理误当成取消新配对", err)
	}
	wait, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()
	if err := m.Wait(wait, s.RequestID); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatal(err)
	}
	close(finish)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if runs != 1 || m.Status().State != "failed" || m.Status().Running {
		t.Fatal(runs, m.Status())
	}
	if _, err := m.Start(context.Background(), testPhone, time.Second); err != nil {
		t.Fatal(err)
	}
	_ = m.Wait(context.Background(), m.Status().RequestID)
}

func TestManagerCancellationKeepsSlotUntilRunnerCleanup(t *testing.T) {
	entered, cleanup := make(chan struct{}), make(chan struct{})
	m, err := NewManager(func(ctx context.Context, phone string, confirmation *Confirmation) error {
		close(entered)
		<-ctx.Done()
		<-cleanup
		return ctx.Err()
	})
	if err != nil {
		t.Fatal(err)
	}
	status, err := m.Start(context.Background(), testPhone, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	<-entered
	if err := m.Cancel(status.RequestID); err != nil {
		t.Fatal(err)
	}
	if current := m.Status(); !current.Running || current.State != "canceling" || current.Prompt.Digits != "" {
		t.Fatal(current)
	}
	if _, err := m.Start(context.Background(), "02:AA:BB:CC:DD:EE", time.Second); !errors.Is(err, ErrBusy) {
		t.Fatal("清理完成前不能开始另一轮", err)
	}
	wait, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()
	if err := m.Wait(wait, status.RequestID); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatal(err)
	}
	if !m.Status().Running {
		t.Fatal("观察超时不能代替执行器退出")
	}
	close(cleanup)
	if err := m.Wait(context.Background(), status.RequestID); err != nil {
		t.Fatal(err)
	}
	if current := m.Status(); current.Running || current.State != "canceled" {
		t.Fatal(current)
	}
}

func TestManagerConfirmationIsNotPairingSuccessAndOldIDCannotAuthorizeNextTask(t *testing.T) {
	server, client := net.Pipe()
	defer client.Close()
	allowExit := make(chan struct{})
	paired := make(chan struct{})
	m, _ := NewManager(func(ctx context.Context, phone string, confirmation *Confirmation) error {
		if err := confirmation.Serve(server); err != nil {
			return err
		}
		close(paired)
		<-allowExit
		return nil
	})
	s, err := m.Start(context.Background(), testPhone, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.NewEncoder(client).Encode(map[string]any{"request_id": s.RequestID, "passkey": 42}); err != nil {
		t.Fatal(err)
	}
	waitState(t, m.current.confirmation, "waiting_confirmation")
	if err := m.Confirm("old-request", "000042"); !errors.Is(err, ErrNotPending) {
		t.Fatal(err)
	}
	if err := m.Confirm(s.RequestID, "999999"); !errors.Is(err, ErrDigitsMismatch) {
		t.Fatal(err)
	}
	if err := m.Confirm(s.RequestID, "000042"); err != nil {
		t.Fatal(err)
	}
	var reply map[string]string
	if err := json.NewDecoder(client).Decode(&reply); err != nil || reply["digits"] != "000042" {
		t.Fatal(reply, err)
	}
	<-paired
	if current := m.Status(); !current.Running || current.State != "pairing" || current.Prompt.Digits != "" {
		t.Fatal("数字送出仍不能声称蓝牙配对完成", current)
	}
	close(allowExit)
	if err := m.Wait(context.Background(), s.RequestID); err != nil {
		t.Fatal(err)
	}
	if current := m.Status(); current.State != "bluetooth_paired" || current.Running {
		t.Fatal(current)
	}
	// 下一轮执行器改为仅等待取消；旧 ID 不能确认、取消新任务。
	m.run = func(ctx context.Context, _ string, _ *Confirmation) error { <-ctx.Done(); return ctx.Err() }
	next, err := m.Start(context.Background(), testPhone, time.Second)
	if err != nil || next.RequestID == s.RequestID {
		t.Fatal(next, err)
	}
	if err := m.Cancel(s.RequestID); !errors.Is(err, ErrNotPending) {
		t.Fatal(err)
	}
	if err := m.Confirm(s.RequestID, "000042"); !errors.Is(err, ErrNotPending) {
		t.Fatal(err)
	}
	_ = m.Cancel(next.RequestID)
	_ = m.Wait(context.Background(), next.RequestID)
}

func TestManagerRunnerFailureIsSanitizedAndDoesNotClaimCarPlay(t *testing.T) {
	m, _ := NewManager(func(context.Context, string, *Confirmation) error {
		return errors.New("synthetic private protocol body")
	})
	s, err := m.Start(context.Background(), testPhone, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	_ = m.Wait(context.Background(), s.RequestID)
	if current := m.Status(); current.State != "failed" || current.Running || current.Prompt.Digits != "" {
		t.Fatal(current)
	}
}

func TestManagerParentDeadlineWaitsForCleanup(t *testing.T) {
	exited := make(chan struct{})
	m, _ := NewManager(func(ctx context.Context, _ string, _ *Confirmation) error {
		<-ctx.Done()
		close(exited)
		return nil
	})
	s, err := m.Start(context.Background(), testPhone, 10*time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	_ = m.Wait(context.Background(), s.RequestID)
	<-exited
	if current := m.Status(); current.State != "canceled" || current.Running {
		t.Fatal(current)
	}
}

func TestManagerRejectsMissingRunnerInvalidTargetAndWindow(t *testing.T) {
	if _, err := NewManager(nil); err == nil {
		t.Fatal("没有执行器不能提供任务入口")
	}
	m, _ := NewManager(func(context.Context, string, *Confirmation) error { return nil })
	for _, phone := range []string{"", "192.168.88.151", "00:00:00:00:00:00", "02:aa:bb:cc:dd:ee"} {
		if _, err := m.Start(context.Background(), phone, time.Second); err == nil {
			t.Fatal(phone)
		}
	}
	for _, window := range []time.Duration{0, -time.Second, 121 * time.Second} {
		if _, err := m.Start(context.Background(), testPhone, window); err == nil {
			t.Fatal(window)
		}
	}
	if m.Status().State != "idle" {
		t.Fatal("无效输入不应占用任务槽")
	}
}
