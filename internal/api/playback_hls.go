package api

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/wen-ryon/teslabox/internal/media"
)

const hlsDirectoryPrefix = "teslabox-playback-hls-"

var hlsAssetName = regexp.MustCompile(`^(index\.m3u8|init\.mp4|segment-[0-9]{5}\.m4s)$`)

type playbackHLSSession struct {
	id, directory string
	cancel        context.CancelFunc
	done, removed chan struct{}
	mu            sync.Mutex
	lastAccess    time.Time
	err           error
	probe         url.Values
	files         sync.RWMutex
}

type playbackHLSManager struct {
	mu       sync.Mutex
	sessions map[string]*playbackHLSSession
	root     string
}

func newPlaybackHLSManager() *playbackHLSManager {
	return &playbackHLSManager{sessions: make(map[string]*playbackHLSSession), root: os.TempDir()}
}

func (m *playbackHLSManager) start(parent context.Context, compositor MediaCompositor, request media.CompositeRequest, release func(), logger *slog.Logger, probe url.Values) (*playbackHLSSession, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if len(m.sessions) >= 2 {
		return nil, errors.New("播放会话正在使用，请关闭其他压缩播放后重试")
	}
	// 只清理本功能创建、超过最长会话寿命的临时目录，处理服务异常退出的遗留文件。
	entries, _ := os.ReadDir(m.root)
	for _, entry := range entries {
		if !entry.IsDir() || !strings.HasPrefix(entry.Name(), hlsDirectoryPrefix) {
			continue
		}
		if info, err := entry.Info(); err == nil && time.Since(info.ModTime()) > 10*time.Minute {
			_ = os.RemoveAll(filepath.Join(m.root, entry.Name()))
		}
	}
	var token [16]byte
	if _, err := rand.Read(token[:]); err != nil {
		return nil, err
	}
	directory, err := os.MkdirTemp(m.root, hlsDirectoryPrefix)
	if err != nil {
		return nil, err
	}
	// HLS 的多个 HTTP 请求共享有界会话，创建请求返回后转码仍需继续。
	ctx, cancel := context.WithTimeout(context.WithoutCancel(parent), 5*time.Minute)
	s := &playbackHLSSession{id: hex.EncodeToString(token[:]), directory: directory, cancel: cancel,
		done: make(chan struct{}), removed: make(chan struct{}), lastAccess: time.Now(), probe: make(url.Values)}
	if validPlaybackProbeID(probe.Get("probe_id")) {
		for _, key := range []string{"probe_id", "probe_case", "probe_track"} {
			s.probe.Set(key, normalizeDiagnosticText(probe.Get(key), 64))
		}
	}
	m.sessions[s.id] = s
	request.HLSDirectory = directory
	go func() {
		defer close(s.done)
		defer release()
		encodeCtx, stop := context.WithTimeout(ctx, 90*time.Second)
		defer stop()
		err := compositor.Stream(encodeCtx, request, io.Discard)
		s.mu.Lock()
		s.err = err
		s.mu.Unlock()
		logger.Info("HLS 转码结束", "hls_session", s.id, "error", err)
	}()
	go func() {
		defer close(s.removed)
		ticker := time.NewTicker(2 * time.Second)
		defer ticker.Stop()
		defer cancel()
		for ctx.Err() == nil {
			select {
			case <-ctx.Done():
			case <-ticker.C:
				s.mu.Lock()
				idle := time.Since(s.lastAccess) > 45*time.Second
				s.mu.Unlock()
				var bytes int64
				files, _ := os.ReadDir(directory)
				for _, file := range files {
					if info, err := file.Info(); err == nil {
						bytes += info.Size()
					}
				}
				if idle || bytes > 64<<20 {
					cancel()
				}
			}
		}
		// 等待 FFmpeg 退出及快照引用释放，再清理目录，避免生成进程重建已删文件。
		<-s.done
		s.files.Lock()
		if err := os.RemoveAll(directory); err != nil {
			logger.Warn("HLS 临时分片清理失败", "error", err)
		}
		s.files.Unlock()
		m.mu.Lock()
		delete(m.sessions, s.id)
		m.mu.Unlock()
	}()
	return s, nil
}

func (s *playbackHLSSession) waitReady(ctx context.Context) error {
	timer := time.NewTimer(15 * time.Second)
	defer timer.Stop()
	ticker := time.NewTicker(50 * time.Millisecond)
	defer ticker.Stop()
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		s.mu.Lock()
		failure := s.err
		s.mu.Unlock()
		if failure != nil {
			return failure
		}
		body, _ := os.ReadFile(filepath.Join(s.directory, "index.m3u8"))
		if strings.Count(string(body), "#EXTINF:") >= 2 || (strings.Contains(string(body), "#EXT-X-ENDLIST") && strings.Contains(string(body), "#EXTINF:")) {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-timer.C:
			return errors.New("HLS 首批分片生成超时")
		case <-s.done:
			body, _ = os.ReadFile(filepath.Join(s.directory, "index.m3u8"))
			s.mu.Lock()
			err := s.err
			s.mu.Unlock()
			if err == nil && strings.Contains(string(body), "#EXT-X-ENDLIST") && strings.Contains(string(body), "#EXTINF:") {
				return nil
			}
			if err != nil {
				return err
			}
			return errors.New("HLS 分片生成失败")
		case <-ticker.C:
		}
	}
}

func (m *playbackHLSManager) lookup(id string) *playbackHLSSession {
	m.mu.Lock()
	s := m.sessions[id]
	m.mu.Unlock()
	if s != nil {
		s.mu.Lock()
		s.lastAccess = time.Now()
		s.mu.Unlock()
	}
	return s
}

func (m *playbackHLSManager) register(mux *http.ServeMux, logger *slog.Logger) {
	mux.HandleFunc("DELETE /api/v1/playback-hls/{session}", func(w http.ResponseWriter, r *http.Request) {
		if s := m.lookup(r.PathValue("session")); s != nil {
			s.cancel()
			// 正常切换先释放旧转码槽，避免新会话与即将退出的进程竞争。
			select {
			case <-s.removed:
			case <-r.Context().Done():
			case <-time.After(2 * time.Second):
			}
		}
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("POST /api/v1/playback-hls/{session}/keepalive", func(w http.ResponseWriter, r *http.Request) {
		if m.lookup(r.PathValue("session")) == nil {
			http.NotFound(w, r)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("GET /api/v1/playback-hls/{session}/{asset}", func(w http.ResponseWriter, r *http.Request) {
		s := m.lookup(r.PathValue("session"))
		asset := r.PathValue("asset")
		if s == nil || !hlsAssetName.MatchString(asset) {
			http.NotFound(w, r)
			return
		}
		if len(s.probe) > 0 {
			r = r.Clone(r.Context())
			r.URL.RawQuery = s.probe.Encode()
		}
		w, finish := beginPlaybackProbe(w, r, logger, "transcoded")
		defer finish()
		s.mu.Lock()
		failed := s.err != nil
		s.mu.Unlock()
		if failed {
			writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "HLS 转码已中断，请重新播放"})
			return
		}
		s.files.RLock()
		file, err := os.Open(filepath.Join(s.directory, asset))
		s.files.RUnlock()
		if err != nil {
			http.NotFound(w, r)
			return
		}
		defer file.Close()
		info, err := file.Stat()
		if err != nil {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Cache-Control", "no-store")
		if asset == "index.m3u8" {
			body, err := io.ReadAll(io.LimitReader(file, 64<<10))
			if err != nil {
				http.Error(w, "播放列表读取失败", http.StatusServiceUnavailable)
				return
			}
			// 录像应从用户选择的位置开始，不能被原生播放器当作直播自动追到最新分片。
			body = bytes.Replace(body, []byte("#EXTM3U\n"), []byte("#EXTM3U\n#EXT-X-START:TIME-OFFSET=0,PRECISE=YES\n"), 1)
			w.Header().Set("Content-Type", "application/vnd.apple.mpegurl")
			http.ServeContent(w, r, asset, info.ModTime(), bytes.NewReader(body))
			return
		}
		w.Header().Set("Content-Type", "video/mp4")
		http.ServeContent(w, r, asset, info.ModTime(), file)
	})
}
