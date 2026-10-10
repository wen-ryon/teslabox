package certdelivery

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/sha256"
	"crypto/x509"
	"encoding/asn1"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// MFIBundle 通过独立签名域分发软件 v3 身份，不能与网页证书发布物互换。
type MFIBundle struct {
	Version     uint64 `json:"version"`
	PrivateKey  []byte `json:"private_key"`
	Certificate []byte `json:"certificate"`
}

const mfiBundlePrefix = "teslabox-mfi-bundle-v1\n"

func validateMFI(bundle MFIBundle) error {
	invalid := errors.New("软件 MFi v3 身份格式或公私钥匹配无效")
	if bundle.Version == 0 || len(bundle.PrivateKey) == 0 || len(bundle.PrivateKey) > 16384 || len(bundle.Certificate) < 128 || len(bundle.Certificate) > 2048 {
		return invalid
	}
	parsed, err := x509.ParsePKCS8PrivateKey(bundle.PrivateKey)
	if err != nil {
		return invalid
	}
	key, ok := parsed.(*ecdsa.PrivateKey)
	if !ok || key.Curve != elliptic.P256() {
		return invalid
	}
	var content struct {
		Type asn1.ObjectIdentifier
		Data asn1.RawValue `asn1:"explicit,tag:0"`
	}
	if rest, err := asn1.Unmarshal(bundle.Certificate, &content); err != nil || len(rest) != 0 || !content.Type.Equal(asn1.ObjectIdentifier{1, 2, 840, 113549, 1, 7, 2}) {
		return invalid
	}
	var signed struct {
		Version      int
		Digests      asn1.RawValue
		Content      asn1.RawValue
		Certificates asn1.RawValue `asn1:"optional,tag:0"`
		CRLs         asn1.RawValue `asn1:"optional,tag:1"`
		Signers      asn1.RawValue
	}
	if rest, err := asn1.Unmarshal(content.Data.Bytes, &signed); err != nil || len(rest) != 0 || signed.Certificates.Class != 2 || signed.Certificates.Tag != 0 {
		return invalid
	}
	var certificate asn1.RawValue
	if rest, err := asn1.Unmarshal(signed.Certificates.Bytes, &certificate); err != nil || len(rest) != 0 {
		return invalid
	}
	cert, err := x509.ParseCertificate(certificate.FullBytes)
	if err != nil {
		return invalid
	}
	public, ok := cert.PublicKey.(*ecdsa.PublicKey)
	if !ok || !public.Equal(&key.PublicKey) {
		return invalid
	}
	// 此处验证编码和密钥一致性；Apple 信任与实机认证由原生接收端验收。
	return nil
}

// SignMFIBundle 在私有发布流程中校验身份后签名，不把身份写入公开更新附件。
func SignMFIBundle(bundle MFIBundle, key ed25519.PrivateKey) ([]byte, error) {
	if len(key) != ed25519.PrivateKeySize {
		return nil, errors.New("MFi 发布签名密钥无效")
	}
	if err := validateMFI(bundle); err != nil {
		return nil, err
	}
	payload, err := json.Marshal(bundle)
	if err != nil {
		return nil, err
	}
	defer clear(payload)
	return json.Marshal(Envelope{Payload: payload, Signature: ed25519.Sign(key, append([]byte(mfiBundlePrefix), payload...))})
}

func verifyMFIEnvelope(data []byte, key ed25519.PublicKey) (MFIBundle, error) {
	var envelope Envelope
	if len(data) > maxEnvelope || len(key) != ed25519.PublicKeySize || json.Unmarshal(data, &envelope) != nil {
		return MFIBundle{}, errors.New("MFi 发布物无效")
	}
	defer clear(envelope.Payload)
	if !ed25519.Verify(key, append([]byte(mfiBundlePrefix), envelope.Payload...), envelope.Signature) {
		return MFIBundle{}, errors.New("MFi 发布签名无效")
	}
	var bundle MFIBundle
	if json.Unmarshal(envelope.Payload, &bundle) != nil || validateMFI(bundle) != nil {
		clear(bundle.PrivateKey)
		return MFIBundle{}, errors.New("MFi 发布身份无效")
	}
	return bundle, nil
}

type mfiSelection struct {
	Version              uint64 `json:"version"`
	CredentialsDirectory string `json:"credentials_directory"`
	CertificateSHA256    string `json:"certificate_sha256"`
}

func installMFI(directory string, bundle MFIBundle) (bool, error) {
	if err := validateMFI(bundle); err != nil {
		return false, err
	}
	if err := privateDirectory(directory); err != nil {
		return false, err
	}
	digest := sha256.Sum256(bundle.Certificate)
	hash := hex.EncodeToString(digest[:])
	versions := filepath.Join(directory, "versions")
	if err := privateDirectory(versions); err != nil {
		return false, err
	}
	target := filepath.Join(versions, fmt.Sprintf("%d-%s", bundle.Version, hash))
	selection := mfiSelection{bundle.Version, target, hash}
	active := filepath.Join(directory, "active.json")
	if data, err := readPrivate(active, 4096); err == nil {
		var previous mfiSelection
		if json.Unmarshal(data, &previous) != nil || previous.Version == 0 || len(previous.CertificateSHA256) != sha256.Size*2 || previous.CertificateSHA256 != strings.ToLower(previous.CertificateSHA256) {
			return false, errors.New("当前 MFi 发布状态无效，保留身份")
		}
		if _, err := hex.DecodeString(previous.CertificateSHA256); err != nil || previous.CredentialsDirectory != filepath.Join(versions, fmt.Sprintf("%d-%s", previous.Version, previous.CertificateSHA256)) {
			return false, errors.New("当前 MFi 发布路径无效，保留身份")
		}
		if previous.Version > bundle.Version {
			return false, errors.New("MFi 发布版本回退被拒绝")
		}
		if previous.Version == bundle.Version && previous != selection {
			return false, errors.New("同版本 MFi 身份改变被拒绝")
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return false, err
	}
	if _, err := os.Lstat(target); errors.Is(err, os.ErrNotExist) {
		stage, err := os.MkdirTemp(versions, ".mfi-*")
		if err != nil {
			return false, err
		}
		defer os.RemoveAll(stage)
		if err := writePrivate(filepath.Join(stage, "identity.pk8"), bundle.PrivateKey); err != nil {
			return false, err
		}
		if err := writePrivate(filepath.Join(stage, "certificate.p7b"), bundle.Certificate); err != nil {
			return false, err
		}
		if err := os.Rename(stage, target); err != nil {
			return false, err
		}
		parent, err := os.Open(versions)
		if err != nil {
			return false, err
		}
		err = syncDirectory(parent)
		parent.Close()
		if err != nil {
			return false, err
		}
	} else if err != nil {
		return false, err
	}
	// 已存在目录必须匹配全部内容，不能只认目录名或摘要标记。
	for name, expected := range map[string][]byte{"identity.pk8": bundle.PrivateKey, "certificate.p7b": bundle.Certificate} {
		actual, err := readPrivate(filepath.Join(target, name), 16384)
		if err != nil {
			return false, err
		}
		match := bytes.Equal(actual, expected)
		clear(actual)
		if !match {
			return false, errors.New("已有 MFi 版本内容不一致")
		}
	}
	data, err := json.Marshal(selection)
	if err != nil {
		return false, err
	}
	previous, _ := readPrivate(active, 4096)
	if string(previous) == string(data) {
		return false, nil
	}
	return true, writePrivate(active, data)
}

// EnableMFI 在监听前配置私有发布路径，与网页证书共用设备登记和停用控制。
func (s *DistributionServer) EnableMFI(path string) error {
	if !filepath.IsAbs(path) || filepath.Clean(path) != path || path == s.bundlePath {
		return errors.New("MFi 发布路径无效")
	}
	s.mfiBundlePath = path
	return nil
}

// PublishMFI 仅在维护者私有目录读取身份，发布失败不会覆盖现有版本。
func PublishMFI(directory, signingKeyPath, outputPath string) error {
	key, err := readPrivate(filepath.Join(directory, "identity.pk8"), 16384)
	if err != nil {
		return err
	}
	defer clear(key)
	cert, err := readPrivate(filepath.Join(directory, "certificate.p7b"), 2048)
	if err != nil {
		return err
	}
	encoded, err := readPrivate(signingKeyPath, 1024)
	if err != nil {
		return err
	}
	defer clear(encoded)
	signing, err := base64.StdEncoding.DecodeString(string(encoded))
	if err != nil || len(signing) != ed25519.PrivateKeySize {
		return errors.New("MFi 发布签名密钥无效")
	}
	defer clear(signing)
	if err := privateDirectory(filepath.Dir(outputPath)); err != nil {
		return err
	}
	version := uint64(time.Now().UnixNano())
	if data, err := readPrivate(outputPath, maxEnvelope); err == nil {
		defer clear(data)
		previous, err := verifyMFIEnvelope(data, ed25519.PrivateKey(signing).Public().(ed25519.PublicKey))
		if err != nil {
			return err
		}
		clear(previous.PrivateKey)
		if version <= previous.Version {
			return errors.New("MFi 发布时间未推进")
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	data, err := SignMFIBundle(MFIBundle{version, key, cert}, signing)
	if err != nil {
		return err
	}
	defer clear(data)
	return writePrivate(outputPath, data)
}

// CheckMFI 复用设备下载身份，先校验签名与密钥匹配，再原子选择完整身份版本。
func (c *Client) CheckMFI(ctx context.Context) (bool, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.config.MFIDirectory == "" {
		return false, errors.New("软件 MFi 下载未配置")
	}
	state, err := c.identity()
	if err != nil {
		return false, err
	}
	defer clear(state.PrivateKey)
	envelope, err := c.downloadEnvelope(ctx, ed25519.PrivateKey(state.PrivateKey), "mfi", "/v1/mfi-bundle")
	if err != nil {
		return false, err
	}
	defer clear(envelope.Payload)
	data, err := json.Marshal(envelope)
	if err != nil {
		return false, err
	}
	defer clear(data)
	bundle, err := verifyMFIEnvelope(data, c.publicKey)
	if err != nil {
		return false, err
	}
	defer clear(bundle.PrivateKey)
	if bundle.Version < state.MFIVersion {
		return false, errors.New("软件 MFi 发布版本回退被拒绝")
	}
	changed, err := installMFI(c.config.MFIDirectory, bundle)
	if err != nil {
		return false, err
	}
	state.MFIVersion = bundle.Version
	if err := c.saveIdentity(state); err != nil {
		return changed, errors.New("软件 MFi 发布版本保存失败")
	}
	return changed, nil
}
