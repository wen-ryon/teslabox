// Package media 定义视频与输入的通道接口。
//
// 这一层是给「网页端访问」预留的接缝：本项目跑在 Rock 3A 上，
// 而画面最终要在**特斯拉车机的浏览器**里显示——车机只能通过浏览器访问，
// 没有 HDMI 直出的机会，也不走 DRM/KMS 渲染。
//
// 所以这里只定义接口和数据模型，不实现解码、渲染和输入采集：
//
//	视频：iPhone → 板子（协议栈产出 H.264）→ 实现 VideoSink → 浏览器（WebRTC / MSE）
//	触摸：浏览器（touch 事件）→ 实现 TouchSource → 板子 → iAP2 HID → iPhone
//
// **音频不做**：手机直接连车机蓝牙放声音，CarPlay 的音频通道不接入。
//
// 后期只要实现这两个接口，就能把画面接到网页上，协议栈代码一行不用改。
package media

import (
	"errors"
	"time"
)

// ErrClosed 表示通道已关闭，不能再写入。
var ErrClosed = errors.New("media: 通道已关闭")

// VideoCodec 描述视频流的编码格式。
type VideoCodec uint8

const (
	VideoH264 VideoCodec = iota // H.264 / AVC，CarPlay 有线和无线都用这个
	VideoHEVC                   // H.265，部分新机型在无线模式下使用
)

func (c VideoCodec) String() string {
	switch c {
	case VideoH264:
		return "h264"
	case VideoHEVC:
		return "hevc"
	default:
		return "unknown"
	}
}

// VideoFormat 描述一次视频流的参数。分辨率或 SPS/PPS 变化时必须重新协商。
type VideoFormat struct {
	Codec     VideoCodec
	Width     int
	Height    int
	FrameRate float64
	// SPS 与 PPS 是 H.264 的解码参数集。带内（in-band）流可以为空，
	// 但 WebRTC / MSE 通常需要把它们单独交给解码器。
	SPS []byte
	PPS []byte
}

// VideoFrame 是一帧编码后的视频数据。
//
// Payload 通常是一个完整的访问单元（Access Unit），可以含多个 NALU。
// 为了避免每帧分配，实现方可以在 Write 返回后复用 Payload——调用方不得持有引用。
type VideoFrame struct {
	Format    VideoFormat
	Payload   []byte
	Timestamp time.Time
	// KeyFrame 标记这一帧是否关键帧（IDR）。丢帧策略和非关键帧丢弃都依赖它。
	KeyFrame bool
	// Sequence 是单调递增的帧序号，用于检测丢帧和实现乱序重排。
	Sequence uint64
}

// VideoSink 消费视频帧。
//
// 实现要点：
//   - Write 必须快速返回，不得在内部做阻塞的 I/O，否则会反压到 iAP2 接收循环；
//   - 帧可以丢弃——网络拥塞时优先丢非关键帧；
//   - 关闭后再次 Write 应返回 ErrClosed。
type VideoSink interface {
	// SetFormat 在流参数变化时被调用，实现方据此重建解码器或重新协商 WebRTC。
	SetFormat(format VideoFormat) error
	// Write 提交一帧。允许丢弃。
	Write(frame VideoFrame) error
	Close() error
}

// 音频通道刻意不定义：手机直接连车机蓝牙放声音，
// CarPlay 的音频流不需要也不应该走板子。

// TouchAction 描述一次触摸的动作类型。
type TouchAction uint8

const (
	TouchDown   TouchAction = iota // 按下
	TouchMove                      // 移动
	TouchUp                        // 抬起
	TouchCancel                    // 取消（例如多点触控被系统打断）
)

// TouchEvent 是一次触摸事件，坐标以 CarPlay 画面的逻辑分辨率为准。
//
// 网页端拿到的是 CSS 像素，需要在提交前按画面实际分辨率归一化到 0..Width/Height。
type TouchEvent struct {
	Action TouchAction
	// X / Y 是归一化后的坐标，原点在左上角。
	X float32
	Y float32
	// PointerID 区分多指触控的不同手指。
	PointerID uint32
	Timestamp time.Time
}

// TouchSource 提供输入事件。
//
// 网页端的 WebSocket 收到触摸事件后写入这里，协议栈再翻译成 iAP2 HID 报告发给 iPhone。
// Next 在没有事件时应阻塞等待；Close 之后必须让 Next 立即返回错误。
type TouchSource interface {
	Next() (TouchEvent, error)
	Close() error
}

// ButtonEvent 是物理按键或旋钮事件。
type ButtonEvent struct {
	// Code 是 HID usage code，具体取值由 iAP2 HID 描述符决定。
	Code uint16
	// Pressed 表示按下还是松开。
	Pressed   bool
	Timestamp time.Time
}

// ButtonSource 提供按键事件，语义同 TouchSource。
type ButtonSource interface {
	Next() (ButtonEvent, error)
	Close() error
}

// SinkSet 把一路流的通道打包，便于整体替换。
type SinkSet struct {
	Video VideoSink
	// Touch 与 Button 是输入方向，由网页端驱动。
	Touch  TouchSource
	Button ButtonSource
}

// Close 关闭全部已设置的通道，返回第一个错误。
func (s *SinkSet) Close() error {
	var first error
	for _, c := range []interface{ Close() error }{s.Video, s.Touch, s.Button} {
		// 接口零值比较：只有真正设置了的通道才需要关闭。
		if c == nil {
			continue
		}
		if err := c.Close(); err != nil && first == nil {
			first = err
		}
	}
	return first
}

// Normalize 把网页的 CSS 像素坐标换算成 CarPlay 逻辑坐标。
//
// viewWidth/viewHeight 是浏览器里画面的显示尺寸，
// logicalWidth/logicalHeight 是 iPhone 那边报上来的画面分辨率。
func Normalize(x, y, viewWidth, viewHeight, logicalWidth, logicalHeight float32) (float32, float32) {
	if viewWidth <= 0 || viewHeight <= 0 || logicalWidth <= 0 || logicalHeight <= 0 {
		return 0, 0
	}
	return (x / viewWidth) * logicalWidth, (y / viewHeight) * logicalHeight
}
