//go:build linux

package carplaypairing

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/godbus/dbus/v5"
)

const incomingAgentPath dbus.ObjectPath = "/org/teslabox/CarPlayPairing"

func incomingPhone(path dbus.ObjectPath, props map[string]dbus.Variant) string {
	address, _ := props["Address"].Value().(string)
	adapter, _ := props["Adapter"].Value().(dbus.ObjectPath)
	kind, _ := props["AddressType"].Value().(string)
	blocked, _ := props["Blocked"].Value().(bool)
	class, _ := props["Class"].Value().(uint32)
	icon, _ := props["Icon"].Value().(string)
	if !phoneAddress.MatchString(address) || address == "00:00:00:00:00:00" ||
		path != dbus.ObjectPath("/org/bluez/hci0/dev_"+strings.ReplaceAll(address, ":", "_")) ||
		adapter != "/org/bluez/hci0" || kind != "public" || blocked || (class&0x1f00 != 0x0200 && icon != "phone") {
		return ""
	}
	return address
}

func phoneProperties(ctx context.Context, conn *dbus.Conn, owner string, path dbus.ObjectPath) (map[string]dbus.Variant, error) {
	var props map[string]dbus.Variant
	err := conn.Object(owner, path).CallWithContext(ctx, "org.freedesktop.DBus.Properties.GetAll", 0, "org.bluez.Device1").Store(&props)
	return props, err
}

func incomingLinkReady(props map[string]dbus.Variant) bool {
	// Paired 可能来自旧缓存；本轮连接及 SDP 完成前不能关闭系统配对入口。
	return props["Paired"].Value() == true && props["Connected"].Value() == true && props["ServicesResolved"].Value() == true
}

func verifyIncomingPhone(ctx context.Context, phone string) error {
	conn, err := dbus.ConnectSystemBus(dbus.WithContext(ctx))
	if err != nil {
		return err
	}
	defer conn.Close()
	var owner string
	if err = conn.BusObject().CallWithContext(ctx, "org.freedesktop.DBus.GetNameOwner", 0, "org.bluez").Store(&owner); err != nil {
		return err
	}
	path := dbus.ObjectPath("/org/bluez/hci0/dev_" + strings.ReplaceAll(phone, ":", "_"))
	props, err := phoneProperties(ctx, conn, owner, path)
	if err != nil || incomingPhone(path, props) != phone || !incomingLinkReady(props) {
		return errors.New("手机主动配对的系统身份尚未确认")
	}
	return nil
}

type incomingAgent struct {
	mu       sync.Mutex
	ctx      context.Context
	conn     *dbus.Conn
	owner    string
	manager  *Manager
	pending  dbus.ObjectPath
	deadline time.Time
	logger   *slog.Logger
	profile  *incomingProfile
}

func (a *incomingAgent) logCallback(method, result string) {
	if a.logger != nil {
		// 只记录阶段与结果，不写配对数字、手机名称或密钥。
		a.logger.Info("CarPlay 蓝牙配对回调", "method", method, "result", result)
	}
}

func rejected() *dbus.Error {
	return dbus.NewError("org.bluez.Error.Rejected", []interface{}{"CarPlay pairing unavailable"})
}

func (m *Manager) incomingAvailable() bool {
	m.mu.Lock()
	busy := m.closed || m.scanDone != nil || m.current != nil && (m.current.status.Running || m.current.cleanupPending)
	m.mu.Unlock()
	return !busy && m.incomingReady != nil && m.incomingReady()
}

func (m *Manager) incomingHandoffActive(device dbus.ObjectPath) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed || m.current == nil || !m.current.incoming || !m.current.status.Running || m.current.confirmation.ctx.Err() != nil {
		return false
	}
	return device == dbus.ObjectPath("/org/bluez/hci0/dev_"+strings.ReplaceAll(m.current.status.PhoneID, ":", "_"))
}

func (a *incomingAgent) authorize(sender dbus.Sender, path dbus.ObjectPath) *dbus.Error {
	if string(sender) != a.owner || a.ctx.Err() != nil || !a.manager.incomingAvailable() {
		return rejected()
	}
	ctx, cancel := context.WithTimeout(a.ctx, 3*time.Second)
	defer cancel()
	props, err := phoneProperties(ctx, a.conn, a.owner, path)
	if err != nil || incomingPhone(path, props) == "" {
		return rejected()
	}
	return a.recordPending(path)
}

func (a *incomingAgent) recordPending(path dbus.ObjectPath) *dbus.Error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.ctx.Err() != nil {
		return rejected()
	}
	if a.pending != "" && a.pending != path && time.Now().Before(a.deadline) {
		return rejected()
	}
	a.pending, a.deadline = path, time.Now().Add(120*time.Second)
	return nil
}

func (a *incomingAgent) disconnected(device dbus.ObjectPath) {
	if a.profile == nil {
		return
	}
	generation := a.profile.connectionGeneration()
	ctx, cancel := context.WithTimeout(a.ctx, 3*time.Second)
	defer cancel()
	props, err := phoneProperties(ctx, a.conn, a.owner, device)
	if err != nil || props["Connected"].Value() == true {
		return
	}
	a.profile.disconnected(device, generation)
	a.logCallback("Disconnected", "cleared_current_generation")
}

// RequestAuthorization 仅接收开启功能且尚未登记时由手机主动发起的系统配对。
func (a *incomingAgent) RequestAuthorization(sender dbus.Sender, device dbus.ObjectPath) *dbus.Error {
	err := a.authorize(sender, device)
	result := "accepted"
	if err != nil {
		result = "rejected"
	}
	a.logCallback("RequestAuthorization", result)
	return err
}

// RequestConfirmation 按手机端确认流程自动接收，仍限制为本轮未登记手机且不记录配对数字。
func (a *incomingAgent) RequestConfirmation(sender dbus.Sender, device dbus.ObjectPath, passkey uint32) *dbus.Error {
	if passkey > 999999 {
		a.logCallback("RequestConfirmation", "invalid_passkey")
		return rejected()
	}
	err := a.authorize(sender, device)
	result := "accepted"
	if err != nil {
		result = "rejected"
	}
	a.logCallback("RequestConfirmation", result)
	return err
}

// RequestPinCode 不支持旧式固定 PIN 配对。
func (a *incomingAgent) RequestPinCode(sender dbus.Sender, device dbus.ObjectPath) (string, *dbus.Error) {
	a.logCallback("RequestPinCode", "unsupported")
	return "", rejected()
}

// RequestPasskey 不接受缺少用户输入的口令请求。
func (a *incomingAgent) RequestPasskey(sender dbus.Sender, device dbus.ObjectPath) (uint32, *dbus.Error) {
	a.logCallback("RequestPasskey", "unsupported")
	return 0, rejected()
}

// AuthorizeService 只允许当前本轮已经系统配对的手机继续服务发现。
func (a *incomingAgent) AuthorizeService(sender dbus.Sender, device dbus.ObjectPath, uuid string) (result *dbus.Error) {
	defer func() {
		outcome := "accepted"
		if result != nil {
			outcome = "rejected"
		}
		a.logCallback("AuthorizeService", outcome)
	}()
	a.mu.Lock()
	pending := a.pending
	a.mu.Unlock()
	if string(sender) != a.owner || pending != device {
		return rejected()
	}
	ctx, cancel := context.WithTimeout(a.ctx, 3*time.Second)
	defer cancel()
	props, err := phoneProperties(ctx, a.conn, a.owner, device)
	if err != nil || props["Paired"].Value() != true {
		return rejected()
	}
	return nil
}

// Cancel 清理本轮未完成的手机请求，不删除系统配对记录。
func (a *incomingAgent) Cancel(sender dbus.Sender) *dbus.Error {
	if string(sender) != a.owner {
		return rejected()
	}
	if a.profile != nil {
		a.profile.cancelConnection()
	}
	a.mu.Lock()
	a.pending = ""
	a.mu.Unlock()
	a.logCallback("Cancel", "cleared")
	return nil
}

// Release 在 BlueZ 释放自有 Agent 时撤销本轮等待。
func (a *incomingAgent) Release(sender dbus.Sender) *dbus.Error { return a.Cancel(sender) }

func (m *Manager) startIncoming(ctx context.Context, phone string) (TaskStatus, error) {
	return m.startIncomingWithRelease(ctx, phone, nil)
}

func (m *Manager) startIncomingWithRelease(ctx context.Context, phone string, release func(context.Context) error) (TaskStatus, error) {
	if !m.incomingAvailable() {
		return TaskStatus{}, ErrBusy
	}
	// 只有本轮连接与服务发现完成才能交接热点，旧 Paired 缓存不能接管管理网络。
	if err := verifyIncomingPhone(ctx, phone); err != nil {
		return TaskStatus{}, err
	}
	plan, err := m.HandoffPlan(ctx)
	if err != nil {
		m.failIncoming(ctx, phone, "network_handoff")
		return TaskStatus{}, err
	}
	return m.startWithRelease(ctx, phone, 120*time.Second, plan, release, true)
}

func (m *Manager) failIncoming(ctx context.Context, phone, phase string) {
	confirmation, err := NewConfirmation(ctx, 120*time.Second)
	if err != nil {
		return
	}
	confirmation.setPhase(phase)
	confirmation.recordFailurePhase()
	confirmation.cancel()
	done := make(chan struct{})
	close(done)
	prompt := confirmation.Status()
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed || m.current != nil && (m.current.status.Running || m.current.cleanupPending) {
		return
	}
	m.current = &pairingTask{confirmation: confirmation, done: done, cancel: func() {}, incoming: true, status: TaskStatus{RequestID: prompt.RequestID, PhoneID: phone, State: "failed", ExpiresAt: prompt.ExpiresAt}}
}

// StartIncoming 随主程序管理手机发起的配对，Close 会等待蓝牙临时状态清理。
func (m *Manager) StartIncoming(ctx context.Context, logger *slog.Logger) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed || m.incomingDone != nil {
		return
	}
	child, cancel := context.WithCancel(ctx)
	m.logger = logger
	m.incomingCancel, m.incomingDone = cancel, make(chan struct{})
	go func() { defer close(m.incomingDone); m.serveIncoming(child, logger) }()
}

func (m *Manager) serveIncoming(ctx context.Context, logger *slog.Logger) {
	if logger == nil {
		logger = slog.Default()
	}
	for ctx.Err() == nil {
		if err := m.serveIncomingBus(ctx, logger); err != nil && ctx.Err() == nil {
			logger.Warn("CarPlay 手机主动配对服务暂不可用", "error", err)
		}
		timer := time.NewTimer(3 * time.Second)
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
		}
	}
}

func (m *Manager) serveIncomingBus(ctx context.Context, logger *slog.Logger) error {
	defer m.setIncomingListening(false)
	// 退出后仍需恢复适配器属性，总线不能随业务上下文提前关闭。
	conn, err := dbus.ConnectSystemBus()
	if err != nil {
		return err
	}
	defer conn.Close()
	var owner string
	if err = conn.BusObject().CallWithContext(ctx, "org.freedesktop.DBus.GetNameOwner", 0, "org.bluez").Store(&owner); err != nil {
		return err
	}
	adapter := conn.Object(owner, "/org/bluez/hci0")
	signals := make(chan *dbus.Signal, 32)
	conn.Signal(signals)
	defer conn.RemoveSignal(signals)
	if err := conn.AddMatchSignal(dbus.WithMatchSender(owner), dbus.WithMatchInterface("org.freedesktop.DBus.Properties"), dbus.WithMatchMember("PropertiesChanged"), dbus.WithMatchPathNamespace("/org/bluez/hci0")); err != nil {
		return err
	}
	if err := conn.AddMatchSignal(dbus.WithMatchSender("org.freedesktop.DBus"), dbus.WithMatchInterface("org.freedesktop.DBus"), dbus.WithMatchMember("NameOwnerChanged"), dbus.WithMatchArg(0, "org.bluez")); err != nil {
		return err
	}
	set := func(op context.Context, key string, value interface{}) error {
		return adapter.CallWithContext(op, "org.freedesktop.DBus.Properties.Set", 0, "org.bluez.Adapter1", key, dbus.MakeVariant(value)).Err
	}
	get := func(key string) (dbus.Variant, error) {
		var value dbus.Variant
		err := adapter.CallWithContext(ctx, "org.freedesktop.DBus.Properties.Get", 0, "org.bluez.Adapter1", key).Store(&value)
		return value, err
	}
	original := map[string]dbus.Variant{}
	for _, key := range []string{"Alias", "Discoverable", "Pairable", "DiscoverableTimeout", "PairableTimeout"} {
		value, err := get(key)
		if err != nil {
			return err
		}
		original[key] = value
	}
	a := &incomingAgent{ctx: ctx, conn: conn, owner: owner, manager: m, logger: logger}
	a.profile = newIncomingProfile(a)
	if err := conn.Export(a.profile, incomingProfilePath, "org.bluez.Profile1"); err != nil {
		return err
	}
	if err := conn.Export(a, incomingAgentPath, "org.bluez.Agent1"); err != nil {
		return err
	}
	agents := conn.Object(owner, "/org/bluez")
	registered := false
	applied := map[string]interface{}{}
	defer func() {
		cleanup, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cancel()
		if err := a.profile.unregister(cleanup); err != nil {
			logger.Warn("CarPlay 首登服务注销未确认，断开自有总线清理", "error", err)
		}
		a.profile.dropConnection()
		if registered {
			_ = agents.CallWithContext(cleanup, "org.bluez.AgentManager1.UnregisterAgent", 0, incomingAgentPath).Err
		}
		// 仅恢复仍保持为本服务写入值的属性，避免覆盖其他蓝牙管理者的修改。
		for _, key := range []string{"Discoverable", "Pairable", "DiscoverableTimeout", "PairableTimeout", "Alias"} {
			value, owned := applied[key]
			if !owned {
				continue
			}
			var current dbus.Variant
			if adapter.CallWithContext(cleanup, "org.freedesktop.DBus.Properties.Get", 0, "org.bluez.Adapter1", key).Store(&current) == nil && current.Value() == value {
				_ = set(cleanup, key, original[key].Value())
			}
		}
	}()
	apply := func(key string, value interface{}) error {
		if old, ok := applied[key]; ok && old == value {
			return nil
		}
		if err := set(ctx, key, value); err != nil {
			return err
		}
		applied[key] = value
		return nil
	}
	if err := apply("Alias", "TeslaBox CarPlay"); err != nil {
		return err
	}
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	lastPendingState := ""
	var lastPaired, lastConnected, lastResolved bool
	for {
		open := m.incomingAvailable()
		a.mu.Lock()
		pending, deadline := a.pending, a.deadline
		a.mu.Unlock()
		if open && !registered {
			// 双方使用数字确认能力，避免旧配对记录被 BlueZ 的 JustWorksRepairing 策略提前拒绝。
			// 手机仍需确认；板端按产品既定流程接收，不宣称用户核对过两端数字。
			if err := agents.CallWithContext(ctx, "org.bluez.AgentManager1.RegisterAgent", 0, incomingAgentPath, "DisplayYesNo").Err; err != nil {
				return err
			}
			registered = true
			if err := agents.CallWithContext(ctx, "org.bluez.AgentManager1.RequestDefaultAgent", 0, incomingAgentPath).Err; err != nil {
				return err
			}
		}
		if registered && !open && pending == "" {
			if err := agents.CallWithContext(ctx, "org.bluez.AgentManager1.UnregisterAgent", 0, incomingAgentPath).Err; err != nil {
				return err
			}
			registered = false
		}
		if open {
			registerCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
			err := a.profile.register(registerCtx)
			cancel()
			if err != nil {
				return err
			}
		} else if !a.profile.handoffProtected() {
			releaseCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
			err := a.profile.unregister(releaseCtx)
			cancel()
			if err != nil {
				return err
			}
		}
		for _, property := range []string{"DiscoverableTimeout", "PairableTimeout"} {
			if err := apply(property, uint32(0)); err != nil {
				return err
			}
		}
		if err := apply("Pairable", open); err != nil {
			return err
		}
		if err := apply("Discoverable", open); err != nil {
			return err
		}
		m.setIncomingListening(open && registered)
		if pending == "" {
			lastPendingState = ""
		}
		if pending != "" {
			if !open || time.Now().After(deadline) {
				logger.Info("CarPlay 蓝牙配对等待结束", "expired", time.Now().After(deadline), "registration_available", open)
				a.mu.Lock()
				a.pending = ""
				a.mu.Unlock()
				a.profile.dropConnection()
			} else {
				readCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
				props, err := phoneProperties(readCtx, conn, owner, pending)
				cancel()
				state := "unavailable"
				if err == nil {
					state = "pairing"
					if props["Paired"].Value() == true {
						state = "waiting_services"
					}
					if incomingLinkReady(props) {
						state = "ready"
					}
				}
				paired, connected, resolved := props["Paired"].Value() == true, props["Connected"].Value() == true, props["ServicesResolved"].Value() == true
				if state != lastPendingState || paired != lastPaired || connected != lastConnected || resolved != lastResolved {
					logger.Info("CarPlay 蓝牙配对链路状态", "state", state, "paired", paired, "connected", connected, "services_resolved", resolved)
					lastPendingState = state
					lastPaired, lastConnected, lastResolved = paired, connected, resolved
				}
				if err == nil && incomingLinkReady(props) {
					phone := incomingPhone(pending, props)
					if phone == "" {
						a.mu.Lock()
						a.pending = ""
						a.mu.Unlock()
						continue
					}
					// 信任只授予本轮由手机确认完成的系统配对，广播和未配对设备没有这条路径。
					if err := conn.Object(owner, pending).CallWithContext(ctx, "org.freedesktop.DBus.Properties.Set", 0, "org.bluez.Device1", "Trusted", dbus.MakeVariant(true)).Err; err != nil {
						return err
					}
					if err = a.profile.beginHandoff(pending); err != nil {
						return err
					}
					_, err = m.startIncomingWithRelease(ctx, phone, func(op context.Context) error {
						releaseCtx, cancel := context.WithTimeout(op, 5*time.Second)
						defer cancel()
						return a.profile.unregister(releaseCtx)
					})
					if err != nil {
						releaseCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
						_ = a.profile.unregister(releaseCtx)
						cancel()
						logger.Warn("CarPlay 主动配对后登记未启动", "error", err)
						return err
					} else {
						logger.Info("CarPlay 手机主动配对完成，等待视频登记")
					}
					a.mu.Lock()
					a.pending = ""
					a.mu.Unlock()
				}
			}
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-conn.Context().Done():
			return errors.New("蓝牙系统总线已断开")
		case <-ticker.C:
		case signal := <-signals:
			if signal != nil && signal.Name == "org.freedesktop.DBus.NameOwnerChanged" && len(signal.Body) == 3 && signal.Body[0] == "org.bluez" {
				return errors.New("蓝牙服务身份已变化，重新建立主动配对入口")
			}
			// 已配对但视频登记失败的手机，重新主动连接也可以重新尝试；不导入缓存记录。
			if signal == nil || signal.Sender != owner || len(signal.Body) != 3 || signal.Body[0] != "org.bluez.Device1" {
				continue
			}
			changed, ok := signal.Body[1].(map[string]dbus.Variant)
			if ok && changed["Connected"].Value() == true {
				if a.authorize(dbus.Sender(owner), signal.Path) == nil {
					a.logCallback("Connected", "waiting_pairing_and_services")
				}
			}
			if ok && changed["Connected"].Value() == false {
				a.disconnected(signal.Path)
			}
		}
	}
}
