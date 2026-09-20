package api

import (
	"io"
	"log/slog"
	"net/http/httptest"
	"testing"

	"github.com/wen-ryon/teslabox/internal/teslacam"
)

func TestDurationPollDoesNotActivateMedia(t *testing.T) {
	controller := fakeController{activated: func() { t.Fatal("时长轮询触发了磁盘快照") }}
	handler := NewHandler(controller, slog.New(slog.NewTextHandler(io.Discard, nil)))
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, httptest.NewRequest("GET", "/api/v1/video-sessions?metadata_only=true&session_id=missing", nil))
	if response.Code != 200 {
		t.Fatalf("status = %d", response.Code)
	}
}

func TestUnavailableSessionStillReportsPendingDurations(t *testing.T) {
	session := videoSession{Moments: []videoGroup{{Videos: []teslacam.Video{
		{DurationState: "unavailable"}, {DurationState: "estimated"},
	}}}}
	refineVideoSessionDurations("", &session)
	if session.DurationState != "unavailable" || !session.DurationPending {
		t.Fatalf("state=%s pending=%v", session.DurationState, session.DurationPending)
	}
	session.Moments[0].Videos[1].DurationState = "verified"
	refineVideoSessionDurations("", &session)
	if session.DurationPending {
		t.Fatal("已完成仍标为等待")
	}
}
