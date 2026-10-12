package carplaypairing

import (
	"context"
	"testing"
	"time"
)

type handoffStub struct{ plan HandoffPlan }

func (h *handoffStub) PlanCarPlayHandoff(context.Context) (HandoffPlan, error) { return h.plan, nil }
func (h *handoffStub) BeginCarPlayHandoff(context.Context, string, string, HandoffPlan) error {
	return nil
}
func (h *handoffStub) FinishCarPlayHandoff(context.Context, string, bool) error { return nil }

func TestHandoffRequiresFreshExplicitConfirmationAndKeepsTaskIdentity(t *testing.T) {
	entered := make(chan struct{})
	m, _ := NewManager(func(ctx context.Context, _ string, _ *Confirmation) error {
		close(entered)
		<-ctx.Done()
		return ctx.Err()
	})
	m.ConfigureHandoff(&handoffStub{HandoffPlan{Fingerprint: "current", Required: true}})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if _, err := m.Start(ctx, testPhone, time.Second); err == nil {
		t.Fatal("旧入口绕过交接确认")
	}
	for _, test := range []struct {
		name, fingerprint string
		confirm           bool
	}{{"未确认", "current", false}, {"旧配置", "old", true}} {
		t.Run(test.name, func(t *testing.T) {
			if _, err := m.StartConfirmed(ctx, testPhone, time.Second, test.fingerprint, test.confirm); err == nil {
				t.Fatal("错误授权启动登记")
			}
		})
	}
	first, err := m.StartConfirmed(ctx, testPhone, time.Second, "current", true)
	if err != nil {
		t.Fatal(err)
	}
	<-entered
	second, err := m.StartConfirmed(ctx, testPhone, time.Second, "current", true)
	if err != nil || second.RequestID != first.RequestID {
		t.Fatal("重复请求重建任务", err)
	}
	cancel()
	wait, end := context.WithTimeout(context.Background(), time.Second)
	defer end()
	if err := m.Wait(wait, first.RequestID); err != nil {
		t.Fatal(err)
	}
}
