package certdelivery

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"time"

	"github.com/wen-ryon/teslabox/internal/webtls"
)

// GenerateSigningKey 仅在维护者指定的私有目录生成发布密钥，拒绝覆盖已有密钥。
func GenerateSigningKey(path string) (string, error) {
	if err := privateDirectory(filepath.Dir(path)); err != nil {
		return "", err
	}
	public, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return "", err
	}
	defer clear(private)
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return "", err
	}
	defer file.Close()
	data := []byte(base64.StdEncoding.EncodeToString(private))
	defer clear(data)
	if _, err := file.Write(data); err != nil {
		return "", err
	}
	if err := file.Sync(); err != nil {
		return "", err
	}
	return base64.StdEncoding.EncodeToString(public), nil
}

// Publish 校验 CA 信任与密钥匹配后发布续签结果，失败时保留原发布物。
func Publish(bundlePath, signingKeyPath, outputPath string) error {
	pem, err := readPrivate(bundlePath, 128*1024)
	if err != nil {
		return err
	}
	defer clear(pem)
	keyBytes, err := readPrivate(signingKeyPath, 1024)
	if err != nil {
		return err
	}
	defer clear(keyBytes)
	key, err := base64.StdEncoding.DecodeString(string(keyBytes))
	if err != nil || len(key) != ed25519.PrivateKeySize {
		return errors.New("证书发布签名密钥无效")
	}
	defer clear(key)
	if err := privateDirectory(filepath.Dir(outputPath)); err != nil {
		return err
	}
	directory, err := os.MkdirTemp(filepath.Dir(outputPath), ".validation-*")
	if err != nil {
		return err
	}
	defer os.RemoveAll(directory)
	if _, err := webtls.InstallBundle(filepath.Join(directory, "bundle.pem"), ProductDomain, pem); err != nil {
		return err
	}
	version := uint64(time.Now().UnixNano())
	if previous, err := readPrivate(outputPath, maxEnvelope); err == nil {
		defer clear(previous)
		old, err := verifyEnvelope(previous, ed25519.PrivateKey(key).Public().(ed25519.PublicKey))
		if err != nil {
			return errors.New("当前证书发布物签名无效，拒绝覆盖")
		}
		defer clear(old.PEM)
		if version <= old.Version {
			return errors.New("证书发布时间未推进，拒绝回退")
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	data, err := SignBundle(Bundle{Version: version, Domain: ProductDomain, PEM: pem}, key)
	if err != nil {
		return err
	}
	defer clear(data)
	return writePrivate(outputPath, data)
}

// DeviceListJSON 只输出维护者查看所需字段，不输出公钥或任何私钥。
func DeviceListJSON(records []DeviceRecord) ([]byte, error) {
	type summary struct {
		ID            string    `json:"id"`
		RegisteredAt  time.Time `json:"registered_at"`
		LastRequestAt time.Time `json:"last_request_at"`
		Revoked       bool      `json:"revoked"`
	}
	result := make([]summary, 0, len(records))
	for _, record := range records {
		result = append(result, summary{record.ID, record.RegisteredAt, record.LastRequestAt, record.Revoked})
	}
	return json.MarshalIndent(result, "", "  ")
}
