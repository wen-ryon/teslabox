package carplaypairing

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestScanRetainsSlotThroughCancelCleanupAndCloseWaits(t *testing.T) {
	m, _ := NewManager(func(context.Context, string, *Confirmation) error { return nil })
	entered, cleanup := make(chan struct{}), make(chan struct{})
	m.scanner = func(ctx context.Context) ([]Device, error) {
		close(entered)
		<-ctx.Done()
		<-cleanup
		return nil, ctx.Err()
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	finished := make(chan error, 1)
	go func() { _, err := m.Scan(ctx); finished <- err }()
	<-entered
	if !m.Scanning() {
		t.Fatal("搜索未占有任务槽")
	}
	if _, err := m.Start(context.Background(), testPhone, time.Second); !errors.Is(err, ErrBusy) {
		t.Fatal(err)
	}
	if _, err := m.Scan(context.Background()); !errors.Is(err, ErrBusy) {
		t.Fatal(err)
	}
	wait, stop := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer stop()
	if err := m.Close(wait); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatal(err)
	}
	if !m.Scanning() {
		t.Fatal("搜索清理未退出就释放任务槽")
	}
	close(cleanup)
	if err := <-finished; !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	if m.Scanning() {
		t.Fatal("搜索退出后仍占用槽")
	}
	if err := m.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestScanNeverStartsDuringPairingOrCleanupUncertainty(t *testing.T) {
	m, _ := NewManager(func(context.Context, string, *Confirmation) error { return ErrCleanupPending })
	m.scanner = func(context.Context) ([]Device, error) { t.Fatal("清理未确认时发起搜索"); return nil, nil }
	s, _ := m.Start(context.Background(), testPhone, time.Second)
	_ = m.Wait(context.Background(), s.RequestID)
	if _, err := m.Scan(context.Background()); !errors.Is(err, ErrBusy) {
		t.Fatal(err)
	}
}
