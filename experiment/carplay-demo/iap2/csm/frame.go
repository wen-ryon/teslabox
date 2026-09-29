package csm

import (
	"bytes"
	"fmt"
)

// Parameter 是消息体里的一个参数：u16 总长 | u16 参数ID | 载荷。
type Parameter struct {
	ID      uint16
	payload []byte
}

// NewParameter 构造参数。载荷长度溢出时返回错误。
func NewParameter(id uint16, payload []byte) (*Parameter, error) {
	if len(payload) > MaxParamBytes-ParamHeaderBytes {
		return nil, protocolf("参数载荷 %d 字节，超过上限 %d", len(payload), MaxParamBytes-ParamHeaderBytes)
	}
	return &Parameter{ID: id, payload: bytes.Clone(payload)}, nil
}

// MustParameter 同 NewParameter，失败时 panic。用于编译期已知的常量参数。
func MustParameter(id uint16, payload []byte) *Parameter {
	p, err := NewParameter(id, payload)
	if err != nil {
		panic(err)
	}
	return p
}

// Payload 返回载荷的副本。
func (p *Parameter) Payload() []byte { return bytes.Clone(p.payload) }

// Encode 编码成完整的参数字节序列（含 4 字节头）。
func (p *Parameter) Encode() []byte {
	out := make([]byte, 0, ParamHeaderBytes+len(p.payload))
	out = append(out, U16(uint16(ParamHeaderBytes+len(p.payload)))...)
	out = append(out, U16(p.ID)...)
	out = append(out, p.payload...)
	return out
}

func (p *Parameter) String() string {
	return fmt.Sprintf("Parameter(0x%04x, %d bytes)", p.ID, len(p.payload))
}

// List 是有序的参数集合。
//
// 用切片而不是 map：iAP2 允许重复的参数 ID，而且顺序在协议里是有意义的，
// 转成 map 会同时丢掉重复项和顺序。
type List struct {
	params []*Parameter
}

// NewList 构造参数列表。
func NewList(params ...*Parameter) *List {
	return &List{params: append([]*Parameter(nil), params...)}
}

// Empty 返回空列表。
func Empty() *List { return &List{} }

// Size 返回参数个数。
func (l *List) Size() int { return len(l.params) }

// IsEmpty 判断是否为空。
func (l *List) IsEmpty() bool { return len(l.params) == 0 }

// At 按下标取参数。
func (l *List) At(i int) *Parameter { return l.params[i] }

// All 返回全部参数的副本。
func (l *List) All() []*Parameter { return append([]*Parameter(nil), l.params...) }

// Has 判断是否含有某个参数 ID。
func (l *List) Has(id uint16) bool {
	for _, p := range l.params {
		if p.ID == id {
			return true
		}
	}
	return false
}

// First 返回第一个匹配的参数，没有则 nil。
// Append 追加参数并返回自身，便于链式构造。
// 追加顺序即线序：iAP2 里同一 ID 重复出现、以及参数先后，都是有意义的。
func (l *List) Append(params ...*Parameter) *List {
	l.params = append(l.params, params...)
	return l
}

func (l *List) First(id uint16) *Parameter {
	for _, p := range l.params {
		if p.ID == id {
			return p
		}
	}
	return nil
}

// Get 返回第一个匹配的参数，缺失时返回错误。
func (l *List) Get(id uint16) (*Parameter, error) {
	if p := l.First(id); p != nil {
		return p, nil
	}
	return nil, protocolf("缺少必需的参数 0x%04x", id)
}

// Same 返回所有相同 ID 的参数（协议允许重复）。
func (l *List) Same(id uint16) []*Parameter {
	var out []*Parameter
	for _, p := range l.params {
		if p.ID == id {
			out = append(out, p)
		}
	}
	return out
}

// Encode 把全部参数编码成消息体。
func (l *List) Encode() ([]byte, error) {
	out := make([]byte, 0, 64)
	for _, p := range l.params {
		encoded := p.Encode()
		if len(out)+len(encoded) > MaxBodyBytes {
			return nil, protocolf("消息体超过 %d 字节上限", MaxBodyBytes)
		}
		out = append(out, encoded...)
	}
	return out, nil
}

// ParseList 解析消息体。会拒绝截断的头部、非法的长度和末尾多余字节。
func ParseList(body []byte) (*List, error) {
	var params []*Parameter
	offset := 0
	for offset < len(body) {
		if len(body)-offset < ParamHeaderBytes {
			return nil, protocolf("参数头被截断（剩 %d 字节）", len(body)-offset)
		}
		length, err := ReadU16(body, offset)
		if err != nil {
			return nil, err
		}
		if int(length) < ParamHeaderBytes || int(length) > len(body)-offset {
			return nil, protocolf("参数长度 %d 非法（剩余 %d 字节）", length, len(body)-offset)
		}
		id, err := ReadU16(body, offset+2)
		if err != nil {
			return nil, err
		}
		params = append(params, &Parameter{
			ID:      id,
			payload: bytes.Clone(body[offset+ParamHeaderBytes : offset+int(length)]),
		})
		offset += int(length)
	}
	return &List{params: params}, nil
}

// Frame 是一条完整的控制会话消息：u16 消息ID + 消息体。
type Frame struct {
	MessageID uint16
	body      []byte
}

// NewFrame 构造一条消息。
func NewFrame(messageID uint16, body []byte) (*Frame, error) {
	if len(body) > MaxBodyBytes {
		return nil, protocolf("消息体 %d 字节，超过上限 %d", len(body), MaxBodyBytes)
	}
	return &Frame{MessageID: messageID, body: bytes.Clone(body)}, nil
}

// NewFrameOf 用参数列表构造一条消息。
func NewFrameOf(messageID uint16, params *List) (*Frame, error) {
	body, err := params.Encode()
	if err != nil {
		return nil, err
	}
	return NewFrame(messageID, body)
}

// Body 返回消息体副本。
func (f *Frame) Body() []byte { return bytes.Clone(f.body) }

// Params 解析消息体。
func (f *Frame) Params() (*List, error) { return ParseList(f.body) }

// Encode 编码成完整的 CSM 帧（含 6 字节头）。
func (f *Frame) Encode() []byte {
	out := make([]byte, 0, HeaderBytes+len(f.body))
	out = append(out, U16(Start)...)
	out = append(out, U16(uint16(HeaderBytes+len(f.body)))...)
	out = append(out, U16(f.MessageID)...)
	out = append(out, f.body...)
	return out
}

func (f *Frame) String() string {
	return fmt.Sprintf("Frame(0x%04x, %d bytes)", f.MessageID, len(f.body))
}

// Framer 从字节流中重组出完整的 CSM 帧。
//
// 传输层（TCP、USB bulk）不保证一帧一次到齐，也不保证一次只来一帧，
// 所以需要这个缓冲：容忍任意分片、多帧粘连、以及 0x4040 之前的垃圾字节。
type Framer struct {
	buf []byte
}

// NewFramer 构造一个空的重组器。
func NewFramer() *Framer { return &Framer{} }

// Offer 喂入一段字节流，返回本次新解析出的所有完整帧。
func (f *Framer) Offer(chunk []byte) []*Frame {
	var frames []*Frame
	if len(chunk) == 0 {
		return frames
	}
	f.buf = append(f.buf, chunk...)
	for {
		frame, consumed, retry := f.tryDrain()
		if retry {
			continue
		}
		if consumed == 0 {
			break
		}
		f.buf = f.buf[consumed:]
		if frame != nil {
			frames = append(frames, frame)
		}
	}
	return frames
}

// tryDrain 尝试从缓冲区取出一帧。
// 返回 (帧, 消耗的字节数, 是否应继续尝试)。
func (f *Framer) tryDrain() (*Frame, int, bool) {
	// 跳过起始标记之前的垃圾字节。
	skipped := 0
	for len(f.buf)-skipped >= 2 {
		if f.buf[skipped] == 0x40 && f.buf[skipped+1] == 0x40 {
			break
		}
		skipped++
	}
	if skipped > 0 {
		f.buf = f.buf[skipped:]
		return nil, skipped, true
	}
	if len(f.buf) < HeaderBytes {
		return nil, 0, false
	}
	length, err := ReadU16(f.buf, 2)
	if err != nil {
		return nil, 0, false
	}
	if int(length) < HeaderBytes {
		// 长度字段非法，丢弃一个字节后重新找同步。
		f.buf = f.buf[1:]
		return nil, 0, true
	}
	if len(f.buf) < int(length) {
		return nil, 0, false // 还没收满，等下一批
	}
	messageID, err := ReadU16(f.buf, 4)
	if err != nil {
		return nil, 0, false
	}
	frame := &Frame{
		MessageID: messageID,
		body:      bytes.Clone(f.buf[HeaderBytes:length]),
	}
	return frame, int(length), false
}

// Pending 返回缓冲区里尚未解析的字节数，便于诊断卡帧。
func (f *Framer) Pending() int { return len(f.buf) }
