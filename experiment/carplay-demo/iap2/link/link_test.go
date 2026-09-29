package link

import (
	"bytes"
	"testing"
)

func TestChecksumSumIsZero(t *testing.T) {
	// 校验和的定义就是让整段字节和 mod 256 归零，这是验证端唯一的判据。
	for _, data := range [][]byte{
		{},
		{0x01},
		{0xff, 0xff},
		{0x12, 0x34, 0x56, 0x78, 0x9a, 0xbc, 0xde, 0xf0},
	} {
		withChecksum := append(append([]byte(nil), data...), Checksum(data, len(data)))
		if !ChecksumValid(withChecksum) {
			t.Errorf("数据 % x 加校验和后仍未通过验证", data)
		}
	}
}

func TestHeaderRoundTrip(t *testing.T) {
	h := Header{
		Length:          8,
		Control:         ControlSynchronize | ControlAcknowledgement,
		Sequence:        InitialSequence,
		Acknowledgement: 0,
		SessionID:       SessionControl,
	}
	encoded := EncodeHeader(h)
	if len(encoded) != HeaderBytes {
		t.Fatalf("帧头 %d 字节, 期望 %d", len(encoded), HeaderBytes)
	}
	if encoded[0] != 0xff || encoded[1] != 0x5a {
		t.Errorf("起始标记 = %02x %02x, 期望 ff 5a", encoded[0], encoded[1])
	}
	parsed, err := ParseHeader(encoded)
	if err != nil {
		t.Fatalf("ParseHeader 失败: %v", err)
	}
	if parsed.Length != 8 || parsed.Sequence != InitialSequence || parsed.SessionID != SessionControl {
		t.Errorf("解析结果不符: %+v", parsed)
	}
	if !parsed.IsSynchronize() {
		t.Error("SYN 位应被识别")
	}
	if !parsed.HasControl(ControlAcknowledgement) {
		t.Error("ACK 位应被识别")
	}
}

func TestParseHeaderRejectsCorruption(t *testing.T) {
	encoded := EncodeHeader(Header{Length: 4, SessionID: SessionControl})
	corrupted := append([]byte(nil), encoded...)
	corrupted[5] ^= 0xff // 改坏序号
	if _, err := ParseHeader(corrupted); err == nil {
		t.Error("校验和错误的帧头应被拒绝")
	}
	if _, err := ParseHeader([]byte{0x00, 0x11, 0x22}); err == nil {
		t.Error("长度不足的帧头应被拒绝")
	}
}

func TestFrameRoundTrip(t *testing.T) {
	f := &Frame{
		Header:  Header{Control: ControlAcknowledgement, Sequence: 100, SessionID: SessionEA},
		Payload: []byte{0x40, 0x40, 0x00, 0x08},
	}
	encoded, err := f.Encode()
	if err != nil {
		t.Fatalf("编码失败: %v", err)
	}
	// 头 9 + 载荷 4 + 尾部校验 1
	if len(encoded) != 14 {
		t.Fatalf("帧长 %d, 期望 14", len(encoded))
	}
	h, err := ParseHeader(encoded[:HeaderBytes])
	if err != nil {
		t.Fatalf("解析头失败: %v", err)
	}
	if h.Length != 4 {
		t.Errorf("长度字段 %d, 期望 4（长度由 Encode 按实际载荷重写）", h.Length)
	}
}

func TestFrameRejectsOversizedPayload(t *testing.T) {
	f := &Frame{Payload: make([]byte, MaxPayloadBytes+1)}
	if _, err := f.Encode(); err == nil {
		t.Error("超过上限的载荷应被拒绝")
	}
}

func TestFramerReassemblesChunkedFrame(t *testing.T) {
	frame, err := (&Frame{
		Header:  Header{Sequence: 1, SessionID: SessionControl},
		Payload: bytes.Repeat([]byte{0xab}, 40),
	}).Encode()
	if err != nil {
		t.Fatalf("编码失败: %v", err)
	}

	framer := NewFramer()
	var got []*Frame
	for i := 0; i < len(frame); i += 7 {
		end := i + 7
		if end > len(frame) {
			end = len(frame)
		}
		got = append(got, framer.Offer(frame[i:end])...)
	}
	if len(got) != 1 {
		t.Fatalf("分片重组出 %d 帧, 期望 1", len(got))
	}
	if !bytes.Equal(got[0].Payload, bytes.Repeat([]byte{0xab}, 40)) {
		t.Error("重组后的载荷与原始不符")
	}
}

func TestFramerDropsBadChecksumFrame(t *testing.T) {
	frame, err := (&Frame{
		Header:  Header{Sequence: 1, SessionID: SessionControl},
		Payload: []byte{0x01, 0x02, 0x03},
	}).Encode()
	if err != nil {
		t.Fatalf("编码失败: %v", err)
	}
	// 破坏载荷，让尾部校验和对不上。
	frame[HeaderBytes] ^= 0xff

	got := NewFramer().Offer(frame)
	if len(got) != 0 {
		t.Errorf("校验和错误的帧应被丢弃，却解析出 %d 帧", len(got))
	}
}

func TestFramerSkipsGarbage(t *testing.T) {
	frame, err := (&Frame{Header: Header{SessionID: SessionControl}}).Encode()
	if err != nil {
		t.Fatalf("编码失败: %v", err)
	}
	stream := append([]byte{0xde, 0xad, 0xbe, 0xef}, frame...)
	got := NewFramer().Offer(stream)
	if len(got) != 1 {
		t.Fatalf("解析出 %d 帧, 期望 1", len(got))
	}
}

func TestSequenceWrapAround(t *testing.T) {
	if got := NextSequence(0xff); got != 0x00 {
		t.Errorf("0xff 的下一个序号 = %#x, 期望 0x00", got)
	}
	if got := SequenceDistance(0x02, 0xff); got != 0x03 {
		t.Errorf("回绕后的序号距离 = %d, 期望 3", got)
	}
}

func TestIAP2Marker(t *testing.T) {
	// iPhone 靠这 6 个字节确认对面是 iAP2 配件，字节错一个就不会切模式。
	want := []byte{0xff, 0x55, 0x02, 0x00, 0xee, 0x10}
	if !bytes.Equal(IAP2Marker, want) {
		t.Errorf("配件标记 = % x, 期望 % x", IAP2Marker, want)
	}
}
