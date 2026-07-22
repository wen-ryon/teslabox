package api

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/wen-ryon/teslabox/internal/service"
)

type fakeGadgetAdmin struct {
	paused      bool
	pauseCalls  int
	resumeCalls int
}

func (a *fakeGadgetAdmin) PauseGadget(context.Context) (service.Snapshot, error) {
	a.paused = true
	a.pauseCalls++
	return service.Snapshot{Phase: service.PhaseReadyForGadget, GadgetPaused: true}, nil
}

func (a *fakeGadgetAdmin) ResumeGadget(context.Context) (service.Snapshot, error) {
	a.paused = true
	a.resumeCalls++
	return service.Snapshot{Phase: service.PhaseReadyForGadget, GadgetPaused: true, GadgetResumePending: true}, nil
}

func TestGadgetAdminRequiresPhysicalDisconnectConfirmation(t *testing.T) {
	t.Parallel()
	admin := &fakeGadgetAdmin{}
	handler := NewHandler(fakeController{}, slog.New(slog.NewTextHandler(io.Discard, nil)), WithGadgetAdmin(admin))

	for _, body := range []string{`{}`, `{"confirm_physical_disconnect":false}`} {
		response := httptest.NewRecorder()
		request := httptest.NewRequest(http.MethodPost, "/api/v1/gadget/disconnect", strings.NewReader(body))
		handler.ServeHTTP(response, request)
		if response.Code != http.StatusBadRequest {
			t.Fatalf("body %s status = %d, want %d", body, response.Code, http.StatusBadRequest)
		}
	}
	if admin.pauseCalls != 0 {
		t.Fatalf("pause calls = %d, want 0", admin.pauseCalls)
	}
}

func TestGadgetAdminPausesAndResumes(t *testing.T) {
	t.Parallel()
	admin := &fakeGadgetAdmin{}
	handler := NewHandler(fakeController{}, slog.New(slog.NewTextHandler(io.Discard, nil)), WithGadgetAdmin(admin))

	disconnect := httptest.NewRecorder()
	disconnectRequest := httptest.NewRequest(http.MethodPost, "/api/v1/gadget/disconnect", strings.NewReader(`{"confirm_physical_disconnect":true}`))
	handler.ServeHTTP(disconnect, disconnectRequest)
	if disconnect.Code != http.StatusOK || !strings.Contains(disconnect.Body.String(), `"gadget_paused":true`) || admin.pauseCalls != 1 {
		t.Fatalf("disconnect status = %d, body = %s, calls = %d", disconnect.Code, disconnect.Body.String(), admin.pauseCalls)
	}

	unconfirmed := httptest.NewRecorder()
	handler.ServeHTTP(unconfirmed, httptest.NewRequest(http.MethodPost, "/api/v1/gadget/reconnect", strings.NewReader(`{}`)))
	if unconfirmed.Code != http.StatusBadRequest || admin.resumeCalls != 0 {
		t.Fatalf("unconfirmed reconnect status = %d, calls = %d", unconfirmed.Code, admin.resumeCalls)
	}

	reconnect := httptest.NewRecorder()
	handler.ServeHTTP(reconnect, httptest.NewRequest(http.MethodPost, "/api/v1/gadget/reconnect", strings.NewReader(`{"confirm_physical_connection":true}`)))
	if reconnect.Code != http.StatusOK || !strings.Contains(reconnect.Body.String(), `"gadget_paused":true`) || !strings.Contains(reconnect.Body.String(), `"gadget_resume_pending":true`) || admin.resumeCalls != 1 {
		t.Fatalf("reconnect status = %d, body = %s, calls = %d", reconnect.Code, reconnect.Body.String(), admin.resumeCalls)
	}
}

func TestGadgetAdminWarmRebootRecovery(t *testing.T) {
	t.Parallel()
	admin := &fakeGadgetAdmin{}
	power := &fakeDeviceAdmin{}
	handler := NewHandler(
		fakeController{},
		slog.New(slog.NewTextHandler(io.Discard, nil)),
		WithGadgetAdmin(admin),
		WithDeviceAdmin(power),
	)

	unconfirmed := httptest.NewRecorder()
	handler.ServeHTTP(unconfirmed, httptest.NewRequest(http.MethodPost, "/api/v1/gadget/reboot-recover", strings.NewReader(`{}`)))
	if unconfirmed.Code != http.StatusBadRequest || admin.resumeCalls != 0 || power.powerAction != "" {
		t.Fatalf("unconfirmed status = %d, resume calls = %d, power = %q", unconfirmed.Code, admin.resumeCalls, power.powerAction)
	}

	response := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/api/v1/gadget/reboot-recover", strings.NewReader(`{"confirm_physical_connection":true}`))
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusAccepted || admin.resumeCalls != 1 || power.powerAction != "reboot" {
		t.Fatalf("status = %d, body = %s, resume calls = %d, power = %q", response.Code, response.Body.String(), admin.resumeCalls, power.powerAction)
	}
}
