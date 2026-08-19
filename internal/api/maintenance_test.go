package api

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/wen-ryon/teslabox/internal/maintenanceguard"
	"github.com/wen-ryon/teslabox/internal/maintenancemode"
	"github.com/wen-ryon/teslabox/internal/mediaassets"
	"github.com/wen-ryon/teslabox/internal/service"
	mediasnapshot "github.com/wen-ryon/teslabox/internal/snapshot"
	"github.com/wen-ryon/teslabox/internal/teslacam"
	"github.com/wen-ryon/teslabox/internal/updater"
)

type fakeMaintenanceAutomation struct{ settings maintenancemode.Settings }

func (a *fakeMaintenanceAutomation) Settings() maintenancemode.Settings { return a.settings }

func (a *fakeMaintenanceAutomation) SetAutoRecovery(enabled bool) (maintenancemode.Settings, error) {
	a.settings.AutoRecovery = enabled
	return a.settings, nil
}

type fakeMediaAssetsAdmin struct {
	applyCalls              int
	recordingDeleteCalls    int
	recordingDeletePaths    []string
	recordingDeleteReleased func() bool
	reclaimCalls            int
	reclaimResult           mediaassets.SpaceReclaimResult
	repairCalls             int
	cancelRepairCalls       int
	repairResult            mediaassets.SpaceReclaimResult
}

func (a *fakeMediaAssetsAdmin) Status(context.Context) (mediaassets.Status, error) {
	return mediaassets.Status{}, nil
}

func (a *fakeMediaAssetsAdmin) UploadStream(context.Context, mediaassets.Category, func(func(mediaassets.UploadFile) error) error) ([]mediaassets.Asset, error) {
	return nil, nil
}

func (a *fakeMediaAssetsAdmin) Delete(context.Context, string) (mediaassets.Status, error) {
	return mediaassets.Status{}, nil
}

func (a *fakeMediaAssetsAdmin) DeleteExternal(context.Context, mediaassets.Asset) (mediaassets.Status, error) {
	return mediaassets.Status{}, nil
}

func (a *fakeMediaAssetsAdmin) DeleteRecordingsAndReclaim(_ context.Context, request mediaassets.RecordingDeleteRequest) (mediaassets.RecordingDeleteResult, error) {
	a.recordingDeleteCalls++
	a.recordingDeletePaths = append([]string(nil), request.Paths...)
	if a.recordingDeleteReleased != nil && !a.recordingDeleteReleased() {
		return mediaassets.RecordingDeleteResult{}, errors.New("媒体快照租约尚未释放")
	}
	return mediaassets.RecordingDeleteResult{
		RequestedFiles: len(request.Paths),
		DeletedFiles:   len(request.Paths),
		DeletedBytes:   11,
		DeletedEvents:  1,
		SpaceReclaim:   a.reclaimResult,
	}, nil
}

func (a *fakeMediaAssetsAdmin) ReclaimSpace(context.Context) (mediaassets.SpaceReclaimResult, error) {
	a.reclaimCalls++
	return a.reclaimResult, nil
}

func (a *fakeMediaAssetsAdmin) RepairSnapshotReserve(context.Context) (mediaassets.SpaceReclaimResult, error) {
	a.repairCalls++
	return a.repairResult, nil
}

func (a *fakeMediaAssetsAdmin) CancelSnapshotReserveRepair(context.Context) (mediaassets.SpaceReclaimResult, error) {
	a.cancelRepairCalls++
	return a.repairResult, nil
}

func (a *fakeMediaAssetsAdmin) SnapshotReserveRepairStatus() (mediaassets.SpaceReclaimResult, error) {
	return a.repairResult, nil
}

func (a *fakeMediaAssetsAdmin) Apply(context.Context) (mediaassets.Status, error) {
	a.applyCalls++
	return mediaassets.Status{}, nil
}

func (a *fakeMediaAssetsAdmin) OpenPreview(context.Context, string) (*os.File, mediaassets.Asset, error) {
	return nil, mediaassets.Asset{}, mediaassets.ErrPreviewUnavailable
}

func TestMaintenanceSettingRequiresRiskConfirmation(t *testing.T) {
	automation := &fakeMaintenanceAutomation{}
	handler := NewHandler(fakeController{}, slog.New(slog.NewTextHandler(io.Discard, nil)), WithMaintenanceAutomation(automation))

	missing := httptest.NewRecorder()
	handler.ServeHTTP(missing, httptest.NewRequest(http.MethodPost, "/api/v1/maintenance", strings.NewReader(`{"auto_recovery":true}`)))
	if missing.Code != http.StatusBadRequest || automation.Settings().AutoRecovery {
		t.Fatalf("unconfirmed setting = %d, %#v", missing.Code, automation.Settings())
	}

	confirmed := httptest.NewRecorder()
	handler.ServeHTTP(confirmed, httptest.NewRequest(http.MethodPost, "/api/v1/maintenance", strings.NewReader(`{"auto_recovery":true,"confirm_risk":true}`)))
	if confirmed.Code != http.StatusOK || !automation.Settings().AutoRecovery {
		t.Fatalf("confirmed setting = %d, %#v", confirmed.Code, automation.Settings())
	}
}

func TestAutomaticMaintenanceAppliesMediaAndSchedulesReboot(t *testing.T) {
	previousDelay := automaticMaintenanceDisconnectDelay
	automaticMaintenanceDisconnectDelay = 0
	t.Cleanup(func() { automaticMaintenanceDisconnectDelay = previousDelay })

	automation := &fakeMaintenanceAutomation{settings: maintenancemode.Settings{AutoRecovery: true}}
	assets := &fakeMediaAssetsAdmin{}
	gadget := &fakeGadgetAdmin{}
	power := &fakeDeviceAdmin{}
	handler := NewHandler(
		fakeController{}, slog.New(slog.NewTextHandler(io.Discard, nil)),
		WithMediaAssetsAdmin(assets), WithGadgetAdmin(gadget), WithDeviceAdmin(power), WithMaintenanceAutomation(automation),
	)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, httptest.NewRequest(http.MethodPost, "/api/v1/media-assets/apply", strings.NewReader(`{}`)))
	if response.Code != http.StatusOK || assets.applyCalls != 1 || gadget.pauseCalls != 1 || gadget.resumeCalls != 1 || power.powerAction != "reboot" {
		t.Fatalf("apply = %d, calls = apply:%d pause:%d resume:%d power:%q", response.Code, assets.applyCalls, gadget.pauseCalls, gadget.resumeCalls, power.powerAction)
	}
}

func TestRecordingDeleteRequiresConfirmationAndUsesSnapshotPaths(t *testing.T) {
	base := time.Date(2026, 7, 26, 3, 30, 0, 0, time.Local)
	var released bool
	controller := fakeController{
		state: service.Snapshot{
			MediaSnapshot: &mediasnapshot.View{ID: "snapshot"},
			Videos: []teslacam.Video{
				{
					ID: "front", Path: "TeslaCam/SavedClips/2026-07-26_03-30-00/2026-07-26_03-30-00-front.mp4",
					ClipType: "saved", Event: "2026-07-26_03-30-00", Camera: "front", RecordedAt: base,
				},
				{
					ID: "back", Path: "TeslaCam/SavedClips/2026-07-26_03-30-00/2026-07-26_03-30-00-back.mp4",
					ClipType: "saved", Event: "2026-07-26_03-30-00", Camera: "back", RecordedAt: base,
				},
			},
		},
		released: func() { released = true },
	}
	sessionID := groupVideoSessions(groupVideos(controller.state.Videos, videoGroupTolerance), sessionContinuityGap)[0].ID
	assets := &fakeMediaAssetsAdmin{recordingDeleteReleased: func() bool { return released }}
	handler := NewHandler(
		controller, slog.New(slog.NewTextHandler(io.Discard, nil)),
		WithMediaAssetsAdmin(assets), WithMaintenanceAutomation(&fakeMaintenanceAutomation{}),
	)

	missing := httptest.NewRecorder()
	missingRequest := httptest.NewRequest(http.MethodPost, "/api/v1/video-sessions/"+sessionID+"/delete", strings.NewReader(`{}`))
	handler.ServeHTTP(missing, missingRequest)
	if missing.Code != http.StatusBadRequest || assets.recordingDeleteCalls != 0 {
		t.Fatalf("unconfirmed delete = %d, calls = %d", missing.Code, assets.recordingDeleteCalls)
	}

	confirmed := httptest.NewRecorder()
	confirmedRequest := httptest.NewRequest(http.MethodPost, "/api/v1/video-sessions/"+sessionID+"/delete", strings.NewReader(`{"confirm_otg_disconnected":true}`))
	handler.ServeHTTP(confirmed, confirmedRequest)
	if confirmed.Code != http.StatusOK || assets.recordingDeleteCalls != 1 || !released {
		t.Fatalf("confirmed delete = %d, calls = %d, released = %t, body = %s", confirmed.Code, assets.recordingDeleteCalls, released, confirmed.Body.String())
	}
	expectedPaths := []string{controller.state.Videos[0].Path, controller.state.Videos[1].Path}
	slices.Sort(expectedPaths)
	slices.Sort(assets.recordingDeletePaths)
	if !slices.Equal(assets.recordingDeletePaths, expectedPaths) {
		t.Fatalf("recording paths = %#v", assets.recordingDeletePaths)
	}
}

func TestAutomaticRecordingDeleteSchedulesWarmReboot(t *testing.T) {
	previousDelay := automaticMaintenanceDisconnectDelay
	automaticMaintenanceDisconnectDelay = 0
	t.Cleanup(func() { automaticMaintenanceDisconnectDelay = previousDelay })

	video := teslacam.Video{
		ID: "recent", Path: "TeslaCam/RecentClips/2026-07-26_03-30-00-front.mp4",
		ClipType: "recent", Camera: "front", RecordedAt: time.Date(2026, 7, 26, 3, 30, 0, 0, time.Local),
	}
	controller := fakeController{state: service.Snapshot{
		MediaSnapshot: &mediasnapshot.View{ID: "snapshot"},
		Videos:        []teslacam.Video{video},
	}}
	assets := &fakeMediaAssetsAdmin{}
	gadget := &fakeGadgetAdmin{}
	power := &fakeDeviceAdmin{}
	automation := &fakeMaintenanceAutomation{settings: maintenancemode.Settings{AutoRecovery: true}}
	handler := NewHandler(
		controller, slog.New(slog.NewTextHandler(io.Discard, nil)),
		WithMediaAssetsAdmin(assets), WithGadgetAdmin(gadget), WithDeviceAdmin(power), WithMaintenanceAutomation(automation),
	)
	sessionID := groupVideoSessions(groupVideos(controller.state.Videos, videoGroupTolerance), sessionContinuityGap)[0].ID
	response := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/api/v1/video-sessions/"+sessionID+"/delete", strings.NewReader(`{}`))
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusOK || assets.recordingDeleteCalls != 1 ||
		gadget.pauseCalls != 1 || gadget.resumeCalls != 1 || power.powerAction != "reboot" {
		t.Fatalf("automatic delete = %d, calls = delete:%d pause:%d resume:%d power:%q body=%s",
			response.Code, assets.recordingDeleteCalls, gadget.pauseCalls, gadget.resumeCalls, power.powerAction, response.Body.String())
	}
}

func TestBatchRecordingDeleteUsesOneMaintenanceOperationAndReclaimsSpace(t *testing.T) {
	base := time.Date(2026, 7, 26, 4, 0, 0, 0, time.Local)
	var released bool
	controller := fakeController{
		state: service.Snapshot{
			MediaSnapshot: &mediasnapshot.View{ID: "snapshot"},
			Videos: []teslacam.Video{
				{
					ID: "saved-front", Path: "TeslaCam/SavedClips/2026-07-26_04-00-00/2026-07-26_04-00-00-front.mp4",
					ClipType: "saved", Event: "2026-07-26_04-00-00", Camera: "front", RecordedAt: base,
				},
				{
					ID: "sentry-front", Path: "TeslaCam/SentryClips/2026-07-26_04-05-00/2026-07-26_04-05-00-front.mp4",
					ClipType: "sentry", Event: "2026-07-26_04-05-00", Camera: "front", RecordedAt: base.Add(5 * time.Minute),
				},
			},
		},
		released: func() { released = true },
	}
	sessions := groupVideoSessions(groupVideos(controller.state.Videos, videoGroupTolerance), sessionContinuityGap)
	if len(sessions) != 2 {
		t.Fatalf("sessions = %d, want 2", len(sessions))
	}
	assets := &fakeMediaAssetsAdmin{
		recordingDeleteReleased: func() bool { return released },
		reclaimResult:           mediaassets.SpaceReclaimResult{ReclaimedBytes: 2048},
	}
	handler := NewHandler(
		controller, slog.New(slog.NewTextHandler(io.Discard, nil)),
		WithMediaAssetsAdmin(assets), WithMaintenanceAutomation(&fakeMaintenanceAutomation{}),
	)
	body := `{"session_ids":["` + sessions[0].ID + `","` + sessions[1].ID + `"],"confirm_otg_disconnected":true}`
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, httptest.NewRequest(http.MethodPost, "/api/v1/video-sessions/delete", strings.NewReader(body)))

	if response.Code != http.StatusOK || assets.recordingDeleteCalls != 1 || !released {
		t.Fatalf("batch delete = %d, calls = %d, released = %t, body = %s",
			response.Code, assets.recordingDeleteCalls, released, response.Body.String())
	}
	if len(assets.recordingDeletePaths) != 2 || !strings.Contains(response.Body.String(), `"reclaimed_bytes":2048`) {
		t.Fatalf("batch paths = %#v, body = %s", assets.recordingDeletePaths, response.Body.String())
	}
}

func TestSpaceReclaimRequiresConfirmationAndSupportsAutomaticRecovery(t *testing.T) {
	previousDelay := automaticMaintenanceDisconnectDelay
	automaticMaintenanceDisconnectDelay = 0
	t.Cleanup(func() { automaticMaintenanceDisconnectDelay = previousDelay })

	manualAssets := &fakeMediaAssetsAdmin{}
	manualHandler := NewHandler(
		fakeController{}, slog.New(slog.NewTextHandler(io.Discard, nil)),
		WithMediaAssetsAdmin(manualAssets), WithMaintenanceAutomation(&fakeMaintenanceAutomation{}),
	)
	missing := httptest.NewRecorder()
	manualHandler.ServeHTTP(missing, httptest.NewRequest(http.MethodPost, "/api/v1/storage/reclaim", strings.NewReader(`{}`)))
	if missing.Code != http.StatusBadRequest || manualAssets.reclaimCalls != 0 {
		t.Fatalf("unconfirmed reclaim = %d, calls = %d", missing.Code, manualAssets.reclaimCalls)
	}

	assets := &fakeMediaAssetsAdmin{reclaimResult: mediaassets.SpaceReclaimResult{CandidateBytes: 4096, ReclaimedBytes: 2048}}
	gadget := &fakeGadgetAdmin{}
	power := &fakeDeviceAdmin{}
	handler := NewHandler(
		fakeController{}, slog.New(slog.NewTextHandler(io.Discard, nil)),
		WithMediaAssetsAdmin(assets), WithGadgetAdmin(gadget), WithDeviceAdmin(power),
		WithMaintenanceAutomation(&fakeMaintenanceAutomation{settings: maintenancemode.Settings{AutoRecovery: true}}),
	)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, httptest.NewRequest(http.MethodPost, "/api/v1/storage/reclaim", strings.NewReader(`{}`)))
	if response.Code != http.StatusOK || assets.reclaimCalls != 1 ||
		gadget.pauseCalls != 1 || gadget.resumeCalls != 1 || power.powerAction != "reboot" {
		t.Fatalf("automatic reclaim = %d, calls = reclaim:%d pause:%d resume:%d power:%q body=%s",
			response.Code, assets.reclaimCalls, gadget.pauseCalls, gadget.resumeCalls, power.powerAction, response.Body.String())
	}
}

func TestSnapshotReserveRepairOnlyRecoversGadgetWhenRuntimeSafe(t *testing.T) {
	previousDelay := automaticMaintenanceDisconnectDelay
	automaticMaintenanceDisconnectDelay = 0
	t.Cleanup(func() { automaticMaintenanceDisconnectDelay = previousDelay })
	tests := []struct {
		name       string
		ready      bool
		wantReboot bool
	}{
		{name: "safe repair schedules recovery", ready: true, wantReboot: true},
		{name: "unsafe repair keeps gadget paused"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			assets := &fakeMediaAssetsAdmin{repairResult: mediaassets.SpaceReclaimResult{RuntimeReady: test.ready, State: maintenanceguard.StateCompleted}}
			gadget := &fakeGadgetAdmin{}
			power := &fakeDeviceAdmin{}
			handler := NewHandler(
				fakeController{}, slog.New(slog.NewTextHandler(io.Discard, nil)),
				WithMediaAssetsAdmin(assets), WithGadgetAdmin(gadget), WithDeviceAdmin(power),
				WithMaintenanceAutomation(&fakeMaintenanceAutomation{settings: maintenancemode.Settings{AutoRecovery: true}}),
			)
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, httptest.NewRequest(http.MethodPost, "/api/v1/storage/snapshot-reserve/repair", strings.NewReader(`{}`)))
			if response.Code != http.StatusOK || assets.repairCalls != 1 {
				t.Fatalf("repair = %d, calls = %d, body = %s", response.Code, assets.repairCalls, response.Body.String())
			}
			if (power.powerAction == "reboot") != test.wantReboot {
				t.Fatalf("power action = %q, want reboot = %t", power.powerAction, test.wantReboot)
			}
		})
	}
}

func TestCancelSnapshotReserveRepairRequiresPhysicalDisconnect(t *testing.T) {
	assets := &fakeMediaAssetsAdmin{repairResult: mediaassets.SpaceReclaimResult{RuntimeReady: false, State: maintenanceguard.StateCancelledUnsafe}}
	handler := NewHandler(fakeController{}, slog.New(slog.NewTextHandler(io.Discard, nil)), WithMediaAssetsAdmin(assets))

	missing := httptest.NewRecorder()
	handler.ServeHTTP(missing, httptest.NewRequest(http.MethodPost, "/api/v1/storage/snapshot-reserve/cancel", strings.NewReader(`{}`)))
	if missing.Code != http.StatusBadRequest || assets.cancelRepairCalls != 0 {
		t.Fatalf("unconfirmed cancel = %d, calls = %d", missing.Code, assets.cancelRepairCalls)
	}

	confirmed := httptest.NewRecorder()
	handler.ServeHTTP(confirmed, httptest.NewRequest(http.MethodPost, "/api/v1/storage/snapshot-reserve/cancel", strings.NewReader(`{"confirm_otg_disconnected":true}`)))
	if confirmed.Code != http.StatusOK || assets.cancelRepairCalls != 1 || !strings.Contains(confirmed.Body.String(), "cancelled_unsafe") {
		t.Fatalf("confirmed cancel = %d, calls = %d, body = %s", confirmed.Code, assets.cancelRepairCalls, confirmed.Body.String())
	}
}

func TestAutomaticMaintenanceRecoverySurvivesExpiredOperationContext(t *testing.T) {
	gadget := &fakeGadgetAdmin{}
	power := &fakeDeviceAdmin{}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if err := finishAutomaticMaintenance(ctx, gadget, power); err != nil {
		t.Fatalf("finishAutomaticMaintenance() error = %v", err)
	}
	if gadget.resumeCalls != 1 || gadget.resumeContextErr != nil || power.powerAction != "reboot" {
		t.Fatalf("recovery = resume:%d context:%v power:%q", gadget.resumeCalls, gadget.resumeContextErr, power.powerAction)
	}
}

func TestRecordingSessionPathsRejectsMultipleEventDirectories(t *testing.T) {
	session := videoSession{
		ClipType: "sentry",
		Moments: []videoGroup{
			{Videos: []teslacam.Video{{Path: "TeslaCam/SentryClips/event-a/2026-07-26_03-30-00-front.mp4"}}},
			{Videos: []teslacam.Video{{Path: "TeslaCam/SentryClips/event-b/2026-07-26_03-31-00-front.mp4"}}},
		},
	}
	if _, err := recordingSessionPaths(session); err == nil {
		t.Fatal("multiple event directories were accepted")
	}
}

func TestAutomaticMaintenanceInstallsUpdateAndPreparesWarmReboot(t *testing.T) {
	previousDelay := automaticMaintenanceDisconnectDelay
	automaticMaintenanceDisconnectDelay = 0
	t.Cleanup(func() { automaticMaintenanceDisconnectDelay = previousDelay })

	automation := &fakeMaintenanceAutomation{settings: maintenancemode.Settings{UpdateAutoRecovery: true}}
	update := &fakeUpdateAdmin{status: updater.Status{Configured: true, State: updater.StateDownloaded}}
	gadget := &fakeGadgetAdmin{}
	power := &fakeDeviceAdmin{}
	handler := NewHandler(
		fakeController{}, slog.New(slog.NewTextHandler(io.Discard, nil)),
		WithUpdateAdmin(update), WithGadgetAdmin(gadget), WithDeviceAdmin(power), WithMaintenanceAutomation(automation),
	)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, httptest.NewRequest(http.MethodPost, "/api/v1/update/install", nil))
	if response.Code != http.StatusAccepted || gadget.pauseCalls != 1 || gadget.resumeCalls != 1 || power.powerAction != "" {
		t.Fatalf("install = %d, pause:%d resume:%d power:%q", response.Code, gadget.pauseCalls, gadget.resumeCalls, power.powerAction)
	}
}
