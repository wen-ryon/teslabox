package webauth

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestManagerSessionLifecycle(t *testing.T) {
	t.Parallel()
	manager := newTestManager(t, "correct horse battery staple")
	now := time.Date(2026, 7, 16, 8, 0, 0, 0, time.UTC)
	manager.now = func() time.Time { return now }

	if err := manager.Authenticate("192.168.8.2:1234", "correct horse battery staple"); err != nil {
		t.Fatalf("Authenticate() error = %v", err)
	}
	request := httptest.NewRequest("GET", "http://teslabox.local/api/v1/storage", nil)
	recorder := httptest.NewRecorder()
	manager.SetSession(recorder, request)
	response := recorder.Result()
	cookies := response.Cookies()
	if len(cookies) != 1 || !cookies[0].HttpOnly || cookies[0].SameSite != http.SameSiteStrictMode || cookies[0].Secure {
		t.Fatalf("cookies = %#v", cookies)
	}
	request.AddCookie(cookies[0])
	if !manager.Authenticated(request) {
		t.Fatal("Authenticated() = false")
	}
	now = now.Add(2 * time.Hour)
	if manager.Authenticated(request) {
		t.Fatal("expired session remained authenticated")
	}
}

func TestManagerRejectsTamperedCookie(t *testing.T) {
	t.Parallel()
	manager := newTestManager(t, "password")
	request := httptest.NewRequest("GET", "http://teslabox.local/", nil)
	recorder := httptest.NewRecorder()
	manager.SetSession(recorder, request)
	cookie := recorder.Result().Cookies()[0]
	cookie.Value += "tampered"
	request.AddCookie(cookie)
	if manager.Authenticated(request) {
		t.Fatal("tampered session remained authenticated")
	}
}

func TestManagerRateLimitsRepeatedFailuresAndResetsAfterWindow(t *testing.T) {
	t.Parallel()
	manager := newTestManager(t, "password")
	now := time.Date(2026, 7, 16, 8, 0, 0, 0, time.UTC)
	manager.now = func() time.Time { return now }
	for range maxLoginFailures {
		if err := manager.Authenticate("192.168.8.2:1234", "wrong"); !errors.Is(err, ErrInvalidCredentials) {
			t.Fatalf("Authenticate() error = %v", err)
		}
	}
	if err := manager.Authenticate("192.168.8.2:1234", "password"); !errors.Is(err, ErrRateLimited) {
		t.Fatalf("Authenticate() error = %v, want rate limited", err)
	}
	now = now.Add(loginWindow)
	if err := manager.Authenticate("192.168.8.2:1234", "password"); err != nil {
		t.Fatalf("Authenticate() after reset error = %v", err)
	}
}

func TestNewSupportsExplicitDisabledModeAndRejectsPartialConfig(t *testing.T) {
	t.Parallel()
	disabled, err := New(Config{})
	if err != nil || disabled.Enabled() {
		t.Fatalf("disabled manager = %#v, error = %v", disabled, err)
	}
	if !disabled.Authenticated(httptest.NewRequest("GET", "http://teslabox.local/", nil)) {
		t.Fatal("disabled authentication rejected request")
	}
	if _, err := New(Config{PasswordSHA256: strings.Repeat("a", 64), SessionTTL: time.Hour}); err == nil {
		t.Fatal("New() error = nil, want missing secret error")
	}
}

func newTestManager(t *testing.T, password string) *Manager {
	t.Helper()
	digest := sha256.Sum256([]byte(password))
	manager, err := New(Config{
		PasswordSHA256: hex.EncodeToString(digest[:]),
		SessionSecret:  strings.Repeat("ab", 32),
		SessionTTL:     time.Hour,
	})
	if err != nil {
		t.Fatal(err)
	}
	return manager
}
