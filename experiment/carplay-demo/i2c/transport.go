// Package i2c 定义访问 MFi 认证协处理器的 I2C 传输抽象。
//
// 之所以要有这一层抽象：同一颗 MFi 芯片可能挂在两种不同的通路上——
// 通过 CH341 USB 转接板（开发调试用），或者直接焊在板载 I2C 总线上（量产形态）。
// 上层协议栈只依赖本包的 Transport 接口，两种后端可以互换。
package i2c

import (
	"errors"
	"fmt"
)

// Transport 抽象一条 I2C 总线上的单次事务。
//
// 语义与 xcertplay 的 I2cTransport 对齐：write 阶段以 STOP 结束，
// read 是一次独立的事务（两次之间需要间隔，间隔由具体实现负责）。
// write 为 nil 且 readLen 为 0 是非法请求；两者可以同时有效。
type Transport interface {
	// Transaction 先写入 write（可为 nil），再读取 readLen 字节（可为 0）。
	// 返回读取到的数据；readLen 为 0 时返回 nil。
	Transaction(addr7 uint8, write []byte, readLen int) ([]byte, error)
}

// 设备版本无效值。xcertplay 用这两个值判定「该地址上没有器件」。
const (
	InvalidVersionZero = 0x00
	InvalidVersionHigh = 0xff
)

// NackError 表示总线上没有器件应答某一字节。
// 注意：首次 NACK 往往只是唤醒休眠中的协处理器，调用方应当重试而非直接失败。
type NackError struct {
	Addr   uint8 // 7 位从机地址
	Byte   int   // 第几个被写的字节（从 0 开始）
	Status uint8 // CH341 回传的状态字节
}

func (e *NackError) Error() string {
	return fmt.Sprintf("i2c: 地址 0x%02x 第 %d 字节被 NACK（状态 0x%02x）", e.Addr, e.Byte, e.Status)
}

// ErrNoDevice 表示候选地址上都没有器件响应。
var ErrNoDevice = errors.New("i2c: 总线上没有器件响应")

// ErrShortRead 表示读到的数据比请求的长度短。
type ErrShortRead struct {
	Want int
	Got  int
}

func (e *ErrShortRead) Error() string {
	return fmt.Sprintf("i2c: 读到 %d 字节，期望 %d 字节", e.Got, e.Want)
}
