package certdelivery

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func deliveryFixture(t *testing.T) (*DistributionServer, *Client, ed25519.PrivateKey, string) {
	t.Helper()
	directory := t.TempDir()
	if err := os.Chmod(directory, 0o700); err != nil {
		t.Fatal(err)
	}
	public, key, _ := ed25519.GenerateKey(rand.Reader)
	published := filepath.Join(directory, "published", "bundle.json")
	data, _ := SignBundle(Bundle{Version: 1, Domain: ProductDomain, PEM: []byte("test-pem")}, key)
	if err := writePrivate(published, data); err != nil {
		t.Fatal(err)
	}
	service, err := NewDistributionServer(filepath.Join(directory, "devices"), published, public)
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewTLSServer(service.Handler())
	t.Cleanup(server.Close)
	client, err := NewClient(ClientConfig{Endpoint: server.URL, PublicKey: base64.StdEncoding.EncodeToString(public), IdentityPath: filepath.Join(directory, "client", "identity.json"), CertificatePath: filepath.Join(directory, "client", "certificate.pem")})
	if err != nil {
		t.Fatal(err)
	}
	client.http.Transport = server.Client().Transport
	// 证书链与热替换由 webtls 独立测试；此处验证网络、身份、签名与防回退的完整流程。
	client.install = func(path, domain string, pem []byte) (bool, error) {
		if domain != ProductDomain {
			t.Fatal("wrong domain")
		}
		previous, _ := os.ReadFile(path)
		return !bytes.Equal(previous, pem), writePrivate(path, pem)
	}
	return service, client, key, published
}

func TestRegistrationDownloadAndRevocation(t *testing.T) {
	service, client, _, _ := deliveryFixture(t)
	if changed, err := client.Check(context.Background()); err != nil || !changed {
		t.Fatalf("first download: %v %v", changed, err)
	}
	records, err := service.Devices()
	if err != nil || len(records) != 1 || records[0].LastRequestAt.IsZero() {
		t.Fatalf("records: %v %v", records, err)
	}
	id := records[0].ID
	if changed, err := client.Check(context.Background()); err != nil || changed {
		t.Fatalf("repeat download: %v %v", changed, err)
	}
	if err := service.Revoke(id, true); err != nil {
		t.Fatal(err)
	}
	if _, err := client.Check(context.Background()); err == nil {
		t.Fatal("revoked device downloaded")
	}
	if err := service.Revoke(id, false); err != nil {
		t.Fatal(err)
	}
	if _, err := client.Check(context.Background()); err != nil {
		t.Fatal(err)
	}
	records, _ = service.Devices()
	if len(records) != 1 || records[0].ID != id {
		t.Fatal("device identity changed")
	}
	view, _ := DeviceListJSON(records)
	if bytes.Contains(view, []byte("private_key")) || bytes.Contains(view, []byte("public_key")) || bytes.Contains(view, []byte("online")) {
		t.Fatal("unwanted device fields")
	}
}

func TestTamperingAndRollbackPreserveInstalledBundle(t *testing.T) {
	_, client, key, path := deliveryFixture(t)
	ctx := context.Background()
	if _, err := client.Check(ctx); err != nil {
		t.Fatal(err)
	}
	newer, _ := SignBundle(Bundle{Version: 2, Domain: ProductDomain, PEM: []byte("new-pem")}, key)
	writePrivate(path, newer)
	if _, err := client.Check(ctx); err != nil {
		t.Fatal(err)
	}
	older, _ := SignBundle(Bundle{Version: 1, Domain: ProductDomain, PEM: []byte("old-pem")}, key)
	writePrivate(path, older)
	if _, err := client.Check(ctx); err == nil {
		t.Fatal("accepted rollback")
	}
	var envelope Envelope
	json.Unmarshal(newer, &envelope)
	envelope.Payload[0] ^= 1
	corrupt, _ := json.Marshal(envelope)
	writePrivate(path, corrupt)
	if _, err := client.Check(ctx); err == nil {
		t.Fatal("accepted tampered bundle")
	}
	actual, _ := os.ReadFile(client.config.CertificatePath)
	if string(actual) != "new-pem" {
		t.Fatal("failed update destroyed installed bundle")
	}
}

func TestEnvelopeRejectsWrongSignerAndDomain(t *testing.T) {
	public, key, _ := ed25519.GenerateKey(rand.Reader)
	data, err := SignBundle(Bundle{Version: 1, Domain: ProductDomain, PEM: []byte("test-pem")}, key)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := verifyEnvelope(data, public); err != nil {
		t.Fatal(err)
	}
	other, _, _ := ed25519.GenerateKey(rand.Reader)
	if _, err := verifyEnvelope(data, other); err == nil {
		t.Fatal("accepted wrong signer")
	}
	var envelope Envelope
	json.Unmarshal(data, &envelope)
	envelope.Payload[0] ^= 1
	data, _ = json.Marshal(envelope)
	if _, err := verifyEnvelope(data, public); err == nil {
		t.Fatal("accepted modified payload")
	}
	if _, err := SignBundle(Bundle{Version: 1, Domain: "other.teslabox.online", PEM: []byte("test")}, key); err == nil {
		t.Fatal("accepted wrong product domain")
	}
	if _, err := verifyEnvelope(make([]byte, maxEnvelope+1), public); err == nil {
		t.Fatal("accepted oversized envelope")
	}
}

func TestUnavailableServerPreservesExistingIdentityAndCertificate(t *testing.T) {
	_, client, _, _ := deliveryFixture(t)
	if _, err := client.Check(context.Background()); err != nil {
		t.Fatal(err)
	}
	beforeIdentity, _ := os.ReadFile(client.config.IdentityPath)
	beforeCertificate, _ := os.ReadFile(client.config.CertificatePath)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := client.Check(ctx); err == nil {
		t.Fatal("cancelled check succeeded")
	}
	afterIdentity, _ := os.ReadFile(client.config.IdentityPath)
	afterCertificate, _ := os.ReadFile(client.config.CertificatePath)
	if !bytes.Equal(beforeIdentity, afterIdentity) || !bytes.Equal(beforeCertificate, afterCertificate) {
		t.Fatal("network failure changed saved identity or certificate")
	}
}

func TestChallengesAreBoundToDeviceAndSingleUse(t *testing.T) {
	service, client, _, _ := deliveryFixture(t)
	ctx := context.Background()
	if _, err := client.Check(ctx); err != nil {
		t.Fatal(err)
	}
	state, _ := client.identity()
	key := ed25519.PrivateKey(state.PrivateKey)
	id := deviceID(key.Public().(ed25519.PublicKey))
	var challenge challenge
	client.request(ctx, "/v1/challenge", map[string]string{"device_id": id}, &challenge, 4096)
	request := download{DeviceID: id, Nonce: challenge.Nonce, Proof: ed25519.Sign(key, downloadPayload(id, challenge.Nonce))}
	var envelope Envelope
	wrong := request
	wrong.Proof = make([]byte, ed25519.SignatureSize)
	if client.request(ctx, "/v1/bundle", wrong, &envelope, maxEnvelope) == nil {
		t.Fatal("accepted wrong proof")
	}
	if err := client.request(ctx, "/v1/bundle", request, &envelope, maxEnvelope); err != nil {
		t.Fatal(err)
	}
	if client.request(ctx, "/v1/bundle", request, &envelope, maxEnvelope) == nil {
		t.Fatal("accepted replay")
	}
	client.request(ctx, "/v1/challenge", map[string]string{"device_id": id}, &challenge, 4096)
	service.mu.Lock()
	service.pending[challenge.Nonce] = pendingChallenge{device: id, expires: time.Now().Add(-time.Second)}
	service.mu.Unlock()
	request.Nonce = challenge.Nonce
	request.Proof = ed25519.Sign(key, downloadPayload(id, request.Nonce))
	if client.request(ctx, "/v1/bundle", request, &envelope, maxEnvelope) == nil {
		t.Fatal("accepted expired challenge")
	}
}

func TestClientRefusesRedirectAndInvalidConfiguration(t *testing.T) {
	_, client, _, _ := deliveryFixture(t)
	destination := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { t.Error("redirect reached destination") }))
	defer destination.Close()
	origin := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { http.Redirect(w, r, destination.URL, 307) }))
	defer origin.Close()
	client.config.Endpoint = origin.URL
	client.http.Transport = origin.Client().Transport
	if _, err := client.Check(context.Background()); err == nil {
		t.Fatal("accepted redirect")
	}
	for _, endpoint := range []string{"http://example.com", "https://user:pass@example.com", "https://example.com/path", "https://example.com/?token=secret", "https://" + ProductDomain} {
		config := client.config
		config.Endpoint = endpoint
		if _, err := NewClient(config); err == nil {
			t.Fatalf("accepted %s", endpoint)
		}
	}
}
