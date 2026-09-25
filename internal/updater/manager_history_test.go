package updater

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestManagerSelectsSignedHistoricalRollback(t *testing.T) {
	publicKey, privateKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	rollback := testManifest(t, privateKey, "0.1.6")
	current := testManifest(t, privateKey, "0.1.7")
	upgrade := testManifest(t, privateKey, "0.1.8")
	catalog := Catalog{
		SchemaVersion: catalogSchemaVersion,
		Channel:       "stable",
		GeneratedAt:   time.Now().UTC().Format(time.RFC3339),
		Manifests:     []Manifest{rollback, current, upgrade},
	}
	if err := catalog.Sign(privateKey); err != nil {
		t.Fatal(err)
	}
	server := httptest.NewTLSServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("Content-Type", "application/json")
		switch request.URL.Path {
		case "/latest.json":
			_ = json.NewEncoder(writer).Encode(upgrade)
		case "/history.json":
			_ = json.NewEncoder(writer).Encode(catalog)
		default:
			http.NotFound(writer, request)
		}
	}))
	defer server.Close()
	client, err := NewClient(ClientConfig{
		ManifestURL: server.URL + "/latest.json", CatalogURL: server.URL + "/history.json",
		PublicKey: publicKey, Channel: "stable", Board: "radxa-rock-3a", Arch: "arm64",
		DownloadDir: t.TempDir(), MaximumSize: 128 * 1024 * 1024, HTTPClient: server.Client(),
	})
	if err != nil {
		t.Fatal(err)
	}
	manager := &Manager{
		config: Config{CurrentVersion: "0.1.7", Channel: "stable"},
		client: client,
		state:  Status{Configured: true, HistoryConfigured: true, CurrentVersion: "0.1.7", Channel: "stable", State: StateIdle},
	}
	history, err := manager.History(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(history.Releases) != 2 || history.Releases[0].Version != "0.1.8" || history.Releases[1].Action != "rollback" {
		t.Fatalf("History() = %#v", history.Releases)
	}
	status, err := manager.Select(context.Background(), "0.1.6")
	if err != nil {
		t.Fatal(err)
	}
	if status.State != StateAvailable || !status.Available || status.SelectedAction != "rollback" || status.LatestVersion != "0.1.6" {
		t.Fatalf("Select() status = %#v", status)
	}
}

func TestManagerBetaRCUpgradeAndHistory(t *testing.T) {
	publicKey, privateKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	manifests := make([]Manifest, 0, 3)
	for _, version := range []string{"0.1.31-rc.1", "0.1.31-rc.2", "0.1.31-rc.10"} {
		manifest := testManifest(t, privateKey, version)
		manifest.Channel = "beta"
		if err := manifest.Sign(privateKey); err != nil {
			t.Fatal(err)
		}
		manifests = append(manifests, manifest)
	}
	catalog := Catalog{
		SchemaVersion: catalogSchemaVersion,
		Channel:       "beta",
		GeneratedAt:   time.Now().UTC().Format(time.RFC3339),
		Manifests:     manifests,
	}
	if err := catalog.Sign(privateKey); err != nil {
		t.Fatal(err)
	}
	server := httptest.NewTLSServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("Content-Type", "application/json")
		switch request.URL.Path {
		case "/latest.json":
			_ = json.NewEncoder(writer).Encode(manifests[1])
		case "/history.json":
			_ = json.NewEncoder(writer).Encode(catalog)
		default:
			http.NotFound(writer, request)
		}
	}))
	defer server.Close()
	client, err := NewClient(ClientConfig{
		ManifestURL: server.URL + "/latest.json", CatalogURL: server.URL + "/history.json",
		PublicKey: publicKey, Channel: "beta", Board: "radxa-rock-3a", Arch: "arm64",
		DownloadDir: t.TempDir(), MaximumSize: 128 * 1024 * 1024, HTTPClient: server.Client(),
	})
	if err != nil {
		t.Fatal(err)
	}
	manager := &Manager{
		config: Config{CurrentVersion: "0.1.31-rc.1", Channel: "beta"},
		client: client,
		state:  Status{Configured: true, HistoryConfigured: true, CurrentVersion: "0.1.31-rc.1", Channel: "beta", State: StateIdle},
	}
	status, err := manager.Check(context.Background())
	if err != nil || !status.Available || status.LatestVersion != "0.1.31-rc.2" || status.SelectedAction != "upgrade" {
		t.Fatalf("Check() = %#v, %v", status, err)
	}
	history, err := manager.History(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(history.Releases) != 2 || history.Releases[0].Version != "0.1.31-rc.10" || history.Releases[1].Version != "0.1.31-rc.2" {
		t.Fatalf("History() = %#v", history.Releases)
	}
	status, err = manager.Select(context.Background(), "0.1.31-rc.10")
	if err != nil || status.LatestVersion != "0.1.31-rc.10" || status.SelectedAction != "upgrade" {
		t.Fatalf("Select() = %#v, %v", status, err)
	}
}
