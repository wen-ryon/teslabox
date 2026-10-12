//go:build linux

package carplaypairing

import (
	"context"
	"io"
	"log/slog"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/godbus/dbus/v5"
)

type incomingFixture struct {
	mu         sync.Mutex
	adapter    map[string]dbus.Variant
	phone      map[string]dbus.Variant
	registered dbus.Sender
	capability string
	trusted    bool
}

// Get 用于核对退出恢复，隔离测试不接触系统适配器。
func (f *incomingFixture) Get(iface, key string) (dbus.Variant, *dbus.Error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.adapter[key], nil
}

// Set 记录实际总线写入，核对未配对时不会授予信任。
func (f *incomingFixture) Set(iface, key string, value dbus.Variant) *dbus.Error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if iface == "org.bluez.Device1" {
		if key == "Trusted" {
			f.trusted = value.Value() == true
		}
		return nil
	}
	f.adapter[key] = value
	return nil
}

// GetAll 返回真实 D-Bus 编解码的手机属性。
func (f *incomingFixture) GetAll(iface string) (map[string]dbus.Variant, *dbus.Error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	copy := make(map[string]dbus.Variant)
	for k, v := range f.phone {
		copy[k] = v
	}
	return copy, nil
}

// RegisterAgent 记录实际调用方，供模拟手机从 BlueZ 发起确认。
func (f *incomingFixture) RegisterAgent(sender dbus.Sender, path dbus.ObjectPath, capability string) *dbus.Error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.registered, f.capability = sender, capability
	return nil
}

// RequestDefaultAgent 仅核对本轮自有 Agent。
func (f *incomingFixture) RequestDefaultAgent(sender dbus.Sender, path dbus.ObjectPath) *dbus.Error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if sender != f.registered {
		return rejected()
	}
	return nil
}

// UnregisterAgent 验证关闭后释放 Agent。
func (f *incomingFixture) UnregisterAgent(sender dbus.Sender, path dbus.ObjectPath) *dbus.Error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.registered = ""
	return nil
}

func TestIncomingPhoneIdentityRejectsNonPhoneAndWrongAdapter(t *testing.T) {
	p := map[string]dbus.Variant{"Address": dbus.MakeVariant(testPhone), "Adapter": dbus.MakeVariant(dbus.ObjectPath("/org/bluez/hci0")), "AddressType": dbus.MakeVariant("public"), "Class": dbus.MakeVariant(uint32(0x7a020c))}
	path := dbus.ObjectPath("/org/bluez/hci0/dev_02_11_22_33_44_55")
	if incomingPhone(path, p) != testPhone {
		t.Fatal("手机身份未识别")
	}
	p["Class"] = dbus.MakeVariant(uint32(0x0500))
	if incomingPhone(path, p) != "" {
		t.Fatal("键盘被登记成手机")
	}
	p["Icon"] = dbus.MakeVariant("phone")
	p["Blocked"] = dbus.MakeVariant(true)
	if incomingPhone(path, p) != "" {
		t.Fatal("已阻止设备被接受")
	}
	if incomingPhone("/org/bluez/hci1/dev_02_11_22_33_44_55", p) != "" {
		t.Fatal("其他适配器被接受")
	}
}

func TestIncomingLinkRequiresCurrentConnectionAndServices(t *testing.T) {
	for _, state := range []struct {
		name      string
		paired    bool
		connected bool
		resolved  bool
		ready     bool
	}{
		{"旧配对缓存", true, false, false, false},
		{"ACL已连接但服务未完成", true, true, false, false},
		{"未完成配对", false, true, true, false},
		{"断开后残留服务", true, false, true, false},
		{"本轮链路就绪", true, true, true, true},
	} {
		t.Run(state.name, func(t *testing.T) {
			props := map[string]dbus.Variant{"Paired": dbus.MakeVariant(state.paired), "Connected": dbus.MakeVariant(state.connected), "ServicesResolved": dbus.MakeVariant(state.resolved)}
			if incomingLinkReady(props) != state.ready {
				t.Fatal("缓存或未完成连接被当作就绪", state)
			}
		})
	}
	if incomingLinkReady(nil) {
		t.Fatal("缺失属性被当作就绪")
	}
}

func TestIncomingWindowRejectsDisabledBusyAndCleanupPending(t *testing.T) {
	for _, state := range []string{"disabled", "closed", "scanning", "pairing", "cleanup_pending"} {
		t.Run(state, func(t *testing.T) {
			m, _ := NewManager(func(context.Context, string, *Confirmation) error { return nil })
			m.incomingReady = func() bool { return state != "disabled" }
			switch state {
			case "closed":
				m.closed = true
			case "scanning":
				m.scanDone = make(chan struct{})
			case "pairing":
				m.current = &pairingTask{status: TaskStatus{Running: true}}
			case "cleanup_pending":
				m.current = &pairingTask{cleanupPending: true}
			}
			if m.incomingAvailable() {
				t.Fatal("非待登记状态仍允许新手机")
			}
		})
	}
}

func TestIncomingPairingUsesPhoneConfirmationAndRestoresAdapter(t *testing.T) {
	for _, paired := range []bool{false, true} {
		name := "新手机主动配对"
		if paired {
			name = "已配对手机重新主动连接"
		}
		t.Run(name, func(t *testing.T) { incomingPairingScenario(t, paired) })
	}
}

func incomingPairingScenario(t *testing.T, alreadyPaired bool) {
	ctx, address, old := privateDiscoveryBus(t)
	t.Setenv("DBUS_SYSTEM_BUS_ADDRESS", address)
	f := &incomingFixture{adapter: map[string]dbus.Variant{
		"Alias": dbus.MakeVariant("rock-3a"), "Pairable": dbus.MakeVariant(false), "Discoverable": dbus.MakeVariant(false), "PairableTimeout": dbus.MakeVariant(uint32(0)), "DiscoverableTimeout": dbus.MakeVariant(uint32(180)),
	}, phone: map[string]dbus.Variant{
		"Address": dbus.MakeVariant(testPhone), "Adapter": dbus.MakeVariant(dbus.ObjectPath("/org/bluez/hci0")), "AddressType": dbus.MakeVariant("public"), "Icon": dbus.MakeVariant("phone"), "Paired": dbus.MakeVariant(false),
		"Connected": dbus.MakeVariant(true), "ServicesResolved": dbus.MakeVariant(false),
	}}
	phonePath := dbus.ObjectPath("/org/bluez/hci0/dev_02_11_22_33_44_55")
	for _, entry := range []struct {
		path  dbus.ObjectPath
		iface string
	}{{"/org/bluez/hci0", "org.freedesktop.DBus.Properties"}, {phonePath, "org.freedesktop.DBus.Properties"}, {"/org/bluez", "org.bluez.AgentManager1"}} {
		if err := old.conn.Export(f, entry.path, entry.iface); err != nil {
			t.Fatal(err)
		}
	}
	var ready atomic.Bool
	ready.Store(true)
	started := make(chan bool, 1)
	m, _ := NewManager(func(ctx context.Context, phone string, c *Confirmation) error {
		started <- c.incoming && phone == testPhone
		return nil
	})
	m.incomingReady = ready.Load
	m.StartIncoming(ctx, slog.New(slog.NewTextHandler(io.Discard, nil)))
	wait := func(check func() bool) {
		t.Helper()
		deadline := time.Now().Add(2 * time.Second)
		for !check() {
			if time.Now().After(deadline) {
				t.Fatal("隔离主动配对等待超时")
			}
			time.Sleep(10 * time.Millisecond)
		}
	}
	wait(func() bool {
		f.mu.Lock()
		defer f.mu.Unlock()
		return f.registered != "" && f.adapter["Discoverable"].Value() == true
	})
	f.mu.Lock()
	target := string(f.registered)
	capability := f.capability
	trusted := f.trusted
	f.mu.Unlock()
	if capability != "NoInputNoOutput" || trusted {
		t.Fatal("首次配对能力或信任边界错误")
	}
	attacker, err := dbus.Connect(address, dbus.WithContext(ctx))
	if err != nil {
		t.Fatal(err)
	}
	defer attacker.Close()
	if attacker.Object(target, incomingAgentPath).CallWithContext(ctx, "org.bluez.Agent1.RequestAuthorization", 0, phonePath).Err == nil {
		t.Fatal("非BlueZ调用者取得授权")
	}
	select {
	case <-started:
		t.Fatal("系统配对尚未完成就开始登记")
	case <-time.After(25 * time.Millisecond):
	}
	f.mu.Lock()
	if alreadyPaired {
		f.phone["Paired"] = dbus.MakeVariant(true)
	}
	f.mu.Unlock()
	if alreadyPaired {
		if err := old.conn.Emit(phonePath, "org.freedesktop.DBus.Properties.PropertiesChanged", "org.bluez.Device1", map[string]dbus.Variant{"Connected": dbus.MakeVariant(true)}, []string{}); err != nil {
			t.Fatal(err)
		}
	} else {
		if err := old.conn.Object(target, incomingAgentPath).CallWithContext(ctx, "org.bluez.Agent1.RequestAuthorization", 0, phonePath).Err; err != nil {
			t.Fatal(err)
		}
		f.mu.Lock()
		f.phone["Paired"] = dbus.MakeVariant(true)
		f.mu.Unlock()
	}
	// 缓存配对和新 ACL 连接不能证明本轮服务发现完成，不能提前关闭配对入口。
	select {
	case <-started:
		t.Fatal("服务发现尚未完成就开始登记")
	case <-time.After(1100 * time.Millisecond):
	}
	f.mu.Lock()
	if f.trusted || f.registered == "" || f.adapter["Pairable"].Value() != true {
		f.mu.Unlock()
		t.Fatal("服务发现期间提前交接或关闭配对入口")
	}
	f.phone["ServicesResolved"] = dbus.MakeVariant(true)
	f.mu.Unlock()
	if err := old.conn.Emit(phonePath, "org.freedesktop.DBus.Properties.PropertiesChanged", "org.bluez.Device1", map[string]dbus.Variant{"ServicesResolved": dbus.MakeVariant(true)}, []string{}); err != nil {
		t.Fatal(err)
	}
	select {
	case ok := <-started:
		if !ok {
			t.Fatal("再次发起出站配对")
		}
	case <-ctx.Done():
		t.Fatal("没有自动进入登记")
	}
	ready.Store(false)
	closeCtx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := m.Close(closeCtx); err != nil {
		t.Fatal(err)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.registered != "" || f.adapter["Discoverable"].Value() != false || f.adapter["Pairable"].Value() != false || f.adapter["Alias"].Value() != "rock-3a" || !f.trusted {
		t.Fatal("Agent或适配器状态没有恢复", f.adapter)
	}
}
