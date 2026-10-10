package webtls

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"
)

func certificateFixture(t *testing.T, domain string, expiry time.Time) ([]byte, *x509.CertPool) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	root := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "test root"}, NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour), IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign}
	rootDER, err := x509.CreateCertificate(rand.Reader, root, root, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	root, err = x509.ParseCertificate(rootDER)
	if err != nil {
		t.Fatal(err)
	}
	leaf := &x509.Certificate{SerialNumber: big.NewInt(time.Now().UnixNano()), DNSNames: []string{domain}, NotBefore: time.Now().Add(-time.Hour), NotAfter: expiry, KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}}
	leafDER, err := x509.CreateCertificate(rand.Reader, leaf, root, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	keyDER, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	bundle := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: leafDER})
	bundle = append(bundle, pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER})...)
	roots := x509.NewCertPool()
	roots.AddCert(root)
	return bundle, roots
}

func TestCertificateValidationAndReplacement(t *testing.T) {
	const domain = "carplay.teslabox.dev"
	directory := t.TempDir()
	if err := os.Chmod(directory, 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(directory, "certificate.pem")
	bundle, roots := certificateFixture(t, domain, time.Now().Add(time.Hour))
	if err := os.WriteFile(path, bundle, 0o600); err != nil {
		t.Fatal(err)
	}
	first, err := loadCertificate(path, domain, roots, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := loadCertificate(path, "other.teslabox.dev", roots, time.Now()); err == nil {
		t.Fatal("accepted wrong domain")
	}
	if _, err := loadCertificate(path, domain, x509.NewCertPool(), time.Now()); err == nil {
		t.Fatal("accepted untrusted issuer")
	}
	if _, err := loadCertificate(path, domain, roots, time.Now().Add(2*time.Hour)); err == nil {
		t.Fatal("accepted expired certificate")
	}
	newBundle, newRoots := certificateFixture(t, domain, time.Now().Add(time.Hour))
	if err := os.WriteFile(path, newBundle, 0o600); err != nil {
		t.Fatal(err)
	}
	second, err := loadCertificate(path, domain, newRoots, time.Now())
	if err != nil || first.Leaf.SerialNumber.Cmp(second.Leaf.SerialNumber) == 0 {
		t.Fatalf("replacement not loaded: %v", err)
	}
	if err := os.WriteFile(path, append(newBundle, make([]byte, 128*1024)...), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := loadCertificate(path, domain, newRoots, time.Now()); err == nil {
		t.Fatal("accepted oversized bundle")
	}
}

func TestDomainAndHostIsolation(t *testing.T) {
	for _, domain := range []string{"teslabox.local", "192.168.88.1", "https://carplay.teslabox.dev", "carplay", "bad..dev"} {
		if err := ValidateDomain(domain); err == nil {
			t.Errorf("accepted %q", domain)
		}
	}
	server, err := NewServer(":443", "carplay.teslabox.dev", "/missing/certificate.pem", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusNoContent) }))
	if err != nil {
		t.Fatal(err)
	}
	for _, item := range []struct {
		host string
		want int
	}{{"carplay.teslabox.dev", 204}, {"carplay.teslabox.dev:443", 204}, {"teslabox.local", 421}} {
		r := httptest.NewRequest("GET", "https://"+item.host+"/", nil)
		w := httptest.NewRecorder()
		server.Handler.ServeHTTP(w, r)
		if w.Code != item.want {
			t.Errorf("host %s: %d", item.host, w.Code)
		}
	}
	if _, err := server.TLSConfig.GetCertificate(&tls.ClientHelloInfo{ServerName: "other.teslabox.dev"}); err == nil {
		t.Fatal("accepted wrong SNI")
	}
	if _, err := server.TLSConfig.GetCertificate(&tls.ClientHelloInfo{ServerName: "carplay.teslabox.dev"}); err == nil {
		t.Fatal("accepted missing certificate")
	}
}

func TestPrivateBundlePermissions(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("Linux ownership and file permissions")
	}
	directory := t.TempDir()
	if err := os.Chmod(directory, 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(directory, "certificate.pem")
	bundle, roots := certificateFixture(t, "carplay.teslabox.dev", time.Now().Add(time.Hour))
	if err := os.WriteFile(path, bundle, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := loadCertificate(path, "carplay.teslabox.dev", roots, time.Now()); err == nil {
		t.Fatal("accepted readable private key")
	}
	if err := os.Chmod(path, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(directory, 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := loadCertificate(path, "carplay.teslabox.dev", roots, time.Now()); err == nil {
		t.Fatal("accepted public directory")
	}
	if err := os.Chmod(directory, 0o700); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(directory, "linked.pem")
	if err := os.Symlink(path, link); err != nil {
		t.Fatal(err)
	}
	if _, err := loadCertificate(link, "carplay.teslabox.dev", roots, time.Now()); err == nil {
		t.Fatal("accepted symlink")
	}
}
