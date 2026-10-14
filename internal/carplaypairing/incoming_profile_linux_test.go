//go:build linux

package carplaypairing

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/godbus/dbus/v5"
	"golang.org/x/sys/unix"
)

// RegisterProfile 模拟真实总线注册并核对协议声明，测试不接触系统蓝牙。
func (f *incomingFixture) RegisterProfile(sender dbus.Sender, path dbus.ObjectPath, uuid string, options map[string]dbus.Variant) *dbus.Error {
	f.mu.Lock()
	if f.failProfile || f.profile != "" || path != incomingProfilePath || uuid != incomingProfileUUID ||
		options["Channel"].Value() != uint16(4) || options["Role"].Value() != "server" ||
		options["RequireAuthentication"].Value() != true || options["AutoConnect"].Value() != false {
		f.mu.Unlock()
		return rejected()
	}
	f.profile = sender
	f.adapter["UUIDs"] = dbus.MakeVariant([]string{incomingProfileUUID})
	hook := f.onRegister
	f.mu.Unlock()
	if hook != nil {
		hook(sender)
	}
	return nil
}

// UnregisterProfile 保留其他服务并验证只有注册者可以注销本轮服务。
func (f *incomingFixture) UnregisterProfile(sender dbus.Sender, path dbus.ObjectPath) *dbus.Error {
	f.mu.Lock()
	if f.failUnregister || sender != f.profile || path != incomingProfilePath {
		f.mu.Unlock()
		return rejected()
	}
	f.unregisterCalls++
	hook := f.beforeUnregister
	f.mu.Unlock()
	if hook != nil {
		hook()
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.profile = ""
	f.adapter["UUIDs"] = dbus.MakeVariant([]string{})
	if f.disconnectOnRelease {
		f.phone["Connected"] = dbus.MakeVariant(false)
		f.phone["ServicesResolved"] = dbus.MakeVariant(false)
	}
	return nil
}

func profileFixture(t *testing.T) (*incomingProfile, *incomingFixture, context.Context) {
	t.Helper()
	ctx, address, old := privateDiscoveryBus(t)
	t.Setenv("DBUS_SYSTEM_BUS_ADDRESS", address)
	f := &incomingFixture{adapter: map[string]dbus.Variant{"UUIDs": dbus.MakeVariant([]string{})}, phone: map[string]dbus.Variant{
		"Address": dbus.MakeVariant(testPhone), "Adapter": dbus.MakeVariant(dbus.ObjectPath("/org/bluez/hci0")),
		"AddressType": dbus.MakeVariant("public"), "Icon": dbus.MakeVariant("phone"),
		"Paired": dbus.MakeVariant(true), "Connected": dbus.MakeVariant(true), "ServicesResolved": dbus.MakeVariant(false),
	}}
	f.bluez = old.conn
	phone := dbus.ObjectPath("/org/bluez/hci0/dev_02_11_22_33_44_55")
	for _, item := range []struct {
		path  dbus.ObjectPath
		iface string
	}{
		{"/org/bluez/hci0", "org.freedesktop.DBus.Properties"}, {phone, "org.freedesktop.DBus.Properties"},
		{"/org/bluez", "org.bluez.ProfileManager1"},
	} {
		if err := old.conn.Export(f, item.path, item.iface); err != nil {
			t.Fatal(err)
		}
	}
	conn, err := dbus.Connect(address, dbus.WithContext(ctx))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	owner := old.conn.Names()[0]
	m, _ := NewManager(func(context.Context, string, *Confirmation) error { return nil })
	m.incomingReady = func() bool { return true }
	a := &incomingAgent{ctx: ctx, conn: conn, owner: owner, manager: m}
	p := newIncomingProfile(a)
	a.profile = p
	t.Cleanup(p.dropConnection)
	return p, f, ctx
}

func TestIncomingProfileOwnsRegistrationAndReleasesBeforeReceiver(t *testing.T) {
	p, f, ctx := profileFixture(t)
	if err := p.register(ctx); err != nil {
		t.Fatal(err)
	}
	if present, err := p.present(ctx); err != nil || !present {
		t.Fatal(present, err)
	}
	if err := p.unregister(ctx); err != nil {
		t.Fatal(err)
	}
	f.mu.Lock()
	remaining := f.profile
	f.mu.Unlock()
	if remaining != "" {
		t.Fatal("接收端启动前首登Profile未释放")
	}
	if err := p.register(ctx); err != nil {
		t.Fatal("失败后不能重新开放首登", err)
	}
	if err := p.unregister(ctx); err != nil {
		t.Fatal(err)
	}
}

func TestIncomingProfileRejectsConflictingOrFailedRegistration(t *testing.T) {
	for _, conflict := range []bool{true, false} {
		p, f, ctx := profileFixture(t)
		f.mu.Lock()
		f.failProfile = !conflict
		if conflict {
			f.adapter["UUIDs"] = dbus.MakeVariant([]string{incomingProfileUUID})
		}
		f.mu.Unlock()
		if p.register(ctx) == nil {
			t.Fatal("占用或失败的注册被当作成功")
		}
		p.mu.Lock()
		accepting := p.accepting
		p.mu.Unlock()
		if accepting {
			t.Fatal("注册失败后仍接收连接")
		}
		if err := p.unregister(ctx); err != nil {
			t.Fatal("未持有Profile却等待他人广播撤销", err)
		}
	}
}

func socketFD(t *testing.T) int {
	t.Helper()
	pair, err := unix.Socketpair(unix.AF_UNIX, unix.SOCK_STREAM, 0)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = unix.Close(pair[1]) })
	return pair[0]
}

func TestIncomingProfileHoldsEarlyFDWithoutClaimingServicesOrVideo(t *testing.T) {
	p, _, ctx := profileFixture(t)
	if err := p.register(ctx); err != nil {
		t.Fatal(err)
	}
	fd := socketFD(t)
	phone := dbus.ObjectPath("/org/bluez/hci0/dev_02_11_22_33_44_55")
	if err := p.NewConnection(dbus.Sender(p.agent.owner), phone, dbus.UnixFD(fd), nil); err != nil {
		t.Fatal(err)
	}
	if _, err := unix.FcntlInt(uintptr(fd), unix.F_GETFD, 0); err != nil {
		t.Fatal("服务发现完成前关闭了连接", err)
	}
	if p.agent.pending != phone || p.agent.manager.Status().State != "idle" {
		t.Fatal("连接被冒充登记完成")
	}
	if err := p.agent.Cancel(dbus.Sender(p.agent.owner)); err != nil {
		t.Fatal(err)
	}
	if _, err := unix.FcntlInt(uintptr(fd), unix.F_GETFD, 0); err != unix.EBADF {
		t.Fatal("取消后描述符未释放", err)
	}
	if err := p.unregister(ctx); err != nil {
		t.Fatal(err)
	}
}

func TestIncomingProfileRejectsForeignSenderAndUnpairedFD(t *testing.T) {
	for _, foreign := range []bool{true, false} {
		p, f, ctx := profileFixture(t)
		if err := p.register(ctx); err != nil {
			t.Fatal(err)
		}
		sender := dbus.Sender(p.agent.owner)
		if foreign {
			sender = ":99.99"
		} else {
			f.mu.Lock()
			f.phone["Paired"] = dbus.MakeVariant(false)
			f.mu.Unlock()
		}
		fd := socketFD(t)
		if p.NewConnection(sender, "/org/bluez/hci0/dev_02_11_22_33_44_55", dbus.UnixFD(fd), nil) == nil {
			t.Fatal("非本轮已配对手机取得连接")
		}
		if _, err := unix.FcntlInt(uintptr(fd), unix.F_GETFD, 0); err != unix.EBADF {
			t.Fatal("拒绝后的FD泄漏", err)
		}
		if err := p.unregister(ctx); err != nil {
			t.Fatal(err)
		}
	}
}

func TestIncomingProfileUnregisterFailureBlocksHandoff(t *testing.T) {
	p, f, ctx := profileFixture(t)
	if err := p.register(ctx); err != nil {
		t.Fatal(err)
	}
	f.mu.Lock()
	f.failUnregister = true
	f.mu.Unlock()
	short, cancel := context.WithTimeout(ctx, 100*time.Millisecond)
	defer cancel()
	if p.unregister(short) == nil {
		t.Fatal("注销失败被当作可交接")
	}
	p.mu.Lock()
	accepting := p.accepting
	p.mu.Unlock()
	if accepting {
		t.Fatal("注销失败后继续接受新连接")
	}
	f.mu.Lock()
	f.failUnregister = false
	f.mu.Unlock()
	if err := p.unregister(ctx); err != nil {
		t.Fatal(err)
	}
}

func TestIncomingProfileOwnerReleaseClosesFD(t *testing.T) {
	p, _, ctx := profileFixture(t)
	if err := p.register(ctx); err != nil {
		t.Fatal(err)
	}
	fd := socketFD(t)
	p.mu.Lock()
	p.file = fd
	p.mu.Unlock()
	if p.Release(":99.99") == nil {
		t.Fatal("外部发送者释放了自有Profile")
	}
	if err := p.Release(dbus.Sender(p.agent.owner)); err != nil {
		t.Fatal(err)
	}
	if _, err := unix.FcntlInt(uintptr(fd), unix.F_GETFD, 0); err != unix.EBADF {
		t.Fatal("BlueZ释放后FD泄漏", err)
	}
}

func TestIncomingProfileRealBusTransfersAndClosesOwnFD(t *testing.T) {
	p, f, ctx := profileFixture(t)
	if err := p.agent.conn.Export(p, incomingProfilePath, "org.bluez.Profile1"); err != nil {
		t.Fatal(err)
	}
	if err := p.register(ctx); err != nil {
		t.Fatal(err)
	}
	fd := socketFD(t)
	defer unix.Close(fd)
	phone := dbus.ObjectPath("/org/bluez/hci0/dev_02_11_22_33_44_55")
	err := f.bluez.Object(p.agent.conn.Names()[0], incomingProfilePath).CallWithContext(ctx,
		"org.bluez.Profile1.NewConnection", 0, phone, dbus.UnixFD(fd), map[string]dbus.Variant{}).Err
	if err != nil {
		t.Fatal(err)
	}
	p.mu.Lock()
	own := p.file
	p.mu.Unlock()
	if own < 0 || own == fd {
		t.Fatal("未通过总线取得独立FD", own, fd)
	}
	if err := p.unregister(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := unix.FcntlInt(uintptr(own), unix.F_GETFD, 0); err != unix.EBADF {
		t.Fatal("注销后自有FD仍开启", err)
	}
	if _, err := unix.FcntlInt(uintptr(fd), unix.F_GETFD, 0); err != nil {
		t.Fatal("注销误关了发送端FD", err)
	}
}

func TestIncomingProfileReleaseDuringRegistrationNeverRevivesLease(t *testing.T) {
	p, f, ctx := profileFixture(t)
	f.mu.Lock()
	f.onRegister = func(sender dbus.Sender) {
		if err := p.Release(dbus.Sender(p.agent.owner)); err != nil {
			t.Fatal(err)
		}
	}
	f.mu.Unlock()
	if p.register(ctx) == nil {
		t.Fatal("注册返回前已撤销却被报告就绪")
	}
	p.mu.Lock()
	active := p.registered || p.accepting
	p.mu.Unlock()
	if active {
		t.Fatal("已撤销的Profile被复活")
	}
}

func TestIncomingProfileCancelDuringFDValidationCannotRestorePending(t *testing.T) {
	p, f, ctx := profileFixture(t)
	if err := p.register(ctx); err != nil {
		t.Fatal(err)
	}
	reading, resume := make(chan struct{}), make(chan struct{})
	f.mu.Lock()
	f.beforePhoneRead = func() { close(reading); <-resume }
	f.mu.Unlock()
	fd := socketFD(t)
	done := make(chan *dbus.Error, 1)
	go func() {
		done <- p.NewConnection(dbus.Sender(p.agent.owner), "/org/bluez/hci0/dev_02_11_22_33_44_55", dbus.UnixFD(fd), nil)
	}()
	select {
	case <-reading:
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	if err := p.agent.Cancel(dbus.Sender(p.agent.owner)); err != nil {
		t.Fatal(err)
	}
	close(resume)
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("已取消的旧连接被重新接收")
		}
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	if p.agent.pending != "" {
		t.Fatal("取消后旧连接复活了登记目标")
	}
	if _, err := unix.FcntlInt(uintptr(fd), unix.F_GETFD, 0); err != unix.EBADF {
		t.Fatal("取消竞态FD泄漏", err)
	}
	if err := p.unregister(ctx); err != nil {
		t.Fatal(err)
	}
}

func TestIncomingProfileConcurrentCleanupConsumesOneLease(t *testing.T) {
	p, f, ctx := profileFixture(t)
	if err := p.register(ctx); err != nil {
		t.Fatal(err)
	}
	fd := socketFD(t)
	if err := p.NewConnection(dbus.Sender(p.agent.owner), "/org/bluez/hci0/dev_02_11_22_33_44_55", dbus.UnixFD(fd), nil); err != nil {
		t.Fatal(err)
	}
	if err := p.beginHandoff("/org/bluez/hci0/dev_02_11_22_33_44_55"); err != nil {
		t.Fatal(err)
	}
	started, resume := make(chan struct{}), make(chan struct{})
	f.mu.Lock()
	f.beforeUnregister = func() { close(started); <-resume }
	f.mu.Unlock()
	first, second := make(chan error, 1), make(chan error, 1)
	go func() { first <- p.unregister(ctx) }()
	select {
	case <-started:
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	if !p.handoffProtected() {
		t.Fatal("注销未确认就解除交接保护")
	}
	go func() { second <- p.unregister(ctx) }()
	select {
	case err := <-second:
		t.Fatal("并发清理绕过单次注销", err)
	case <-time.After(25 * time.Millisecond):
	}
	f.mu.Lock()
	calls := f.unregisterCalls
	f.mu.Unlock()
	if calls != 1 {
		t.Fatal("重复发起Profile注销", calls)
	}
	close(resume)
	for _, done := range []chan error{first, second} {
		select {
		case err := <-done:
			if err != nil {
				t.Fatal("已完成清理被误判失败", err)
			}
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		}
	}
	f.mu.Lock()
	calls = f.unregisterCalls
	f.mu.Unlock()
	if calls != 1 || p.handoffProtected() {
		t.Fatal("交接租约没有唯一消费", calls)
	}
	if _, err := unix.FcntlInt(uintptr(fd), unix.F_GETFD, 0); err != unix.EBADF {
		t.Fatal("自有FD没有清理", err)
	}
}

func TestIncomingProfileLateFDOnlyAcceptsLiveHandoffPhone(t *testing.T) {
	for _, mode := range []string{"current_phone", "other_phone", "canceled"} {
		t.Run(mode, func(t *testing.T) {
			p, _, ctx := profileFixture(t)
			if err := p.register(ctx); err != nil {
				t.Fatal(err)
			}
			phone := dbus.ObjectPath("/org/bluez/hci0/dev_02_11_22_33_44_55")
			if err := p.beginHandoff(phone); err != nil {
				t.Fatal(err)
			}
			taskCtx, cancel := context.WithCancel(ctx)
			defer cancel()
			p.agent.manager.mu.Lock()
			p.agent.manager.current = &pairingTask{incoming: true, status: TaskStatus{PhoneID: testPhone, Running: true}, confirmation: &Confirmation{ctx: taskCtx}}
			p.agent.manager.mu.Unlock()
			if mode == "other_phone" {
				phone = "/org/bluez/hci0/dev_02_11_22_33_44_66"
			}
			if mode == "canceled" {
				cancel()
			}
			fd := socketFD(t)
			err := p.NewConnection(dbus.Sender(p.agent.owner), phone, dbus.UnixFD(fd), nil)
			if (err == nil) != (mode == "current_phone") {
				t.Fatal("晚到连接门禁错误", mode, err)
			}
			if p.agent.pending != "" {
				t.Fatal("交接连接被当作新的登记任务")
			}
			if mode == "current_phone" {
				if _, err := unix.FcntlInt(uintptr(fd), unix.F_GETFD, 0); err != nil {
					t.Fatal("最后验证前关闭晚到连接", err)
				}
			}
			if err := p.unregister(ctx); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestIncomingProfileOldDisconnectCannotCloseNewConnection(t *testing.T) {
	p, f, ctx := profileFixture(t)
	if err := p.register(ctx); err != nil {
		t.Fatal(err)
	}
	phone := dbus.ObjectPath("/org/bluez/hci0/dev_02_11_22_33_44_55")
	reading, resume := make(chan struct{}), make(chan struct{})
	var calls atomic.Int32
	f.mu.Lock()
	f.phone["Connected"] = dbus.MakeVariant(false)
	f.beforePhoneRead = func() {
		if calls.Add(1) == 1 {
			close(reading)
			<-resume
		}
	}
	f.mu.Unlock()
	done := make(chan struct{})
	go func() { p.agent.disconnected(phone); close(done) }()
	select {
	case <-reading:
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	f.mu.Lock()
	f.phone["Connected"] = dbus.MakeVariant(true)
	f.mu.Unlock()
	fd := socketFD(t)
	if err := p.NewConnection(dbus.Sender(p.agent.owner), phone, dbus.UnixFD(fd), nil); err != nil {
		t.Fatal(err)
	}
	close(resume)
	select {
	case <-done:
	case <-ctx.Done():
		t.Fatal("连接与断开清理互相锁死")
	}
	if _, err := unix.FcntlInt(uintptr(fd), unix.F_GETFD, 0); err != nil {
		t.Fatal("旧事件关闭了新的自有FD", err)
	}
	p.agent.mu.Lock()
	pending := p.agent.pending
	p.agent.mu.Unlock()
	if pending != phone {
		t.Fatal("旧事件清除了新连接目标")
	}
	if err := p.unregister(ctx); err != nil {
		t.Fatal(err)
	}
}

func TestIncomingProfileSuccessfulFDBeforeRegisterReturnsPreservesLease(t *testing.T) {
	p, f, ctx := profileFixture(t)
	if err := p.agent.conn.Export(p, incomingProfilePath, "org.bluez.Profile1"); err != nil {
		t.Fatal(err)
	}
	fd := socketFD(t)
	defer unix.Close(fd)
	result := make(chan error, 1)
	f.mu.Lock()
	f.onRegister = func(sender dbus.Sender) {
		result <- f.bluez.Object(string(sender), incomingProfilePath).CallWithContext(ctx,
			"org.bluez.Profile1.NewConnection", 0, dbus.ObjectPath("/org/bluez/hci0/dev_02_11_22_33_44_55"), dbus.UnixFD(fd), map[string]dbus.Variant{}).Err
	}
	f.mu.Unlock()
	if err := p.register(ctx); err != nil {
		t.Fatal("正常早到FD被误当作注册撤销", err)
	}
	if err := <-result; err != nil {
		t.Fatal(err)
	}
	p.mu.Lock()
	active := p.registered && p.accepting
	own := p.file
	p.mu.Unlock()
	if !active || own < 0 {
		t.Fatal("注册后有效连接或租约丢失")
	}
	if _, err := unix.FcntlInt(uintptr(own), unix.F_GETFD, 0); err != nil {
		t.Fatal("早到FD未保留", err)
	}
	if err := p.unregister(ctx); err != nil {
		t.Fatal(err)
	}
}

func TestIncomingProfileCancelDuringRegisterInvalidatesLease(t *testing.T) {
	p, f, ctx := profileFixture(t)
	f.mu.Lock()
	f.onRegister = func(sender dbus.Sender) { _ = p.agent.Cancel(dbus.Sender(p.agent.owner)) }
	f.mu.Unlock()
	if p.register(ctx) == nil {
		t.Fatal("注册期间取消没有撤销本轮租约")
	}
	p.mu.Lock()
	accepting := p.accepting
	p.mu.Unlock()
	if accepting {
		t.Fatal("已取消的注册继续接收连接")
	}
	if err := p.unregister(ctx); err != nil {
		t.Fatal("取消后的实际注册没有清理", err)
	}
}
