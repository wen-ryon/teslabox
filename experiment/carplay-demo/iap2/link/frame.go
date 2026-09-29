// Package link 实现 iAP2 的链路层（link layer），负责 CSM 帧之下那层的可靠传输。
//
// 链路帧格式（对照 xcertplay 的 Iap2LinkEngine）：
//
//	FF 5A   起始标记
//	u16     payload 长度（不含头尾校验）
//	u8      控制字节：SYN 0x80 / ACK 0x40 / EAK 0x20 / RST 0x10
//	u8      发送序号
//	u8      确认序号
//	u8      会话 ID
//	u8      头部校验和（前 8 个字节之和取负，mod 256）
//	...     payload
//	u8      payload 校验和（payload 之和取负，mod 256）
//
// 链路层要处理的事情：同步（SYN 握手）、按序确认、扩展确认（EAK，即选择重传）、
// 超时重传、链路重置。本文件只做帧的编解码与校验，状态机在 engine.go。
package link

import (
	"encoding/binary"
	"errors"
	"fmt"
)

// 帧结构常量。
const (
	StartHigh = 0xff
	StartLow  = 0x5a

	// HeaderBytes 是链路帧头长度，含头部校验和。
	HeaderBytes = 9

	// ChecksumBytes 是 payload 尾部校验和的字节数。
	ChecksumBytes = 1

	// MaxWireFrameBytes 是一帧在链路上的最大长度。
	MaxWireFrameBytes = 0xffff

	// MaxPayloadBytes 是单帧能承载的最大 payload。
	MaxPayloadBytes = MaxWireFrameBytes - HeaderBytes - ChecksumBytes

	// InitialSequence 是握手后使用的初始序号。
	InitialSequence = 99
)

// 控制位。一帧可以同时带多个控制位（例如 SYN|ACK）。
const (
	ControlSynchronize            = 0x80 // SYN：链路同步
	ControlAcknowledgement        = 0x40 // ACK：确认
	ControlExtendedAcknowledement = 0x20 // EAK：扩展确认，要求选择重传
	ControlReset                  = 0x10 // RST：重置链路
)

// 会话 ID。控制会话跑 CSM，其余会话跑外部配件协议等数据。
const (
	SessionControl      = 10 // 控制会话，跑 CSM
	SessionEA           = 11 // 外部配件协议会话
	SessionFileTransfer = 12 // 文件传输会话
)

// IAP2Marker 是 iPhone 在 USB 上等待的配件握手标记。
// 车机要在枚举完成后先把这 6 个字节发给 iPhone，它才会切进 iAP2 模式。
var IAP2Marker = []byte{0xff, 0x55, 0x02, 0x00, 0xee, 0x10}

// ErrProtocol 表示帧格式或校验和错误。
var ErrProtocol = errors.New("link: 协议错误")

func protocolf(format string, args ...any) error {
	return fmt.Errorf("%w: %s", ErrProtocol, fmt.Sprintf(format, args...))
}

// Header 是链路帧头（不含 payload 与其校验和）。
type Header struct {
	Length          uint16 // payload 字节数
	Control         byte   // 控制位组合
	Sequence        byte   // 发送序号
	Acknowledgement byte   // 确认序号
	SessionID       byte   // 会话 ID
}

// HasControl 判断是否置了某个控制位。
func (h Header) HasControl(bit byte) bool { return h.Control&bit != 0 }

// IsSynchronize 是否是 SYN 帧。
func (h Header) IsSynchronize() bool { return h.HasControl(ControlSynchronize) }

// IsReset 是否是 RST 帧。
func (h Header) IsReset() bool { return h.HasControl(ControlReset) }

// HasPayload 是否携带 payload。
func (h Header) HasPayload() bool { return h.Length > 0 }

// Checksum 计算校验和：取前 count 个字节之和的负值，截断到 8 位。
//
// 这样整段（数据 + 校验和）的字节和 mod 256 恰好为 0，验证时一次求和即可。
func Checksum(data []byte, count int) byte {
	if count > len(data) {
		count = len(data)
	}
	var sum int
	for _, b := range data[:count] {
		sum += int(b)
	}
	return byte(-sum & 0xff)
}

// ChecksumValid 校验一段以校验和结尾的字节是否完好。
func ChecksumValid(data []byte) bool {
	var sum int
	for _, b := range data {
		sum += int(b)
	}
	return sum&0xff == 0
}

// EncodeHeader 编码 9 字节帧头（含头部校验和）。
func EncodeHeader(h Header) []byte {
	out := make([]byte, HeaderBytes)
	out[0] = StartHigh
	out[1] = StartLow
	binary.BigEndian.PutUint16(out[2:], h.Length)
	out[4] = h.Control
	out[5] = h.Sequence
	out[6] = h.Acknowledgement
	out[7] = h.SessionID
	out[8] = Checksum(out, 8)
	return out
}

// ParseHeader 解析 9 字节帧头，校验起始标记与头部校验和。
func ParseHeader(b []byte) (*Header, error) {
	if len(b) != HeaderBytes {
		return nil, protocolf("帧头 %d 字节，应为 %d", len(b), HeaderBytes)
	}
	if b[0] != StartHigh || b[1] != StartLow {
		return nil, protocolf("起始标记不是 FF 5A，得到 %02x %02x", b[0], b[1])
	}
	if !ChecksumValid(b) {
		return nil, protocolf("帧头校验和错误")
	}
	return &Header{
		Length:          binary.BigEndian.Uint16(b[2:]),
		Control:         b[4],
		Sequence:        b[5],
		Acknowledgement: b[6],
		SessionID:       b[7],
	}, nil
}

// Frame 是一条完整的链路帧（头 + payload + payload 校验和）。
type Frame struct {
	Header  Header
	Payload []byte
}

// Encode 编码完整的链路帧。payload 可以为空（例如纯 ACK）。
func (f *Frame) Encode() ([]byte, error) {
	if len(f.Payload) > MaxPayloadBytes {
		return nil, protocolf("payload %d 字节，超过上限 %d", len(f.Payload), MaxPayloadBytes)
	}
	h := f.Header
	h.Length = uint16(len(f.Payload))

	out := make([]byte, 0, HeaderBytes+len(f.Payload)+ChecksumBytes)
	out = append(out, EncodeHeader(h)...)
	out = append(out, f.Payload...)
	out = append(out, Checksum(f.Payload, len(f.Payload)))
	return out, nil
}

// EncodeFrame 是 Encode 的便捷形式，出错时 panic（仅用于常量帧）。
func MustEncode(f *Frame) []byte {
	out, err := f.Encode()
	if err != nil {
		panic(err)
	}
	return out
}

// SequenceDistance 计算序号的前进距离（考虑 8 位回绕）。
func SequenceDistance(sequence, previous byte) byte { return sequence - previous }

// NextSequence 返回下一个序号（8 位回绕）。
func NextSequence(sequence byte) byte { return sequence + 1 }

// Framer 从字节流中重组链路帧。
// 会跳过起始标记之前的垃圾字节，并按长度字段等待整帧到齐。
type Framer struct {
	buf []byte
}

// NewFramer 构造重组器。
func NewFramer() *Framer { return &Framer{} }

// Offer 喂入字节流，返回本次新解析出的完整帧（payload 已去掉尾部校验和）。
// 校验和不通过的帧会被丢弃——链路层靠重传恢复，不需要向上层报告坏帧。
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

func (f *Framer) tryDrain() (*Frame, int, bool) {
	skipped := 0
	for len(f.buf)-skipped >= 2 {
		if f.buf[skipped] == StartHigh && f.buf[skipped+1] == StartLow {
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
	header, err := ParseHeader(f.buf[:HeaderBytes])
	if err != nil {
		// 头坏了（多半是校验和），跳过一个字节重新找同步。
		f.buf = f.buf[1:]
		return nil, 0, true
	}
	total := HeaderBytes + int(header.Length) + ChecksumBytes
	if len(f.buf) < total {
		return nil, 0, false
	}

	payloadWithChecksum := f.buf[HeaderBytes:total]
	var frame *Frame
	if ChecksumValid(payloadWithChecksum) {
		frame = &Frame{
			Header:  *header,
			Payload: append([]byte(nil), payloadWithChecksum[:len(payloadWithChecksum)-ChecksumBytes]...),
		}
	}
	return frame, total, false
}

// Pending 返回未解析的字节数。
func (f *Framer) Pending() int { return len(f.buf) }
