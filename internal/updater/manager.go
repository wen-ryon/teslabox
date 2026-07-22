package updater

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"
)

// State 是设置页可稳定展示的更新阶段。
type State string

const (
	StateDisabled    State = "disabled"
	StateIdle        State = "idle"
	StateChecking    State = "checking"
	StateAvailable   State = "available"
	StateCurrent     State = "current"
	StateDownloading State = "downloading"
	StateDownloaded  State = "downloaded"
	StateInstalling  State = "installing"
	StateFailed      State = "failed"
)

// Status 不暴露板端文件路径或内部命令输出，只返回管理页面需要的信息。
type Status struct {
	Configured      bool   `json:"configured"`
	CurrentVersion  string `json:"current_version"`
	CurrentCommit   string `json:"current_commit"`
	Channel         string `json:"channel"`
	State           State  `json:"state"`
	LatestVersion   string `json:"latest_version,omitempty"`
	PublishedAt     string `json:"published_at,omitempty"`
	ReleaseNotes    string `json:"release_notes,omitempty"`
	Available       bool   `json:"available"`
	ProgressPercent int    `json:"progress_percent"`
	DownloadedBytes int64  `json:"downloaded_bytes,omitempty"`
	TotalBytes      int64  `json:"total_bytes,omitempty"`
	Error           string `json:"error,omitempty"`
	LastCheckedAt   string `json:"last_checked_at,omitempty"`
	InstallResult   string `json:"install_result,omitempty"`
}

// Config 定义在线更新功能的本地策略，私钥永远不属于设备配置。
type Config struct {
	ManifestURL    string
	PublicKey      string
	Channel        string
	Board          string
	Arch           string
	CurrentVersion string
	CurrentCommit  string
	DownloadDir    string
	RequestPath    string
	ResultPath     string
	MaximumSize    int64
	Logger         *slog.Logger
	CanInstall     func() error
}

// ServiceStarter 只允许启动预先安装的固定 systemd 单元，避免把远程字段拼进命令。
type ServiceStarter interface {
	StartUpdate(context.Context) error
}

// Manager 串行化检查、下载和安装请求，并为页面保存可轮询状态。
type Manager struct {
	config      Config
	client      *Client
	starter     ServiceStarter
	logger      *slog.Logger
	stateMu     sync.RWMutex
	state       Status
	manifest    *Manifest
	packagePath string
	opMu        sync.Mutex
}

// New 创建更新管理器；未同时配置清单地址和公钥时功能保持禁用而不阻止主服务启动。
func New(config Config, starter ServiceStarter) (*Manager, error) {
	if config.Channel == "" {
		config.Channel = "stable"
	}
	if config.Board == "" {
		config.Board = "radxa-rock-3a"
	}
	if config.Arch == "" {
		config.Arch = runtime.GOARCH
	}
	if config.MaximumSize == 0 {
		config.MaximumSize = 128 * 1024 * 1024
	}
	if strings.ContainsAny(config.Channel+config.Board+config.Arch, "/\\\t\r\n") {
		return nil, errors.New("更新通道、板型或架构标识无效")
	}
	if config.Logger == nil {
		config.Logger = slog.Default()
	}
	manager := &Manager{config: config, starter: starter, logger: config.Logger}
	manager.state = Status{
		Configured: false, CurrentVersion: config.CurrentVersion, CurrentCommit: config.CurrentCommit,
		Channel: config.Channel, State: StateDisabled,
	}
	manifestURL := strings.TrimSpace(config.ManifestURL)
	publicKeyValue := strings.TrimSpace(config.PublicKey)
	if manifestURL == "" && publicKeyValue == "" {
		return manager, nil
	}
	if manifestURL == "" || publicKeyValue == "" {
		return nil, errors.New("更新清单地址和 Ed25519 公钥必须同时配置")
	}
	if !filepath.IsAbs(config.RequestPath) || !filepath.IsAbs(config.ResultPath) {
		return nil, errors.New("更新请求和结果路径必须是绝对路径")
	}
	publicKey, err := parsePublicKey(publicKeyValue)
	if err != nil {
		return nil, err
	}
	client, err := NewClient(ClientConfig{
		ManifestURL: manifestURL, PublicKey: publicKey, Channel: config.Channel,
		Board: config.Board, Arch: config.Arch, DownloadDir: config.DownloadDir,
		MaximumSize: config.MaximumSize,
	})
	if err != nil {
		return nil, err
	}
	manager.client = client
	manager.state.Configured = true
	manager.state.State = StateIdle
	return manager, nil
}

// Status 返回线程安全的更新快照，并合并独立安装助手留下的最终结果。
func (m *Manager) Status() Status {
	m.stateMu.RLock()
	status := m.state
	m.stateMu.RUnlock()
	if m.config.ResultPath != "" {
		if content, err := os.ReadFile(m.config.ResultPath); err == nil {
			var result InstallResult
			decodeErr := json.Unmarshal(content, &result)
			updatedAt, timeErr := time.Parse(time.RFC3339, result.UpdatedAt)
			age := time.Since(updatedAt)
			if decodeErr == nil && timeErr == nil && age >= 0 && age <= 7*24*time.Hour {
				status.InstallResult = result.Status
				// 只在进程尚未执行更新操作时恢复上次结果，避免旧结果覆盖本次检查或下载状态。
				if status.State == StateIdle {
					if status.Configured && result.Status == "success" && result.Version == status.CurrentVersion {
						status.State = StateCurrent
					}
					if result.Status == "failed" {
						status.State = StateFailed
						status.Error = result.Message
					}
				}
			}
		}
	}
	return status
}

// Check 获取并验签最新清单，签名失败时不会保留清单供后续下载。
func (m *Manager) Check(ctx context.Context) (Status, error) {
	if m.client == nil {
		return m.Status(), errors.New("在线更新尚未配置")
	}
	if !m.opMu.TryLock() {
		return m.Status(), errors.New("已有更新操作正在进行")
	}
	defer m.opMu.Unlock()
	m.updateState(func(status *Status) {
		status.State, status.Error = StateChecking, ""
	})
	checkCtx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	manifest, err := m.client.FetchManifest(checkCtx)
	if err != nil {
		m.fail("检查更新失败", err)
		return m.Status(), err
	}
	available, err := newerVersion(manifest.Version, m.config.CurrentVersion)
	if err == nil && manifest.MinVersion != "" && m.config.CurrentVersion != "dev" {
		var meetsMinimum bool
		meetsMinimum, err = versionAtLeast(m.config.CurrentVersion, manifest.MinVersion)
		if err == nil && !meetsMinimum {
			err = errors.New("当前版本过旧，不能直接安装此更新")
		}
	}
	if err != nil {
		m.fail("比较更新版本失败", err)
		return m.Status(), err
	}
	m.manifest = &manifest
	m.packagePath = ""
	now := time.Now().Format(time.RFC3339)
	m.updateState(func(status *Status) {
		status.State = StateCurrent
		if available {
			status.State = StateAvailable
		}
		status.LatestVersion, status.PublishedAt = manifest.Version, manifest.PublishedAt
		status.ReleaseNotes, status.Available = manifest.ReleaseNotes, available
		status.ProgressPercent, status.DownloadedBytes, status.TotalBytes = 0, 0, manifest.Size
		status.LastCheckedAt, status.Error = now, ""
	})
	return m.Status(), nil
}

// StartDownload 在后台下载已验签的清单目标，避免慢速公网连接占住浏览器请求。
func (m *Manager) StartDownload() (Status, error) {
	if m.client == nil {
		return m.Status(), errors.New("在线更新尚未配置")
	}
	if !m.opMu.TryLock() {
		return m.Status(), errors.New("已有更新操作正在进行")
	}
	if m.manifest == nil || !m.Status().Available {
		m.opMu.Unlock()
		return m.Status(), errors.New("请先检查并确认存在可用更新")
	}
	manifest := *m.manifest
	m.updateState(func(status *Status) {
		status.State, status.Error = StateDownloading, ""
		status.ProgressPercent, status.DownloadedBytes, status.TotalBytes = 0, 0, manifest.Size
	})
	go func() {
		defer m.opMu.Unlock()
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
		defer cancel()
		path, err := m.client.Download(ctx, manifest, func(current, total int64) {
			percent := 0
			if total > 0 {
				percent = int(current * 100 / total)
			}
			m.updateState(func(status *Status) {
				status.DownloadedBytes, status.TotalBytes, status.ProgressPercent = current, total, min(percent, 100)
			})
		})
		if err != nil {
			m.fail("下载更新失败", err)
			return
		}
		m.packagePath = path
		m.updateState(func(status *Status) {
			status.State, status.ProgressPercent, status.Error = StateDownloaded, 100, ""
		})
	}()
	return m.Status(), nil
}

// Install 写入只含可信本地文件名和摘要的请求，再启动固定的一次性安装服务。
func (m *Manager) Install(ctx context.Context) (Status, error) {
	if m.client == nil || m.starter == nil {
		return m.Status(), errors.New("在线安装尚未配置")
	}
	if !m.opMu.TryLock() {
		return m.Status(), errors.New("已有更新操作正在进行")
	}
	defer m.opMu.Unlock()
	if m.manifest == nil || m.packagePath == "" || m.Status().State != StateDownloaded {
		return m.Status(), errors.New("请先完成更新下载")
	}
	if m.config.CanInstall != nil {
		if err := m.config.CanInstall(); err != nil {
			return m.Status(), err
		}
	}
	request := InstallRequest{
		Version: m.manifest.Version, PackageName: filepath.Base(m.packagePath),
		SHA256: m.manifest.SHA256, Size: m.manifest.Size,
	}
	if err := writeJSONAtomic(m.config.RequestPath, request, 0o600); err != nil {
		return m.Status(), fmt.Errorf("保存安装请求: %w", err)
	}
	_ = os.Remove(m.config.ResultPath)
	if err := m.starter.StartUpdate(ctx); err != nil {
		m.fail("启动更新安装服务失败", err)
		return m.Status(), err
	}
	m.updateState(func(status *Status) {
		status.State, status.Error = StateInstalling, ""
	})
	return m.Status(), nil
}

func (m *Manager) updateState(update func(*Status)) {
	m.stateMu.Lock()
	defer m.stateMu.Unlock()
	update(&m.state)
}

func (m *Manager) fail(message string, err error) {
	m.logger.Warn(message, "error", err)
	m.updateState(func(status *Status) {
		status.State, status.Error = StateFailed, message+"："+err.Error()
	})
}

func versionAtLeast(current, minimum string) (bool, error) {
	currentParts, err := parseStableVersion(current)
	if err != nil {
		return false, err
	}
	minimumParts, err := parseStableVersion(minimum)
	if err != nil {
		return false, err
	}
	for index := range currentParts {
		if currentParts[index] != minimumParts[index] {
			return currentParts[index] > minimumParts[index], nil
		}
	}
	return true, nil
}

func writeJSONAtomic(path string, value any, mode os.FileMode) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	content, err := json.Marshal(value)
	if err != nil {
		return err
	}
	temporary, err := os.CreateTemp(filepath.Dir(path), ".state-*")
	if err != nil {
		return err
	}
	temporaryPath := temporary.Name()
	defer os.Remove(temporaryPath)
	if err := temporary.Chmod(mode); err != nil {
		temporary.Close()
		return err
	}
	if _, err := temporary.Write(append(content, '\n')); err != nil {
		temporary.Close()
		return err
	}
	if err := temporary.Sync(); err != nil {
		temporary.Close()
		return err
	}
	if err := temporary.Close(); err != nil {
		return err
	}
	return os.Rename(temporaryPath, path)
}

// InstallRequest 是主服务和 root 安装助手之间的最小可信请求。
type InstallRequest struct {
	Version     string `json:"version"`
	PackageName string `json:"package_name"`
	SHA256      string `json:"sha256"`
	Size        int64  `json:"size"`
}

// InstallResult 由独立安装助手原子写入，供重启后的页面展示结果。
type InstallResult struct {
	Version   string `json:"version"`
	Status    string `json:"status"`
	Message   string `json:"message,omitempty"`
	UpdatedAt string `json:"updated_at"`
}
