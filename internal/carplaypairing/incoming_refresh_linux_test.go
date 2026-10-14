//go:build linux

package carplaypairing

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/godbus/dbus/v5"
	"golang.org/x/sys/unix"
)

type refreshFixture struct {
	mu      sync.Mutex
	calls   int
	hook    func()
	succeed bool
}

// ConnectProfile 模拟真实手机 iAP 解析，方法错误与属性刷新是独立证据。
func (f *refreshFixture) ConnectProfile(uuid string) *dbus.Error {
	if uuid != incomingPhoneProfileUUID {
		return rejected()
	}
	f.mu.Lock()
	f.calls++
	hook := f.hook
	succeed := f.succeed
	f.mu.Unlock()
	if hook != nil {
		hook()
	}
	if succeed {
		return nil
	}
	return &dbus.Error{Name: "org.bluez.Error.NotAvailable"}
}

func refreshTestFixture(t *testing.T) (*incomingProfile, *incomingFixture, *refreshFixture, context.Context, dbus.ObjectPath) {
	t.Helper()
	p, f, ctx := profileFixture(t)
	phone := dbus.ObjectPath("/org/bluez/hci0/dev_02_11_22_33_44_55")
	r := &refreshFixture{}
	if err := f.bluez.Export(r, phone, "org.bluez.Device1"); err != nil {
		t.Fatal(err)
	}
	if err := p.register(ctx); err != nil {
		t.Fatal(err)
	}
	if err := p.NewConnection(dbus.Sender(p.agent.owner), phone, dbus.UnixFD(socketFD(t)), nil); err != nil {
		t.Fatal(err)
	}
	p.mu.Lock()
	p.acceptedAt = time.Now().Add(-3 * time.Second)
	p.mu.Unlock()
	t.Cleanup(func() {
		cleanup, cancel := context.WithTimeout(context.WithoutCancel(ctx), time.Second)
		defer cancel()
		if err := p.unregister(cleanup); err != nil {
			t.Error(err)
		}
	})
	return p, f, r, ctx, phone
}

func refreshProps(t *testing.T, p *incomingProfile, phone dbus.ObjectPath) map[string]dbus.Variant {
	t.Helper()
	props, err := phoneProperties(p.agent.ctx, p.agent.conn, p.agent.owner, phone)
	if err != nil {
		t.Fatal(err)
	}
	return props
}

func waitRefreshTest(t *testing.T, p *incomingProfile) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := p.waitRefresh(ctx); err != nil {
		t.Fatal("解析未随取消或期限及时退出", err)
	}
}

func TestIncomingRefreshOnceAndErrorCannotReplaceGate(t *testing.T) {
	p, f, r, _, phone := refreshTestFixture(t)
	r.mu.Lock()
	r.hook = func() {
		f.mu.Lock()
		f.phone["ServicesResolved"] = dbus.MakeVariant(true)
		f.mu.Unlock()
	}
	r.mu.Unlock()
	props := refreshProps(t, p, phone)
	for i := 0; i < 20; i++ {
		p.refreshServices(phone, props, time.Now())
	}
	waitRefreshTest(t, p)
	r.mu.Lock()
	calls := r.calls
	r.mu.Unlock()
	if calls != 1 || !incomingLinkReady(refreshProps(t, p, phone)) || p.agent.manager.Status().State != "idle" {
		t.Fatal("重复调用、忽略真实属性或直接伪造交接", calls)
	}
}

func TestIncomingRefreshNaturalResolutionNeedsNoCall(t *testing.T) {
	p, f, r, _, phone := refreshTestFixture(t)
	p.mu.Lock()
	p.acceptedAt = time.Now()
	p.mu.Unlock()
	p.refreshServices(phone, refreshProps(t, p, phone), time.Now())
	f.mu.Lock()
	f.phone["ServicesResolved"] = dbus.MakeVariant(true)
	f.mu.Unlock()
	p.refreshServices(phone, refreshProps(t, p, phone), time.Now().Add(3*time.Second))
	waitRefreshTest(t, p)
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.calls != 0 {
		t.Fatal("自然解析被额外连接打断")
	}
}

func TestIncomingRefreshMethodSuccessWithoutPropertiesCannotHandoff(t *testing.T) {
	p, _, r, _, phone := refreshTestFixture(t)
	r.mu.Lock()
	r.succeed = true
	r.mu.Unlock()
	p.refreshServices(phone, refreshProps(t, p, phone), time.Now())
	waitRefreshTest(t, p)
	if incomingLinkReady(refreshProps(t, p, phone)) || p.agent.manager.Status().State != "idle" {
		t.Fatal("方法返回成功被当成服务完成或登记成功")
	}
}

func TestIncomingRefreshRejectsMissingOwnership(t *testing.T) {
	for _, state := range []string{"no_pending", "expired", "no_fd", "disconnected", "unpaired", "wrong_identity", "handoff", "canceled"} {
		t.Run(state, func(t *testing.T) {
			p, f, r, ctx, phone := refreshTestFixture(t)
			switch state {
			case "no_pending", "expired":
				p.agent.mu.Lock()
				if state == "no_pending" {
					p.agent.pending = ""
				} else {
					p.agent.deadline = time.Now().Add(-time.Second)
				}
				p.agent.mu.Unlock()
			case "no_fd":
				p.dropConnection()
			case "disconnected", "unpaired", "wrong_identity":
				f.mu.Lock()
				key := "Connected"
				if state == "unpaired" {
					key = "Paired"
				}
				f.phone[key] = dbus.MakeVariant(false)
				if state == "wrong_identity" {
					f.phone["Address"] = dbus.MakeVariant("02:00:00:00:00:99")
				}
				f.mu.Unlock()
			case "handoff":
				if err := p.beginHandoff(phone); err != nil {
					t.Fatal(err)
				}
			case "canceled":
				canceled, cancel := context.WithCancel(ctx)
				cancel()
				p.agent.ctx = canceled
			}
			props, _ := phoneProperties(ctx, p.agent.conn, p.agent.owner, phone)
			p.refreshServices(phone, props, time.Now())
			waitRefreshTest(t, p)
			r.mu.Lock()
			defer r.mu.Unlock()
			if r.calls != 0 {
				t.Fatal("没有本轮所有权仍请求解析")
			}
		})
	}
}

func TestIncomingRefreshCancelAndLateReplyDoNotCloseNewFD(t *testing.T) {
	p, _, r, _, phone := refreshTestFixture(t)
	started, release := make(chan struct{}), make(chan struct{})
	r.mu.Lock()
	r.hook = func() { close(started); <-release }
	r.mu.Unlock()
	p.refreshServices(phone, refreshProps(t, p, phone), time.Now())
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("未请求解析")
	}
	p.cancelConnection()
	waitRefreshTest(t, p)
	fd := socketFD(t)
	if err := p.NewConnection(dbus.Sender(p.agent.owner), phone, dbus.UnixFD(fd), nil); err != nil {
		t.Fatal(err)
	}
	close(release)
	r.mu.Lock()
	r.hook = nil
	r.mu.Unlock()
	p.mu.Lock()
	p.acceptedAt = time.Now().Add(-3 * time.Second)
	p.mu.Unlock()
	p.refreshServices(phone, refreshProps(t, p, phone), time.Now())
	waitRefreshTest(t, p)
	r.mu.Lock()
	calls := r.calls
	r.mu.Unlock()
	if calls != 2 {
		t.Fatal("旧代次拦截了新代次的唯一解析", calls)
	}
	if _, err := unix.FcntlInt(uintptr(fd), unix.F_GETFD, 0); err != nil || p.agent.manager.Status().State != "idle" {
		t.Fatal("旧解析影响新FD或绕过交接", err)
	}
}

func TestIncomingRefreshDeadlineDoesNotRetryOrRevivePending(t *testing.T) {
	p, _, r, _, phone := refreshTestFixture(t)
	started, release := make(chan struct{}), make(chan struct{})
	r.mu.Lock()
	r.hook = func() { close(started); <-release }
	r.mu.Unlock()
	p.agent.mu.Lock()
	p.agent.deadline = time.Now().Add(100 * time.Millisecond)
	p.agent.mu.Unlock()
	p.refreshServices(phone, refreshProps(t, p, phone), time.Now())
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("未请求解析")
	}
	waitRefreshTest(t, p)
	close(release)
	p.refreshServices(phone, refreshProps(t, p, phone), time.Now())
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.calls != 1 {
		t.Fatal("到期后重试解析", r.calls)
	}
}

func TestIncomingRefreshStopsBeforeHandoffReleaseOrUnregister(t *testing.T) {
	for _, action := range []string{"handoff", "release", "unregister", "owner_context"} {
		t.Run(action, func(t *testing.T) {
			p, f, r, ctx, phone := refreshTestFixture(t)
			started, release := make(chan struct{}), make(chan struct{})
			defer close(release)
			r.mu.Lock()
			r.hook = func() { close(started); <-release }
			r.mu.Unlock()
			ownerCtx, cancelOwner := context.WithCancel(ctx)
			defer cancelOwner()
			p.agent.ctx = ownerCtx
			p.refreshServices(phone, refreshProps(t, p, phone), time.Now())
			select {
			case <-started:
			case <-time.After(time.Second):
				t.Fatal("解析未启动")
			}
			switch action {
			case "handoff":
				if err := p.beginHandoff(phone); err != nil {
					t.Fatal(err)
				}
			case "release":
				f.mu.Lock()
				f.profile = ""
				f.adapter["UUIDs"] = dbus.MakeVariant([]string{})
				f.mu.Unlock()
				if err := p.Release(dbus.Sender(p.agent.owner)); err != nil {
					t.Fatal(err)
				}
			case "unregister":
				if err := p.unregister(ctx); err != nil {
					t.Fatal(err)
				}
			case "owner_context":
				cancelOwner()
			}
			waitRefreshTest(t, p)
			if p.agent.manager.Status().State != "idle" {
				t.Fatal("取消解析被当作登记完成")
			}
		})
	}
}

func TestIncomingReleasedHandoffRejectsLateFD(t *testing.T) {
	p, f, _, ctx, phone := refreshTestFixture(t)
	if err := p.beginHandoff(phone); err != nil {
		t.Fatal(err)
	}
	confirmation, err := NewConfirmation(ctx, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer confirmation.cancel()
	p.agent.manager.mu.Lock()
	p.agent.manager.current = &pairingTask{incoming: true, confirmation: confirmation, status: TaskStatus{PhoneID: testPhone, Running: true}}
	p.agent.manager.mu.Unlock()
	f.mu.Lock()
	f.profile = ""
	f.adapter["UUIDs"] = dbus.MakeVariant([]string{})
	f.mu.Unlock()
	if err := p.Release(dbus.Sender(p.agent.owner)); err != nil {
		t.Fatal(err)
	}
	fd := socketFD(t)
	if p.NewConnection(dbus.Sender(p.agent.owner), phone, dbus.UnixFD(fd), nil) == nil {
		t.Fatal("释放后仍接收交接迟到FD")
	}
	if _, err := unix.FcntlInt(uintptr(fd), unix.F_GETFD, 0); err != unix.EBADF {
		t.Fatal("拒绝迟到FD时泄漏描述符", err)
	}
}

func TestIncomingCancelClearsOldPendingBeforeAdmittingNewFD(t *testing.T) {
	p, _, _, _, phone := refreshTestFixture(t)
	p.mu.Lock()
	oldFD := p.file
	p.mu.Unlock()
	p.agent.mu.Lock()
	canceled := make(chan struct{})
	go func() { _ = p.agent.Cancel(dbus.Sender(p.agent.owner)); close(canceled) }()
	limit := time.Now().Add(time.Second)
	for p.mu.TryLock() {
		p.mu.Unlock()
		if time.Now().After(limit) {
			p.agent.mu.Unlock()
			t.Fatal("取消未取得Profile锁")
		}
		time.Sleep(time.Millisecond)
	}
	_, err := unix.FcntlInt(uintptr(oldFD), unix.F_GETFD, 0)
	if err != nil {
		p.agent.mu.Unlock()
		t.Fatal("尚未取得Agent锁就关闭了旧FD", err)
	}
	p.agent.mu.Unlock()
	<-canceled
	fd := socketFD(t)
	if err := p.NewConnection(dbus.Sender(p.agent.owner), phone, dbus.UnixFD(fd), nil); err != nil {
		t.Fatal(err)
	}
	p.agent.mu.Lock()
	pending := p.agent.pending
	p.agent.mu.Unlock()
	if pending != phone {
		t.Fatal("取消尾部清除了新连接pending")
	}
}

func TestIncomingLateAuthorizationCannotReviveCanceledLease(t *testing.T) {
	for _, action := range []string{"cancel", "release"} {
		t.Run(action, func(t *testing.T) {
			p, f, _, _, phone := refreshTestFixture(t)
			started, release := make(chan struct{}), make(chan struct{})
			f.mu.Lock()
			f.beforePhoneRead = func() { close(started); <-release }
			f.mu.Unlock()
			done := make(chan *dbus.Error, 1)
			go func() { done <- p.agent.RequestConfirmation(dbus.Sender(p.agent.owner), phone, 123456) }()
			select {
			case <-started:
			case <-time.After(time.Second):
				t.Fatal("授权未进入属性读取")
			}
			if action == "cancel" {
				if err := p.agent.Cancel(dbus.Sender(p.agent.owner)); err != nil {
					t.Fatal(err)
				}
			} else {
				f.mu.Lock()
				f.profile = ""
				f.adapter["UUIDs"] = dbus.MakeVariant([]string{})
				f.mu.Unlock()
				if err := p.Release(dbus.Sender(p.agent.owner)); err != nil {
					t.Fatal(err)
				}
			}
			close(release)
			if err := <-done; err == nil {
				t.Fatal("旧租约迟到授权被接受")
			}
			p.agent.mu.Lock()
			pending := p.agent.pending
			p.agent.mu.Unlock()
			if pending != "" {
				t.Fatal("迟到授权复活了pending")
			}
		})
	}
}
