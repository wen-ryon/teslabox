package updater

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"testing"
	"time"
)

func TestCatalogVerifyAcceptsSignedEntries(t *testing.T) {
	publicKey, privateKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	manifest := testManifest(t, privateKey, "0.1.6")
	catalog := Catalog{
		SchemaVersion: catalogSchemaVersion,
		Channel:       "stable",
		GeneratedAt:   time.Now().UTC().Format(time.RFC3339),
		Manifests:     []Manifest{manifest},
	}
	if err := catalog.Sign(privateKey); err != nil {
		t.Fatal(err)
	}
	if err := catalog.Verify(publicKey, "stable", "radxa-rock-3a", "arm64", 128*1024*1024); err != nil {
		t.Fatalf("Verify() error = %v", err)
	}
}

func TestCatalogVerifyRejectsModifiedOrder(t *testing.T) {
	publicKey, privateKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	first := testManifest(t, privateKey, "0.1.5")
	second := testManifest(t, privateKey, "0.1.6")
	catalog := Catalog{
		SchemaVersion: catalogSchemaVersion,
		Channel:       "stable",
		GeneratedAt:   time.Now().UTC().Format(time.RFC3339),
		Manifests:     []Manifest{first, second},
	}
	if err := catalog.Sign(privateKey); err != nil {
		t.Fatal(err)
	}
	catalog.Manifests[0], catalog.Manifests[1] = catalog.Manifests[1], catalog.Manifests[0]
	if err := catalog.Verify(publicKey, "stable", "radxa-rock-3a", "arm64", 128*1024*1024); err == nil {
		t.Fatal("Verify() unexpectedly accepted reordered catalog")
	}
}

func testManifest(t *testing.T, privateKey ed25519.PrivateKey, version string) Manifest {
	t.Helper()
	digest := sha256.Sum256([]byte(version))
	manifest := Manifest{
		SchemaVersion: manifestSchemaVersion,
		Version:       version,
		Channel:       "stable",
		PublishedAt:   time.Now().UTC().Format(time.RFC3339),
		Board:         "radxa-rock-3a",
		Arch:          "arm64",
		MinVersion:    "0.1.0",
		PackageURL:    "https://updates.example.test/teslabox-linux-arm64",
		SHA256:        hex.EncodeToString(digest[:]),
		Size:          4096,
		ReleaseNotes:  "test release",
	}
	if err := manifest.Sign(privateKey); err != nil {
		t.Fatal(err)
	}
	return manifest
}
