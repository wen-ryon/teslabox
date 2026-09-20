package updater

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"
)

func TestManifestVerifyRejectsTampering(t *testing.T) {
	publicKey, privateKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	manifest := signedTestManifest(t, privateKey, "https://example.com/teslabox", []byte("program"))
	if err := manifest.Verify(publicKey, "stable", "radxa-rock-3a", "arm64", 1024); err != nil {
		t.Fatalf("Verify() error = %v", err)
	}
	manifest.ReleaseNotes = "被篡改的说明"
	if err := manifest.Verify(publicKey, "stable", "radxa-rock-3a", "arm64", 1024); err == nil {
		t.Fatal("Verify() error = nil, want tampered manifest rejection")
	}
}

func TestClientFetchAndDownloadVerifiedPackage(t *testing.T) {
	publicKey, privateKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	packageContent := []byte("verified-arm64-binary")
	var manifest Manifest
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/latest.json":
			if err := json.NewEncoder(w).Encode(manifest); err != nil {
				t.Error(err)
			}
		case "/teslabox":
			_, _ = w.Write(packageContent)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	manifest = signedTestManifest(t, privateKey, server.URL+"/teslabox", packageContent)
	client, err := NewClient(ClientConfig{
		ManifestURL: server.URL + "/latest.json", PublicKey: publicKey,
		Channel: "stable", Board: "radxa-rock-3a", Arch: "arm64",
		DownloadDir: t.TempDir(), MaximumSize: 1024, HTTPClient: server.Client(),
	})
	if err != nil {
		t.Fatal(err)
	}
	fetched, err := client.FetchManifest(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	var lastProgress int64
	path, err := client.Download(context.Background(), fetched, func(current, _ int64) { lastProgress = current })
	if err != nil {
		t.Fatal(err)
	}
	content, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(content) != string(packageContent) || lastProgress != int64(len(packageContent)) {
		t.Fatalf("download = %q, progress = %d", content, lastProgress)
	}
	if filepath.Base(path) != "teslabox-1.2.3-linux-arm64" {
		t.Fatalf("download path = %q", path)
	}
}

func TestParsePublicKey(t *testing.T) {
	publicKey, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	encoded := base64.StdEncoding.EncodeToString(publicKey)
	decoded, err := parsePublicKey(encoded)
	if err != nil || string(decoded) != string(publicKey) {
		t.Fatalf("parsePublicKey() = %x, %v", decoded, err)
	}
}

func TestVersionComparison(t *testing.T) {
	tests := []struct {
		candidate string
		current   string
		want      bool
	}{
		{candidate: "0.2.0", current: "0.1.9", want: true},
		{candidate: "1.0.0", current: "1.0.0", want: false},
		{candidate: "1.0.0", current: "2.0.0", want: false},
		{candidate: "0.1.0", current: "dev", want: true},
		{candidate: "0.1.7", current: "candidate-20260723-ui-layout", want: true},
	}
	for _, test := range tests {
		got, err := newerVersion(test.candidate, test.current)
		if err != nil {
			t.Fatalf("newerVersion(%q, %q) error = %v", test.candidate, test.current, err)
		}
		if got != test.want {
			t.Fatalf("newerVersion(%q, %q) = %v, want %v", test.candidate, test.current, got, test.want)
		}
	}
}

func TestNonStableCurrentVersionCanInstallSignedStableRelease(t *testing.T) {
	tests := []struct {
		name    string
		version string
		want    bool
	}{
		{name: "empty", version: "", want: true},
		{name: "development alias", version: "dev", want: true},
		{name: "legacy candidate", version: "candidate-20260723-ui-layout", want: true},
		{name: "prefixed test release", version: "v0.1.22-test.1", want: true},
		{name: "test release", version: "0.1.22-test.1", want: true},
		{name: "commit build", version: "build-441bb27", want: true},
		{name: "stable release", version: "0.1.22", want: false},
		{name: "stable release with whitespace", version: " 0.1.22 ", want: false},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := isNonStableVersion(test.version); got != test.want {
				t.Fatalf("isNonStableVersion(%q) = %v, want %v", test.version, got, test.want)
			}
			available, err := newerVersion("0.1.22", test.version)
			if err != nil {
				t.Fatalf("newerVersion(0.1.22, %q) error = %v", test.version, err)
			}
			if available != test.want {
				t.Fatalf("newerVersion(0.1.22, %q) = %v, want %v", test.version, available, test.want)
			}
		})
	}
}

func TestManagerManifestActionAllowsNonStableBuildToInstallStableRelease(t *testing.T) {
	manager := &Manager{config: Config{CurrentVersion: "v0.1.22-test.1"}}
	action, compatible := manager.manifestAction(Manifest{
		Version:    "0.1.22",
		MinVersion: "9.9.9",
	})
	if !compatible || action != "install" {
		t.Fatalf("manifestAction() = %q, %v, want install, true", action, compatible)
	}
}

func TestNewManagerIsDisabledWithoutTrustConfiguration(t *testing.T) {
	manager, err := New(Config{CurrentVersion: "0.1.0", CurrentCommit: "abc"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	status := manager.Status()
	if status.Configured || status.State != StateDisabled || status.CurrentVersion != "0.1.0" {
		t.Fatalf("Status() = %+v", status)
	}
}

func TestManagerStatusKeepsAvailableUpdateAfterPreviousInstall(t *testing.T) {
	resultPath := filepath.Join(t.TempDir(), "result.json")
	result := InstallResult{
		Version:   "0.1.0",
		Status:    "success",
		UpdatedAt: time.Now().Format(time.RFC3339),
	}
	content, err := json.Marshal(result)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(resultPath, content, 0o600); err != nil {
		t.Fatal(err)
	}
	manager := &Manager{
		config: Config{ResultPath: resultPath},
		state: Status{
			Configured:     true,
			CurrentVersion: "0.1.0",
			LatestVersion:  "0.1.1",
			State:          StateAvailable,
			Available:      true,
		},
	}

	status := manager.Status()
	if status.State != StateAvailable || !status.Available {
		t.Fatalf("Status() = %+v, want available update preserved", status)
	}
}

type fakeStarter struct {
	started atomic.Bool
}

func (f *fakeStarter) StartUpdate(context.Context) error {
	f.started.Store(true)
	return nil
}

func TestManagerCheckAllowsNonStableBuildToInstallStableRelease(t *testing.T) {
	publicKey, privateKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	packageContent := []byte("verified-package")
	var manifest Manifest
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/latest.json" {
			_ = json.NewEncoder(w).Encode(manifest)
			return
		}
		if r.URL.Path == "/teslabox" {
			_, _ = w.Write(packageContent)
			return
		}
		http.NotFound(w, r)
	}))
	defer server.Close()
	manifest = signedTestManifest(t, privateKey, server.URL+"/teslabox", packageContent)
	root := t.TempDir()
	starter := &fakeStarter{}
	manager, err := New(Config{
		ManifestURL: server.URL + "/latest.json",
		PublicKey:   base64.StdEncoding.EncodeToString(publicKey), Channel: "stable", Board: "radxa-rock-3a", Arch: "arm64",
		CurrentVersion: "v0.1.22-test.1", CurrentCommit: "test", DownloadDir: filepath.Join(root, "updates"),
		RequestPath: filepath.Join(root, "request.json"), ResultPath: filepath.Join(root, "result.json"), MaximumSize: 1024,
		AutoReboot: func() bool { return true },
	}, starter)
	if err != nil {
		t.Fatal(err)
	}
	manager.client.config.HTTPClient = server.Client()
	status, err := manager.Check(context.Background())
	if err != nil || !status.Available {
		t.Fatalf("Check() = %+v, %v", status, err)
	}
	if _, err := manager.StartDownload(); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(3 * time.Second)
	for manager.Status().State == StateDownloading && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if status = manager.Status(); status.State != StateDownloaded || status.ProgressPercent != 100 {
		t.Fatalf("download status = %+v", status)
	}
	status, err = manager.Install(context.Background())
	if err != nil || status.State != StateInstalling || !starter.started.Load() {
		t.Fatalf("Install() = %+v, %v, started=%v", status, err, starter.started.Load())
	}
	request, err := readInstallRequest(filepath.Join(root, "request.json"))
	if err != nil || request.SHA256 != manifest.SHA256 || request.PackageName != "teslabox-1.2.3-linux-arm64" || !request.RebootAfterInstall {
		t.Fatalf("install request = %+v, %v", request, err)
	}
}

func TestManagerSetChannelPersistsSelectionAndUsesBetaSource(t *testing.T) {
	publicKey, privateKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	packageContent := []byte("beta-package")
	var stableManifest, betaManifest Manifest
	server := httptest.NewTLSServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("Content-Type", "application/json")
		switch request.URL.Path {
		case "/stable/latest.json":
			_ = json.NewEncoder(writer).Encode(stableManifest)
		case "/beta/latest.json":
			_ = json.NewEncoder(writer).Encode(betaManifest)
		case "/teslabox":
			_, _ = writer.Write(packageContent)
		default:
			http.NotFound(writer, request)
		}
	}))
	defer server.Close()
	stableManifest = signedTestManifest(t, privateKey, server.URL+"/teslabox", packageContent)
	betaManifest = stableManifest
	betaManifest.Version = "1.2.4"
	betaManifest.Channel = "beta"
	if err := betaManifest.Sign(privateKey); err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	statePath := filepath.Join(root, "update-channel.json")
	manager, err := New(Config{
		ManifestURL: server.URL + "/stable/latest.json", CatalogURL: server.URL + "/stable/history.json",
		BetaManifestURL: server.URL + "/beta/latest.json", BetaCatalogURL: server.URL + "/beta/history.json",
		PublicKey: base64.StdEncoding.EncodeToString(publicKey), Channel: "stable", ChannelStatePath: statePath,
		Board: "radxa-rock-3a", Arch: "arm64", CurrentVersion: "1.2.3", DownloadDir: filepath.Join(root, "updates"),
		RequestPath: filepath.Join(root, "request.json"), ResultPath: filepath.Join(root, "result.json"), MaximumSize: 1024,
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, client := range manager.clients {
		client.config.HTTPClient = server.Client()
	}
	status, err := manager.SetChannel("beta")
	if err != nil || status.Channel != "beta" || !status.HistoryConfigured {
		t.Fatalf("SetChannel() = %+v, %v", status, err)
	}
	content, err := os.ReadFile(statePath)
	if err != nil || string(content) != "{\"channel\":\"beta\"}\n" {
		t.Fatalf("saved channel = %q, %v", content, err)
	}
	status, err = manager.Check(context.Background())
	if err != nil || !status.Available || status.LatestVersion != "1.2.4" || status.Channel != "beta" {
		t.Fatalf("beta Check() = %+v, %v", status, err)
	}
}

func TestManagerCheckReportsMissingBetaPreviewWithoutFailure(t *testing.T) {
	publicKey, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewTLSServer(http.NotFoundHandler())
	defer server.Close()
	root := t.TempDir()
	manager, err := New(Config{
		ManifestURL: server.URL + "/stable/latest.json", CatalogURL: server.URL + "/stable/history.json",
		BetaManifestURL: server.URL + "/beta/latest.json", BetaCatalogURL: server.URL + "/beta/history.json",
		PublicKey: base64.StdEncoding.EncodeToString(publicKey), Channel: "stable", Board: "radxa-rock-3a", Arch: "arm64",
		CurrentVersion: "1.2.3", DownloadDir: filepath.Join(root, "updates"), RequestPath: filepath.Join(root, "request.json"),
		ResultPath: filepath.Join(root, "result.json"), MaximumSize: 1024,
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, client := range manager.clients {
		client.config.HTTPClient = server.Client()
	}
	if _, err := manager.SetChannel("beta"); err != nil {
		t.Fatal(err)
	}
	status, err := manager.Check(context.Background())
	if err != nil || status.State != StateCurrent || status.Available || status.Message != "内测通道暂未发布可用版本。" {
		t.Fatalf("Check() = %+v, %v", status, err)
	}
	status, err = manager.SetChannel("stable")
	if err != nil || status.Channel != "stable" || status.Message != "" {
		t.Fatalf("SetChannel(stable) = %+v, %v", status, err)
	}
}

func signedTestManifest(t *testing.T, privateKey ed25519.PrivateKey, packageURL string, content []byte) Manifest {
	t.Helper()
	hash := sha256.Sum256(content)
	manifest := Manifest{
		SchemaVersion: 1, Version: "1.2.3", Channel: "stable",
		PublishedAt: "2026-07-21T10:00:00+08:00", Board: "radxa-rock-3a", Arch: "arm64",
		MinVersion: "0.1.0", PackageURL: packageURL, SHA256: hex.EncodeToString(hash[:]),
		Size: int64(len(content)), ReleaseNotes: "测试发布",
	}
	if err := manifest.Sign(privateKey); err != nil {
		t.Fatal(err)
	}
	return manifest
}
