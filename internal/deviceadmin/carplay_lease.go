package deviceadmin

import (
	"context"
	"crypto/rand"
	"fmt"
	"github.com/wen-ryon/teslabox/internal/carplaysettings"
	"net/netip"
	"regexp"
	"strings"
	"time"
)

var carPlayOwnerPattern = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9._-]{0,63}$`)
var carPlayInterfacePattern = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_.-]{0,14}$`)

// 只认领本进程首次快照中已保存的同信道连接，重启后不从旧记录推断授权。
type carPlayStartupAdoption struct {
	leaseUUID  string
	expiresAt  time.Time
	candidates map[string]bool
}

// CarPlayNetworkLease 保存断开前的恢复身份及唯一临时热点身份，不保存无线密码。
type CarPlayNetworkLease struct {
	ForwardTable              int                `json:"forward_table,omitempty"`
	ForwardInterface          string             `json:"forward_interface,omitempty"`
	ForwardGateway            string             `json:"forward_gateway,omitempty"`
	ForwardSource             string             `json:"forward_source,omitempty"`
	WebAliasAddress           string             `json:"web_alias_address,omitempty"`
	WebAliasTarget            string             `json:"web_alias_target,omitempty"`
	Owner                     string             `json:"owner"`
	ExpiresAt                 time.Time          `json:"expires_at"`
	Plan                      CarPlayNetworkPlan `json:"plan"`
	RuntimeAPUUID             string             `json:"runtime_ap_uuid"`
	RuntimeAPName             string             `json:"runtime_ap_name"`
	RuntimePANUUID            string             `json:"runtime_pan_uuid,omitempty"`
	RuntimePANAddress         string             `json:"runtime_pan_address,omitempty"`
	DeviceAutoConnectManaged  bool               `json:"device_autoconnect_managed"`
	OriginalDeviceAutoConnect bool               `json:"original_device_autoconnect"`
}

func carPlayNetworkBusy() error {
	return &APOperationError{Code: "carplay_network_reserved", Message: "CarPlay 正在使用无线网络",
		Suggestion: "可确认暂时中断 CarPlay 后强制连接 Wi-Fi，连接结束后自动恢复 CarPlay；录像继续运行。"}
}

func (m *Manager) carPlayBlocksWiFiLocked() bool {
	return m.carPlayRecoveryUncertain || m.carPlayWiFiReconnect || m.carPlayEnrollmentHandoff != nil && (!m.carPlayEnrollmentHandoff.Committed || m.carPlayEnrollmentHandoff.Restoring) || m.carPlayNetworkHold != nil &&
		(m.carPlayNetworkHold.Plan.DisableWiFiUpstream || m.carPlayNetworkHold.Plan.ConcurrencyMode == "same_channel" || m.carPlayNetworkHold.Plan.ConcurrencyMode == "exclusive")
}

func (m *Manager) carPlayBluetoothWiFiGuardLocked() error {
	if m.carPlayNetworkHold != nil && m.carPlayNetworkHold.Plan.DisableWiFiUpstream {
		return &APOperationError{Code: "carplay_bluetooth_source", Message: "蓝牙共享网络模式已暂停无线供网",
			Suggestion: "请先在 CarPlay 设置中将网络来源切换为自动或无线，再连接 Wi-Fi。"}
	}
	return nil
}

func (m *Manager) carPlayNetworkReservedLocked() bool {
	return m.carPlayRecoveryUncertain || m.carPlayWiFiReconnect || m.carPlayNetworkHold != nil || m.carPlayEnrollmentHandoff != nil && (!m.carPlayEnrollmentHandoff.Committed || m.carPlayEnrollmentHandoff.Restoring)
}

// GuardCarPlayRecovery 在无法信任恢复记录时保留当前网络，不猜测连接身份或阻止其他业务启动。
func (m *Manager) GuardCarPlayRecovery() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.carPlayRecoveryUncertain = true
	m.apUpdateGeneration++
}

func validateCarPlayLease(lease CarPlayNetworkLease) error {
	if lease.ForwardTable != 0 && (lease.ForwardTable < 20000 || lease.ForwardTable >= 30000 || lease.ForwardInterface != "" && !carPlayInterfacePattern.MatchString(lease.ForwardInterface)) {
		return invalidInput("CarPlay 出网路由恢复身份无效")
	}
	if lease.ForwardTable == 0 && (lease.ForwardInterface != "" || lease.ForwardGateway != "" || lease.ForwardSource != "") {
		return invalidInput("CarPlay 出网恢复记录缺少专属路由表")
	}
	if lease.ForwardTable != 0 && lease.ForwardSource != "wifi" && lease.ForwardSource != "ethernet" && lease.ForwardSource != "bluetooth" {
		return invalidInput("CarPlay 出网恢复来源无效")
	}
	if lease.ForwardGateway != "" {
		address, err := netip.ParseAddr(lease.ForwardGateway)
		if err != nil || !address.Is4() || address.IsUnspecified() || address.IsMulticast() || lease.ForwardInterface == "" {
			return invalidInput("CarPlay 出网恢复网关无效")
		}
	}
	if err := validateCarPlayWebAlias(lease); err != nil {
		return err
	}
	if lease.RuntimePANUUID == "" && lease.RuntimePANAddress != "" || lease.RuntimePANUUID != "" && !carPlayBluetoothAddressPattern.MatchString(lease.RuntimePANAddress) {
		return invalidInput("CarPlay 蓝牙供网恢复地址无效")
	}
	if lease.RuntimePANUUID != "" && (!validUUID(lease.RuntimePANUUID) || lease.RuntimePANUUID == lease.RuntimeAPUUID || lease.RuntimePANUUID == lease.Plan.APUUID || lease.RuntimePANUUID == lease.Plan.UpstreamUUID) {
		return invalidInput("CarPlay 蓝牙供网恢复身份无效")
	}
	plan := lease.Plan
	if !carPlayOwnerPattern.MatchString(lease.Owner) || lease.ExpiresAt.IsZero() ||
		!validUUID(plan.APUUID) || !validUUID(lease.RuntimeAPUUID) || plan.APUUID == lease.RuntimeAPUUID ||
		lease.RuntimeAPName != "TeslaBox-CarPlay-lease-"+lease.RuntimeAPUUID ||
		!carPlayInterfacePattern.MatchString(plan.APInterface) || !validCarPlayChannel(plan.Channel) {
		return invalidInput("CarPlay 网络租约的恢复记录无效")
	}
	if plan.UpstreamUUID != "" && (!validUUID(plan.UpstreamUUID) || !carPlayInterfacePattern.MatchString(plan.UpstreamInterface)) {
		return invalidInput("CarPlay 上游恢复记录无效")
	}
	if plan.PauseUpstream && plan.UpstreamUUID == "" {
		return invalidInput("CarPlay 租约缺少上游恢复身份")
	}
	if lease.DeviceAutoConnectManaged && !carPlayInterfacePattern.MatchString(plan.UpstreamInterface) {
		return invalidInput("CarPlay 租约缺少设备自动连接的恢复接口")
	}
	if plan.DisableWiFiUpstream && !lease.DeviceAutoConnectManaged {
		return invalidInput("蓝牙供网租约缺少无线自动连接恢复记录")
	}
	if plan.DisableWiFiUpstream && plan.UpstreamUUID != "" && !plan.PauseUpstream {
		return invalidInput("蓝牙供网租约没有暂停原无线连接")
	}
	switch plan.ConcurrencyMode {
	case "same_channel", "exclusive":
		if !lease.DeviceAutoConnectManaged {
			return invalidInput("共享无线设备的租约缺少自动连接恢复记录")
		}
	case "independent_radio", "multi_channel":
		if plan.PauseUpstream && !plan.DisableWiFiUpstream {
			return invalidInput("独立信道租约不能暂停上游")
		}
	default:
		return invalidInput("CarPlay 租约的无线并发能力无效")
	}
	return nil
}

// RestoreCarPlayNetworkHold 在网络自愈启动前恢复保护，租约是否过期由自动化服务负责回收。
func (m *Manager) RestoreCarPlayNetworkHold(lease CarPlayNetworkLease) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := validateCarPlayLease(lease); err != nil {
		return err
	}
	if m.carPlayNetworkHold != nil {
		return carPlayNetworkBusy()
	}
	snapshot := lease
	m.carPlayNetworkHold = &snapshot
	m.apUpdateGeneration++
	return nil
}

// AcquireCarPlayNetwork 先写入恢复日志再暂停上游，失败后保留租约供显式释放或到期回收。
// checkpoint 必须同步持久化；回调不应再次调用设备管理器，以免重复取得网络锁。
func (m *Manager) AcquireCarPlayNetwork(ctx context.Context, owner string, request CarPlayNetworkRequest,
	expiresAt time.Time, checkpoint func(*CarPlayNetworkLease) error,
) (CarPlayNetworkLease, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.carPlayRecoveryUncertain || m.carPlayWiFiReconnect {
		return CarPlayNetworkLease{}, carPlayNetworkBusy()
	}
	if value := m.carPlayEnrollmentHandoff; value != nil && (value.Restoring || !value.Committed && m.carPlayEnrollmentLive != value.Operation) {
		return CarPlayNetworkLease{}, carPlayNetworkBusy()
	}
	if checkpoint == nil || !carPlayOwnerPattern.MatchString(owner) || !expiresAt.After(time.Now()) || expiresAt.After(time.Now().Add(time.Hour)) {
		return CarPlayNetworkLease{}, invalidInput("CarPlay 租约参数无效")
	}
	if err := validateCarPlayChannels(request.Channels); err != nil {
		return CarPlayNetworkLease{}, err
	}
	preference, _, preferenceErr := m.carPlayNetworkPreference()
	if preferenceErr != nil {
		return CarPlayNetworkLease{}, preferenceErr
	}
	var lease CarPlayNetworkLease
	upstreamRestored := false
	firstAcquire := m.carPlayNetworkHold == nil
	var startupUpstreams map[string]bool
	if m.carPlayNetworkHold != nil {
		lease = *m.carPlayNetworkHold
		if lease.Owner != owner {
			return lease, carPlayNetworkBusy()
		}
		compatible := false
		for _, channel := range request.Channels {
			compatible = compatible || channel == lease.Plan.Channel
		}
		if !compatible {
			return lease, invalidInput("续租不能改变活动 CarPlay 热点信道")
		}
		if pending := m.carPlayStartupAdoption; pending != nil && pending.leaseUUID == lease.RuntimeAPUUID && time.Now().Before(pending.expiresAt) {
			startupUpstreams = pending.candidates
		}
	} else {
		settings, status, err := m.carPlayNetworkSnapshot(ctx)
		if err != nil {
			return lease, err
		}
		if settings.Enabled {
			return lease, ErrAPActive
		}
		if m.prepareCarPlayStartupUpstream(ctx, settings, status, request.Channels) {
			if err := ctx.Err(); err != nil {
				return lease, err
			}
			settings, status, err = m.carPlayNetworkSnapshot(ctx)
			if err != nil {
				return lease, err
			}
			if settings.Enabled {
				return lease, ErrAPActive
			}
		}
		channel := carPlayStartupChannel(ctx, settings.ConcurrencyMode, status.CurrentConnection != "", func(readCtx context.Context) int {
			_, current := m.activeWirelessChannel(readCtx, status.Interface)
			return current
		})
		plan, err := planCarPlayNetworkForSource(settings, status, channel, request.Channels, preference)
		if err != nil {
			return lease, err
		}
		upstreamRestored = validUUID(plan.UpstreamUUID) && status.currentConnectionUUID == plan.UpstreamUUID && !plan.PauseUpstream
		var identity [16]byte
		if _, err := rand.Read(identity[:]); err != nil {
			return lease, err
		}
		identity[6] = (identity[6] & 15) | 64
		identity[8] = (identity[8] & 63) | 128
		uuid := fmt.Sprintf("%x-%x-%x-%x-%x", identity[:4], identity[4:6], identity[6:8], identity[8:10], identity[10:])
		lease = CarPlayNetworkLease{Owner: owner, Plan: plan, RuntimeAPUUID: uuid, RuntimeAPName: "TeslaBox-CarPlay-lease-" + uuid}
		if m.carPlayPublicAddress != "" {
			target, err := m.carPlayWebAliasTarget(ctx, plan.APUUID)
			if err != nil {
				return lease, err
			}
			lease.WebAliasAddress, lease.WebAliasTarget = m.carPlayPublicAddress, target
			if err := validateCarPlayWebAlias(lease); err != nil {
				return lease, err
			}
		}
		if plan.DisableWiFiUpstream || plan.ConcurrencyMode == "same_channel" || plan.ConcurrencyMode == "exclusive" {
			startupUpstreams = make(map[string]bool, len(status.SavedNetworks))
			for _, network := range status.SavedNetworks {
				startupUpstreams[network.UUID] = validUUID(network.UUID)
			}
			if !carPlayInterfacePattern.MatchString(plan.UpstreamInterface) {
				return lease, invalidInput("无法确认共享设备的上游接口")
			}
			value, err := m.run(ctx, "nmcli", "-g", "GENERAL.AUTOCONNECT", "device", "show", plan.UpstreamInterface)
			if err != nil {
				return lease, err
			}
			switch strings.TrimSpace(string(value)) {
			case "yes":
				lease.OriginalDeviceAutoConnect = true
			case "no":
				lease.OriginalDeviceAutoConnect = false
			default:
				return lease, invalidInput("无法确认设备自动连接的原始状态")
			}
			lease.DeviceAutoConnectManaged = true
		}
	}
	lease.ExpiresAt = expiresAt
	shouldDisconnect := false
	if lease.DeviceAutoConnectManaged {
		active, err := m.run(ctx, "nmcli", "-t", "-f", "UUID,DEVICE", "connection", "show", "--active")
		if err != nil {
			return lease, err
		}
		ownedAPActive := false
		for _, line := range strings.Split(strings.TrimSpace(string(active)), "\n") {
			fields := splitNMCLI(line)
			ownedAPActive = ownedAPActive || len(fields) == 2 && fields[0] == lease.RuntimeAPUUID && fields[1] == lease.Plan.APInterface
		}
		for _, line := range strings.Split(strings.TrimSpace(string(active)), "\n") {
			fields := splitNMCLI(line)
			if len(fields) != 2 || fields[1] != lease.Plan.UpstreamInterface {
				continue
			}
			if fields[0] == lease.RuntimeAPUUID {
				// 互斥接口已切成 AP，续租不能再对整个设备执行 disconnect。
				continue
			}
			if fields[0] != lease.Plan.UpstreamUUID {
				if lease.Plan.UpstreamUUID != "" || !startupUpstreams[fields[0]] || firstAcquire && ownedAPActive {
					return lease, carPlayNetworkBusy()
				}
				if !firstAcquire {
					// 蓝牙供网尚未验证时不能取消排队的 Wi-Fi；它可能在 AP 启动后才完成。
					// 只接受短启动窗口内、已知配置且 AP/STA 实际同信道的晚到连接。
					if preference != "bluetooth" || lease.Plan.DisableWiFiUpstream || lease.Plan.ConcurrencyMode != "same_channel" || !ownedAPActive {
						return lease, carPlayNetworkBusy()
					}
					_, channel := m.activeWirelessChannel(ctx, lease.Plan.UpstreamInterface)
					runtime := m.activeAPRuntime(ctx, lease.Plan.APInterface)
					if channel != lease.Plan.Channel || runtime.Type != "AP" || runtime.Channel != lease.Plan.Channel {
						return lease, carPlayNetworkBusy()
					}
				}
				lease.Plan.UpstreamUUID = fields[0]
				lease.Plan.PauseUpstream = firstAcquire
			}
			if !lease.Plan.PauseUpstream {
				_, channel := m.activeWirelessChannel(ctx, lease.Plan.UpstreamInterface)
				if channel == 0 {
					// 上游正在消失时 NM 仍可能报告原连接；已确认自己的 AP 保持原信道即可续租。
					// 不能据这个过渡窗口回收媒体和 PAN，也不能把它当成新信道进行切换。
					if ownedAPActive {
						runtime := m.activeAPRuntime(ctx, lease.Plan.APInterface)
						if runtime.Type == "AP" && runtime.Channel == lease.Plan.Channel {
							continue
						}
					}
					return lease, invalidInput("无法确认 CarPlay 续租时的上游信道")
				}
				lease.Plan.PauseUpstream = channel != lease.Plan.Channel
				upstreamRestored = !lease.Plan.PauseUpstream
			}
			shouldDisconnect = lease.Plan.PauseUpstream
		}
	}
	if preference == "bluetooth" && !lease.Plan.DisableWiFiUpstream && shouldDisconnect {
		return lease, invalidInput("蓝牙供网尚未验证，保留现有无线连接")
	}
	if err := checkpoint(&lease); err != nil {
		return lease, err
	}
	snapshot := lease
	m.carPlayNetworkHold = &snapshot
	if firstAcquire {
		m.carPlayStartupAdoption = &carPlayStartupAdoption{leaseUUID: lease.RuntimeAPUUID,
			expiresAt: time.Now().Add(time.Minute), candidates: startupUpstreams}
	}
	if upstreamRestored {
		// 同信道上游已经恢复，后续断线属于新事件，不能继续等旧冷却。
		m.carPlayWiFiRecoveryAfter = time.Time{}
	}
	m.carPlayVehicleChannels = append([]int(nil), request.Channels...)
	m.apUpdateGeneration++
	if lease.DeviceAutoConnectManaged {
		if _, err := m.run(ctx, "nmcli", "device", "set", lease.Plan.UpstreamInterface, "autoconnect", "no"); err != nil {
			return lease, fmt.Errorf("暂停共享设备的自动连接: %w", err)
		}
	}
	if shouldDisconnect {
		if _, err := m.run(ctx, "nmcli", "--wait", "15", "connection", "down", "uuid", lease.Plan.UpstreamUUID); err != nil && !isNMDeviceInactive(err) {
			return lease, fmt.Errorf("为 CarPlay 暂停上游 WiFi: %w", err)
		}
	} else if firstAcquire && preference != "bluetooth" && lease.DeviceAutoConnectManaged && lease.Plan.UpstreamUUID == "" {
		// autoconnect=no 不会取消已经排队的激活；AP 启动前取消它，避免首次续租回收热点。
		// 原设备策略已持久化，释放租约时恢复自动连接；这里不修改保存的 Wi-Fi 配置。
		if _, err := m.run(ctx, "nmcli", "--wait", "15", "device", "disconnect", lease.Plan.UpstreamInterface); err != nil && !isNMDeviceInactive(err) {
			return lease, fmt.Errorf("为 CarPlay 取消进行中的上游连接: %w", err)
		}
	}
	return lease, nil
}

// ReleaseCarPlayNetwork 只清理本次分配的热点身份，恢复上游成功后才清除持久恢复日志。
func (m *Manager) ReleaseCarPlayNetwork(ctx context.Context, owner string, checkpoint func(*CarPlayNetworkLease) error) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.carPlayRecoveryUncertain {
		return carPlayNetworkBusy()
	}
	if m.carPlayNetworkHold == nil {
		return nil
	}
	lease := *m.carPlayNetworkHold
	if checkpoint == nil || lease.Owner != owner {
		return carPlayNetworkBusy()
	}
	if err := m.removeCarPlayForwarding(ctx, lease); err != nil {
		return err
	}
	if err := m.removeCarPlayPAN(ctx, lease); err != nil {
		return err
	}
	profiles, err := m.run(ctx, "nmcli", "-t", "--escape", "yes", "-f", "UUID,NAME", "connection", "show")
	if err != nil {
		return err
	}
	for _, line := range strings.Split(strings.TrimSpace(string(profiles)), "\n") {
		fields := splitNMCLI(line)
		if len(fields) != 2 || fields[0] != lease.RuntimeAPUUID {
			continue
		}
		if fields[1] != lease.RuntimeAPName {
			return invalidInput("CarPlay 临时热点身份已改变，拒绝清理其他连接")
		}
		identity, err := m.run(ctx, "nmcli", "-g", "connection.interface-name,802-11-wireless.mode", "connection", "show", "uuid", lease.RuntimeAPUUID)
		if err != nil || strings.TrimSpace(string(identity)) != lease.Plan.APInterface+"\nap" {
			return invalidInput("CarPlay 临时热点接口已改变，拒绝清理")
		}
		active, err := m.run(ctx, "nmcli", "-t", "-f", "UUID,DEVICE", "connection", "show", "--active")
		if err != nil {
			return err
		}
		for _, connection := range strings.Split(strings.TrimSpace(string(active)), "\n") {
			identity := splitNMCLI(connection)
			if len(identity) == 2 && identity[0] == lease.RuntimeAPUUID {
				if identity[1] != lease.Plan.APInterface {
					return invalidInput("CarPlay 临时热点的活动接口已改变，拒绝清理")
				}
				if _, err := m.run(ctx, "nmcli", "--wait", "15", "connection", "down", "uuid", lease.RuntimeAPUUID); err != nil {
					return err
				}
			}
		}
		if _, err := m.run(ctx, "nmcli", "connection", "delete", "uuid", lease.RuntimeAPUUID); err != nil {
			return err
		}
	}
	upstreamOccupied := false
	// DNS 文件属于已持久化的租约 UUID；关闭热点后清理，再恢复上游并清除日志。
	if err := m.removeCarPlayWebAlias(ctx, lease); err != nil {
		return err
	}
	if err := m.removeCarPlayDNS(lease); err != nil {
		return err
	}
	if lease.DeviceAutoConnectManaged || lease.Plan.PauseUpstream {
		active, err := m.run(ctx, "nmcli", "-t", "-f", "UUID,DEVICE", "connection", "show", "--active")
		if err != nil {
			return err
		}
		for _, line := range strings.Split(strings.TrimSpace(string(active)), "\n") {
			fields := splitNMCLI(line)
			if len(fields) != 2 {
				continue
			}
			if fields[1] == lease.Plan.APInterface && fields[0] != lease.Plan.UpstreamUUID {
				return carPlayNetworkBusy()
			}
			upstreamOccupied = upstreamOccupied || fields[1] == lease.Plan.UpstreamInterface
		}
	}
	if lease.DeviceAutoConnectManaged {
		if _, err := m.run(ctx, "nmcli", "device", "set", lease.Plan.UpstreamInterface, "autoconnect", yesNo(lease.OriginalDeviceAutoConnect)); err != nil {
			return fmt.Errorf("恢复共享设备自动连接策略: %w", err)
		}
	}
	recoveryHandoff := false
	preference, priority, preferenceErr := m.carPlayNetworkPreference()
	if preferenceErr != nil {
		return preferenceErr
	}
	if validUUID(m.carPlayWiFiRecoveryUUID) {
		recoveryHandoff = priority != "carplay" && (preference == "auto" || preference == "wifi")
	}
	// 用户已选择蓝牙时不再强连旧热点，否则旧热点不可达会阻止释放和下一轮蓝牙接管。
	// 设备原自动连接策略仍按租约恢复，新租约负责重新暂停 STA，避免留下无人管理的禁用状态。
	if lease.Plan.PauseUpstream && !recoveryHandoff && preference != "bluetooth" {
		// 外部操作已经换了上游时不覆盖；原连接未就绪则保留日志，让到期回收重试。
		if !upstreamOccupied {
			if _, err := m.run(ctx, "nmcli", "--wait", "15", "connection", "up", "uuid", lease.Plan.UpstreamUUID, "ifname", lease.Plan.UpstreamInterface); err != nil {
				return fmt.Errorf("恢复 CarPlay 前的上游 WiFi: %w", err)
			}
		}
	}
	// 已安排恢复时交还统一 Wi-Fi 入口选定新目标，不能先强制恢复已离开范围的旧上游。
	if err := checkpoint(nil); err != nil {
		return err
	}
	m.carPlayNetworkHold = nil
	if value := m.carPlayEnrollmentHandoff; value != nil {
		enabled := true
		if m.carPlaySettingsPath != "" {
			store, err := carplaysettings.New(m.carPlaySettingsPath)
			if err != nil {
				return err
			}
			enabled = store.Settings().Enabled
		}
		if !value.Committed || !enabled {
			return m.restoreHandoffLocked(ctx)
		}
	}
	return nil
}
