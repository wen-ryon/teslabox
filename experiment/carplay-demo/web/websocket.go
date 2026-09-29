// Package web 提供浏览器访问通道：视频帧下行、触摸事件上行。
//
// 之所以自己实现 WebSocket 而不引第三方库：这个 module 要保持零依赖，
// 而服务端只需要「握手 + 收发帧」这一小块能力（RFC 6455 的一个子集）。
package web

import (
	"bufio"
	"crypto/sha1"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"sync"
)

// websocketGUID 是 RFC 6455 规定的握手魔串。
const websocketGUID = "258EAFA5-E914-47DA-95CA-C5AB0DC85B11"

// WebSocket 操作码。
const (
	opContinuation = 0x0
	opText         = 0x1
	opBinary       = 0x2
	opClose        = 0x8
	opPing         = 0x9
	opPong         = 0xa
)

// ErrNotUpgradable 表示请求不是合法的 WebSocket 升级请求。
var ErrNotUpgradable = errors.New("web: 不是合法的 WebSocket 升级请求")

// wsConn 是一个最小 WebSocket 连接。
type wsConn struct {
	conn   net.Conn
	reader *bufio.Reader
	// writeMu 保证一整帧连续写出，避免并发写互相插队。
	writeMu sync.Mutex
}

// upgrade 完成 WebSocket 握手，接管底层连接。
func upgrade(w http.ResponseWriter, r *http.Request) (*wsConn, error) {
	if !strings.EqualFold(r.Header.Get("Upgrade"), "websocket") {
		return nil, ErrNotUpgradable
	}
	key := r.Header.Get("Sec-WebSocket-Key")
	if key == "" {
		return nil, ErrNotUpgradable
	}
	if !headerContainsToken(r.Header, "Connection", "Upgrade") {
		return nil, ErrNotUpgradable
	}

	hijacker, ok := w.(http.Hijacker)
	if !ok {
		return nil, errors.New("web: 当前 http.ResponseWriter 不支持劫持连接")
	}
	conn, buf, err := hijacker.Hijack()
	if err != nil {
		return nil, err
	}

	accept := computeAccept(key)
	_, err = fmt.Fprintf(conn,
		"HTTP/1.1 101 Switching Protocols\r\n"+
			"Upgrade: websocket\r\n"+
			"Connection: Upgrade\r\n"+
			"Sec-WebSocket-Accept: %s\r\n\r\n", accept)
	if err != nil {
		conn.Close()
		return nil, err
	}
	// Hijack 返回的是 ReadWriter，这里只需要它的读半边。
	return &wsConn{conn: conn, reader: buf.Reader}, nil
}

// computeAccept 计算 Sec-WebSocket-Accept。
func computeAccept(key string) string {
	sum := sha1.Sum([]byte(key + websocketGUID))
	return base64.StdEncoding.EncodeToString(sum[:])
}

// WriteMessage 发出一条完整帧。服务端发出的帧不加掩码。
func (c *wsConn) WriteMessage(opcode byte, payload []byte) error {
	c.writeMu.Lock()
	defer c.writeMu.Unlock()

	header := make([]byte, 0, 10)
	header = append(header, 0x80|opcode) // FIN + opcode

	switch {
	case len(payload) < 126:
		header = append(header, byte(len(payload)))
	case len(payload) < 65536:
		header = append(header, 126, byte(len(payload)>>8), byte(len(payload)))
	default:
		header = append(header, 127,
			byte(len(payload)>>56), byte(len(payload)>>48),
			byte(len(payload)>>40), byte(len(payload)>>32),
			byte(len(payload)>>24), byte(len(payload)>>16),
			byte(len(payload)>>8), byte(len(payload)))
	}
	if _, err := c.conn.Write(header); err != nil {
		return err
	}
	if len(payload) > 0 {
		_, err := c.conn.Write(payload)
		return err
	}
	return nil
}

// ReadMessage 读取一条帧。客户端发来的帧带掩码，需要还原。
func (c *wsConn) ReadMessage() (byte, []byte, error) {
	header := make([]byte, 2)
	if _, err := io.ReadFull(c.reader, header); err != nil {
		return 0, nil, err
	}
	opcode := header[0] & 0x0f
	masked := header[1]&0x80 != 0
	length := int64(header[1] & 0x7f)

	switch length {
	case 126:
		extended := make([]byte, 2)
		if _, err := io.ReadFull(c.reader, extended); err != nil {
			return 0, nil, err
		}
		length = int64(extended[0])<<8 | int64(extended[1])
	case 127:
		extended := make([]byte, 8)
		if _, err := io.ReadFull(c.reader, extended); err != nil {
			return 0, nil, err
		}
		length = 0
		for _, b := range extended {
			length = length<<8 | int64(b)
		}
	}
	if length < 0 || length > maxMessageBytes {
		return 0, nil, fmt.Errorf("web: 消息长度 %d 超出限制", length)
	}

	var mask [4]byte
	if masked {
		if _, err := io.ReadFull(c.reader, mask[:]); err != nil {
			return 0, nil, err
		}
	}
	payload := make([]byte, length)
	if length > 0 {
		if _, err := io.ReadFull(c.reader, payload); err != nil {
			return 0, nil, err
		}
		if masked {
			for i := range payload {
				payload[i] ^= mask[i%4]
			}
		}
	}
	return opcode, payload, nil
}

// Close 关闭底层连接。
func (c *wsConn) Close() error { return c.conn.Close() }

// maxMessageBytes 是单条消息的上限，防止恶意客户端喂超大帧。
const maxMessageBytes = 16 << 20

func headerContainsToken(header http.Header, name, token string) bool {
	for _, value := range header.Values(name) {
		for _, part := range strings.Split(value, ",") {
			if strings.EqualFold(strings.TrimSpace(part), token) {
				return true
			}
		}
	}
	return false
}
