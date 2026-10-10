package certdelivery

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"encoding/asn1"
	"encoding/base64"
	"encoding/json"
	"math/big"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func syntheticMFI(t *testing.T, version uint64) MFIBundle {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	private, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	template := &x509.Certificate{SerialNumber: big.NewInt(1), NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour)}
	cert, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	info, err := asn1.Marshal(struct{ Type asn1.ObjectIdentifier }{asn1.ObjectIdentifier{1, 2, 840, 113549, 1, 7, 1}})
	if err != nil {
		t.Fatal(err)
	}
	signed, err := asn1.Marshal(struct {
		Version      int
		Digests      asn1.RawValue
		Content      asn1.RawValue
		Certificates asn1.RawValue `asn1:"tag:0"`
		Signers      asn1.RawValue
	}{1, asn1.RawValue{FullBytes: []byte{0x31, 0}}, asn1.RawValue{FullBytes: info}, asn1.RawValue{Class: 2, Tag: 0, IsCompound: true, Bytes: cert}, asn1.RawValue{FullBytes: []byte{0x31, 0}}})
	if err != nil {
		t.Fatal(err)
	}
	cms, err := asn1.Marshal(struct {
		Type asn1.ObjectIdentifier
		Data asn1.RawValue `asn1:"explicit,tag:0"`
	}{asn1.ObjectIdentifier{1, 2, 840, 113549, 1, 7, 2}, asn1.RawValue{Class: 2, Tag: 0, IsCompound: true, Bytes: signed}})
	if err != nil {
		t.Fatal(err)
	}
	return MFIBundle{Version: version, PrivateKey: private, Certificate: cms}
}

func TestMFIIndependentSignaturesAndMatchingKeys(t *testing.T) {
	public, key, _ := ed25519.GenerateKey(rand.Reader)
	bundle := syntheticMFI(t, 1)
	data, err := SignMFIBundle(bundle, key)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := verifyMFIEnvelope(data, public); err != nil {
		t.Fatal(err)
	}
	if _, err := verifyEnvelope(data, public); err == nil {
		t.Fatal("MFi publication accepted as HTTPS certificate")
	}
	web, _ := SignBundle(Bundle{1, ProductDomain, []byte("synthetic-pem")}, key)
	if _, err := verifyMFIEnvelope(web, public); err == nil {
		t.Fatal("HTTPS publication accepted as MFi identity")
	}
	other := syntheticMFI(t, 2)
	for _, test := range []struct {
		name   string
		bundle MFIBundle
	}{
		{"mismatched key", MFIBundle{1, other.PrivateKey, bundle.Certificate}},
		{"malformed CMS", MFIBundle{1, bundle.PrivateKey, make([]byte, 128)}},
		{"malformed PKCS8", MFIBundle{1, []byte("not-a-key"), bundle.Certificate}},
		{"trailing CMS bytes", MFIBundle{1, bundle.PrivateKey, append(bytes.Clone(bundle.Certificate), 0)}},
		{"zero version", MFIBundle{0, bundle.PrivateKey, bundle.Certificate}},
	} {
		t.Run(test.name, func(t *testing.T) {
			if _, err := SignMFIBundle(test.bundle, key); err == nil {
				t.Fatal("invalid identity published")
			}
		})
	}
}

func TestPublishMFICreatesPrivateOutputAndPreservesVersionOnInvalidIdentity(t *testing.T) {
	root := t.TempDir()
	if err := os.Chmod(root, 0o700); err != nil {
		t.Fatal(err)
	}
	identity := filepath.Join(root, "issuer")
	privateDirectory(identity)
	bundle := syntheticMFI(t, 1)
	writePrivate(filepath.Join(identity, "identity.pk8"), bundle.PrivateKey)
	writePrivate(filepath.Join(identity, "certificate.p7b"), bundle.Certificate)
	public, key, _ := ed25519.GenerateKey(rand.Reader)
	signing := filepath.Join(identity, "signing.key")
	writePrivate(signing, []byte(base64.StdEncoding.EncodeToString(key)))
	output := filepath.Join(root, "new-published-directory", "mfi.json")
	if err := PublishMFI(identity, signing, output); err != nil {
		t.Fatal(err)
	}
	before, err := readPrivate(output, maxEnvelope)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := verifyMFIEnvelope(before, public); err != nil {
		t.Fatal(err)
	}
	writePrivate(filepath.Join(identity, "identity.pk8"), []byte("broken"))
	if err := PublishMFI(identity, signing, output); err == nil {
		t.Fatal("invalid identity replaced publication")
	}
	after, _ := readPrivate(output, maxEnvelope)
	if !bytes.Equal(before, after) {
		t.Fatal("failed publication changed previous version")
	}
}

func TestMFIDirectoryRejectsRootAndUncleanPaths(t *testing.T) {
	_, client, _, _ := deliveryFixture(t)
	for _, directory := range []string{"/", "relative", "/var/lib/../mfi", "/var/lib/mfi/", client.config.IdentityPath, client.config.CertificatePath} {
		config := client.config
		config.MFIDirectory = directory
		if _, err := NewClient(config); err == nil {
			t.Fatalf("accepted MFi directory %q", directory)
		}
	}
}

func TestMFIDownloadReusesDeviceAndRevocationPreservesActiveIdentity(t *testing.T) {
	service, client, key, published := deliveryFixture(t)
	path := filepath.Join(filepath.Dir(published), "mfi.json")
	if err := service.EnableMFI(path); err != nil {
		t.Fatal(err)
	}
	client.config.MFIDirectory = filepath.Join(filepath.Dir(client.config.IdentityPath), "mfi")
	bundle := syntheticMFI(t, 1)
	data, err := SignMFIBundle(bundle, key)
	if err != nil {
		t.Fatal(err)
	}
	if err := writePrivate(path, data); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if _, err := client.Check(ctx); err != nil {
		t.Fatal(err)
	}
	if changed, err := client.CheckMFI(ctx); err != nil || !changed {
		t.Fatalf("MFi install: changed=%v err=%v", changed, err)
	}
	if changed, err := client.CheckMFI(ctx); err != nil || changed {
		t.Fatalf("MFi repeat: changed=%v err=%v", changed, err)
	}
	active := filepath.Join(client.config.MFIDirectory, "active.json")
	before, err := readPrivate(active, 4096)
	if err != nil {
		t.Fatal(err)
	}
	var selected mfiSelection
	if json.Unmarshal(before, &selected) != nil {
		t.Fatal("missing active identity")
	}
	private, err := readPrivate(filepath.Join(selected.CredentialsDirectory, "identity.pk8"), 16384)
	if err != nil || !bytes.Equal(private, bundle.PrivateKey) {
		t.Fatal("installed key mismatch")
	}
	clear(private)
	records, err := service.Devices()
	if err != nil || len(records) != 1 {
		t.Fatal("MFi created separate device identity")
	}
	if err := service.Revoke(records[0].ID, true); err != nil {
		t.Fatal(err)
	}
	if _, err := client.CheckMFI(ctx); err == nil {
		t.Fatal("revoked identity downloaded MFi")
	}
	after, _ := readPrivate(active, 4096)
	if !bytes.Equal(before, after) {
		t.Fatal("failed download changed active identity")
	}
}

func TestMFIAtomicSelectionRejectsRollbackAndIncompleteVersion(t *testing.T) {
	root := t.TempDir()
	if err := os.Chmod(root, 0o700); err != nil {
		t.Fatal(err)
	}
	directory := filepath.Join(root, "mfi")
	current := syntheticMFI(t, 2)
	if _, err := installMFI(directory, current); err != nil {
		t.Fatal(err)
	}
	active := filepath.Join(directory, "active.json")
	before, _ := readPrivate(active, 4096)
	if _, err := installMFI(directory, syntheticMFI(t, 1)); err == nil {
		t.Fatal("rollback accepted")
	}
	if _, err := installMFI(directory, syntheticMFI(t, 2)); err == nil {
		t.Fatal("same version changed identity")
	}
	var selection mfiSelection
	json.Unmarshal(before, &selection)
	if err := os.Remove(filepath.Join(selection.CredentialsDirectory, "certificate.p7b")); err != nil {
		t.Fatal(err)
	}
	if _, err := installMFI(directory, current); err == nil {
		t.Fatal("incomplete version accepted")
	}
	after, _ := readPrivate(active, 4096)
	if !bytes.Equal(before, after) {
		t.Fatal("failed installation replaced active selection")
	}
}

func TestMFIDamagedSelectionDoesNotReplaceCurrentIdentity(t *testing.T) {
	root := t.TempDir()
	if err := os.Chmod(root, 0o700); err != nil {
		t.Fatal(err)
	}
	directory := filepath.Join(root, "mfi")
	if _, err := installMFI(directory, syntheticMFI(t, 1)); err != nil {
		t.Fatal(err)
	}
	active := filepath.Join(directory, "active.json")
	for _, selection := range []mfiSelection{{Version: 1}, {1, filepath.Join(root, "outside"), strings.Repeat("a", 64)}} {
		data, _ := json.Marshal(selection)
		if err := writePrivate(active, data); err != nil {
			t.Fatal(err)
		}
		if _, err := installMFI(directory, syntheticMFI(t, 2)); err == nil {
			t.Fatal("damaged selection accepted")
		}
		after, _ := readPrivate(active, 4096)
		if !bytes.Equal(data, after) {
			t.Fatal("damaged selection overwritten")
		}
	}
}

func TestMFIDownloadChallengeCannotBeUsedForOtherResourceOrReplayed(t *testing.T) {
	service, client, key, published := deliveryFixture(t)
	path := filepath.Join(filepath.Dir(published), "mfi.json")
	service.EnableMFI(path)
	data, _ := SignMFIBundle(syntheticMFI(t, 1), key)
	writePrivate(path, data)
	if _, err := client.Check(context.Background()); err != nil {
		t.Fatal(err)
	}
	state, err := client.identity()
	if err != nil {
		t.Fatal(err)
	}
	defer clear(state.PrivateKey)
	device := ed25519.PrivateKey(state.PrivateKey)
	id := deviceID(device.Public().(ed25519.PublicKey))
	var challenge challenge
	if err := client.request(context.Background(), "/v1/challenge", map[string]string{"device_id": id, "resource": "mfi"}, &challenge, 4096); err != nil {
		t.Fatal(err)
	}
	proof := download{DeviceID: id, Nonce: challenge.Nonce, Proof: ed25519.Sign(device, scopedDownloadPayload("mfi", id, challenge.Nonce))}
	var envelope Envelope
	if err := client.request(context.Background(), "/v1/bundle", proof, &envelope, maxEnvelope); err == nil {
		t.Fatal("MFi proof authorized HTTPS download")
	}
	if err := client.request(context.Background(), "/v1/mfi-bundle", proof, &envelope, maxEnvelope); err != nil {
		t.Fatal(err)
	}
	clear(envelope.Payload)
	if err := client.request(context.Background(), "/v1/mfi-bundle", proof, &envelope, maxEnvelope); err == nil {
		t.Fatal("MFi download proof replay accepted")
	}
	request := httptest.NewRequest(http.MethodPost, "/v1/mfi-bundle", bytes.NewBufferString(`{"device_id":"unknown","nonce":"unknown","proof":""}`))
	response := httptest.NewRecorder()
	service.Handler().ServeHTTP(response, request)
	if response.Code != http.StatusForbidden {
		t.Fatalf("anonymous MFi status=%d", response.Code)
	}
}
