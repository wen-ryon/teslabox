package nasupload

import (
	"context"
	"sync/atomic"
	"testing"
)

func saveDefaultTestTarget(t *testing.T, m *Manager, name string) Target {
	t.Helper()
	target, err := m.SaveTarget(context.Background(), Target{Name: name, Protocol: "smb", Host: "localhost", User: "test", Password: "test-only", Path: "share/" + name, Quality: Quality{Mode: "original"}})
	if err != nil {
		t.Fatal(err)
	}
	return target
}

func TestDefaultTargetRequiresExplicitSelectionAndPersists(t *testing.T) {
	m, _, root := fixture(t, &fakeTransport{})
	a := saveDefaultTestTarget(t, m, "a")
	var released atomic.Int32
	if _, err := m.DefaultTarget(); err == nil {
		t.Fatal("唯一目标不应自动成为默认目标")
	}
	if _, err := m.Create(Request{TargetID: a.ID}, "disk", root, "s", inputs(t, root), nil, func() { released.Add(1) }); err == nil || released.Load() != 1 {
		t.Fatal("未配置默认目标时应拒绝上传并释放快照")
	}
	if _, err := m.SetDefaultTarget("missing"); err == nil {
		t.Fatal("不存在的默认目标被接受")
	}
	if _, err := m.SetDefaultTarget(a.ID); err != nil {
		t.Fatal(err)
	}
	reopened, err := New(m.cfg)
	if err != nil {
		t.Fatal(err)
	}
	got, err := reopened.DefaultTarget()
	if err != nil || got.ID != a.ID || !got.IsDefault || got.Credential != "" || got.Password != "" {
		t.Fatalf("默认目标恢复或脱敏失败：%v", err)
	}
	if list := reopened.Targets(); len(list) != 1 || !list[0].IsDefault {
		t.Fatal("配置列表未标记默认目标")
	}
}

func TestDefaultTargetSwitchCannotRerouteQueuedTasks(t *testing.T) {
	f := &fakeTransport{}
	m, _, root := fixture(t, f)
	a, b := saveDefaultTestTarget(t, m, "a"), saveDefaultTestTarget(t, m, "b")
	files := inputs(t, root)
	if _, err := m.SetDefaultTarget(a.ID); err != nil {
		t.Fatal(err)
	}
	first, err := m.Create(Request{}, "disk", root, "s", files, nil, func() {})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = m.SetDefaultTarget(b.ID); err != nil {
		t.Fatal(err)
	}
	var released atomic.Int32
	if _, err = m.Create(Request{TargetID: a.ID}, "disk", root, "s", files, nil, func() { released.Add(1) }); err == nil || released.Load() != 1 {
		t.Fatal("旧目标仍可绕过默认目标")
	}
	second, err := m.Create(Request{}, "disk", root, "s", files, nil, func() {})
	if err != nil {
		t.Fatal(err)
	}
	if first.Request.TargetID != a.ID || second.Request.TargetID != b.ID {
		t.Fatal("新任务未使用创建时的默认目标")
	}
	runManager(t, m)
	waitState(t, m, first.ID, "completed")
	waitState(t, m, second.ID, "completed")
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.targets) != 4 || f.targets[0].ID != a.ID || f.targets[1].ID != a.ID || f.targets[2].ID != b.ID || f.targets[3].ID != b.ID {
		t.Fatal("切换默认目标后已排队任务发生改道")
	}
}

func TestDeletingDefaultTargetDoesNotChooseAnother(t *testing.T) {
	m, _, _ := fixture(t, &fakeTransport{})
	a, b := saveDefaultTestTarget(t, m, "a"), saveDefaultTestTarget(t, m, "b")
	if _, err := m.SetDefaultTarget(b.ID); err != nil {
		t.Fatal(err)
	}
	if err := m.DeleteTarget(b.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := m.DefaultTarget(); err == nil {
		t.Fatal("删除默认目标后不应自动选中其他目标")
	}
	if targets := m.Targets(); len(targets) != 1 || targets[0].ID != a.ID || targets[0].IsDefault {
		t.Fatal("剩余配置或默认标记异常")
	}
	reopened, err := New(m.cfg)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = reopened.DefaultTarget(); err == nil {
		t.Fatal("默认目标清除没有持久化")
	}
}
