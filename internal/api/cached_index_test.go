package api

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http/httptest"
	"testing"

	"github.com/wen-ryon/teslabox/internal/service"
	"github.com/wen-ryon/teslabox/internal/teslacam"
)

type cachedController struct {
	fakeController
	cached    service.CachedMediaIndex
	requested bool
}

func (c *cachedController) CachedMedia(request bool) service.CachedMediaIndex {
	c.requested = request
	return c.cached
}

func TestCachedIndexIsPreviewOnly(t *testing.T) {
	for _, empty := range []bool{false, true} {
		name := "nonempty"
		if empty {
			name = "empty"
		}
		t.Run(name, func(t *testing.T) {
			c := &cachedController{cached: service.CachedMediaIndex{ID: "cache:disk"}}
			c.activated = func() { t.Fatal("历史列表被同步扫描阻塞") }
			if !empty {
				c.cached.Videos = []teslacam.Video{{ID: "one", Path: "TeslaCam/RecentClips/one.mp4", ClipType: "recent", Camera: "front", Duration: 17, DurationState: "verified"}}
			}
			handler := NewHandler(c, slog.New(slog.NewTextHandler(io.Discard, nil)))
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, httptest.NewRequest("GET", "/api/v1/video-sessions", nil))
			var payload videoSessionListResponse
			if err := json.Unmarshal(response.Body.Bytes(), &payload); err != nil {
				t.Fatal(err)
			}
			if !payload.Cached || !c.requested {
				t.Fatalf("缓存空列表也应核对: %+v", payload)
			}
			for _, item := range payload.Items {
				if !item.Cached || item.ThumbnailURL != "" || item.SnapshotID != "cache:disk" {
					t.Fatalf("unsafe preview: %+v", item)
				}
			}
		})
	}
}
