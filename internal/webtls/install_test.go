package webtls

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestInstallPreservesCertificateOnInvalidDownload(t *testing.T) {
	directory := t.TempDir()
	os.Chmod(directory, 0o700)
	path := filepath.Join(directory, "certificate.pem")
	const domain = "carplay.teslabox.online"
	now := time.Now()
	first, roots := certificateFixture(t, domain, now.Add(time.Hour))
	changed, err := installBundle(path, domain, first, roots, now)
	if err != nil || !changed {
		t.Fatalf("first install: %v %v", changed, err)
	}
	for _, data := range [][]byte{[]byte("invalid"), append(first, make([]byte, 128*1024)...)} {
		if _, err := installBundle(path, domain, data, roots, now); err == nil {
			t.Fatal("accepted invalid certificate")
		}
		actual, err := os.ReadFile(path)
		if err != nil || !bytes.Equal(actual, first) {
			t.Fatal("invalid download replaced current certificate")
		}
	}
	if changed, err := installBundle(path, domain, first, roots, now); changed || err != nil {
		t.Fatalf("identical install: %v %v", changed, err)
	}
	if _, err := installBundle(path, domain, first, roots, now.Add(2*time.Hour)); err == nil {
		t.Fatal("accepted expired certificate")
	}
	other, otherRoots := certificateFixture(t, "other.teslabox.online", now.Add(time.Hour))
	if _, err := installBundle(path, domain, other, otherRoots, now); err == nil {
		t.Fatal("accepted wrong domain")
	}
	if _, err := installBundle(path, domain, first, otherRoots, now); err == nil {
		t.Fatal("accepted untrusted certificate")
	}
}

func TestInstallLoadsReplacementWithoutRestart(t *testing.T) {
	directory := t.TempDir()
	os.Chmod(directory, 0o700)
	path := filepath.Join(directory, "certificate.pem")
	now := time.Now()
	first, _ := certificateFixture(t, "carplay.teslabox.online", now.Add(30*time.Minute))
	if err := os.WriteFile(path, first, 0o600); err != nil {
		t.Fatal(err)
	}
	second, roots := certificateFixture(t, "carplay.teslabox.online", now.Add(time.Hour))
	if changed, err := installBundle(path, "carplay.teslabox.online", second, roots, now); err != nil || !changed {
		t.Fatalf("replacement: %v %v", changed, err)
	}
	if _, err := loadCertificate(path, "carplay.teslabox.online", roots, now); err != nil {
		t.Fatal(err)
	}
}
