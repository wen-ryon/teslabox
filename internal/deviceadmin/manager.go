// Package deviceadmin 管理 NetworkManager 无线配置和设备电源操作。
package deviceadmin

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/netip"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/wen-ryon/teslabox/internal/system"
)

const (
	defaultAPInterface      = "ap0"
	apDisableDelay          = time.Second
	commandTimeout          = 20 * time.Second
	recoveryTimeout         = 50 * time.Second
	recoveryInitialBackoff  = 5 * time.Second
	recoveryMaxBackoff      = time.Minute
	networkEventDebounce    = 2 * time.Second
	networkWatcherRetry     = 30 * time.Second
	networkFallbackInterval = 2 * time.Minute
)

var (
	// ErrAPActive 在 AP+STA 并发不可用时返回；当前硬件已支持并发，此错误保留用于回退场景。
	ErrAPActive = errors.New("无线热点正在使用当前网卡")
	// ErrWiFiUnavailable 表示系统没有发现可供 NetworkManager 管理的无线接口。
	ErrWiFiUnavailable = errors.New("未检测到可用的无线网卡")
	// ErrUnsupportedSecurity 表示第一版尚不能安全配置该无线加密类型。
	ErrUnsupportedSecurity     = errors.New("暂不支持该无线网络的加密类型")
	errAutoNetworkInvisible    = errors.New("自动连接网络暂不可见")
	errWiFiPasswordRequired    = errors.New("已保存 WiFi 缺少可由系统读取的密码，请在设置中心重新输入密码")
	errNetworkInventoryChanged = errors.New("NetworkManager 无线设备清单已变化")
)

// InputError 表示可以安全返回给管理页面的请求参数错误。
type InputError struct {
	Message string
}

// Error 返回面向用户的简体中文校验说明。
func (e *InputError) Error() string { return e.Message }

// SavedNetwork 是不包含密码的 NetworkManager 客户端连接摘要。
type SavedNetwork struct {
	Name             string       `json:"name"`
	UUID             string       `json:"uuid"`
	SSID             string       `json:"ssid"`
	Security         string       `json:"security,omitempty"`
	Active           bool         `json:"active"`
	AutoConnect      bool         `json:"auto_connect"`
	RequiresPassword bool         `json:"requires_password,omitempty"`
	IPv4             IPv4Settings `json:"ipv4"`
}

// IPv4Settings 描述已保存 WiFi 的 IPv4 配置；DHCP 模式不会返回临时租约地址。
type IPv4Settings struct {
	Mode       string   `json:"mode"`
	Address    string   `json:"address,omitempty"`
	SubnetMask string   `json:"subnet_mask,omitempty"`
	Gateway    string   `json:"gateway,omitempty"`
	DNS        []string `json:"dns"`
}

// AccessPoint 是扫描结果中按 SSID 去重后的当前连接或最强接入点。
type AccessPoint struct {
	SSID     string `json:"ssid"`
	BSSID    string `json:"bssid"`
	Signal   int    `json:"signal"`
	Band     string `json:"band"`
	Channel  int    `json:"channel"`
	Security string `json:"security"`
	Saved    bool   `json:"saved"`
	Active   bool   `json:"active"`
}

// WiFiStatus 描述客户端 WiFi 页面需要的当前状态。
type WiFiStatus struct {
	Available         bool           `json:"available"`
	Interface         string         `json:"interface"`
	MACAddress        string         `json:"mac_address,omitempty"`
	IPAddress         string         `json:"ip_address,omitempty"`
	CurrentConnection string         `json:"current_connection,omitempty"`
	APActive          bool           `json:"ap_active"`
	ConcurrentMode    bool           `json:"concurrent_mode"`
	ChannelCoupled    bool           `json:"channel_coupled"`
	SingleRadioNotice string         `json:"single_radio_notice,omitempty"`
	SavedNetworks     []SavedNetwork `json:"saved_networks"`
	AvailableNetworks []AccessPoint  `json:"available_networks"`
	Recovery          RecoveryStatus `json:"recovery"`
}

// RecoveryStatus 描述运行期上游 WiFi 自愈的最近结果。
type RecoveryStatus struct {
	Enabled       bool       `json:"enabled"`
	LastAttemptAt *time.Time `json:"last_attempt_at,omitempty"`
	LastSuccessAt *time.Time `json:"last_success_at,omitempty"`
	LastError     string     `json:"last_error,omitempty"`
	RecoveredSSID string     `json:"recovered_ssid,omitempty"`
	Failures      int        `json:"failures,omitempty"`
	NextAttemptAt *time.Time `json:"next_attempt_at,omitempty"`
}

// APSettings 描述热点配置；密码永远不会通过读取接口返回。
type APSettings struct {
	Available      bool   `json:"available"`
	Enabled        bool   `json:"enabled"`
	Interface      string `json:"interface,omitempty"`
	MACAddress     string `json:"mac_address,omitempty"`
	ConnectionName string `json:"connection_name,omitempty"`
	UUID           string `json:"uuid,omitempty"`
	SSID           string `json:"ssid,omitempty"`
	Band           string `json:"band,omitempty"`
	Channel        int    `json:"channel,omitempty"`
	Security       string `json:"security,omitempty"`
	Address        string `json:"address,omitempty"`
	AutoConnect    bool   `json:"auto_connect"`
	Sharing        bool   `json:"sharing"`
}

// ConnectRequest 描述连接已保存网络或新网络的请求。
type ConnectRequest struct {
	UUID                string `json:"uuid,omitempty"`
	SSID                string `json:"ssid,omitempty"`
	BSSID               string `json:"bssid,omitempty"`
	Channel             int    `json:"channel,omitempty"`
	Password            string `json:"password,omitempty"`
	Security            string `json:"security,omitempty"`
	AutoConnect         *bool  `json:"auto_connect,omitempty"`
	ConfirmAPDisconnect bool   `json:"confirm_ap_disconnect"`
}

// AutoConnectUpdate 描述已保存网络的自动连接策略变更。
type AutoConnectUpdate struct {
	UUID    string `json:"uuid"`
	Enabled bool   `json:"enabled"`
}

// IPv4Update 描述单个已保存 WiFi 的 IPv4 获取方式；静态模式只接受 IPv4 地址。
type IPv4Update struct {
	UUID       string   `json:"uuid"`
	Mode       string   `json:"mode"`
	Address    string   `json:"address,omitempty"`
	SubnetMask string   `json:"subnet_mask,omitempty"`
	Gateway    string   `json:"gateway,omitempty"`
	DNS        []string `json:"dns"`
}

// APUpdate 描述允许从管理页面修改的热点字段。
type APUpdate struct {
	Enabled     bool   `json:"enabled"`
	SSID        string `json:"ssid"`
	Password    string `json:"password,omitempty"`
	Band        string `json:"band"`
	Channel     int    `json:"channel"`
	AutoConnect bool   `json:"auto_connect"`
}

// Manager 串行执行会改变网络状态的命令，避免多个页面操作互相覆盖。
type Manager struct {
	runner                  system.Runner
	logger                  *slog.Logger
	configuredWiFiInterface string
	configuredAPInterface   string
	mu                      sync.Mutex
	recoveryMu              sync.RWMutex
	recovery                RecoveryStatus
	schedule                func(time.Duration, func())
	apUpdateGeneration      uint64
	monitoredWiFiInterface  string
	watchNetworkEvents      networkEventWatch
	sameWirelessPHY         func(string, string) (bool, bool)
}

// NetworkConfig 允许管理员固定无线接口；客户端接口留空时会根据 NetworkManager 自动发现。
type NetworkConfig struct {
	WiFiInterface string
	APInterface   string
}

// New 创建基于 NetworkManager 和 systemd 的设备管理器；可选配置用于兼容非标准网卡命名。
func New(runner system.Runner, logger *slog.Logger, configs ...NetworkConfig) *Manager {
	networkConfig := NetworkConfig{APInterface: defaultAPInterface}
	if logger == nil {
		logger = slog.Default()
	}
	if len(configs) > 0 {
		networkConfig = configs[0]
		if networkConfig.APInterface == "" {
			networkConfig.APInterface = defaultAPInterface
		}
	}
	return &Manager{
		runner: runner, logger: logger,
		configuredWiFiInterface: strings.TrimSpace(networkConfig.WiFiInterface),
		configuredAPInterface:   strings.TrimSpace(networkConfig.APInterface),
		recovery:                RecoveryStatus{Enabled: true},
		schedule:                func(delay time.Duration, operation func()) { time.AfterFunc(delay, operation) },
		watchNetworkEvents:      watchNetworkManagerEvents,
		sameWirelessPHY:         wirelessInterfacesSharePHY,
	}
}

// Run 订阅 NetworkManager 事件，并只在上游 WiFi 状态、BSSID 或信道实际变化时恢复 AP+STA。
// 正常待机不执行轮询，避免车载供电下反复唤醒无线芯片。
func (m *Manager) Run(ctx context.Context) {
	events := make(chan networkEvent, 1)
	watchErrors := make(chan error, 1)
	eventTimer := stoppedTimer()
	defer eventTimer.Stop()
	retryTimer := stoppedTimer()
	defer retryTimer.Stop()
	watcherRetryTimer := stoppedTimer()
	defer watcherRetryTimer.Stop()
	fallbackTimer := stoppedTimer()
	defer fallbackTimer.Stop()

	m.reconcileForNetworkChange(ctx, retryTimer)
	watcherOnline := m.startNetworkWatcher(ctx, events, watchErrors)
	if !watcherOnline {
		resetTimer(fallbackTimer, networkFallbackInterval)
	}
	for {
		select {
		case <-ctx.Done():
			return
		case <-events:
			// 漫游和 DHCP 阶段会连续产生多个状态变化，合并为一次同步可避免无意义的重复命令。
			resetTimer(eventTimer, networkEventDebounce)
		case <-eventTimer.C:
			m.reconcileForNetworkChange(ctx, retryTimer)
		case <-retryTimer.C:
			m.reconcileForNetworkChange(ctx, retryTimer)
		case watchErr := <-watchErrors:
			watcherOnline = false
			if errors.Is(watchErr, errNetworkInventoryChanged) {
				// AP 虚拟接口创建和无线网卡热插拔都会改变设备清单，这是重建监听目标的正常信号。
				// 保持事件驱动模式，避免把一次预期变化降级成两分钟轮询。
				resetTimer(watcherRetryTimer, networkEventDebounce)
				continue
			}
			if watchErr != nil {
				m.logger.Warn("NetworkManager 事件监听中断，已切换低频后备检查", "error", watchErr)
			}
			resetTimer(watcherRetryTimer, networkWatcherRetry)
			resetTimer(fallbackTimer, networkFallbackInterval)
		case <-watcherRetryTimer.C:
			watcherOnline = m.startNetworkWatcher(ctx, events, watchErrors)
			if watcherOnline {
				stopTimer(fallbackTimer)
			} else {
				resetTimer(watcherRetryTimer, networkWatcherRetry)
			}
		case <-fallbackTimer.C:
			if watcherOnline {
				continue
			}
			m.reconcileForFallback(ctx, retryTimer)
			// 无线网卡可能在启动后才插入；发现接口后立即恢复事件监听，
			// 避免设备余下生命周期一直依赖低频轮询。
			watcherOnline = m.startNetworkWatcher(ctx, events, watchErrors)
			if watcherOnline {
				stopTimer(fallbackTimer)
			} else {
				resetTimer(fallbackTimer, networkFallbackInterval)
			}
		}
	}
}

func (m *Manager) startNetworkWatcher(ctx context.Context, events chan<- networkEvent, watchErrors chan<- error) bool {
	interfaceName := m.monitoredInterface()
	if interfaceName == "" || m.watchNetworkEvents == nil {
		return false
	}
	go func() {
		err := m.watchNetworkEvents(ctx, interfaceName, func(event networkEvent) {
			select {
			case events <- event:
			default:
				// 事件会在主循环中按时间窗口合并，队列已满表示已有一次同步待执行。
			}
		})
		if ctx.Err() != nil {
			return
		}
		if err == nil {
			err = errors.New("NetworkManager 事件监听意外退出")
		}
		select {
		case watchErrors <- err:
		case <-ctx.Done():
		}
	}()
	return true
}

func (m *Manager) reconcileForNetworkChange(ctx context.Context, retryTimer *time.Timer) {
	if !m.recoveryDue() {
		m.scheduleRecoveryRetry(retryTimer)
		return
	}
	recoveryCtx, cancel := context.WithTimeout(ctx, recoveryTimeout)
	recovered, ssid, err := m.reconcileAutoConnect(recoveryCtx)
	cancel()
	if err != nil {
		m.recordRecovery(ssid, false, err)
		if !errors.Is(err, errAutoNetworkInvisible) {
			m.logger.Warn("自动恢复上游 WiFi 失败", "ssid", ssid, "error", err)
		}
		m.scheduleRecoveryRetry(retryTimer)
		return
	}
	if recovered {
		m.recordRecovery(ssid, true, nil)
		m.logger.Info("上游 WiFi 已自动恢复", "ssid", ssid)
		stopTimer(retryTimer)
		return
	}
	m.clearRecoveryFailure()
	stopTimer(retryTimer)
}

func (m *Manager) reconcileForFallback(ctx context.Context, retryTimer *time.Timer) {
	recoveryCtx, cancel := context.WithTimeout(ctx, recoveryTimeout)
	recovered, ssid, err := m.reconcileAutoConnectIfNeeded(recoveryCtx)
	cancel()
	if err != nil {
		m.recordRecovery(ssid, false, err)
		if !errors.Is(err, errAutoNetworkInvisible) {
			m.logger.Warn("自动恢复上游 WiFi 失败", "ssid", ssid, "error", err)
		}
		m.scheduleRecoveryRetry(retryTimer)
		return
	}
	if recovered {
		m.recordRecovery(ssid, true, nil)
		m.logger.Info("上游 WiFi 已自动恢复", "ssid", ssid)
	}
}

func (m *Manager) scheduleRecoveryRetry(timer *time.Timer) {
	m.recoveryMu.RLock()
	next := m.recovery.NextAttemptAt
	m.recoveryMu.RUnlock()
	if next == nil {
		return
	}
	delay := time.Until(*next)
	if delay < 0 {
		delay = 0
	}
	resetTimer(timer, delay)
}

func (m *Manager) monitoredInterface() string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.monitoredWiFiInterface
}

func stoppedTimer() *time.Timer {
	timer := time.NewTimer(time.Hour)
	stopTimer(timer)
	return timer
}

func resetTimer(timer *time.Timer, delay time.Duration) {
	stopTimer(timer)
	timer.Reset(delay)
}

func stopTimer(timer *time.Timer) {
	if !timer.Stop() {
		select {
		case <-timer.C:
		default:
		}
	}
}

// WiFiStatus 返回客户端连接和已保存网络，不读取任何连接密码。
func (m *Manager) WiFiStatus(ctx context.Context) (WiFiStatus, error) {
	profiles, err := m.wirelessProfiles(ctx)
	if err != nil {
		return WiFiStatus{}, err
	}
	wifiInterface, apInterface, wifiAvailable, _ := m.networkInterfaces(ctx, profiles)
	status := WiFiStatus{Available: wifiAvailable, Interface: wifiInterface, SavedNetworks: []SavedNetwork{}}
	if wifiAvailable {
		status.MACAddress, status.IPAddress = m.interfaceDetails(ctx, wifiInterface)
	}
	status.Recovery = m.recoveryStatus()
	apSSIDs := make(map[string]bool)
	for _, profile := range profiles {
		if profile.Mode == "ap" {
			apSSIDs[profile.SSID] = true
			status.APActive = status.APActive || profile.Active
			concurrent := wifiAvailable && profile.Active && profile.Device == apInterface && apInterface != wifiInterface
			status.ConcurrentMode = status.ConcurrentMode || concurrent
			if concurrent {
				samePHY, known := m.sameWirelessPHY(wifiInterface, apInterface)
				// 无法读取 sysfs 拓扑时按共用射频处理，宁可多做一次信道对齐，也不能让单射频并发随机失败。
				status.ChannelCoupled = status.ChannelCoupled || !known || samePHY
			}
			continue
		}
		if profile.Active {
			status.CurrentConnection = profile.Name
		}
		status.SavedNetworks = append(status.SavedNetworks, SavedNetwork{
			Name: profile.Name, UUID: profile.UUID, SSID: profile.SSID,
			Security: profile.Security, Active: profile.Active, AutoConnect: profile.AutoConnect,
			RequiresPassword: profile.RequiresPassword, IPv4: profileIPv4Settings(profile),
		})
	}
	if status.APActive {
		if status.ConcurrentMode {
			if status.ChannelCoupled {
				status.SingleRadioNotice = "AP+STA 并发模式已启用；连接不同信道的上游 WiFi 时，热点会短暂切换到相同信道。"
			} else {
				status.SingleRadioNotice = "热点和上游 WiFi 使用独立无线设备，可以分别使用不同信道。"
			}
		} else {
			status.SingleRadioNotice = fmt.Sprintf("热点正在占用 %s；当前系统没有可用的 AP+STA 并发接口，连接上游 WiFi 会被阻止。", wifiInterface)
		}
	}
	if !status.Available {
		status.SingleRadioNotice = "未检测到可用的无线网卡；热点和 WiFi 管理不可用，但 TeslaUSB、媒体与有线管理不受影响。"
		return status, nil
	}
	saved := make(map[string]bool, len(status.SavedNetworks))
	for _, profile := range status.SavedNetworks {
		saved[profile.SSID] = true
	}
	available, _ := m.cachedAccessPoints(ctx, wifiInterface, saved)
	for _, item := range available {
		if !apSSIDs[item.SSID] {
			status.AvailableNetworks = append(status.AvailableNetworks, item)
		}
	}
	return status, nil
}

func (m *Manager) reconcileAutoConnect(ctx context.Context) (bool, string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.reconcileAutoConnectLocked(ctx)
}

// reconcileAutoConnectIfNeeded 是 D-Bus 监听不可用时的低频后备路径。
// 它只检查连接状态；确认断连后才执行完整恢复，避免后备机制持续扫描无线网络。
func (m *Manager) reconcileAutoConnectIfNeeded(ctx context.Context) (bool, string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	if m.monitoredWiFiInterface != "" {
		connected, err := m.wifiInterfaceConnected(ctx, m.monitoredWiFiInterface)
		if err == nil && connected {
			return false, "", nil
		}
	}
	return m.reconcileAutoConnectLocked(ctx)
}

func (m *Manager) reconcileAutoConnectLocked(ctx context.Context) (bool, string, error) {
	status, err := m.WiFiStatus(ctx)
	if err != nil {
		return false, "", err
	}
	m.monitoredWiFiInterface = status.Interface
	if !status.Available || status.Interface == "" {
		return false, "", nil
	}
	if status.APActive && !status.ConcurrentMode {
		return false, "", nil
	}
	if state, stateErr := m.wifiInterfaceState(ctx, status.Interface); stateErr == nil && networkManagerActivationInProgress(state) {
		// NetworkManager 的启动自动连接可能仍在认证或获取地址；重复 connection up
		// 会重置同一激活流程，并让短暂的 CLI 等待超时演变成持续断连。
		return false, "", nil
	}
	if current, found := currentAutoConnectNetwork(status); found {
		available, listErr := m.cachedAccessPoints(ctx, status.Interface, map[string]bool{})
		if listErr != nil {
			return false, current.SSID, listErr
		}
		if target, visible := findAccessPoint(available, current.SSID); visible && target.Active {
			settings, settingsErr := m.APSettings(ctx)
			if settingsErr != nil {
				return false, current.SSID, settingsErr
			}
			if !status.APActive || !settings.Enabled || !status.ChannelCoupled || settings.Channel == target.Channel {
				return false, "", nil
			}
			if err := m.alignAPChannel(ctx, status, target.Channel); err != nil {
				return false, current.SSID, fmt.Errorf("跟随上游 WiFi 信道: %w", err)
			}
			if _, err := m.run(
				ctx,
				"nmcli", "--wait", "15", "connection", "up", "uuid", current.UUID,
				"ifname", status.Interface, "ap", target.BSSID,
			); err != nil {
				activated, pending := m.activationStateAfterWaitTimeout(ctx, status.Interface, err)
				if activated {
					return true, current.SSID, nil
				}
				if pending {
					return false, "", nil
				}
				return false, current.SSID, fmt.Errorf("恢复 %s: %w", current.SSID, err)
			}
			return true, current.SSID, nil
		}
	} else if status.CurrentConnection != "" {
		return false, "", nil
	}
	if !status.APActive || !status.ConcurrentMode || !status.ChannelCoupled {
		// 普通 STA 自动连接属于 NetworkManager 的职责。后台重复扫描和 connection up 会重置
		// 系统正在进行的认证，并且无法读取桌面密钥代理持有的密码；这里只处理单射频热点信道约束。
		return false, "", nil
	}
	candidates := m.autoConnectCandidates(ctx, status.SavedNetworks)
	if len(candidates) == 0 {
		return false, "", nil
	}
	if _, err := m.run(ctx, "nmcli", "--wait", "12", "device", "wifi", "rescan", "ifname", status.Interface); err != nil {
		return false, "", fmt.Errorf("重新扫描上游 WiFi: %w", err)
	}
	available, err := m.cachedAccessPoints(ctx, status.Interface, map[string]bool{})
	if err != nil {
		return false, "", err
	}
	var failures []error
	visible := false
	failedSSID := ""
	for _, candidate := range candidates {
		target, found := findAccessPoint(available, candidate.network.SSID)
		if !found {
			continue
		}
		visible = true
		if candidate.network.RequiresPassword {
			failures = append(failures, fmt.Errorf("%s: %w", candidate.network.SSID, errWiFiPasswordRequired))
			failedSSID = candidate.network.SSID
			continue
		}
		if err := m.alignAPChannel(ctx, status, target.Channel); err != nil {
			failures = append(failures, fmt.Errorf("同步 %s 热点信道: %w", candidate.network.SSID, err))
			failedSSID = candidate.network.SSID
			continue
		}
		if _, err := m.run(
			ctx,
			"nmcli", "connection", "modify", "uuid", candidate.network.UUID,
			"connection.autoconnect", "yes",
		); err != nil {
			failures = append(failures, fmt.Errorf("恢复 %s 系统自动连接: %w", candidate.network.SSID, err))
			failedSSID = candidate.network.SSID
			continue
		}
		// 重写 autoconnect 会清除 NetworkManager 对该连接的临时阻止状态；真正的认证和选点
		// 仍由 NetworkManager 完成，避免服务进程替代系统 WiFi 管理器持有或索取密码。
		return false, "", nil
	}
	if len(failures) > 0 {
		return false, failedSSID, errors.Join(failures...)
	}
	if !visible {
		return false, candidates[0].network.SSID, errAutoNetworkInvisible
	}
	return false, "", nil
}

func (m *Manager) wifiInterfaceConnected(ctx context.Context, interfaceName string) (bool, error) {
	state, err := m.wifiInterfaceState(ctx, interfaceName)
	return state == 100, err
}

func (m *Manager) wifiInterfaceState(ctx context.Context, interfaceName string) (int, error) {
	output, err := m.run(ctx, "nmcli", "-g", "GENERAL.STATE", "device", "show", interfaceName)
	if err != nil {
		return 0, fmt.Errorf("读取 %s 连接状态: %w", interfaceName, err)
	}
	fields := strings.Fields(string(output))
	if len(fields) == 0 {
		return 0, fmt.Errorf("读取 %s 连接状态: 返回为空", interfaceName)
	}
	state, err := strconv.Atoi(fields[0])
	if err != nil {
		return 0, fmt.Errorf("读取 %s 连接状态: %w", interfaceName, err)
	}
	return state, nil
}

func networkManagerActivationInProgress(state int) bool {
	return state >= 40 && state < 100 || state == 110
}

func isNMCLIWaitTimeout(err error) bool {
	var commandError *system.CommandError
	return errors.As(err, &commandError) && commandError.Code == 3
}

func (m *Manager) activationStateAfterWaitTimeout(ctx context.Context, interfaceName string, waitErr error) (bool, bool) {
	if !isNMCLIWaitTimeout(waitErr) {
		return false, false
	}
	state, err := m.wifiInterfaceState(ctx, interfaceName)
	if err != nil {
		return false, false
	}
	return state == 100, networkManagerActivationInProgress(state)
}

func currentAutoConnectNetwork(status WiFiStatus) (SavedNetwork, bool) {
	for _, network := range status.SavedNetworks {
		if network.Name == status.CurrentConnection && network.AutoConnect {
			return network, true
		}
	}
	return SavedNetwork{}, false
}

type autoConnectCandidate struct {
	network  SavedNetwork
	priority int
}

func (m *Manager) autoConnectCandidates(ctx context.Context, networks []SavedNetwork) []autoConnectCandidate {
	candidates := make([]autoConnectCandidate, 0, len(networks))
	for _, network := range networks {
		if !network.AutoConnect {
			continue
		}
		priority := 0
		output, err := m.run(ctx, "nmcli", "-g", "connection.autoconnect-priority", "connection", "show", "uuid", network.UUID)
		if err == nil {
			priority, _ = strconv.Atoi(strings.TrimSpace(string(output)))
		}
		candidates = append(candidates, autoConnectCandidate{network: network, priority: priority})
	}
	for index := 1; index < len(candidates); index++ {
		for current := index; current > 0 && candidates[current].priority > candidates[current-1].priority; current-- {
			candidates[current], candidates[current-1] = candidates[current-1], candidates[current]
		}
	}
	return candidates
}

func findAccessPoint(items []AccessPoint, ssid string) (AccessPoint, bool) {
	for _, item := range items {
		if item.SSID == ssid {
			return item, true
		}
	}
	return AccessPoint{}, false
}

func (m *Manager) recoveryStatus() RecoveryStatus {
	m.recoveryMu.RLock()
	defer m.recoveryMu.RUnlock()
	return m.recovery
}

func (m *Manager) recordRecovery(ssid string, success bool, cause error) {
	now := time.Now()
	m.recoveryMu.Lock()
	defer m.recoveryMu.Unlock()
	m.recovery.LastAttemptAt = &now
	m.recovery.RecoveredSSID = ssid
	m.recovery.LastError = ""
	if cause != nil {
		m.recovery.LastError = cause.Error()
		m.recovery.Failures++
		delay := recoveryInitialBackoff
		for attempt := 1; attempt < m.recovery.Failures && delay < recoveryMaxBackoff; attempt++ {
			delay *= 2
			if delay > recoveryMaxBackoff {
				delay = recoveryMaxBackoff
			}
		}
		next := now.Add(delay)
		m.recovery.NextAttemptAt = &next
		return
	}
	if success {
		m.recovery.LastSuccessAt = &now
	}
	m.recovery.Failures = 0
	m.recovery.NextAttemptAt = nil
}

func (m *Manager) clearRecoveryFailure() {
	m.recoveryMu.Lock()
	defer m.recoveryMu.Unlock()
	m.recovery.LastError = ""
	m.recovery.Failures = 0
	m.recovery.NextAttemptAt = nil
}

func (m *Manager) recoveryDue() bool {
	m.recoveryMu.RLock()
	defer m.recoveryMu.RUnlock()
	return m.recovery.NextAttemptAt == nil || !time.Now().Before(*m.recovery.NextAttemptAt)
}

// Scan 手动请求 NetworkManager 扫描，并返回按 SSID 去重后的结果。
// AP+STA 并发模式下扫描在自动识别的客户端接口上执行，不影响热点虚拟接口。
func (m *Manager) Scan(ctx context.Context) ([]AccessPoint, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	status, err := m.WiFiStatus(ctx)
	if err != nil {
		return nil, err
	}
	if !status.Available || status.Interface == "" {
		return nil, ErrWiFiUnavailable
	}
	if status.APActive && !status.ConcurrentMode {
		return nil, ErrAPActive
	}
	if _, err := m.run(ctx, "nmcli", "--wait", "12", "device", "wifi", "rescan", "ifname", status.Interface); err != nil {
		return nil, fmt.Errorf("扫描无线网络: %w", err)
	}
	saved := make(map[string]bool, len(status.SavedNetworks))
	for _, profile := range status.SavedNetworks {
		saved[profile.SSID] = true
	}
	return m.cachedAccessPoints(ctx, status.Interface, saved)
}

// Connect 连接已保存网络或创建 WPA/WPA2、开放网络配置。
// AP+STA 并发模式下连接在自动识别的客户端接口上执行，热点虚拟接口不受影响。
func (m *Manager) Connect(ctx context.Context, request ConnectRequest) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	status, err := m.WiFiStatus(ctx)
	if err != nil {
		return err
	}
	if !status.Available || status.Interface == "" {
		return ErrWiFiUnavailable
	}
	if status.APActive && !status.ConcurrentMode && !request.ConfirmAPDisconnect {
		return ErrAPActive
	}
	ssid := request.SSID
	if request.UUID != "" {
		if !validUUID(request.UUID) {
			return invalidInput("连接 UUID 无效")
		}
		network, exists := findSavedNetwork(status.SavedNetworks, request.UUID)
		if !exists {
			return invalidInput("未找到可连接的已保存 WiFi")
		}
		ssid = network.SSID
		if request.Password == "" && network.RequiresPassword {
			return invalidInput(errWiFiPasswordRequired.Error())
		}
		if request.Password != "" {
			if !profileUsesPSK(network.Security) {
				return ErrUnsupportedSecurity
			}
			if err := validatePassword(request.Password); err != nil {
				return err
			}
			if _, err := m.run(
				ctx,
				"nmcli",
				"connection", "modify", "uuid", request.UUID,
				"802-11-wireless-security.psk-flags", "0",
				"802-11-wireless-security.psk", request.Password,
			); err != nil {
				return fmt.Errorf("保存已保存网络的 WiFi 密码: %w", err)
			}
		}
	}
	target, err := m.resolveAccessPoint(ctx, status.Interface, ssid, request.BSSID, request.Channel)
	if err != nil {
		return err
	}
	if status.APActive && status.ConcurrentMode && status.ChannelCoupled {
		if err := m.alignAPChannel(ctx, status, target.Channel); err != nil {
			return err
		}
	}
	if request.UUID != "" {
		if target.BSSID != "" {
			if _, err := m.run(ctx, "nmcli", "connection", "modify", "uuid", request.UUID, "802-11-wireless.bssid", target.BSSID); err != nil {
				return fmt.Errorf("锁定上游 WiFi 接入点: %w", err)
			}
		}
		_, err = m.run(ctx, "nmcli", "--wait", "15", "connection", "up", "uuid", request.UUID, "ifname", status.Interface)
		if err != nil {
			return fmt.Errorf("连接已保存网络: %w", err)
		}
		if request.AutoConnect != nil {
			return m.setAutoConnect(ctx, request.UUID, *request.AutoConnect)
		}
		return nil
	}
	if err := validateSSID(request.SSID); err != nil {
		return err
	}
	security := normalizeSecurity(request.Security)
	profileName := "TeslaBox-Upstream-" + request.SSID
	args := []string{"--wait", "15", "device", "wifi", "connect", request.SSID}
	if target.BSSID != "" {
		args = append(args, "bssid", target.BSSID)
	}
	args = append(args, "ifname", status.Interface, "name", profileName)
	if security != "open" {
		if security != "wpa" {
			return ErrUnsupportedSecurity
		}
		if err := validatePassword(request.Password); err != nil {
			return err
		}
		args = append(args, "password", request.Password)
	}
	if _, err := m.run(ctx, "nmcli", args...); err != nil {
		return fmt.Errorf("连接无线网络: %w", err)
	}
	autoConnect := true
	if request.AutoConnect != nil {
		autoConnect = *request.AutoConnect
	}
	saveArgs := []string{"connection", "modify", "id", profileName, "connection.autoconnect", yesNo(autoConnect)}
	if security != "open" {
		// 显式改为系统持有密钥，避免无桌面环境重启后只能向不存在的用户密钥代理索取密码。
		saveArgs = append(
			saveArgs,
			"802-11-wireless-security.psk-flags", "0",
			"802-11-wireless-security.psk", request.Password,
		)
	}
	if _, err := m.run(ctx, "nmcli", saveArgs...); err != nil {
		return fmt.Errorf("保存 WiFi 自动连接设置: %w", err)
	}
	return nil
}

// SetAutoConnect 更新已保存客户端网络的自动连接策略，不影响当前连接。
func (m *Manager) SetAutoConnect(ctx context.Context, update AutoConnectUpdate) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if !validUUID(update.UUID) {
		return invalidInput("连接 UUID 无效")
	}
	profiles, err := m.wirelessProfiles(ctx)
	if err != nil {
		return err
	}
	for _, item := range profiles {
		if item.UUID == update.UUID && item.Mode != "ap" {
			return m.setAutoConnect(ctx, update.UUID, update.Enabled)
		}
	}
	return invalidInput("未找到可管理的已保存 WiFi")
}

func (m *Manager) setAutoConnect(ctx context.Context, uuid string, enabled bool) error {
	if _, err := m.run(ctx, "nmcli", "connection", "modify", "uuid", uuid, "connection.autoconnect", yesNo(enabled)); err != nil {
		return fmt.Errorf("更新 WiFi 自动连接设置: %w", err)
	}
	return nil
}

// UpdateIPv4 将 DHCP 或静态 IPv4 设置保存到指定 WiFi 配置；当前连接会立即重新激活以应用新地址。
func (m *Manager) UpdateIPv4(ctx context.Context, update IPv4Update) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if !validUUID(update.UUID) {
		return invalidInput("连接 UUID 无效")
	}
	normalized, err := normalizeIPv4Update(update)
	if err != nil {
		return err
	}
	profiles, err := m.wirelessProfiles(ctx)
	if err != nil {
		return err
	}
	selected := profile{}
	for _, item := range profiles {
		if item.UUID == update.UUID {
			selected = item
			break
		}
	}
	if selected.UUID == "" || selected.Mode == "ap" {
		return invalidInput("未找到可管理的已保存 WiFi")
	}
	args := []string{"connection", "modify", "uuid", update.UUID}
	if normalized.Mode == "dhcp" {
		args = append(args,
			"ipv4.method", "auto",
			"ipv4.addresses", "",
			"ipv4.gateway", "",
			"ipv4.dns", "",
			"ipv4.ignore-auto-dns", "no",
		)
	} else {
		prefix, _ := subnetMaskPrefix(normalized.SubnetMask)
		args = append(args,
			"ipv4.method", "manual",
			"ipv4.addresses", normalized.Address+"/"+strconv.Itoa(prefix),
			"ipv4.gateway", normalized.Gateway,
			"ipv4.dns", strings.Join(normalized.DNS, ","),
			"ipv4.ignore-auto-dns", "yes",
		)
	}
	if _, err := m.run(ctx, "nmcli", args...); err != nil {
		return fmt.Errorf("保存 WiFi IPv4 设置: %w", err)
	}
	if !selected.Active {
		return nil
	}
	wifiInterface, _, wifiAvailable, _ := m.networkInterfaces(ctx, profiles)
	if !wifiAvailable || wifiInterface == "" {
		return errors.New("无法识别当前 WiFi 接口，IPv4 设置已保存但尚未应用")
	}
	if _, err := m.run(ctx, "nmcli", "--wait", "15", "connection", "up", "uuid", update.UUID, "ifname", wifiInterface); err != nil {
		return fmt.Errorf("应用当前 WiFi IPv4 设置: %w", err)
	}
	return nil
}

// Forget 删除指定的客户端连接配置，但不允许删除 AP 配置。
func (m *Manager) Forget(ctx context.Context, uuid string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if !validUUID(uuid) {
		return invalidInput("连接 UUID 无效")
	}
	profiles, err := m.wirelessProfiles(ctx)
	if err != nil {
		return err
	}
	selected := profile{}
	for _, item := range profiles {
		if item.UUID == uuid {
			selected = item
			break
		}
	}
	if selected.UUID == "" || selected.Mode == "ap" {
		return invalidInput("未找到可删除的已保存 WiFi")
	}
	if selected.Active {
		return invalidInput("不能忘记当前正在使用的 WiFi")
	}
	if _, err := m.run(ctx, "nmcli", "connection", "delete", "uuid", uuid); err != nil {
		return fmt.Errorf("忘记无线网络: %w", err)
	}
	return nil
}

// APSettings 返回当前热点配置并隐藏密码。
func (m *Manager) APSettings(ctx context.Context) (APSettings, error) {
	profiles, err := m.wirelessProfiles(ctx)
	if err != nil {
		return APSettings{}, err
	}
	_, apInterface, _, apAvailable := m.networkInterfaces(ctx, profiles)
	for _, profile := range profiles {
		if profile.Mode != "ap" {
			continue
		}
		return APSettings{
			Available: apAvailable, Enabled: profile.Active, ConnectionName: profile.Name, UUID: profile.UUID,
			Interface: apInterface,
			SSID:      profile.SSID, Band: profile.Band, Channel: profile.Channel,
			Security: profile.Security, Address: profile.Address, AutoConnect: profile.AutoConnect,
			Sharing: profile.IPv4Method == "shared",
		}.withInterfaceDetails(m.interfaceDetails(ctx, apInterface)), nil
	}
	return APSettings{Available: false}, nil
}

func (s APSettings) withInterfaceDetails(macAddress, ipAddress string) APSettings {
	s.MACAddress = macAddress
	if ipAddress != "" {
		s.Address = ipAddress
	}
	return s
}

func (m *Manager) interfaceDetails(ctx context.Context, interfaceName string) (string, string) {
	output, err := m.run(ctx, "nmcli", "--escape", "no", "-g", "GENERAL.HWADDR,IP4.ADDRESS", "device", "show", interfaceName)
	if err != nil {
		return "", ""
	}
	return parseInterfaceDetails(string(output))
}

func parseInterfaceDetails(output string) (string, string) {
	var macAddress string
	var ipAddress string
	for _, value := range strings.Split(strings.TrimSpace(output), "\n") {
		value = strings.TrimSpace(value)
		if macAddress == "" && validBSSID(value) {
			macAddress = strings.ToUpper(value)
			continue
		}
		if prefix, err := netip.ParsePrefix(value); err == nil && prefix.Addr().Is4() {
			ipAddress = value
			break
		}
	}
	return macAddress, ipAddress
}

type networkDevice struct {
	Name      string
	Connected bool
}

func (m *Manager) networkInterfaces(ctx context.Context, profiles []profile) (string, string, bool, bool) {
	apInterface := m.apInterface(profiles)
	activeClientInterface := ""
	for _, item := range profiles {
		if item.Mode != "ap" && item.Active && item.Device != "" {
			activeClientInterface = item.Device
			break
		}
	}
	output, err := m.run(ctx, "nmcli", "-t", "--escape", "yes", "-f", "DEVICE,TYPE,STATE", "device", "status")
	devices := []networkDevice{}
	if err == nil {
		devices = parseWiFiDevices(string(output))
	}
	deviceExists := func(name string) bool {
		for _, item := range devices {
			if item.Name == name {
				return true
			}
		}
		return false
	}
	apAvailable := deviceExists(apInterface)
	if !apAvailable {
		for _, item := range profiles {
			if item.Mode == "ap" && item.Active && item.Device != "" {
				apAvailable = true
				break
			}
		}
	}
	if m.configuredWiFiInterface != "" {
		available := deviceExists(m.configuredWiFiInterface) || activeClientInterface == m.configuredWiFiInterface
		return m.configuredWiFiInterface, apInterface, available, apAvailable
	}
	if activeClientInterface != "" {
		return activeClientInterface, apInterface, true, apAvailable
	}
	for _, item := range devices {
		if item.Name != apInterface && item.Connected {
			return item.Name, apInterface, true, apAvailable
		}
	}
	for _, item := range devices {
		if item.Name != apInterface {
			return item.Name, apInterface, true, apAvailable
		}
	}
	// 单网卡热点模式下，AP 配置占用的仍是真实无线设备。
	// 保留它用于展示，并由上层在连接前要求用户确认关闭热点。
	if deviceExists(apInterface) {
		return apInterface, apInterface, true, apAvailable
	}
	for _, item := range profiles {
		if item.Mode == "ap" && item.Active && item.Device != "" {
			return item.Device, apInterface, true, apAvailable
		}
	}
	return "", apInterface, false, apAvailable
}

func (m *Manager) apInterface(profiles []profile) string {
	for _, item := range profiles {
		if item.Mode == "ap" && item.Active && item.Device != "" {
			return item.Device
		}
	}
	for _, item := range profiles {
		if item.Mode == "ap" && item.ConfiguredDevice != "" {
			return item.ConfiguredDevice
		}
	}
	if m.configuredAPInterface != "" {
		return m.configuredAPInterface
	}
	return defaultAPInterface
}

func parseWiFiDevices(output string) []networkDevice {
	devices := make([]networkDevice, 0)
	for _, line := range strings.Split(strings.TrimSpace(output), "\n") {
		fields := splitNMCLI(line)
		if len(fields) != 3 || fields[0] == "" || fields[1] != "wifi" {
			continue
		}
		state := strings.ToLower(fields[2])
		devices = append(devices, networkDevice{
			Name:      fields[0],
			Connected: strings.HasPrefix(state, "connected") || strings.HasPrefix(state, "100"),
		})
	}
	return devices
}

func wirelessInterfacesSharePHY(left, right string) (bool, bool) {
	if left == "" || right == "" {
		return true, false
	}
	if left == right {
		return true, true
	}
	leftPHY, leftErr := filepath.EvalSymlinks(filepath.Join("/sys/class/net", left, "phy80211"))
	rightPHY, rightErr := filepath.EvalSymlinks(filepath.Join("/sys/class/net", right, "phy80211"))
	if leftErr != nil || rightErr != nil {
		return true, false
	}
	return filepath.Clean(leftPHY) == filepath.Clean(rightPHY), true
}

// UpdateAP 校验并更新现有热点；空密码表示保留原密码。
func (m *Manager) UpdateAP(ctx context.Context, update APUpdate) error {
	m.mu.Lock()
	locked := true
	defer func() {
		if locked {
			m.mu.Unlock()
		}
	}()
	if err := validateSSID(update.SSID); err != nil {
		return err
	}
	if update.Password != "" {
		if err := validatePassword(update.Password); err != nil {
			return err
		}
	}
	if !validBandChannel(update.Band, update.Channel) {
		return invalidInput("热点频段或信道无效")
	}
	settings, err := m.APSettings(ctx)
	if err != nil {
		return err
	}
	if !settings.Available || !validUUID(settings.UUID) {
		return invalidInput("未找到可管理的 AP 配置")
	}
	args := []string{"connection", "modify", "uuid", settings.UUID,
		"802-11-wireless.ssid", update.SSID,
		"802-11-wireless.band", update.Band,
		"802-11-wireless.channel", strconv.Itoa(update.Channel),
		"connection.autoconnect", yesNo(update.AutoConnect),
		"ipv4.method", "shared",
	}
	if update.Password != "" {
		args = append(args, "802-11-wireless-security.key-mgmt", "wpa-psk", "802-11-wireless-security.psk", update.Password)
	}
	if _, err := m.run(ctx, "nmcli", args...); err != nil {
		return fmt.Errorf("保存热点配置: %w", err)
	}
	m.apUpdateGeneration++
	generation := m.apUpdateGeneration
	if !update.Enabled && !settings.Enabled {
		return nil
	}
	if !update.Enabled {
		// 管理页面通常正通过该热点访问；响应发出后再关闭，避免请求上下文取消导致 nmcli 半途被终止。
		m.mu.Unlock()
		locked = false
		m.schedule(apDisableDelay, func() {
			m.mu.Lock()
			defer m.mu.Unlock()
			if generation != m.apUpdateGeneration {
				return
			}
			operationCtx, cancel := context.WithTimeout(context.Background(), commandTimeout)
			defer cancel()
			if _, err := m.runner.Run(operationCtx, "nmcli", "--wait", "15", "connection", "down", "uuid", settings.UUID); err != nil {
				m.logger.Error("关闭无线热点失败", "uuid", settings.UUID, "error", err)
			}
		})
		return nil
	}
	operationArgs := []string{"--wait", "15", "connection", "up", "uuid", settings.UUID}
	if settings.Interface != "" {
		operationArgs = append(operationArgs, "ifname", settings.Interface)
	}
	if _, err := m.run(ctx, "nmcli", operationArgs...); err != nil {
		return fmt.Errorf("应用热点配置: %w", err)
	}
	return nil
}

// SchedulePower 在 HTTP 响应发出后执行关机或重启。
func (m *Manager) SchedulePower(action string) error {
	command := ""
	switch action {
	case "reboot":
		command = "reboot"
	case "shutdown":
		command = "poweroff"
	default:
		return invalidInput("电源操作无效")
	}
	m.schedule(2*time.Second, func() {
		ctx, cancel := context.WithTimeout(context.Background(), commandTimeout)
		defer cancel()
		if _, err := m.runner.Run(ctx, "systemctl", command); err != nil {
			m.logger.Error("执行设备电源操作失败", "action", action, "error", err)
		}
	})
	return nil
}

type profile struct {
	Name, UUID, SSID, Mode, Band, Security, Address, IPv4Method string
	Gateway                                                     string
	DNS                                                         []string
	Device, ConfiguredDevice                                    string
	Channel                                                     int
	Active, AutoConnect, RequiresPassword                       bool
}

func (m *Manager) wirelessProfiles(ctx context.Context) ([]profile, error) {
	output, err := m.run(ctx, "nmcli", "-t", "--escape", "yes", "-f", "NAME,UUID,TYPE,DEVICE,AUTOCONNECT", "connection", "show")
	if err != nil {
		return nil, fmt.Errorf("读取 NetworkManager 连接: %w", err)
	}
	profiles := make([]profile, 0)
	for _, line := range strings.Split(strings.TrimSpace(string(output)), "\n") {
		fields := splitNMCLI(line)
		if len(fields) != 5 || fields[2] != "802-11-wireless" {
			continue
		}
		item := profile{Name: fields[0], UUID: fields[1], Device: fields[3], Active: fields[3] != "", AutoConnect: fields[4] == "yes"}
		item, err = m.profileDetails(ctx, item)
		if err != nil {
			return nil, err
		}
		if item.Mode == "ap" {
			item.ConfiguredDevice = m.profileInterface(ctx, item.UUID)
		}
		profiles = append(profiles, item)
	}
	return profiles, nil
}

func (m *Manager) profileDetails(ctx context.Context, item profile) (profile, error) {
	output, err := m.run(ctx, "nmcli", "-g", "802-11-wireless.mode,802-11-wireless.ssid,802-11-wireless.band,802-11-wireless.channel,802-11-wireless-security.key-mgmt,ipv4.method,ipv4.addresses", "connection", "show", "uuid", item.UUID)
	if err != nil {
		return profile{}, fmt.Errorf("读取无线连接 %s: %w", item.Name, err)
	}
	lines := strings.Split(strings.TrimSuffix(string(output), "\n"), "\n")
	for len(lines) < 7 {
		lines = append(lines, "")
	}
	item.Mode = lines[0]
	item.SSID = lines[1]
	item.Band = lines[2]
	item.Channel, _ = strconv.Atoi(lines[3])
	item.Security = lines[4]
	item.IPv4Method = lines[5]
	item.Address = lines[6]
	if item.Mode != "ap" && profileUsesPSK(item.Security) {
		available, known := m.profilePSKAvailable(ctx, item.UUID)
		item.RequiresPassword = known && !available
	}
	item.Gateway, item.DNS, err = m.profileIPv4Details(ctx, item.UUID)
	if err != nil {
		return profile{}, err
	}
	return item, nil
}

func (m *Manager) profilePSKAvailable(ctx context.Context, uuid string) (bool, bool) {
	output, err := m.run(
		ctx,
		"nmcli",
		"--show-secrets",
		"-g", "802-11-wireless-security.psk-flags,802-11-wireless-security.psk",
		"connection", "show", "uuid", uuid,
	)
	if err != nil {
		// 某些旧版 NetworkManager 不支持 --show-secrets；密钥状态未知时继续沿用原连接行为，
		// 不能因为一个增强诊断查询失败而让全部 WiFi 和 AP 管理不可用。
		return false, false
	}
	normalized := strings.TrimSuffix(string(output), "\n")
	if normalized == "" {
		// 兼容不支持查询密钥标志的旧 NetworkManager；未知状态不能误报为缺少密码。
		return false, false
	}
	lines := strings.Split(normalized, "\n")
	flagsText := strings.TrimSpace(lines[0])
	psk := ""
	if len(lines) > 1 {
		psk = strings.TrimSpace(strings.Join(lines[1:], "\n"))
	}
	flags := 0
	if first := strings.Fields(flagsText); len(first) > 0 {
		flags, _ = strconv.Atoi(first[0])
	}
	agentOwnedOrNotSaved := flags&0x3 != 0 ||
		strings.Contains(strings.ToLower(flagsText), "agent-owned") ||
		strings.Contains(strings.ToLower(flagsText), "not-saved")
	return psk != "" && !agentOwnedOrNotSaved, true
}

func profileUsesPSK(security string) bool {
	normalized := strings.ToLower(strings.TrimSpace(security))
	return normalized == "wpa-psk" || normalized == "sae"
}

func (m *Manager) profileIPv4Details(ctx context.Context, uuid string) (string, []string, error) {
	output, err := m.run(ctx, "nmcli", "-g", "ipv4.gateway,ipv4.dns", "connection", "show", "uuid", uuid)
	if err != nil {
		return "", nil, fmt.Errorf("读取无线连接 IPv4 配置: %w", err)
	}
	lines := strings.Split(strings.TrimSuffix(string(output), "\n"), "\n")
	if len(lines) == 0 {
		return "", []string{}, nil
	}
	gateway := strings.TrimSpace(lines[0])
	dns := parseIPv4List(strings.Join(lines[1:], ","))
	return gateway, dns, nil
}

func profileIPv4Settings(item profile) IPv4Settings {
	settings := IPv4Settings{Mode: "dhcp", DNS: []string{}}
	if strings.ToLower(strings.TrimSpace(item.IPv4Method)) != "manual" {
		return settings
	}
	settings.Mode = "static"
	prefix, err := netip.ParsePrefix(strings.TrimSpace(strings.Split(item.Address, ",")[0]))
	if err == nil && prefix.Addr().Is4() {
		settings.Address = prefix.Addr().String()
		settings.SubnetMask = subnetMaskFromPrefix(prefix.Bits())
	}
	if gateway, err := netip.ParseAddr(strings.TrimSpace(item.Gateway)); err == nil && gateway.Is4() {
		settings.Gateway = gateway.String()
	}
	settings.DNS = append(settings.DNS, item.DNS...)
	return settings
}

func (m *Manager) profileInterface(ctx context.Context, uuid string) string {
	output, err := m.run(ctx, "nmcli", "-g", "connection.interface-name", "connection", "show", "uuid", uuid)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(output))
}

func (m *Manager) cachedAccessPoints(ctx context.Context, interfaceName string, saved map[string]bool) ([]AccessPoint, error) {
	output, err := m.run(ctx, "nmcli", "-t", "--escape", "yes", "-f", "IN-USE,SSID,BSSID,SIGNAL,CHAN,SECURITY", "device", "wifi", "list", "ifname", interfaceName, "--rescan", "no")
	if err != nil {
		return nil, fmt.Errorf("读取无线网络: %w", err)
	}
	return parseAccessPoints(string(output), saved), nil
}

func (m *Manager) resolveAccessPoint(ctx context.Context, interfaceName, ssid, bssid string, channel int) (AccessPoint, error) {
	if err := validateSSID(ssid); err != nil {
		return AccessPoint{}, err
	}
	if bssid != "" && !validBSSID(bssid) {
		return AccessPoint{}, invalidInput("WiFi BSSID 无效")
	}
	items, err := m.cachedAccessPoints(ctx, interfaceName, map[string]bool{})
	if err != nil {
		return AccessPoint{}, err
	}
	for _, item := range items {
		if item.SSID != ssid {
			continue
		}
		if bssid != "" && !strings.EqualFold(item.BSSID, bssid) {
			continue
		}
		if channel != 0 && item.Channel != channel {
			continue
		}
		return item, nil
	}
	return AccessPoint{}, invalidInput("最近的扫描结果中未找到目标 WiFi，请重新扫描")
}

func (m *Manager) alignAPChannel(ctx context.Context, status WiFiStatus, channel int) error {
	band, ok := concurrentBand(channel)
	if !ok {
		return invalidInput("上游 WiFi 信道不支持 AP+STA 并发")
	}
	settings, err := m.APSettings(ctx)
	if err != nil {
		return err
	}
	if !settings.Available || !settings.Enabled || settings.Channel == channel {
		return nil
	}
	upstreamUUID := ""
	for _, network := range status.SavedNetworks {
		if network.Name == status.CurrentConnection {
			upstreamUUID = network.UUID
			break
		}
	}
	restoreUpstream := func(cause error) error {
		if upstreamUUID == "" {
			return cause
		}
		if _, restoreErr := m.run(ctx, "nmcli", "--wait", "15", "connection", "up", "uuid", upstreamUUID, "ifname", status.Interface); restoreErr != nil {
			return errors.Join(cause, fmt.Errorf("恢复原上游 WiFi: %w", restoreErr))
		}
		return cause
	}
	if status.CurrentConnection != "" {
		if _, err := m.run(ctx, "nmcli", "device", "disconnect", status.Interface); err != nil {
			if !isNMDeviceInactive(err) {
				return fmt.Errorf("断开原上游 WiFi: %w", err)
			}
		}
	}
	if _, err := m.run(
		ctx,
		"nmcli",
		"connection", "modify", "uuid", settings.UUID,
		"802-11-wireless.band", band,
		"802-11-wireless.channel", strconv.Itoa(channel),
	); err != nil {
		return restoreUpstream(fmt.Errorf("同步热点信道: %w", err))
	}
	if _, err := m.run(ctx, "nmcli", "--wait", "20", "connection", "up", "uuid", settings.UUID, "ifname", settings.Interface); err != nil {
		cause := fmt.Errorf("在上游 WiFi 信道重启热点: %w", err)
		if _, restoreErr := m.run(
			ctx,
			"nmcli",
			"connection", "modify", "uuid", settings.UUID,
			"802-11-wireless.band", settings.Band,
			"802-11-wireless.channel", strconv.Itoa(settings.Channel),
		); restoreErr != nil {
			cause = errors.Join(cause, fmt.Errorf("恢复热点原信道配置: %w", restoreErr))
		} else if _, restoreErr := m.run(ctx, "nmcli", "--wait", "20", "connection", "up", "uuid", settings.UUID, "ifname", settings.Interface); restoreErr != nil {
			cause = errors.Join(cause, fmt.Errorf("恢复热点原信道: %w", restoreErr))
		}
		return restoreUpstream(cause)
	}
	return nil
}

func isNMDeviceInactive(err error) bool {
	var commandError *system.CommandError
	return errors.As(err, &commandError) && commandError.Code == 6 &&
		strings.Contains(strings.ToLower(commandError.Output), "not active")
}

func (m *Manager) run(parent context.Context, name string, args ...string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(parent, commandTimeout)
	defer cancel()
	return m.runner.Run(ctx, name, args...)
}

func parseAccessPoints(output string, saved map[string]bool) []AccessPoint {
	strongest := make(map[string]AccessPoint)
	for _, line := range strings.Split(strings.TrimSpace(output), "\n") {
		fields := splitNMCLI(line)
		if len(fields) != 6 || fields[1] == "" || !validBSSID(fields[2]) {
			continue
		}
		signal, errSignal := strconv.Atoi(fields[3])
		channel, errChannel := strconv.Atoi(fields[4])
		if errSignal != nil || errChannel != nil {
			continue
		}
		candidate := AccessPoint{
			SSID: fields[1], BSSID: strings.ToUpper(fields[2]), Signal: signal, Band: channelBand(channel), Channel: channel,
			Security: displaySecurity(fields[5]), Saved: saved[fields[1]], Active: fields[0] == "*",
		}
		current, exists := strongest[candidate.SSID]
		// 当前连接必须优先于同名的强信号接入点，否则页面会把已连接网络误报为未连接，
		// 后续连接还可能把单射频 AP+STA 组合切换到另一个信道。
		if !exists || candidate.Active && !current.Active || candidate.Active == current.Active && candidate.Signal > current.Signal {
			strongest[candidate.SSID] = candidate
		}
	}
	items := make([]AccessPoint, 0, len(strongest))
	for _, item := range strongest {
		items = append(items, item)
	}
	slicesSortAccessPoints(items)
	return items
}

func slicesSortAccessPoints(items []AccessPoint) {
	for i := 1; i < len(items); i++ {
		for j := i; j > 0 && items[j].Signal > items[j-1].Signal; j-- {
			items[j], items[j-1] = items[j-1], items[j]
		}
	}
}

func splitNMCLI(line string) []string {
	fields := []string{}
	var current strings.Builder
	escaped := false
	for _, char := range line {
		if escaped {
			current.WriteRune(char)
			escaped = false
			continue
		}
		if char == '\\' {
			escaped = true
			continue
		}
		if char == ':' {
			fields = append(fields, current.String())
			current.Reset()
			continue
		}
		current.WriteRune(char)
	}
	fields = append(fields, current.String())
	return fields
}

func validateSSID(value string) error {
	if value == "" || len([]byte(value)) > 32 || !utf8.ValidString(value) {
		return invalidInput("SSID 必须是 1 到 32 字节的有效文本")
	}
	return nil
}

func validatePassword(value string) error {
	if len([]byte(value)) < 8 || len([]byte(value)) > 63 || !utf8.ValidString(value) {
		return invalidInput("WiFi 密码必须是 8 到 63 字节")
	}
	return nil
}

func normalizeIPv4Update(update IPv4Update) (IPv4Update, error) {
	update.Mode = strings.ToLower(strings.TrimSpace(update.Mode))
	switch update.Mode {
	case "dhcp":
		update.Address, update.SubnetMask, update.Gateway, update.DNS = "", "", "", []string{}
		return update, nil
	case "static":
	default:
		return IPv4Update{}, invalidInput("IPv4 获取方式仅支持 DHCP 或静态地址")
	}
	address, err := parseUsableIPv4(update.Address, "IP 地址")
	if err != nil {
		return IPv4Update{}, err
	}
	prefix, err := subnetMaskPrefix(update.SubnetMask)
	if err != nil {
		return IPv4Update{}, err
	}
	if !isUsableHostAddress(address, prefix) {
		return IPv4Update{}, invalidInput("IP 地址不能是当前子网的网络地址或广播地址")
	}
	gateway, err := parseUsableIPv4(update.Gateway, "网关")
	if err != nil {
		return IPv4Update{}, err
	}
	if !netip.PrefixFrom(address, prefix).Contains(gateway) || !isUsableHostAddress(gateway, prefix) || gateway == address {
		return IPv4Update{}, invalidInput("网关必须是与 IP 地址同一子网内的可用地址")
	}
	dns := make([]string, 0, len(update.DNS))
	if len(update.DNS) > 4 {
		return IPv4Update{}, invalidInput("最多可配置 4 个 DNS 服务器")
	}
	for _, value := range update.DNS {
		address, err := parseUsableIPv4(value, "DNS 服务器")
		if err != nil {
			return IPv4Update{}, err
		}
		dns = append(dns, address.String())
	}
	update.Address = address.String()
	update.SubnetMask = subnetMaskFromPrefix(prefix)
	update.Gateway = gateway.String()
	update.DNS = dns
	return update, nil
}

func parseUsableIPv4(value, field string) (netip.Addr, error) {
	address, err := netip.ParseAddr(strings.TrimSpace(value))
	if err != nil || !address.Is4() || address.IsUnspecified() || address.IsLoopback() || address.IsMulticast() || address.IsLinkLocalUnicast() {
		return netip.Addr{}, invalidInput(field + "必须是可用的 IPv4 地址")
	}
	return address, nil
}

func subnetMaskPrefix(value string) (int, error) {
	mask, err := netip.ParseAddr(strings.TrimSpace(value))
	if err != nil || !mask.Is4() {
		return 0, invalidInput("子网掩码必须是 IPv4 地址")
	}
	bytes := mask.As4()
	prefix := 0
	seenZero := false
	for _, value := range bytes {
		for bit := 7; bit >= 0; bit-- {
			one := value&(1<<bit) != 0
			if !one {
				seenZero = true
				continue
			}
			if seenZero {
				return 0, invalidInput("子网掩码必须连续")
			}
			prefix++
		}
	}
	if prefix < 1 || prefix > 30 {
		return 0, invalidInput("子网掩码仅支持 /1 到 /30")
	}
	return prefix, nil
}

func subnetMaskFromPrefix(prefix int) string {
	if prefix < 0 || prefix > 32 {
		return ""
	}
	mask := uint32(0)
	if prefix > 0 {
		mask = ^uint32(0) << (32 - prefix)
	}
	return fmt.Sprintf("%d.%d.%d.%d", byte(mask>>24), byte(mask>>16), byte(mask>>8), byte(mask))
}

func isUsableHostAddress(address netip.Addr, prefix int) bool {
	if prefix >= 31 {
		return true
	}
	bytes := address.As4()
	value := uint32(bytes[0])<<24 | uint32(bytes[1])<<16 | uint32(bytes[2])<<8 | uint32(bytes[3])
	mask := ^uint32(0) << (32 - prefix)
	network := value & mask
	broadcast := network | ^mask
	return value != network && value != broadcast
}

func parseIPv4List(value string) []string {
	items := make([]string, 0)
	for _, raw := range strings.FieldsFunc(value, func(char rune) bool { return char == ',' || char == ';' || char == ' ' || char == '\t' || char == '\n' }) {
		address, err := netip.ParseAddr(strings.TrimSpace(raw))
		if err == nil && address.Is4() {
			items = append(items, address.String())
		}
	}
	return items
}

func validUUID(value string) bool {
	if len(value) != 36 {
		return false
	}
	for index, char := range value {
		if index == 8 || index == 13 || index == 18 || index == 23 {
			if char != '-' {
				return false
			}
			continue
		}
		if !((char >= '0' && char <= '9') || (char >= 'a' && char <= 'f') || (char >= 'A' && char <= 'F')) {
			return false
		}
	}
	return true
}

func validBSSID(value string) bool {
	if len(value) != 17 {
		return false
	}
	for index, char := range value {
		if index%3 == 2 {
			if char != ':' {
				return false
			}
			continue
		}
		if !((char >= '0' && char <= '9') || (char >= 'a' && char <= 'f') || (char >= 'A' && char <= 'F')) {
			return false
		}
	}
	return true
}

func concurrentBand(channel int) (string, bool) {
	if channel >= 1 && channel <= 13 {
		return "bg", true
	}
	if channel > 0 && validBandChannel("a", channel) {
		return "a", true
	}
	return "", false
}

func validBandChannel(band string, channel int) bool {
	if band == "a" {
		switch channel {
		case 0, 36, 40, 44, 48, 149, 153, 157, 161, 165:
			return true
		default:
			return false
		}
	}
	if band == "bg" {
		return channel >= 0 && channel <= 13
	}
	return false
}

func normalizeSecurity(value string) string {
	upper := strings.ToUpper(strings.TrimSpace(value))
	if upper == "" || upper == "--" || upper == "OPEN" {
		return "open"
	}
	if strings.Contains(upper, "WPA") && !strings.Contains(upper, "802.1X") && !strings.Contains(upper, "EAP") {
		return "wpa"
	}
	return "unsupported"
}

func displaySecurity(value string) string {
	if normalizeSecurity(value) == "open" {
		return "OPEN"
	}
	return strings.TrimSpace(value)
}

func channelBand(channel int) string {
	if channel > 0 && channel <= 14 {
		return "2.4 GHz"
	}
	if channel > 14 {
		return "5 GHz"
	}
	return "未知"
}

func yesNo(value bool) string {
	if value {
		return "yes"
	}
	return "no"
}

func findSavedNetwork(networks []SavedNetwork, uuid string) (SavedNetwork, bool) {
	for _, network := range networks {
		if network.UUID == uuid {
			return network, true
		}
	}
	return SavedNetwork{}, false
}

func invalidInput(message string) error { return &InputError{Message: message} }
