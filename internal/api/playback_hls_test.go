package api

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/wen-ryon/teslabox/internal/media"
	"github.com/wen-ryon/teslabox/internal/service"
	mediasnapshot "github.com/wen-ryon/teslabox/internal/snapshot"
	"github.com/wen-ryon/teslabox/internal/teslacam"
)

type hlsTestCompositor struct{ fail bool }

func (c hlsTestCompositor) Stream(ctx context.Context, request media.CompositeRequest, _ io.Writer) error {
	if c.fail {
		return errors.New("encoder failed")
	}
	files := map[string]string{
		"init.mp4": "init", "segment-00000.m4s": "0123456789", "segment-00001.m4s": "segment-two",
		"index.m3u8": "#EXTM3U\n#EXT-X-VERSION:7\n#EXT-X-TARGETDURATION:1\n#EXT-X-PLAYLIST-TYPE:EVENT\n#EXT-X-MAP:URI=\"init.mp4\"\n#EXTINF:1.000,\nsegment-00000.m4s\n#EXTINF:1.000,\nsegment-00001.m4s\n",
	}
	for _, name := range []string{"init.mp4", "segment-00000.m4s", "segment-00001.m4s", "index.m3u8"} {
		body := files[name]
		if err := os.WriteFile(filepath.Join(request.HLSDirectory, name), []byte(body), 0o600); err != nil {
			return err
		}
	}
	<-ctx.Done()
	return ctx.Err()
}

func TestPlaybackHLSBoundsAndIdleCleanup(t *testing.T) {
	t.Parallel()
	m := newPlaybackHLSManager()
	m.root = t.TempDir()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	var released atomic.Int32
	var sessions []*playbackHLSSession
	t.Cleanup(func() {
		for _, s := range sessions {
			s.cancel()
			<-s.removed
		}
	})
	for range 2 {
		s, err := m.start(t.Context(), hlsTestCompositor{}, media.CompositeRequest{}, func() { released.Add(1) }, logger, nil)
		if err != nil {
			t.Fatal(err)
		}
		sessions = append(sessions, s)
		if err := s.waitReady(t.Context()); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := m.start(t.Context(), hlsTestCompositor{}, media.CompositeRequest{}, func() { t.Error("rejected session acquired lease") }, logger, nil); err == nil {
		t.Fatal("session limit not enforced")
	}
	sessions[0].mu.Lock()
	sessions[0].lastAccess = time.Now().Add(-time.Minute)
	sessions[0].mu.Unlock()
	// 稀疏文件只模拟统计大小，不实际分配大量测试磁盘空间。
	if err := os.Truncate(filepath.Join(sessions[1].directory, "segment-00000.m4s"), 65<<20); err != nil {
		t.Fatal(err)
	}
	for _, s := range sessions {
		select {
		case <-s.removed:
		case <-time.After(5 * time.Second):
			t.Fatal("idle or oversized session was not reclaimed")
		}
	}
	if released.Load() != 2 {
		t.Fatalf("released=%d", released.Load())
	}
}

func TestPlaybackHLSFiniteAssetsAndCancellation(t *testing.T) {
	t.Parallel()
	m := newPlaybackHLSManager()
	m.root = t.TempDir()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	var released atomic.Int32
	s, err := m.start(t.Context(), hlsTestCompositor{}, media.CompositeRequest{}, func() { released.Add(1) }, logger, url.Values{"probe_id": {"0123456789abcdef0123456789abcdef"}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.cancel(); <-s.removed })
	if err := s.waitReady(t.Context()); err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	m.register(mux, logger)
	base := "/api/v1/playback-hls/" + s.id
	r := httptest.NewRequest(http.MethodGet, base+"/index.m3u8", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, r)
	if w.Code != 200 || !strings.Contains(w.Body.String(), "TIME-OFFSET=0") || w.Header().Get("Content-Type") != "application/vnd.apple.mpegurl" {
		t.Fatalf("playlist: %d %s", w.Code, w.Body.String())
	}
	if w.Header().Get("Content-Length") == "" {
		t.Fatal("playlist must have a finite length")
	}
	r = httptest.NewRequest(http.MethodGet, base+"/segment-00000.m4s", nil)
	r.Header.Set("Range", "bytes=0-1")
	w = httptest.NewRecorder()
	mux.ServeHTTP(w, r)
	if w.Code != 206 || w.Body.String() != "01" || w.Header().Get("Content-Range") != "bytes 0-1/10" {
		t.Fatalf("range: %d %s", w.Code, w.Body.String())
	}
	for _, name := range []string{"segment-00000.m4s.tmp", "secret", "..%2Finit.mp4"} {
		w = httptest.NewRecorder()
		mux.ServeHTTP(w, httptest.NewRequest(http.MethodGet, base+"/"+name, nil))
		if w.Code == 200 {
			t.Fatalf("unexpected asset served: %s", name)
		}
	}
	if released.Load() != 0 {
		t.Fatal("snapshot released while encoding")
	}
	w = httptest.NewRecorder()
	mux.ServeHTTP(w, httptest.NewRequest(http.MethodDelete, base, nil))
	select {
	case <-s.removed:
	case <-time.After(3 * time.Second):
		t.Fatal("session did not stop")
	}
	if released.Load() != 1 {
		t.Fatalf("snapshot released %d times", released.Load())
	}
	if _, err := os.Stat(s.directory); !os.IsNotExist(err) {
		t.Fatalf("temporary directory remains: %v", err)
	}
}

func TestPlaybackHLSStartupFailureReleasesLease(t *testing.T) {
	t.Parallel()
	m := newPlaybackHLSManager()
	m.root = t.TempDir()
	var released atomic.Int32
	s, err := m.start(t.Context(), hlsTestCompositor{fail: true}, media.CompositeRequest{}, func() { released.Add(1) }, slog.New(slog.NewTextHandler(io.Discard, nil)), nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.cancel(); <-s.removed })
	if err := s.waitReady(t.Context()); err == nil {
		t.Fatal("failed encoder reported ready")
	}
	<-s.done
	if released.Load() != 1 {
		t.Fatal("lease not released")
	}
}

func TestPlaybackHLSCreateSurvivesCreatingRequestAndDeleteStopsEncoder(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "front.mp4"), []byte("video"), 0o600); err != nil {
		t.Fatal(err)
	}
	videos := []teslacam.Video{{ID: "front", Camera: "front", Path: "front.mp4", RecordedAt: time.Now(), Duration: 60}}
	var acquired, released atomic.Int32
	controller := fakeController{state: service.Snapshot{MediaSnapshot: &mediasnapshot.View{ID: "snap", MountPath: root}, Videos: videos}, acquired: func() { acquired.Add(1) }, released: func() { released.Add(1) }}
	h := NewHandler(controller, slog.New(slog.NewTextHandler(io.Discard, nil)), WithMediaCompositor(hlsTestCompositor{}))
	group := groupVideos(videos, videoGroupTolerance)[0]
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	r := httptest.NewRequest(http.MethodPost, "/api/v1/video-groups/"+group.ID+"/composite/hls?layout=single&camera=front&quality=smooth", nil).WithContext(ctx)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != 201 {
		t.Fatalf("create: %d %s", w.Code, w.Body.String())
	}
	var body map[string]string
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodDelete, body["session_url"], nil))
	})
	cancel()
	w = httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest(http.MethodGet, body["url"], nil))
	if w.Code != 200 || acquired.Load() != 1 || released.Load() != 0 {
		t.Fatalf("detached session: %d acquired=%d released=%d", w.Code, acquired.Load(), released.Load())
	}
	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodDelete, body["session_url"], nil))
	deadline := time.Now().Add(3 * time.Second)
	for released.Load() == 0 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if released.Load() != 1 {
		t.Fatal("delete failed to release snapshot")
	}
}
