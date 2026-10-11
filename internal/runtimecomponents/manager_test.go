package runtimecomponents

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"github.com/wen-ryon/teslabox/internal/webauth"
	"github.com/wen-ryon/teslabox/internal/webui"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func componentFor(data []byte, u string) Component {
	h := sha256.Sum256(data)
	return Component{ID: "nas", Version: "1.75.1", URL: u, Size: int64(len(data)), SHA256: hex.EncodeToString(h[:]), Format: "zip"}
}
func testManager(t *testing.T, c Component, client *http.Client, install Installer) *Manager {
	t.Helper()
	m, e := New(Config{Root: filepath.Join(t.TempDir(), "private"), Components: []Component{c}, Install: map[string]Installer{"nas": install}, HTTPClient: client, RetryInterval: time.Millisecond})
	if e != nil {
		t.Fatal(e)
	}
	return m
}
func TestParseRequiresBoundCompleteRelease(t *testing.T) {
	data := []byte("archive")
	c := componentFor(data, "https://example.invalid/fixed.zip")
	media := c
	media.ID = "media"
	media.Format = "tar.gz"
	cp := media
	cp.ID = "carplay"
	cases := []struct {
		name string
		d    Descriptor
		ok   bool
	}{{"complete", Descriptor{1, []Component{c, media, cp}}, true}, {"empty", Descriptor{1, nil}, false}, {"duplicate", Descriptor{1, []Component{c, c, cp}}, false}, {"wrong_schema", Descriptor{2, []Component{c, media, cp}}, false}}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			b, _ := json.Marshal(tc.d)
			_, e := Parse(b)
			if (e == nil) != tc.ok {
				t.Fatalf("parse err=%v", e)
			}
		})
	}
	b, _ := json.Marshal(Descriptor{1, []Component{c, media, cp}})
	if _, e := Parse(append(b, []byte(" {}")...)); e == nil {
		t.Fatal("accepted trailing object")
	}
}
func TestEnsureDownloadsOnceAndOfflineReusesVerifiedBytes(t *testing.T) {
	data := []byte("fixed archive")
	var hits, installed atomic.Int32
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { hits.Add(1); w.Write(data) }))
	defer server.Close()
	c := componentFor(data, server.URL+"/fixed.zip")
	m := testManager(t, c, server.Client(), func(ctx context.Context, c Component, p string) error {
		if e := ValidateFile(ctx, c, p); e != nil {
			return e
		}
		installed.Add(1)
		return nil
	})
	if e := m.Ensure(context.Background(), "nas"); e != nil {
		t.Fatal(e)
	}
	server.Close()
	if e := m.Ensure(context.Background(), "nas"); e != nil {
		t.Fatal(e)
	}
	if hits.Load() != 1 || installed.Load() != 2 || !m.Ready("nas") {
		t.Fatalf("hits %d installs %d status %+v", hits.Load(), installed.Load(), m.Status())
	}
	states := m.Status()
	states[0].State = "failed"
	if !m.Ready("nas") {
		t.Fatal("status leaked mutable state")
	}
}
func TestEnsureRejectsUntrustedResponseBeforeInstaller(t *testing.T) {
	for _, kind := range []string{"bad_hash", "oversized", "short", "http_error", "redirect"} {
		t.Run(kind, func(t *testing.T) {
			data := []byte("archive")
			var installed atomic.Int32
			server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch kind {
				case "http_error":
					w.WriteHeader(503)
				case "redirect":
					http.Redirect(w, r, "https://elsewhere.invalid/fixed.zip", 302)
				case "oversized":
					w.Write(append(data, 'x'))
				case "short":
					w.Write(data[:2])
				default:
					w.Write([]byte("corrupt"))
				}
			}))
			defer server.Close()
			m := testManager(t, componentFor(data, server.URL+"/fixed.zip"), server.Client(), func(context.Context, Component, string) error { installed.Add(1); return nil })
			if e := m.Ensure(context.Background(), "nas"); e == nil {
				t.Fatal("accepted response")
			}
			if installed.Load() != 0 || m.Ready("nas") {
				t.Fatal("activated untrusted input")
			}
		})
	}
}
func TestOfflineSeedAndFailedInstallPreservePrevious(t *testing.T) {
	data := []byte("archive")
	c := componentFor(data, "https://example.invalid/fixed.zip")
	previous := filepath.Join(t.TempDir(), "old")
	os.WriteFile(previous, []byte("previous"), 0600)
	m := testManager(t, c, nil, func(context.Context, Component, string) error { return errors.New("self-test failed") })
	source := filepath.Join(t.TempDir(), "input.zip")
	os.WriteFile(source, data, 0600)
	if e := m.Seed(context.Background(), "nas", source); e != nil {
		t.Fatal(e)
	}
	if e := m.Ensure(context.Background(), "nas"); e == nil {
		t.Fatal("accepted bad self-test")
	}
	if b, _ := os.ReadFile(previous); string(b) != "previous" {
		t.Fatal("previous changed")
	}
	if e := ValidateFile(context.Background(), c, ArchivePath(m.cfg.Root, c)); e != nil {
		t.Fatal(e)
	}
}
func TestCorruptCacheIsNeverExecuted(t *testing.T) {
	data := []byte("archive")
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(503) }))
	defer server.Close()
	c := componentFor(data, server.URL+"/fixed.zip")
	var installs atomic.Int32
	m := testManager(t, c, server.Client(), func(context.Context, Component, string) error { installs.Add(1); return nil })
	if e := secureDirectory(m.cfg.Root); e != nil {
		t.Fatal(e)
	}
	os.WriteFile(ArchivePath(m.cfg.Root, c), []byte("corrupt"), 0600)
	if e := m.Ensure(context.Background(), "nas"); e == nil {
		t.Fatal("used corrupt cache")
	}
	if installs.Load() != 0 {
		t.Fatal("executed corrupt cache")
	}
}
func TestCancellationStopsRetries(t *testing.T) {
	data := []byte("archive")
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(503) }))
	defer server.Close()
	m := testManager(t, componentFor(data, server.URL+"/fixed.zip"), server.Client(), func(context.Context, Component, string) error { return nil })
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { m.Run(ctx); close(done) }()
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("worker did not cancel")
	}
}
func TestPendingGateLeavesCoreHealthAndManagementAvailable(t *testing.T) {
	m := testManager(t, componentFor([]byte("a"), "https://example.invalid/fixed.zip"), nil, func(context.Context, Component, string) error { return nil })
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(204) })
	for _, tc := range []struct {
		path, method string
		status       int
	}{{"/healthz", "GET", 204}, {"/api/v1/device/wifi", "GET", 204}, {"/api/v1/uploads", "GET", 204}, {"/api/v1/uploads", "POST", 503}, {"/api/v1/runtime-components", "GET", 200}} {
		w := httptest.NewRecorder()
		m.Gate(next).ServeHTTP(w, httptest.NewRequest(tc.method, tc.path, nil))
		if w.Code != tc.status {
			t.Fatalf("%s %s =>%d", tc.method, tc.path, w.Code)
		}
	}
}

func TestConcurrentEnsureSerializesDownload(t *testing.T) {
	data := []byte("fixed archive")
	var hits atomic.Int32
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { hits.Add(1); w.Write(data) }))
	defer server.Close()
	m := testManager(t, componentFor(data, server.URL+"/fixed.zip"), server.Client(), func(context.Context, Component, string) error { return nil })
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := m.Ensure(context.Background(), "nas"); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	if hits.Load() != 1 {
		t.Fatalf("重复下载 %d 次", hits.Load())
	}
}
func TestComponentAPIStaysInsideActualWebAuthentication(t *testing.T) {
	m := testManager(t, componentFor([]byte("a"), "https://example.invalid/fixed.zip"), nil, func(context.Context, Component, string) error { return nil })
	digest := sha256.Sum256([]byte("owner-password"))
	auth, err := webauth.New(webauth.Config{StatePath: filepath.Join(t.TempDir(), "auth.json"), PasswordSHA256: hex.EncodeToString(digest[:]), SessionSecret: strings.Repeat("ab", 32), SessionTTL: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(204) })
	handler := webui.NewHandler(m.Gate(next), auth, nil)
	anonymous := httptest.NewRecorder()
	handler.ServeHTTP(anonymous, httptest.NewRequest("GET", "http://teslabox.local/api/v1/runtime-components", nil))
	if anonymous.Code != 401 {
		t.Fatalf("未登录组件状态泄漏: %d", anonymous.Code)
	}
	login := httptest.NewRequest("POST", "http://teslabox.local/api/v1/auth/login", strings.NewReader(`{"password":"owner-password"}`))
	login.Header.Set("Origin", "http://teslabox.local")
	logged := httptest.NewRecorder()
	handler.ServeHTTP(logged, login)
	if logged.Code != 200 {
		t.Fatal(logged.Code, logged.Body.String())
	}
	for _, tc := range []struct {
		path string
		code int
	}{{"/api/v1/runtime-components", 200}, {"/api/v1/thumbnails/id", 503}, {"/api/v1/videos/id/metadata", 503}, {"/api/v1/video-groups/id/composite", 503}, {"/api/v1/videos/id/content", 204}, {"/api/v1/carplay/settings", 204}} {
		r := httptest.NewRequest("GET", "http://teslabox.local"+tc.path, nil)
		for _, c := range logged.Result().Cookies() {
			r.AddCookie(c)
		}
		out := httptest.NewRecorder()
		handler.ServeHTTP(out, r)
		if out.Code != tc.code {
			t.Fatalf("%s code=%d body=%s", tc.path, out.Code, out.Body.String())
		}
	}
}

func TestComponentRedirectPolicy(t *testing.T) {
	for _, tc := range []struct {
		name, initial, destination string
		hops                       int
		allowed                    bool
	}{
		{"same_host", "https://downloads.invalid/fixed", "https://downloads.invalid/asset", 1, true},
		{"gitee_same_host", "https://gitee.com/fixed", "https://gitee.com/asset", 1, true},
		{"gitee_cdn", "https://gitee.com/fixed", "https://foruda.gitee.com/asset?signature=temporary", 2, true},
		{"gitee_explicit_https_port", "https://gitee.com:443/fixed", "https://foruda.gitee.com:443/asset", 2, true},
		{"gitee_maximum_hops", "https://gitee.com/fixed", "https://foruda.gitee.com/asset", 3, true},
		{"too_many_hops", "https://gitee.com/fixed", "https://foruda.gitee.com/asset", 4, false},
		{"missing_initial", "https://gitee.com/fixed", "https://foruda.gitee.com/asset", 0, false},
		{"http_downgrade", "https://gitee.com/fixed", "http://foruda.gitee.com/asset", 2, false},
		{"http_initial", "http://gitee.com/fixed", "https://foruda.gitee.com/asset", 2, false},
		{"destination_credentials", "https://gitee.com/fixed", "https://user:password@foruda.gitee.com/asset", 2, false},
		{"initial_credentials", "https://user:password@gitee.com/fixed", "https://foruda.gitee.com/asset", 2, false},
		{"destination_port", "https://gitee.com/fixed", "https://foruda.gitee.com:8443/asset", 2, false},
		{"gitee_destination_port", "https://gitee.com/fixed", "https://gitee.com:8443/asset", 1, false},
		{"initial_port", "https://gitee.com:8443/fixed", "https://foruda.gitee.com/asset", 2, false},
		{"initial_port_same_host", "https://gitee.com:8443/fixed", "https://gitee.com:8443/asset", 1, false},
		{"other_gitee_subdomain", "https://gitee.com/fixed", "https://elsewhere.gitee.com/asset", 2, false},
		{"deceptive_suffix", "https://gitee.com/fixed", "https://foruda.gitee.com.attacker.invalid/asset", 2, false},
		{"third_party", "https://gitee.com/fixed", "https://elsewhere.invalid/asset", 2, false},
		{"other_initial_borrows_cdn", "https://elsewhere.invalid/fixed", "https://foruda.gitee.com/asset", 1, false},
		{"other_initial_borrows_gitee", "https://elsewhere.invalid/fixed", "https://gitee.com/asset", 1, false},
		{"initial_cdn_cannot_borrow_gitee", "https://foruda.gitee.com/fixed", "https://gitee.com/asset", 1, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			initial, err := http.NewRequest(http.MethodGet, tc.initial, nil)
			if err != nil {
				t.Fatal(err)
			}
			destination, err := http.NewRequest(http.MethodGet, tc.destination, nil)
			if err != nil {
				t.Fatal(err)
			}
			via := make([]*http.Request, tc.hops)
			for i := range via {
				via[i] = initial
			}
			if err := checkComponentRedirect(destination, via); (err == nil) != tc.allowed {
				t.Fatalf("allowed=%v, err=%v", tc.allowed, err)
			}
		})
	}
}

type componentRoundTripFunc func(*http.Request) (*http.Response, error)

func (f componentRoundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) {
	return f(req)
}

func TestGiteeRedirectChainRetainsArchiveVerification(t *testing.T) {
	for _, tc := range []struct {
		name, destination string
		data              []byte
		allowed           bool
	}{
		{"verified", "https://foruda.gitee.com/asset?signature=temporary", []byte("fixed archive"), true},
		{"corrupt", "https://foruda.gitee.com/asset?signature=temporary", []byte("wrong archive"), false},
		{"short", "https://foruda.gitee.com/asset", []byte("short"), false},
		{"oversized", "https://foruda.gitee.com/asset", []byte("fixed archive extra"), false},
		{"third_party", "https://elsewhere.invalid/asset", []byte("fixed archive"), false},
		{"http_downgrade", "http://foruda.gitee.com/asset", []byte("fixed archive"), false},
		{"credentials", "https://user:password@foruda.gitee.com/asset", []byte("fixed archive"), false},
		{"abnormal_port", "https://foruda.gitee.com:8443/asset", []byte("fixed archive"), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var hits, installed int
			client := &http.Client{Transport: componentRoundTripFunc(func(req *http.Request) (*http.Response, error) {
				hits++
				header := make(http.Header)
				status := http.StatusOK
				body := ""
				switch req.URL.Host + req.URL.Path {
				case "gitee.com/fixed":
					status = http.StatusFound
					header.Set("Location", "https://gitee.com/intermediate")
				case "gitee.com/intermediate":
					status = http.StatusFound
					header.Set("Location", tc.destination)
				default:
					body = string(tc.data)
				}
				return &http.Response{StatusCode: status, Header: header, Body: io.NopCloser(strings.NewReader(body)), ContentLength: int64(len(body)), Request: req}, nil
			})}
			m := testManager(t, componentFor([]byte("fixed archive"), "https://gitee.com/fixed"), client,
				func(ctx context.Context, c Component, path string) error {
					installed++
					return ValidateFile(ctx, c, path)
				})
			err := m.Ensure(context.Background(), "nas")
			if (err == nil) != tc.allowed || m.Ready("nas") != tc.allowed {
				t.Fatalf("allowed=%v, ready=%v, err=%v", tc.allowed, m.Ready("nas"), err)
			}
			if tc.allowed && (installed != 1 || hits != 3) {
				t.Fatalf("installs=%d requests=%d", installed, hits)
			}
			if !tc.allowed && installed != 0 {
				t.Fatal("unverified archive reached installer")
			}
			parsed, err := url.Parse(tc.destination)
			if err != nil {
				t.Fatal(err)
			}
			if (parsed.Scheme != "https" || parsed.User != nil || parsed.Host != "foruda.gitee.com") && hits != 2 {
				t.Fatal("forbidden redirect reached transport")
			}
		})
	}
}
