package main

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/wen-ryon/teslabox/internal/updater"
)

func TestCreateCatalog(t *testing.T) {
	publicKey, privateKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	directory := t.TempDir()
	privateKeyPath := filepath.Join(directory, "private.key")
	if err := os.WriteFile(privateKeyPath, []byte(base64.StdEncoding.EncodeToString(privateKey)), 0o600); err != nil {
		t.Fatal(err)
	}
	manifest := updater.Manifest{
		SchemaVersion: 1, Version: "0.1.7", Channel: "stable", PublishedAt: time.Now().UTC().Format(time.RFC3339),
		Board: "radxa-rock-3a", Arch: "arm64", MinVersion: "0.1.0",
		PackageURL: "https://updates.example.test/teslabox-linux-arm64",
		SHA256:     strings.Repeat("0", 64), Size: 1,
	}
	if err := manifest.Sign(privateKey); err != nil {
		t.Fatal(err)
	}
	manifestPath := filepath.Join(directory, "manifest-0.1.7.json")
	content, err := json.Marshal(manifest)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(manifestPath, content, 0o600); err != nil {
		t.Fatal(err)
	}
	output := filepath.Join(directory, "history.json")
	if err := createCatalog(privateKeyPath, output, "stable", "radxa-rock-3a", manifestPaths{manifestPath}); err != nil {
		t.Fatal(err)
	}
	generated, err := os.ReadFile(output)
	if err != nil {
		t.Fatal(err)
	}
	var catalog updater.Catalog
	if err := json.Unmarshal(generated, &catalog); err != nil {
		t.Fatal(err)
	}
	if err := catalog.Verify(publicKey, "stable", "radxa-rock-3a", "arm64", 128*1024*1024); err != nil {
		t.Fatalf("Verify() error = %v", err)
	}
}
