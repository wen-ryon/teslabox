package nasupload

import (
	"context"
	"testing"
)

func TestServerAndQualitySavesAreIndependentAndPersistent(t *testing.T) {
	m, _, root := fixture(t, &fakeTransport{})
	target := addTarget(t, m)
	task, err := m.Create(Request{TargetID: target.ID}, "disk", root, "snapshot", inputs(t, root), nil, func() {})
	if err != nil {
		t.Fatal(err)
	}
	before := m.state.Targets[0]
	quality := Quality{Mode: "compressed", Height: 480, FPS: 15, BitrateKbps: 1500, Layout: "composite4a"}
	saved, err := m.SaveQuality(target.ID, quality)
	if err != nil || saved.Quality != quality || !saved.IsDefault || saved.Password != "" || saved.Credential != "" {
		t.Fatal("独立保存画质或脱敏失败", err)
	}
	want := before
	want.Quality = quality
	want.Revision = m.state.Targets[0].Revision
	if m.state.Targets[0] != want {
		t.Fatal("画质保存修改了服务器字段或凭据")
	}
	// 用较旧的服务器草稿保存，仍必须保留刚刚保存的新画质。
	target.Name = "更名后的服务器"
	target.Path = "share/new"
	target.Quality = Quality{Mode: "invalid-draft"}
	saved, err = m.SaveServer(context.Background(), target)
	if err != nil || saved.Quality != quality || saved.Name != target.Name || saved.Path != target.Path {
		t.Fatal("服务器保存覆盖画质，或未保存服务器字段", err)
	}
	if m.state.Targets[0].Credential != before.Credential {
		t.Fatal("留空密码时服务器保存丢失凭据")
	}
	if *m.History()[0].Request.Quality != *task.Request.Quality || task.Request.Quality.Mode != "original" {
		t.Fatal("独立保存改动了已创建任务的画质")
	}
	for _, test := range []struct {
		name, id string
		quality  Quality
	}{
		{name: "不存在的服务器", id: "missing", quality: quality},
		{name: "无效画质", id: target.ID, quality: Quality{Mode: "compressed", Height: 481}},
	} {
		t.Run(test.name, func(t *testing.T) {
			if _, err := m.SaveQuality(test.id, test.quality); err == nil {
				t.Fatal("无效保存被接受")
			}
		})
	}
	reopened, err := New(m.cfg)
	if err != nil {
		t.Fatal(err)
	}
	got, err := reopened.DefaultTarget()
	if err != nil || got.Quality != quality || got.Name != target.Name || got.Path != target.Path {
		t.Fatal("重启后服务器与画质未分别保留", err)
	}
}
