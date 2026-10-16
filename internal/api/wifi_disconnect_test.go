package api

import (
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestWiFiDisconnectRouteTargetsUUID(t *testing.T) {
	t.Parallel()
	admin := &fakeDeviceAdmin{}
	mux := http.NewServeMux()
	registerWiFiDisconnectRoute(mux, admin, slog.New(slog.NewTextHandler(io.Discard, nil)))
	response := httptest.NewRecorder()
	mux.ServeHTTP(response, httptest.NewRequest(http.MethodPost, "/api/v1/network/wifi/disconnect", strings.NewReader(`{"uuid":"11111111-1111-1111-1111-111111111111"}`)))
	if response.Code != 200 || admin.disconnectedUUID != "11111111-1111-1111-1111-111111111111" {
		t.Fatalf("断开结果=%d %s uuid=%s", response.Code, response.Body.String(), admin.disconnectedUUID)
	}
}
