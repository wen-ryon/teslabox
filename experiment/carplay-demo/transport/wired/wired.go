// Package wired 建立有线（USB）CarPlay 的数据通道。
//
// 完整链路：
//
//	iPhone 插入 → USB 枚举出 bulk 端点
//	          → 配件发 IAP2Marker，iPhone 切进 iAP2 模式
//	          → USBMUX v2 版本握手 + setup
//	          → USBMUX 内拨号到 lockdown 端口 62078
//	          → 得到一条 net.Conn，iAP2 会话跑在上面
//
// 本包只负责打通到 net.Conn 为止，协议逻辑交给上层的 carplay.Host。
package wired

import (
	"errors"
	"net"

	"github.com/wen-ryon/teslabox/experiment/carplay-demo/iap2/link"
	"github.com/wen-ryon/teslabox/experiment/carplay-demo/transport/usbmux"
)

// MarkerPipe 是 iPhone 的 USB bulk 管道。
//
// SendMarker 必须在任何 USBMUX 帧之前调用：iPhone 靠这 6 个字节
// 判断对面是 iAP2 配件，否则它一直停在普通 USB 设备模式。
type MarkerPipe interface {
	usbmux.Pipe
	// SendMarker 发出 link.IAP2Marker（FF 55 02 00 EE 10）。
	SendMarker() error
}

// ErrNoDevice 表示没有找到 iPhone。
var ErrNoDevice = errors.New("wired: 未检测到 iPhone")

// Dial 在已打开的 bulk 管道上完成 iAP2 模式切换与 USBMUX 握手，
// 返回一条可以跑 iAP2 的字节流。
func Dial(pipe MarkerPipe) (net.Conn, error) {
	if err := pipe.SendMarker(); err != nil {
		return nil, err
	}
	mux, err := usbmux.Open(pipe)
	if err != nil {
		return nil, err
	}
	conn, err := mux.Connect(usbmux.LockdownPort)
	if err != nil {
		_ = mux.Close()
		return nil, err
	}
	return &sessionConn{Conn: conn, mux: mux, pipe: pipe}, nil
}

// sessionConn 把 USBMUX 连接与底层资源绑在一起，关闭时一并释放。
type sessionConn struct {
	net.Conn
	mux  *usbmux.Client
	pipe usbmux.Pipe
}

// Close 关闭连接、USBMUX 与 USB 管道。
func (c *sessionConn) Close() error {
	err := c.Conn.Close()
	if muxErr := c.mux.Close(); err == nil {
		err = muxErr
	}
	return err
}

// Marker 返回配件握手标记，方便实现方直接取用。
func Marker() []byte {
	return append([]byte(nil), link.IAP2Marker...)
}
