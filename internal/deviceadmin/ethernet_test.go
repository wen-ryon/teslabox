package deviceadmin

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/godbus/dbus/v5"
)

const ethernetTestUUID = "a1111111-1111-4111-8111-111111111111"
const ethernetTestDevice = dbus.ObjectPath("/org/freedesktop/NetworkManager/Devices/1")
const ethernetTestProfile = dbus.ObjectPath("/org/freedesktop/NetworkManager/Settings/1")
const ethernetTestCheckpoint = dbus.ObjectPath("/org/freedesktop/NetworkManager/Checkpoint/1")

func TestEthernetDuplexDoesNotInventCapabilities(t *testing.T) {
	for input, want := range map[string]string{"full\n": "full", "Half\n": "half", "unknown": "", "": "", "1000": "", "full extra": ""} {
		if got := normalizeEthernetDuplex(input); got != want {
			t.Fatalf("双工解析 %q: got %q want %q", input, got, want)
		}
	}
	for _, iface := range []string{".", "..", "../eth0", "/eth0", "-x"} {
		if ethernetInterfaceValid(iface) {
			t.Fatalf("接受了不合法接口名 %q", iface)
		}
	}
}

type ethernetTestCall struct {
	path   dbus.ObjectPath
	method string
	args   []any
}
type ethernetTestBus struct {
	calls         []ethernetTestCall
	state         uint32
	active        bool
	checkpoint    bool
	config        ethernetConfig
	old           ethernetConfig
	fail          string
	rollbackCode  uint32
	second        bool
	noAvailable   bool
	extraProfiles map[dbus.ObjectPath]ethernetConfig
}

func (b *ethernetTestBus) close() {}
func ethernetTestProperties(values map[string]any) ethernetProperties {
	result := ethernetProperties{}
	for key, value := range values {
		result[key] = dbus.MakeVariant(value)
	}
	return result
}

func ethernetTestSettings() EthernetSettings {
	return EthernetSettings{Name: "有线连接", AutoConnect: true, WakeOnLAN: 1, IPv4: EthernetIPSettings{Method: "auto", RouteMetric: -1}, IPv6: EthernetIPSettings{Method: "auto", RouteMetric: -1}}
}

func newEthernetTestManager() (*EthernetManager, *ethernetTestBus) {
	b := &ethernetTestBus{state: 100, active: true, config: ethernetPatchConfig(nil, "eth0", ethernetTestUUID, ethernetTestSettings())}
	m := NewEthernetManager(nil)
	m.open = func() (ethernetBus, error) { return b, nil }
	return m, b
}

func (b *ethernetTestBus) call(ctx context.Context, path dbus.ObjectPath, method string, args ...any) ([]any, error) {
	b.calls = append(b.calls, ethernetTestCall{path, method, args})
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if strings.HasSuffix(method, b.fail) && b.fail != "" {
		return nil, errors.New("注入失败")
	}
	switch {
	case strings.HasSuffix(method, ".ListConnections"):
		paths := []dbus.ObjectPath{ethernetTestProfile}
		for path := range b.extraProfiles {
			paths = append(paths, path)
		}
		return []any{paths}, nil
	case strings.HasSuffix(method, ".GetDevices"):
		paths := []dbus.ObjectPath{ethernetTestDevice, "/wifi"}
		if b.second {
			paths = append(paths, "/ether2")
		}
		return []any{paths}, nil
	case strings.HasSuffix(method, ".GetAll"):
		iface := args[0].(string)
		props := ethernetProperties{}
		switch {
		case path == "/wifi":
			props = ethernetTestProperties(map[string]any{"DeviceType": uint32(2)})
		case path == nmEthernetRoot:
			paths := []dbus.ObjectPath{}
			if b.checkpoint {
				paths = append(paths, ethernetTestCheckpoint)
			}
			props = ethernetTestProperties(map[string]any{"Checkpoints": paths})
		case iface == nmEthernetDevice:
			active := dbus.ObjectPath("/")
			if b.active {
				active = "/active"
			}
			props = ethernetTestProperties(map[string]any{"DeviceType": uint32(1), "Interface": "eth0", "Managed": true, "State": b.state, "ActiveConnection": active, "AvailableConnections": []dbus.ObjectPath{ethernetTestProfile}, "Ip4Config": dbus.ObjectPath("/"), "Ip6Config": dbus.ObjectPath("/")})
			if b.noAvailable {
				props["AvailableConnections"] = dbus.MakeVariant([]dbus.ObjectPath{})
			}
			if path == "/ether2" {
				props["Interface"] = dbus.MakeVariant("eth1")
				props["ActiveConnection"] = dbus.MakeVariant(dbus.ObjectPath("/"))
				props["State"] = dbus.MakeVariant(uint32(30))
			}
		case iface == nmEthernetDevice+".Wired":
			props = ethernetTestProperties(map[string]any{"Carrier": true, "Speed": uint32(1000), "HwAddress": "AA:BB:CC:DD:EE:01", "PermHwAddress": "AA:BB:CC:DD:EE:01"})
		case iface == nmEthernetManager+".Connection.Active":
			props = ethernetTestProperties(map[string]any{"Uuid": ethernetTestUUID, "Connection": ethernetTestProfile})
		default:
			return nil, errors.New("未定义的属性读取")
		}
		return []any{props}, nil
	case strings.HasSuffix(method, ".GetSettings"):
		if config, exists := b.extraProfiles[path]; exists {
			return []any{config}, nil
		}
		return []any{b.config}, nil
	case strings.HasSuffix(method, ".CheckpointCreate"):
		if b.checkpoint {
			return nil, errors.New("已有检查点")
		}
		if args[1] != uint32(120) || args[2] != uint32(0) {
			return nil, errors.New("回退参数错误，不能删除其他新增连接")
		}
		b.checkpoint = true
		b.old = b.config
		return []any{ethernetTestCheckpoint}, nil
	case strings.HasSuffix(method, ".UpdateUnsaved"):
		b.config = args[0].(ethernetConfig)
		return nil, nil
	case strings.HasSuffix(method, ".ActivateConnection"):
		b.active = true
		b.state = 40
		return []any{dbus.ObjectPath("/active")}, nil
	case strings.HasSuffix(method, ".Disconnect"):
		b.active = false
		b.state = 30
		return nil, nil
	case strings.HasSuffix(method, ".Save"):
		return nil, nil
	case strings.HasSuffix(method, ".CheckpointDestroy"):
		b.checkpoint = false
		return nil, nil
	case strings.HasSuffix(method, ".CheckpointRollback"):
		b.checkpoint = false
		b.config = b.old
		b.active = true
		b.state = 100
		return []any{map[string]uint32{string(ethernetTestDevice): b.rollbackCode}}, nil
	case strings.HasSuffix(method, ".Update2"):
		b.config = args[0].(ethernetConfig)
		return []any{map[string]dbus.Variant{}}, nil
	default:
		return nil, errors.New("未定义的调用: " + method)
	}
}

func TestEthernetStatusDoesNotClaimActivationIsConnected(t *testing.T) {
	m, b := newEthernetTestManager()
	b.state = 70
	status, err := m.EthernetStatus(context.Background())
	if err != nil || len(status.Devices) != 1 || status.Devices[0].State != "connecting" || status.Devices[0].Profiles[0].Active {
		t.Fatalf("把获取地址误报已连接: %+v %v", status, err)
	}
	if status.Devices[0].HardwareNotice == "" || len(status.Devices[0].LinkModes) != 0 {
		t.Fatal("没有 ethtool 却声称已知硬件能力")
	}
}

func TestEthernetUnpluggedPortKeepsSavedProfilesEditableWithoutActivation(t *testing.T) {
	m, b := newEthernetTestManager()
	b.state, b.active, b.noAvailable = 20, false, true
	status, err := m.EthernetStatus(t.Context())
	if err != nil || len(status.Devices) != 1 {
		t.Fatalf("拔线状态读取失败: %+v %v", status, err)
	}
	device := status.Devices[0]
	if device.State != "unavailable" || len(device.Profiles) != 1 || device.Profiles[0].Active || !device.Profiles[0].Editable {
		t.Fatalf("把未插线当成没有已保存配置: %+v", device)
	}
	settings := device.Profiles[0].Settings
	settings.Name = "拔线时修改的配置"
	trial, err := m.UpdateEthernet(t.Context(), EthernetUpdate{Interface: "eth0", UUID: ethernetTestUUID, Settings: settings})
	if err != nil || trial != nil {
		t.Fatalf("不能单独保存拔线网口配置: %+v %v", trial, err)
	}
	for _, call := range b.calls {
		if strings.HasSuffix(call.method, ".ActivateConnection") || strings.HasSuffix(call.method, ".CheckpointCreate") {
			t.Fatal("单独保存配置意外激活了网口")
		}
	}
}

func TestEthernetSavedProfileDiscoveryDoesNotLeakOtherInterfacesOrWiFi(t *testing.T) {
	m, b := newEthernetTestManager()
	b.active, b.noAvailable = false, true
	foreign := ethernetPatchConfig(nil, "eth9", "b1111111-1111-4111-8111-111111111111", ethernetTestSettings())
	foreignMAC := ethernetPatchConfig(nil, "", "c1111111-1111-4111-8111-111111111111", ethernetTestSettings())
	foreignMAC["802-3-ethernet"]["mac-address"] = dbus.MakeVariant([]byte{0, 1, 2, 3, 4, 5})
	wifi := ethernetPatchConfig(nil, "", "d1111111-1111-4111-8111-111111111111", ethernetTestSettings())
	wifi["connection"]["type"] = dbus.MakeVariant("802-11-wireless")
	b.extraProfiles = map[dbus.ObjectPath]ethernetConfig{"/foreign": foreign, "/foreign_mac": foreignMAC, "/wifi_profile": wifi}
	status, err := m.EthernetStatus(t.Context())
	if err != nil || len(status.Devices) != 1 || len(status.Devices[0].Profiles) != 1 || status.Devices[0].Profiles[0].UUID != ethernetTestUUID {
		t.Fatalf("已保存配置越过接口／MAC／类型边界: %+v %v", status, err)
	}
}

func TestEthernetSavedProfileMACBindingRequiresKnownIdentity(t *testing.T) {
	config := ethernetPatchConfig(nil, "", ethernetTestUUID, ethernetTestSettings())
	device := EthernetDevice{Interface: "eth0", MAC: "02:00:00:00:00:01", PermanentMAC: "AA:BB:CC:DD:EE:01"}
	for _, binding := range []any{[]byte{0xaa, 0xbb, 0xcc, 0xdd, 0xee, 1}, "aa:bb:cc:dd:ee:01", []byte{}} {
		config["802-3-ethernet"]["mac-address"] = dbus.MakeVariant(binding)
		if !ethernetSavedProfileMatches(config, device) {
			t.Fatalf("有效 MAC 绑定没有匹配: %v", binding)
		}
	}
	for _, binding := range []any{[]byte{0, 1}, "not-a-mac", uint32(1)} {
		config["802-3-ethernet"]["mac-address"] = dbus.MakeVariant(binding)
		if ethernetSavedProfileMatches(config, device) {
			t.Fatalf("不明确 MAC 绑定被放行: %v", binding)
		}
	}
	config["802-3-ethernet"]["mac-address"] = dbus.MakeVariant("aa:bb:cc:dd:ee:01")
	if ethernetSavedProfileMatches(config, EthernetDevice{Interface: "eth0"}) {
		t.Fatal("硬件身份未知时猜测了配置归属")
	}
}

func TestEthernetActiveUpdateIsUnsavedUntilConfirmed(t *testing.T) {
	m, b := newEthernetTestManager()
	settings := ethernetTestSettings()
	settings.IPv4 = EthernetIPSettings{Method: "manual", Addresses: []string{"192.0.2.15/24"}, Gateway: "192.0.2.1", DNS: []string{"223.5.5.5"}, RouteMetric: 50}
	trial, err := m.UpdateEthernet(context.Background(), EthernetUpdate{Interface: "eth0", UUID: ethernetTestUUID, Apply: true, Settings: settings})
	if err != nil || trial == nil || !b.checkpoint {
		t.Fatalf("试用失败: %+v %v", trial, err)
	}
	for _, call := range b.calls {
		if strings.HasSuffix(call.method, ".Save") || strings.HasSuffix(call.method, ".Update2") {
			t.Fatal("确认前已写入磁盘")
		}
	}
	if err := m.FinishEthernet(context.Background(), trial.Token, true); err == nil {
		t.Fatal("获取 IP 时允许确认")
	}
	b.state = 100
	if err := m.FinishEthernet(context.Background(), trial.Token, true); err != nil {
		t.Fatal(err)
	}
	if b.checkpoint || m.pending != nil {
		t.Fatal("确认后仍占用检查点")
	}
	last := b.calls[len(b.calls)-1]
	if !strings.HasSuffix(last.method, ".CheckpointDestroy") {
		t.Fatalf("未先保存再取消回退: %+v", last)
	}
}

func TestEthernetFailureAndCancelRollback(t *testing.T) {
	for _, failure := range []string{".UpdateUnsaved", ".ActivateConnection"} {
		t.Run(failure, func(t *testing.T) {
			m, b := newEthernetTestManager()
			b.fail = failure
			settings := ethernetTestSettings()
			settings.MTU = 1400
			if _, err := m.UpdateEthernet(context.Background(), EthernetUpdate{Interface: "eth0", UUID: ethernetTestUUID, Apply: true, Settings: settings}); err == nil {
				t.Fatal("未返回失败")
			}
			if m.pending != nil || b.checkpoint || ethernetProfile(b.config).Settings.MTU != 0 {
				t.Fatal("失败后没有回退")
			}
		})
	}
	m, b := newEthernetTestManager()
	trial, err := m.ActEthernet(context.Background(), EthernetAction{Interface: "eth0", UUID: ethernetTestUUID, Action: "disconnect"})
	if err != nil {
		t.Fatal(err)
	}
	if err := m.FinishEthernet(context.Background(), "wrong-token", true); err == nil {
		t.Fatal("其他请求可确认")
	}
	if err := m.FinishEthernet(context.Background(), trial.Token, false); err != nil {
		t.Fatal(err)
	}
	if !b.active || m.pending != nil {
		t.Fatal("主动回退失败")
	}
}

func TestEthernetCheckpointFailureDoesNotMutate(t *testing.T) {
	m, b := newEthernetTestManager()
	b.fail = ".CheckpointCreate"
	if _, err := m.ActEthernet(context.Background(), EthernetAction{Interface: "eth0", UUID: ethernetTestUUID, Action: "disconnect"}); err == nil {
		t.Fatal("没有回退保护仍执行")
	}
	for _, call := range b.calls {
		if strings.HasSuffix(call.method, ".Disconnect") {
			t.Fatal("保护失败后改变了网络")
		}
	}
}

func TestEthernetRejectsStaleAndUnsupportedRequests(t *testing.T) {
	for _, input := range []EthernetUpdate{
		{Interface: "wlan0", UUID: ethernetTestUUID, Apply: true, Settings: ethernetTestSettings()},
		{Interface: "eth0", UUID: "b1111111-1111-4111-8111-111111111111", Apply: true, Settings: ethernetTestSettings()},
		{Interface: "eth0", UUID: ethernetTestUUID, Apply: false, Settings: ethernetTestSettings()},
	} {
		m, b := newEthernetTestManager()
		if _, err := m.UpdateEthernet(context.Background(), input); err == nil {
			t.Fatalf("无效身份未拒绝: %+v", input)
		}
		if b.checkpoint {
			t.Fatal("无效输入启动了事务")
		}
	}
	m, b := newEthernetTestManager()
	b.config["802-1x"] = ethernetTestProperties(map[string]any{"eap": []string{"tls"}})
	if _, err := m.ActEthernet(context.Background(), EthernetAction{Interface: "eth0", UUID: ethernetTestUUID, Action: "connect"}); err == nil {
		t.Fatal("覆盖企业认证")
	}
}

func TestEthernetExpirySurvivesProcessRestartWithoutCommit(t *testing.T) {
	m, b := newEthernetTestManager()
	trial, err := m.ActEthernet(context.Background(), EthernetAction{Interface: "eth0", UUID: ethernetTestUUID, Action: "disconnect"})
	if err != nil {
		t.Fatal(err)
	}
	m2 := NewEthernetManager(nil)
	m2.open = func() (ethernetBus, error) { return b, nil }
	if _, err := m2.ActEthernet(context.Background(), EthernetAction{Interface: "eth0", UUID: ethernetTestUUID, Action: "connect"}); err == nil {
		t.Fatal("重启后覆盖原检查点")
	}
	// 模拟 NM 自己超时回退；应用重启不需要负责触发它。
	b.checkpoint = false
	b.active = true
	b.state = 100
	if err := m.FinishEthernet(context.Background(), trial.Token, true); err == nil {
		t.Fatal("过期事务仍可确认")
	}
	if m.pending != nil {
		t.Fatal("过期状态未清理")
	}
}

func TestEthernetNearExpiryAndForeignMutationCannotConfirm(t *testing.T) {
	m, b := newEthernetTestManager()
	now := time.Now()
	m.now = func() time.Time { return now }
	trial, err := m.UpdateEthernet(context.Background(), EthernetUpdate{Interface: "eth0", UUID: ethernetTestUUID, Apply: true, Settings: ethernetTestSettings()})
	if err != nil {
		t.Fatal(err)
	}
	b.state = 100
	b.config["connection"]["id"] = dbus.MakeVariant("被其他工具修改")
	if err := m.FinishEthernet(context.Background(), trial.Token, true); err == nil {
		t.Fatal("接受了外部变化")
	}
	now = trial.ExpiresAt.Add(-time.Second)
	if err := m.FinishEthernet(context.Background(), trial.Token, true); err == nil {
		t.Fatal("接受临界超时")
	}
}

func TestEthernetIPValidationAndRoundTrip(t *testing.T) {
	valid := ethernetTestSettings()
	valid.IPv4 = EthernetIPSettings{Method: "manual", Addresses: []string{"192.168.3.10/24", "192.168.4.10/24"}, Gateway: "192.168.3.1", DNS: []string{"8.8.4.4"}, RouteMetric: 0}
	valid.IPv6 = EthernetIPSettings{Method: "manual", Addresses: []string{"2001:db8::2/64"}, Gateway: "fe80::1", DNS: []string{"2001:4860:4860::8888"}, RouteMetric: 4294967295}
	valid.MTU = 1500
	if err := validateEthernetSettings(valid); err != nil {
		t.Fatal(err)
	}
	config := ethernetPatchConfig(nil, "eth0", ethernetTestUUID, valid)
	if actual := ethernetProfile(config).Settings; !ethernetSettingsEqual(actual, valid) {
		t.Fatalf("D-Bus 地址／DNS 往返变化: %+v", actual)
	}
	for _, mutate := range []func(*EthernetSettings){
		func(s *EthernetSettings) { s.MTU = 1000 }, func(s *EthernetSettings) { s.IPv4.RouteMetric = -2 },
		func(s *EthernetSettings) { s.IPv4.DNS = []string{"::1"} }, func(s *EthernetSettings) { s.IPv6.Gateway = "fe80::1%eth0" },
		func(s *EthernetSettings) { s.IPv4.Gateway = "10.0.0.1" }, func(s *EthernetSettings) { s.IPv4.Method = "shared" },
		func(s *EthernetSettings) { s.WakeOnLAN = 0x41 }, func(s *EthernetSettings) { s.Duplex = "full" },
	} {
		input := valid
		mutate(&input)
		if err := validateEthernetSettings(input); err == nil {
			t.Fatalf("无效配置未拒绝: %+v", input)
		}
	}
}

func TestEthernetPatchPreservesUneditedFields(t *testing.T) {
	original := ethernetPatchConfig(nil, "eth0", ethernetTestUUID, ethernetTestSettings())
	original["ipv4"]["route-data"] = dbus.MakeVariant([]map[string]dbus.Variant{{"dest": dbus.MakeVariant("10.0.0.0")}})
	original["connection"]["permissions"] = dbus.MakeVariant([]string{"user:rock:"})
	patched := ethernetPatchConfig(original, "eth0", ethernetTestUUID, ethernetTestSettings())
	patched["connection"]["id"] = dbus.MakeVariant("new")
	if ethernetString(original["connection"], "id") == "new" || patched["ipv4"]["route-data"].Signature().String() != "aa{sv}" || patched["connection"]["permissions"].Signature().String() != "as" {
		t.Fatal("修改了原配置或丢失高级字段")
	}
}

func TestEthernetHardwareCapabilitiesComeFromSupportedModes(t *testing.T) {
	device := EthernetDevice{LinkModes: []EthernetLinkMode{}}
	parseEthernetCapabilities("Supported link modes: 10baseT/Half 10baseT/Full\n 100baseT/Full 1000baseT/Full\nAdvertised link modes: 2500baseT/Full\nSupports auto-negotiation: Yes\nDuplex: Full\nSupports Wake-on: pg\n", &device)
	if len(device.LinkModes) != 4 || device.AutoNegotiate == nil || !*device.AutoNegotiate || device.WakeOnLANModes != "pg" || device.Duplex != "full" {
		t.Fatalf("能力解析错误: %+v", device)
	}
	value := ethernetTestSettings()
	value.Speed = 2500
	value.Duplex = "full"
	if err := validateEthernetHardware(device, nil, value); err == nil {
		t.Fatal("把 advertised 当成 supported")
	}
	value.Speed = 1000
	value.WakeOnLAN = 64
	if err := validateEthernetHardware(device, nil, value); err != nil {
		t.Fatal(err)
	}
	value.WakeOnLAN = 32
	if err := validateEthernetHardware(device, nil, value); err == nil {
		t.Fatal("允许不支持的 ARP 唤醒")
	}
}

func TestEthernetCannotModifyProfileActiveOnAnotherPort(t *testing.T) {
	m, b := newEthernetTestManager()
	b.second = true
	status, err := m.EthernetStatus(context.Background())
	if err != nil || len(status.Devices) != 2 {
		t.Fatalf("读取多网口失败: %+v %v", status, err)
	}
	if status.Devices[1].Profiles[0].Editable {
		t.Fatal("在 eth1 可以修改 eth0 活动配置")
	}
	if _, err := m.UpdateEthernet(context.Background(), EthernetUpdate{Interface: "eth1", UUID: ethernetTestUUID, Settings: ethernetTestSettings()}); err == nil {
		t.Fatal("跨网口修改未拒绝")
	}
	if b.checkpoint {
		t.Fatal("跨网口请求建立了错误的检查点")
	}
}
