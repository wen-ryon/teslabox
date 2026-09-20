package service

import (
	mediasnapshot "github.com/wen-ryon/teslabox/internal/snapshot"
	"github.com/wen-ryon/teslabox/internal/teslacam"
	"testing"
	"time"
)

func TestAutomaticRefreshDoesNotWaitForCalibration(t *testing.T) {
	now := time.Now()
	m := &Manager{lastSnapshotAt: now.Add(-time.Hour), mediaActiveUntil: now.Add(time.Hour), mediaLeases: make(map[string]int)}
	m.state.MediaSnapshot = &mediasnapshot.View{ID: "current"}
	m.state.Videos = []teslacam.Video{{ID: "pending"}}
	if !m.shouldRunAutomaticSnapshotLocked(now) {
		t.Fatal("pending calibration blocked new recordings")
	}
	m.calibratedSnapshot = "current"
	if !m.shouldRunAutomaticSnapshotLocked(now) {
		t.Fatal("completed calibration blocked refresh")
	}
	m.state.MediaSnapshot = &mediasnapshot.View{ID: "replacement"}
	if !m.shouldRunAutomaticSnapshotLocked(now) {
		t.Fatal("replacement calibration blocked refresh")
	}
}
