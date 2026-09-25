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
	Configured        bool            `json:"configured"`
	CurrentVersion    string          `json:"current_version"`
	CurrentCommit     string          `json:"current_commit"`
	Channel           string          `json:"channel"`
	Channels          []ChannelOption `json:"channels,omitempty"`
	State             State           `json:"state"`
	LatestVersion     string          `json:"latest_version,omitempty"`
	PublishedAt       string          `json:"published_at,omitempty"`
	ReleaseNotes      string          `json:"release_notes,omitempty"`
	SelectedAction    string          `json:"selected_action,omitempty"`
	HistoryConfigured bool            `json:"history_configured"`
	Available         bool            `json:"available"`
	ProgressPercent   int             `json:"progress_percent"`
	DownloadedBytes   int64           `json:"downloaded_bytes,omitempty"`
	TotalBytes        int64           `json:"total_bytes,omitempty"`
	Error             string          `json:"error,omitempty"`
	LastCheckedAt     string          `json:"last_checked_at,omitempty"`
	InstallResult     string          `json:"install_result,omitempty"`
	Message           string          `json:"message,omitempty"`
}

// ChannelOption 描述设备可切换的更新通道；可用性仅代表本机已配置对应签名源。
type ChannelOption struct {
	ID          string `json:"id"`
	Label       string `json:"label"`
	Description string `json:"description"`
	Available   bool   `json:"available"`
}

// Config 定义在线更新功能的本地策略，私钥永远不属于设备配置。
type Config struct {
	ManifestURL      string
	CatalogURL       string
	BetaManifestURL  string
	BetaCatalogURL   string
	PublicKey        string
	Channel          string
	ChannelStatePath string
	Board            string
	Arch             string
	CurrentVersion   string
	CurrentCommit    string
	DownloadDir      string
	RequestPath      string
	ResultPath       string
	MaximumSize      int64
	Logger           *slog.Logger
	CanInstall       func() error
	AutoReboot       func() bool
}

// ServiceStarter 只允许启动预先安装的固定 systemd 单元，避免把远程字段拼进命令。
type ServiceStarter interface {
	StartUpdate(context.Context) error
}

// Manager 串行化检查、下载和安装请求，并为页面保存可轮询状态。
type Manager struct {
	config      Config
	client      *Client
	clients     map[string]*Client
	starter     ServiceStarter
	logger      *slog.Logger
	stateMu     sync.RWMutex
	state       Status
	manifest    *Manifest
	packagePath string
	opMu        sync.Mutex
}

type channelSelection struct {
	Channel string `json:"channel"`
}

// New 创建更新管理器；未同时配置清单地址和公钥时功能保持禁用而不阻止主服务启动。
func New(config Config, starter ServiceStarter) (*Manager, error) {
	if config.Channel == "" {
		config.Channel = "stable"
	}
	if !validChannel(config.Channel) {
		return nil, errors.New("更新通道仅支持 stable 或 beta")
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
	manager := &Manager{config: config, starter: starter, logger: config.Logger, clients: make(map[string]*Client)}
	manager.state = Status{
		Configured: false, CurrentVersion: config.CurrentVersion, CurrentCommit: config.CurrentCommit,
		Channel: config.Channel, Channels: manager.channelOptions(), State: StateDisabled,
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
	if config.ChannelStatePath != "" && (!filepath.IsAbs(config.ChannelStatePath) || filepath.Clean(config.ChannelStatePath) == string(filepath.Separator)) {
		return nil, errors.New("更新通道状态路径必须是非根目录的绝对路径")
	}
	publicKey, err := parsePublicKey(publicKeyValue)
	if err != nil {
		return nil, err
	}
	if err := manager.addClient(config.Channel, manifestURL, config.CatalogURL, publicKey); err != nil {
		return nil, err
	}
	betaManifestURL := strings.TrimSpace(config.BetaManifestURL)
	betaCatalogURL := strings.TrimSpace(config.BetaCatalogURL)
	if (betaManifestURL == "") != (betaCatalogURL == "") {
		return nil, errors.New("内测更新清单地址和历史目录地址必须同时配置")
	}
	if betaManifestURL != "" {
		if err := manager.addClient("beta", betaManifestURL, betaCatalogURL, publicKey); err != nil {
			return nil, err
		}
	}
	selectedChannel := config.Channel
	if saved, ok := readChannelSelection(config.ChannelStatePath); ok && manager.clients[saved] != nil {
		selectedChannel = saved
	}
	manager.config.Channel = selectedChannel
	manager.client = manager.clients[selectedChannel]
	manager.state.Configured = true
	manager.state.Channel = selectedChannel
	manager.state.Channels = manager.channelOptions()
	manager.state.HistoryConfigured = manager.client.HasCatalog()
	manager.state.State = StateIdle
	return manager, nil
}

func (m *Manager) addClient(channel, manifestURL, catalogURL string, publicKey []byte) error {
	client, err := NewClient(ClientConfig{
		ManifestURL: manifestURL, CatalogURL: catalogURL, PublicKey: publicKey, Channel: channel,
		Board: m.config.Board, Arch: m.config.Arch, DownloadDir: m.config.DownloadDir,
		MaximumSize: m.config.MaximumSize,
	})
	if err != nil {
		return err
	}
	m.clients[channel] = client
	return nil
}

func (m *Manager) channelOptions() []ChannelOption {
	return []ChannelOption{
		{ID: "stable", Label: "稳定版", Description: "只接收维护者正式发布并签名的版本。", Available: m.clients["stable"] != nil},
		{ID: "beta", Label: "内测版", Description: "可接收维护者发布的预览版本，可能包含尚未覆盖全部场景的改动。", Available: m.clients["beta"] != nil},
	}
}

func validChannel(channel string) bool {
	return channel == "stable" || channel == "beta"
}

func readChannelSelection(path string) (string, bool) {
	if strings.TrimSpace(path) == "" {
		return "", false
	}
	content, err := os.ReadFile(path)
	if err != nil {
		return "", false
	}
	var selection channelSelection
	if json.Unmarshal(content, &selection) != nil || !validChannel(selection.Channel) {
		return "", false
	}
	return selection.Channel, true
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

// SetChannel 切换更新来源并持久保存选择。切换后必须重新检查，旧通道的候选包不会被继续安装。
func (m *Manager) SetChannel(channel string) (Status, error) {
	channel = strings.TrimSpace(channel)
	if !validChannel(channel) {
		return m.Status(), errors.New("更新通道仅支持稳定版或内测版")
	}
	if !m.opMu.TryLock() {
		return m.Status(), errors.New("已有更新操作正在进行")
	}
	defer m.opMu.Unlock()
	if m.client == nil {
		return m.Status(), errors.New("在线更新尚未配置")
	}
	client := m.clients[channel]
	if client == nil {
		return m.Status(), errors.New("内测更新源尚未配置")
	}
	current := m.Status()
	if current.State == StateDownloading || current.State == StateDownloaded || current.State == StateInstalling {
		return current, errors.New("请先完成或放弃当前更新，再切换更新通道")
	}
	if channel == m.config.Channel {
		return current, nil
	}
	if m.config.ChannelStatePath != "" {
		if err := writeJSONAtomic(m.config.ChannelStatePath, channelSelection{Channel: channel}, 0o600); err != nil {
			return current, fmt.Errorf("保存更新通道: %w", err)
		}
	}
	m.config.Channel = channel
	m.client = client
	m.manifest = nil
	m.packagePath = ""
	m.updateState(func(status *Status) {
		status.Channel = channel
		status.Channels = m.channelOptions()
		status.State, status.Error, status.Message = StateIdle, "", ""
		status.HistoryConfigured = client.HasCatalog()
		status.LatestVersion, status.PublishedAt, status.ReleaseNotes = "", "", ""
		status.SelectedAction, status.Available = "", false
		status.ProgressPercent, status.DownloadedBytes, status.TotalBytes = 0, 0, 0
		status.LastCheckedAt = ""
	})
	return m.Status(), nil
}

// Check 获取并验签最新清单，签名失败时不会保留清单供后续下载。
func (m *Manager) Check(ctx context.Context) (Status, error) {
	if !m.opMu.TryLock() {
		return m.Status(), errors.New("已有更新操作正在进行")
	}
	defer m.opMu.Unlock()
	if m.client == nil {
		return m.Status(), errors.New("在线更新尚未配置")
	}
	m.updateState(func(status *Status) {
		status.State, status.Error = StateChecking, ""
	})
	checkCtx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	manifest, err := m.client.FetchManifest(checkCtx)
	if err != nil {
		if m.noBetaRelease(err) {
			m.updateState(func(status *Status) {
				status.State, status.Error = StateCurrent, ""
				status.LatestVersion, status.PublishedAt, status.ReleaseNotes = "", "", ""
				status.SelectedAction, status.Available = "", false
				status.ProgressPercent, status.DownloadedBytes, status.TotalBytes = 0, 0, 0
				status.LastCheckedAt = time.Now().Format(time.RFC3339)
				status.Message = "内测通道暂未发布可用版本。"
			})
			return m.Status(), nil
		}
		m.fail("检查更新失败", err)
		return m.Status(), err
	}
	available, err := newerVersion(manifest.Version, m.config.CurrentVersion)
	if err == nil && manifest.MinVersion != "" && !isUnversionedBuild(m.config.CurrentVersion) {
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
		status.SelectedAction = ""
		if available {
			status.SelectedAction = "upgrade"
		}
		status.ProgressPercent, status.DownloadedBytes, status.TotalBytes = 0, 0, manifest.Size
		status.LastCheckedAt, status.Error, status.Message = now, "", ""
	})
	return m.Status(), nil
}

// History 返回维护者签名且与当前程序兼容的可选历史版本；未配置目录时保持普通更新可用。
func (m *Manager) History(ctx context.Context) (ReleaseHistory, error) {
	if !m.opMu.TryLock() {
		return ReleaseHistory{}, errors.New("已有更新操作正在进行")
	}
	defer m.opMu.Unlock()
	if m.client == nil {
		return ReleaseHistory{}, errors.New("在线更新尚未配置")
	}
	if !m.client.HasCatalog() {
		return ReleaseHistory{Configured: false}, nil
	}
	checkCtx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	catalog, err := m.client.FetchCatalog(checkCtx)
	if err != nil {
		if m.noBetaRelease(err) {
			return ReleaseHistory{Configured: true, Message: "内测通道暂未发布可选版本。"}, nil
		}
		return ReleaseHistory{}, err
	}
	releases := make([]Release, 0, len(catalog.Manifests))
	for _, manifest := range catalog.Manifests {
		action, compatible := m.manifestAction(manifest)
		if !compatible {
			if manifest.Version != m.config.CurrentVersion && m.logger != nil {
				m.logger.Warn("忽略与当前版本不兼容的历史更新", "version", manifest.Version)
			}
			continue
		}
		releases = append(releases, Release{
			Version: manifest.Version, PublishedAt: manifest.PublishedAt,
			ReleaseNotes: manifest.ReleaseNotes, Action: action,
		})
	}
	sortReleases(releases)
	return ReleaseHistory{Configured: true, GeneratedAt: catalog.GeneratedAt, Releases: releases}, nil
}

// Select 从刚刚验签的历史目录中选择一个准确版本；客户端提交的版本号从不携带下载地址。
func (m *Manager) Select(ctx context.Context, version string) (Status, error) {
	version = strings.TrimSpace(version)
	if _, err := parseReleaseVersion(version); err != nil {
		return m.Status(), errors.New("请选择有效的更新版本")
	}
	if !m.opMu.TryLock() {
		return m.Status(), errors.New("已有更新操作正在进行")
	}
	defer m.opMu.Unlock()
	if m.client == nil {
		return m.Status(), errors.New("在线更新尚未配置")
	}
	if !m.client.HasCatalog() {
		return m.Status(), errors.New("历史版本目录尚未配置")
	}
	checkCtx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	catalog, err := m.client.FetchCatalog(checkCtx)
	if err != nil {
		return m.Status(), err
	}
	for _, manifest := range catalog.Manifests {
		if manifest.Version != version {
			continue
		}
		action, compatible := m.manifestAction(manifest)
		if !compatible {
			return m.Status(), errors.New("所选版本与当前程序不兼容，不能直接安装")
		}
		m.manifest = &manifest
		m.packagePath = ""
		m.updateState(func(status *Status) {
			status.State, status.Error = StateAvailable, ""
			status.LatestVersion, status.PublishedAt = manifest.Version, manifest.PublishedAt
			status.ReleaseNotes, status.SelectedAction = manifest.ReleaseNotes, action
			status.Available = true
			status.ProgressPercent, status.DownloadedBytes, status.TotalBytes = 0, 0, manifest.Size
			status.LastCheckedAt = time.Now().Format(time.RFC3339)
		})
		return m.Status(), nil
	}
	return m.Status(), errors.New("指定版本不在维护者签名的历史目录中")
}

// StartDownload 在后台下载已验签的清单目标，避免慢速公网连接占住浏览器请求。
func (m *Manager) StartDownload() (Status, error) {
	if !m.opMu.TryLock() {
		return m.Status(), errors.New("已有更新操作正在进行")
	}
	if m.client == nil {
		m.opMu.Unlock()
		return m.Status(), errors.New("在线更新尚未配置")
	}
	if m.manifest == nil || !m.Status().Available {
		m.opMu.Unlock()
		return m.Status(), errors.New("请先检查更新或选择历史版本")
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

func (m *Manager) manifestAction(manifest Manifest) (string, bool) {
	if manifest.Version == m.config.CurrentVersion {
		return "", false
	}
	if manifest.MinVersion != "" && !isUnversionedBuild(m.config.CurrentVersion) {
		meetsMinimum, err := versionAtLeast(m.config.CurrentVersion, manifest.MinVersion)
		if err != nil || !meetsMinimum {
			return "", false
		}
	}
	if isUnversionedBuild(m.config.CurrentVersion) {
		return "install", true
	}
	isNewer, err := newerVersion(manifest.Version, m.config.CurrentVersion)
	if err != nil {
		return "", false
	}
	if isNewer {
		return "upgrade", true
	}
	return "rollback", true
}

func sortReleases(releases []Release) {
	for left := range releases {
		for right := left + 1; right < len(releases); right++ {
			leftVersion, leftErr := parseReleaseVersion(releases[left].Version)
			rightVersion, rightErr := parseReleaseVersion(releases[right].Version)
			if leftErr != nil || rightErr != nil {
				continue
			}
			if compareReleaseVersions(leftVersion, rightVersion) < 0 {
				releases[left], releases[right] = releases[right], releases[left]
			}
		}
	}
}

// Install 写入只含可信本地文件名和摘要的请求，再启动固定的一次性安装服务。
func (m *Manager) Install(ctx context.Context) (Status, error) {
	if !m.opMu.TryLock() {
		return m.Status(), errors.New("已有更新操作正在进行")
	}
	defer m.opMu.Unlock()
	if m.client == nil || m.starter == nil {
		return m.Status(), errors.New("在线安装尚未配置")
	}
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
	if m.config.AutoReboot != nil {
		request.RebootAfterInstall = m.config.AutoReboot()
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

func (m *Manager) noBetaRelease(err error) bool {
	return m.config.Channel == "beta" && strings.Contains(err.Error(), "更新服务器返回 HTTP 404")
}

func versionAtLeast(current, minimum string) (bool, error) {
	currentVersion, err := parseReleaseVersion(current)
	if err != nil {
		return false, err
	}
	minimumParts, err := parseStableVersion(minimum)
	if err != nil {
		return false, err
	}
	return compareReleaseVersions(currentVersion, releaseVersion{core: minimumParts}) >= 0, nil
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
	Version            string `json:"version"`
	PackageName        string `json:"package_name"`
	SHA256             string `json:"sha256"`
	Size               int64  `json:"size"`
	RebootAfterInstall bool   `json:"reboot_after_install,omitempty"`
}

// InstallResult 由独立安装助手原子写入，供重启后的页面展示结果。
type InstallResult struct {
	Version   string `json:"version"`
	Status    string `json:"status"`
	Message   string `json:"message,omitempty"`
	UpdatedAt string `json:"updated_at"`
}
