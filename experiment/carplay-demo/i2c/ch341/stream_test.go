package ch341

import (
	"bytes"
	"testing"
)

// 本文件中的期望值全部来自真实硬件实测：
// 沁恒 CH341 转接板 + MFi 认证协处理器（地址 0x11，device version 0x05，
// 证书 908 字节）。改动编解码逻辑前请确认这些向量是否仍然成立。

func TestConfiguration(t *testing.T) {
	want := []byte{0xaa, 0x61, 0x00}
	if got := Configuration(Speed100kHz); !bytes.Equal(got, want) {
		t.Errorf("Configuration(100kHz) = % x, 期望 % x", got, want)
	}
}

func TestTransaction(t *testing.T) {
	cases := []struct {
		name    string
		addr7   uint8
		write   []byte
		readLen int
		want    []byte
	}{
		{
			name:  "选择寄存器 0x00（写地址+寄存器号，带 STOP）",
			addr7: 0x11, write: []byte{0x00},
			want: []byte{0xaa, 0x74, 0x80, 0x22, 0x80, 0x00, 0x75, 0x00},
		},
		{
			name:  "纯读 1 字节（设备版本）",
			addr7: 0x11, readLen: 1,
			want: []byte{0xaa, 0x74, 0x80, 0x23, 0xc0, 0x75, 0x00},
		},
		{
			name:  "纯读 2 字节（证书长度，大端）",
			addr7: 0x11, readLen: 2,
			want: []byte{0xaa, 0x74, 0x80, 0x23, 0xc1, 0xc0, 0x75, 0x00},
		},
		{
			name:  "写单字节到控制寄存器（启动认证）",
			addr7: 0x11, write: []byte{0x10, 0x01},
			want: []byte{0xaa, 0x74, 0x80, 0x22, 0x80, 0x10, 0x80, 0x01, 0x75, 0x00},
		},
		{
			name:  "地址 0x10 的寄存器选择",
			addr7: 0x10, write: []byte{0x00},
			want: []byte{0xaa, 0x74, 0x80, 0x20, 0x80, 0x00, 0x75, 0x00},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := Transaction(tc.addr7, tc.write, tc.readLen)
			if !bytes.Equal(got, tc.want) {
				t.Errorf("得到 % x\n期望 % x", got, tc.want)
			}
			if len(got) > MaxPacket {
				t.Errorf("流包长度 %d 超过 %d", len(got), MaxPacket)
			}
		})
	}
}

func TestSegmentedRead(t *testing.T) {
	cases := []struct {
		length    int
		wantSegs  int
		wantLast  []byte
		wantLastD int
	}{
		{length: 128, wantSegs: 4, wantLast: []byte{0xaa, 0xdf, 0xc0, 0x75, 0x00}, wantLastD: 32},
		{length: 908, wantSegs: 29, wantLast: []byte{0xaa, 0xcb, 0xc0, 0x75, 0x00}, wantLastD: 12},
		// 正好 32 字节不需要分段，首段自带 START 和地址。
		{length: 32, wantSegs: 1, wantLast: []byte{0xaa, 0x74, 0x81, 0x23, 0xdf, 0xc0, 0x75, 0x00}, wantLastD: 32},
	}
	for _, tc := range cases {
		segs := SegmentedRead(0x11, tc.length)
		if len(segs) != tc.wantSegs {
			t.Fatalf("长度 %d: 得到 %d 段，期望 %d", tc.length, len(segs), tc.wantSegs)
		}
		total := 0
		for i, s := range segs {
			total += s.DataBytes
			if len(s.Payload) > MaxPacket {
				t.Errorf("长度 %d 第 %d 段: payload %d 字节超过 %d",
					tc.length, i, len(s.Payload), MaxPacket)
			}
		}
		if total != tc.length {
			t.Errorf("长度 %d: 各段数据字节合计 %d", tc.length, total)
		}
		last := segs[len(segs)-1]
		if !bytes.Equal(last.Payload, tc.wantLast) {
			t.Errorf("长度 %d 末段 = % x，期望 % x", tc.length, last.Payload, tc.wantLast)
		}
		if last.DataBytes != tc.wantLastD {
			t.Errorf("长度 %d 末段数据字节 = %d，期望 %d", tc.length, last.DataBytes, tc.wantLastD)
		}
	}
}

func TestSegmentedReadIntermediateHasNoStop(t *testing.T) {
	// 中间段必须省略 STOP，否则 I2C 事务会被切断，后续段读到的是垃圾。
	segs := SegmentedRead(0x11, 128)
	for i, s := range segs[:len(segs)-1] {
		if bytes.Contains(s.Payload, []byte{Stop}) {
			t.Errorf("第 %d 段含 STOP，I2C 事务会被提前结束", i)
		}
		if len(s.Payload) != MaxPacket {
			t.Errorf("第 %d 段长度 %d，中间段应补齐到 %d", i, len(s.Payload), MaxPacket)
		}
	}
}

func TestStatusBytes(t *testing.T) {
	cases := []struct{ writeLen, readLen, want int }{
		{0, 0, 0},
		{1, 0, 2}, // 地址 + 1 数据字节
		{2, 0, 3},
		{0, 1, 1}, // 只有读地址字节
		{0, 2, 1},
		{1, 1, 3},
	}
	for _, tc := range cases {
		if got := StatusBytes(tc.writeLen, tc.readLen); got != tc.want {
			t.Errorf("StatusBytes(%d,%d) = %d, 期望 %d", tc.writeLen, tc.readLen, got, tc.want)
		}
	}
}

func TestSplitReplyAlignsFromTail(t *testing.T) {
	// 实测：读设备版本时控制器回传 2 个状态字节 + 1 个数据字节 0x05。
	status, data, err := SplitReply([]byte{0x00, 0x00, 0x05}, 1)
	if err != nil {
		t.Fatalf("SplitReply 出错: %v", err)
	}
	if !bytes.Equal(data, []byte{0x05}) {
		t.Errorf("数据 = % x，期望 05", data)
	}
	if len(status) != 2 {
		t.Errorf("状态字节数 = %d，期望 2", len(status))
	}
	if n := FirstNack(status); n != -1 {
		t.Errorf("全部 ACK 时 FirstNack = %d，期望 -1", n)
	}
}

func TestFirstNackDetectsMissingDevice(t *testing.T) {
	// 实测：地址上无器件时状态字节为 0xef（bit7 置位）。
	if n := FirstNack([]byte{0x00, 0xef}); n != 1 {
		t.Errorf("FirstNack = %d，期望 1", n)
	}
	if n := FirstNack([]byte{0xef}); n != 0 {
		t.Errorf("FirstNack = %d，期望 0", n)
	}
}

func TestSplitReplyShort(t *testing.T) {
	if _, _, err := SplitReply([]byte{0x00}, 2); err == nil {
		t.Error("应答短于数据长度时应报错")
	}
}
