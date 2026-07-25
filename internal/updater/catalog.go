package updater

import (
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"
)

const (
	catalogSchemaVersion  = 1
	maximumCatalogEntries = 24
)

// Catalog 是维护者签名的历史发布目录；每个条目仍会独立校验其更新清单签名。
type Catalog struct {
	SchemaVersion int        `json:"schema_version"`
	Channel       string     `json:"channel"`
	GeneratedAt   string     `json:"generated_at"`
	Manifests     []Manifest `json:"manifests"`
	Signature     string     `json:"signature"`
}

// Sign 使用离线发布私钥签署整个目录，避免设备接受被服务器篡改、删改或重排的历史版本列表。
func (c *Catalog) Sign(privateKey ed25519.PrivateKey) error {
	if len(privateKey) != ed25519.PrivateKeySize {
		return errors.New("Ed25519 私钥无效")
	}
	c.Signature = base64.StdEncoding.EncodeToString(ed25519.Sign(privateKey, c.signingPayload()))
	return nil
}

// Verify 校验目录身份、发布目标和每个条目的独立签名，防止历史版本入口弱化在线更新信任边界。
func (c Catalog) Verify(publicKey ed25519.PublicKey, channel, board, arch string, maximumSize int64) error {
	if c.SchemaVersion != catalogSchemaVersion {
		return fmt.Errorf("不支持的历史版本目录格式 %d", c.SchemaVersion)
	}
	if strings.TrimSpace(c.Channel) != channel {
		return errors.New("历史版本目录通道与设备不匹配")
	}
	if _, err := time.Parse(time.RFC3339, c.GeneratedAt); err != nil {
		return fmt.Errorf("历史版本目录生成时间无效: %w", err)
	}
	if len(c.Manifests) == 0 || len(c.Manifests) > maximumCatalogEntries {
		return fmt.Errorf("历史版本目录条目数必须在 1 到 %d 之间", maximumCatalogEntries)
	}
	seen := make(map[string]struct{}, len(c.Manifests))
	for _, manifest := range c.Manifests {
		if _, exists := seen[manifest.Version]; exists {
			return errors.New("历史版本目录包含重复版本")
		}
		if err := manifest.Verify(publicKey, channel, board, arch, maximumSize); err != nil {
			return fmt.Errorf("历史版本 %s 无效: %w", manifest.Version, err)
		}
		seen[manifest.Version] = struct{}{}
	}
	signature, err := base64.StdEncoding.DecodeString(c.Signature)
	if err != nil || len(signature) != ed25519.SignatureSize {
		return errors.New("历史版本目录签名格式无效")
	}
	if !ed25519.Verify(publicKey, c.signingPayload(), signature) {
		return errors.New("历史版本目录签名校验失败")
	}
	return nil
}

func (c Catalog) signingPayload() []byte {
	values := []string{
		"teslabox-update-catalog-v1",
		strconv.Itoa(c.SchemaVersion),
		c.Channel,
		c.GeneratedAt,
	}
	for _, manifest := range c.Manifests {
		digest := sha256.Sum256(manifest.signingPayload())
		values = append(values, hex.EncodeToString(digest[:]))
	}
	return []byte(strings.Join(values, "\n"))
}

// Release 是管理页面可展示的一个已验签历史版本，不携带远程下载地址或本地路径。
type Release struct {
	Version      string `json:"version"`
	PublishedAt  string `json:"published_at"`
	ReleaseNotes string `json:"release_notes,omitempty"`
	Action       string `json:"action"`
}

// ReleaseHistory 表示历史版本目录的可用状态和经本机兼容性筛选后的候选版本。
type ReleaseHistory struct {
	Configured  bool      `json:"configured"`
	GeneratedAt string    `json:"generated_at,omitempty"`
	Releases    []Release `json:"releases"`
}
