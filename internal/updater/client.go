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

const maximumManifestSize = 64 * 1024

// ClientConfig 固定远程更新的信任根、设备目标和资源上限。
type ClientConfig struct {
	ManifestURL string
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
	manifestURL, err := url.Parse(strings.TrimSpace(config.ManifestURL))
	if err != nil || !manifestURL.IsAbs() || manifestURL.Scheme != "https" || manifestURL.Host == "" || manifestURL.User != nil {
		return nil, errors.New("更新清单地址必须是无凭据的 HTTPS 地址")
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

// FetchManifest 下载并校验尺寸受限的最新版本清单。
func (c *Client) FetchManifest(ctx context.Context) (Manifest, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, c.config.ManifestURL, nil)
	if err != nil {
		return Manifest{}, fmt.Errorf("创建更新检查请求: %w", err)
	}
	response, err := c.config.HTTPClient.Do(request)
	if err != nil {
		return Manifest{}, fmt.Errorf("下载更新清单: %w", err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return Manifest{}, fmt.Errorf("更新服务器返回 HTTP %d", response.StatusCode)
	}
	if response.ContentLength > maximumManifestSize {
		return Manifest{}, errors.New("更新清单超过大小限制")
	}
	content, err := io.ReadAll(io.LimitReader(response.Body, maximumManifestSize+1))
	if err != nil {
		return Manifest{}, fmt.Errorf("读取更新清单: %w", err)
	}
	if len(content) > maximumManifestSize {
		return Manifest{}, errors.New("更新清单超过大小限制")
	}
	var manifest Manifest
	decoder := json.NewDecoder(strings.NewReader(string(content)))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&manifest); err != nil {
		return Manifest{}, fmt.Errorf("解析更新清单: %w", err)
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return Manifest{}, errors.New("更新清单包含多余内容")
	}
	if err := manifest.Verify(c.config.PublicKey, c.config.Channel, c.config.Board, c.config.Arch, c.config.MaximumSize); err != nil {
		return Manifest{}, err
	}
	return manifest, nil
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
