package webauth

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
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

func TestManagerAcceptsLegacyV1SessionUntilPasswordChanges(t *testing.T) {
	t.Parallel()
	manager := newTestManager(t, "password")
	expiresAt := time.Now().Add(time.Hour).Unix()
	payload := "v1." + strconv.FormatInt(expiresAt, 10)
	manager.stateMu.RLock()
	secret := append([]byte(nil), manager.credential.secret...)
	manager.stateMu.RUnlock()
	cookie := &http.Cookie{Name: CookieName, Value: payload + "." + signPayload(secret, payload)}
	request := httptest.NewRequest("GET", "http://teslabox.local/", nil)
	request.AddCookie(cookie)
	if !manager.Authenticated(request) {
		t.Fatal("legacy v1 session was rejected before credential migration")
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

func TestNewCreatesForcedDefaultPasswordForFreshInstall(t *testing.T) {
	statePath := filepath.Join(t.TempDir(), "web-auth.json")
	manager, err := New(Config{StatePath: statePath, SessionTTL: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	if !manager.Enabled() || !manager.MustChangePassword() {
		t.Fatalf("enabled = %v, must change = %v", manager.Enabled(), manager.MustChangePassword())
	}
	if err := manager.Authenticate("192.168.8.2:1234", DefaultPassword); err != nil {
		t.Fatalf("default password authentication error = %v", err)
	}
	content, err := os.ReadFile(statePath)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(content, []byte(DefaultPassword)) || !bytes.Contains(content, []byte(`"must_change": true`)) {
		t.Fatalf("credential state leaked password or omitted forced-change flag: %s", content)
	}
}

func TestNewRejectsPartialLegacyConfig(t *testing.T) {
	t.Parallel()
	if _, err := New(Config{StatePath: filepath.Join(t.TempDir(), "web-auth.json"), PasswordSHA256: strings.Repeat("a", 64), SessionTTL: time.Hour}); err == nil {
		t.Fatal("New() error = nil, want missing secret error")
	}
}

func TestChangePasswordMigratesLegacyStateAndInvalidatesSession(t *testing.T) {
	manager := newTestManager(t, "old-password")
	request := httptest.NewRequest("GET", "http://teslabox.local/api/v1/storage", nil)
	recorder := httptest.NewRecorder()
	manager.SetSession(recorder, request)
	oldCookie := recorder.Result().Cookies()[0]

	if err := manager.ChangePassword("192.168.8.2:1234", "old-password", "new memorable password"); err != nil {
		t.Fatal(err)
	}
	if manager.MustChangePassword() {
		t.Fatal("MustChangePassword() = true after change")
	}
	request.AddCookie(oldCookie)
	if manager.Authenticated(request) {
		t.Fatal("old session remained authenticated after password change")
	}
	if err := manager.Authenticate("192.168.8.2:1234", "old-password"); !errors.Is(err, ErrInvalidCredentials) {
		t.Fatalf("old password error = %v", err)
	}
	if err := manager.Authenticate("192.168.8.2:1234", "new memorable password"); err != nil {
		t.Fatalf("new password error = %v", err)
	}

	reloaded, err := New(Config{StatePath: manager.statePath, SessionTTL: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	if err := reloaded.Authenticate("192.168.8.2:1234", "new memorable password"); err != nil {
		t.Fatalf("reloaded password error = %v", err)
	}
}

func TestChangePasswordValidatesCurrentPasswordAndPolicy(t *testing.T) {
	t.Parallel()
	manager := newTestManager(t, "old-password")
	tests := []struct {
		name    string
		current string
		next    string
		want    error
	}{
		{name: "wrong current password", current: "wrong-password", next: "valid-new-password", want: ErrInvalidCredentials},
		{name: "new password too short", current: "old-password", next: "short", want: ErrPasswordPolicy},
		{name: "unchanged password", current: "old-password", next: "old-password", want: ErrPasswordUnchanged},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if err := manager.ChangePassword("192.168.8.2:1234", test.current, test.next); !errors.Is(err, test.want) {
				t.Fatalf("ChangePassword() error = %v, want %v", err, test.want)
			}
		})
	}
}

func newTestManager(t *testing.T, password string) *Manager {
	t.Helper()
	digest := sha256.Sum256([]byte(password))
	manager, err := New(Config{
		StatePath:      filepath.Join(t.TempDir(), "web-auth.json"),
		PasswordSHA256: hex.EncodeToString(digest[:]),
		SessionSecret:  strings.Repeat("ab", 32),
		SessionTTL:     time.Hour,
	})
	if err != nil {
		t.Fatal(err)
	}
	return manager
}
