package nasupload

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestRcloneDiagnosticsNeverExposeRawCredentialsOrServerOutput(t *testing.T) {
	for _, diagnostic := range []string{"ssh: handshake failed: knownhosts: key mismatch private-token", "ssh: unable to authenticate private-token", "x509: certificate signed by unknown authority private-token", "dial tcp: i/o timeout private-token"} {
		err := classifyCommandError(errors.New("exit status 1"), diagnostic)
		if strings.Contains(err.Error(), "private-token") {
			t.Fatal("raw command diagnostic leaked")
		}
	}
}

func TestRcloneRealWebDAVWriteReadPublishAndIdempotency(t *testing.T) {
	executable := os.Getenv("TESLABOX_TEST_RCLONE")
	if executable == "" {
		t.Skip("set TESLABOX_TEST_RCLONE to exercise the pinned real rclone binary")
	}
	var mu sync.Mutex
	files := map[string][]byte{}
	dirs := map[string]bool{"/": true}
	failPut, loseMoveResponse, recordingPuts := false, false, 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		user, password, ok := r.BasicAuth()
		if !ok || user != "test" || password != "test-password" {
			w.Header().Set("WWW-Authenticate", `Basic realm="test"`)
			w.WriteHeader(401)
			return
		}
		name := path.Clean(r.URL.Path)
		mu.Lock()
		defer mu.Unlock()
		switch r.Method {
		case "OPTIONS":
			w.Header().Set("DAV", "1, 2")
			w.WriteHeader(200)
		case "PROPFIND":
			data, exists := files[name]
			if !exists && !dirs[name] {
				w.WriteHeader(404)
				return
			}
			w.Header().Set("Content-Type", "application/xml")
			w.WriteHeader(207)
			fmt.Fprint(w, `<?xml version="1.0"?><d:multistatus xmlns:d="DAV:">`)
			response := func(item string, size int, directory bool) {
				resource := ""
				if directory {
					resource = "<d:collection/>"
					if item != "/" {
						item += "/"
					}
				}
				fmt.Fprintf(w, `<d:response><d:href>%s</d:href><d:propstat><d:prop><d:resourcetype>%s</d:resourcetype><d:getcontentlength>%d</d:getcontentlength><d:getlastmodified>Fri, 02 Oct 2026 04:00:00 GMT</d:getlastmodified></d:prop><d:status>HTTP/1.1 200 OK</d:status></d:propstat></d:response>`, item, resource, size)
			}
			response(name, len(data), dirs[name])
			if dirs[name] && r.Header.Get("Depth") != "0" {
				for item, data := range files {
					if path.Dir(item) == name {
						response(item, len(data), false)
					}
				}
				for item := range dirs {
					if item != name && path.Dir(item) == name {
						response(item, 0, true)
					}
				}
			}
			fmt.Fprint(w, `</d:multistatus>`)
		case "MKCOL":
			dirs[name] = true
			w.WriteHeader(201)
		case "PUT":
			if strings.Contains(name, "verified.mp4") {
				recordingPuts++
				if failPut {
					failPut = false
					files[name], _ = io.ReadAll(io.LimitReader(r.Body, 1024))
					w.WriteHeader(503)
					return
				}
			}
			data, err := io.ReadAll(io.LimitReader(r.Body, 2<<20))
			if err != nil {
				w.WriteHeader(500)
				return
			}
			files[name] = data
			w.WriteHeader(201)
		case "GET":
			data, exists := files[name]
			if !exists {
				w.WriteHeader(404)
				return
			}
			w.Header().Set("Content-Length", fmt.Sprint(len(data)))
			_, _ = w.Write(data)
		case "HEAD":
			data, exists := files[name]
			if !exists {
				w.WriteHeader(404)
				return
			}
			w.Header().Set("Content-Length", fmt.Sprint(len(data)))
		case "MOVE":
			destination, err := url.Parse(r.Header.Get("Destination"))
			if err != nil {
				w.WriteHeader(400)
				return
			}
			data, exists := files[name]
			if !exists {
				w.WriteHeader(404)
				return
			}
			files[path.Clean(destination.Path)] = data
			delete(files, name)
			if loseMoveResponse && strings.Contains(name, "verified.mp4") {
				loseMoveResponse = false
				w.WriteHeader(503)
				return
			}
			w.WriteHeader(201)
		case "DELETE":
			delete(files, name)
			w.WriteHeader(204)
		default:
			w.WriteHeader(405)
		}
	}))
	defer server.Close()
	r := &Rclone{Executable: executable, WorkDir: t.TempDir()}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	credential, err := r.Obscure(ctx, "test-password")
	if err != nil {
		t.Fatal(err)
	}
	target := Target{Protocol: "webdav", URL: server.URL, User: "test", Credential: credential, AllowHTTP: true, Path: "backup"}
	if err = r.Test(ctx, target); err != nil {
		t.Fatal(err)
	}
	local := filepath.Join(t.TempDir(), "sample.mp4")
	content := []byte(strings.Repeat("recording-test", 4096))
	if err = os.WriteFile(local, content, 0o600); err != nil {
		t.Fatal(err)
	}
	hash := sha256.Sum256(content)
	digest := hex.EncodeToString(hash[:])
	mu.Lock()
	failPut, loseMoveResponse = true, true
	mu.Unlock()
	if err = r.Upload(ctx, target, local, "backup/verified.mp4", digest, "task1"); err == nil {
		t.Fatal("incomplete PUT reported success")
	}
	if err = r.Upload(ctx, target, local, "backup/verified.mp4", digest, "task1"); err == nil {
		t.Fatal("lost publish response reported success")
	}
	for i := 0; i < 2; i++ {
		if err = r.Upload(ctx, target, local, "backup/verified.mp4", digest, "task1"); err != nil {
			t.Fatal(err)
		}
	}
	mu.Lock()
	if recordingPuts != 2 {
		t.Errorf("successful published file uploaded again: PUT=%d", recordingPuts)
	}
	if _, exists := files["/backup/verified.mp4.teslabox-task1.partial"]; exists {
		t.Error("published file left as partial")
	}
	files["/backup/verified.mp4"] = []byte("different-content")
	mu.Unlock()
	if err = r.Upload(ctx, target, local, "backup/verified.mp4", digest, "task1"); err == nil {
		t.Fatal("mismatching destination accepted or overwritten")
	}
}
