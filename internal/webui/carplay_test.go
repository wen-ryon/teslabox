package webui

import (
	"bufio"
	"crypto/tls"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

type carplayAuth struct{ authenticated, change bool }

func TestCarPlayAutoPlaySettingsDefaultsOff(t *testing.T) {
	handler := NewHandler(http.NotFoundHandler(), &carplayAuth{authenticated: true}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	reply := httptest.NewRecorder()
	handler.ServeHTTP(reply, httptest.NewRequest("GET", "http://teslabox/settings", nil))
	if reply.Code != http.StatusOK {
		t.Fatalf("设置页不可用: %d", reply.Code)
	}
	body := reply.Body.String()
	if !strings.Contains(body, `id="carplay-auto_play" type="checkbox">`) || !strings.Contains(body, "连接后自动播放音乐") {
		t.Fatal("自动播放设置未嵌入或默认被开启")
	}
}

func TestCarPlayDedicatedHome(t *testing.T) {
	handler := NewHandler(http.NotFoundHandler(), &carplayAuth{},
		slog.New(slog.NewTextHandler(io.Discard, nil)),
		WithCarPlay(http.NotFoundHandler()), WithCarPlayHome("carplay.teslabox.online"))
	for _, item := range []struct {
		url     string
		tls     bool
		carplay bool
	}{
		{"https://carplay.teslabox.online/", true, true},
		{"https://carplay.teslabox.online:443/", true, true},
		{"http://carplay.teslabox.online/", false, false},
		{"http://teslabox.local/", false, false},
		{"https://other.online/", true, false},
		{"https://carplay.teslabox.online/settings", true, false},
	} {
		r := httptest.NewRequest(http.MethodGet, item.url, nil)
		if item.tls {
			r.TLS = &tls.ConnectionState{}
		}
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, r)
		if w.Code != http.StatusOK || strings.Contains(w.Body.String(), "<title>TeslaBox CarPlay</title>") != item.carplay {
			t.Fatalf("首页入口错误: %s", item.url)
		}
	}
}

func (a *carplayAuth) Enabled() bool                                   { return true }
func (a *carplayAuth) MustChangePassword() bool                        { return a.change }
func (a *carplayAuth) Authenticate(string, string) error               { return nil }
func (a *carplayAuth) ChangePassword(string, string, string) error     { return nil }
func (a *carplayAuth) SetSession(http.ResponseWriter, *http.Request)   {}
func (a *carplayAuth) ClearSession(http.ResponseWriter, *http.Request) {}
func (a *carplayAuth) Authenticated(*http.Request) bool                { return a.authenticated }

func carplayTokenFixture(t *testing.T) string {
	t.Helper()
	directory := t.TempDir()
	if err := os.Chmod(directory, 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(directory, "browser-token")
	if err := os.WriteFile(path, []byte(strings.Repeat("a", 64)), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestCarPlayAuthenticationAndPrivateProxy(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Cookie") != "tbcp="+strings.Repeat("a", 64) || r.Header.Get("Authorization") != "" || r.Host != strings.TrimPrefix(r.Header.Get("Origin"), "http://") {
			t.Error("upstream credential or origin boundary failed")
		}
		w.Header().Set("Set-Cookie", "tbcp=secret")
		io.WriteString(w, `{"frames":1}`)
	}))
	defer backend.Close()
	auth := &carplayAuth{}
	handler := NewHandler(http.NotFoundHandler(), auth, slog.New(slog.NewTextHandler(io.Discard, nil)), WithCarPlay(newCarPlayProxy(carplayTokenFixture(t), strings.TrimPrefix(backend.URL, "http://"))))
	for _, item := range []struct {
		path, origin          string
		authenticated, change bool
		want                  int
	}{
		{"/carplay/api/status", "", false, false, 401},
		{"/carplay/api/status", "", true, true, 428},
		{"/carplay/api/status", "http://other.host", true, false, 403},
		{"/carplay/video", "", true, false, 403},
		{"/carplay/login", "", true, false, 404},
		{"/carplay/api/status?target=evil", "", true, false, 404},
		{"/carplay/api/status", "", true, false, 200},
		{"/carplay", "", false, false, 200},
		{"/carplay/connection.js", "", false, false, 200},
	} {
		auth.authenticated = item.authenticated
		auth.change = item.change
		r := httptest.NewRequest("GET", "http://teslabox.local"+item.path, nil)
		r.Header.Set("Cookie", "owner_session=private; tbcp=attacker")
		r.Header.Set("Authorization", "Bearer private")
		r.Header.Set("Origin", item.origin)
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, r)
		if w.Code != item.want {
			t.Errorf("%s: status %d want %d", item.path, w.Code, item.want)
		}
		if strings.Contains(w.Header().Get("Set-Cookie"), "tbcp") || strings.Contains(w.Body.String(), strings.Repeat("a", 64)) {
			t.Fatal("receiver token leaked")
		}
		if item.path == "/carplay" && (strings.Contains(w.Body.String(), "<script>") || strings.Contains(w.Body.String(), "<style>")) {
			t.Fatal("inline content conflicts with CSP")
		}
		if item.path == "/carplay/connection.js" && !strings.Contains(w.Body.String(), "'/carplay/video'") {
			t.Fatal("video path not scoped")
		}
	}
}

func TestCarPlayUpgradeTransfersBothDirections(t *testing.T) {
	result := make(chan error, 1)
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		connection, buffer, err := w.(http.Hijacker).Hijack()
		if err != nil {
			result <- err
			return
		}
		defer connection.Close()
		connection.SetDeadline(time.Now().Add(3 * time.Second))
		fmt.Fprint(buffer, "HTTP/1.1 101 Switching Protocols\r\nConnection: Upgrade\r\nUpgrade: websocket\r\n\r\n")
		buffer.Flush()
		packet := make([]byte, 3)
		_, err = io.ReadFull(buffer, packet)
		if err == nil && string(packet) != "hid" {
			err = fmt.Errorf("touch packet changed")
		}
		if err == nil {
			_, err = buffer.WriteString("h264")
			buffer.Flush()
		}
		result <- err
	}))
	defer backend.Close()
	front := httptest.NewServer(newCarPlayProxy(carplayTokenFixture(t), strings.TrimPrefix(backend.URL, "http://")))
	defer front.Close()
	connection, err := net.DialTimeout("tcp", strings.TrimPrefix(front.URL, "http://"), time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer connection.Close()
	connection.SetDeadline(time.Now().Add(3 * time.Second))
	fmt.Fprintf(connection, "GET /carplay/video HTTP/1.1\r\nHost: %s\r\nOrigin: %s\r\nConnection: Upgrade\r\nUpgrade: websocket\r\n\r\n", strings.TrimPrefix(front.URL, "http://"), front.URL)
	reader := bufio.NewReader(connection)
	response, err := http.ReadResponse(reader, nil)
	if err != nil || response.StatusCode != 101 {
		t.Fatalf("upgrade: %v %#v", err, response)
	}
	if _, err = connection.Write([]byte("hid")); err != nil {
		t.Fatal(err)
	}
	packet := make([]byte, 4)
	if _, err = io.ReadFull(reader, packet); err != nil || string(packet) != "h264" {
		t.Fatalf("video transfer: %q %v", packet, err)
	}
	if err = <-result; err != nil {
		t.Fatal(err)
	}
}
