package deviceadmin

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/godbus/dbus/v5"
	"github.com/wen-ryon/teslabox/internal/system"
)

const ethernetTrialTimeout = 120 * time.Second

type ethernetPending struct {
	public     EthernetTrial
	checkpoint dbus.ObjectPath
	profile    dbus.ObjectPath
	settings   *EthernetSettings
}

// EthernetManager 只管理普通有线口；事务回退由 NetworkManager 执行，不依赖本进程计时器。
type EthernetManager struct {
	mu      sync.Mutex
	runner  system.Runner
	open    func() (ethernetBus, error)
	pending *ethernetPending
	now     func() time.Time
}

// NewEthernetManager 延迟连接系统总线，未使用有线设置时不改变任何设备或配置。
func NewEthernetManager(runner system.Runner) *EthernetManager {
	return &EthernetManager{runner: runner, open: openEthernetBus, now: time.Now}
}

// EthernetStatus 读取真实设备状态和配置，能力查询失败只关闭相关硬件选项。
func (m *EthernetManager) EthernetStatus(ctx context.Context) (EthernetStatus, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	bus, err := m.open()
	if err != nil {
		return EthernetStatus{}, err
	}
	defer bus.close()
	if err := m.refreshPending(ctx, bus); err != nil {
		return EthernetStatus{}, err
	}
	records, err := ethernetInventory(ctx, bus)
	if err != nil {
		return EthernetStatus{}, err
	}
	status := EthernetStatus{Devices: []EthernetDevice{}}
	for _, record := range records {
		device := record.device
		m.readLinkCapabilities(ctx, &device)
		status.Devices = append(status.Devices, device)
	}
	if m.pending != nil {
		value := m.pending.public
		status.Pending = &value
	}
	return status, nil
}

// UpdateEthernet 对活动配置先试用再持久化，未激活配置允许单独保存且不自动抢占网口。
func (m *EthernetManager) UpdateEthernet(ctx context.Context, input EthernetUpdate) (*EthernetTrial, error) {
	if err := validateEthernetSettings(input.Settings); err != nil {
		return nil, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	bus, err := m.open()
	if err != nil {
		return nil, err
	}
	defer bus.close()
	record, err := m.editRecord(ctx, bus, input.Interface, input.UUID)
	if err != nil {
		return nil, err
	}
	var original ethernetConfig
	profile := record.profiles[input.UUID]
	if input.UUID != "" {
		original = record.configs[input.UUID]
		if !ethernetProfile(original).Editable {
			return nil, &InputError{Message: "当前配置包含未支持的企业或高级选项，不能覆盖"}
		}
	}
	active := input.UUID != "" && record.device.ConnectionUUID == input.UUID
	if input.Apply && (!active || record.device.StateCode != 100) {
		return nil, &InputError{Message: "请先连接并确认该有线配置，再应用修改；未连接的配置可以单独保存"}
	}
	if !input.Apply && active {
		return nil, &InputError{Message: "活动配置必须使用“保存并应用”，以便失联时自动回退"}
	}
	m.readLinkCapabilities(ctx, &record.device)
	if err := validateEthernetHardware(record.device, original, input.Settings); err != nil {
		return nil, err
	}
	uuid := input.UUID
	if uuid == "" {
		uuid, err = newEthernetUUID()
		if err != nil {
			return nil, err
		}
	}
	config := ethernetPatchConfig(original, input.Interface, uuid, input.Settings)
	if !input.Apply {
		if profile == "" {
			_, err = bus.call(ctx, dbus.ObjectPath(string(nmEthernetRoot)+"/Settings"), nmEthernetManager+".Settings.AddConnection2", config, uint32(0x1|0x20), map[string]dbus.Variant{})
		} else {
			_, err = bus.call(ctx, profile, nmEthernetSettings+".Update2", config, uint32(0x1|0x20|0x40), map[string]dbus.Variant{})
		}
		return nil, err
	}
	trialSettings := ethernetProfile(config).Settings
	trial, err := m.beginTrial(ctx, bus, record, uuid, "apply", profile, &trialSettings)
	if err != nil {
		return nil, err
	}
	if _, err = bus.call(ctx, profile, nmEthernetSettings+".UpdateUnsaved", config); err == nil {
		_, err = bus.call(ctx, nmEthernetRoot, nmEthernetManager+".ActivateConnection", profile, record.path, dbus.ObjectPath("/"))
	}
	if err != nil {
		return nil, errors.Join(err, m.rollbackAfterFailure(ctx, bus))
	}
	return trial, nil
}

// ActEthernet 使用同一回退保护执行连接或断开，拒绝对已经变化的连接做旧页面操作。
func (m *EthernetManager) ActEthernet(ctx context.Context, input EthernetAction) (*EthernetTrial, error) {
	if input.Action != "connect" && input.Action != "disconnect" {
		return nil, &InputError{Message: "有线操作只支持连接或断开"}
	}
	if !validUUID(input.UUID) {
		return nil, &InputError{Message: "有线连接 UUID 无效"}
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	bus, err := m.open()
	if err != nil {
		return nil, err
	}
	defer bus.close()
	record, err := m.editRecord(ctx, bus, input.Interface, input.UUID)
	if err != nil {
		return nil, err
	}
	if input.Action == "disconnect" && record.device.ConnectionUUID != input.UUID {
		return nil, &InputError{Message: "有线连接已变化，请刷新后再断开"}
	}
	profile := record.profiles[input.UUID]
	if !ethernetProfile(record.configs[input.UUID]).Editable {
		return nil, &InputError{Message: "该配置需使用系统网络管理工具连接"}
	}
	trial, err := m.beginTrial(ctx, bus, record, input.UUID, input.Action, profile, nil)
	if err != nil {
		return nil, err
	}
	if input.Action == "disconnect" {
		_, err = bus.call(ctx, record.path, nmEthernetDevice+".Disconnect")
	} else {
		_, err = bus.call(ctx, nmEthernetRoot, nmEthernetManager+".ActivateConnection", profile, record.path, dbus.ObjectPath("/"))
	}
	if err != nil {
		return nil, errors.Join(err, m.rollbackAfterFailure(ctx, bus))
	}
	return trial, nil
}

// FinishEthernet 在真实新连接可用后确认，或明确请求回退；不接受客户端指定 D-Bus 路径。
func (m *EthernetManager) FinishEthernet(ctx context.Context, token string, keep bool) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	bus, err := m.open()
	if err != nil {
		return err
	}
	defer bus.close()
	if err := m.refreshPending(ctx, bus); err != nil {
		return err
	}
	if m.pending == nil || token == "" || token != m.pending.public.Token {
		return &InputError{Message: "有线试用已过期或标识不匹配，请刷新状态"}
	}
	if !keep {
		return m.rollbackAfterFailure(ctx, bus)
	}
	pending := m.pending
	if m.now().Add(15 * time.Second).After(pending.public.ExpiresAt) {
		return &InputError{Message: "距离回退不足 15 秒，请等待恢复后重试"}
	}
	records, err := ethernetInventory(ctx, bus)
	if err != nil {
		return err
	}
	var record *ethernetRecord
	for i := range records {
		if records[i].device.Interface == pending.public.Interface {
			record = &records[i]
			break
		}
	}
	if record == nil {
		return &InputError{Message: "试用网口已不存在，不能确认"}
	}
	if pending.public.Action == "disconnect" {
		if record.device.ConnectionUUID != "" || record.device.StateCode > 30 {
			return &InputError{Message: "网口尚未断开或已被其他连接占用，不能确认"}
		}
	} else if record.device.StateCode != 100 || record.device.ConnectionUUID != pending.public.UUID {
		return &InputError{Message: "新有线连接尚未完成激活，不能确认"}
	}
	if pending.settings != nil {
		actual := ethernetProfile(record.configs[pending.public.UUID]).Settings
		if !ethernetSettingsEqual(actual, *pending.settings) {
			return &InputError{Message: "试用期间配置被其他操作修改，不能确认，请回退后重试"}
		}
		if _, err = bus.call(ctx, pending.profile, nmEthernetSettings+".Save"); err != nil {
			return errors.Join(err, m.rollbackAfterFailure(ctx, bus))
		}
	}
	if _, err = bus.call(ctx, nmEthernetRoot, nmEthernetManager+".CheckpointDestroy", pending.checkpoint); err != nil {
		return err
	}
	m.pending = nil
	return nil
}

func (m *EthernetManager) editRecord(ctx context.Context, bus ethernetBus, iface, uuid string) (ethernetRecord, error) {
	if !ethernetInterfaceValid(iface) || uuid != "" && !validUUID(uuid) {
		return ethernetRecord{}, &InputError{Message: "有线接口或 UUID 无效"}
	}
	if err := m.refreshPending(ctx, bus); err != nil {
		return ethernetRecord{}, err
	}
	if m.pending != nil {
		return ethernetRecord{}, &InputError{Message: "有线设置正在试用，请先确认或回退"}
	}
	records, err := ethernetInventory(ctx, bus)
	if err != nil {
		return ethernetRecord{}, err
	}
	for _, record := range records {
		if record.device.Interface != iface {
			continue
		}
		if !record.device.Managed {
			return ethernetRecord{}, &InputError{Message: "该网口未由 NetworkManager 管理，不能覆盖系统配置"}
		}
		if uuid != "" && record.profiles[uuid] == "" {
			return ethernetRecord{}, &InputError{Message: "配置不属于当前有线口或已变化，请刷新"}
		}
		for _, profile := range record.device.Profiles {
			if profile.UUID == uuid && !profile.Editable {
				return ethernetRecord{}, &InputError{Message: profile.Reason}
			}
		}
		return record, nil
	}
	return ethernetRecord{}, &InputError{Message: "未找到指定的普通以太网接口"}
}

func (m *EthernetManager) beginTrial(ctx context.Context, bus ethernetBus, record ethernetRecord, uuid, action string, profile dbus.ObjectPath, settings *EthernetSettings) (*EthernetTrial, error) {
	startedAt := m.now()
	body, err := bus.call(ctx, nmEthernetRoot, nmEthernetManager+".CheckpointCreate", []dbus.ObjectPath{record.path}, uint32(ethernetTrialTimeout/time.Second), uint32(0))
	if err != nil {
		return nil, errors.Join(&InputError{Message: "无法建立 NetworkManager 自动回退保护，本次未更改有线网络，请检查系统网络服务"}, err)
	}
	var checkpoint dbus.ObjectPath
	if err := dbus.Store(body, &checkpoint); err != nil || !checkpoint.IsValid() || checkpoint == "/" {
		return nil, fmt.Errorf("NetworkManager 回退标识无效")
	}
	value := EthernetTrial{Token: rand.Text(), Interface: record.device.Interface, UUID: uuid, Action: action, ExpiresAt: startedAt.Add(ethernetTrialTimeout)}
	m.pending = &ethernetPending{public: value, checkpoint: checkpoint, profile: profile, settings: settings}
	return &value, nil
}

func (m *EthernetManager) refreshPending(ctx context.Context, bus ethernetBus) error {
	if m.pending == nil {
		return nil
	}
	props, err := ethernetGetAll(ctx, bus, nmEthernetRoot, nmEthernetManager)
	if err != nil {
		return err
	}
	paths, ok := props["Checkpoints"].Value().([]dbus.ObjectPath)
	if !ok {
		return fmt.Errorf("无法核实有线回退保护状态")
	}
	for _, path := range paths {
		if path == m.pending.checkpoint {
			return nil
		}
	}
	m.pending = nil
	return nil
}

func (m *EthernetManager) rollbackAfterFailure(ctx context.Context, bus ethernetBus) error {
	if m.pending == nil {
		return nil
	}
	// 客户端断线也要尝试主动回退；若失败，NM 自己的 120 秒计时器仍保留。
	cleanup, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	body, err := bus.call(cleanup, nmEthernetRoot, nmEthernetManager+".CheckpointRollback", m.pending.checkpoint)
	if err != nil {
		return err
	}
	var results map[string]uint32
	if err := dbus.Store(body, &results); err != nil {
		return err
	}
	for _, code := range results {
		if code != 0 {
			m.pending = nil
			return fmt.Errorf("有线回退未完全成功（原因码 %d），请通过其他网络检查", code)
		}
	}
	// 兼容回退后仍保留检查点的 NM 版本，仅清理本次检查点，不碰其他管理器的事务。
	props, err := ethernetGetAll(cleanup, bus, nmEthernetRoot, nmEthernetManager)
	if err != nil {
		return err
	}
	paths, ok := props["Checkpoints"].Value().([]dbus.ObjectPath)
	if !ok {
		return fmt.Errorf("回退完成但无法核实检查点清理状态")
	}
	for _, path := range paths {
		if path == m.pending.checkpoint {
			if _, err := bus.call(cleanup, nmEthernetRoot, nmEthernetManager+".CheckpointDestroy", path); err != nil {
				return err
			}
			break
		}
	}
	m.pending = nil
	return nil
}

func ethernetSettingsEqual(a, b EthernetSettings) bool {
	// JSON 将空切片统一后比较，避免 NM 对空列表的规范化造成误判。
	for _, value := range []*EthernetSettings{&a, &b} {
		if len(value.IPv4.Addresses) == 0 {
			value.IPv4.Addresses = nil
		}
		if len(value.IPv4.DNS) == 0 {
			value.IPv4.DNS = nil
		}
		if len(value.IPv6.Addresses) == 0 {
			value.IPv6.Addresses = nil
		}
		if len(value.IPv6.DNS) == 0 {
			value.IPv6.DNS = nil
		}
	}
	left, _ := json.Marshal(a)
	right, _ := json.Marshal(b)
	return string(left) == string(right)
}

func newEthernetUUID() (string, error) {
	var value [16]byte
	if _, err := rand.Read(value[:]); err != nil {
		return "", err
	}
	value[6] = (value[6] & 0x0f) | 0x40
	value[8] = (value[8] & 0x3f) | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", value[:4], value[4:6], value[6:8], value[8:10], value[10:]), nil
}

var ethernetModePattern = regexp.MustCompile(`(\d+)base[^\s/]*/(Full|Half)`)

func (m *EthernetManager) readLinkCapabilities(ctx context.Context, device *EthernetDevice) {
	// 老 OTA 设备可能尚未安装 ethtool；实际双工仍可从内核读取，但不能据此推断可选链路能力。
	if device.Duplex == "" {
		device.Duplex = readEthernetDuplex(device.Interface)
	}
	if m.runner == nil || !ethernetInterfaceValid(device.Interface) {
		device.HardwareNotice = "无法读取硬件能力，已禁用速率协商和唤醒选项"
		return
	}
	query, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	output, err := m.runner.Run(query, "ethtool", device.Interface)
	if err != nil {
		device.HardwareNotice = "无法读取 ethtool 硬件能力，已保留现有链路设置"
		return
	}
	parseEthernetCapabilities(string(output), device)
}

func normalizeEthernetDuplex(value string) string {
	value = strings.ToLower(strings.TrimSpace(value))
	if value == "full" || value == "half" {
		return value
	}
	return ""
}

func parseEthernetCapabilities(output string, device *EthernetDevice) {
	section := ""
	seen := map[EthernetLinkMode]bool{}
	for _, line := range strings.Split(output, "\n") {
		text := strings.TrimSpace(line)
		if key, value, ok := strings.Cut(text, ":"); ok {
			section = key
			switch key {
			case "Duplex":
				if value = strings.ToLower(strings.TrimSpace(value)); value == "full" || value == "half" {
					device.Duplex = value
				}
			case "Supports auto-negotiation":
				if flag := strings.TrimSpace(value); flag == "Yes" || flag == "No" {
					supported := flag == "Yes"
					device.AutoNegotiate = &supported
				}
			case "Supports Wake-on":
				device.WakeOnLANModes = strings.TrimSpace(value)
			}
			text = value
		}
		if section == "Supported link modes" {
			for _, match := range ethernetModePattern.FindAllStringSubmatch(text, -1) {
				speed, err := strconv.ParseUint(match[1], 10, 32)
				if err != nil {
					continue
				}
				mode := EthernetLinkMode{Speed: uint32(speed), Duplex: strings.ToLower(match[2])}
				if !seen[mode] {
					device.LinkModes = append(device.LinkModes, mode)
					seen[mode] = true
				}
			}
		}
	}
}

func validateEthernetHardware(device EthernetDevice, original ethernetConfig, value EthernetSettings) error {
	old := ethernetProfile(original).Settings
	if original == nil {
		old = EthernetSettings{WakeOnLAN: 1}
	}
	if value.AutoNegotiate != old.AutoNegotiate || value.Speed != old.Speed || value.Duplex != old.Duplex {
		if device.AutoNegotiate == nil || len(device.LinkModes) == 0 {
			return &InputError{Message: "未能核实网卡链路能力，不能修改速率或协商模式"}
		}
		if value.AutoNegotiate && !*device.AutoNegotiate {
			return &InputError{Message: "网卡未报告自动协商支持"}
		}
		if value.Speed != 0 {
			found := false
			for _, mode := range device.LinkModes {
				found = found || mode.Speed == value.Speed && mode.Duplex == value.Duplex
			}
			if !found {
				return &InputError{Message: "所选速率／双工不在网卡支持列表内"}
			}
		}
	}
	if value.WakeOnLAN != old.WakeOnLAN {
		if device.WakeOnLANModes == "" {
			return &InputError{Message: "未能核实网卡唤醒能力，不能修改 Wake-on-LAN"}
		}
		for _, mode := range []struct {
			flag   uint32
			letter string
		}{{2, "p"}, {4, "u"}, {8, "m"}, {16, "b"}, {32, "a"}, {64, "g"}} {
			if value.WakeOnLAN&mode.flag != 0 && !strings.Contains(device.WakeOnLANModes, mode.letter) {
				return &InputError{Message: "网卡未报告支持所选 Wake-on-LAN 模式"}
			}
		}
	}
	return nil
}
