package carplaypairing

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"strings"
	"testing"
	"time"
)

func beginConfirmation(t *testing.T, window time.Duration) (*Confirmation, net.Conn, <-chan error) {
	t.Helper()
	c, err := NewConfirmation(context.Background(), window)
	if err != nil {
		t.Fatal(err)
	}
	server, client := net.Pipe()
	result := make(chan error, 1)
	go func() { result <- c.Serve(server) }()
	t.Cleanup(func() {
		_ = c.Cancel(c.Status().RequestID)
		_ = client.Close()
	})
	return c, client, result
}

func waitState(t *testing.T, c *Confirmation, state string) ConfirmationStatus {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		status := c.Status()
		if status.State == state {
			return status
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("未进入状态 %s，实际 %s", state, c.Status().State)
	return ConfirmationStatus{}
}

func awaitResult(t *testing.T, result <-chan error) error {
	t.Helper()
	select {
	case err := <-result:
		return err
	case <-time.After(time.Second):
		t.Fatal("配对确认线程没有退出")
		return nil
	}
}

func TestConfirmationRequiresCurrentDigitsAndDoesNotClaimPaired(t *testing.T) {
	c, client, result := beginConfirmation(t, 2*time.Second)
	id := c.Status().RequestID
	if err := json.NewEncoder(client).Encode(map[string]any{"request_id": id, "passkey": 42}); err != nil {
		t.Fatal(err)
	}
	status := waitState(t, c, "waiting_confirmation")
	if status.Digits != "000042" {
		t.Fatalf("六位数字格式错误: %q", status.Digits)
	}
	if !errors.Is(c.Confirm(strings.Repeat("0", 32), "000042"), ErrNotPending) {
		t.Fatal("旧操作能够批准本轮配对")
	}
	if !errors.Is(c.Confirm(id, "42"), ErrDigitsMismatch) || c.Status().State != "waiting_confirmation" {
		t.Fatal("错误数字消耗了确认窗口")
	}
	if err := c.Confirm(id, "000042"); err != nil {
		t.Fatal(err)
	}
	if !errors.Is(c.Confirm(id, "000042"), ErrNotPending) {
		t.Fatal("重复确认被接受")
	}
	var reply map[string]string
	if err := json.NewDecoder(client).Decode(&reply); err != nil {
		t.Fatal(err)
	}
	if reply["request_id"] != id || reply["digits"] != "000042" || len(reply) != 2 {
		t.Fatal("实际回复不符合接收端协议")
	}
	if err := awaitResult(t, result); err != nil {
		t.Fatal(err)
	}
	if status := c.Status(); status.State != "confirmation_sent" || status.Digits != "" {
		t.Fatal("确认后状态泄露数字或错误声称配对完成")
	}
	server2, client2 := net.Pipe()
	defer client2.Close()
	if !errors.Is(c.Serve(server2), ErrNotPending) {
		t.Fatal("同一操作重复使用连接")
	}
}

func TestConfirmationRejectsMalformedNativeChallenge(t *testing.T) {
	for _, body := range []string{
		`{"request_id":"old","passkey":42}`,
		`{"request_id":"%s"}`,
		`{"request_id":"%s","passkey":null}`,
		`{"request_id":"%s","passkey":1000000}`,
		`{"request_id":"%s","passkey":-1}`,
		`{"request_id":"%s","passkey":42,"approved":true}`,
		`{"request_id":"%s","passkey":42,"passkey":43}`,
		`{"request_id":"%s","passkey":42} {}`,
		strings.Repeat("x", 257),
	} {
		t.Run(body, func(t *testing.T) {
			c, client, result := beginConfirmation(t, time.Second)
			if strings.Contains(body, "%s") {
				body = fmt.Sprintf(body, c.Status().RequestID)
			}
			_, _ = fmt.Fprintln(client, body)
			if awaitResult(t, result) == nil || c.Status().State != "failed" || c.Status().Digits != "" {
				t.Fatal("异常提示被接受或数字未清理")
			}
		})
	}
}

func TestConfirmationCancelInterruptsReadAndAwaitingHuman(t *testing.T) {
	for _, challengeSent := range []bool{false, true} {
		t.Run(fmt.Sprint(challengeSent), func(t *testing.T) {
			c, client, result := beginConfirmation(t, 2*time.Second)
			id := c.Status().RequestID
			if challengeSent {
				_ = json.NewEncoder(client).Encode(map[string]any{"request_id": id, "passkey": 123456})
				waitState(t, c, "waiting_confirmation")
			}
			if err := c.Cancel(id); err != nil {
				t.Fatal(err)
			}
			if awaitResult(t, result) == nil || c.Status().State != "canceled" || c.Status().Digits != "" {
				t.Fatal("取消没有结束本轮连接")
			}
			_ = client.SetReadDeadline(time.Now().Add(time.Second))
			if _, err := bufio.NewReader(client).ReadByte(); err == nil {
				t.Fatal("取消后连接仍然输出确认")
			}
		})
	}
}

func TestConfirmationTimeoutClosesNativeConnection(t *testing.T) {
	c, client, result := beginConfirmation(t, 30*time.Millisecond)
	_ = json.NewEncoder(client).Encode(map[string]any{"request_id": c.Status().RequestID, "passkey": 42})
	if awaitResult(t, result) == nil || c.Status().State != "canceled" || c.Status().Digits != "" {
		t.Fatal("到期后仍等待人工确认")
	}
}

func TestConfirmationCancelInterruptsBlockedApprovalWrite(t *testing.T) {
	c, client, result := beginConfirmation(t, time.Second)
	id := c.Status().RequestID
	_ = json.NewEncoder(client).Encode(map[string]any{"request_id": id, "passkey": 42})
	waitState(t, c, "waiting_confirmation")
	if err := c.Confirm(id, "000042"); err != nil {
		t.Fatal(err)
	}
	// 不读取回复，模拟接收端退出；取消必须中断阻塞写入并等待线程退出。
	if err := c.Cancel(id); err != nil {
		t.Fatal(err)
	}
	if awaitResult(t, result) == nil || c.Status().State != "canceled" {
		t.Fatal("回复阻塞时无法取消或提前标记确认已送达")
	}
}

func TestConfirmationHonorsParentDeadlineAndRejectsUnboundedWindow(t *testing.T) {
	if _, err := NewConfirmation(context.Background(), 121*time.Second); err == nil {
		t.Fatal("允许无限配对窗口")
	}
	parent, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	c, err := NewConfirmation(parent, 120*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Cancel(c.Status().RequestID)
	deadline, _ := parent.Deadline()
	if !c.Status().ExpiresAt.Equal(deadline) {
		t.Fatal("确认窗口超过父任务生命周期")
	}
}
