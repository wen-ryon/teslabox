// Package web 提供浏览器访问通道。
//
// 特斯拉车机只能通过浏览器访问，所以板子要把画面和交互都搬到网页上：
//
//	下行：H.264 视频帧 → WebSocket 二进制帧 → 浏览器（WebRTC 或 MSE 解码播放）
//	上行：浏览器触摸事件 → WebSocket 文本帧（JSON）→ 板子 → iAP2 HID → iPhone
//
// 音频不做：手机直接连车机蓝牙放声音。
package web

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"strconv"
	"sync"
	"time"
)

// Status 是给前端看的连接状态。
type Status struct {
	// State 是 CarPlay 启动链路的当前阶段。
	State string `json:"state"`
	// Wireless 标记当前是有线还是无线。
	Wireless bool `json:"wireless"`
	// VideoFormat 描述当前视频流参数，未建立时为空。
	VideoFormat map[string]any `json:"videoFormat,omitempty"`
	// Viewers 是正在观看的浏览器数量。
	Viewers int `json:"viewers"`
}

// TouchEvent 是浏览器上报的触摸事件，坐标是画面内的 CSS 像素。
type TouchEvent struct {
	Action string  `json:"action"` // down / move / up / cancel
	X      float32 `json:"x"`
	Y      float32 `json:"y"`
	// ViewWidth / ViewHeight 是浏览器里画面的显示尺寸，用于归一化。
	ViewWidth  float32 `json:"viewWidth"`
	ViewHeight float32 `json:"viewHeight"`
	PointerID  uint32  `json:"pointerId,omitempty"`
}

// ActionCode 把前端的动作字符串映射成 HID 动作码。
func (e TouchEvent) ActionCode() int {
	switch e.Action {
	case "down":
		return 0
	case "move":
		return 1
	case "up":
		return 2
	default:
		return 3 // cancel
	}
}

// Server 是给浏览器提供服务的 HTTP 服务器。
type Server struct {
	// Addr 是监听地址，例如 ":8080"。
	Addr string

	// Status 返回当前状态，由调用方注入。
	Status func() Status

	// Video 是视频帧来源；每收到一帧就推给所有浏览器。
	// 帧应当是完整的 H.264 访问单元。
	Video <-chan []byte

	// Touch 接收浏览器上报的触摸事件，调用方负责转发给 CarPlay。
	Touch chan<- TouchEvent

	mu      sync.Mutex
	viewers map[*wsConn]struct{}
}

// NewServer 构造服务器。
func NewServer(addr string) *Server {
	return &Server{
		Addr:    addr,
		viewers: make(map[*wsConn]struct{}),
	}
}

// Run 启动 HTTP 服务，直到 ctx 取消。
func (s *Server) Run(ctx context.Context) error {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/status", s.handleStatus)
	mux.HandleFunc("/ws", s.handleWebSocket)
	mux.HandleFunc("/", s.handleIndex)

	server := &http.Server{
		Addr:    s.Addr,
		Handler: mux,
	}

	if s.Video != nil {
		go s.fanOutVideo(ctx)
	}

	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = server.Shutdown(shutdownCtx)
	}()

	listener, err := net.Listen("tcp", s.Addr)
	if err != nil {
		return err
	}
	return server.Serve(listener)
}

// handleStatus 返回 JSON 状态。
func (s *Server) handleStatus(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	status := Status{}
	if s.Status != nil {
		status = s.Status()
	}
	s.mu.Lock()
	status.Viewers = len(s.viewers)
	s.mu.Unlock()
	_ = json.NewEncoder(w).Encode(status)
}

// handleIndex 返回一个最小页面，方便直接打开验证链路是否通。
//
// 真正的渲染交给前端项目；这里只说明接口约定。
func (s *Server) handleIndex(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = w.Write([]byte(indexPage))
}

// handleWebSocket 处理浏览器连接：下行推视频，上行收触摸。
func (s *Server) handleWebSocket(w http.ResponseWriter, r *http.Request) {
	conn, err := upgrade(w, r)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	s.addViewer(conn)
	defer func() {
		s.removeViewer(conn)
		_ = conn.Close()
	}()

	for {
		opcode, payload, err := conn.ReadMessage()
		if err != nil {
			return
		}
		switch opcode {
		case opText:
			var event TouchEvent
			if err := json.Unmarshal(payload, &event); err != nil {
				continue
			}
			if s.Touch != nil {
				select {
				case s.Touch <- event:
				default:
					// 处理不过来就丢掉，触摸事件允许丢失。
				}
			}
		case opClose:
			return
		case opPing:
			_ = conn.WriteMessage(opPong, payload)
		}
	}
}

// fanOutVideo 把视频帧广播给所有浏览器。
func (s *Server) fanOutVideo(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case frame, ok := <-s.Video:
			if !ok {
				return
			}
			s.broadcast(frame)
		}
	}
}

// broadcast 向所有连接发一帧二进制数据。
//
// 写失败的客户端会被直接移除：特斯拉车机在休眠或切页面时会断链，
// 不能让一个坏连接拖慢整条广播。
func (s *Server) broadcast(frame []byte) {
	s.mu.Lock()
	viewers := make([]*wsConn, 0, len(s.viewers))
	for conn := range s.viewers {
		viewers = append(viewers, conn)
	}
	s.mu.Unlock()

	for _, conn := range viewers {
		if err := conn.WriteMessage(opBinary, frame); err != nil {
			s.removeViewer(conn)
			_ = conn.Close()
		}
	}
}

func (s *Server) addViewer(conn *wsConn) {
	s.mu.Lock()
	s.viewers[conn] = struct{}{}
	s.mu.Unlock()
}

func (s *Server) removeViewer(conn *wsConn) {
	s.mu.Lock()
	delete(s.viewers, conn)
	s.mu.Unlock()
}

// ErrNoVideoSource 表示没有配置视频源。
var ErrNoVideoSource = errors.New("web: 未配置视频源")

// IndexPort 是默认监听端口，便于配置文件引用。
const IndexPort = 8080

// DefaultAddr 返回默认监听地址。
func DefaultAddr() string { return ":" + strconv.Itoa(IndexPort) }

const indexPage = `<!doctype html>
<meta charset="utf-8">
<title>CarPlay</title>
<style>
  body { margin:0; background:#000; color:#eee; font:14px system-ui; }
  #video { width:100vw; height:100vh; object-fit:contain; background:#000; }
  #hint { position:fixed; left:12px; top:12px; opacity:.6; }
</style>
<div id="hint">CarPlay 画面通道</div>
<canvas id="video"></canvas>
<script>
// 接口约定：
//   GET  /api/status  返回 {state, wireless, videoFormat, viewers}
//   WS   /ws          下行二进制帧 = 一个 H.264 访问单元；上行 JSON 触摸事件
//                     {"action":"down|move|up|cancel","x":0,"y":0,
//                      "viewWidth":800,"viewHeight":480}
const ws = new WebSocket(location.origin.replace(/^http/, 'ws') + '/ws');
ws.binaryType = 'arraybuffer';
ws.onmessage = (e) => { /* 把 e.data 交给 MSE 或 WebRTC 解码渲染 */ };

const canvas = document.getElementById('video');
function send(action, x, y) {
  ws.send(JSON.stringify({
    action, x, y,
    viewWidth: canvas.clientWidth, viewHeight: canvas.clientHeight,
  }));
}
canvas.addEventListener('pointerdown', (e) => send('down', e.offsetX, e.offsetY));
canvas.addEventListener('pointermove', (e) => send('move', e.offsetX, e.offsetY));
canvas.addEventListener('pointerup',   (e) => send('up',   e.offsetX, e.offsetY));
</script>
`
