package deviceadmin

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"net"
	"regexp"
	"strings"
	"time"

	"github.com/godbus/dbus/v5"
)

var carPlayBluetoothAddressPattern = regexp.MustCompile(`^(?:[A-F0-9]{2}:){5}[A-F0-9]{2}$`)

// CarPlayUpstreamState 区分已有上游、蓝牙供网与仅建立链路，避免把连接成功当成互联网可用。
type CarPlayUpstreamState struct {
	Source          string `json:"source"`
	Interface       string `json:"interface,omitempty"`
	Internet        bool   `json:"internet"`
	APChannel       int    `json:"ap_channel,omitempty"`
	WiFiPaused      bool   `json:"wifi_paused,omitempty"`
	RecoveryUUID    string `json:"recovery_uuid,omitempty"`
	RecoveryChannel int    `json:"recovery_channel,omitempty"`
}

// ResetCarPlayPAN 仅释放当前租约的供网连接，让手机重新交接视频角色，保留热点与配对。
func (m *Manager) ResetCarPlayPAN(ctx context.Context, owner, phone string, checkpoint func(*CarPlayNetworkLease) error) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.carPlayRecoveryUncertain || m.carPlayNetworkHold == nil || m.carPlayNetworkHold.Owner != owner || !m.carPlayNetworkHold.ExpiresAt.After(time.Now()) || checkpoint == nil {
		return carPlayNetworkBusy()
	}
	address, err := net.ParseMAC(phone)
	if err != nil || len(address) != 6 {
		return invalidInput("蓝牙供网需要有效的手机地址")
	}
	lease := *m.carPlayNetworkHold
	if lease.RuntimePANAddress != "" && !strings.EqualFold(lease.RuntimePANAddress, phone) {
		return invalidInput("不能释放其他手机的蓝牙供网")
	}
	if lease.RuntimePANUUID == "" {
		return nil
	}
	// 先核对专属配置，不能因恢复视频而修改已被其他功能接管的连接。
	exists, err := m.carPlayPANExists(ctx, lease)
	if err != nil {
		return err
	}
	if lease.ForwardSource == "bluetooth" && lease.ForwardTable != 0 {
		if err := m.setCarPlayForwarding(ctx, &lease, "", checkpoint); err != nil {
			return err
		}
	}
	if exists {
		_, err = m.run(ctx, "nmcli", "connection", "delete", "uuid", lease.RuntimePANUUID)
	}
	// 保留恢复身份，下一次供网仍创建同一租约专属配置。
	return err
}

// EnsureCarPlayUpstream 在已有上游失效时协调同信道恢复与租约专属 PAN，保持媒体所有者控制 AP 生命周期。
func (m *Manager) EnsureCarPlayUpstream(ctx context.Context, owner, phone string, checkpoint func(*CarPlayNetworkLease) error) (result CarPlayUpstreamState, operationErr error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.carPlayRecoveryUncertain || m.carPlayNetworkHold == nil || m.carPlayNetworkHold.Owner != owner || !m.carPlayNetworkHold.ExpiresAt.After(time.Now()) || checkpoint == nil {
		return CarPlayUpstreamState{}, carPlayNetworkBusy()
	}
	address, err := net.ParseMAC(phone)
	if err != nil || len(address) != 6 {
		return CarPlayUpstreamState{}, invalidInput("蓝牙供网需要有效的手机地址")
	}
	lease := *m.carPlayNetworkHold
	preference, recoveryPriority, err := m.carPlayNetworkPreference()
	if err != nil {
		return CarPlayUpstreamState{}, err
	}
	if preference == "auto" && lease.ForwardTable != 0 {
		if err = m.removeCarPlayForwarding(ctx, lease); err != nil {
			return CarPlayUpstreamState{}, err
		}
		lease.ForwardTable = 0
		lease.ForwardInterface = ""
		lease.ForwardGateway = ""
		lease.ForwardSource = ""
		if err = checkpoint(&lease); err != nil {
			return CarPlayUpstreamState{}, err
		}
		copy := lease
		m.carPlayNetworkHold = &copy
	}
	if preference != "auto" && preference != "bluetooth" && lease.ForwardSource != preference {
		lease.ForwardSource = preference
		if lease.ForwardTable != 0 {
			if err = checkpoint(&lease); err != nil {
				return CarPlayUpstreamState{}, err
			}
			copy := lease
			m.carPlayNetworkHold = &copy
		}
		if err = m.setCarPlayForwarding(ctx, &lease, "", checkpoint); err != nil {
			return CarPlayUpstreamState{}, err
		}
	}
	phone = strings.ToUpper(phone)
	if lease.RuntimePANAddress != "" && lease.RuntimePANAddress != phone {
		return CarPlayUpstreamState{}, invalidInput("活动租约不能切换蓝牙供网手机")
	}
	active, err := m.run(ctx, "nmcli", "-t", "--escape", "yes", "-f", "UUID,TYPE,DEVICE", "connection", "show", "--active")
	if err != nil {
		return CarPlayUpstreamState{}, err
	}
	for _, line := range strings.Split(strings.TrimSpace(string(active)), "\n") {
		fields := splitNMCLI(line)
		if len(fields) != 3 || fields[0] == lease.RuntimeAPUUID || fields[2] == lease.Plan.APInterface || !carPlayInterfacePattern.MatchString(fields[2]) {
			continue
		}
		if fields[1] != "802-3-ethernet" && fields[1] != "802-11-wireless" {
			continue
		}
		if preference == "bluetooth" || preference == "ethernet" && fields[1] != "802-3-ethernet" || preference == "wifi" && fields[1] != "802-11-wireless" {
			continue
		}
		if m.carPlayConnectionInternet(ctx, fields[0], fields[2]) {
			if err := m.removeCarPlayPAN(ctx, lease); err != nil {
				return CarPlayUpstreamState{}, err
			}
			if preference != "auto" {
				if err = m.setCarPlayForwarding(ctx, &lease, fields[2], checkpoint); err != nil {
					return CarPlayUpstreamState{}, err
				}
			}
			m.carPlayWiFiRecoveryAfter = time.Time{}
			return CarPlayUpstreamState{Source: "existing_upstream", Interface: fields[2], Internet: true}, nil
		}
	}
	if recoveryPriority != "carplay" && (preference == "auto" || preference == "wifi") && !m.manualWiFiDisconnect &&
		lease.Plan.ConcurrencyMode == "same_channel" && time.Now().After(m.carPlayWiFiRecoveryAfter) {
		// 这里只报告恢复目标，媒体所有者结束会话、释放 AP 后才允许连接上游。
		status, statusErr := m.WiFiStatus(ctx)
		if statusErr == nil && status.Interface == lease.Plan.UpstreamInterface {
			// 有效上游在前面已返回；断线时刷新一次，不能拿离家前的扫描缓存反复连接旧网络。
			points, pointsErr := m.cachedAccessPoints(ctx, status.Interface, nil)
			if status.CurrentConnection == "" || pointsErr != nil {
				m.carPlayWiFiRecoveryAfter = time.Now().Add(30 * time.Second)
				if _, scanErr := m.run(ctx, "nmcli", "--wait", "12", "device", "wifi", "rescan", "ifname", status.Interface); scanErr == nil {
					points, pointsErr = m.cachedAccessPoints(ctx, status.Interface, nil)
				} else {
					// 新扫描失败不能据旧缓存重建 AP，保留本地画面待下一轮恢复。
					pointsErr = scanErr
				}
			}
			ordered := make([]SavedNetwork, 0, len(status.SavedNetworks))
			if pointsErr == nil {
				for _, candidate := range m.autoConnectCandidates(ctx, status.SavedNetworks) {
					if until := m.carPlayWiFiFailures[candidate.network.UUID]; time.Now().Before(until) {
						continue
					}
					delete(m.carPlayWiFiFailures, candidate.network.UUID)
					ordered = append(ordered, candidate.network)
				}
			}
			// 续租请求携带车机允许的信道，不能选择手机拒绝用于 CarPlay 的候选。
			if len(m.carPlayVehicleChannels) > 0 {
				compatible := make([]AccessPoint, 0, len(points))
				for _, point := range points {
					for _, channel := range m.carPlayVehicleChannels {
						if point.Channel == channel {
							compatible = append(compatible, point)
							break
						}
					}
				}
				points = compatible
			}
			target, channel := carPlayWiFiRecoveryTarget(lease, ordered, points)
			if pointsErr == nil && target != "" {
				m.carPlayWiFiRecoveryAfter = time.Now().Add(3 * time.Minute)
				m.carPlayWiFiRecoveryUUID = target
				return CarPlayUpstreamState{Source: "wifi_recovery_required", Interface: status.Interface,
					RecoveryUUID: target, RecoveryChannel: channel}, nil
			}
		}
	}
	if preference == "wifi" || preference == "ethernet" {
		if err = m.setCarPlayForwarding(ctx, &lease, "", checkpoint); err != nil {
			return CarPlayUpstreamState{}, err
		}
		if err = m.removeCarPlayPAN(ctx, lease); err != nil {
			return CarPlayUpstreamState{}, err
		}
		return CarPlayUpstreamState{Source: preference, Internet: false}, nil
	}
	if lease.RuntimePANUUID == "" {
		if err := ctx.Err(); err != nil {
			return CarPlayUpstreamState{}, err
		}
		var identity [16]byte
		if _, err := rand.Read(identity[:]); err != nil {
			return CarPlayUpstreamState{}, err
		}
		identity[6], identity[8] = identity[6]&15|64, identity[8]&63|128
		lease.RuntimePANUUID = fmt.Sprintf("%x-%x-%x-%x-%x", identity[:4], identity[4:6], identity[6:8], identity[8:10], identity[10:])
		lease.RuntimePANAddress = phone
		// 先持久化唯一身份，即使创建或激活中途退出也能安全回收。
		if err := checkpoint(&lease); err != nil {
			return CarPlayUpstreamState{}, err
		}
		m.carPlayNetworkHold = &lease
	}
	exists, err := m.carPlayPANExists(ctx, lease)
	if err != nil {
		return CarPlayUpstreamState{}, err
	}
	if !exists {
		if err := m.carPlayPANProfileCreate(ctx, lease.RuntimePANUUID, address); err != nil {
			return CarPlayUpstreamState{}, err
		}
	}
	// 已有配置必须仍指向同一手机，不复用被外部修改的蓝牙连接。
	identity, err := m.run(ctx, "nmcli", "--escape", "no", "-g", "bluetooth.bdaddr,bluetooth.type", "connection", "show", "uuid", lease.RuntimePANUUID)
	if err != nil || !strings.EqualFold(strings.TrimSpace(string(identity)), phone+"\npanu") {
		return CarPlayUpstreamState{}, invalidInput("CarPlay 蓝牙供网的手机身份已改变")
	}
	linkEstablished := false
	defer func() {
		if !linkEstablished {
			cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
			defer cancel()
			if preference == "bluetooth" && lease.Plan.DisableWiFiUpstream {
				operationErr = errors.Join(operationErr, m.setCarPlayForwarding(cleanupCtx, &lease, "", checkpoint))
			}
			operationErr = errors.Join(operationErr, m.removeCarPlayPAN(cleanupCtx, lease))
		}
	}()
	device, err := m.run(ctx, "nmcli", "-g", "GENERAL.DEVICES", "connection", "show", "uuid", lease.RuntimePANUUID)
	if err != nil {
		return CarPlayUpstreamState{}, err
	}
	if strings.TrimSpace(string(device)) == "" || strings.TrimSpace(string(device)) == "--" {
		if _, err := m.run(ctx, "nmcli", "--wait", "20", "connection", "up", "uuid", lease.RuntimePANUUID); err != nil {
			return CarPlayUpstreamState{}, errors.New("蓝牙供网未建立，请检查手机个人热点与 NAP 服务")
		}
		device, err = m.run(ctx, "nmcli", "-g", "GENERAL.DEVICES", "connection", "show", "uuid", lease.RuntimePANUUID)
		if err != nil {
			return CarPlayUpstreamState{}, err
		}
	}
	interfaceName, err := m.carPlayPANInterface(ctx, lease.RuntimePANUUID)
	if err != nil {
		return CarPlayUpstreamState{}, err
	}
	// udev 可将 bnep0 重命名为 enx...；归属由活动蓝牙连接 UUID 验证，不能按名称前缀误删供网。
	if !carPlayInterfacePattern.MatchString(interfaceName) || interfaceName == lease.Plan.APInterface || interfaceName == lease.Plan.UpstreamInterface {
		return CarPlayUpstreamState{}, invalidInput("蓝牙供网未返回有效的 BNEP 接口")
	}
	internet := m.carPlayConnectionInternet(ctx, lease.RuntimePANUUID, interfaceName)
	// 验证失败保留已有上游；供网建立后调整 AP，不能释放包含 PAN 的整个租约。
	linkEstablished = true
	if preference == "bluetooth" && internet && !lease.Plan.DisableWiFiUpstream {
		if err = m.promoteCarPlayBluetoothLocked(ctx, &lease, checkpoint); err != nil {
			return CarPlayUpstreamState{}, err
		}
	}
	if preference == "bluetooth" && lease.Plan.DisableWiFiUpstream {
		lease.ForwardSource = preference
		device := ""
		if internet {
			device = interfaceName
		}
		if err = m.setCarPlayForwarding(ctx, &lease, device, checkpoint); err != nil {
			return CarPlayUpstreamState{}, err
		}
	}
	// 蜂窝或 DNS 短暂失败不代表蓝牙链路失效，保留接口供下一轮探测恢复。
	linkEstablished = true
	return CarPlayUpstreamState{Source: "bluetooth", Interface: interfaceName, Internet: internet, APChannel: lease.Plan.Channel, WiFiPaused: lease.Plan.DisableWiFiUpstream}, nil
}

func carPlayWiFiRecoveryTarget(lease CarPlayNetworkLease, profiles []SavedNetwork, points []AccessPoint) (string, int) {
	// 调用方按项目已有的自动连接优先级排序，不能永久锁定最初的上游 UUID。
	for _, profile := range profiles {
		if !validUUID(profile.UUID) || !profile.AutoConnect || profile.RequiresPassword || profile.SSID == "" {
			continue
		}
		channel, signal := 0, -1
		for _, point := range points {
			if point.SSID == profile.SSID && validCarPlayChannel(point.Channel) &&
				!(point.Channel == lease.Plan.Channel && point.Active) && point.Signal > signal {
				channel, signal = point.Channel, point.Signal
			}
		}
		if channel != 0 {
			return profile.UUID, channel
		}
	}
	return "", 0
}

func (m *Manager) carPlayConnectionInternet(ctx context.Context, identity, interfaceName string) bool {
	value, err := m.run(ctx, "nmcli", "-g", "IP4.DNS", "connection", "show", "uuid", identity)
	if err != nil {
		return false
	}
	var servers []string
	// nmcli 的数组字段可用竖线分隔，不能把多个 DNS 当成单个 IP 而误封锁上游。
	for _, line := range strings.FieldsFunc(string(value), func(r rune) bool {
		return r == '|' || r == '\n' || r == '\r'
	}) {
		if ip := net.ParseIP(strings.TrimSpace(line)); ip != nil && ip.To4() != nil {
			servers = append(servers, ip.String())
		}
		if len(servers) == 2 {
			break
		}
	}
	return len(servers) > 0 && m.carPlayInternetProbe(ctx, interfaceName, servers)
}

func carPlayPANName(lease CarPlayNetworkLease) string {
	return "TeslaBox-CarPlay-PAN-" + lease.RuntimePANUUID
}

func (m *Manager) carPlayPANExists(ctx context.Context, lease CarPlayNetworkLease) (bool, error) {
	profiles, err := m.run(ctx, "nmcli", "-t", "--escape", "yes", "-f", "UUID,NAME", "connection", "show")
	if err != nil {
		return false, err
	}
	for _, line := range strings.Split(strings.TrimSpace(string(profiles)), "\n") {
		fields := splitNMCLI(line)
		if len(fields) != 2 || fields[0] != lease.RuntimePANUUID {
			continue
		}
		if fields[1] != carPlayPANName(lease) {
			return false, invalidInput("蓝牙供网恢复身份已改变，拒绝修改")
		}
		identity, err := m.run(ctx, "nmcli", "--escape", "no", "-g", "connection.type,bluetooth.type,bluetooth.bdaddr", "connection", "show", "uuid", lease.RuntimePANUUID)
		if err != nil || !strings.EqualFold(strings.TrimSpace(string(identity)), "bluetooth\npanu\n"+lease.RuntimePANAddress) {
			return false, invalidInput("蓝牙供网恢复配置类型已改变")
		}
		return true, nil
	}
	return false, nil
}

func activeCarPlayPANInterface(ctx context.Context, identity string) (string, error) {
	conn, err := dbus.ConnectSystemBus()
	if err != nil {
		return "", errors.New("无法读取蓝牙供网接口")
	}
	defer conn.Close()
	var value dbus.Variant
	root := conn.Object(networkManagerBusName, dbus.ObjectPath("/org/freedesktop/NetworkManager"))
	if err := root.CallWithContext(ctx, "org.freedesktop.DBus.Properties.Get", 0, networkManagerBusName, "ActiveConnections").Store(&value); err != nil {
		return "", err
	}
	var paths []dbus.ObjectPath
	if err := dbus.Store([]any{value.Value()}, &paths); err != nil {
		return "", err
	}
	for _, path := range paths {
		object := conn.Object(networkManagerBusName, path)
		var properties map[string]dbus.Variant
		if err := object.CallWithContext(ctx, "org.freedesktop.DBus.Properties.GetAll", 0, networkManagerBusName+".Connection.Active").Store(&properties); err != nil {
			return "", err
		}
		if properties["Uuid"].Value() != identity {
			continue
		}
		if properties["Type"].Value() != "bluetooth" {
			return "", errors.New("活动连接不属于蓝牙供网")
		}
		var devices []dbus.ObjectPath
		if err := dbus.Store([]any{properties["Devices"].Value()}, &devices); err != nil {
			return "", err
		}
		if len(devices) != 1 {
			return "", errors.New("蓝牙供网活动设备数量无效")
		}
		if err := conn.Object(networkManagerBusName, devices[0]).CallWithContext(ctx, "org.freedesktop.DBus.Properties.Get", 0, networkManagerBusName+".Device", "IpInterface").Store(&value); err != nil {
			return "", err
		}
		name, ok := value.Value().(string)
		if !ok {
			return "", errors.New("蓝牙供网 IP 接口类型无效")
		}
		return name, nil
	}
	return "", errors.New("蓝牙供网没有活动连接")
}

func (m *Manager) removeCarPlayPAN(ctx context.Context, lease CarPlayNetworkLease) error {
	if lease.RuntimePANUUID == "" {
		return nil
	}
	exists, err := m.carPlayPANExists(ctx, lease)
	if err != nil || !exists {
		return err
	}
	// 删除专属 NM 配置会停用它自己的 PAN；不能 Disconnect 整个手机蓝牙设备。
	_, err = m.run(ctx, "nmcli", "connection", "delete", "uuid", lease.RuntimePANUUID)
	return err
}

func createCarPlayPANProfile(ctx context.Context, identity string, address []byte) error {
	conn, err := dbus.ConnectSystemBus()
	if err != nil {
		return errors.New("蓝牙供网无法连接系统 D-Bus")
	}
	defer conn.Close()
	settings := carPlayNMSettings{
		"connection": {"id": dbus.MakeVariant("TeslaBox-CarPlay-PAN-" + identity), "uuid": dbus.MakeVariant(identity), "type": dbus.MakeVariant("bluetooth"), "autoconnect": dbus.MakeVariant(false)},
		"bluetooth":  {"type": dbus.MakeVariant("panu"), "bdaddr": dbus.MakeVariant(address)},
		"ipv4":       {"method": dbus.MakeVariant("auto"), "route-metric": dbus.MakeVariant(int64(1)), "dns-priority": dbus.MakeVariant(int32(10)), "dhcp-timeout": dbus.MakeVariant(int32(15))},
		"ipv6":       {"method": dbus.MakeVariant("disabled")},
	}
	object := conn.Object(networkManagerBusName, dbus.ObjectPath("/org/freedesktop/NetworkManager/Settings"))
	if err := object.CallWithContext(ctx, "org.freedesktop.NetworkManager.Settings.AddConnection2", 0, settings, uint32(0x2|0x20), map[string]dbus.Variant{}).Err; err != nil {
		return errors.New("NetworkManager 无法创建 CarPlay 蓝牙供网配置")
	}
	return nil
}
