package updater

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync"
	"testing"
)

func TestChannelSwitchConcurrentOperations(t *testing.T) {
	key, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { http.NotFound(w, r) }))
	defer server.Close()
	root := t.TempDir()
	m, err := New(Config{ManifestURL: server.URL + "/stable", CatalogURL: server.URL + "/history", BetaManifestURL: server.URL + "/beta", BetaCatalogURL: server.URL + "/beta-history", PublicKey: base64.StdEncoding.EncodeToString(key), DownloadDir: root, RequestPath: filepath.Join(root, "request"), ResultPath: filepath.Join(root, "result")}, nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, client := range m.clients {
		client.config.HTTPClient = server.Client()
	}
	var wg sync.WaitGroup
	for worker := 0; worker < 6; worker++ {
		wg.Add(1)
		go func(worker int) {
			defer wg.Done()
			for i := 0; i < 100; i++ {
				switch worker {
				case 0:
					_, _ = m.SetChannel([]string{"stable", "beta"}[i%2])
				case 1:
					_, _ = m.Check(context.Background())
				case 2:
					_, _ = m.History(context.Background())
				case 3:
					_, _ = m.Select(context.Background(), "1.2.3")
				case 4:
					_, _ = m.StartDownload()
				case 5:
					_, _ = m.Install(context.Background())
				}
			}
		}(worker)
	}
	wg.Wait()
	if status := m.Status(); status.Channel != "stable" && status.Channel != "beta" {
		t.Fatalf("invalid status: %+v", status)
	}
}
