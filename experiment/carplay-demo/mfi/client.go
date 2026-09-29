// Package mfi 实现与 Apple MFi 认证协处理器（iPod Accessories 认证芯片）的交互。
//
// 该芯片挂在 I2C 上，车机侧用它向 iPhone 证明「我是合法配件」。
// 访问方式是寄存器式的：先用一次只写事务选中寄存器（带 STOP），
// 再用一次纯读事务取数据——这个两段式序列来自 LIVI 的实现，
// 也被 xcertplay 沿用。
//
// 本包只依赖 i2c.Transport，因此同一套逻辑既能跑在 CH341 USB 转接板上，
// 也能跑在板载 I2C 总线上。
package mfi

import (
	"encoding/binary"
	"errors"
	"fmt"
	"time"

	"github.com/wen-ryon/teslabox/experiment/carplay-demo/i2c"
)

// 寄存器地址。
const (
	RegDeviceVersion     = 0x00 // 器件版本
	RegProtocolMajor     = 0x02 // 协议主版本
	RegError             = 0x05 // 错误码
	RegAuthControlStatus = 0x10 // 认证控制/状态
	RegResponseLength    = 0x11 // 签名响应长度（大端 uint16）
	RegResponseData      = 0x12 // 签名响应数据
	RegChallengeLength   = 0x20 // 挑战长度（大端 uint16）
	RegChallengeData     = 0x21 // 挑战数据
	RegCertificateLength = 0x30 // 证书长度（大端 uint16）
	RegCertificateData   = 0x31 // 证书数据窗口起点
)

const (
	// AuthStart 写入 RegAuthControlStatus 启动一次签名运算。
	AuthStart = 0x01
	// AuthSuccess 是认证完成后的状态值。
	AuthSuccess = 0x10

	// CertificateWindow 是证书数据寄存器窗口大小：每读满 128 字节，
	// 寄存器号加一继续读。
	CertificateWindow = 128

	// MaxCertificate 是证书读取上限，防止芯片返回离谱的长度。
	MaxCertificate = 65525

	// MinChallenge / MaxChallenge 是挑战数据的长度约束。
	MinChallenge = 1
	MaxChallenge = 128
)

// 默认时序参数。
const (
	DefaultIOTimeout   = 2 * time.Second
	DefaultRetryDelay  = 20 * time.Millisecond
	DefaultAuthTimeout = 3 * time.Second
	DefaultAuthPoll    = 10 * time.Millisecond
	DefaultAuthSettle  = 10 * time.Millisecond
	DefaultScanTimeout = 2 * time.Second
)

// Chip 是一颗被发现的认证协处理器。
type Chip struct {
	Addr7         uint8 // 7 位 I2C 地址
	DeviceVersion uint8 // 寄存器 0x00 的值
}

// WriteAddr 返回 8 位写地址。
func (c Chip) WriteAddr() uint8 { return c.Addr7 << 1 }

// ReadAddr 返回 8 位读地址。
func (c Chip) ReadAddr() uint8 { return c.Addr7<<1 | 1 }

func (c Chip) String() string {
	return fmt.Sprintf("MFi@0x%02x (version 0x%02x)", c.Addr7, c.DeviceVersion)
}

// Client 面向单颗芯片的寄存器访问客户端。
//
// 芯片在空闲时会休眠，第一次访问常常 NACK——这是正常的唤醒过程，
// 所以每次读写都带重试。Client 的方法不是并发安全的：同一颗芯片的
// 事务必须串行化（xcertplay 也是用一把全局锁串行化访问）。
type Client struct {
	transport i2c.Transport
	addr      uint8

	IOTimeout   time.Duration // 单次寄存器读写的重试窗口
	RetryDelay  time.Duration // 重试间隔
	AuthTimeout time.Duration // 等待签名完成的超时
	AuthPoll    time.Duration // 轮询状态间隔
}

// NewClient 返回一个客户端。opts 为空时使用默认时序。
func NewClient(tr i2c.Transport, addr7 uint8) *Client {
	return &Client{
		transport:   tr,
		addr:        addr7,
		IOTimeout:   DefaultIOTimeout,
		RetryDelay:  DefaultRetryDelay,
		AuthTimeout: DefaultAuthTimeout,
		AuthPoll:    DefaultAuthPoll,
	}
}

// Addr 返回芯片的 7 位地址。
func (c *Client) Addr() uint8 { return c.addr }

// retry 在重试窗口内反复执行 fn，直到成功或超时。
// 休眠中的协处理器需要被唤醒，单次失败不能当作器件不存在。
func (c *Client) retry(fn func() error) error {
	deadline := time.Now().Add(c.IOTimeout)
	var last error
	for {
		last = fn()
		if last == nil {
			return nil
		}
		if !time.Now().Before(deadline) {
			return last
		}
		time.Sleep(c.RetryDelay)
	}
}

// readRegister 选中寄存器后读取 length 字节。
func (c *Client) readRegister(register uint8, length int) ([]byte, error) {
	if length < 1 {
		return nil, errors.New("mfi: 读取长度必须为正")
	}
	var data []byte
	err := c.retry(func() error {
		if _, err := c.transport.Transaction(c.addr, []byte{register}, 0); err != nil {
			return err
		}
		got, err := c.transport.Transaction(c.addr, nil, length)
		if err != nil {
			return err
		}
		if len(got) != length {
			return &i2c.ErrShortRead{Want: length, Got: len(got)}
		}
		data = got
		return nil
	})
	return data, err
}

// writeRegister 向寄存器写入数据。
func (c *Client) writeRegister(register uint8, payload []byte) error {
	buf := make([]byte, 0, len(payload)+1)
	buf = append(buf, register)
	buf = append(buf, payload...)
	return c.retry(func() error {
		got, err := c.transport.Transaction(c.addr, buf, 0)
		if err != nil {
			return err
		}
		if len(got) != 0 {
			return fmt.Errorf("mfi: 写寄存器 0x%02x 返回了 %d 字节数据", register, len(got))
		}
		return nil
	})
}

// DeviceVersion 读取寄存器 0x00。
func (c *Client) DeviceVersion() (uint8, error) {
	data, err := c.readRegister(RegDeviceVersion, 1)
	if err != nil {
		return 0, err
	}
	return data[0], nil
}

// ProtocolMajor 读取寄存器 0x02。
func (c *Client) ProtocolMajor() (uint8, error) {
	data, err := c.readRegister(RegProtocolMajor, 1)
	if err != nil {
		return 0, err
	}
	return data[0], nil
}

// ErrorCode 读取寄存器 0x05。
func (c *Client) ErrorCode() (uint8, error) {
	data, err := c.readRegister(RegError, 1)
	if err != nil {
		return 0, err
	}
	return data[0], nil
}

// CertificateLength 读取证书长度（大端 uint16）。
func (c *Client) CertificateLength() (int, error) {
	data, err := c.readRegister(RegCertificateLength, 2)
	if err != nil {
		return 0, err
	}
	n := int(binary.BigEndian.Uint16(data))
	if n < 1 || n > MaxCertificate {
		return 0, fmt.Errorf("mfi: 证书长度 %d 超出 1..%d", n, MaxCertificate)
	}
	return n, nil
}

// Certificate 读出完整的 DER PKCS#7 证书。
//
// 证书按 128 字节窗口分页：每读满一页，寄存器号加一。
func (c *Client) Certificate() ([]byte, error) {
	total, err := c.CertificateLength()
	if err != nil {
		return nil, err
	}
	out := make([]byte, 0, total)
	register := uint8(RegCertificateData)
	for len(out) < total {
		count := CertificateWindow
		if remaining := total - len(out); remaining < count {
			count = remaining
		}
		data, err := c.readRegister(register, count)
		if err != nil {
			return nil, fmt.Errorf("mfi: 读证书窗口 0x%02x 失败: %w", register, err)
		}
		out = append(out, data...)
		register++
	}
	return out[:total], nil
}

// SignChallenge 让芯片对 iPhone 下发的挑战签名，返回签名结果。
//
// 芯片需要时间完成运算，所以要轮询 RegAuthControlStatus 直到变成 AuthSuccess。
func (c *Client) SignChallenge(challenge []byte) ([]byte, error) {
	if len(challenge) < MinChallenge || len(challenge) > MaxChallenge {
		return nil, fmt.Errorf("mfi: 挑战长度必须在 %d..%d 之间，得到 %d",
			MinChallenge, MaxChallenge, len(challenge))
	}

	length := make([]byte, 2)
	binary.BigEndian.PutUint16(length, uint16(len(challenge)))
	if err := c.writeRegister(RegChallengeLength, length); err != nil {
		return nil, err
	}
	if err := c.writeRegister(RegChallengeData, challenge); err != nil {
		return nil, err
	}
	if err := c.writeRegister(RegAuthControlStatus, []byte{AuthStart}); err != nil {
		return nil, err
	}

	time.Sleep(DefaultAuthSettle)
	deadline := time.Now().Add(c.AuthTimeout)
	for {
		status, err := c.readRegister(RegAuthControlStatus, 1)
		if err == nil && status[0] == AuthSuccess {
			break
		}
		if !time.Now().Before(deadline) {
			return nil, fmt.Errorf("mfi: 等待签名超时，最后状态 %v", err)
		}
		time.Sleep(c.AuthPoll)
	}

	raw, err := c.readRegister(RegResponseLength, 2)
	if err != nil {
		return nil, err
	}
	n := int(binary.BigEndian.Uint16(raw))
	if n < 1 || n > MaxCertificate {
		return nil, fmt.Errorf("mfi: 签名长度 %d 超出范围", n)
	}
	return c.readRegister(RegResponseData, n)
}

// Scanner 在候选地址上找第一颗响应的芯片。
type Scanner struct {
	Transport  i2c.Transport
	Candidates []uint8       // 默认 {0x10, 0x11}，顺序与 LIVI 一致
	Timeout    time.Duration // 整体探测窗口
	RetryDelay time.Duration
}

// Scan 返回第一颗返回有效器件版本的芯片。
//
// 注意第一次探测大概率全 NACK：那只是唤醒休眠的协处理器，
// 所以这里在超时窗口内反复轮询候选地址，而不是一次失败就放弃。
func (s *Scanner) Scan() (*Chip, error) {
	candidates := s.Candidates
	if len(candidates) == 0 {
		candidates = []uint8{0x10, 0x11}
	}
	timeout := s.Timeout
	if timeout <= 0 {
		timeout = DefaultScanTimeout
	}
	delay := s.RetryDelay
	if delay <= 0 {
		delay = DefaultRetryDelay
	}

	deadline := time.Now().Add(timeout)
	var lastErr error
	for {
		for _, addr := range candidates {
			client := NewClient(s.Transport, addr)
			client.IOTimeout = 0 // 单次尝试，由外层循环负责重试
			version, err := client.DeviceVersion()
			if err != nil {
				lastErr = err
				continue
			}
			if version == i2c.InvalidVersionZero || version == i2c.InvalidVersionHigh {
				lastErr = fmt.Errorf("mfi: 地址 0x%02x 返回的器件版本 0x%02x 无效", addr, version)
				continue
			}
			return &Chip{Addr7: addr, DeviceVersion: version}, nil
		}
		if !time.Now().Before(deadline) {
			if lastErr == nil {
				lastErr = i2c.ErrNoDevice
			}
			return nil, fmt.Errorf("%w（已探测 %v）", lastErr, candidates)
		}
		time.Sleep(delay)
	}
}
