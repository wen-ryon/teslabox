package carplay

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"net"
	"sync"
	"testing"
	"time"

	"github.com/wen-ryon/teslabox/experiment/carplay-demo/i2c"
	"github.com/wen-ryon/teslabox/experiment/carplay-demo/iap2/csm"
	"github.com/wen-ryon/teslabox/experiment/carplay-demo/iap2/endpoint"
	"github.com/wen-ryon/teslabox/experiment/carplay-demo/iap2/link"
	"github.com/wen-ryon/teslabox/experiment/carplay-demo/iap2/session"
	"github.com/wen-ryon/teslabox/experiment/carplay-demo/mfi"
)

// fakeI2C 模拟一颗 MFi 协处理器，让编排器在没有硬件时也能跑完整流程。
type fakeI2C struct {
	certificate []byte
	register    uint8
	signed      []byte
}

func (f *fakeI2C) Transaction(addr7 uint8, write []byte, readLen int) ([]byte, error) {
	if len(write) > 0 {
		f.register = write[0]
		if f.register == mfi.RegAuthControlStatus && len(write) > 1 && write[1] == mfi.AuthStart {
			f.signed = bytes.Repeat([]byte{0x7c}, 64)
		}
		return nil, nil
	}
	switch f.register {
	case mfi.RegDeviceVersion:
		return []byte{0x05}, nil
	case mfi.RegCertificateLength:
		binary.BigEndian.PutUint16(make([]byte, 2), uint16(len(f.certificate)))
		out := make([]byte, 2)
		binary.BigEndian.PutUint16(out, uint16(len(f.certificate)))
		return out, nil
	case mfi.RegCertificateData:
		out := make([]byte, readLen)
		for i := range out {
			out[i] = f.certificate[i%len(f.certificate)]
		}
		return out, nil
	case mfi.RegAuthControlStatus:
		return []byte{mfi.AuthSuccess}, nil
	case mfi.RegResponseLength:
		out := make([]byte, 2)
		binary.BigEndian.PutUint16(out, uint16(len(f.signed)))
		return out, nil
	case mfi.RegResponseData:
		out := make([]byte, readLen)
		copy(out, f.signed)
		return out, nil
	default:
		return nil, i2c.ErrNoDevice
	}
}

// pipeConn 是一条内存里的双向字节流，模拟 USBMUX 或蓝牙 RFCOMM 通道。
//
// in 是「我要读的」，等于对端的 out；out 是「我写出去的」。
type pipeConn struct {
	mu     sync.Mutex
	in     *bytes.Buffer
	out    *bytes.Buffer
	closed bool
}

func newPipe() (*pipeConn, *pipeConn) {
	toPhone := &bytes.Buffer{}
	toHost := &bytes.Buffer{}
	a := &pipeConn{in: toHost, out: toPhone}
	b := &pipeConn{in: toPhone, out: toHost}
	return a, b
}

func (p *pipeConn) Read(b []byte) (int, error) {
	p.mu.Lock()
	if p.in.Len() == 0 {
		p.mu.Unlock()
		// 没有数据时返回 0 字节且不带错误：真实的 net.Conn 会阻塞等待，
		// 直接返回 error 会让调用方以为连接断了。
		time.Sleep(2 * time.Millisecond)
		return 0, nil
	}
	n, _ := p.in.Read(b)
	p.mu.Unlock()
	return n, nil
}

func (p *pipeConn) Write(b []byte) (int, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed {
		return 0, errors.New("已关闭")
	}
	return p.out.Write(b)
}

func (p *pipeConn) Close() error {
	p.mu.Lock()
	p.closed = true
	p.mu.Unlock()
	return nil
}

func (p *pipeConn) LocalAddr() net.Addr              { return nil }
func (p *pipeConn) RemoteAddr() net.Addr             { return nil }
func (p *pipeConn) SetDeadline(time.Time) error      { return nil }
func (p *pipeConn) SetReadDeadline(time.Time) error  { return nil }
func (p *pipeConn) SetWriteDeadline(time.Time) error { return nil }

// phoneSession 站在 iPhone 一侧驱动链路层。
type phoneSession struct {
	conn   *pipeConn
	engine *link.Engine
	csm    *csm.Framer
}

func newPhone(conn *pipeConn) *phoneSession {
	return &phoneSession{
		conn:   conn,
		engine: link.NewEngine(link.DefaultConfig()),
		csm:    csm.NewFramer(),
	}
}

// Send 发一条 CSM 消息给配件。
func (p *phoneSession) Send(frame *csm.Frame) error {
	chunks, err := session.SplitForLink(frame, 0)
	if err != nil {
		return err
	}
	for _, chunk := range chunks {
		if err := p.engine.Send(link.SessionControl, chunk); err != nil {
			return err
		}
	}
	out := p.engine.Poll(time.Now())
	_, err = p.conn.Write(out)
	return err
}

// Receive 读取配件发来的 CSM 消息。
func (p *phoneSession) Receive(timeout time.Duration) []*csm.Frame {
	deadline := time.Now().Add(timeout)
	var collected []*csm.Frame
	buf := make([]byte, 4096)
	for time.Now().Before(deadline) {
		n, _ := p.conn.Read(buf)
		if n > 0 {
			for _, f := range p.engine.Offer(buf[:n]) {
				collected = append(collected, p.csm.Offer(f.Payload)...)
			}
		}
		if len(collected) > 0 {
			return collected
		}
		time.Sleep(2 * time.Millisecond)
	}
	return collected
}

// TestHostCompletesHandshake 走完整的启动链路：
// 身份 → 证书 → 挑战签名 → 认证通过 → 启动 CarPlay。
func TestHostCompletesHandshake(t *testing.T) {
	hostConn, phoneConn := newPipe()
	certificate := bytes.Repeat([]byte{0x30, 0x82}, 64)

	mfiClient := mfi.NewClient(&fakeI2C{certificate: certificate}, 0x11)
	mfiClient.IOTimeout = 200 * time.Millisecond
	mfiClient.RetryDelay = time.Millisecond

	host := New(Options{
		Conn:         hostConn,
		MFI:          mfiClient,
		Identity:     session.DefaultIdentity(),
		LocalAddress: "fe80::1",
	})

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	runErr := make(chan error, 1)
	go func() { runErr <- host.Run(ctx) }()

	phone := newPhone(phoneConn)

	// iPhone 侧先收 SYN 并回 SYN|ACK，完成链路同步。
	handshakeDone := make(chan error, 1)
	// 单独用一个 framer 识别 SYN：Engine.Offer 会把 SYN 消费掉，
	// 不会作为结果返回给调用方。
	detect := link.NewFramer()
	go func() {
		buf := make([]byte, 4096)
		for {
			n, _ := phoneConn.Read(buf)
			if n > 0 {
				// 同时喂给 phone 的链路引擎，让它推进到可发送状态。
				phone.engine.Offer(buf[:n])
				for _, f := range detect.Offer(buf[:n]) {
					if f.Header.IsSynchronize() {
						reply, err := (&link.Frame{Header: link.Header{
							Control:   link.ControlSynchronize | link.ControlAcknowledgement,
							Sequence:  f.Header.Sequence,
							SessionID: link.SessionControl,
						}}).Encode()
						if err != nil {
							handshakeDone <- err
							return
						}
						_, _ = phoneConn.Write(reply)
						handshakeDone <- nil
						return
					}
				}
			}
			time.Sleep(time.Millisecond)
		}
	}()
	select {
	case err := <-handshakeDone:
		if err != nil {
			t.Fatalf("回 SYN|ACK 失败: %v", err)
		}
	case err := <-runErr:
		t.Fatalf("配件主循环提前退出: %v", err)
	case <-time.After(3 * time.Second):
		t.Fatal("链路同步未发生：配件没有发出 SYN")
	}
	// 等配件处理完 SYN|ACK。
	time.Sleep(50 * time.Millisecond)

	// 1) iPhone 索要身份。
	startIdent, err := csm.NewFrame(endpoint.StartIdentification, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := phone.Send(startIdent); err != nil {
		t.Fatalf("发送 StartIdentification 失败: %v", err)
	}
	replies := phone.Receive(2 * time.Second)
	if len(replies) == 0 {
		t.Fatal("配件没有回复身份信息")
	}
	if replies[0].MessageID != endpoint.IdentificationInformation {
		t.Fatalf("期望 0x%04x，收到 0x%04x",
			endpoint.IdentificationInformation, replies[0].MessageID)
	}
	if host.State() != StateIdentifying {
		t.Errorf("状态 = %v，期望 身份识别", host.State())
	}

	// 2) iPhone 接受身份并索要证书。
	accepted, _ := csm.NewFrame(endpoint.IdentificationAccepted, nil)
	if err := phone.Send(accepted); err != nil {
		t.Fatal(err)
	}
	reqCert, _ := csm.NewFrame(endpoint.RequestAuthenticationCertificate, nil)
	if err := phone.Send(reqCert); err != nil {
		t.Fatal(err)
	}
	replies = phone.Receive(2 * time.Second)
	found := false
	for _, r := range replies {
		if r.MessageID == endpoint.AuthenticationCertificate {
			found = true
			params, err := r.Params()
			if err != nil {
				t.Fatal(err)
			}
			p, err := params.Get(endpoint.AuthCertificateSerial)
			if err != nil {
				t.Fatalf("证书消息缺少证书参数: %v", err)
			}
			if len(p.Payload()) == 0 {
				t.Error("证书为空")
			}
		}
	}
	if !found {
		t.Fatal("配件没有回传证书")
	}

	// 3) iPhone 下发挑战，配件应回签名。
	challenge := bytes.Repeat([]byte{0x5a}, 20)
	reqChallenge, err := csm.NewFrameOf(endpoint.RequestAuthenticationChallengeResponse,
		csm.NewList(csm.MustParameter(endpoint.AuthChallengeData, challenge)))
	if err != nil {
		t.Fatal(err)
	}
	if err := phone.Send(reqChallenge); err != nil {
		t.Fatal(err)
	}
	replies = phone.Receive(2 * time.Second)
	found = false
	for _, r := range replies {
		if r.MessageID == endpoint.AuthenticationResponse {
			found = true
		}
	}
	if !found {
		t.Fatal("配件没有回传签名")
	}

	// 4) 认证通过，配件应启动 CarPlay 并开 HID。
	succeeded, _ := csm.NewFrame(endpoint.AuthenticationSucceeded, nil)
	if err := phone.Send(succeeded); err != nil {
		t.Fatal(err)
	}
	replies = phone.Receive(2 * time.Second)
	var gotSession, gotHID bool
	for _, r := range replies {
		switch r.MessageID {
		case endpoint.CarPlayStartSession:
			gotSession = true
		case endpoint.HIDStartHandler:
			gotHID = true
		}
	}
	if !gotSession {
		t.Error("认证通过后没有发出 CarPlayStartSession")
	}
	if !gotHID {
		t.Error("认证通过后没有开启 HID 通道")
	}
	if host.State() != StateRunning {
		t.Errorf("最终状态 = %v，期望 运行中", host.State())
	}
}

func TestHostRejectsMissingIdentity(t *testing.T) {
	hostConn, _ := newPipe()
	host := New(Options{
		Conn:     hostConn,
		Identity: session.Identity{}, // 缺必填字段
	})
	frame, err := session.BuildIdentificationInformation(host.opts.Identity)
	if err == nil && frame != nil {
		t.Error("身份信息缺必填字段时应当构造失败")
	}
}

func TestHostRequiresMFIForCertificate(t *testing.T) {
	hostConn, _ := newPipe()
	host := New(Options{
		Conn:     hostConn,
		Identity: session.DefaultIdentity(),
	})
	if err := host.sendCertificate(); err == nil {
		t.Error("未配置 MFi 芯片时请求证书应报错")
	}
}
