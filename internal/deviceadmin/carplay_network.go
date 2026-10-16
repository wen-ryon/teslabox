package deviceadmin

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"
)

func carPlayStartupChannel(ctx context.Context, mode string, connected bool, read func(context.Context) int) int {
	channel := read(ctx)
	if channel != 0 || mode != "same_channel" || !connected {
		return channel
	}
	// NM 会先报告活动配置再完成关联；短暂等待真实信道，避免整项服务退出后重新枚举网络。
	waitCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	ticker := time.NewTicker(250 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-waitCtx.Done():
			return 0
		case <-ticker.C:
			if channel = read(waitCtx); channel != 0 {
				return channel
			}
		}
	}
}

func (m *Manager) carPlayNetworkSnapshot(ctx context.Context) (APSettings, WiFiStatus, error) {
	// 启动只需要上游身份与信道，不读取管理页的扫描列表，也不重复枚举每个已保存配置。
	profiles, err := m.wirelessProfiles(ctx)
	if err != nil {
		return APSettings{}, WiFiStatus{}, err
	}
	settings, err := m.apSettingsFromProfiles(ctx, profiles)
	if err != nil {
		return APSettings{}, WiFiStatus{}, err
	}
	wifiInterface, _, available, _ := m.networkInterfaces(ctx, profiles)
	status := WiFiStatus{Interface: wifiInterface, Available: available}
	for _, item := range profiles {
		if item.Mode != "ap" && item.Active {
			status.CurrentConnection = item.SSID
			status.currentConnectionUUID = item.UUID
		}
		if item.Mode != "ap" {
			status.SavedNetworks = append(status.SavedNetworks, SavedNetwork{
				UUID: item.UUID, SSID: item.SSID, AutoConnect: item.AutoConnect,
				RequiresPassword: item.RequiresPassword,
			})
		}
	}
	return settings, status, nil
}

func (m *Manager) prepareCarPlayStartupUpstream(ctx context.Context, settings APSettings, status WiFiStatus, channels []int) bool {
	// 租约会暂停单射频自动连接；在建 AP 前给可见上游一次有界机会，避免建好后又拆建。
	if settings.ConcurrencyMode != "same_channel" || settings.Enabled || !status.Available ||
		status.Interface == "" || status.CurrentConnection != "" || m.manualWiFiDisconnect {
		return false
	}
	preference, priority, err := m.carPlayNetworkPreference()
	if err != nil || priority == "carplay" || preference != "auto" && preference != "wifi" {
		return false
	}
	if preference == "auto" {
		active, err := m.run(ctx, "nmcli", "-t", "--escape", "yes", "-f", "UUID,TYPE,DEVICE", "connection", "show", "--active")
		if err != nil {
			return false
		}
		for _, line := range strings.Split(strings.TrimSpace(string(active)), "\n") {
			fields := splitNMCLI(line)
			if len(fields) == 3 && fields[1] == "802-3-ethernet" && carPlayInterfacePattern.MatchString(fields[2]) {
				return false
			}
		}
	}
	startupCtx, cancel := context.WithTimeout(ctx, 12*time.Second)
	defer cancel()
	state, err := m.wifiInterfaceState(startupCtx, status.Interface)
	if err != nil {
		return false
	}
	if networkManagerActivationInProgress(state) {
		// NM 已经在认证或获取地址，不能 connection up 重置这次激活。
		_ = m.waitWiFiActivation(startupCtx, status.Interface)
		return true
	}
	if state != 30 {
		return false
	}
	points, err := m.cachedAccessPoints(startupCtx, status.Interface, nil)
	if err != nil {
		return false
	}
	allowed := make(map[int]bool, len(settings.AllowedChannels))
	for _, channel := range settings.AllowedChannels {
		allowed[channel] = true
	}
	compatible := make(map[int]bool, len(channels))
	for _, channel := range channels {
		compatible[channel] = allowed[channel]
	}
	filtered := make([]AccessPoint, 0, len(points))
	for _, point := range points {
		if compatible[point.Channel] {
			filtered = append(filtered, point)
		}
	}
	profiles := make([]SavedNetwork, 0, len(status.SavedNetworks))
	for _, candidate := range m.autoConnectCandidates(startupCtx, status.SavedNetworks) {
		if time.Now().After(m.carPlayWiFiFailures[candidate.network.UUID]) {
			profiles = append(profiles, candidate.network)
		}
	}
	uuid, _ := carPlayWiFiRecoveryTarget(CarPlayNetworkLease{}, profiles, filtered)
	if uuid == "" || startupCtx.Err() != nil {
		return false
	}
	// 选点与认证仍由 NM 完成；不扫描、不修改凭据或设备自动连接策略。
	started := time.Now()
	if automatic, readErr := m.run(startupCtx, "nmcli", "-g", "GENERAL.AUTOCONNECT", "device", "show", status.Interface); readErr == nil && strings.TrimSpace(string(automatic)) == "yes" {
		// 首次扫描结果出现后，NM 的自动激活可能晚于状态查询；短暂让出选点机会，避免两次激活竞速。
		if observed := m.waitWiFiAutoConnect(startupCtx, status.Interface); observed {
			m.logger.Info("CarPlay 启动前采用系统无线连接", "elapsed_ms", time.Since(started).Milliseconds())
			return true
		}
	}
	err = m.activateSavedWiFi(startupCtx, uuid, status.Interface,
		[]string{"--wait", "8", "connection", "up", "uuid", uuid, "ifname", status.Interface})
	if isNMCLIWaitTimeout(err) {
		err = m.waitWiFiActivation(startupCtx, status.Interface)
	}
	m.logger.Info("CarPlay 启动前上游连接结束", "elapsed_ms", time.Since(started).Milliseconds(), "connected", err == nil)
	return true
}

func (m *Manager) waitWiFiAutoConnect(ctx context.Context, interfaceName string) bool {
	graceCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	ticker := time.NewTicker(250 * time.Millisecond)
	defer ticker.Stop()
	for {
		state, err := m.wifiInterfaceState(graceCtx, interfaceName)
		if err != nil {
			return false
		}
		if state == 100 {
			return true
		}
		if networkManagerActivationInProgress(state) {
			_ = m.waitWiFiActivation(ctx, interfaceName)
			return true
		}
		select {
		case <-graceCtx.Done():
			return false
		case <-ticker.C:
		}
	}
}

func (m *Manager) activateSavedWiFi(ctx context.Context, uuid, interfaceName string, args []string) error {
	// 恢复设备自动连接后 NM 可能已经开始激活同一配置，重复 up 会打断 DHCP。
	current, err := m.run(ctx, "nmcli", "-g", "GENERAL.CON-UUID", "device", "show", interfaceName)
	if err == nil && strings.TrimSpace(string(current)) == uuid {
		state, stateErr := m.wifiInterfaceState(ctx, interfaceName)
		if stateErr == nil {
			if state == 100 {
				return nil
			}
			if networkManagerActivationInProgress(state) {
				waitCtx, cancel := context.WithTimeout(ctx, recoveryTimeout)
				defer cancel()
				return m.waitWiFiUUIDActivation(waitCtx, interfaceName, uuid)
			}
		}
	}
	_, err = m.run(ctx, "nmcli", args...)
	if isNMCLIWaitTimeout(err) {
		current, queryErr := m.run(ctx, "nmcli", "-g", "GENERAL.CON-UUID", "device", "show", interfaceName)
		if queryErr == nil && strings.TrimSpace(string(current)) == uuid {
			// CLI 的短等待结束不代表 NM 的认证/DHCP 已结束；等待原激活，不能重复 up。
			waitCtx, cancel := context.WithTimeout(ctx, recoveryTimeout)
			defer cancel()
			return m.waitWiFiUUIDActivation(waitCtx, interfaceName, uuid)
		}
	}
	return err
}

func (m *Manager) waitWiFiUUIDActivation(ctx context.Context, interfaceName, uuid string) error {
	if err := m.waitWiFiActivation(ctx, interfaceName); err != nil {
		return err
	}
	current, err := m.run(ctx, "nmcli", "-g", "GENERAL.CON-UUID", "device", "show", interfaceName)
	if err != nil {
		return err
	}
	if strings.TrimSpace(string(current)) != uuid {
		return errors.New("无线激活期间连接已变化，目标网络未确认连接")
	}
	return nil
}

func (m *Manager) waitWiFiActivation(ctx context.Context, interfaceName string) error {
	ticker := time.NewTicker(250 * time.Millisecond)
	defer ticker.Stop()
	for {
		state, err := m.wifiInterfaceState(ctx, interfaceName)
		if err != nil {
			return err
		}
		if state == 100 {
			return nil
		}
		if !networkManagerActivationInProgress(state) {
			return fmt.Errorf("无线激活已结束但尚未连接")
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
	}
}

// CarPlayNetworkRequest 显式提供车机支持的信道，避免根据用户语言猜测车辆地区。
type CarPlayNetworkRequest struct {
	Channels []int `json:"channels"`
}

// CarPlayNetworkPlan 是只读决策快照；执行切换前必须重新校验并取得网络租约。
type CarPlayNetworkPlan struct {
	APInterface         string `json:"ap_interface"`
	APUUID              string `json:"ap_uuid"`
	Channel             int    `json:"channel"`
	ConcurrencyMode     string `json:"concurrency_mode"`
	UpstreamInterface   string `json:"upstream_interface,omitempty"`
	UpstreamUUID        string `json:"upstream_uuid,omitempty"`
	PauseUpstream       bool   `json:"pause_upstream"`
	DisableWiFiUpstream bool   `json:"disable_wifi_upstream,omitempty"`
}

// PlanCarPlayNetwork 在断开上游前验证车机与网卡的信道交集，不修改 NetworkManager 配置。
func (m *Manager) PlanCarPlayNetwork(ctx context.Context, request CarPlayNetworkRequest) (CarPlayNetworkPlan, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := validateCarPlayChannels(request.Channels); err != nil {
		return CarPlayNetworkPlan{}, err
	}
	settings, status, err := m.carPlayNetworkSnapshot(ctx)
	if err != nil {
		return CarPlayNetworkPlan{}, err
	}
	_, upstreamChannel := m.activeWirelessChannel(ctx, status.Interface)
	preference, _, err := m.carPlayNetworkPreference()
	if err != nil {
		return CarPlayNetworkPlan{}, err
	}
	return planCarPlayNetworkForSource(settings, status, upstreamChannel, request.Channels, preference)
}

func planCarPlayNetworkForSource(settings APSettings, status WiFiStatus, upstreamChannel int, channels []int, source string) (CarPlayNetworkPlan, error) {
	if source != "bluetooth" {
		return planCarPlayNetwork(settings, status, upstreamChannel, channels)
	}
	plan, err := planCarPlayNetwork(settings, status, upstreamChannel, channels)
	if err == nil && plan.PauseUpstream {
		return plan, invalidInput("蓝牙供网验证前不能中断现有无线连接")
	}
	return plan, err
}

func planCarPlayBluetoothTarget(settings APSettings, status WiFiStatus, channels []int) (CarPlayNetworkPlan, error) {
	settings.Enabled = false
	// 蓝牙专用供网不受旧 STA 信道约束；只在监管域和车机声明的交集中选非 DFS 五 GHz。
	preferred := []int{36, 40, 44, 48, 149, 153, 157, 161}
	selected := 0
	for _, channel := range preferred {
		allowed, vehicle := false, false
		for _, value := range settings.AllowedChannels {
			allowed = allowed || value == channel
		}
		for _, value := range channels {
			vehicle = vehicle || value == channel
		}
		if allowed && vehicle {
			if selected == 0 || settings.Channel == channel {
				selected = channel
			}
			if settings.Channel == channel {
				break
			}
		}
	}
	if selected != 0 {
		settings.Channel = selected
	}
	upstream := status.currentConnectionUUID
	if status.CurrentConnection != "" && !validUUID(upstream) {
		return CarPlayNetworkPlan{}, invalidInput("无法确认蓝牙供网前的无线连接身份")
	}
	status.CurrentConnection, status.currentConnectionUUID = "", ""
	plan, err := planCarPlayNetwork(settings, status, 0, channels)
	if err != nil {
		return plan, err
	}
	plan.UpstreamUUID, plan.PauseUpstream = upstream, upstream != ""
	plan.DisableWiFiUpstream = true
	return plan, nil
}

func validateCarPlayChannels(channels []int) error {
	if len(channels) == 0 || len(channels) > 64 {
		return invalidInput("必须提供 1 到 64 个车机支持的信道")
	}
	seen := make(map[int]bool, len(channels))
	for _, channel := range channels {
		if !validCarPlayChannel(channel) || seen[channel] {
			return invalidInput("车机信道必须互不重复，范围为 1 到 13 或 36 到 177")
		}
		seen[channel] = true
	}
	return nil
}

func validCarPlayChannel(channel int) bool {
	return channel >= 1 && channel <= 13 || channel >= 36 && channel <= 177
}

func planCarPlayNetwork(settings APSettings, status WiFiStatus, upstreamChannel int, channels []int) (CarPlayNetworkPlan, error) {
	if !settings.Available || settings.Interface == "" || !validUUID(settings.UUID) {
		return CarPlayNetworkPlan{}, ErrWiFiUnavailable
	}
	if settings.ReasonCode == "ap_interface_conflict" {
		return CarPlayNetworkPlan{}, &APOperationError{Code: settings.ReasonCode,
			Message:    "热点接口存在其他连接配置，无法安排 CarPlay 网络",
			Suggestion: "请先检查并解决热点接口的冲突配置。"}
	}
	switch settings.ConcurrencyMode {
	case "independent_radio", "multi_channel", "same_channel", "exclusive":
	default:
		return CarPlayNetworkPlan{}, &APOperationError{Code: "capability_unavailable",
			Message:    "无法确认 CarPlay 热点与上游 WiFi 的并发能力",
			Suggestion: "请检查实际无线设备、驱动和接口配置。"}
	}
	allowed := make(map[int]bool, len(settings.AllowedChannels))
	for _, channel := range settings.AllowedChannels {
		allowed[channel] = true
	}
	compatible := make(map[int]bool, len(channels))
	selected := 0
	for _, channel := range channels {
		if allowed[channel] {
			compatible[channel] = true
			if selected == 0 {
				selected = channel
			}
		}
	}
	if selected == 0 {
		return CarPlayNetworkPlan{}, &APOperationError{Code: "no_carplay_channel",
			Message:    "车机支持的信道与网卡允许创建热点的信道没有交集",
			Suggestion: "请核对车辆地区、无线监管域和网卡的热点能力。"}
	}
	// 蓝牙或有线上游没有 Wi-Fi 信道约束，不能因车机列表排序丢掉用户保存的 AP 信道。
	if !settings.Enabled && compatible[settings.Channel] {
		selected = settings.Channel
	}
	// 已运行的兼容热点优先保持信道，避免规划本身建议中断活动会话。
	if settings.Enabled {
		if !compatible[settings.ActiveChannel] {
			return CarPlayNetworkPlan{}, &APOperationError{Code: "active_ap_channel_incompatible",
				Message:    "当前热点的活动信道不在车机支持列表中",
				Suggestion: "请在没有活动会话时调整热点信道后重试。"}
		}
		selected = settings.ActiveChannel
	}
	connected := status.CurrentConnection != ""
	if connected && (!validUUID(status.currentConnectionUUID) || status.Interface == "") {
		return CarPlayNetworkPlan{}, &APOperationError{Code: "upstream_identity_unavailable",
			Message:    "无法确认当前上游连接的恢复身份",
			Suggestion: "请等待 NetworkManager 连接状态稳定后重试。"}
	}
	pause := false
	if connected {
		switch settings.ConcurrencyMode {
		case "exclusive":
			pause = true
		case "same_channel":
			if upstreamChannel == 0 {
				return CarPlayNetworkPlan{}, &APOperationError{Code: "upstream_channel_unavailable",
					Message:    "无法读取共享无线设备的上游活动信道",
					Suggestion: "请检查 iw 和当前 WiFi 连接状态。"}
			}
			if !settings.Enabled && compatible[upstreamChannel] {
				selected = upstreamChannel
			}
			pause = selected != upstreamChannel
		}
	}
	return CarPlayNetworkPlan{
		APInterface: settings.Interface, APUUID: settings.UUID, Channel: selected,
		ConcurrencyMode: settings.ConcurrencyMode, UpstreamInterface: status.Interface,
		UpstreamUUID: status.currentConnectionUUID, PauseUpstream: pause,
	}, nil
}
