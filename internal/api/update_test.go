package api

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/wen-ryon/teslabox/internal/updater"
)

type fakeUpdateAdmin struct {
	status updater.Status
}

func (f *fakeUpdateAdmin) Status() updater.Status { return f.status }

func (f *fakeUpdateAdmin) Check(context.Context) (updater.Status, error) {
	f.status.State = updater.StateCurrent
	return f.status, nil
}

func (f *fakeUpdateAdmin) History(context.Context) (updater.ReleaseHistory, error) {
	return updater.ReleaseHistory{Configured: true, Releases: []updater.Release{{Version: "0.1.0", Action: "rollback"}}}, nil
}

func (f *fakeUpdateAdmin) Select(_ context.Context, version string) (updater.Status, error) {
	f.status.State = updater.StateAvailable
	f.status.LatestVersion = version
	return f.status, nil
}

func (f *fakeUpdateAdmin) StartDownload() (updater.Status, error) {
	f.status.State = updater.StateDownloading
	return f.status, nil
}

func (f *fakeUpdateAdmin) Install(context.Context) (updater.Status, error) {
	f.status.State = updater.StateInstalling
	return f.status, nil
}

func TestUpdateRoutesExposeStatusAndActions(t *testing.T) {
	admin := &fakeUpdateAdmin{status: updater.Status{Configured: true, CurrentVersion: "0.1.0", State: updater.StateIdle}}
	handler := NewHandler(fakeController{}, slog.New(slog.NewTextHandler(io.Discard, nil)), WithUpdateAdmin(admin))

	for _, test := range []struct {
		method string
		path   string
		body   string
		want   int
	}{
		{method: http.MethodGet, path: "/api/v1/update", want: http.StatusOK},
		{method: http.MethodPost, path: "/api/v1/update/check", want: http.StatusOK},
		{method: http.MethodGet, path: "/api/v1/update/history", want: http.StatusOK},
		{method: http.MethodPost, path: "/api/v1/update/select", want: http.StatusBadRequest},
		{method: http.MethodPost, path: "/api/v1/update/select", body: `{"version":"0.1.0"}{"version":"0.1.1"}`, want: http.StatusBadRequest},
		{method: http.MethodPost, path: "/api/v1/update/select", body: `{"version":"0.1.0"}`, want: http.StatusOK},
		{method: http.MethodPost, path: "/api/v1/update/download", want: http.StatusAccepted},
		{method: http.MethodPost, path: "/api/v1/update/install", want: http.StatusAccepted},
	} {
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, httptest.NewRequest(test.method, test.path, strings.NewReader(test.body)))
		if response.Code != test.want {
			t.Fatalf("%s %s status = %d, want %d", test.method, test.path, response.Code, test.want)
		}
	}
}
