// Package deviceadmin 管理 NetworkManager 无线配置和设备电源操作。
package deviceadmin

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/netip"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/wen-ryon/teslabox/internal/system"
)

const (
	defaultWiFiInterface = "wlan0"
	apInterfaceName      = "ap0"
	commandTimeout       = 20 * time.Second
	recoveryInterval     = 15 * time.Second
	recoveryTimeout      = 50 * time.Second
	recoveryMaxBackoff   = time.Minute
)

var (
	// ErrAPActive 在 AP+STA 并发不可用时返回；当前硬件已支持并发，此错误保留用于回退场景。
	ErrAPActive = errors.New("无线热点正在使用当前网卡")
	// ErrUnsupportedSecurity 表示第一版尚不能安全配置该无线加密类型。
	ErrUnsupportedSecurity  = errors.New("暂不支持该无线网络的加密类型")
	errAutoNetworkInvisible = errors.New("自动连接网络暂不可见")
)

// InputError 表示可以安全返回给管理页面的请求参数错误。
type InputError struct {
	Message string
}

// Error 返回面向用户的简体中文校验说明。
func (e *InputError) Error() string { return e.Message }

// SavedNetwork 是不包含密码的 NetworkManager 客户端连接摘要。
type SavedNetwork struct {
	Name        string `json:"name"`
	UUID        string `json:"uuid"`
	SSID        string `json:"ssid"`
	Active      bool   `json:"active"`
	AutoConnect bool   `json:"auto_connect"`
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
	runner        system.Runner
	logger        *slog.Logger
	interfaceName string
	mu            sync.Mutex
	recoveryMu    sync.RWMutex
	recovery      RecoveryStatus
	schedule      func(time.Duration, func())
}

// New 创建基于 NetworkManager 和 systemd 的设备管理器。
func New(runner system.Runner, logger *slog.Logger) *Manager {
	return &Manager{
		runner: runner, logger: logger, interfaceName: defaultWiFiInterface,
		recovery: RecoveryStatus{Enabled: true},
		schedule: func(delay time.Duration, operation func()) { time.AfterFunc(delay, operation) },
	}
}

// Run 周期检查已保存的自动连接网络，并在 BSSID 或信道变化后恢复 AP+STA。
func (m *Manager) Run(ctx context.Context) {
	ticker := time.NewTicker(recoveryInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if !m.recoveryDue() {
				continue
			}
			recoveryCtx, cancel := context.WithTimeout(ctx, recoveryTimeout)
			recovered, ssid, err := m.reconcileAutoConnect(recoveryCtx)
			cancel()
			if err != nil {
				m.recordRecovery(ssid, false, err)
				if !errors.Is(err, errAutoNetworkInvisible) {
					m.logger.Warn("自动恢复上游 WiFi 失败", "ssid", ssid, "error", err)
				}
				continue
			}
			if recovered {
				m.recordRecovery(ssid, true, nil)
				m.logger.Info("上游 WiFi 已自动恢复", "ssid", ssid)
			}
		}
	}
}

// WiFiStatus 返回客户端连接和已保存网络，不读取任何连接密码。
func (m *Manager) WiFiStatus(ctx context.Context) (WiFiStatus, error) {
	profiles, err := m.wirelessProfiles(ctx)
	if err != nil {
		return WiFiStatus{}, err
	}
	status := WiFiStatus{Available: true, Interface: m.interfaceName, SavedNetworks: []SavedNetwork{}}
	status.MACAddress, status.IPAddress = m.interfaceDetails(ctx, m.interfaceName)
	status.Recovery = m.recoveryStatus()
	apSSIDs := make(map[string]bool)
	for _, profile := range profiles {
		if profile.Mode == "ap" {
			apSSIDs[profile.SSID] = true
			status.APActive = status.APActive || profile.Active
			status.ConcurrentMode = status.ConcurrentMode || profile.Active && profile.Device == apInterfaceName
			continue
		}
		if profile.Active {
			status.CurrentConnection = profile.Name
		}
		status.SavedNetworks = append(status.SavedNetworks, SavedNetwork{
			Name: profile.Name, UUID: profile.UUID, SSID: profile.SSID,
			Active: profile.Active, AutoConnect: profile.AutoConnect,
		})
	}
	if status.APActive {
		if status.ConcurrentMode {
			status.SingleRadioNotice = "AP+STA 并发模式已启用；连接不同信道的上游 WiFi 时，热点会短暂切换到相同信道。"
		} else {
			status.SingleRadioNotice = "热点正在占用 wlan0；当前系统没有可用的 AP+STA 并发接口，连接上游 WiFi 会被阻止。"
		}
	}
	saved := make(map[string]bool, len(status.SavedNetworks))
	for _, profile := range status.SavedNetworks {
		saved[profile.SSID] = true
	}
	available, _ := m.cachedAccessPoints(ctx, saved)
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
	status, err := m.WiFiStatus(ctx)
	if err != nil {
		return false, "", err
	}
	if status.APActive && !status.ConcurrentMode {
		return false, "", nil
	}
	if current, found := currentAutoConnectNetwork(status); found {
		available, listErr := m.cachedAccessPoints(ctx, map[string]bool{})
		if listErr != nil {
			return false, current.SSID, listErr
		}
		if target, visible := findAccessPoint(available, current.SSID); visible && target.Active {
			settings, settingsErr := m.APSettings(ctx)
			if settingsErr != nil {
				return false, current.SSID, settingsErr
			}
			if !status.APActive || !settings.Enabled || settings.Channel == target.Channel {
				return false, "", nil
			}
			if err := m.alignAPChannel(ctx, status, target.Channel); err != nil {
				return false, current.SSID, fmt.Errorf("跟随上游 WiFi 信道: %w", err)
			}
			if _, err := m.run(ctx, "nmcli", "connection", "modify", "uuid", current.UUID, "802-11-wireless.bssid", target.BSSID); err != nil {
				return false, current.SSID, fmt.Errorf("更新 %s BSSID: %w", current.SSID, err)
			}
			if _, err := m.run(ctx, "nmcli", "--wait", "15", "connection", "up", "uuid", current.UUID, "ifname", m.interfaceName); err != nil {
				return false, current.SSID, fmt.Errorf("恢复 %s: %w", current.SSID, err)
			}
			return true, current.SSID, nil
		}
	} else if status.CurrentConnection != "" {
		return false, "", nil
	}
	candidates := m.autoConnectCandidates(ctx, status.SavedNetworks)
	if len(candidates) == 0 {
		return false, "", nil
	}
	if _, err := m.run(ctx, "nmcli", "--wait", "12", "device", "wifi", "rescan", "ifname", m.interfaceName); err != nil {
		return false, "", fmt.Errorf("重新扫描上游 WiFi: %w", err)
	}
	available, err := m.cachedAccessPoints(ctx, map[string]bool{})
	if err != nil {
		return false, "", err
	}
	var failures []error
	visible := false
	for _, candidate := range candidates {
		target, found := findAccessPoint(available, candidate.network.SSID)
		if !found {
			continue
		}
		visible = true
		if status.APActive && status.ConcurrentMode {
			if err := m.alignAPChannel(ctx, status, target.Channel); err != nil {
				failures = append(failures, fmt.Errorf("同步 %s 热点信道: %w", candidate.network.SSID, err))
				continue
			}
		}
		if _, err := m.run(ctx, "nmcli", "connection", "modify", "uuid", candidate.network.UUID, "802-11-wireless.bssid", target.BSSID); err != nil {
			failures = append(failures, fmt.Errorf("更新 %s BSSID: %w", candidate.network.SSID, err))
			continue
		}
		if _, err := m.run(ctx, "nmcli", "--wait", "15", "connection", "up", "uuid", candidate.network.UUID, "ifname", m.interfaceName); err != nil {
			failures = append(failures, fmt.Errorf("连接 %s: %w", candidate.network.SSID, err))
			continue
		}
		return true, candidate.network.SSID, nil
	}
	if len(failures) > 0 {
		return false, candidates[0].network.SSID, errors.Join(failures...)
	}
	if !visible {
		return false, candidates[0].network.SSID, errAutoNetworkInvisible
	}
	return false, "", nil
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
		delay := recoveryInterval
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

func (m *Manager) recoveryDue() bool {
	m.recoveryMu.RLock()
	defer m.recoveryMu.RUnlock()
	return m.recovery.NextAttemptAt == nil || !time.Now().Before(*m.recovery.NextAttemptAt)
}

// Scan 手动请求 NetworkManager 扫描，并返回按 SSID 去重后的结果。
// AP+STA 并发模式下扫描在 wlan0 上执行，不影响 ap0 上的热点。
func (m *Manager) Scan(ctx context.Context) ([]AccessPoint, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	status, err := m.WiFiStatus(ctx)
	if err != nil {
		return nil, err
	}
	if status.APActive && !status.ConcurrentMode {
		return nil, ErrAPActive
	}
	if _, err := m.run(ctx, "nmcli", "--wait", "12", "device", "wifi", "rescan", "ifname", m.interfaceName); err != nil {
		return nil, fmt.Errorf("扫描无线网络: %w", err)
	}
	saved := make(map[string]bool, len(status.SavedNetworks))
	for _, profile := range status.SavedNetworks {
		saved[profile.SSID] = true
	}
	return m.cachedAccessPoints(ctx, saved)
}

// Connect 连接已保存网络或创建 WPA/WPA2、开放网络配置。
// AP+STA 并发模式下连接在 wlan0 上执行，ap0 上的热点不受影响。
func (m *Manager) Connect(ctx context.Context, request ConnectRequest) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	status, err := m.WiFiStatus(ctx)
	if err != nil {
		return err
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
	}
	target, err := m.resolveAccessPoint(ctx, ssid, request.BSSID, request.Channel)
	if err != nil {
		return err
	}
	if status.APActive && status.ConcurrentMode {
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
		_, err = m.run(ctx, "nmcli", "--wait", "15", "connection", "up", "uuid", request.UUID, "ifname", m.interfaceName)
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
	args = append(args, "ifname", m.interfaceName, "name", profileName)
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
	if _, err := m.run(ctx, "nmcli", "connection", "modify", "id", profileName, "connection.autoconnect", yesNo(autoConnect)); err != nil {
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
	for _, profile := range profiles {
		if profile.Mode != "ap" {
			continue
		}
		return APSettings{
			Available: true, Enabled: profile.Active, ConnectionName: profile.Name, UUID: profile.UUID,
			Interface: apInterfaceName,
			SSID:      profile.SSID, Band: profile.Band, Channel: profile.Channel,
			Security: profile.Security, Address: profile.Address, AutoConnect: profile.AutoConnect,
			Sharing: profile.IPv4Method == "shared",
		}.withInterfaceDetails(m.interfaceDetails(ctx, apInterfaceName)), nil
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

// UpdateAP 校验并更新现有热点；空密码表示保留原密码。
func (m *Manager) UpdateAP(ctx context.Context, update APUpdate) error {
	m.mu.Lock()
	defer m.mu.Unlock()
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
	if !update.Enabled && !settings.Enabled {
		return nil
	}
	operation := "down"
	if update.Enabled {
		operation = "up"
	}
	if _, err := m.run(ctx, "nmcli", "--wait", "15", "connection", operation, "uuid", settings.UUID); err != nil {
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
	Device                                                      string
	Channel                                                     int
	Active, AutoConnect                                         bool
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
	return item, nil
}

func (m *Manager) cachedAccessPoints(ctx context.Context, saved map[string]bool) ([]AccessPoint, error) {
	output, err := m.run(ctx, "nmcli", "-t", "--escape", "yes", "-f", "IN-USE,SSID,BSSID,SIGNAL,CHAN,SECURITY", "device", "wifi", "list", "ifname", m.interfaceName, "--rescan", "no")
	if err != nil {
		return nil, fmt.Errorf("读取无线网络: %w", err)
	}
	return parseAccessPoints(string(output), saved), nil
}

func (m *Manager) resolveAccessPoint(ctx context.Context, ssid, bssid string, channel int) (AccessPoint, error) {
	if err := validateSSID(ssid); err != nil {
		return AccessPoint{}, err
	}
	if bssid != "" && !validBSSID(bssid) {
		return AccessPoint{}, invalidInput("WiFi BSSID 无效")
	}
	items, err := m.cachedAccessPoints(ctx, map[string]bool{})
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
	if status.CurrentConnection != "" {
		if _, err := m.run(ctx, "nmcli", "device", "disconnect", m.interfaceName); err != nil {
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
		return fmt.Errorf("同步热点信道: %w", err)
	}
	if _, err := m.run(ctx, "nmcli", "--wait", "20", "connection", "up", "uuid", settings.UUID, "ifname", apInterfaceName); err != nil {
		return fmt.Errorf("在上游 WiFi 信道重启热点: %w", err)
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
	if channel >= 36 && channel <= 165 {
		return "a", true
	}
	return "", false
}

func validBandChannel(band string, channel int) bool {
	if band == "a" {
		return channel == 0 || channel == 149 || channel == 153 || channel == 157 || channel == 161
	}
	if band == "bg" {
		return channel == 0 || channel == 1 || channel == 6 || channel == 11
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
