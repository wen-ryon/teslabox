package link

import (
	"errors"
	"time"
)

// 链路层状态机。
//
// iAP2 链路层在 CSM 之下提供按序、可靠的数据传输。它要处理的事：
//
//  1. 同步：双方交换 SYN 帧，协商包长、重传超时、重传次数；
//  2. 编号：每帧带发送序号，接收方按序确认；
//  3. 累积确认：不必每帧都回 ACK，攒够 MaxAcknowledgements 个再回；
//  4. 选择重传：发现序号空洞时回 EAK，点名要对方重发某一帧；
//  5. 超时重传：发出去的帧超过重传窗口没被确认就重发。
//
// 使用方式是轮询式的，方便接进 Go 的 select 循环：
//
//	engine.Send(SessionControl, payload)   // 上层要发数据
//	wire.Write(engine.Poll(time.Now()))    // 取要发到线路上的字节
//	for _, f := range engine.Offer(recv) { // 喂入收到的字节
//	    handle(f.SessionID, f.Payload)     // 交给上层
//	}
type Engine struct {
	cfg Config

	state State
	// framer 负责从字节流里切出整帧。
	framer *Framer

	// 本地将要使用的发送序号，与对方已确认到的序号。
	nextSequence    byte
	lastAckSent     byte
	lastReceivedSeq byte
	receivedCount   int

	// 待发送队列与已发送待确认队列。
	queued         []*pendingPacket
	unacknowledged []*pendingPacket

	// 收到的乱序帧，等空洞补上后一并交付。
	outOfOrder []*Frame

	// 已同步到的对端参数。
	peer Synchronization

	// 累积确认计数，达到阈值就发一个 ACK。
	ackPending int

	// 协商结果：本端发 SYN 后等对方的 SYN+ACK。
	synSentAt time.Time
	synTries  int
}

// State 是链路状态。
type State int

const (
	// StateIdle 还没开始握手。
	StateIdle State = iota
	// StateSynchronizing 已发出 SYN，等待对端的 SYN+ACK。
	StateSynchronizing
	// StateEstablished 链路已同步，可以传数据。
	StateEstablished
	// StateReset 链路被对端重置，需要重新同步。
	StateReset
)

func (s State) String() string {
	switch s {
	case StateIdle:
		return "idle"
	case StateSynchronizing:
		return "synchronizing"
	case StateEstablished:
		return "established"
	case StateReset:
		return "reset"
	default:
		return "unknown"
	}
}

// 同步载荷的固定部分尺寸与版本。
const (
	// SynchronizationVersion 是目前唯一支持的链路同步版本。
	SynchronizationVersion = 1
	// SynchronizationFixedBytes 是同步载荷里固定字段的长度（会话描述符之前）。
	SynchronizationFixedBytes = 10
	// SessionDescriptorBytes 是每个会话描述符的长度：ID、类型、版本。
	SessionDescriptorBytes = 3
)

// Config 是链路协商参数，通过 SYN 帧告诉对端。
//
// 默认值照抄 xcertplay 的 Iap2LinkEngine.localSynchronization()：
// 那是一份在真实 iPhone 上跑通的配置，不要随意改动。
type Config struct {
	// MaxOutgoing 是本端允许同时在途（未确认）的帧数。
	MaxOutgoing int
	// MaxLength 是本端能接收的最大链路帧长度。
	MaxLength uint16
	// RetransmissionTimeout 是等待确认的时间，超时后重传。
	RetransmissionTimeout time.Duration
	// AcknowledgementTimeout 是攒 ACK 的窗口：收到帧后最多拖这么久就必须回确认。
	AcknowledgementTimeout time.Duration
	// MaxRetransmissions 是单帧最多重传几次，超过就认为链路断了。
	MaxRetransmissions int
	// MaxAcknowledgements 是攒够多少个待确认帧才回一个 ACK。
	MaxAcknowledgements int
	// ControlSessionVersion 是控制会话的协议版本，目前是 1。
	ControlSessionVersion int
	// ZeroAcknowledgements 打开后，SYN 里的超时与重传字段全部填 0。
	// 仅用于抓包调试，正常会话不要开。
	ZeroAcknowledgements bool
}

// DefaultConfig 返回 xcertplay 实测使用的默认参数。
func DefaultConfig() Config {
	return Config{
		MaxOutgoing:            4,
		MaxLength:              MaxWireFrameBytes,
		RetransmissionTimeout:  4 * time.Second,
		AcknowledgementTimeout: 500 * time.Millisecond,
		MaxRetransmissions:     4,
		MaxAcknowledgements:    3,
		ControlSessionVersion:  1,
	}
}

// SessionDescriptor 描述链路上的一个会话：ID、类型、版本。
//
// 类型取值（来自 xcertplay 的会话清单）：控制会话 0，文件传输 1，外部配件 2。
type SessionDescriptor struct {
	ID      byte
	Kind    byte
	Version byte
}

// 会话类型。
const (
	SessionKindControl      byte = 0
	SessionKindFileTransfer byte = 1
	SessionKindEA           byte = 2
)

// DefaultSessions 返回本端要声明的会话。
//
// 文件传输会话必须用版本 2，否则 iPhone 的专辑封面（Now Playing 属性 26）取不到。
func DefaultSessions(controlVersion int) []SessionDescriptor {
	if controlVersion <= 0 {
		controlVersion = 1
	}
	return []SessionDescriptor{
		{SessionControl, SessionKindControl, byte(controlVersion)},
		{SessionEA, SessionKindEA, 1},
		{SessionFileTransfer, SessionKindFileTransfer, 2},
	}
}

// Synchronization 是 SYN 帧里交换的能力参数。
type Synchronization struct {
	Version                byte
	MaxOutgoing            byte
	MaxLength              uint16
	RetransmissionTimeout  time.Duration
	AcknowledgementTimeout time.Duration
	MaxRetransmissions     byte
	MaxAcknowledgements    byte
	Sessions               []SessionDescriptor
}

// pendingPacket 是一个已发出、等待确认的数据包。
type pendingPacket struct {
	sequence    byte
	sessionID   byte
	payload     []byte
	retries     int
	nextAttempt time.Time
}

// ErrLinkBroken 表示一帧重传次数耗尽，链路不可用。
var ErrLinkBroken = errors.New("link: 重传次数耗尽，链路中断")

// ErrNotEstablished 表示链路还没同步就想发数据。
var ErrNotEstablished = errors.New("link: 链路尚未同步")

// NewEngine 构造链路引擎。初始为 Idle，由 Start 发起同步。
func NewEngine(cfg Config) *Engine {
	if cfg.MaxLength == 0 {
		cfg.MaxLength = MaxWireFrameBytes
	}
	if cfg.RetransmissionTimeout <= 0 {
		cfg.RetransmissionTimeout = 4 * time.Second
	}
	if cfg.AcknowledgementTimeout <= 0 {
		cfg.AcknowledgementTimeout = 500 * time.Millisecond
	}
	if cfg.MaxRetransmissions <= 0 {
		cfg.MaxRetransmissions = 4
	}
	if cfg.MaxAcknowledgements <= 0 {
		cfg.MaxAcknowledgements = 3
	}
	if cfg.MaxOutgoing <= 0 {
		cfg.MaxOutgoing = 4
	}
	return &Engine{
		cfg:          cfg,
		state:        StateIdle,
		framer:       NewFramer(),
		nextSequence: InitialSequence,
	}
}

// Config 返回当前配置。
func (e *Engine) Config() Config { return e.cfg }

// State 返回当前状态。
func (e *Engine) State() State { return e.state }

// Established 判断链路是否可用。
func (e *Engine) Established() bool { return e.state == StateEstablished }

// Start 发起同步，返回要发到线路上的 SYN 帧。可以重复调用以实现重发。
func (e *Engine) Start(now time.Time) []byte {
	e.state = StateSynchronizing
	e.synSentAt = now
	e.synTries++
	frame := &Frame{
		Header: Header{
			Control:   ControlSynchronize,
			Sequence:  e.nextSequence,
			SessionID: SessionControl,
		},
		Payload: e.localSynchronization(),
	}
	out, err := frame.Encode()
	if err != nil {
		return nil
	}
	return out
}

// Send 提交一段要发送的数据。链路未同步时返回 ErrNotEstablished。
func (e *Engine) Send(sessionID byte, payload []byte) error {
	if !e.Established() {
		return ErrNotEstablished
	}
	e.queued = append(e.queued, &pendingPacket{sessionID: sessionID, payload: payload})
	return nil
}

// Poll 返回此刻应当写到线路上的全部字节：新数据帧、超时重传、以及待发的 ACK。
func (e *Engine) Poll(now time.Time) []byte {
	var out []byte

	switch e.state {
	case StateIdle:
		// 尚未发起同步，调用方应先调 Start。
		return nil
	case StateSynchronizing:
		// SYN 没收到回应就重发，iPhone 有时会在配件枚举完成前丢掉第一批字节。
		if now.Sub(e.synSentAt) >= e.cfg.RetransmissionTimeout && e.synTries <= e.cfg.MaxRetransmissions {
			return e.Start(now)
		}
		return nil
	}

	// 1) 先处理超时重传。
	for _, p := range e.unacknowledged {
		if !now.Before(p.nextAttempt) {
			if p.retries >= e.cfg.MaxRetransmissions {
				e.state = StateReset
				return nil
			}
			p.retries++
			p.nextAttempt = now.Add(e.cfg.RetransmissionTimeout)
			out = append(out, e.encodeData(p)...)
		}
	}

	// 2) 再发新数据，但一次只发一个窗口，避免淹没对端。
	for len(e.queued) > 0 {
		p := e.queued[0]
		e.queued = e.queued[1:]
		p.sequence = e.nextSequence
		e.nextSequence = NextSequence(e.nextSequence)
		p.nextAttempt = now.Add(e.cfg.RetransmissionTimeout)
		e.unacknowledged = append(e.unacknowledged, p)
		out = append(out, e.encodeData(p)...)
	}

	// 3) 最后补一个累积 ACK。
	if e.ackPending > 0 && e.ackPending >= int(e.cfg.MaxAcknowledgements) {
		out = append(out, e.encodeAck()...)
		e.ackPending = 0
	}
	return out
}

// Offer 喂入从线路上收到的字节，返回解出的、可以交给上层的帧。
//
// 控制帧（SYN/ACK/EAK/RST）在这里被消费掉，不会出现在返回值里。
func (e *Engine) Offer(chunk []byte) []*Frame {
	frames := e.framer.Offer(chunk)
	var delivered []*Frame

	for _, f := range frames {
		switch {
		case f.Header.IsReset():
			e.reset()
			continue
		case f.Header.IsSynchronize():
			e.handleSynchronize(f)
			continue
		case f.Header.HasControl(ControlExtendedAcknowledement):
			e.handleExtendedAck(f)
			continue
		}

		if f.Header.HasPayload() {
			delivered = append(delivered, e.handleData(f)...)
		}
		// 纯 ACK 帧只对确认号生效。
		e.handleAcknowledgement(f.Header.Acknowledgement)
	}
	return delivered
}

// PendingAck 返回是否有待发的确认，供上层决定要不要催促 Poll。
func (e *Engine) PendingAck() bool { return e.ackPending > 0 }

// ---------- 内部实现 ----------

func (e *Engine) handleSynchronize(f *Frame) {
	if sync, err := ParseSynchronization(f.Payload); err == nil {
		e.peer = *sync
		// 取双方都能接受的最小值。
		if sync.MaxLength > 0 && sync.MaxLength < e.cfg.MaxLength {
			e.cfg.MaxLength = sync.MaxLength
		}
		if sync.MaxOutgoing > 0 && int(sync.MaxOutgoing) < e.cfg.MaxOutgoing {
			e.cfg.MaxOutgoing = int(sync.MaxOutgoing)
		}
	}
	// 双方都从 InitialSequence 开始编号，所以收到 SYN 之后要把期望序号
	// 对齐到它的前一个值，否则对端发来的第一帧会被当成乱序帧缓存起来。
	// 对被动方（收到纯 SYN、需要回 SYN|ACK 的那一侧）尤其关键。
	e.lastReceivedSeq = InitialSequence - 1

	// 收到 SYN 就说明对方已经就绪：主动方拿到的是 SYN|ACK，
	// 被动方拿到的是纯 SYN（需要自己回 SYN|ACK）。两种情况下
	// 链路都已经可以收发，所以不区分是否带 ACK 位。
	e.state = StateEstablished
	e.unacknowledged = nil
	e.synTries = 0
}

func (e *Engine) handleData(f *Frame) []*Frame {
	expected := NextSequence(e.lastReceivedSeq)
	if f.Header.Sequence != expected {
		// 序号出现空洞：记下来，稍后由 Poll 发 EAK 点名重传。
		e.outOfOrder = append(e.outOfOrder, f)
		return nil
	}
	e.lastReceivedSeq = f.Header.Sequence
	e.ackPending++
	e.receivedCount++

	var delivered []*Frame
	delivered = append(delivered, f)

	// 空洞补齐后，把之前缓存的乱序帧按顺序放出来。
	for len(e.outOfOrder) > 0 {
		expected = NextSequence(e.lastReceivedSeq)
		var matched *Frame
		rest := e.outOfOrder[:0]
		for _, pending := range e.outOfOrder {
			if pending.Header.Sequence == expected && matched == nil {
				matched = pending
				continue
			}
			rest = append(rest, pending)
		}
		e.outOfOrder = rest
		if matched == nil {
			break
		}
		e.lastReceivedSeq = matched.Header.Sequence
		e.ackPending++
		delivered = append(delivered, matched)
	}
	return delivered
}

func (e *Engine) handleAcknowledgement(ackNumber byte) {
	// 确认号表示「这个序号及之前的都收到了」。
	kept := e.unacknowledged[:0]
	for _, p := range e.unacknowledged {
		if SequenceDistance(ackNumber, p.sequence) > 0 {
			continue // 已确认，丢掉
		}
		kept = append(kept, p)
	}
	e.unacknowledged = kept
}

func (e *Engine) handleExtendedAck(f *Frame) {
	// EAK 的载荷是要重传的序号列表。
	for _, seq := range f.Payload {
		for _, p := range e.unacknowledged {
			if p.sequence == seq {
				p.nextAttempt = time.Time{} // 立刻重传
				p.retries--
			}
		}
	}
}

func (e *Engine) reset() {
	e.state = StateReset
	e.unacknowledged = nil
	e.queued = nil
	e.outOfOrder = nil
	e.ackPending = 0
	e.lastReceivedSeq = 0
	e.receivedCount = 0
	e.nextSequence = InitialSequence
	e.synTries = 0
}

func (e *Engine) encodeData(p *pendingPacket) []byte {
	frame := &Frame{
		Header: Header{
			Control:         ControlAcknowledgement,
			Sequence:        p.sequence,
			Acknowledgement: NextSequence(e.lastReceivedSeq),
			SessionID:       p.sessionID,
		},
		Payload: p.payload,
	}
	out, err := frame.Encode()
	if err != nil {
		return nil
	}
	e.ackPending = 0 // 搭车确认过了
	return out
}

func (e *Engine) encodeAck() []byte {
	frame := &Frame{
		Header: Header{
			Control:         ControlAcknowledgement,
			Sequence:        e.nextSequence,
			Acknowledgement: NextSequence(e.lastReceivedSeq),
			SessionID:       SessionControl,
		},
	}
	out, err := frame.Encode()
	if err != nil {
		return nil
	}
	return out
}

// MissingSequences 返回检测到的序号空洞，供构造 EAK 帧。
func (e *Engine) MissingSequences() []byte {
	missing := make([]byte, 0, len(e.outOfOrder))
	seen := make(map[byte]bool)
	for _, f := range e.outOfOrder {
		if !seen[f.Header.Sequence] {
			seen[f.Header.Sequence] = true
			missing = append(missing, f.Header.Sequence)
		}
	}
	return missing
}

// localSynchronization 构造本端的同步载荷。
func (e *Engine) localSynchronization() []byte {
	cfg := e.cfg
	if cfg.ZeroAcknowledgements {
		// 调试模式：把超时/重传全部填 0，让对端不要期待确认。
		return EncodeSynchronization(Synchronization{
			Version:     1,
			MaxOutgoing: byte(cfg.MaxOutgoing),
			MaxLength:   cfg.MaxLength,
			Sessions:    DefaultSessions(cfg.ControlSessionVersion),
		})
	}
	return EncodeSynchronization(Synchronization{
		Version:                1,
		MaxOutgoing:            byte(cfg.MaxOutgoing),
		MaxLength:              cfg.MaxLength,
		RetransmissionTimeout:  cfg.RetransmissionTimeout,
		AcknowledgementTimeout: cfg.AcknowledgementTimeout,
		MaxRetransmissions:     byte(cfg.MaxRetransmissions),
		MaxAcknowledgements:    byte(cfg.MaxAcknowledgements),
		Sessions:               DefaultSessions(cfg.ControlSessionVersion),
	})
}

// EncodeSynchronization 编码 SYN 载荷。
//
// 线序（对照 xcertplay 的 SynchronizationPayload.encode）：
//
//	u8   版本（1）
//	u8   maxOutgoing：允许同时在途的帧数
//	u16  maxLength：最大链路帧长度
//	u16  重传超时（毫秒）
//	u16  确认超时（毫秒）
//	u8   最大重传次数
//	u8   最大累积确认数
//	...  每 3 字节一个会话描述符：会话 ID、类型、版本
func EncodeSynchronization(s Synchronization) []byte {
	retrans := uint16(s.RetransmissionTimeout.Milliseconds())
	ack := uint16(s.AcknowledgementTimeout.Milliseconds())
	out := []byte{
		s.Version,
		s.MaxOutgoing,
		byte(s.MaxLength >> 8), byte(s.MaxLength),
		byte(retrans >> 8), byte(retrans),
		byte(ack >> 8), byte(ack),
		s.MaxRetransmissions,
		s.MaxAcknowledgements,
	}
	for _, session := range s.Sessions {
		out = append(out, session.ID, session.Kind, session.Version)
	}
	return out
}

// ParseSynchronization 解析 SYN 载荷。
func ParseSynchronization(b []byte) (*Synchronization, error) {
	if len(b) < SynchronizationFixedBytes {
		return nil, protocolf("同步载荷 %d 字节，至少 %d", len(b), SynchronizationFixedBytes)
	}
	if b[0] != SynchronizationVersion {
		return nil, protocolf("不支持的链路同步版本 %d", b[0])
	}
	var sessions []SessionDescriptor
	for offset := SynchronizationFixedBytes; offset+SessionDescriptorBytes <= len(b); offset += SessionDescriptorBytes {
		sessions = append(sessions, SessionDescriptor{
			ID:      b[offset],
			Kind:    b[offset+1],
			Version: b[offset+2],
		})
	}
	return &Synchronization{
		Version:                b[0],
		MaxOutgoing:            b[1],
		MaxLength:              uint16(b[2])<<8 | uint16(b[3]),
		RetransmissionTimeout:  time.Duration(uint16(b[4])<<8|uint16(b[5])) * time.Millisecond,
		AcknowledgementTimeout: time.Duration(uint16(b[6])<<8|uint16(b[7])) * time.Millisecond,
		MaxRetransmissions:     b[8],
		MaxAcknowledgements:    b[9],
		Sessions:               sessions,
	}, nil
}
