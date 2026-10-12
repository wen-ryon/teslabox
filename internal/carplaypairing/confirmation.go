// Package carplaypairing 为独立接收端提供限时数字确认，不操作录像、热点或系统服务。
package carplaypairing

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"sync"
	"time"
)

var (
	// ErrNotPending 防止旧页面或重复提交授权另一轮配对。
	ErrNotPending = errors.New("当前没有等待确认的 CarPlay 配对")
	// ErrDigitsMismatch 允许用户修正输入，不替用户批准不一致的数字。
	ErrDigitsMismatch = errors.New("配对数字不一致")
)

// ConfirmationStatus 只在等待人工确认时包含数字，不表示蓝牙已经配对完成。
type ConfirmationStatus struct {
	RequestID string    `json:"request_id"`
	State     string    `json:"state"`
	Digits    string    `json:"digits,omitempty"`
	ExpiresAt time.Time `json:"expires_at"`
}

// Confirmation 由一次配对任务持有，数字只保存在内存，不能复用到下一轮。
type Confirmation struct {
	failurePhase string
	incoming     bool
	phase        string
	handoff      NetworkHandoff
	handoffPlan  HandoffPlan
	mu           sync.Mutex
	ctx          context.Context
	cancel       context.CancelFunc
	status       ConfirmationStatus
	served       bool
	decision     chan string
}

func (c *Confirmation) setPhase(phase string) { c.mu.Lock(); defer c.mu.Unlock(); c.phase = phase }
func (c *Confirmation) currentPhase() string  { c.mu.Lock(); defer c.mu.Unlock(); return c.phase }

func (c *Confirmation) recordFailurePhase() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.failurePhase = c.phase
}

func (c *Confirmation) failedPhase() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.failurePhase
}

// NewConfirmation 要求明确的限时窗口，避免管理页离开后无限等待。
func NewConfirmation(parent context.Context, window time.Duration) (*Confirmation, error) {
	if parent == nil || window <= 0 || window > 120*time.Second {
		return nil, errors.New("配对确认窗口必须介于 0 与 120 秒之间")
	}
	if err := parent.Err(); err != nil {
		return nil, err
	}
	var nonce [16]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		return nil, fmt.Errorf("生成配对操作标识: %w", err)
	}
	ctx, cancel := context.WithTimeout(parent, window)
	deadline, _ := ctx.Deadline()
	return &Confirmation{
		ctx: ctx, cancel: cancel,
		status: ConfirmationStatus{RequestID: hex.EncodeToString(nonce[:]), State: "waiting_native", ExpiresAt: deadline},
		// 只有一次合法提交；缓冲让 HTTP 确认不依赖接收端读取的时机。
		decision: make(chan string, 1),
	}, nil
}

// Status 在过期或取消后隐藏数字，调用者不得把该快照写入日志。
func (c *Confirmation) Status() ConfirmationStatus {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.expireLocked()
	return c.status
}

func (c *Confirmation) expireLocked() {
	if c.ctx.Err() != nil && (c.status.State == "waiting_native" || c.status.State == "waiting_confirmation" || c.status.State == "sending_confirmation") {
		c.status.State = "canceled"
		c.status.Digits = ""
	}
}

// Confirm 只授权本轮匹配的六位数字，错误输入不消耗确认机会。
func (c *Confirmation) Confirm(requestID, digits string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.expireLocked()
	if requestID != c.status.RequestID || c.status.State != "waiting_confirmation" {
		return ErrNotPending
	}
	if digits != c.status.Digits {
		return ErrDigitsMismatch
	}
	c.status.State = "sending_confirmation"
	c.status.Digits = ""
	c.decision <- digits
	return nil
}

// Cancel 清理本次任务的确认窗口，不影响蓝牙适配器或其他服务。
func (c *Confirmation) Cancel(requestID string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.expireLocked()
	if requestID != c.status.RequestID || c.status.State == "confirmation_sent" || c.status.State == "failed" {
		return ErrNotPending
	}
	c.status.State = "canceled"
	c.status.Digits = ""
	c.cancel()
	return nil
}

// Serve 在调用线程处理一次已验证本机身份的连接；调用方负责私有 Unix socket 和配对进程生命周期。
// 成功仅表示数字已传给接收端，配对是否完成仍须由 BlueZ 的实际结果确认。
func (c *Confirmation) Serve(conn net.Conn) (err error) {
	if conn == nil {
		return errors.New("配对确认连接为空")
	}
	c.mu.Lock()
	c.expireLocked()
	if c.served || c.status.State != "waiting_native" {
		c.mu.Unlock()
		_ = conn.Close()
		return ErrNotPending
	}
	c.served = true
	requestID := c.status.RequestID
	deadline := c.status.ExpiresAt
	c.mu.Unlock()
	stop := context.AfterFunc(c.ctx, func() { _ = conn.Close() })
	defer func() {
		stop()
		_ = conn.Close()
		c.mu.Lock()
		if err != nil {
			if c.ctx.Err() != nil || c.status.State == "canceled" {
				c.status.State = "canceled"
			} else {
				c.status.State = "failed"
			}
		}
		c.status.Digits = ""
		c.mu.Unlock()
		c.cancel()
	}()
	if err = conn.SetDeadline(deadline); err != nil {
		return err
	}
	line, readErr := bufio.NewReaderSize(conn, 256).ReadSlice('\n')
	if readErr != nil || len(line) > 256 {
		return errors.New("配对提示未完成或超过预算")
	}
	passkey, decodeErr := decodeChallenge(line, requestID)
	if decodeErr != nil {
		return decodeErr
	}
	c.mu.Lock()
	c.expireLocked()
	if c.status.State != "waiting_native" {
		c.mu.Unlock()
		return ErrNotPending
	}
	c.status.State = "waiting_confirmation"
	c.status.Digits = fmt.Sprintf("%06d", passkey)
	c.mu.Unlock()
	select {
	case <-c.ctx.Done():
		return c.ctx.Err()
	case digits := <-c.decision:
		if err = c.ctx.Err(); err != nil {
			return err
		}
		if err = json.NewEncoder(conn).Encode(struct {
			RequestID string `json:"request_id"`
			Digits    string `json:"digits"`
		}{requestID, digits}); err != nil {
			return err
		}
		c.mu.Lock()
		if c.status.State == "canceled" || c.ctx.Err() != nil {
			c.mu.Unlock()
			return context.Canceled
		}
		c.status.State = "confirmation_sent"
		c.mu.Unlock()
		return nil
	}
}

func decodeChallenge(line []byte, requestID string) (uint32, error) {
	decoder := json.NewDecoder(bytes.NewReader(line))
	invalid := errors.New("配对提示不属于本轮操作或格式不合法")
	start, err := decoder.Token()
	if err != nil || start != json.Delim('{') {
		return 0, invalid
	}
	seen := make(map[string]bool, 2)
	var id string
	var passkey *uint32
	for decoder.More() {
		field, err := decoder.Token()
		name, ok := field.(string)
		if err != nil || !ok || seen[name] {
			return 0, invalid
		}
		seen[name] = true
		switch name {
		case "request_id":
			if decoder.Decode(&id) != nil {
				return 0, invalid
			}
		case "passkey":
			if decoder.Decode(&passkey) != nil {
				return 0, invalid
			}
		default:
			return 0, invalid
		}
	}
	end, err := decoder.Token()
	if err != nil || end != json.Delim('}') || id != requestID || passkey == nil || *passkey > 999999 {
		return 0, invalid
	}
	var extra any
	if decoder.Decode(&extra) != io.EOF {
		return 0, invalid
	}
	return *passkey, nil
}
