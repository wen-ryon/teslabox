// Package csm 实现 iAP2 的控制会话消息（Control Session Message, CSM）层。
//
// 两层结构：
//
//	CSM 帧    0x4040 | u16 帧长 | u16 消息ID | 消息体
//	消息体    若干参数，每个参数 = u16 参数总长 | u16 参数ID | 载荷
//
// 全部大端，且**没有校验和**（校验由下层的链路帧负责）。
// 对照 xcertplay 的 iap2/wire：Iap2CsmFramer + Iap2Wire + Iap2WireCodec。
package csm

import (
	"encoding/binary"
	"errors"
	"fmt"
	"strings"
)

// 帧与参数的固定尺寸。
const (
	// Start 是 CSM 帧的起始标记。
	Start = 0x4040

	// HeaderBytes 是 CSM 帧头长度：起始标记 2 + 帧长 2 + 消息ID 2。
	HeaderBytes = 6

	// ParamHeaderBytes 是参数头长度：参数总长 2 + 参数ID 2。
	ParamHeaderBytes = 4

	// MaxFrameBytes 是一帧的最大长度（长度字段是 u16）。
	MaxFrameBytes = 0xffff

	// MaxBodyBytes 是消息体的最大长度。
	MaxBodyBytes = MaxFrameBytes - HeaderBytes

	// MaxParamBytes 是单个参数（含头）的最大长度。
	MaxParamBytes = 0xffff
)

// ErrProtocol 表示一个格式错误或不完整的 CSM 值。
var ErrProtocol = errors.New("csm: 协议错误")

func protocolf(format string, args ...any) error {
	return fmt.Errorf("%w: %s", ErrProtocol, fmt.Sprintf(format, args...))
}

// ---------- 标量编码 ----------

// U8 编码一个无符号字节。
func U8(v byte) []byte { return []byte{v} }

// Bool 编码布尔值：真为 1，假为 0。
func Bool(v bool) []byte {
	if v {
		return []byte{1}
	}
	return []byte{0}
}

// U16 编码大端 uint16。
func U16(v uint16) []byte {
	out := make([]byte, 2)
	binary.BigEndian.PutUint16(out, v)
	return out
}

// U32 编码大端 uint32。
func U32(v uint32) []byte {
	out := make([]byte, 4)
	binary.BigEndian.PutUint32(out, v)
	return out
}

// U64 编码大端 uint64。
func U64(v uint64) []byte {
	out := make([]byte, 8)
	binary.BigEndian.PutUint64(out, v)
	return out
}

// String 编码一个以 NUL 结尾的 UTF-8 字符串。
// 字符串内部不能含有 U+0000，否则接收方无法正确切分。
func String(v string) ([]byte, error) {
	if strings.ContainsRune(v, 0) {
		return nil, protocolf("字符串不能包含 U+0000")
	}
	return append([]byte(v), 0), nil
}

// MustString 同 String，但内部字符串出现 NUL 时直接 panic。
// 仅用于编译期已知的常量字符串。
func MustString(v string) []byte {
	out, err := String(v)
	if err != nil {
		panic(err)
	}
	return out
}

// Strings 编码多个 NUL 结尾的字符串，首尾相连。
func Strings(values ...string) ([]byte, error) {
	out := make([]byte, 0, 32)
	for _, v := range values {
		encoded, err := String(v)
		if err != nil {
			return nil, err
		}
		out = append(out, encoded...)
	}
	return out, nil
}

// U16List 编码一组大端 uint16。
func U16List(values ...uint16) []byte {
	out := make([]byte, 0, len(values)*2)
	for _, v := range values {
		out = append(out, U16(v)...)
	}
	return out
}

// ---------- 标量解码 ----------

// ReadU8 读取一个字节。
func ReadU8(b []byte, offset int) (byte, error) {
	if err := require(b, offset, 1); err != nil {
		return 0, err
	}
	return b[offset], nil
}

// ReadBool 读取布尔值。nonzeroIsTrue 为真时任何非零都算真。
func ReadBool(b []byte, offset int, nonzeroIsTrue bool) (bool, error) {
	v, err := ReadU8(b, offset)
	if err != nil {
		return false, err
	}
	if nonzeroIsTrue {
		return v != 0, nil
	}
	return v == 1, nil
}

// ReadU16 读取大端 uint16。
func ReadU16(b []byte, offset int) (uint16, error) {
	if err := require(b, offset, 2); err != nil {
		return 0, err
	}
	return binary.BigEndian.Uint16(b[offset:]), nil
}

// ReadU32 读取大端 uint32。
func ReadU32(b []byte, offset int) (uint32, error) {
	if err := require(b, offset, 4); err != nil {
		return 0, err
	}
	return binary.BigEndian.Uint32(b[offset:]), nil
}

// ReadU64 读取大端 uint64。
func ReadU64(b []byte, offset int) (uint64, error) {
	if err := require(b, offset, 8); err != nil {
		return 0, err
	}
	return binary.BigEndian.Uint64(b[offset:]), nil
}

// ReadString 读取一个 NUL 结尾的字符串，要求载荷以 0 收尾。
func ReadString(b []byte) (string, error) {
	if len(b) == 0 {
		return "", protocolf("字符串载荷为空")
	}
	if b[len(b)-1] != 0 {
		return "", protocolf("字符串未以 NUL 结尾")
	}
	return string(b[:len(b)-1]), nil
}

// ReadStrings 读取首尾相连的多个 NUL 结尾字符串。
func ReadStrings(b []byte) ([]string, error) {
	if len(b) == 0 {
		return nil, nil
	}
	if b[len(b)-1] != 0 {
		return nil, protocolf("字符串数组未以 NUL 结尾")
	}
	var out []string
	start := 0
	for i, c := range b {
		if c != 0 {
			continue
		}
		out = append(out, string(b[start:i]))
		start = i + 1
	}
	return out, nil
}

// ReadU16List 读取一组大端 uint16，长度必须是偶数。
func ReadU16List(b []byte) ([]uint16, error) {
	if len(b)%2 != 0 {
		return nil, protocolf("u16 数组长度为奇数: %d", len(b))
	}
	out := make([]uint16, 0, len(b)/2)
	for i := 0; i < len(b); i += 2 {
		out = append(out, binary.BigEndian.Uint16(b[i:]))
	}
	return out, nil
}

func require(b []byte, offset, n int) error {
	if offset < 0 || n < 0 || offset+n > len(b) {
		return protocolf("需要 %d 字节（偏移 %d），但载荷只有 %d 字节", n, offset, len(b))
	}
	return nil
}
