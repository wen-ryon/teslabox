package main

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestRunRejectsInvalidOutput(t *testing.T) {
	t.Parallel()
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	exitCode := run(t.Context(), []string{"--output", "yaml"}, &stdout, &stderr)
	if exitCode != 2 || !strings.Contains(stderr.String(), "output 必须是") {
		t.Fatalf("exitCode = %d, stderr = %q", exitCode, stderr.String())
	}
}

func TestRunReturnsCanceledExitCode(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	exitCode := run(ctx, []string{"--duration", "1s", "--interval", "1s", "--require-snapshot-rotation=false"}, &stdout, &stderr)
	if exitCode != 130 {
		t.Fatalf("exitCode = %d, stderr = %q", exitCode, stderr.String())
	}
}

func TestNewHTTPClientDisablesProxyByDefault(t *testing.T) {
	t.Parallel()
	client := newHTTPClient(time.Second, false)
	transport, ok := client.Transport.(*http.Transport)
	if !ok || transport.Proxy != nil {
		t.Fatalf("transport = %#v", client.Transport)
	}
	if client.Jar == nil {
		t.Fatal("client.Jar = nil")
	}
}

func TestLoginStoresSessionCookie(t *testing.T) {
	t.Parallel()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/auth/login" || r.Method != http.MethodPost {
			t.Fatalf("request = %s %s", r.Method, r.URL.Path)
		}
		var input struct {
			Password string `json:"password"`
		}
		if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
			t.Fatal(err)
		}
		if input.Password != "management-password" {
			t.Fatalf("password = %q", input.Password)
		}
		http.SetCookie(w, &http.Cookie{Name: "teslabox_session", Value: "signed", Path: "/"})
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	client := newHTTPClient(time.Second, false)
	if err := login(t.Context(), client, server.URL, "management-password"); err != nil {
		t.Fatal(err)
	}
	request, err := http.NewRequest(http.MethodGet, server.URL+"/api/v1/storage", nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, cookie := range client.Jar.Cookies(request.URL) {
		if cookie.Name == "teslabox_session" && cookie.Value == "signed" {
			return
		}
	}
	t.Fatal("登录响应的会话 Cookie 未保存")
}
