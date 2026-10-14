//go:build linux

package carplaypairing

import (
	"context"
	"testing"
	"time"
)

func TestIncomingBudgetKeepsPhaseAndTotalLimits(t *testing.T) {
	now := time.Now()
	total := now.Add(180 * time.Second)
	setup := phaseDeadline(total, now, 60*time.Second)
	video := phaseDeadline(total, now.Add(50*time.Second), 120*time.Second)
	if setup != now.Add(60*time.Second) || video != now.Add(170*time.Second) {
		t.Fatal("准备阶段挤占视频预算", setup, video)
	}
	if phaseDeadline(total, now.Add(80*time.Second), 120*time.Second) != total {
		t.Fatal("视频阶段越过总截止")
	}
}

func TestIncomingBudgetDoesNotExpandManualConfirmation(t *testing.T) {
	if c, err := NewConfirmation(context.Background(), 180*time.Second); err == nil {
		c.cancel()
		t.Fatal("扩大了普通数字确认窗口")
	}
	parent, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	c, err := newConfirmation(parent, 180*time.Second, 180*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer c.cancel()
	deadline, _ := parent.Deadline()
	if c.Status().ExpiresAt.After(deadline) {
		t.Fatal("首次登记脱离父截止")
	}
	cancel()
	if c.ctx.Err() == nil {
		t.Fatal("父任务取消没有传播")
	}
}
