// Package certdelivery 实现统一网页证书的签名分发与独立设备下载身份。
package certdelivery

import (
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

const maxEnvelope = 256 * 1024

// ProductDomain 保持所有盒子的车机入口一致。
const ProductDomain = "carplay.teslabox.online"

// DefaultEndpoint 和 DefaultPublicKey 由维护者构建时注入，不要求产品用户配置。
var DefaultEndpoint = ""
var DefaultPublicKey = ""
var DefaultPublicAddress = ""

// Bundle 描述统一证书版本；私钥只通过受认证的 HTTPS 响应分发。
type Bundle struct {
	Version uint64 `json:"version"`
	Domain  string `json:"domain"`
	PEM     []byte `json:"pem"`
}

// Envelope 对精确负载字节签名，避免 JSON 重编码造成签名歧义。
type Envelope struct {
	Payload   []byte `json:"payload"`
	Signature []byte `json:"signature"`
}

// SignBundle 将续签结果转换为设备可验证的发布物，签名密钥不下发到盒子。
func SignBundle(bundle Bundle, key ed25519.PrivateKey) ([]byte, error) {
	if len(key) != ed25519.PrivateKeySize || bundle.Version == 0 || bundle.Domain != ProductDomain || len(bundle.PEM) == 0 || len(bundle.PEM) > 128*1024 {
		return nil, errors.New("统一证书发布参数无效")
	}
	payload, err := json.Marshal(bundle)
	if err != nil {
		return nil, err
	}
	return json.Marshal(Envelope{Payload: payload, Signature: ed25519.Sign(key, append([]byte("teslabox-certificate-bundle-v1\n"), payload...))})
}

func verifyEnvelope(data []byte, key ed25519.PublicKey) (Bundle, error) {
	var envelope Envelope
	var bundle Bundle
	if len(data) > maxEnvelope || len(key) != ed25519.PublicKeySize || json.Unmarshal(data, &envelope) != nil || !ed25519.Verify(key, append([]byte("teslabox-certificate-bundle-v1\n"), envelope.Payload...), envelope.Signature) {
		return bundle, errors.New("统一证书发布签名无效")
	}
	if json.Unmarshal(envelope.Payload, &bundle) != nil || bundle.Version == 0 || bundle.Domain != ProductDomain || len(bundle.PEM) == 0 || len(bundle.PEM) > 128*1024 {
		return Bundle{}, errors.New("统一证书负载无效")
	}
	return bundle, nil
}

func deviceID(key ed25519.PublicKey) string {
	digest := sha256.Sum256(key)
	return hex.EncodeToString(digest[:])
}

type registration struct {
	PublicKey []byte `json:"public_key"`
	Proof     []byte `json:"proof"`
}

type identity struct {
	PrivateKey []byte `json:"private_key"`
	Version    uint64 `json:"version"`
	MFIVersion uint64 `json:"mfi_version,omitempty"`
}

type download struct {
	DeviceID string `json:"device_id"`
	Nonce    string `json:"nonce"`
	Proof    []byte `json:"proof"`
}

type challenge struct {
	Nonce   string    `json:"nonce"`
	Expires time.Time `json:"expires"`
}

func registrationPayload(public []byte) []byte {
	return []byte("teslabox-certificate-registration-v1\n" + base64.StdEncoding.EncodeToString(public))
}

func downloadPayload(id, nonce string) []byte {
	return []byte(fmt.Sprintf("teslabox-certificate-download-v1\n%s\n%s\n%s", ProductDomain, id, nonce))
}

func scopedDownloadPayload(resource, id, nonce string) []byte {
	if resource == "mfi" {
		return []byte(fmt.Sprintf("teslabox-mfi-download-v1\n%s\n%s", id, nonce))
	}
	return downloadPayload(id, nonce)
}
