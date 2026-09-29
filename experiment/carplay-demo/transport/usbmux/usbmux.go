// Package usbmux 实现 USBMUX v2 协议，用于在 iPhone 的 USB bulk 管道上开出 TCP 流。
//
// 有线 CarPlay 并不是直接走 USB 网卡：iPhone 插上后先以 bulk 端点暴露一个
// USBMUX 通道，配件在这个通道上完成版本握手，然后才能「拨号」到 iPhone 上的
// 具体端口（例如 lockdown 的 62078），拿到一条字节流，iAP2 就跑在这条流上。
//
// 帧头 16 字节（全部小端）：
//
//	u32 protocol  0=版本, 2=setup, 6=TCP
//	u32 length    含头的总长度
//	u32 word8     版本帧里是版本号；iPhone 的回复不带 0xFEEDFACE
//	u16 sequence / u16 ack
//
// 本包的字段布局与流程对照 xcertplay 的 Iap2UsbMuxHost。
package usbmux

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"sync"
	"time"
)

// 协议号与固定值。
const (
	ProtocolVersion = 0
	ProtocolSetup   = 2
	ProtocolTCP     = 6

	// USBMuxVersion 是目前使用的协议版本。
	USBMuxVersion = 2
	// SetupValue 是 setup 帧的唯一载荷字节。
	SetupValue = 0x07

	MuxHeaderBytes      = 16
	VersionMessageBytes = 20
	TCPHeaderBytes      = 20
	TCPWindow           = 512
	MaxFrameBytes       = 65536

	// LockdownPort 是 iPhone 上 lockdown 服务的端口。
	LockdownPort = 62078
	// FirstSourcePort 是本端分配源端口的起点。
	FirstSourcePort = 1
)

// USBMUX 隧道内的 TCP 标志位。
const (
	TCPFin = 0x01
	TCPSyn = 0x02
	TCPRst = 0x04
	TCPAck = 0x10
)

// 默认超时。
const (
	HandshakeTimeout = 60 * time.Second
	ConnectTimeout   = 5 * time.Second
	WriteTimeout     = 2 * time.Second
)

// ErrProtocol 表示 USBMUX 层的协议错误。
var ErrProtocol = errors.New("usbmux: 协议错误")

// ErrClosed 表示通道已关闭。
var ErrClosed = errors.New("usbmux: 通道已关闭")

func protocolf(format string, args ...any) error {
	return fmt.Errorf("%w: %s", ErrProtocol, fmt.Sprintf(format, args...))
}

// Frame 是一条 USBMUX 帧。
type Frame struct {
	Protocol uint32
	Length   uint32
	Word8    uint32
	Sequence uint32
	Payload  []byte
}

// Encode 编码一条帧。Length 由载荷长度自动计算。
func (f *Frame) Encode() []byte {
	f.Length = uint32(MuxHeaderBytes + len(f.Payload))
	out := make([]byte, f.Length)
	binary.LittleEndian.PutUint32(out[0:], f.Protocol)
	binary.LittleEndian.PutUint32(out[4:], f.Length)
	binary.LittleEndian.PutUint32(out[8:], f.Word8)
	binary.LittleEndian.PutUint16(out[12:], uint16(f.Sequence))
	binary.LittleEndian.PutUint16(out[14:], uint16(f.Sequence>>16))
	copy(out[MuxHeaderBytes:], f.Payload)
	return out
}

// ParseFrame 解析一条完整帧。
//
// 只信任 length 字段：iPhone 的回复并不在第四个字段里带 0xFEEDFACE，
// 按 magic 校验会误判。（这一点是 xcertplay 注释里特别点名的。）
func ParseFrame(b []byte) (*Frame, error) {
	if len(b) < MuxHeaderBytes {
		return nil, protocolf("帧长 %d 小于头部 %d", len(b), MuxHeaderBytes)
	}
	length := binary.LittleEndian.Uint32(b[4:])
	if length < MuxHeaderBytes || length > MaxFrameBytes {
		return nil, protocolf("帧长度字段 %d 非法", length)
	}
	if uint32(len(b)) < length {
		return nil, protocolf("数据 %d 字节，帧声明 %d 字节", len(b), length)
	}
	return &Frame{
		Protocol: binary.LittleEndian.Uint32(b[0:]),
		Length:   length,
		Word8:    binary.LittleEndian.Uint32(b[8:]),
		Sequence: uint32(binary.LittleEndian.Uint16(b[12:])),
		Payload:  append([]byte(nil), b[MuxHeaderBytes:length]...),
	}, nil
}

// VersionMessage 构造版本握手帧（20 字节）。
func VersionMessage() []byte {
	out := make([]byte, VersionMessageBytes)
	binary.LittleEndian.PutUint32(out[0:], ProtocolVersion)
	binary.LittleEndian.PutUint32(out[4:], VersionMessageBytes)
	binary.LittleEndian.PutUint32(out[8:], USBMuxVersion)
	return out
}

// Pipe 是 USBMUX 之下的字节管道——在真实环境里就是 iPhone 的 bulk 端点。
type Pipe interface {
	Write(data []byte) error
	Read() ([]byte, error)
	Close() error
}

// Client 是一个 USBMUX 主机。
type Client struct {
	pipe Pipe

	mu       sync.Mutex
	closed   bool
	conns    map[uint16]*Conn
	nextPort uint16
	seq      uint32

	rx     []byte
	rxCond *sync.Cond
}

// Open 完成版本握手与 setup，返回可用的 USBMUX 客户端。
func Open(pipe Pipe) (*Client, error) {
	c := &Client{
		pipe:     pipe,
		conns:    make(map[uint16]*Conn),
		nextPort: FirstSourcePort,
	}
	c.rxCond = sync.NewCond(&c.mu)
	if err := c.handshake(); err != nil {
		c.Close()
		return nil, err
	}
	return c, nil
}

// handshake 做版本交换，随后发 setup 帧。
func (c *Client) handshake() error {
	if err := c.pipe.Write(VersionMessage()); err != nil {
		return err
	}
	// iPhone 会回一个同样的 proto=0 / length=20 / version=2 的包。
	// 注意：不要去等「协议 1」的版本回复，那样会丢掉合法回复并超时。
	deadline := time.Now().Add(HandshakeTimeout)
	for {
		frame, err := c.takeFrame(deadline)
		if err != nil {
			return err
		}
		if frame.Protocol == ProtocolVersion &&
			frame.Length == VersionMessageBytes &&
			frame.Word8 == USBMuxVersion {
			break
		}
		// 手机里可能残留上一次连接的 TCP 帧，跳过即可。
		if frame.Protocol != ProtocolTCP {
			return protocolf("版本回复异常: proto=%d length=%d version=%d",
				frame.Protocol, frame.Length, frame.Word8)
		}
	}
	setup := &Frame{Protocol: ProtocolSetup, Payload: []byte{SetupValue}}
	return c.pipe.Write(setup.Encode())
}

// Connect 向 iPhone 的指定端口发起一条 TCP 流。
//
// 返回的 *Conn 实现 net.Conn，iAP2 会话可以直接跑在上面。
func (c *Client) Connect(port uint16) (*Conn, error) {
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return nil, ErrClosed
	}
	source := c.nextPort
	c.nextPort++
	conn := &Conn{client: c, source: source, dest: port}
	c.conns[source] = conn
	c.mu.Unlock()

	// 发 SYN。
	if err := conn.sendFlags(TCPSyn, nil); err != nil {
		c.remove(source)
		return nil, err
	}
	deadline := time.Now().Add(ConnectTimeout)
	for !conn.established() {
		if !time.Now().Before(deadline) {
			c.remove(source)
			return nil, fmt.Errorf("usbmux: 连接端口 %d 超时", port)
		}
		time.Sleep(5 * time.Millisecond)
	}
	return conn, nil
}

// takeFrame 从接收缓冲里取一条完整帧，必要时继续读管道。
func (c *Client) takeFrame(deadline time.Time) (*Frame, error) {
	for {
		c.mu.Lock()
		if c.closed {
			c.mu.Unlock()
			return nil, ErrClosed
		}
		if len(c.rx) >= MuxHeaderBytes {
			if frame, err := ParseFrame(c.rx); err == nil {
				c.rx = c.rx[frame.Length:]
				c.mu.Unlock()
				return frame, nil
			} else if uint32(len(c.rx)) >= binary.LittleEndian.Uint32(c.rx[4:]) {
				c.mu.Unlock()
				return nil, ErrProtocol
			}
		}
		c.mu.Unlock()

		if !time.Now().Before(deadline) {
			return nil, fmt.Errorf("usbmux: 等待帧超时")
		}
		data, err := c.pipe.Read()
		if err != nil {
			return nil, err
		}
		if len(data) == 0 {
			time.Sleep(time.Millisecond)
			continue
		}
		c.mu.Lock()
		c.rx = append(c.rx, data...)
		c.mu.Unlock()
	}
}

// dispatch 分发一帧到对应的连接。
func (c *Client) dispatch(frame *Frame) {
	if frame.Protocol != ProtocolTCP || len(frame.Payload) < TCPHeaderBytes {
		return
	}
	dest := binary.LittleEndian.Uint16(frame.Payload[2:])
	c.mu.Lock()
	conn := c.conns[dest]
	c.mu.Unlock()
	if conn == nil {
		return
	}
	conn.onPacket(frame.Payload)
}

// remove 移除一条连接。
func (c *Client) remove(source uint16) {
	c.mu.Lock()
	delete(c.conns, source)
	c.mu.Unlock()
}

// Close 关闭 USBMUX 与底层管道。
func (c *Client) Close() error {
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return nil
	}
	c.closed = true
	conns := make([]*Conn, 0, len(c.conns))
	for _, conn := range c.conns {
		conns = append(conns, conn)
	}
	c.conns = make(map[uint16]*Conn)
	c.mu.Unlock()

	for _, conn := range conns {
		_ = conn.Close()
	}
	return c.pipe.Close()
}

// Conn 是 USBMUX 隧道里的一条 TCP 流，实现 net.Conn。
type Conn struct {
	client   *Client
	source   uint16
	dest     uint16
	mu       sync.Mutex
	seq      uint32
	ack      uint32
	up       bool
	down     bool
	rx       []byte
	rxNotify chan struct{}
}

func (c *Conn) established() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.up
}

// onPacket 处理收到的 TCP 包。
func (c *Conn) onPacket(packet []byte) {
	if len(packet) < TCPHeaderBytes {
		return
	}
	flags := packet[13]
	seq := binary.LittleEndian.Uint32(packet[4:])
	// TCP 头长度在 offset 12 的高 4 位，单位是 4 字节。
	headerBytes := int(packet[12]>>4&0x0f) * 4
	if headerBytes < TCPHeaderBytes || headerBytes > len(packet) {
		headerBytes = TCPHeaderBytes
	}
	payload := packet[headerBytes:]

	c.mu.Lock()
	switch {
	case flags&TCPSyn != 0 && flags&TCPAck != 0:
		c.up = true
		c.ack = seq + 1
		// 回一个 ACK 完成三次握手。
		go c.sendFlags(TCPAck, nil)
	case flags&TCPAck != 0:
		if !c.up && flags&TCPSyn == 0 {
			c.up = true
		}
	case flags&TCPFin != 0:
		c.down = true
	}
	if len(payload) > 0 && c.ack == 0 {
		c.ack = seq + uint32(len(payload))
	} else if len(payload) > 0 {
		c.ack += uint32(len(payload))
	}
	if len(payload) > 0 {
		c.rx = append(c.rx, payload...)
		select {
		case c.rxNotify <- struct{}{}:
		default:
		}
	}
	c.mu.Unlock()
}

// sendFlags 发一个带指定标志的 TCP 包。
func (c *Conn) sendFlags(flags byte, payload []byte) error {
	c.mu.Lock()
	packet := make([]byte, TCPHeaderBytes+len(payload))
	binary.LittleEndian.PutUint16(packet[0:], c.source)
	binary.LittleEndian.PutUint16(packet[2:], c.dest)
	binary.LittleEndian.PutUint32(packet[4:], c.seq)
	binary.LittleEndian.PutUint32(packet[8:], c.ack)
	packet[12] = byte(TCPHeaderBytes/4) << 4 // 头长，单位 4 字节
	packet[13] = flags
	binary.LittleEndian.PutUint16(packet[14:], TCPWindow)
	copy(packet[TCPHeaderBytes:], payload)
	if len(payload) > 0 {
		c.seq += uint32(len(payload))
	}
	c.mu.Unlock()

	frame := &Frame{Protocol: ProtocolTCP, Payload: packet}
	c.client.mu.Lock()
	closed := c.client.closed
	c.client.mu.Unlock()
	if closed {
		return ErrClosed
	}
	return c.client.pipe.Write(frame.Encode())
}

// Read 实现 net.Conn。
func (c *Conn) Read(b []byte) (int, error) {
	c.mu.Lock()
	if len(c.rx) == 0 {
		c.mu.Unlock()
		select {
		case <-c.rxNotify:
		case <-time.After(WriteTimeout):
			return 0, io.ErrNoProgress
		}
		c.mu.Lock()
	}
	n := copy(b, c.rx)
	c.rx = c.rx[n:]
	c.mu.Unlock()
	if n == 0 {
		return 0, io.EOF
	}
	return n, nil
}

// Write 实现 net.Conn。
func (c *Conn) Write(b []byte) (int, error) {
	if len(b) == 0 {
		return 0, nil
	}
	// 单次载荷不超过 16 KB，与 iPhone 的窗口一致。
	const maxPayload = 16 * 1024
	written := 0
	for len(b) > 0 {
		chunk := b
		if len(chunk) > maxPayload {
			chunk = chunk[:maxPayload]
		}
		if err := c.sendFlags(TCPAck, chunk); err != nil {
			return written, err
		}
		written += len(chunk)
		b = b[len(chunk):]
	}
	return written, nil
}

// Close 发 FIN 并从客户端注销。
func (c *Conn) Close() error {
	_ = c.sendFlags(TCPFin|TCPAck, nil)
	c.client.remove(c.source)
	return nil
}

// 以下方法满足 net.Conn 接口，隧道内没有真实的地址与超时语义。
func (c *Conn) LocalAddr() net.Addr                { return usbmuxAddr(c.source) }
func (c *Conn) RemoteAddr() net.Addr               { return usbmuxAddr(c.dest) }
func (c *Conn) SetDeadline(t time.Time) error      { return nil }
func (c *Conn) SetReadDeadline(t time.Time) error  { return nil }
func (c *Conn) SetWriteDeadline(t time.Time) error { return nil }

type usbmuxAddr uint16

func (a usbmuxAddr) Network() string { return "usbmux" }
func (a usbmuxAddr) String() string  { return fmt.Sprintf("usbmux:%d", uint16(a)) }
