package api

import (
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/wen-ryon/teslabox/internal/teslacam"
)

func TestArchivePaginationBeyondThousandEvents(t *testing.T) {
	videos := largeIndex(66066)
	seen := make(map[string]bool)
	for offset := 0; offset < 1001; offset += 50 {
		w := httptest.NewRecorder()
		r := httptest.NewRequest("GET", fmt.Sprintf("/api/v1/video-sessions?clip_type=archive&limit=50&offset=%d", offset), nil)
		writeVideoSessionList(w, r, videos, "snapshot")
		var result videoSessionListResponse
		if err := json.Unmarshal(w.Body.Bytes(), &result); err != nil {
			t.Fatal(err)
		}
		if result.Total != 1001 || result.SnapshotID != "snapshot" {
			t.Fatalf("total=%d", result.Total)
		}
		for _, item := range result.Items {
			if seen[item.ID] {
				t.Fatal("duplicate event")
			}
			seen[item.ID] = true
		}
	}
	if len(seen) != 1001 {
		t.Fatalf("loaded=%d", len(seen))
	}
}

func largeIndex(count int) []teslacam.Video {
	videos := make([]teslacam.Video, count)
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	for i := range videos {
		videos[i] = teslacam.Video{ID: fmt.Sprint(i), ClipType: "saved", Event: fmt.Sprint(i / 66), Camera: []string{"front", "back", "left_repeater", "right_repeater", "left_pillar", "right_pillar"}[i%6], RecordedAt: base.Add(time.Duration(i/6) * time.Minute), Duration: 60}
	}
	return videos
}

func TestFullRecentListPreserves24HourSession(t *testing.T) {
	videos := largeIndex(8640)
	for i := range videos {
		videos[i].ClipType = "recent"
		videos[i].Event = ""
	}
	index := &sessionIndex{}
	w := httptest.NewRecorder()
	r := httptest.NewRequest("GET", "/api/v1/video-sessions?clip_type=recent&limit=1000", nil)
	writeVideoSessionPage(w, r, index.get("snapshot", videos), "snapshot")
	var response videoSessionListResponse
	if err := json.Unmarshal(w.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if response.Total != 1 || response.Items[0].SegmentCount != 1440 || response.Items[0].RecentWindow {
		t.Fatal("recent session was split")
	}
	if index.recent != nil {
		t.Fatal("original interface unnecessarily built navigation windows")
	}
	if len(index.get("snapshot", videos)[0].Moments) != 1440 {
		t.Fatal("lost full timeline")
	}
}

func BenchmarkFullRecentGroupingReuse(b *testing.B) {
	videos := largeIndex(8640)
	for i := range videos {
		videos[i].ClipType = "recent"
		videos[i].Event = ""
	}
	index := &sessionIndex{}
	index.get("snapshot", videos)
	b.Run("regroup", func(b *testing.B) {
		for i := 0; i < b.N; i++ {
			groupVideoSessions(groupVideos(videos, videoGroupTolerance), sessionContinuityGap)
		}
	})
	b.Run("snapshot-cache", func(b *testing.B) {
		for i := 0; i < b.N; i++ {
			index.get("snapshot", videos)
		}
	})
}

func BenchmarkLargeRecordingIndex(b *testing.B) {
	for _, count := range []int{1000, 10000, 30000} {
		b.Run(fmt.Sprint(count), func(b *testing.B) {
			videos := largeIndex(count)
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				groupVideoSessions(groupVideos(videos, videoGroupTolerance), sessionContinuityGap)
			}
		})
	}
}

func BenchmarkCachedRecordingPage(b *testing.B) {
	videos := largeIndex(30000)
	index := &sessionIndex{}
	sessions := index.get("snapshot", videos)
	r := httptest.NewRequest("GET", "/api/v1/video-sessions?clip_type=archive&limit=50", nil)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		w := httptest.NewRecorder()
		writeVideoSessionPage(w, r, sessions, "snapshot")
	}
}

func TestSessionIndexIsolatedFromPageRefinement(t *testing.T) {
	index := &sessionIndex{}
	videos := largeIndex(66)
	sessions := index.get("first", videos)
	copy := cloneSessionForDuration(sessions[0])
	copy.Moments[0].Videos[0].Duration = 2
	copy.Moments[0].TimelineStart = 123
	if sessions[0].Moments[0].Videos[0].Duration != 60 || sessions[0].Moments[0].TimelineStart != 0 {
		t.Fatal("page mutated cached index")
	}
	if len(index.get("empty", nil)) != 0 {
		t.Fatal("old snapshot retained deleted files")
	}
}
