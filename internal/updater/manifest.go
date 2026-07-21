// Package updater 实现只接受签名发布物的 TeslaBox 在线更新流程。
package updater

import (
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"
)

const manifestSchemaVersion = 1

var stableVersionPattern = regexp.MustCompile(`^(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)$`)

// Manifest 描述一个经过 Ed25519 签名的单文件 ARM64 更新。
type Manifest struct {
	SchemaVersion int    `json:"schema_version"`
	Version       string `json:"version"`
	Channel       string `json:"channel"`
	PublishedAt   string `json:"published_at"`
	Board         string `json:"board"`
	Arch          string `json:"arch"`
	MinVersion    string `json:"min_version"`
	PackageURL    string `json:"package_url"`
	SHA256        string `json:"sha256"`
	Size          int64  `json:"size"`
	ReleaseNotes  string `json:"release_notes"`
	Signature     string `json:"signature"`
}

// Sign 使用离线保存的发布私钥签署清单；设备端和仓库中都不应保存该私钥。
func (m *Manifest) Sign(privateKey ed25519.PrivateKey) error {
	if len(privateKey) != ed25519.PrivateKeySize {
		return errors.New("Ed25519 私钥无效")
	}
	m.Signature = base64.StdEncoding.EncodeToString(ed25519.Sign(privateKey, m.signingPayload()))
	return nil
}

// Verify 验证清单字段、目标设备和发布签名，防止下载源被替换后执行任意程序。
func (m Manifest) Verify(publicKey ed25519.PublicKey, channel, board, arch string, maximumSize int64) error {
	if m.SchemaVersion != manifestSchemaVersion {
		return fmt.Errorf("不支持的更新清单版本 %d", m.SchemaVersion)
	}
	if _, err := parseStableVersion(m.Version); err != nil {
		return fmt.Errorf("版本号无效: %w", err)
	}
	if m.MinVersion != "" {
		if _, err := parseStableVersion(m.MinVersion); err != nil {
			return fmt.Errorf("最低版本号无效: %w", err)
		}
	}
	if m.Channel != channel || m.Board != board || m.Arch != arch {
		return errors.New("更新包与当前通道或设备不匹配")
	}
	for _, value := range []string{m.Channel, m.PublishedAt, m.Board, m.Arch, m.MinVersion, m.PackageURL, m.SHA256} {
		if strings.ContainsAny(value, "\r\n") {
			return errors.New("更新清单字段包含非法换行")
		}
	}
	if _, err := time.Parse(time.RFC3339, m.PublishedAt); err != nil {
		return errors.New("发布时间格式无效")
	}
	parsedURL, err := url.Parse(m.PackageURL)
	if err != nil || !parsedURL.IsAbs() || !strings.EqualFold(parsedURL.Scheme, "https") || parsedURL.Host == "" || parsedURL.User != nil {
		return errors.New("更新包地址必须是无凭据的 HTTPS 地址")
	}
	if m.Size <= 0 || m.Size > maximumSize {
		return errors.New("更新包大小超出允许范围")
	}
	if len(m.SHA256) != sha256.Size*2 {
		return errors.New("更新包 SHA-256 长度无效")
	}
	if _, err := hex.DecodeString(m.SHA256); err != nil || m.SHA256 != strings.ToLower(m.SHA256) {
		return errors.New("更新包 SHA-256 格式无效")
	}
	if len(publicKey) != ed25519.PublicKeySize {
		return errors.New("更新公钥无效")
	}
	signature, err := base64.StdEncoding.DecodeString(m.Signature)
	if err != nil || len(signature) != ed25519.SignatureSize {
		return errors.New("更新签名格式无效")
	}
	if !ed25519.Verify(publicKey, m.signingPayload(), signature) {
		return errors.New("更新清单签名校验失败")
	}
	return nil
}

func (m Manifest) signingPayload() []byte {
	notesHash := sha256.Sum256([]byte(m.ReleaseNotes))
	values := []string{
		"teslabox-update-manifest-v1",
		strconv.Itoa(m.SchemaVersion), m.Version, m.Channel, m.PublishedAt,
		m.Board, m.Arch, m.MinVersion, m.PackageURL, m.SHA256,
		strconv.FormatInt(m.Size, 10), hex.EncodeToString(notesHash[:]),
	}
	return []byte(strings.Join(values, "\n"))
}

func parsePublicKey(value string) (ed25519.PublicKey, error) {
	decoded, err := base64.StdEncoding.DecodeString(strings.TrimSpace(value))
	if err != nil || len(decoded) != ed25519.PublicKeySize {
		return nil, errors.New("TESLABOX_UPDATE_PUBLIC_KEY 必须是 Base64 Ed25519 公钥")
	}
	return ed25519.PublicKey(decoded), nil
}

func newerVersion(candidate, current string) (bool, error) {
	candidateParts, err := parseStableVersion(candidate)
	if err != nil {
		return false, err
	}
	if current == "dev" || current == "" {
		return true, nil
	}
	currentParts, err := parseStableVersion(current)
	if err != nil {
		return false, err
	}
	for index := range candidateParts {
		if candidateParts[index] != currentParts[index] {
			return candidateParts[index] > currentParts[index], nil
		}
	}
	return false, nil
}

func parseStableVersion(value string) ([3]uint64, error) {
	matches := stableVersionPattern.FindStringSubmatch(strings.TrimSpace(value))
	if matches == nil {
		return [3]uint64{}, errors.New("仅支持 x.y.z 格式的稳定版本")
	}
	var result [3]uint64
	for index := range result {
		parsed, err := strconv.ParseUint(matches[index+1], 10, 64)
		if err != nil {
			return [3]uint64{}, errors.New("版本号超出范围")
		}
		result[index] = parsed
	}
	return result, nil
}
