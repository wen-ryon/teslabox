package updater

import (
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"
)

const (
	maximumManifestSize = 64 * 1024
	maximumCatalogSize  = 512 * 1024
)

// ClientConfig 固定远程更新的信任根、设备目标和资源上限。
type ClientConfig struct {
	ManifestURL string
	CatalogURL  string
	PublicKey   ed25519.PublicKey
	Channel     string
	Board       string
	Arch        string
	DownloadDir string
	MaximumSize int64
	HTTPClient  *http.Client
}

// Client 只负责获取已签名清单并把匹配哈希的发布物原子落盘。
type Client struct {
	config ClientConfig
}

// NewClient 创建带 HTTPS 跳转约束和超时的更新客户端。
func NewClient(config ClientConfig) (*Client, error) {
	config.ManifestURL = strings.TrimSpace(config.ManifestURL)
	config.CatalogURL = strings.TrimSpace(config.CatalogURL)
	if err := validateHTTPSURL(config.ManifestURL, "更新清单地址"); err != nil {
		return nil, err
	}
	if strings.TrimSpace(config.CatalogURL) != "" {
		if err := validateHTTPSURL(config.CatalogURL, "历史版本目录地址"); err != nil {
			return nil, err
		}
	}
	if len(config.PublicKey) != ed25519.PublicKeySize {
		return nil, errors.New("更新公钥无效")
	}
	if config.Channel == "" || config.Board == "" || config.Arch == "" {
		return nil, errors.New("更新通道、板型和架构不能为空")
	}
	if !filepath.IsAbs(config.DownloadDir) || filepath.Clean(config.DownloadDir) == string(filepath.Separator) {
		return nil, errors.New("更新下载目录必须是非根目录的绝对路径")
	}
	if config.MaximumSize <= 0 {
		return nil, errors.New("更新包大小上限必须大于零")
	}
	if config.HTTPClient == nil {
		config.HTTPClient = &http.Client{
			Timeout: 30 * time.Minute,
			CheckRedirect: func(request *http.Request, via []*http.Request) error {
				if len(via) >= 5 || request.URL.Scheme != "https" || request.URL.User != nil {
					return errors.New("更新地址发生了不安全的跳转")
				}
				return nil
			},
		}
	}
	return &Client{config: config}, nil
}

func validateHTTPSURL(value, label string) error {
	parsed, err := url.Parse(strings.TrimSpace(value))
	if err != nil || !parsed.IsAbs() || parsed.Scheme != "https" || parsed.Host == "" || parsed.User != nil {
		return fmt.Errorf("%s 必须是无凭据的 HTTPS 地址", label)
	}
	return nil
}

// FetchManifest 下载并校验尺寸受限的最新版本清单。
func (c *Client) FetchManifest(ctx context.Context) (Manifest, error) {
	content, err := c.fetchJSON(ctx, c.config.ManifestURL, maximumManifestSize, "更新清单")
	if err != nil {
		return Manifest{}, err
	}
	var manifest Manifest
	if err := decodeStrictJSON(content, &manifest); err != nil {
		return Manifest{}, fmt.Errorf("解析更新清单: %w", err)
	}
	if err := manifest.Verify(c.config.PublicKey, c.config.Channel, c.config.Board, c.config.Arch, c.config.MaximumSize); err != nil {
		return Manifest{}, err
	}
	return manifest, nil
}

// HasCatalog 表示设备已配置维护者签名的历史版本目录。
func (c *Client) HasCatalog() bool {
	return strings.TrimSpace(c.config.CatalogURL) != ""
}

// FetchCatalog 下载并验证历史版本目录及其所有独立清单。
func (c *Client) FetchCatalog(ctx context.Context) (Catalog, error) {
	if !c.HasCatalog() {
		return Catalog{}, errors.New("历史版本目录尚未配置")
	}
	content, err := c.fetchJSON(ctx, c.config.CatalogURL, maximumCatalogSize, "历史版本目录")
	if err != nil {
		return Catalog{}, err
	}
	var catalog Catalog
	if err := decodeStrictJSON(content, &catalog); err != nil {
		return Catalog{}, fmt.Errorf("解析历史版本目录: %w", err)
	}
	if err := catalog.Verify(c.config.PublicKey, c.config.Channel, c.config.Board, c.config.Arch, c.config.MaximumSize); err != nil {
		return Catalog{}, err
	}
	return catalog, nil
}

func (c *Client) fetchJSON(ctx context.Context, source string, limit int64, label string) ([]byte, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, source, nil)
	if err != nil {
		return nil, fmt.Errorf("创建 %s 请求: %w", label, err)
	}
	response, err := c.config.HTTPClient.Do(request)
	if err != nil {
		return nil, fmt.Errorf("下载 %s: %w", label, err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("更新服务器返回 HTTP %d", response.StatusCode)
	}
	if response.ContentLength > limit {
		return nil, fmt.Errorf("%s 超过大小限制", label)
	}
	content, err := io.ReadAll(io.LimitReader(response.Body, limit+1))
	if err != nil {
		return nil, fmt.Errorf("读取 %s: %w", label, err)
	}
	if int64(len(content)) > limit {
		return nil, fmt.Errorf("%s 超过大小限制", label)
	}
	return content, nil
}

func decodeStrictJSON(content []byte, value any) error {
	decoder := json.NewDecoder(strings.NewReader(string(content)))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(value); err != nil {
		return err
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return errors.New("包含多余内容")
	}
	return nil
}

// Download 下载签名清单指向的程序，并在原子改名前验证长度和 SHA-256。
func (c *Client) Download(ctx context.Context, manifest Manifest, progress func(int64, int64)) (string, error) {
	if err := manifest.Verify(c.config.PublicKey, c.config.Channel, c.config.Board, c.config.Arch, c.config.MaximumSize); err != nil {
		return "", err
	}
	if err := os.MkdirAll(c.config.DownloadDir, 0o700); err != nil {
		return "", fmt.Errorf("创建更新目录: %w", err)
	}
	if err := os.Chmod(c.config.DownloadDir, 0o700); err != nil {
		return "", fmt.Errorf("限制更新目录权限: %w", err)
	}
	c.removeStaleDownloads()
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, manifest.PackageURL, nil)
	if err != nil {
		return "", fmt.Errorf("创建更新下载请求: %w", err)
	}
	response, err := c.config.HTTPClient.Do(request)
	if err != nil {
		return "", fmt.Errorf("下载更新包: %w", err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return "", fmt.Errorf("更新服务器返回 HTTP %d", response.StatusCode)
	}
	if response.ContentLength >= 0 && response.ContentLength != manifest.Size {
		return "", errors.New("更新包长度与签名清单不一致")
	}
	name := "teslabox-" + strings.TrimPrefix(manifest.Version, "v") + "-linux-arm64"
	finalPath := filepath.Join(c.config.DownloadDir, name)
	temporary, err := os.CreateTemp(c.config.DownloadDir, ".download-*")
	if err != nil {
		return "", fmt.Errorf("创建更新临时文件: %w", err)
	}
	temporaryPath := temporary.Name()
	defer os.Remove(temporaryPath)
	if err := temporary.Chmod(0o600); err != nil {
		temporary.Close()
		return "", fmt.Errorf("限制更新文件权限: %w", err)
	}
	hash := sha256.New()
	reader := &progressReader{reader: io.LimitReader(response.Body, manifest.Size+1), total: manifest.Size, progress: progress}
	written, copyErr := io.Copy(io.MultiWriter(temporary, hash), reader)
	if copyErr != nil {
		temporary.Close()
		return "", fmt.Errorf("保存更新包: %w", copyErr)
	}
	if written != manifest.Size {
		temporary.Close()
		return "", errors.New("更新包长度与签名清单不一致")
	}
	if actual := hex.EncodeToString(hash.Sum(nil)); actual != manifest.SHA256 {
		temporary.Close()
		return "", errors.New("更新包 SHA-256 校验失败")
	}
	if err := temporary.Sync(); err != nil {
		temporary.Close()
		return "", fmt.Errorf("同步更新包: %w", err)
	}
	if err := temporary.Close(); err != nil {
		return "", fmt.Errorf("关闭更新包: %w", err)
	}
	if err := os.Rename(temporaryPath, finalPath); err != nil {
		return "", fmt.Errorf("发布已验证更新包: %w", err)
	}
	return finalPath, nil
}

func (c *Client) removeStaleDownloads() {
	entries, err := os.ReadDir(c.config.DownloadDir)
	if err != nil {
		return
	}
	for _, entry := range entries {
		if entry.Type().IsRegular() && strings.HasPrefix(entry.Name(), ".download-") {
			_ = os.Remove(filepath.Join(c.config.DownloadDir, entry.Name()))
		}
	}
}

type progressReader struct {
	reader   io.Reader
	read     int64
	total    int64
	progress func(int64, int64)
}

func (r *progressReader) Read(value []byte) (int, error) {
	count, err := r.reader.Read(value)
	r.read += int64(count)
	if r.progress != nil {
		r.progress(r.read, r.total)
	}
	return count, err
}
