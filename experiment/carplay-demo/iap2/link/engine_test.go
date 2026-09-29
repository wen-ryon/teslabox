package link

import (
	"testing"
	"time"
)

func TestSynchronizationLayout(t *testing.T) {
	// 逐字节验证 SYN 载荷布局，顺序错一个字段 iPhone 就不会完成握手。
	s := Synchronization{
		Version:                1,
		MaxOutgoing:            4,
		MaxLength:              0xffff,
		RetransmissionTimeout:  4 * time.Second,
		AcknowledgementTimeout: 500 * time.Millisecond,
		MaxRetransmissions:     4,
		MaxAcknowledgements:    3,
		Sessions:               DefaultSessions(1),
	}
	enc := EncodeSynchronization(s)

	if len(enc) != SynchronizationFixedBytes+3*SessionDescriptorBytes {
		t.Fatalf("载荷 %d 字节，期望 %d", len(enc), SynchronizationFixedBytes+3*SessionDescriptorBytes)
	}
	if enc[0] != 1 {
		t.Errorf("版本 = %d, 期望 1", enc[0])
	}
	if enc[1] != 4 {
		t.Errorf("maxOutgoing = %d, 期望 4", enc[1])
	}
	if got := uint16(enc[2])<<8 | uint16(enc[3]); got != 0xffff {
		t.Errorf("maxLength = %#x, 期望 0xffff", got)
	}
	if got := uint16(enc[4])<<8 | uint16(enc[5]); got != 4000 {
		t.Errorf("重传超时 = %d ms, 期望 4000", got)
	}
	if got := uint16(enc[6])<<8 | uint16(enc[7]); got != 500 {
		t.Errorf("确认超时 = %d ms, 期望 500", got)
	}
	if enc[8] != 4 {
		t.Errorf("最大重传 = %d, 期望 4", enc[8])
	}
	if enc[9] != 3 {
		t.Errorf("最大累积确认 = %d, 期望 3", enc[9])
	}
}

func TestSynchronizationSessions(t *testing.T) {
	// 三个会话的 (ID, 类型, 版本) 必须与 xcertplay 一致，
	// 尤其文件传输会话必须是版本 2。
	enc := EncodeSynchronization(Synchronization{
		Version:  1,
		Sessions: DefaultSessions(1),
	})
	parsed, err := ParseSynchronization(enc)
	if err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	if len(parsed.Sessions) != 3 {
		t.Fatalf("会话数 %d, 期望 3", len(parsed.Sessions))
	}
	want := []SessionDescriptor{
		{SessionControl, SessionKindControl, 1},
		{SessionEA, SessionKindEA, 1},
		{SessionFileTransfer, SessionKindFileTransfer, 2},
	}
	for i, w := range want {
		if parsed.Sessions[i] != w {
			t.Errorf("会话 %d = %+v, 期望 %+v", i, parsed.Sessions[i], w)
		}
	}
}

func TestSynchronizationRejectsUnknownVersion(t *testing.T) {
	enc := EncodeSynchronization(Synchronization{Version: 1, Sessions: DefaultSessions(1)})
	enc[0] = 2
	if _, err := ParseSynchronization(enc); err == nil {
		t.Error("未知版本应被拒绝")
	}
}

func TestHandshakeEstablishes(t *testing.T) {
	engine := NewEngine(DefaultConfig())
	now := time.Now()

	syn := engine.Start(now)
	if len(syn) == 0 {
		t.Fatal("Start 没有产生 SYN 帧")
	}
	if engine.State() != StateSynchronizing {
		t.Fatalf("状态 = %v, 期望 synchronizing", engine.State())
	}

	// 对端回一个 SYN|ACK。
	reply, err := (&Frame{
		Header: Header{
			Control:   ControlSynchronize | ControlAcknowledgement,
			Sequence:  InitialSequence,
			SessionID: SessionControl,
		},
		Payload: engine.localSynchronization(),
	}).Encode()
	if err != nil {
		t.Fatalf("构造应答失败: %v", err)
	}

	engine.Offer(reply)
	if !engine.Established() {
		t.Fatalf("收到 SYN|ACK 后仍未建立链路，状态 = %v", engine.State())
	}
}

func TestSendRequiresEstablishedLink(t *testing.T) {
	engine := NewEngine(DefaultConfig())
	if err := engine.Send(SessionControl, []byte{0x01}); err != ErrNotEstablished {
		t.Errorf("未同步时发送应返回 ErrNotEstablished，得到 %v", err)
	}
}

func TestDataFrameIsAcknowledged(t *testing.T) {
	engine := NewEngine(DefaultConfig())
	establish(t, engine)

	if err := engine.Send(SessionControl, []byte{0x40, 0x40}); err != nil {
		t.Fatalf("发送失败: %v", err)
	}
	out := engine.Poll(time.Now())
	if len(out) == 0 {
		t.Fatal("Poll 没有产出数据帧")
	}
	if len(engine.unacknowledged) != 1 {
		t.Fatalf("待确认队列长度 %d, 期望 1", len(engine.unacknowledged))
	}

	// 对端确认这一帧。
	sent := engine.unacknowledged[0]
	ack := MustEncodeFrame(Header{
		Control:         ControlAcknowledgement,
		Sequence:        InitialSequence,
		Acknowledgement: NextSequence(sent.sequence),
		SessionID:       SessionControl,
	}, nil)
	engine.Offer(ack)
	if len(engine.unacknowledged) != 0 {
		t.Errorf("确认后待确认队列仍有 %d 个包", len(engine.unacknowledged))
	}
}

func TestResetDropsQueues(t *testing.T) {
	engine := NewEngine(DefaultConfig())
	establish(t, engine)
	_ = engine.Send(SessionControl, []byte{0x01})
	_ = engine.Poll(time.Now())

	rst := MustEncodeFrame(Header{Control: ControlReset, SessionID: SessionControl}, nil)
	engine.Offer(rst)
	if engine.State() != StateReset {
		t.Errorf("状态 = %v, 期望 reset", engine.State())
	}
	if len(engine.unacknowledged) != 0 || len(engine.queued) != 0 {
		t.Error("重置后队列应被清空")
	}
}

// establish 走完同步握手，供其他测试复用。
func establish(t *testing.T, engine *Engine) {
	t.Helper()
	_ = engine.Start(time.Now())
	reply, err := (&Frame{
		Header: Header{
			Control:   ControlSynchronize | ControlAcknowledgement,
			Sequence:  InitialSequence,
			SessionID: SessionControl,
		},
		Payload: engine.localSynchronization(),
	}).Encode()
	if err != nil {
		t.Fatalf("构造 SYN|ACK 失败: %v", err)
	}
	engine.Offer(reply)
	if !engine.Established() {
		t.Fatalf("链路未能建立，状态 = %v", engine.State())
	}
}

// MustEncodeFrame 按帧头与载荷编码一帧，失败即 panic。
func MustEncodeFrame(h Header, payload []byte) []byte {
	out, err := (&Frame{Header: h, Payload: payload}).Encode()
	if err != nil {
		panic(err)
	}
	return out
}
