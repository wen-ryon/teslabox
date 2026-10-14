//go:build linux

package carplaypairing

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/godbus/dbus/v5"
	"golang.org/x/sys/unix"
)

const incomingProfilePath dbus.ObjectPath = "/org/teslabox/CarPlayDiscovery"
const incomingProfileUUID = "00000000-deca-fade-deca-deafdecacaff"

// incomingProfile 只维持系统配对所需的 SDP 和 ACL；不解析 iAP，不把接受连接算作视频登记。
type incomingProfile struct {
	mu              sync.Mutex
	agent           *incomingAgent
	registered      bool
	accepting       bool
	device          dbus.ObjectPath
	file            int
	generation      uint64
	leaseGeneration uint64
	released        bool
	handoff         bool
	handoffDevice   dbus.ObjectPath
	closing         bool
	removing        bool
	operation       chan struct{}
	acceptedAt      time.Time
	refreshAttempt  uint64
	refreshCancel   context.CancelFunc
	refreshWorkers  sync.WaitGroup
}

func (p *incomingProfile) beginHandoff(device dbus.ObjectPath) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if !p.registered || !p.accepting || p.released {
		return errors.New("首次登记 Profile 已不可交接")
	}
	p.handoff, p.accepting = true, false
	p.cancelRefreshLocked()
	p.handoffDevice = device
	return nil
}

func (p *incomingProfile) handoffProtected() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.handoff
}

func verifyAndReleaseIncoming(ctx context.Context, phone string, confirmation *Confirmation) error {
	if err := verifyIncomingPhone(ctx, phone); err != nil {
		return err
	}
	confirmation.setPhase("profile_handoff")
	return confirmation.releaseIncoming(ctx)
}

func newIncomingProfile(agent *incomingAgent) *incomingProfile {
	return &incomingProfile{agent: agent, file: -1, operation: make(chan struct{}, 1)}
}

func (p *incomingProfile) acquire(ctx context.Context) error {
	select {
	case p.operation <- struct{}{}:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (p *incomingProfile) dropConnection() {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.closeConnectionLocked()
}

func (p *incomingProfile) cancelConnection() {
	p.mu.Lock()
	defer p.mu.Unlock()
	// 与新连接接收使用同一锁顺序，旧 Cancel 不能在释放 FD 后清掉新一轮 pending。
	p.agent.mu.Lock()
	defer p.agent.mu.Unlock()
	p.agent.pending = ""
	p.leaseGeneration++
	p.closeConnectionLocked()
}

func (p *incomingProfile) closeConnectionLocked() {
	p.cancelRefreshLocked()
	if p.file >= 0 {
		_ = unix.Close(p.file)
		p.file = -1
	}
	p.device = ""
	p.generation++
}

func (p *incomingProfile) connectionGeneration() uint64 {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.generation
}

func (p *incomingProfile) disconnected(device dbus.ObjectPath, generation uint64) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.generation != generation {
		return
	}
	// 与 NewConnection 统一为 Profile→Agent 顺序；旧连接事件不能关闭后续代次的 FD。
	p.agent.mu.Lock()
	if p.agent.pending == device {
		p.agent.pending = ""
	}
	p.agent.mu.Unlock()
	if p.device == device {
		p.closeConnectionLocked()
	}
}

// NewConnection 持有本轮已配对手机的描述符，直到服务发现和交接完成，避免提前断开 ACL。
func (p *incomingProfile) NewConnection(sender dbus.Sender, device dbus.ObjectPath, fd dbus.UnixFD, properties map[string]dbus.Variant) *dbus.Error {
	file := int(fd)
	owned := false
	defer func() {
		if !owned {
			_ = unix.Close(file)
		}
	}()
	if string(sender) != p.agent.owner || p.agent.ctx.Err() != nil {
		return rejected()
	}
	p.mu.Lock()
	accepting := p.accepting
	late := p.handoff && !p.closing && !p.released && !p.removing && p.handoffDevice == device
	generation := p.generation
	p.mu.Unlock()
	if !(accepting && p.agent.manager.incomingAvailable()) && !(late && p.agent.manager.incomingHandoffActive(device)) {
		return rejected()
	}
	ctx, cancel := context.WithTimeout(p.agent.ctx, 3*time.Second)
	defer cancel()
	props, err := phoneProperties(ctx, p.agent.conn, p.agent.owner, device)
	if err != nil || incomingPhone(device, props) == "" || props["Paired"].Value() != true || props["Connected"].Value() != true {
		return rejected()
	}
	if kind, err := unix.GetsockoptInt(file, unix.SOL_SOCKET, unix.SO_TYPE); err != nil || kind != unix.SOCK_STREAM {
		return rejected()
	}
	if _, err := unix.FcntlInt(uintptr(file), unix.F_SETFD, unix.FD_CLOEXEC); err != nil {
		return rejected()
	}
	available := p.agent.manager.incomingAvailable()
	activeHandoff := p.agent.manager.incomingHandoffActive(device)
	p.mu.Lock()
	defer p.mu.Unlock()
	late = p.handoff && !p.closing && !p.released && !p.removing && p.handoffDevice == device
	if !(p.accepting && available) && !(late && activeHandoff) || p.file >= 0 || p.generation != generation || p.agent.ctx.Err() != nil {
		return rejected()
	}
	if !late {
		if err := p.agent.recordPending(device); err != nil {
			return err
		}
	}
	p.device, p.file, owned = device, file, true
	p.generation++
	p.acceptedAt = time.Now()
	p.agent.logCallback("ProfileNewConnection", "waiting_services")
	return nil
}

// RequestDisconnection 只释放 BlueZ 指定的自有手机连接，重复回调不会重复关闭描述符。
func (p *incomingProfile) RequestDisconnection(sender dbus.Sender, device dbus.ObjectPath) *dbus.Error {
	if string(sender) != p.agent.owner {
		return rejected()
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.device == device && p.file >= 0 {
		p.closeConnectionLocked()
	}
	return nil
}

// Release 在总线拥有者释放 Profile 时立即关闭接收入口和自有连接。
func (p *incomingProfile) Release(sender dbus.Sender) *dbus.Error {
	if string(sender) != p.agent.owner {
		return rejected()
	}
	p.mu.Lock()
	p.accepting, p.registered = false, false
	p.released = true
	p.closing, p.handoff = true, false
	p.handoffDevice = ""
	p.leaseGeneration++
	p.removing = true
	p.agent.mu.Lock()
	p.agent.pending = ""
	p.agent.mu.Unlock()
	p.closeConnectionLocked()
	p.mu.Unlock()
	return nil
}

func (p *incomingProfile) present(ctx context.Context) (bool, error) {
	var uuids []string
	var value dbus.Variant
	err := p.agent.conn.Object(p.agent.owner, "/org/bluez/hci0").CallWithContext(ctx, "org.freedesktop.DBus.Properties.Get", 0, "org.bluez.Adapter1", "UUIDs").Store(&value)
	if err != nil {
		return false, err
	}
	if err := dbus.Store([]interface{}{value.Value()}, &uuids); err != nil {
		return false, err
	}
	for _, uuid := range uuids {
		if uuid == incomingProfileUUID {
			return true, nil
		}
	}
	return false, nil
}

func (p *incomingProfile) register(ctx context.Context) error {
	if err := p.acquire(ctx); err != nil {
		return err
	}
	defer func() { <-p.operation }()
	p.mu.Lock()
	registered := p.registered
	removing := p.removing
	p.mu.Unlock()
	if registered {
		return nil
	}
	if removing {
		return errors.New("上一轮首登广播尚未确认撤销")
	}
	if present, err := p.present(ctx); err != nil {
		return err
	} else if present {
		return errors.New("iAP 服务已被其他接收端持有，首次登记不重复注册")
	}
	options := map[string]dbus.Variant{
		"Name": dbus.MakeVariant("TeslaBox CarPlay"), "Role": dbus.MakeVariant("server"),
		"Channel": dbus.MakeVariant(uint16(4)), "RequireAuthentication": dbus.MakeVariant(true),
		"RequireAuthorization": dbus.MakeVariant(false), "AutoConnect": dbus.MakeVariant(false),
	}
	// BlueZ 可能在 RegisterProfile 返回前投递连接；先开放回调，失败时收回描述符。
	p.mu.Lock()
	p.accepting = true
	p.released = false
	p.generation++
	p.leaseGeneration++
	leaseGeneration := p.leaseGeneration
	p.mu.Unlock()
	if err := p.agent.conn.Object(p.agent.owner, "/org/bluez").CallWithContext(ctx, "org.bluez.ProfileManager1.RegisterProfile", 0, incomingProfilePath, incomingProfileUUID, options).Err; err != nil {
		p.mu.Lock()
		p.accepting = false
		p.mu.Unlock()
		p.dropConnection()
		return err
	}
	p.mu.Lock()
	if p.released || p.leaseGeneration != leaseGeneration {
		p.accepting = false
		// Cancel 可在方法返回前改变连接代次，但 BlueZ 仍可能已注册，必须留给退出清理注销。
		p.registered = !p.released
		p.mu.Unlock()
		return errors.New("注册期间 BlueZ 已释放首登服务")
	}
	p.registered, p.accepting = true, true
	p.mu.Unlock()
	p.agent.logCallback("RegisterProfile", "discovery_ready")
	return nil
}

func (p *incomingProfile) unregister(ctx context.Context) error {
	if err := p.acquire(ctx); err != nil {
		return err
	}
	defer func() { <-p.operation }()
	p.mu.Lock()
	p.accepting = false
	p.closing = true
	p.cancelRefreshLocked()
	p.generation++
	p.leaseGeneration++
	registered := p.registered
	removing := p.removing
	p.mu.Unlock()
	if err := p.waitRefresh(ctx); err != nil {
		return err
	}
	if !registered && !removing {
		p.dropConnection()
		p.mu.Lock()
		p.handoff, p.closing = false, false
		p.handoffDevice = ""
		p.mu.Unlock()
		return nil
	}
	if registered {
		if err := p.agent.conn.Object(p.agent.owner, "/org/bluez").CallWithContext(ctx, "org.bluez.ProfileManager1.UnregisterProfile", 0, incomingProfilePath).Err; err != nil {
			return err
		}
		p.mu.Lock()
		p.registered = false
		p.removing = true
		p.mu.Unlock()
	}
	p.dropConnection()
	// 注销方法返回后仍确认广播撤销，未确认前禁止启动占用同一 UUID/channel 的接收端。
	for {
		present, err := p.present(ctx)
		if err != nil {
			return err
		}
		if !present {
			p.mu.Lock()
			p.removing, p.handoff, p.closing = false, false, false
			p.handoffDevice = ""
			p.mu.Unlock()
			return nil
		}
		timer := time.NewTimer(20 * time.Millisecond)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-timer.C:
		}
	}
}
