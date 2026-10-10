//go:build linux

package carplaypairing

import (
	"bufio"
	"context"
	"io"
	"os/exec"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/godbus/dbus/v5"
)

type discoveryFixture struct {
	mu       sync.Mutex
	sessions map[dbus.Sender]bool
	filters  map[dbus.Sender]string
	fail     bool
}

// Get 在隔离总线模拟适配器电源，测试不会操作系统适配器。
func (f *discoveryFixture) Get(iface, property string) (dbus.Variant, *dbus.Error) {
	if iface != "org.bluez.Adapter1" || property != "Powered" {
		return dbus.Variant{}, &dbus.Error{Name: "org.bluez.Error.Failed", Body: []interface{}{"unexpected property"}}
	}
	return dbus.MakeVariant(true), nil
}

// SetDiscoveryFilter 按连接区分过滤器，防止测试把全局状态误当成自有会话。
func (f *discoveryFixture) SetDiscoveryFilter(sender dbus.Sender, filter map[string]dbus.Variant) *dbus.Error {
	f.mu.Lock()
	defer f.mu.Unlock()
	transport, _ := filter["Transport"].Value().(string)
	f.filters[sender] = transport
	return nil
}

// StartDiscovery 记录实际总线发送方，用于核对独立发现会话。
func (f *discoveryFixture) StartDiscovery(sender dbus.Sender) *dbus.Error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.filters[sender] != "bredr" {
		return &dbus.Error{Name: "org.bluez.Error.Failed", Body: []interface{}{"requires Classic"}}
	}
	f.sessions[sender] = true
	return nil
}

// StopDiscovery 仅删除发起者的会话，模拟其他客户端同时搜索。
func (f *discoveryFixture) StopDiscovery(sender dbus.Sender) *dbus.Error {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.sessions, sender)
	return nil
}

// GetManagedObjects 让真实编解码经过隔离总线，覆盖失败后的发现清理。
func (f *discoveryFixture) GetManagedObjects() (bluezObjects, *dbus.Error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.fail {
		return nil, &dbus.Error{Name: "org.bluez.Error.Failed", Body: []interface{}{"fixture failure"}}
	}
	return bluezObjects{"/org/bluez/hci0/dev_02_11_22_33_44_55": {"org.bluez.Device1": {"Address": dbus.MakeVariant(testPhone), "Adapter": dbus.MakeVariant(dbus.ObjectPath("/org/bluez/hci0")), "AddressType": dbus.MakeVariant("public"), "Alias": dbus.MakeVariant("fixture iPhone")}}}, nil
}

func privateDiscoveryBus(t *testing.T) (context.Context, string, *discoveryFixture) {
	t.Helper()
	if _, err := exec.LookPath("dbus-daemon"); err != nil {
		t.Skip("隔离总线测试需要 dbus-daemon")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	cmd := exec.CommandContext(ctx, "dbus-daemon", "--session", "--nofork", "--print-address=1")
	cmd.Stderr = io.Discard
	pipe, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		cancel()
		t.Fatal(err)
	}
	t.Cleanup(func() { cancel(); _ = cmd.Wait() })
	address, err := bufio.NewReader(pipe).ReadString('\n')
	if err != nil {
		t.Fatal(err)
	}
	address = strings.TrimSpace(address)
	conn, err := dbus.Connect(address, dbus.WithContext(ctx))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	if _, err := conn.RequestName("org.bluez", dbus.NameFlagDoNotQueue); err != nil {
		t.Fatal(err)
	}
	f := &discoveryFixture{sessions: make(map[dbus.Sender]bool), filters: make(map[dbus.Sender]string)}
	for _, export := range []struct {
		path  dbus.ObjectPath
		iface string
	}{{"/org/bluez/hci0", "org.bluez.Adapter1"}, {"/org/bluez/hci0", "org.freedesktop.DBus.Properties"}, {"/", "org.freedesktop.DBus.ObjectManager"}} {
		if err := conn.Export(f, export.path, export.iface); err != nil {
			t.Fatal(err)
		}
	}
	return ctx, address, f
}

func TestDiscoveryUsesOwnDBusSessionAndPreservesOtherDiscoveryOwner(t *testing.T) {
	ctx, address, f := privateDiscoveryBus(t)
	other, err := dbus.Connect(address, dbus.WithContext(ctx))
	if err != nil {
		t.Fatal(err)
	}
	defer other.Close()
	adapter := other.Object("org.bluez", "/org/bluez/hci0")
	if err := adapter.CallWithContext(ctx, "org.bluez.Adapter1.SetDiscoveryFilter", 0, map[string]dbus.Variant{"Transport": dbus.MakeVariant("bredr")}).Err; err != nil {
		t.Fatal(err)
	}
	if err := adapter.CallWithContext(ctx, "org.bluez.Adapter1.StartDiscovery", 0).Err; err != nil {
		t.Fatal(err)
	}
	for _, fail := range []bool{false, true} {
		f.mu.Lock()
		f.fail = fail
		f.mu.Unlock()
		conn, err := dbus.Connect(address, dbus.WithContext(ctx))
		if err != nil {
			t.Fatal(err)
		}
		devices, err := scanOn(ctx, conn, time.Millisecond)
		_ = conn.Close()
		if fail && err == nil || !fail && (err != nil || len(devices) != 1 || devices[0].Name != "fixture iPhone") {
			t.Fatal(devices, err)
		}
		f.mu.Lock()
		remaining := len(f.sessions)
		f.mu.Unlock()
		if remaining != 1 {
			t.Fatal("搜索清理影响其他发现会话", remaining)
		}
	}
}
