package csm

import (
	"bytes"
	"testing"
)

func TestU16RoundTrip(t *testing.T) {
	for _, v := range []uint16{0, 1, 0x4040, 0xffff} {
		got, err := ReadU16(U16(v), 0)
		if err != nil || got != v {
			t.Errorf("U16 往返失败: 期望 %#x, 得到 %#x (%v)", v, got, err)
		}
	}
}

func TestU32RoundTrip(t *testing.T) {
	for _, v := range []uint32{0, 1, 0xdeadbeef, 0xffffffff} {
		got, err := ReadU32(U32(v), 0)
		if err != nil || got != v {
			t.Errorf("U32 往返失败: 期望 %#x, 得到 %#x (%v)", v, got, err)
		}
	}
}

func TestStringIsNulTerminated(t *testing.T) {
	encoded := MustString("CarPlay")
	if len(encoded) != 8 || encoded[7] != 0 {
		t.Fatalf("字符串编码 = % x, 期望 8 字节且以 NUL 结尾", encoded)
	}
	got, err := ReadString(encoded)
	if err != nil || got != "CarPlay" {
		t.Errorf("ReadString = %q (%v)", got, err)
	}
}

func TestStringsRoundTrip(t *testing.T) {
	encoded, err := Strings("alpha", "beta", "")
	if err != nil {
		t.Fatalf("Strings 失败: %v", err)
	}
	got, err := ReadStrings(encoded)
	if err != nil {
		t.Fatalf("ReadStrings 失败: %v", err)
	}
	if len(got) != 3 || got[0] != "alpha" || got[1] != "beta" || got[2] != "" {
		t.Errorf("ReadStrings = %q, 期望 [alpha beta ]", got)
	}
}

func TestStringRejectsEmbeddedNul(t *testing.T) {
	if _, err := String("bad\x00string"); err == nil {
		t.Error("含 NUL 的字符串应被拒绝")
	}
}

func TestFrameEncoding(t *testing.T) {
	// 一条 StartIdentification 形态的消息：消息 ID + 一个 u16 参数。
	params := NewList(MustParameter(0x0001, U16(0x1234)))
	body, err := params.Encode()
	if err != nil {
		t.Fatalf("参数编码失败: %v", err)
	}
	frame, err := NewFrame(0x4e00, body)
	if err != nil {
		t.Fatalf("构造帧失败: %v", err)
	}

	encoded := frame.Encode()
	if encoded[0] != 0x40 || encoded[1] != 0x40 {
		t.Errorf("起始标记 = %02x %02x, 期望 40 40", encoded[0], encoded[1])
	}
	length, _ := ReadU16(encoded, 2)
	if int(length) != len(encoded) {
		t.Errorf("长度字段 %d 不等于帧实际长度 %d", length, len(encoded))
	}
	messageID, _ := ReadU16(encoded, 4)
	if messageID != 0x4e00 {
		t.Errorf("消息 ID = %#x, 期望 0x4e00", messageID)
	}
	// 头 6 字节 + 参数头 4 字节 + 载荷 2 字节
	if len(encoded) != 12 {
		t.Errorf("帧长 %d, 期望 12", len(encoded))
	}
}

func TestParseListRejectsTruncatedHeader(t *testing.T) {
	if _, err := ParseList([]byte{0x00, 0x03}); err == nil {
		t.Error("截断的参数头应报错")
	}
}

func TestParseListRejectsBadLength(t *testing.T) {
	// 声明 4 字节长度但只给了 2 字节。
	if _, err := ParseList([]byte{0x00, 0x40, 0x00, 0x01}); err == nil {
		t.Error("长度超出剩余字节数时应报错")
	}
}

func TestParseListKeepsDuplicateIdsAndOrder(t *testing.T) {
	// iAP2 允许重复参数 ID，且顺序有意义——不能退化成 map。
	body := make([]byte, 0)
	body = append(body, MustParameter(0x0002, []byte{0xaa}).Encode()...)
	body = append(body, MustParameter(0x0002, []byte{0xbb}).Encode()...)
	body = append(body, MustParameter(0x0001, []byte{0xcc}).Encode()...)

	list, err := ParseList(body)
	if err != nil {
		t.Fatalf("ParseList 失败: %v", err)
	}
	if list.Size() != 3 {
		t.Fatalf("参数个数 %d, 期望 3", list.Size())
	}
	dupes := list.Same(0x0002)
	if len(dupes) != 2 {
		t.Fatalf("重复 ID 参数个数 %d, 期望 2", len(dupes))
	}
	if dupes[0].Payload()[0] != 0xaa || dupes[1].Payload()[0] != 0xbb {
		t.Error("重复参数的顺序被改变了")
	}
	if list.At(2).ID != 0x0001 {
		t.Error("参数整体顺序错误")
	}
}

func TestFramerConcatenatedFrames(t *testing.T) {
	f1 := MustEncode(0x4e00, U16(0x0001))
	f2 := MustEncode(0x4e01)
	stream := append(append([]byte(nil), f1...), f2...)

	frames := NewFramer().Offer(stream)
	if len(frames) != 2 {
		t.Fatalf("解析出 %d 帧, 期望 2", len(frames))
	}
	if frames[0].MessageID != 0x4e00 || frames[1].MessageID != 0x4e01 {
		t.Errorf("消息 ID 顺序错误: %#x, %#x", frames[0].MessageID, frames[1].MessageID)
	}
}

func TestFramerSurvivesChunkedDelivery(t *testing.T) {
	// 模拟 TCP：一帧被拆成 3 个字节一组送达。
	frame := MustEncode(0x4e00, MustString("hello"))
	framer := NewFramer()
	var got []*Frame
	for i := 0; i < len(frame); i += 3 {
		end := i + 3
		if end > len(frame) {
			end = len(frame)
		}
		got = append(got, framer.Offer(frame[i:end])...)
	}
	if len(got) != 1 {
		t.Fatalf("分片送达后解析出 %d 帧, 期望 1", len(got))
	}
	if !bytes.Equal(got[0].Body(), MustString("hello")) {
		t.Error("重组后的消息体与原始不符")
	}
}

func TestFramerSkipsGarbageBeforeStart(t *testing.T) {
	frame := MustEncode(0x4e00)
	stream := append([]byte{0x00, 0x11, 0x22, 0xff}, frame...)

	frames := NewFramer().Offer(stream)
	if len(frames) != 1 {
		t.Fatalf("解析出 %d 帧, 期望 1（垃圾前缀应被跳过）", len(frames))
	}
	if frames[0].MessageID != 0x4e00 {
		t.Errorf("消息 ID = %#x", frames[0].MessageID)
	}
}

// MustEncode 构造一帧并编码，用于测试。
func MustEncode(messageID uint16, body ...[]byte) []byte {
	joined := make([]byte, 0)
	for _, b := range body {
		joined = append(joined, b...)
	}
	f, err := NewFrame(messageID, joined)
	if err != nil {
		panic(err)
	}
	return f.Encode()
}
