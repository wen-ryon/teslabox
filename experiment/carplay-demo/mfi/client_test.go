package mfi

import (
	"bytes"
	"testing"
	"time"

	"github.com/wen-ryon/teslabox/experiment/carplay-demo/i2c"
)

// fakeBus 模拟一颗 MFi 协处理器的寄存器行为，用于在无硬件的情况下验证协议逻辑。
// 它的行为照着实测来：选中寄存器是一次只写事务，取数据是一次纯读事务。
type fakeBus struct {
	registers map[uint8][]byte
	selected  uint8
	// wakeups 是前几次访问要假装休眠（返回 NACK）的次数。
	wakeups int
	access  int
	writes  [][]byte
}

func newFakeBus() *fakeBus {
	return &fakeBus{registers: map[uint8][]byte{
		RegDeviceVersion:     {0x05},
		RegProtocolMajor:     {0x02},
		RegError:             {0x00},
		RegAuthControlStatus: {0x00},
	}}
}

func (b *fakeBus) Transaction(addr7 uint8, write []byte, readLen int) ([]byte, error) {
	b.access++
	if b.access <= b.wakeups {
		return nil, &i2c.NackError{Addr: addr7, Byte: 0, Status: 0xef}
	}
	if len(write) > 0 {
		b.writes = append(b.writes, append([]byte(nil), write...))
		b.selected = write[0]
		if len(write) > 1 && write[0] == RegAuthControlStatus && write[1] == AuthStart {
			// 启动签名后芯片立刻算完。
			b.registers[RegAuthControlStatus] = []byte{AuthSuccess}
			b.registers[RegResponseLength] = []byte{0x00, 0x40}
			b.registers[RegResponseData] = bytes.Repeat([]byte{0xab}, 0x40)
		}
		return nil, nil
	}
	data := b.registers[b.selected]
	if readLen > len(data) {
		return nil, &i2c.ErrShortRead{Want: readLen, Got: len(data)}
	}
	return data[:readLen], nil
}

func testClient(b *fakeBus) *Client {
	c := NewClient(b, 0x11)
	c.IOTimeout = 200 * time.Millisecond
	c.RetryDelay = time.Millisecond
	c.AuthPoll = time.Millisecond
	return c
}

func TestScannerFindsChipAfterWakeup(t *testing.T) {
	bus := newFakeBus()
	bus.wakeups = 1 // 首次 NACK 只是唤醒休眠的芯片

	scanner := &Scanner{Transport: bus, Timeout: time.Second, RetryDelay: time.Millisecond}
	chip, err := scanner.Scan()
	if err != nil {
		t.Fatalf("Scan 失败: %v", err)
	}
	if chip.Addr7 != 0x11 {
		t.Errorf("地址 = 0x%02x，期望 0x11", chip.Addr7)
	}
	if chip.DeviceVersion != 0x05 {
		t.Errorf("版本 = 0x%02x，期望 0x05", chip.DeviceVersion)
	}
	if chip.ReadAddr() != 0x23 || chip.WriteAddr() != 0x22 {
		t.Errorf("8 位地址错误: 读 0x%02x 写 0x%02x", chip.ReadAddr(), chip.WriteAddr())
	}
}

func TestScannerGivesUpWhenNothingResponds(t *testing.T) {
	bus := newFakeBus()
	bus.wakeups = 1 << 20 // 永不响应

	scanner := &Scanner{Transport: bus, Timeout: 100 * time.Millisecond, RetryDelay: time.Millisecond}
	if _, err := scanner.Scan(); err == nil {
		t.Fatal("无器件时应当返回错误")
	}
}

func TestIdentityRegisters(t *testing.T) {
	bus := newFakeBus()
	c := testClient(bus)

	if v, err := c.DeviceVersion(); err != nil || v != 0x05 {
		t.Errorf("DeviceVersion = 0x%02x, %v", v, err)
	}
	if v, err := c.ProtocolMajor(); err != nil || v != 0x02 {
		t.Errorf("ProtocolMajor = 0x%02x, %v", v, err)
	}
	if v, err := c.ErrorCode(); err != nil || v != 0x00 {
		t.Errorf("ErrorCode = 0x%02x, %v", v, err)
	}
}

func TestCertificatePagedRead(t *testing.T) {
	bus := newFakeBus()
	// 构造 300 字节证书：128 + 128 + 44，验证跨窗口的寄存器递增。
	cert := make([]byte, 300)
	for i := range cert {
		cert[i] = byte(i)
	}
	bus.registers[RegCertificateLength] = []byte{0x01, 0x2c} // 300
	bus.registers[RegCertificateData] = cert[:128]
	bus.registers[RegCertificateData+1] = cert[128:256]
	bus.registers[RegCertificateData+2] = cert[256:]

	c := testClient(bus)
	got, err := c.Certificate()
	if err != nil {
		t.Fatalf("Certificate 失败: %v", err)
	}
	if !bytes.Equal(got, cert) {
		t.Errorf("读回 %d 字节，与期望的 300 字节证书不一致", len(got))
	}
}

func TestCertificateRejectsBogusLength(t *testing.T) {
	bus := newFakeBus()
	bus.registers[RegCertificateLength] = []byte{0xff, 0xff}
	c := testClient(bus)
	if _, err := c.Certificate(); err == nil {
		t.Fatal("长度 0xffff 超出上限时应报错")
	}
}

func TestSignChallenge(t *testing.T) {
	bus := newFakeBus()
	c := testClient(bus)

	challenge := bytes.Repeat([]byte{0x5a}, 20)
	signature, err := c.SignChallenge(challenge)
	if err != nil {
		t.Fatalf("SignChallenge 失败: %v", err)
	}
	if len(signature) != 0x40 {
		t.Fatalf("签名长度 %d，期望 64", len(signature))
	}

	// 写入序列必须是：挑战长度（大端）→ 挑战数据 → 启动认证。
	if len(bus.writes) < 3 {
		t.Fatalf("写入次数 %d，期望至少 3 次", len(bus.writes))
	}
	if got := bus.writes[0]; !bytes.Equal(got, []byte{RegChallengeLength, 0x00, 0x14}) {
		t.Errorf("挑战长度写入 = % x，期望 20 00 14", got)
	}
	if got := bus.writes[1]; got[0] != RegChallengeData || !bytes.Equal(got[1:], challenge) {
		t.Errorf("挑战数据写入错误: % x...", got[:4])
	}
	if got := bus.writes[2]; !bytes.Equal(got, []byte{RegAuthControlStatus, AuthStart}) {
		t.Errorf("启动认证写入 = % x，期望 10 01", got)
	}
}

func TestSignChallengeRejectsBadLength(t *testing.T) {
	c := testClient(newFakeBus())
	if _, err := c.SignChallenge(nil); err == nil {
		t.Error("空挑战应被拒绝")
	}
	if _, err := c.SignChallenge(make([]byte, 200)); err == nil {
		t.Error("超过 128 字节的挑战应被拒绝")
	}
}
