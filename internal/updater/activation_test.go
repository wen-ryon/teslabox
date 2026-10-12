package updater

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

func TestStartupRefusesOldHelperBeforeMigration(t *testing.T) {
	for _, name := range []string{"old helper", "missing guard", "ready"} {
		t.Run(name, func(t *testing.T) {
			calls := 0
			err := checkStartupProtection(func(command string, args ...string) ([]byte, error) {
				calls++
				if calls == 1 {
					if name == "old helper" {
						return nil, errors.New("unknown flag: protocol")
					}
					return []byte(TransactionProtocol), nil
				}
				if name == "missing guard" {
					return nil, nil
				}
				return []byte("{ path=/usr/local/libexec/teslabox-update-helper ; argv[]=/usr/local/libexec/teslabox-update-helper --recover ; }"), nil
			})
			if (err == nil) != (name == "ready") {
				t.Fatalf("startup result %v", err)
			}
			if name == "old helper" && calls != 1 {
				t.Fatal("旧助手路径不应继续启动")
			}
		})
	}
}

func TestActivationGateFailClosed(t *testing.T) {
	path := filepath.Join(t.TempDir(), "transaction.json")
	if activationPending(path) {
		t.Fatal("无事务应正常启动")
	}
	for _, tc := range []struct {
		data    string
		pending bool
	}{
		{`broken`, true}, {`{"version":2,"phase":"committed"}`, true},
		{`{"version":1,"phase":"prepared"}`, true}, {`{"version":1,"phase":"activating"}`, true},
		{`{"version":1,"phase":"restoring"}`, true}, {`{"version":1,"phase":"committed"}`, false},
		{`{"version":1,"phase":"rolled_back"}`, false},
	} {
		if err := os.WriteFile(path, []byte(tc.data), 0o600); err != nil {
			t.Fatal(err)
		}
		if got := activationPending(path); got != tc.pending {
			t.Fatalf("%s pending=%v", tc.data, got)
		}
		h := activationHandler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(204) }), func() bool { return activationPending(path) })
		for _, route := range []struct{ method, path string }{{"GET", "/healthz"}, {"POST", "/healthz"}, {"GET", "/api/exports"}, {"POST", "/api/exports"}} {
			w := httptest.NewRecorder()
			h.ServeHTTP(w, httptest.NewRequest(route.method, route.path, nil))
			want := 204
			if tc.pending && (route.path != "/healthz" || route.method != "GET") {
				want = 503
			}
			if w.Code != want {
				t.Fatalf("%v got=%d want=%d", route, w.Code, want)
			}
		}
	}
}
