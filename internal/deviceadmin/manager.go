// Package deviceadmin 管理 NetworkManager 无线配置和设备电源操作。
package deviceadmin

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/netip"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/wen-ryon/teslabox/internal/regulatory"
	"github.com/wen-ryon/teslabox/internal/system"
)

const (
	defaultAPInterface      = "ap0"
	managedUpstreamPrefix   = "TeslaBox-Upstream-"
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
	Available             bool   `json:"available"`
	Interface             string `json:"interface"`
	MACAddress            string `json:"mac_address,omitempty"`
	IPAddress             string `json:"ip_address,omitempty"`
	CurrentConnection     string `json:"current_connection,omitempty"`
	ConnectionState       string `json:"connection_state"`
	ConnectionUUID        string `json:"connection_uuid,omitempty"`
	PendingConnection     string `json:"pending_connection,omitempty"`
	currentConnectionUUID string
	APActive              bool           `json:"ap_active"`
	ConcurrentMode        bool           `json:"concurrent_mode"`
	ChannelCoupled        bool           `json:"channel_coupled"`
	SingleRadioNotice     string         `json:"single_radio_notice,omitempty"`
	SavedNetworks         []SavedNetwork `json:"saved_networks"`
	AvailableNetworks     []AccessPoint  `json:"available_networks"`
	Recovery              RecoveryStatus `json:"recovery"`
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
	Available             bool                  `json:"available"`
	Enabled               bool                  `json:"enabled"`
	DesiredEnabled        bool                  `json:"desired_enabled"`
	State                 string                `json:"state"`
	ReasonCode            string                `json:"reason_code,omitempty"`
	StatusMessage         string                `json:"status_message,omitempty"`
	SuggestedAction       string                `json:"suggested_action,omitempty"`
	Interface             string                `json:"interface,omitempty"`
	InterfaceRole         string                `json:"interface_role,omitempty"`
	MACAddress            string                `json:"mac_address,omitempty"`
	ConnectionName        string                `json:"connection_name,omitempty"`
	UUID                  string                `json:"uuid,omitempty"`
	SSID                  string                `json:"ssid,omitempty"`
	Band                  string                `json:"band,omitempty"`
	Channel               int                   `json:"channel,omitempty"`
	ActiveBand            string                `json:"active_band,omitempty"`
	ActiveChannel         int                   `json:"active_channel,omitempty"`
	ConcurrencyMode       string                `json:"concurrency_mode,omitempty"`
	MaxConcurrentChannels int                   `json:"max_concurrent_channels,omitempty"`
	CapabilityNotice      string                `json:"capability_notice,omitempty"`
	AllowedChannels       []int                 `json:"allowed_channels,omitempty"`
	Security              string                `json:"security,omitempty"`
	SecurityMode          string                `json:"security_mode,omitempty"`
	ActiveSecurity        string                `json:"active_security,omitempty"`
	SecurityOptions       []APSecurityOption    `json:"security_options"`
	Address               string                `json:"address,omitempty"`
	AutoConnect           bool                  `json:"auto_connect"`
	Sharing               bool                  `json:"sharing"`
	ConflictingProfiles   []APInterfaceConflict `json:"conflicting_profiles,omitempty"`
}

// APInterfaceConflict 描述抢占预留 AP 接口的普通 WiFi 配置，不包含密码等敏感字段。
type APInterfaceConflict struct {
	Name        string `json:"name"`
	UUID        string `json:"uuid"`
	SSID        string `json:"ssid,omitempty"`
	Active      bool   `json:"active"`
	AutoConnect bool   `json:"auto_connect"`
}

// APOperationError 是可以安全展示给管理页面的热点启动失败。
type APOperationError struct {
	Code       string
	Message    string
	Suggestion string
}

// Error 返回面向用户的简体中文错误说明。
func (e *APOperationError) Error() string { return e.Message }

// ConnectRequest 描述连接已保存网络或新网络的请求。
type ConnectRequest struct {
	UUID                    string `json:"uuid,omitempty"`
	SSID                    string `json:"ssid,omitempty"`
	BSSID                   string `json:"bssid,omitempty"`
	Channel                 int    `json:"channel,omitempty"`
	Password                string `json:"password,omitempty"`
	Security                string `json:"security,omitempty"`
	AutoConnect             *bool  `json:"auto_connect,omitempty"`
	ConfirmAPDisconnect     bool   `json:"confirm_ap_disconnect"`
	ConfirmCarPlayReconnect bool   `json:"confirm_carplay_reconnect"`
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
	Enabled                 bool   `json:"enabled"`
	SSID                    string `json:"ssid"`
	Password                string `json:"password,omitempty"`
	SecurityMode            string `json:"security_mode,omitempty"`
	Band                    string `json:"band"`
	Channel                 int    `json:"channel"`
	AutoConnect             bool   `json:"auto_connect"`
	ConfirmCarPlayReconnect bool   `json:"confirm_carplay_reconnect,omitempty"`
}

// Manager 串行执行会改变网络状态的命令，避免多个页面操作互相覆盖。
type Manager struct {
	openNetworkBus           func() (ethernetBus, error)
	regulatory               *regulatory.Manager
	carPlayEnrollmentHandoff *carPlayEnrollmentHandoff
	carPlayEnrollmentLive    string
	runner                   system.Runner
	logger                   *slog.Logger
	configuredWiFiInterface  string
	configuredAPInterface    string
	mu                       sync.Mutex
	recoveryMu               sync.RWMutex
	recovery                 RecoveryStatus
	schedule                 func(time.Duration, func())
	apUpdateGeneration       uint64
	monitoredWiFiInterface   string
	manualWiFiDisconnect     bool
	carPlayNetworkHold       *CarPlayNetworkLease
	carPlayStartupAdoption   *carPlayStartupAdoption
	carPlayWiFiRecoveryAfter time.Time
	carPlayVehicleChannels   []int
	carPlayWiFiRecoveryUUID  string
	carPlayWiFiFailures      map[string]time.Time
	carPlayDomain            string
	carPlaySettingsPath      string
	carPlayPublicAddress     string
	carPlayDNSDirectory      string
	carPlayRecoveryUncertain bool
	carPlayWiFiReconnect     bool
	carPlayInternetProbe     func(context.Context, string, []string) bool
	carPlayPANProfileCreate  func(context.Context, string, []byte) error
	carPlayPANInterface      func(context.Context, string) (string, error)
	watchNetworkEvents       networkEventWatch
	sameWirelessPHY          func(string, string) (bool, bool)
	wirelessPHYInfo          func(context.Context, string) (string, error)
}

// NetworkConfig 允许管理员固定无线接口；客户端接口留空时会根据 NetworkManager 自动发现。
type NetworkConfig struct {
	Regulatory           *regulatory.Manager
	CarPlaySettingsPath  string
	WiFiInterface        string
	APInterface          string
	CarPlayDomain        string
	CarPlayPublicAddress string
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
	manager := &Manager{
		regulatory: networkConfig.Regulatory,
		runner:     runner, logger: logger,
		configuredWiFiInterface: strings.TrimSpace(networkConfig.WiFiInterface),
		configuredAPInterface:   strings.TrimSpace(networkConfig.APInterface),
		carPlayDomain:           strings.TrimSpace(networkConfig.CarPlayDomain),
		carPlaySettingsPath:     networkConfig.CarPlaySettingsPath,
		carPlayPublicAddress:    strings.TrimSpace(networkConfig.CarPlayPublicAddress),
		carPlayDNSDirectory:     defaultCarPlayDNSDirectory(),
		recovery:                RecoveryStatus{Enabled: true},
		schedule:                func(delay time.Duration, operation func()) { time.AfterFunc(delay, operation) },
		watchNetworkEvents:      watchNetworkManagerEvents,
		sameWirelessPHY:         wirelessInterfacesSharePHY,
	}
	manager.wirelessPHYInfo = manager.readWirelessPHYInfo
	manager.carPlayInternetProbe = probeCarPlayInternet
	manager.carPlayPANProfileCreate = createCarPlayPANProfile
	manager.carPlayPANInterface = activeCarPlayPANInterface
	if err := manager.loadHandoff(); err != nil {
		manager.carPlayRecoveryUncertain = true
		logger.Error("CarPlay 热点交接记录无法确认，暂停网络变更")
	}
	return manager
}

// Run 订阅 NetworkManager 事件，并只在上游 WiFi 状态、BSSID 或信道实际变化时恢复 AP+STA。
// 正常待机不执行轮询，避免车载供电下反复唤醒无线芯片。
func (m *Manager) Run(ctx context.Context) {
	if err := m.clearManagedProfileBSSIDLocks(ctx); err != nil {
		m.logger.Warn("清理旧版上游 WiFi BSSID 锁定失败", "error", err)
	}
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
			// 设备清单变化后旧接口名可能已经失效。事件合并与监听重试由两个计时器驱动，
			// 不能依赖它们恰好按某个顺序触发；重建监听前必须主动刷新目标接口。
			refreshCtx, cancel := context.WithTimeout(ctx, recoveryTimeout)
			if err := m.refreshMonitoredInterface(refreshCtx); err != nil {
				m.logger.Warn("重新识别 NetworkManager 无线接口失败", "error", err)
			}
			cancel()
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

func (m *Manager) refreshMonitoredInterface(ctx context.Context) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	status, err := m.WiFiStatus(ctx)
	if err != nil {
		return err
	}
	m.monitoredWiFiInterface = status.Interface
	return nil
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
	status.ConnectionState = "unavailable"
	runtime := wifiConnectionSnapshot{}
	if wifiAvailable {
		status.MACAddress, status.IPAddress = m.interfaceDetails(ctx, wifiInterface)
		runtime = m.wifiConnectionSnapshot(ctx, wifiInterface)
		status.ConnectionState = runtime.state
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
				channelCoupled, known := m.wirelessInterfacesChannelCoupled(ctx, wifiInterface, apInterface)
				// 无法读取拓扑或驱动能力时按单信道处理，避免未知网卡在跨信道并发时反复启动失败。
				status.ChannelCoupled = status.ChannelCoupled || !known || channelCoupled
			}
			continue
		}
		selected := profile.Device == wifiInterface && profile.UUID == runtime.uuid
		if selected {
			status.ConnectionUUID = profile.UUID
		}
		if selected && runtime.state == "connecting" {
			status.PendingConnection = profile.SSID
		}
		if selected && runtime.state == "connected" {
			// NetworkManager 的连接配置名称可以由程序或用户任意命名，不能当作实际 SSID 展示。
			// 例如 TeslaBox 管理的配置名可能是 TeslaBox-Upstream，但真实接入点仍是家庭 WiFi。
			status.CurrentConnection = profile.SSID
			status.currentConnectionUUID = profile.UUID
		}
		status.SavedNetworks = append(status.SavedNetworks, SavedNetwork{
			Name: profile.Name, UUID: profile.UUID, SSID: profile.SSID,
			Security: profile.Security, Active: selected && runtime.state == "connected", AutoConnect: profile.AutoConnect,
			RequiresPassword: profile.RequiresPassword, IPv4: profileIPv4Settings(profile),
		})
	}
	if runtime.state == "connected" && status.currentConnectionUUID == "" {
		// 设备已经换到另一配置，但配置清单尚未同步；不能将旧清单的任意一项当作已连接。
		status.ConnectionState = "unknown"
	}
	if status.APActive {
		if status.ConcurrentMode {
			if status.ChannelCoupled {
				status.SingleRadioNotice = "AP+STA 并发模式已启用；连接不同信道的上游 WiFi 时，热点会短暂切换到相同信道。"
			} else {
				status.SingleRadioNotice = "当前无线能力允许热点和上游 WiFi 保持各自信道。"
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

	if m.carPlayNetworkReservedLocked() {
		return false, "", nil
	}
	// 健康的STA不代表预留AP仍受保护，D-Bus离线时也要阻止另一接口自动激活。
	if err := m.guardReservedAPAutoConnect(ctx); err != nil {
		return false, "", err
	}
	if m.monitoredWiFiInterface != "" {
		connected, err := m.wifiInterfaceConnected(ctx, m.monitoredWiFiInterface)
		if err == nil && connected {
			return false, "", nil
		}
	}
	return m.reconcileAutoConnectLocked(ctx)
}

func (m *Manager) reconcileAutoConnectLocked(ctx context.Context) (bool, string, error) {
	if m.carPlayNetworkReservedLocked() {
		return false, "", nil
	}
	if err := m.guardReservedAPAutoConnect(ctx); err != nil {
		return false, "", err
	}
	status, err := m.WiFiStatus(ctx)
	if err != nil {
		return false, "", err
	}
	m.monitoredWiFiInterface = status.Interface
	if status.CurrentConnection != "" {
		m.manualWiFiDisconnect = false
	} else if m.manualWiFiDisconnect {
		// 用户主动断开后不由同信道 AP+STA 恢复逻辑再次激活上游。
		return false, "", nil
	}
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
			if !settings.DesiredEnabled || !apRequiresSameChannel(settings.ConcurrencyMode) {
				return false, "", nil
			}
			if settings.State == "running" && settings.ActiveChannel == target.Channel {
				return false, "", nil
			}
			if err := m.ensureAPAfterUpstream(ctx, status, target.Channel); err != nil {
				return false, current.SSID, fmt.Errorf("验证上游信道上的热点: %w", err)
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
	network, found := currentSavedNetwork(status)
	if !found || !network.AutoConnect {
		return SavedNetwork{}, false
	}
	return network, true
}

func currentSavedNetwork(status WiFiStatus) (SavedNetwork, bool) {
	if status.currentConnectionUUID == "" {
		return SavedNetwork{}, false
	}
	return findSavedNetwork(status.SavedNetworks, status.currentConnectionUUID)
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
	if m.carPlayBlocksWiFiLocked() {
		return nil, carPlayNetworkBusy()
	}
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
// 同射频切换先释放旧连接，避免热点和旧上游把目标连接锁在原信道。
func (m *Manager) Connect(ctx context.Context, request ConnectRequest) (err error) {
	if request.ConfirmCarPlayReconnect {
		return m.connectWithCarPlayRestart(ctx, request)
	}
	return m.connectWiFi(ctx, request, false)
}

func (m *Manager) connectWiFi(ctx context.Context, request ConnectRequest, reconnectOwner bool) (err error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.carPlayBluetoothWiFiGuardLocked(); err != nil {
		return err
	}
	if m.carPlayRecoveryUncertain || m.carPlayNetworkHold != nil && m.carPlayBlocksWiFiLocked() || m.carPlayWiFiReconnect && !reconnectOwner {
		return carPlayNetworkBusy()
	}
	defer func() {
		if request.UUID != "" && request.UUID == m.carPlayWiFiRecoveryUUID {
			// 一个目标连接失败只冷却该网络，不能阻止其他已保存网络接替。
			if err != nil {
				if m.carPlayWiFiFailures == nil {
					m.carPlayWiFiFailures = make(map[string]time.Time)
				}
				m.carPlayWiFiFailures[request.UUID] = time.Now().Add(3 * time.Minute)
			} else {
				delete(m.carPlayWiFiFailures, request.UUID)
			}
			m.carPlayWiFiRecoveryUUID = ""
			m.carPlayWiFiRecoveryAfter = time.Time{}
		}
		if err == nil {
			m.manualWiFiDisconnect = false
			// 成功连接已结束上一轮恢复，新断线不能沿用旧失败冷却。
			m.carPlayWiFiRecoveryAfter = time.Time{}
		}
	}()
	status, err := m.WiFiStatus(ctx)
	if err != nil {
		return err
	}
	if !status.Available || status.Interface == "" {
		return ErrWiFiUnavailable
	}
	// 第二个排队的连接请求不能把第一个请求刚建立的同一上游再次断开。
	activeUUID := status.ConnectionUUID
	if activeUUID == "" && request.UUID != "" {
		// 状态快照暂时未知时仍核对实际激活身份；相同请求不能重置正在获取地址的连接。
		if output, queryErr := m.run(ctx, "nmcli", "-g", "GENERAL.CON-UUID", "device", "show", status.Interface); queryErr == nil {
			activeUUID = strings.TrimSpace(string(output))
		}
	}
	unchangedSavedNetwork := request.UUID != "" && request.UUID == activeUUID
	if unchangedSavedNetwork && request.Password == "" && request.BSSID == "" && request.Channel == 0 {
		state, stateErr := m.wifiInterfaceState(ctx, status.Interface)
		if stateErr != nil {
			return stateErr
		}
		if networkManagerActivationInProgress(state) {
			waitCtx, waitCancel := context.WithTimeout(ctx, recoveryTimeout)
			defer waitCancel()
			if err := m.waitWiFiUUIDActivation(waitCtx, status.Interface, request.UUID); err != nil {
				return err
			}
		} else if state != 100 {
			return errors.New("当前无线配置已激活，但设备尚未连接")
		}
		if request.AutoConnect != nil {
			return m.setAutoConnect(ctx, request.UUID, *request.AutoConnect)
		}
		return nil
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
	target, err := m.resolveWiFiSwitchAccessPoint(ctx, status.Interface, request, ssid)
	if err != nil {
		return err
	}
	// 输入错误不能中断已有网络；所有会改动无线状态的操作都在校验之后。
	if request.UUID == "" {
		if err := validateSSID(request.SSID); err != nil {
			return err
		}
		security := normalizeSecurity(request.Security)
		if security != "open" {
			if security != "wpa" {
				return ErrUnsupportedSecurity
			}
			if err := validatePassword(request.Password); err != nil {
				return err
			}
		}
	}
	// 关闭热点可能让发起请求的浏览器离线，切换和回滚不能随 HTTP 取消而中止。
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 90*time.Second)
	defer cancel()
	switchState, err := m.prepareWiFiSwitch(ctx, status, target.Channel)
	defer func() {
		if err != nil && switchState != nil {
			err = errors.Join(err, switchState.rollback(m, status.Interface))
		}
	}()
	if err != nil {
		return err
	}
	switchState.connecting = true
	if request.UUID != "" {
		if err := m.clearProfileBSSIDLock(ctx, request.UUID); err != nil {
			return fmt.Errorf("清除上游 WiFi BSSID 锁定: %w", err)
		}
		activationArgs := []string{"--wait", "15", "connection", "up", "uuid", request.UUID, "ifname", status.Interface}
		if target.BSSID != "" {
			// 只约束本次激活，不能把接入点地址写进配置，否则路由器漫游或更换射频后将无法自动连接。
			activationArgs = append(activationArgs, "ap", target.BSSID)
		}
		err = m.activateSavedWiFi(ctx, request.UUID, status.Interface, activationArgs)
		if err != nil {
			return fmt.Errorf("连接已保存网络: %w", err)
		}
		if err := switchState.restoreAPAfterConnect(ctx, m, status); err != nil {
			return fmt.Errorf("连接上游后恢复热点: %w", err)
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
	profileName := managedUpstreamPrefix + request.SSID
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
	if err := m.activateNewWiFi(ctx, status.Interface, request.SSID, args); err != nil {
		return fmt.Errorf("连接无线网络: %w", err)
	}
	if err := switchState.restoreAPAfterConnect(ctx, m, status); err != nil {
		return fmt.Errorf("连接上游后恢复热点: %w", err)
	}
	autoConnect := true
	if request.AutoConnect != nil {
		autoConnect = *request.AutoConnect
	}
	saveArgs := []string{
		"connection", "modify", "id", profileName,
		"connection.autoconnect", yesNo(autoConnect),
		// device wifi connect 的 bssid 参数可能固化到新配置；连接成功后立即清除，保留后续漫游能力。
		"802-11-wireless.bssid", "",
	}
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

// DisconnectWiFi 只断开当前客户端连接，并保留已保存配置和自动连接策略。
func (m *Manager) DisconnectWiFi(ctx context.Context) error {
	return m.disconnectWiFi(ctx, "")
}

// DisconnectWiFiProfile 防止旧页面的断开按钮误断开用户刚切换的新网络。
func (m *Manager) DisconnectWiFiProfile(ctx context.Context, uuid string) error {
	if !validUUID(uuid) {
		return invalidInput("连接 UUID 无效")
	}
	return m.disconnectWiFi(ctx, uuid)
}

func (m *Manager) disconnectWiFi(ctx context.Context, expectedUUID string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.carPlayBlocksWiFiLocked() {
		return carPlayNetworkBusy()
	}
	status, err := m.WiFiStatus(ctx)
	if err != nil {
		return err
	}
	if !status.Available || status.Interface == "" {
		return ErrWiFiUnavailable
	}
	if expectedUUID != "" && expectedUUID != status.ConnectionUUID {
		return invalidInput("当前 WiFi 已发生变化，请刷新后再断开")
	}
	if status.CurrentConnection == "" && status.ConnectionState != "connecting" {
		return nil
	}
	if _, err := m.run(ctx, "nmcli", "device", "disconnect", status.Interface); err != nil {
		return fmt.Errorf("断开当前 WiFi: %w", err)
	}
	m.manualWiFiDisconnect = true
	return nil
}

func (m *Manager) clearManagedProfileBSSIDLocks(ctx context.Context) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.carPlayNetworkReservedLocked() {
		return nil
	}

	output, err := m.run(ctx, "nmcli", "-t", "--escape", "yes", "-f", "NAME,UUID,TYPE", "connection", "show")
	if err != nil {
		return fmt.Errorf("读取待迁移的 WiFi 配置: %w", err)
	}
	var migrationErrors []error
	for _, line := range strings.Split(strings.TrimSpace(string(output)), "\n") {
		fields := splitNMCLI(line)
		if len(fields) != 3 ||
			(fields[2] != "802-11-wireless" && fields[2] != "wifi") ||
			!strings.HasPrefix(fields[0], managedUpstreamPrefix) ||
			!validUUID(fields[1]) {
			continue
		}
		bssid, readErr := m.run(ctx, "nmcli", "-g", "802-11-wireless.bssid", "connection", "show", "uuid", fields[1])
		if readErr != nil {
			migrationErrors = append(migrationErrors, fmt.Errorf("读取 %s 的 BSSID: %w", fields[0], readErr))
			continue
		}
		if strings.TrimSpace(string(bssid)) == "" {
			continue
		}
		if clearErr := m.clearProfileBSSIDLock(ctx, fields[1]); clearErr != nil {
			migrationErrors = append(migrationErrors, fmt.Errorf("清除 %s 的 BSSID: %w", fields[0], clearErr))
		}
	}
	return errors.Join(migrationErrors...)
}

func (m *Manager) clearProfileBSSIDLock(ctx context.Context, uuid string) error {
	_, err := m.run(ctx, "nmcli", "connection", "modify", "uuid", uuid, "802-11-wireless.bssid", "")
	return err
}

// SetAutoConnect 更新已保存客户端网络的自动连接策略，不影响当前连接。
func (m *Manager) SetAutoConnect(ctx context.Context, update AutoConnectUpdate) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.carPlayBlocksWiFiLocked() {
		return carPlayNetworkBusy()
	}
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
	if m.carPlayBlocksWiFiLocked() {
		return carPlayNetworkBusy()
	}
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
	if m.carPlayRecoveryUncertain || m.carPlayNetworkHold != nil && m.carPlayNetworkHold.Plan.UpstreamUUID == uuid {
		return carPlayNetworkBusy()
	}
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
	if _, err := m.run(ctx, "nmcli", "connection", "delete", "uuid", uuid); err != nil {
		return fmt.Errorf("忘记无线网络: %w", err)
	}
	if selected.Active {
		wifiInterface, _, _, _ := m.networkInterfaces(ctx, profiles)
		if selected.Device == wifiInterface {
			// 删除活动连接由 NetworkManager 一并停用；成功后才抑制程序自身的自动恢复。
			m.manualWiFiDisconnect = true
		}
	}
	return nil
}

// RepairAPInterface 停用并解绑抢占预留 AP 接口的普通 WiFi 配置，但不会删除用户保存的网络。
func (m *Manager) RepairAPInterface(ctx context.Context) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.carPlayNetworkReservedLocked() {
		return carPlayNetworkBusy()
	}
	profiles, err := m.wirelessProfiles(ctx)
	if err != nil {
		return err
	}
	wifiInterface, apInterface, _, apAvailable := m.networkInterfaces(ctx, profiles)
	if !apAvailable || apInterface == "" {
		return invalidInput("未找到可修复的 AP 接口")
	}
	conflicts := apInterfaceConflicts(profiles, wifiInterface, apInterface)
	if len(conflicts) == 0 {
		return nil
	}
	apUUID := ""
	for _, item := range profiles {
		if item.Mode == "ap" {
			apUUID = item.UUID
			break
		}
	}
	if !validUUID(apUUID) {
		return invalidInput("未找到可管理的 AP 配置")
	}
	m.schedule(apDisableDelay, func() {
		m.mu.Lock()
		defer m.mu.Unlock()
		if m.carPlayNetworkReservedLocked() {
			return
		}
		operationCtx, cancel := context.WithTimeout(context.Background(), commandTimeout)
		defer cancel()
		if _, disconnectErr := m.runner.Run(operationCtx, "nmcli", "device", "disconnect", apInterface); disconnectErr != nil && !isNMDeviceInactive(disconnectErr) {
			m.logger.Warn("断开被抢占的 AP 接口失败", "interface", apInterface, "error", disconnectErr)
		}
		for _, conflict := range conflicts {
			if _, modifyErr := m.runner.Run(
				operationCtx, "nmcli", "connection", "modify", "uuid", conflict.UUID,
				"connection.autoconnect", "no", "connection.interface-name", "",
			); modifyErr != nil {
				m.logger.Warn("停用 AP 接口冲突配置失败", "uuid", conflict.UUID, "error", modifyErr)
			}
		}
		if _, bindErr := m.runner.Run(
			operationCtx, "nmcli", "connection", "modify", "uuid", apUUID,
			"connection.interface-name", apInterface,
		); bindErr != nil {
			m.logger.Error("恢复 AP 配置接口绑定失败", "uuid", apUUID, "interface", apInterface, "error", bindErr)
		}
	})
	return nil
}

// APSettings 返回当前热点配置并隐藏密码。
func (m *Manager) APSettings(ctx context.Context) (APSettings, error) {
	profiles, err := m.wirelessProfiles(ctx)
	if err != nil {
		return APSettings{}, err
	}
	return m.apSettingsFromProfiles(ctx, profiles)
}

func (m *Manager) apSettingsFromProfiles(ctx context.Context, profiles []profile) (APSettings, error) {
	wifiInterface, apInterface, wifiAvailable, apAvailable := m.networkInterfaces(ctx, profiles)
	capability := m.apConcurrencyCapability(ctx, wifiInterface, apInterface, wifiAvailable, apAvailable)
	reportedAPInterface := ""
	if apAvailable {
		reportedAPInterface = apInterface
	}
	for _, profile := range profiles {
		if profile.Mode != "ap" {
			continue
		}
		settings := APSettings{
			Available: apAvailable, DesiredEnabled: profile.Active || profile.AutoConnect,
			State: "disabled", StatusMessage: "热点已停用", Interface: reportedAPInterface,
			InterfaceRole: capability.InterfaceRole, ConcurrencyMode: capability.Mode,
			MaxConcurrentChannels: capability.MaxChannels, CapabilityNotice: capability.Notice,
			ConnectionName: profile.Name, UUID: profile.UUID,
			SSID: profile.SSID, Band: profile.Band, Channel: profile.Channel,
			Security: profile.Security, AutoConnect: profile.AutoConnect,
		}
		capabilityInterface := apInterface
		if capabilityInterface == "" {
			capabilityInterface = wifiInterface
		}
		settings.AllowedChannels = m.allowedAPChannels(ctx, capabilityInterface)
		m.readAPSecurity(ctx, &settings)
		settings.ConflictingProfiles = apInterfaceConflicts(profiles, wifiInterface, apInterface)
		if len(settings.ConflictingProfiles) > 0 {
			settings.State = "blocked"
			settings.ReasonCode = "ap_interface_conflict"
			settings.StatusMessage = fmt.Sprintf("检测到 %d 个普通 WiFi 配置正在占用或绑定 AP 接口 %s", len(settings.ConflictingProfiles), apInterface)
			settings.SuggestedAction = "请使用“修复 AP 接口”停用冲突配置，再重新开启热点。"
		}
		if !profile.Active {
			// 预配置接口可能同时正在承载上游 WiFi；热点未激活时不能把该接口的运行态误报为 AP 信息。
			if settings.State != "blocked" && profile.AutoConnect {
				settings.State = "failed"
				settings.ReasonCode = "ap_not_active"
				settings.StatusMessage = "热点计划自动启动，但当前未运行"
				settings.SuggestedAction = "请重新应用热点设置；若仍失败，页面会显示具体受限原因。"
			}
			m.applyAPChannelRestriction(ctx, &settings, wifiInterface, apInterface, profile.Channel)
			return settings, nil
		}
		settings.Interface = apInterface
		settings.Address = profile.Address
		settings.Sharing = profile.IPv4Method == "shared"
		runtime := m.activeAPRuntime(ctx, apInterface)
		settings.ActiveBand, settings.ActiveChannel = runtime.Band, runtime.Channel
		if runtime.Type == "AP" && runtime.SSID == profile.SSID && runtime.Channel > 0 {
			settings.Enabled = true
			settings.State = "running"
			settings.StatusMessage = "热点正在正常广播"
			settings.ActiveSecurity = m.activeAPSecurity(ctx, apInterface)
		} else if settings.State != "blocked" {
			settings.State = "degraded"
			settings.ReasonCode = "ap_not_broadcasting"
			settings.StatusMessage = "NetworkManager 已激活热点配置，但无线接口没有正常广播 SSID"
			settings.SuggestedAction = "请重新应用热点设置；系统会在启动前检查接口、信道和监管域限制。"
			m.applyAPChannelRestriction(ctx, &settings, wifiInterface, apInterface, profile.Channel)
		}
		return settings.withInterfaceDetails(m.interfaceDetails(ctx, apInterface)), nil
	}
	return APSettings{
		Available: false, InterfaceRole: capability.InterfaceRole,
		ConcurrencyMode: capability.Mode, CapabilityNotice: capability.Notice,
	}, nil
}

func apInterfaceConflicts(profiles []profile, wifiInterface, apInterface string) []APInterfaceConflict {
	if apInterface == "" || apInterface == wifiInterface {
		return nil
	}
	conflicts := make([]APInterfaceConflict, 0)
	for _, item := range profiles {
		if item.Mode == "ap" || (item.Device != apInterface && item.ConfiguredDevice != apInterface) {
			continue
		}
		conflicts = append(conflicts, APInterfaceConflict{
			Name: item.Name, UUID: item.UUID, SSID: item.SSID,
			Active: item.Active, AutoConnect: item.AutoConnect,
		})
	}
	return conflicts
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
			// 多接口网卡上，桌面网络面板可能把预留 AP 接口临时激活为客户端。
			// 优先选择另一个活动接口作为上游，避免配置排列顺序掩盖 AP 接口冲突。
			if item.Device != apInterface {
				activeClientInterface = item.Device
				break
			}
			if activeClientInterface == "" {
				activeClientInterface = item.Device
			}
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
	if station := wirelessRoleName("TESLABOX_WIFI_INTERFACE"); station != "" && deviceExists(station) && apAvailable {
		return station, apInterface, true, true
	}
	if m.configuredWiFiInterface != "" {
		available := deviceExists(m.configuredWiFiInterface) || activeClientInterface == m.configuredWiFiInterface
		return m.configuredWiFiInterface, apInterface, available, apAvailable
	}
	if activeClientInterface != "" && activeClientInterface != apInterface {
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
	if activeClientInterface != "" {
		return activeClientInterface, apInterface, true, apAvailable
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
	if reserved := wirelessRoleName("TESLABOX_AP_INTERFACE"); reserved != "" {
		return reserved
	}
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

type apConcurrencyCapability struct {
	Mode          string
	MaxChannels   int
	InterfaceRole string
	Notice        string
}

func (m *Manager) apConcurrencyCapability(ctx context.Context, wifiInterface, apInterface string, wifiAvailable, apAvailable bool) apConcurrencyCapability {
	if !apAvailable {
		return apConcurrencyCapability{
			Mode: "unavailable", InterfaceRole: "预留 AP 接口",
			Notice: "当前没有可用的 AP 接口；热点设置不可用，上游 WiFi 和其他功能不受影响。",
		}
	}
	if !wifiAvailable || wifiInterface == "" || apInterface == "" {
		return apConcurrencyCapability{
			Mode: "unknown", InterfaceRole: "AP 接口",
			Notice: "暂时无法确认 AP+STA 并发能力；系统会按同信道安全模式处理。",
		}
	}
	if wifiInterface == apInterface {
		return apConcurrencyCapability{
			Mode: "exclusive", InterfaceRole: "共享物理接口",
			Notice: "当前网卡不能同时运行热点和上游 WiFi；启用其中一个会断开另一个。",
		}
	}
	samePHY, known := m.sameWirelessPHY(wifiInterface, apInterface)
	if !known {
		return apConcurrencyCapability{
			Mode: "unknown", InterfaceRole: "AP 接口",
			Notice: "暂时无法确认 AP 接口的无线拓扑；系统会按同信道安全模式处理。",
		}
	}
	if !samePHY {
		return apConcurrencyCapability{
			Mode: "independent_radio", InterfaceRole: "独立 AP 网卡",
			Notice: "热点使用独立无线设备，可以与上游 WiFi 保持不同频段和信道。",
		}
	}
	phyInfo, err := m.wirelessPHYInfo(ctx, wifiInterface)
	if err != nil {
		return apConcurrencyCapability{
			Mode: "unknown", InterfaceRole: "AP 虚拟接口",
			Notice: "无法读取网卡的 AP+STA 信道能力；系统会按同信道安全模式处理。",
		}
	}
	channelLimit, found := parseAPSTAChannelLimit(phyInfo)
	if !found {
		return apConcurrencyCapability{
			Mode: "unknown", InterfaceRole: "AP 虚拟接口",
			Notice: "驱动没有报告可识别的 AP+STA 信道组合；系统会按同信道安全模式处理。",
		}
	}
	if channelLimit <= 1 {
		return apConcurrencyCapability{
			Mode: "same_channel", MaxChannels: channelLimit, InterfaceRole: "AP 虚拟接口",
			Notice: "当前网卡支持 AP+STA，但热点必须与上游 WiFi 使用同一频段、同一信道。",
		}
	}
	return apConcurrencyCapability{
		Mode: "multi_channel", MaxChannels: channelLimit, InterfaceRole: "AP 虚拟接口",
		Notice: fmt.Sprintf("当前网卡支持 AP+STA，并允许最多 %d 个活动信道。", channelLimit),
	}
}

func (m *Manager) readWirelessPHYInfo(ctx context.Context, interfaceName string) (string, error) {
	phyPath, err := filepath.EvalSymlinks(filepath.Join("/sys/class/net", interfaceName, "phy80211"))
	if err != nil {
		return "", err
	}
	phyName := filepath.Base(filepath.Clean(phyPath))
	if phyName == "" || phyName == "." || phyName == string(filepath.Separator) {
		return "", fmt.Errorf("无线接口 %s 没有关联有效 PHY", interfaceName)
	}
	output, err := m.run(ctx, "/usr/sbin/iw", "phy", phyName, "info")
	if err != nil {
		return "", err
	}
	return string(output), nil
}

func (m *Manager) wirelessInterfacesChannelCoupled(ctx context.Context, wifiInterface, apInterface string) (bool, bool) {
	samePHY, known := m.sameWirelessPHY(wifiInterface, apInterface)
	if !known || !samePHY {
		return samePHY, known
	}
	phyInfo, err := m.wirelessPHYInfo(ctx, wifiInterface)
	if err != nil {
		return true, false
	}
	channelLimit, found := parseAPSTAChannelLimit(phyInfo)
	if !found {
		return true, false
	}
	return channelLimit <= 1, true
}

func (m *Manager) activeWirelessChannel(ctx context.Context, interfaceName string) (string, int) {
	if interfaceName == "" {
		return "", 0
	}
	output, err := m.run(ctx, "/usr/sbin/iw", "dev", interfaceName, "info")
	if err != nil {
		return "", 0
	}
	if band, channel := parseIWInterfaceChannel(string(output)); channel != 0 {
		return band, channel
	}
	// 部分厂商驱动不在 info 中返回信道，关联链路仍提供实际频率。
	output, err = m.run(ctx, "/usr/sbin/iw", "dev", interfaceName, "link")
	if err != nil {
		return "", 0
	}
	return parseIWLinkChannel(string(output))
}

type apRuntime struct {
	Type    string
	SSID    string
	Band    string
	Channel int
}

func (m *Manager) activeAPRuntime(ctx context.Context, interfaceName string) apRuntime {
	if interfaceName == "" {
		return apRuntime{}
	}
	output, err := m.run(ctx, "/usr/sbin/iw", "dev", interfaceName, "info")
	if err != nil {
		return apRuntime{}
	}
	return parseIWAPRuntime(string(output))
}

func parseIWAPRuntime(output string) apRuntime {
	runtime := apRuntime{}
	for _, line := range strings.Split(output, "\n") {
		fields := strings.Fields(strings.TrimSpace(line))
		if len(fields) < 2 {
			continue
		}
		switch fields[0] {
		case "type":
			runtime.Type = fields[1]
		case "ssid":
			runtime.SSID = strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(line), "ssid"))
		case "channel":
			channel, err := strconv.Atoi(fields[1])
			if err != nil || channel <= 0 {
				continue
			}
			runtime.Channel = channel
			if channel <= 14 {
				runtime.Band = "bg"
			} else {
				runtime.Band = "a"
			}
		}
	}
	return runtime
}

type apChannelCapability struct {
	Found    bool
	Disabled bool
	NoIR     bool
	Radar    bool
}

func parseAPChannelCapabilities(output string) map[int]apChannelCapability {
	capabilities := make(map[int]apChannelCapability)
	for _, line := range strings.Split(output, "\n") {
		// 6 GHz 会复用 2.4/5 GHz 的信道号，例如 5785 MHz 与 6735 MHz 都是信道 157。
		// 当前热点配置只支持 2.4/5 GHz；若仅按信道号建表，后出现的 6 GHz 禁用项会覆盖可用的 5 GHz 项。
		mhz := strings.Index(line, " MHz")
		if mhz < 0 {
			continue
		}
		frequencyFields := strings.Fields(line[:mhz])
		if len(frequencyFields) == 0 {
			continue
		}
		frequency, err := strconv.Atoi(frequencyFields[len(frequencyFields)-1])
		if err != nil || frequency < 2400 || frequency >= 5925 {
			continue
		}
		open := strings.IndexByte(line, '[')
		close := strings.IndexByte(line, ']')
		if open < 0 || close <= open+1 {
			continue
		}
		channel, err := strconv.Atoi(strings.TrimSpace(line[open+1 : close]))
		if err != nil || channel <= 0 {
			continue
		}
		normalized := strings.ToLower(line)
		capabilities[channel] = apChannelCapability{
			Found: true, Disabled: strings.Contains(normalized, "disabled"),
			NoIR:  strings.Contains(normalized, "no ir") || strings.Contains(normalized, "no-ir"),
			Radar: strings.Contains(normalized, "radar"),
		}
	}
	return capabilities
}

func apChannelOperationError(channel int, capability apChannelCapability) *APOperationError {
	switch {
	case !capability.Found:
		return &APOperationError{
			Code: "channel_unknown", Message: fmt.Sprintf("网卡没有报告热点信道 %d", channel),
			Suggestion: "请选择网卡明确支持且允许创建 AP 的信道。",
		}
	case capability.Disabled:
		return &APOperationError{
			Code: "channel_disabled", Message: fmt.Sprintf("热点信道 %d 已被当前网卡或监管域禁用", channel),
			Suggestion: "请选择其他允许的信道。",
		}
	case capability.NoIR:
		return &APOperationError{
			Code: "channel_no_ir", Message: fmt.Sprintf("热点信道 %d 禁止主动发射无线信号", channel),
			Suggestion: "请选择没有 no IR 限制的信道。",
		}
	case capability.Radar:
		return &APOperationError{
			Code: "channel_requires_dfs", Message: fmt.Sprintf("热点信道 %d 被当前监管域标记为 RADAR，当前驱动链无法在该信道创建 AP", channel),
			Suggestion: "请更新无线监管数据库，或把上游和热点调整到 149、153、157、161 等允许信道。",
		}
	default:
		return nil
	}
}

func selectAllowedAPChannel(capabilities map[int]apChannelCapability, band string) (int, bool) {
	preferred := []int{161, 157, 153, 149, 165, 48, 44, 40, 36}
	if band == "bg" {
		preferred = []int{11, 6, 1, 13, 12, 10, 9, 8, 7, 5, 4, 3, 2}
	}
	for _, channel := range preferred {
		capability := capabilities[channel]
		if capability.Found && apChannelOperationError(channel, capability) == nil {
			return channel, true
		}
	}
	return 0, false
}

func (m *Manager) allowedAPChannels(ctx context.Context, interfaceName string) []int {
	if interfaceName == "" {
		return nil
	}
	phyInfo, err := m.wirelessPHYInfo(ctx, interfaceName)
	if err != nil {
		return nil
	}
	capabilities := parseAPChannelCapabilities(phyInfo)
	channels := make([]int, 0, len(capabilities))
	for channel, capability := range capabilities {
		if apChannelOperationError(channel, capability) == nil {
			channels = append(channels, channel)
		}
	}
	sort.Ints(channels)
	return channels
}

func (m *Manager) apChannelPreflight(ctx context.Context, interfaceName string, channel int) error {
	if m.regulatory != nil {
		if err := m.regulatory.Apply(ctx); err != nil {
			return &APOperationError{Code: "regulatory_apply_failed", Message: "无线地区配置未能应用，未强行启动热点", Suggestion: "请核对网络设置中的已保存地区与实际监管域，并查看诊断报告。"}
		}
	}
	return m.checkAPChannel(ctx, interfaceName, channel)
}

// 页面查询只读取信道限制，不能因为刷新设置就改变全局监管域。
func (m *Manager) checkAPChannel(ctx context.Context, interfaceName string, channel int) error {
	if channel <= 0 {
		return nil
	}
	phyInfo, err := m.wirelessPHYInfo(ctx, interfaceName)
	if err != nil {
		return &APOperationError{
			Code: "capability_unavailable", Message: "无法读取无线网卡的热点信道能力",
			Suggestion: "请检查无线网卡、驱动和 iw 工具是否正常。",
		}
	}
	if operationError := apChannelOperationError(channel, parseAPChannelCapabilities(phyInfo)[channel]); operationError != nil {
		return operationError
	}
	return nil
}

func apRequiresSameChannel(mode string) bool {
	return mode == "same_channel" || mode == "unknown"
}

func (m *Manager) applyAPChannelRestriction(ctx context.Context, settings *APSettings, wifiInterface, apInterface string, configuredChannel int) {
	channel := configuredChannel
	interfaceName := apInterface
	if interfaceName == "" {
		interfaceName = wifiInterface
	}
	if apRequiresSameChannel(settings.ConcurrencyMode) && wifiInterface != "" {
		_, activeChannel := m.activeWirelessChannel(ctx, wifiInterface)
		if activeChannel > 0 {
			channel = activeChannel
		}
	}
	if channel <= 0 || interfaceName == "" {
		return
	}
	err := m.checkAPChannel(ctx, interfaceName, channel)
	var operationError *APOperationError
	if !errors.As(err, &operationError) || operationError == nil {
		return
	}
	settings.State = "blocked"
	settings.ReasonCode = operationError.Code
	settings.StatusMessage = operationError.Message
	settings.SuggestedAction = operationError.Suggestion
}

func parseIWInterfaceChannel(output string) (string, int) {
	for _, line := range strings.Split(output, "\n") {
		fields := strings.Fields(strings.TrimSpace(line))
		if len(fields) < 2 || fields[0] != "channel" {
			continue
		}
		channel, err := strconv.Atoi(fields[1])
		if err != nil || channel <= 0 {
			return "", 0
		}
		if channel <= 14 {
			return "bg", channel
		}
		return "a", channel
	}
	return "", 0
}

func parseAPSTAChannelLimit(output string) (int, bool) {
	const sectionHeader = "valid interface combinations:"
	inCombinations := false
	combination := ""
	maxChannels := 0
	flush := func() {
		if !combinationAllowsAPSTA(combination) {
			return
		}
		total, totalFound := parseCombinationLimit(combination, "total <=")
		channelLimit, channelLimitFound := parseCombinationLimit(combination, "#channels <=")
		if !totalFound || total < 2 || !channelLimitFound {
			return
		}
		if channelLimit > maxChannels {
			maxChannels = channelLimit
		}
	}
	for _, line := range strings.Split(output, "\n") {
		trimmed := strings.TrimSpace(line)
		if !inCombinations {
			if trimmed == sectionHeader {
				inCombinations = true
			}
			continue
		}
		if strings.HasPrefix(trimmed, "*") {
			flush()
			combination = trimmed
			continue
		}
		if combination != "" && len(line) > 0 && (line[0] == ' ' || line[0] == '\t') {
			combination += " " + trimmed
			continue
		}
		if combination != "" {
			break
		}
	}
	flush()
	return maxChannels, maxChannels > 0
}

// combinationAllowsAPSTA 按类型组的共享配额判断，避免把单接口组误判为并发。
func combinationAllowsAPSTA(combination string) bool {
	managed, ap := false, false
	for _, group := range strings.Split(combination, "#{")[1:] {
		types, quota, found := strings.Cut(group, "}")
		if !found {
			return false
		}
		required := 0
		for _, interfaceType := range strings.Split(types, ",") {
			switch strings.TrimSpace(interfaceType) {
			case "managed":
				if managed {
					return false
				}
				managed = true
				required++
			case "AP":
				if ap {
					return false
				}
				ap = true
				required++
			}
		}
		if required > 0 {
			quota = strings.TrimSpace(quota)
			if !strings.HasPrefix(quota, "<=") {
				return false
			}
			limit, valid := parseCombinationLimit(quota, "<=")
			if !valid || limit < required {
				return false
			}
		}
	}
	return managed && ap
}

func parseCombinationLimit(combination, marker string) (int, bool) {
	markerIndex := strings.Index(combination, marker)
	if markerIndex < 0 {
		return 0, false
	}
	fields := strings.Fields(combination[markerIndex+len(marker):])
	if len(fields) == 0 {
		return 0, false
	}
	limit, err := strconv.Atoi(strings.TrimRight(fields[0], ","))
	return limit, err == nil
}

// UpdateAP 校验并更新现有热点；空密码表示保留原密码。
func (m *Manager) UpdateAP(ctx context.Context, update APUpdate) error {
	if update.ConfirmCarPlayReconnect {
		return m.updateAPWithCarPlayRestart(ctx, update)
	}
	return m.updateAP(ctx, update, "")
}

func (m *Manager) updateAP(ctx context.Context, update APUpdate, restartTemplate string) error {
	m.mu.Lock()
	if m.carPlayRecoveryUncertain || m.carPlayNetworkHold != nil || m.carPlayWiFiReconnect && restartTemplate == "" {
		m.mu.Unlock()
		return carPlayAPBusy()
	}
	locked := true
	defer func() {
		if locked {
			m.mu.Unlock()
		}
	}()
	if err := validateAPUpdate(update); err != nil {
		return err
	}
	securityArgs, err := apSecurityArguments(update.SecurityMode)
	if err != nil {
		return err
	}
	settings, err := m.APSettings(ctx)
	if err != nil {
		return err
	}
	if !settings.Available || !validUUID(settings.UUID) {
		return invalidInput("未找到可管理的 AP 配置")
	}
	if restartTemplate != "" && settings.UUID != restartTemplate {
		return invalidInput("热点模板身份已改变，未修改其他热点配置")
	}
	if update.SecurityMode == "wpa3" {
		available := false
		for _, option := range settings.SecurityOptions {
			if option.Mode == "wpa3" {
				available = option.Available
			}
		}
		if !available {
			return &APOperationError{
				Code: "ap_security_unavailable", Message: "无法确认当前 AP 接口支持 WPA3-Personal",
				Suggestion: "请检查 wpa_supplicant 控制接口、SAE、管理帧保护和 AP 能力；或明确选择 WPA2，不会自动降级。",
			}
		}
	}
	if update.Password != "" && update.SecurityMode == "" && !profileUsesPSK(settings.Security) {
		return invalidInput("请先选择热点安全认证方式，再设置密码")
	}
	if update.Enabled && len(settings.ConflictingProfiles) > 0 {
		return &APOperationError{
			Code:       "ap_interface_conflict",
			Message:    fmt.Sprintf("检测到普通 WiFi 配置正在占用或绑定 AP 接口 %s", settings.Interface),
			Suggestion: "请先使用“修复 AP 接口”，再重新开启热点。",
		}
	}
	wifiStatus := WiFiStatus{}
	effectiveChannel := update.Channel
	if update.Enabled {
		wifiStatus, err = m.WiFiStatus(ctx)
		if err != nil {
			return err
		}
		if settings.ConcurrencyMode == "exclusive" && wifiStatus.CurrentConnection != "" {
			return &APOperationError{
				Code:       "ap_sta_concurrency_unsupported",
				Message:    "当前无线设备不支持同时运行上游 WiFi 和 AP 热点",
				Suggestion: "请先断开上游 WiFi，或接入独立的 AP 无线网卡。",
			}
		}
	}
	if update.Enabled && apRequiresSameChannel(settings.ConcurrencyMode) && wifiStatus.Interface != "" {
		_, upstreamChannel := m.activeWirelessChannel(ctx, wifiStatus.Interface)
		if upstreamChannel > 0 {
			effectiveChannel = upstreamChannel
		}
	}
	if update.Enabled {
		capabilityInterface := settings.Interface
		if capabilityInterface == "" {
			capabilityInterface = wifiStatus.Interface
		}
		if effectiveChannel == 0 {
			phyInfo, readErr := m.wirelessPHYInfo(ctx, capabilityInterface)
			if readErr != nil {
				return &APOperationError{Code: "capability_unavailable", Message: "无法读取无线网卡的热点信道能力", Suggestion: "请检查无线网卡、驱动和 iw 工具是否正常。"}
			}
			var found bool
			effectiveChannel, found = selectAllowedAPChannel(parseAPChannelCapabilities(phyInfo), update.Band)
			if !found {
				return &APOperationError{Code: "no_allowed_channel", Message: "当前频段没有允许创建热点的信道", Suggestion: "请切换频段、更新无线监管数据库或检查网卡驱动。"}
			}
		}
		if err := m.apChannelPreflight(ctx, capabilityInterface, effectiveChannel); err != nil {
			return err
		}
	}
	autoConnect := update.Enabled && update.AutoConnect
	channelValue := ""
	if effectiveChannel != 0 {
		channelValue = strconv.Itoa(effectiveChannel)
	}
	bandValue := update.Band
	if effectiveChannel > 0 {
		bandValue, _ = concurrentBand(effectiveChannel)
	}
	args := []string{"connection", "modify", "uuid", settings.UUID,
		"802-11-wireless.ssid", update.SSID,
		"802-11-wireless.band", bandValue,
		// NetworkManager 用空属性表示自动选信道，数值 0 会被 nmcli 判定为非法信道。
		"802-11-wireless.channel", channelValue,
		// 关闭热点必须同时禁止开机自动连接，否则旧连接会在重启后重新激活，表现为无法永久关闭。
		"connection.autoconnect", yesNo(autoConnect),
		"connection.autoconnect-retries", "1",
		"ipv4.method", "shared",
	}
	args = append(args, securityArgs...)
	if update.Password != "" {
		args = append(args, "802-11-wireless-security.psk", update.Password)
	}
	if _, err := m.run(ctx, "nmcli", args...); err != nil {
		if update.SecurityMode != "" {
			message := err.Error()
			if update.Password != "" {
				message = strings.ReplaceAll(message, update.Password, "[已隐藏密码]")
			}
			m.logger.Warn("保存热点安全认证失败", "security_mode", update.SecurityMode, "error", message)
			return &APOperationError{
				Code: "ap_security_config_rejected", Message: "系统拒绝保存热点认证配置，未自动降级",
				Suggestion: "请检查 NetworkManager 版本、认证参数及已有密码是否兼容；可通过有线网络查看详细日志。",
			}
		}
		return fmt.Errorf("保存热点配置: %w", err)
	}
	m.apUpdateGeneration++
	generation := m.apUpdateGeneration
	if restartTemplate != "" {
		// CarPlay 会从此模板重建专用热点。普通热点不能占住接口，否则新租约会被拒绝。
		// 显式关闭也阻止本次模板修改触发自动连接；不留下延迟任务误关下一轮专用热点。
		if _, err := m.run(ctx, "nmcli", "--wait", "15", "connection", "down", "uuid", settings.UUID); err != nil && !isNMDeviceInactive(err) {
			return fmt.Errorf("已保存热点模板，但释放普通热点接口失败: %w", err)
		}
		return nil
	}
	if !update.Enabled && !settings.DesiredEnabled && !settings.Enabled {
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
			if _, err := m.runner.Run(operationCtx, "nmcli", "--wait", "15", "connection", "down", "uuid", settings.UUID); err != nil && !isNMDeviceInactive(err) {
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
		// 接口重建可能改变信道权限；重新测量，不能把监管错误泛化为认证不兼容。
		if m.regulatory != nil {
			if channelErr := m.apChannelPreflight(ctx, settings.Interface, effectiveChannel); channelErr != nil {
				return channelErr
			}
		}
		if update.SecurityMode != "" {
			m.logger.Warn("应用热点安全认证失败", "security_mode", update.SecurityMode, "interface", settings.Interface, "error", err)
			return &APOperationError{
				Code: "ap_security_activation_failed", Message: "热点认证配置已保存，但启动失败，未自动降级",
				Suggestion: "请通过有线网络检查日志中的认证、驱动及信道错误，必要时明确改回 WPA2。",
			}
		}
		return fmt.Errorf("应用热点配置: %w", err)
	}
	runtime := m.activeAPRuntime(ctx, settings.Interface)
	if runtime.Type != "AP" || runtime.SSID != update.SSID || runtime.Channel != effectiveChannel {
		return &APOperationError{
			Code:       "ap_not_broadcasting",
			Message:    "NetworkManager 已接受热点配置，但无线接口没有正常广播 SSID",
			Suggestion: "请检查页面显示的能力限制；系统不会把该状态误报为运行中。",
		}
	}
	if update.SecurityMode != "" {
		activeSecurity := m.activeAPSecurity(ctx, settings.Interface)
		if activeSecurity != "" && activeSecurity != update.SecurityMode {
			return &APOperationError{
				Code: "ap_security_mismatch", Message: "接口报告的热点认证与所选认证不一致",
				Suggestion: "请通过有线网络检查驱动和认证服务日志；不会把不同的运行认证误报为所选认证已生效。",
			}
		}
	}
	return nil
}

// ConfigureAPRadio 更新热点的持久频段和信道，并在热点运行时安全重启配置。
// 单信道 AP+STA 设备不允许写入与当前上游 WiFi 冲突的运行信道，避免驱动异常或上游断连。
func (m *Manager) ConfigureAPRadio(ctx context.Context, band string, channel int) (APSettings, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.carPlayNetworkReservedLocked() {
		return APSettings{}, carPlayNetworkBusy()
	}
	if !validBandChannel(band, channel) {
		return APSettings{}, invalidInput("热点频段或信道无效")
	}
	settings, err := m.APSettings(ctx)
	if err != nil {
		return APSettings{}, err
	}
	if !settings.Available || !validUUID(settings.UUID) {
		return APSettings{}, invalidInput("未找到可管理的 AP 配置")
	}
	capabilityInterface := settings.Interface
	if channel > 0 {
		if err := m.apChannelPreflight(ctx, capabilityInterface, channel); err != nil {
			return APSettings{}, err
		}
	}
	effectiveBand, effectiveChannel := band, channel
	if settings.Enabled {
		wifiStatus, statusErr := m.WiFiStatus(ctx)
		if statusErr != nil {
			return APSettings{}, statusErr
		}
		if settings.ConcurrencyMode == "exclusive" && wifiStatus.CurrentConnection != "" {
			return APSettings{}, &APOperationError{
				Code:       "ap_sta_concurrency_unsupported",
				Message:    "当前无线设备不支持同时运行上游 WiFi 和 AP 热点",
				Suggestion: "请先断开上游 WiFi，再修改热点频段和信道。",
			}
		}
		if apRequiresSameChannel(settings.ConcurrencyMode) && wifiStatus.Interface != "" {
			upstreamBand, upstreamChannel := m.activeWirelessChannel(ctx, wifiStatus.Interface)
			if upstreamChannel > 0 {
				if channel > 0 && channel != upstreamChannel {
					return APSettings{}, &APOperationError{
						Code:       "ap_sta_channel_conflict",
						Message:    fmt.Sprintf("当前设备要求 AP 与上游 WiFi 使用同一信道；上游正在使用信道 %d", upstreamChannel),
						Suggestion: fmt.Sprintf("请改用信道 %d、选择 auto，或先断开上游 WiFi。", upstreamChannel),
					}
				}
				effectiveBand, effectiveChannel = upstreamBand, upstreamChannel
			}
		}
		if effectiveChannel == 0 {
			phyInfo, readErr := m.wirelessPHYInfo(ctx, capabilityInterface)
			if readErr != nil {
				return APSettings{}, &APOperationError{Code: "capability_unavailable", Message: "无法读取无线网卡的热点信道能力", Suggestion: "请检查无线网卡、驱动和 iw 工具是否正常。"}
			}
			var found bool
			effectiveChannel, found = selectAllowedAPChannel(parseAPChannelCapabilities(phyInfo), effectiveBand)
			if !found {
				return APSettings{}, &APOperationError{Code: "no_allowed_channel", Message: "当前频段没有允许创建热点的信道", Suggestion: "请切换频段、更新无线监管数据库或检查网卡驱动。"}
			}
		}
		if err := m.apChannelPreflight(ctx, capabilityInterface, effectiveChannel); err != nil {
			return APSettings{}, err
		}
		if settings.Band == effectiveBand && settings.Channel == effectiveChannel && settings.ActiveChannel == effectiveChannel {
			return settings, nil
		}
	}
	channelValue := ""
	if effectiveChannel > 0 {
		channelValue = strconv.Itoa(effectiveChannel)
	}
	if _, err := m.run(ctx, "nmcli", "connection", "modify", "uuid", settings.UUID,
		"802-11-wireless.band", effectiveBand,
		"802-11-wireless.channel", channelValue,
	); err != nil {
		return APSettings{}, fmt.Errorf("保存热点频段和信道: %w", err)
	}
	if settings.Enabled {
		rollback := func(cause error) error {
			oldChannel := ""
			if settings.Channel > 0 {
				oldChannel = strconv.Itoa(settings.Channel)
			}
			if _, restoreErr := m.run(ctx, "nmcli", "connection", "modify", "uuid", settings.UUID,
				"802-11-wireless.band", settings.Band,
				"802-11-wireless.channel", oldChannel,
			); restoreErr != nil {
				return errors.Join(cause, fmt.Errorf("恢复热点原频段和信道: %w", restoreErr))
			}
			restoreArgs := []string{"--wait", "15", "connection", "up", "uuid", settings.UUID}
			if settings.Interface != "" {
				restoreArgs = append(restoreArgs, "ifname", settings.Interface)
			}
			if _, restoreErr := m.run(ctx, "nmcli", restoreArgs...); restoreErr != nil {
				return errors.Join(cause, fmt.Errorf("重新拉起热点原配置: %w", restoreErr))
			}
			return cause
		}
		operationArgs := []string{"--wait", "15", "connection", "up", "uuid", settings.UUID}
		if settings.Interface != "" {
			operationArgs = append(operationArgs, "ifname", settings.Interface)
		}
		if _, err := m.run(ctx, "nmcli", operationArgs...); err != nil {
			return APSettings{}, rollback(fmt.Errorf("应用热点频段和信道: %w", err))
		}
		runtime := m.activeAPRuntime(ctx, settings.Interface)
		if runtime.Type != "AP" || runtime.SSID != settings.SSID || runtime.Channel != effectiveChannel {
			return APSettings{}, rollback(&APOperationError{
				Code:       "ap_not_broadcasting",
				Message:    "热点配置已保存，但无线接口没有按目标信道正常广播",
				Suggestion: "请查看热点能力说明并改用允许的信道。",
			})
		}
	}
	return m.APSettings(ctx)
}

// SetAPRuntime 只切换热点当前运行状态，不改变开机自动连接策略。
// 本机自动化调用不依赖热点传输响应，因此关闭操作必须同步完成后再返回。
func (m *Manager) SetAPRuntime(ctx context.Context, enabled bool) error {
	if enabled {
		settings, err := m.APSettings(ctx)
		if err != nil {
			return err
		}
		return m.UpdateAP(ctx, APUpdate{
			Enabled: enabled, SSID: settings.SSID, Band: settings.Band, Channel: settings.Channel,
			AutoConnect: settings.AutoConnect,
		})
	}

	m.mu.Lock()
	defer m.mu.Unlock()
	if m.carPlayNetworkReservedLocked() {
		return carPlayNetworkBusy()
	}
	settings, err := m.APSettings(ctx)
	if err != nil {
		return err
	}
	if !settings.Available || !validUUID(settings.UUID) {
		return invalidInput("未找到可管理的 AP 配置")
	}
	if !settings.Enabled {
		return nil
	}
	m.apUpdateGeneration++
	if _, err := m.run(ctx, "nmcli", "--wait", "15", "connection", "down", "uuid", settings.UUID); err != nil && !isNMDeviceInactive(err) {
		return fmt.Errorf("关闭无线热点: %w", err)
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
		item.ConfiguredDevice = m.profileInterface(ctx, item.UUID)
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
	if !settings.Available || !settings.DesiredEnabled {
		return nil
	}
	capabilityInterface := settings.Interface
	if capabilityInterface == "" {
		capabilityInterface = status.Interface
	}
	if err := m.apChannelPreflight(ctx, capabilityInterface, channel); err != nil {
		return err
	}
	if settings.State == "running" && settings.ActiveChannel == channel {
		return nil
	}
	upstreamUUID := ""
	if network, found := currentSavedNetwork(status); found {
		upstreamUUID = network.UUID
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
	runtime := m.activeAPRuntime(ctx, settings.Interface)
	if runtime.Type != "AP" || runtime.SSID != settings.SSID || runtime.Channel != channel {
		return restoreUpstream(&APOperationError{
			Code:       "ap_not_broadcasting",
			Message:    "热点配置已激活，但无线接口没有正常广播 SSID",
			Suggestion: "请检查网卡驱动状态，或改用页面提示的允许信道。",
		})
	}
	return nil
}

func (m *Manager) ensureAPAfterUpstream(ctx context.Context, status WiFiStatus, channel int) error {
	settings, err := m.APSettings(ctx)
	if err != nil {
		return err
	}
	if !settings.Available || !settings.DesiredEnabled || !apRequiresSameChannel(settings.ConcurrencyMode) {
		return nil
	}
	band, ok := concurrentBand(channel)
	if !ok {
		return invalidInput("上游 WiFi 信道不支持 AP+STA 并发")
	}
	capabilityInterface := settings.Interface
	if capabilityInterface == "" {
		capabilityInterface = status.Interface
	}
	if err := m.apChannelPreflight(ctx, capabilityInterface, channel); err != nil {
		return err
	}
	if settings.State == "running" && settings.ActiveChannel == channel {
		return nil
	}
	if _, err := m.run(
		ctx, "nmcli", "connection", "modify", "uuid", settings.UUID,
		"802-11-wireless.band", band,
		"802-11-wireless.channel", strconv.Itoa(channel),
		"connection.autoconnect-retries", "1",
	); err != nil {
		return fmt.Errorf("同步热点信道: %w", err)
	}
	if _, err := m.run(ctx, "nmcli", "--wait", "20", "connection", "up", "uuid", settings.UUID, "ifname", settings.Interface); err != nil {
		return fmt.Errorf("在上游 WiFi 稳定后启动热点: %w", err)
	}
	runtime := m.activeAPRuntime(ctx, settings.Interface)
	if runtime.Type != "AP" || runtime.SSID != settings.SSID || runtime.Channel != channel {
		return &APOperationError{
			Code:       "ap_not_broadcasting",
			Message:    "热点配置已激活，但无线接口没有正常广播 SSID",
			Suggestion: "请检查网卡驱动状态，或改用页面提示的允许信道。",
		}
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
