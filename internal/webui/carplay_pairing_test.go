package webui

import (
	"context"
	"io"
	"log/slog"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/wen-ryon/teslabox/internal/api"
)

func TestPairingRoutesUseDeviceAuthenticationAndSameOrigin(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	for _, item := range []struct {
		name   string
		auth   carplayAuth
		origin string
		want   int
	}{
		{"未登录", carplayAuth{}, "", 401},
		{"必须修改密码", carplayAuth{authenticated: true, change: true}, "", 428},
		{"异源", carplayAuth{authenticated: true}, "https://other.example", 403},
		{"同源", carplayAuth{authenticated: true}, "http://teslabox.local", 200},
	} {
		t.Run(item.name, func(t *testing.T) {
			h := NewHandler(api.NewHandler(nil, logger, api.WithCarPlayPairing(nil, context.Background())), &item.auth, logger)
			for _, method := range []string{"GET", "POST"} {
				path := "/api/v1/carplay/pairing"
				want := item.want
				if method == "POST" {
					path += "/confirm"
					if want == 200 {
						want = 503
					}
				}
				r := httptest.NewRequest(method, "http://teslabox.local"+path, strings.NewReader(`{"request_id":"old","digits":"123456"}`))
				if item.origin != "" {
					r.Header.Set("Origin", item.origin)
				}
				w := httptest.NewRecorder()
				h.ServeHTTP(w, r)
				if w.Code != want || strings.Contains(w.Body.String(), "123456") {
					t.Fatal(method, w.Code, w.Body.String())
				}
			}
		})
	}
}
