package api

import (
	"github.com/wen-ryon/teslabox/internal/media"
	"github.com/wen-ryon/teslabox/internal/teslacam"
	"testing"
)

func TestExportTimelineDependencies(t *testing.T) {
	for _, tc := range []struct {
		name         string
		badCamera    string
		start, badAt float64
		want         bool
	}{
		{"无关尾段", "front", 0, 60, true},
		{"无关视角", "back", 0, 0, true},
		{"所选画面损坏", "front", 0, 0, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			session := videoSession{Moments: []videoGroup{
				{TimelineStart: 0, Duration: 60, Videos: []teslacam.Video{{Camera: "front", DurationState: "verified"}, {Camera: "back", DurationState: "verified"}}},
				{TimelineStart: 60, Duration: 60, Videos: []teslacam.Video{{Camera: "front", DurationState: "verified"}}},
			}}
			for i := range session.Moments {
				if session.Moments[i].TimelineStart == tc.badAt {
					for j := range session.Moments[i].Videos {
						if session.Moments[i].Videos[j].Camera == tc.badCamera {
							session.Moments[i].Videos[j].DurationState = "unavailable"
						}
					}
				}
			}
			got := exportTimelineVerified(session, media.ExportRequest{Layout: "single", Cameras: []string{"front"}, StartSeconds: tc.start, DurationSeconds: 10})
			if got != tc.want {
				t.Fatalf("got %v want %v", got, tc.want)
			}
			if tc.badAt == 60 {
				if exportTimelineVerified(session, media.ExportRequest{Layout: "single", Cameras: []string{"front"}, StartSeconds: 70, DurationSeconds: 10}) {
					t.Fatal("accepted unknown timeline")
				}
			}
		})
	}
}
