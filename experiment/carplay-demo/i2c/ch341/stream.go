// Package ch341 实现沁恒 CH341（USB 转 I2C 桥接）的流协议编解码。
//
// CH341 不走标准的 USB 控制传输，而是把一整套 I2C 时序打包成「流」，
// 通过 bulk 端点收发。每个流包最长 32 字节：
//
//	AA <命令序列> 00
//
// 命令字节含义：
//
//	74          START
//	75          STOP
//	80|n        向总线写 n+1 个字节（其后紧跟 n+1 个数据字节）
//	C0|n        从总线读 n 个字节（n 为 0 时读 1 个字节）
//	60..63      设置 I2C 速率（20k/100k/400k/750k）
//	00          流结束
//
// 写命令有两种形态，区别在于控制器是否回传 ACK/NACK 状态字节：
// 裸 80（写 1 字节）会为每个字节回传一个状态字节，状态字节 bit7 置位即 NACK；
// 80|n（n>0，批量写）不回传状态字节。因此地址+数据的写要用裸 80 逐字节发，
// 才能在寄存器寻址阶段检测出「这个地址上没有器件」。
package ch341

import "fmt"

// 流命令字节。
const (
	StreamStart = 0xaa
	Start       = 0x74
	Stop        = 0x75
	Write       = 0x80
	Read        = 0xc0
	StreamEnd   = 0x00
)

// I2C 速率命令。
const (
	Speed20kHz  = 0x60
	Speed100kHz = 0x61
	Speed400kHz = 0x62
	Speed750kHz = 0x63
)

const (
	// MaxPacket 是 CH341 一个流包的最大长度，也是 bulk 端点的 wMaxPacketSize。
	// 控制器按整包应答，所以读取长度必须停在包边界上。
	MaxPacket = 32

	// AckBit 是状态字节中表示 NACK 的位。
	AckBit = 0x80

	// LengthMask 是写/读命令中编码长度的位掩码，实际长度 = (n & LengthMask) + 1。
	LengthMask = 0x3f
)

// Configuration 生成设置 I2C 速率的流包。
func Configuration(speed byte) []byte {
	return []byte{StreamStart, speed, StreamEnd}
}

// Transaction 编码一次完整的 I2C 事务（单段，结果不超过 MaxPacket 字节）。
//
// addr7 是 7 位从机地址。writeData 是要写的数据（可为 nil），
// readLen 是要读的字节数（可为 0）。
func Transaction(addr7 uint8, writeData []byte, readLen int) []byte {
	out := make([]byte, 0, MaxPacket)
	out = append(out, StreamStart)
	if len(writeData) > 0 {
		out = append(out, Start)
		out = append(out, Write, addr7<<1)
		for _, v := range writeData {
			out = append(out, Write, v)
		}
	}
	if readLen > 0 {
		out = append(out, Start)
		out = append(out, Write, addr7<<1|1)
		if readLen > 1 {
			out = append(out, Read+byte(readLen-1))
		}
		out = append(out, Read)
	}
	out = append(out, Stop, StreamEnd)
	return out
}

// SegmentedRead 编码一次超过单包容量的纯读事务。
//
// 中间段以 00 结束并补齐到 MaxPacket，但**故意不发 STOP**，
// 这样 I2C 事务得以延续到下一段；只有最后一段才带 STOP。
// 返回值是若干段，每段需依次写出，并按 DataBytes 读回相应长度。
func SegmentedRead(addr7 uint8, length int) []Segment {
	var segments []Segment

	// 批量写形态 80|1 只发地址，不产生状态字节，
	// 这样读回的数据前面不会多出一个状态字节。
	buf := make([]byte, 0, MaxPacket)
	buf = append(buf, StreamStart, Start, Write+1, addr7<<1|1)

	remaining := length
	for remaining > MaxPacket {
		buf = append(buf, Read+byte(MaxPacket))
		segments = append(segments, finishIntermediate(buf, MaxPacket))
		buf = buf[:0]
		buf = append(buf, StreamStart)
		remaining -= MaxPacket
	}
	if remaining > 1 {
		buf = append(buf, Read+byte(remaining-1))
	}
	buf = append(buf, Read, Stop, StreamEnd)
	segments = append(segments, Segment{Payload: buf, DataBytes: remaining})
	return segments
}

// Segment 是分段读中的一段：写出 Payload，读回 DataBytes 字节。
type Segment struct {
	Payload   []byte
	DataBytes int
}

func finishIntermediate(buf []byte, dataBytes int) Segment {
	buf = append(buf, StreamEnd)
	for len(buf) < MaxPacket {
		buf = append(buf, 0)
	}
	// 必须拷贝：调用方会用 buf[:0] 复用同一块底层数组继续拼下一段，
	// 直接交出 buf 会让已经生成的段被后续 append 悄悄改写。
	out := make([]byte, len(buf))
	copy(out, buf)
	return Segment{Payload: out, DataBytes: dataBytes}
}

// StatusBytes 返回一个流包预期回传的 ACK/NACK 状态字节数。
//
// 它由编码参数推算，而不是扫描包体：包体里的数据字节完全可能正好是 0x80，
// 扫描会把数据误判成写命令。写阶段每写一个字节（地址字节也算）回传一个
// 状态字节；读阶段的地址字节同样用裸 80 发出，也回传一个。
func StatusBytes(writeLen, readLen int) int {
	n := 0
	if writeLen > 0 {
		n += 1 + writeLen // 地址字节 + 数据
	}
	if readLen > 0 {
		n++ // 读地址字节
	}
	return n
}

// SplitReply 把控制器对一个流包的应答拆成状态字节和实际数据。
//
// 状态字节的数量是固件细节：裸 80 每写一个字节回传一个状态字节，
// 但多字节写可能只回传一个甚至不回传。因此这里从**尾部**对齐数据，
// 无论状态字节实际有几个，取出的 dataBytes 个数据字节都是正确的。
func SplitReply(reply []byte, dataBytes int) (status []byte, data []byte, err error) {
	if len(reply) < dataBytes {
		return nil, nil, &ErrShortReply{Want: dataBytes, Got: len(reply)}
	}
	split := len(reply) - dataBytes
	return reply[:split], reply[split:], nil
}

// FirstNack 返回状态字节中第一个 NACK 的位置；全部 ACK 时返回 -1。
func FirstNack(status []byte) int {
	for i, b := range status {
		if b&AckBit != 0 {
			return i
		}
	}
	return -1
}

// ErrShortReply 表示控制器应答的长度短于期望的数据长度。
type ErrShortReply struct {
	Want int
	Got  int
}

func (e *ErrShortReply) Error() string {
	return fmt.Sprintf("ch341: 应答 %d 字节，少于期望的 %d 个数据字节", e.Got, e.Want)
}
