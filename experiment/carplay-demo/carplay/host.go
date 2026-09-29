// Package carplay 把传输、链路、MFi 认证与会话编排成一条完整的 CarPlay 启动链路。
//
// 有线与无线共用这一套：两者只差在最底层那条字节流怎么来
// （有线是 USBMUX 隧道，无线是蓝牙 RFCOMM 上的 TCP），
// 之上的 Identification、Authentication、CarPlay 启动完全一致。
//
//	StateLinkSync → StateIdentifying → StateAuthenticating → StateStarting → StateRunning
package carplay

import (
	"context"
	"errors"
	"fmt"
	"net"
	"sync"
	"time"

	"github.com/wen-ryon/teslabox/experiment/carplay-demo/iap2/csm"
	"github.com/wen-ryon/teslabox/experiment/carplay-demo/iap2/endpoint"
	"github.com/wen-ryon/teslabox/experiment/carplay-demo/iap2/link"
	"github.com/wen-ryon/teslabox/experiment/carplay-demo/iap2/session"
	"github.com/wen-ryon/teslabox/experiment/carplay-demo/mfi"
)

// State 是启动流程的阶段。
type State int

const (
	// StateLinkSync 正在做链路层同步。
	StateLinkSync State = iota
	// StateIdentifying 已同步，等待 iPhone 索要身份。
	StateIdentifying
	// StateAuthenticating 正在做 MFi 认证。
	StateAuthenticating
	// StateStarting 认证通过，请求启动 CarPlay 会话。
	StateStarting
	// StateRunning 会话已建立，视频流开始。
	StateRunning
	// StateStopped 会话结束或出错。
	StateStopped
)

func (s State) String() string {
	switch s {
	case StateLinkSync:
		return "链路同步"
	case StateIdentifying:
		return "身份识别"
	case StateAuthenticating:
		return "MFi 认证"
	case StateStarting:
		return "启动会话"
	case StateRunning:
		return "运行中"
	default:
		return "已停止"
	}
}

// Options 是启动参数。
type Options struct {
	// Conn 是与 iPhone 的字节流：有线来自 USBMUX，无线来自蓝牙 RFCOMM。
	Conn net.Conn
	// MFI 是认证协处理器客户端；为 nil 时认证阶段会失败。
	MFI *mfi.Client
	// Identity 是本端自报的身份。
	Identity session.Identity
	// LocalAddress 是要告诉 iPhone 的本端地址。
	// 有线模式填 NCM/隧道侧的地址，无线模式填 WiFi 侧地址。
	LocalAddress string
	// Wireless 标记这条链路是无线还是有线，影响 CarPlayStartSession 的参数。
	Wireless bool
	// Video 是视频输出通道；为 nil 时视频帧被丢弃。
	Video interface{}
	// Touch 是触摸输入源；为 nil 时不回传触摸。
	Touch TouchSource
	// ReadBuffer 是单次读取的缓冲大小。
	ReadBuffer int
	// Logf 是日志回调，可为 nil。
	Logf func(format string, args ...any)
}

// TouchSource 提供触摸事件，回传给 iPhone 的 HID 通道。
type TouchSource interface {
	// Next 阻塞直到有事件；返回 false 表示源已关闭。
	Next() (TouchPoint, bool)
}

// TouchPoint 是一次触摸。
type TouchPoint struct {
	X, Y   float32
	Action int // 0=按下 1=移动 2=抬起
}

// Host 驱动一条 CarPlay 连接。
type Host struct {
	opts    Options
	engine  *link.Engine
	csmRx   *csm.Framer
	mu      sync.Mutex
	state   State
	stopped chan struct{}
	logf    func(format string, args ...any)
}

// New 构造一个 Host。
func New(opts Options) *Host {
	if opts.ReadBuffer <= 0 {
		opts.ReadBuffer = 4096
	}
	logf := opts.Logf
	if logf == nil {
		logf = func(string, ...any) {}
	}
	return &Host{
		opts:    opts,
		engine:  link.NewEngine(link.DefaultConfig()),
		csmRx:   csm.NewFramer(),
		state:   StateLinkSync,
		stopped: make(chan struct{}),
		logf:    logf,
	}
}

// State 返回当前阶段。
func (h *Host) State() State {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.state
}

func (h *Host) setState(s State) {
	h.mu.Lock()
	h.state = s
	h.mu.Unlock()
	h.logf("carplay: 状态 → %s", s)
}

// Run 驱动整条启动链路，直到 ctx 取消或连接断开。
func (h *Host) Run(ctx context.Context) error {
	defer func() { close(h.stopped) }()

	// 1) 发起链路同步。
	if err := h.writeWire(h.engine.Start(time.Now())); err != nil {
		return err
	}

	// 2) 读循环：喂给链路引擎，再把解出的 CSM 交给状态机。
	buf := make([]byte, h.opts.ReadBuffer)
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}
		n, err := h.opts.Conn.Read(buf)
		if n > 0 {
			for _, frame := range h.engine.Offer(buf[:n]) {
				if err := h.onLinkFrame(frame); err != nil {
					return err
				}
			}
		}
		if err != nil {
			h.setState(StateStopped)
			return err
		}
		// 每轮读之后把待发数据推出去。
		if err := h.flush(time.Now()); err != nil {
			return err
		}
	}
}

// onLinkFrame 处理一条链路帧：把载荷喂给 CSM 重组器。
func (h *Host) onLinkFrame(frame *link.Frame) error {
	if len(frame.Payload) == 0 {
		return nil
	}
	if frame.Header.SessionID != link.SessionControl {
		// 非控制会话的数据（外部配件协议、文件传输）暂不处理。
		return nil
	}
	for _, message := range h.csmRx.Offer(frame.Payload) {
		if err := h.onMessage(message); err != nil {
			return err
		}
	}
	return nil
}

// onMessage 按消息 ID 推进状态机。
func (h *Host) onMessage(message *csm.Frame) error {
	switch message.MessageID {
	case endpoint.StartIdentification:
		h.setState(StateIdentifying)
		return h.sendIdentification()

	case endpoint.IdentificationAccepted:
		h.setState(StateAuthenticating)
		return nil

	case endpoint.IdentificationRejected:
		h.setState(StateStopped)
		return session.ErrIdentificationRejected

	case endpoint.RequestAuthenticationCertificate:
		return h.sendCertificate()

	case endpoint.RequestAuthenticationChallengeResponse:
		return h.respondToChallenge(message)

	case endpoint.AuthenticationFailed:
		h.setState(StateStopped)
		return session.ErrAuthenticationFailed

	case endpoint.AuthenticationSucceeded:
		return h.startCarPlay()

	case endpoint.CarPlayAvailability:
		availability, err := session.ParseCarPlayAvailability(message)
		if err == nil {
			h.logf("carplay: 可用性 有线=%v 无线=%v",
				availability.WiredAvailable, availability.WirelessAvailable)
		}
		return nil

	case endpoint.CarPlaySessionStopped:
		h.setState(StateStopped)
		return nil

	default:
		h.logf("carplay: 未处理的消息 0x%04x", message.MessageID)
		return nil
	}
}

// sendIdentification 回复配件身份。
func (h *Host) sendIdentification() error {
	frame, err := session.BuildIdentificationInformation(h.opts.Identity)
	if err != nil {
		return err
	}
	return h.sendControl(frame)
}

// sendCertificate 从 MFi 芯片读证书并回给 iPhone。
func (h *Host) sendCertificate() error {
	if h.opts.MFI == nil {
		return errors.New("carplay: 未配置 MFi 芯片，无法提供证书")
	}
	certificate, err := h.opts.MFI.Certificate()
	if err != nil {
		return fmt.Errorf("carplay: 读取证书失败: %w", err)
	}
	frame, err := session.BuildAuthenticationCertificate(certificate)
	if err != nil {
		return err
	}
	h.logf("carplay: 已送出 %d 字节证书", len(certificate))
	return h.sendControl(frame)
}

// respondToChallenge 用 MFi 芯片签名挑战并回传。
func (h *Host) respondToChallenge(message *csm.Frame) error {
	if h.opts.MFI == nil {
		return errors.New("carplay: 未配置 MFi 芯片，无法签名")
	}
	challenge, err := session.ParseChallenge(message)
	if err != nil {
		return err
	}
	signature, err := h.opts.MFI.SignChallenge(challenge)
	if err != nil {
		return fmt.Errorf("carplay: 签名失败: %w", err)
	}
	frame, err := session.BuildAuthenticationResponse(signature)
	if err != nil {
		return err
	}
	h.logf("carplay: 已回传 %d 字节签名", len(signature))
	return h.sendControl(frame)
}

// startCarPlay 请求启动会话并开启 HID 通道。
func (h *Host) startCarPlay() error {
	h.setState(StateStarting)
	frame, err := session.BuildCarPlayStartSession(h.opts.LocalAddress, h.opts.Wireless)
	if err != nil {
		return err
	}
	if err := h.sendControl(frame); err != nil {
		return err
	}
	// 触摸屏回传依赖 HID 通道，必须紧接着开。
	hid, err := session.BuildStartHID(1)
	if err != nil {
		return err
	}
	if err := h.sendControl(hid); err != nil {
		return err
	}
	h.setState(StateRunning)
	return nil
}

// sendControl 把一条 CSM 消息切片后交给链路层。
func (h *Host) sendControl(frame *csm.Frame) error {
	chunks, err := session.SplitForLink(frame, 0)
	if err != nil {
		return err
	}
	for _, chunk := range chunks {
		if err := h.engine.Send(link.SessionControl, chunk); err != nil {
			return err
		}
	}
	return h.flush(time.Now())
}

// flush 把链路层的待发字节写到连接上。
func (h *Host) flush(now time.Time) error {
	out := h.engine.Poll(now)
	if len(out) == 0 {
		return nil
	}
	_, err := h.opts.Conn.Write(out)
	return err
}

func (h *Host) writeWire(data []byte) error {
	if len(data) == 0 {
		return nil
	}
	_, err := h.opts.Conn.Write(data)
	return err
}

// TouchLoop 把触摸事件持续回传给 iPhone。
//
// 视频由网页端消费，音频走车机蓝牙，所以这里只处理输入方向。
func (h *Host) TouchLoop(ctx context.Context) {
	if h.opts.Touch == nil {
		return
	}
	for {
		select {
		case <-ctx.Done():
			return
		case <-h.stopped:
			return
		default:
		}
		point, ok := h.opts.Touch.Next()
		if !ok {
			return
		}
		if h.State() != StateRunning {
			continue
		}
		if err := h.sendTouch(point); err != nil {
			h.logf("carplay: 触摸回传失败: %v", err)
		}
	}
}

// sendTouch 发送一条 HID 触摸报告。
//
// 报告格式取决于 iPhone 下发的 HID 描述符，这里按最常见的
// 「报告 ID + 动作 + X + Y」布局填充，接真实设备时需要按描述符校正。
func (h *Host) sendTouch(point TouchPoint) error {
	// HID 报告里的坐标是整数像素，先把归一化坐标换算回整数。
	x, y := uint16(point.X), uint16(point.Y)
	report := []byte{
		0x01,               // 报告 ID
		byte(point.Action), // 动作
		byte(x), byte(x >> 8),
		byte(y), byte(y >> 8),
	}
	frame, err := csm.NewFrameOf(endpoint.HIDAccessoryReport, csm.NewList(
		csm.MustParameter(endpoint.ComponentID, csm.U16(1)),
		csm.MustParameter(1, report),
	))
	if err != nil {
		return err
	}
	return h.sendControl(frame)
}
