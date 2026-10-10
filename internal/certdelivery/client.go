package certdelivery

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math/big"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/wen-ryon/teslabox/internal/webtls"
)

// ClientConfig 由产品安装与构建配置提供，管理页面不要求用户填写证书参数。
type ClientConfig struct {
	Endpoint        string
	PublicKey       string
	IdentityPath    string
	CertificatePath string
	MFIDirectory    string
}

// Client 在后台登记和更新证书，网络错误不得阻塞主程序启动。
type Client struct {
	mu        sync.Mutex
	config    ClientConfig
	publicKey ed25519.PublicKey
	http      *http.Client
	install   func(string, string, []byte) (bool, error)
}

// NewClient 独立验证分发服务 HTTPS 地址与固定签名公钥，禁止重定向下载凭据。
func NewClient(config ClientConfig) (*Client, error) {
	parsed, err := url.Parse(config.Endpoint)
	if err != nil || parsed.Scheme != "https" || parsed.Host == "" || parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" || (parsed.Path != "" && parsed.Path != "/") || strings.EqualFold(parsed.Hostname(), ProductDomain) {
		return nil, errors.New("证书分发地址必须是独立的无凭据 HTTPS 服务地址")
	}
	key, err := base64.StdEncoding.DecodeString(config.PublicKey)
	if err != nil || len(key) != ed25519.PublicKeySize || !(filepath.IsAbs(config.IdentityPath) || path.IsAbs(config.IdentityPath)) || !(filepath.IsAbs(config.CertificatePath) || path.IsAbs(config.CertificatePath)) || config.IdentityPath == config.CertificatePath {
		return nil, errors.New("证书分发签名公钥或文件配置无效")
	}
	config.Endpoint = strings.TrimRight(config.Endpoint, "/")
	if config.MFIDirectory != "" && (!safeMFIDirectory(config.MFIDirectory) || config.MFIDirectory == config.IdentityPath || config.MFIDirectory == config.CertificatePath) {
		return nil, errors.New("MFi 下载目录配置无效")
	}
	return &Client{config: config, publicKey: key, install: webtls.InstallBundle, http: &http.Client{Timeout: 10 * time.Second, CheckRedirect: func(_ *http.Request, _ []*http.Request) error { return errors.New("证书分发请求禁止重定向") }}}, nil
}

func safeMFIDirectory(directory string) bool {
	// Windows 构建机也必须能验证 Linux 产品路径，不能用本机分隔符改写目标路径。
	if path.IsAbs(directory) {
		return path.Clean(directory) == directory && directory != "/" && !strings.Contains(directory, "\\")
	}
	return filepath.IsAbs(directory) && filepath.Clean(directory) == directory && filepath.Dir(directory) != directory
}

func (c *Client) identity() (identity, error) {
	var state identity
	if err := privateDirectory(filepath.Dir(c.config.IdentityPath)); err != nil {
		return state, err
	}
	data, err := readPrivate(c.config.IdentityPath, 4096)
	if errors.Is(err, os.ErrNotExist) {
		_, key, err := ed25519.GenerateKey(rand.Reader)
		if err != nil {
			return state, errors.New("无法生成设备下载身份")
		}
		state.PrivateKey = key
		return state, c.saveIdentity(state)
	}
	if err != nil {
		return state, errors.New("设备下载身份不可用")
	}
	defer clear(data)
	if json.Unmarshal(data, &state) != nil || len(state.PrivateKey) != ed25519.PrivateKeySize || !bytes.Equal(ed25519.NewKeyFromSeed(state.PrivateKey[:ed25519.SeedSize]), state.PrivateKey) {
		return identity{}, errors.New("设备下载身份格式无效")
	}
	return state, nil
}

func (c *Client) saveIdentity(state identity) error {
	data, err := json.Marshal(state)
	if err != nil {
		return err
	}
	defer clear(data)
	return writePrivate(c.config.IdentityPath, data)
}

func (c *Client) request(ctx context.Context, path string, body any, result any, limit int64) error {
	data, err := json.Marshal(body)
	if err != nil {
		return err
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, c.config.Endpoint+path, bytes.NewReader(data))
	if err != nil {
		return errors.New("证书分发请求创建失败")
	}
	request.Header.Set("Content-Type", "application/json")
	response, err := c.http.Do(request)
	if err != nil {
		return errors.New("证书分发服务暂时无法连接")
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return fmt.Errorf("证书分发服务返回状态 %d", response.StatusCode)
	}
	data, err = io.ReadAll(io.LimitReader(response.Body, limit+1))
	defer clear(data)
	if err != nil || int64(len(data)) > limit {
		return errors.New("证书分发响应读取失败或超出大小限制")
	}
	if json.Unmarshal(data, result) != nil {
		return errors.New("证书分发响应格式无效")
	}
	return nil
}

// Check 使用一次性挑战验证设备身份，再验证发布签名和证书后替换本地文件。
func (c *Client) Check(ctx context.Context) (bool, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	state, err := c.identity()
	if err != nil {
		return false, err
	}
	defer clear(state.PrivateKey)
	envelope, err := c.downloadEnvelope(ctx, ed25519.PrivateKey(state.PrivateKey), "", "/v1/bundle")
	if err != nil {
		return false, err
	}
	defer clear(envelope.Payload)
	data, err := json.Marshal(envelope)
	if err != nil {
		return false, err
	}
	defer clear(data)
	bundle, err := verifyEnvelope(data, c.publicKey)
	if err != nil {
		return false, err
	}
	defer clear(bundle.PEM)
	if bundle.Version < state.Version {
		return false, errors.New("统一证书版本回退被拒绝")
	}
	changed, err := c.install(c.config.CertificatePath, bundle.Domain, bundle.PEM)
	if err != nil {
		return changed, err
	}
	state.Version = bundle.Version
	if err := c.saveIdentity(state); err != nil {
		return changed, errors.New("设备证书版本保存失败")
	}
	return changed, nil
}

// Run 只维护证书与软件认证身份，无独立心跳；网络错误不阻塞设备启动。
func (c *Client) Run(ctx context.Context, logger *slog.Logger) {
	for {
		if ctx.Err() != nil {
			return
		}
		checkCtx, cancel := context.WithTimeout(ctx, 35*time.Second)
		changed, err := c.Check(checkCtx)
		cancel()
		retry := err != nil
		if err != nil && ctx.Err() == nil {
			logger.Warn("统一 HTTPS 证书更新未完成，保留当前证书", "error", err)
		} else if changed {
			logger.Info("统一 HTTPS 证书已更新，无需重启服务")
		}
		if c.config.MFIDirectory != "" {
			mfiCtx, mfiCancel := context.WithTimeout(ctx, 35*time.Second)
			mfiChanged, mfiErr := c.CheckMFI(mfiCtx)
			mfiCancel()
			if mfiErr != nil && ctx.Err() == nil {
				logger.Warn("软件 MFi 身份更新未完成，保留当前身份", "error", mfiErr)
				retry = true
			} else if mfiChanged {
				logger.Info("软件 MFi v3 身份已准备，当前手机会话保持")
			}
		}
		if ctx.Err() != nil {
			return
		}
		delay := 6 * time.Hour
		if retry {
			delay = time.Minute
		}
		// 抖动只用于证书后台检查，避免同时开机的设备集中请求服务端。
		if jitter, err := rand.Int(rand.Reader, big.NewInt(int64(delay/10))); err == nil {
			delay += time.Duration(jitter.Int64())
		}
		timer := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
		}
	}
}

func (c *Client) downloadEnvelope(ctx context.Context, key ed25519.PrivateKey, resource, route string) (Envelope, error) {
	public := key.Public().(ed25519.PublicKey)
	id := deviceID(public)
	// 重复登记是幂等操作，服务器重装后也能自动恢复；已停用身份不能重新登记绕过。
	var registered struct {
		DeviceID string `json:"device_id"`
	}
	if err := c.request(ctx, "/v1/register", registration{PublicKey: public, Proof: ed25519.Sign(key, registrationPayload(public))}, &registered, 4096); err != nil {
		return Envelope{}, err
	}
	if registered.DeviceID != id {
		return Envelope{}, errors.New("设备登记身份不匹配")
	}
	var challenge challenge
	request := map[string]string{"device_id": id}
	if resource != "" {
		request["resource"] = resource
	}
	if err := c.request(ctx, "/v1/challenge", request, &challenge, 4096); err != nil {
		return Envelope{}, err
	}
	if len(challenge.Nonce) != 64 || !time.Now().Before(challenge.Expires) || challenge.Expires.After(time.Now().Add(2*time.Minute)) {
		return Envelope{}, errors.New("设备下载挑战无效或过期")
	}
	var envelope Envelope
	if err := c.request(ctx, route, download{DeviceID: id, Nonce: challenge.Nonce, Proof: ed25519.Sign(key, scopedDownloadPayload(resource, id, challenge.Nonce))}, &envelope, maxEnvelope); err != nil {
		return Envelope{}, err
	}
	return envelope, nil
}
