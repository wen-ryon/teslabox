package nasupload

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func TestRecordsCountsVerifiedRemoteFilesAndKeepsHistory(t *testing.T) {
	m, _, _ := fixture(t, &fakeTransport{})
	target := saveDefaultTestTarget(t, m, "a")
	if _, err := m.SetDefaultTarget(target.ID); err != nil {
		t.Fatal(err)
	}
	q := Quality{Mode: "original"}
	merged := Quality{Mode: "compressed", Height: 480, FPS: 15, BitrateKbps: 1500, Layout: "composite4a"}
	at := time.Date(2026, 10, 2, 12, 0, 0, 0, time.UTC)
	add := func(identity, state string, quality Quality, file FileResult) {
		m.state.Jobs = append(m.state.Jobs, &job{Identity: identity, Target: target, Task: Task{ID: newID(), State: state, TargetName: target.Name, Request: Request{TargetID: target.ID, Quality: &quality}, Files: []FileResult{file}}})
	}
	front := FileResult{MomentID: "minute", Cameras: []string{"front"}, Name: "front.mp4", RemotePath: "share/front.mp4", VerifiedAt: at, RecordedAt: at}
	add("disk", "completed", q, front)
	front.VerifiedAt = at.Add(time.Minute)
	add("disk", "waiting_retry", q, front)
	add("disk", "canceled", q, FileResult{MomentID: "minute", Cameras: []string{"back"}, Name: "back.mp4", RemotePath: "share/back.mp4", VerifiedAt: at, RecordedAt: at})
	add("disk", "completed", merged, FileResult{MomentID: "minute", Cameras: []string{"front", "back", "left_repeater", "right_repeater"}, Name: "merged.mp4", RemotePath: "share/merged.mp4", VerifiedAt: at, RecordedAt: at})
	add("other-disk", "completed", q, FileResult{RemotePath: "share/other.mp4", VerifiedAt: at})
	add("disk", "failed", q, FileResult{RemotePath: "share/unverified.mp4"})
	if err := m.saveLocked(); err != nil {
		t.Fatal(err)
	}
	reopened, err := New(m.cfg)
	if err != nil {
		t.Fatal(err)
	}
	files := reopened.Records("disk")
	if len(files) != 3 || files[0].Name != "front.mp4" || !files[0].VerifiedAt.Equal(front.VerifiedAt) {
		t.Fatalf("重复核验、部分成功、合成计数或恢复错误：%+v", files)
	}
	for _, file := range files {
		if !file.IsDefaultTarget || file.TargetID != target.ID {
			t.Fatal("默认目标记录未标记")
		}
	}
	files[0].Cameras[0] = "mutated"
	if reopened.Records("disk")[0].Cameras[0] != "front" {
		t.Fatal("返回结果污染了历史")
	}
	encoded, _ := json.Marshal(files)
	if strings.Contains(string(encoded), "credential") || strings.Contains(string(encoded), "password") {
		t.Fatal("文件记录暴露凭据")
	}
	target.Password = ""
	target.Name = "新名称"
	target.Quality = merged
	if _, err = reopened.SaveTarget(context.Background(), target); err != nil {
		t.Fatal(err)
	}
	if !reopened.Records("disk")[0].IsDefaultTarget {
		t.Fatal("名称和画质变更不应改变远端位置")
	}
	target.Path = "share/new"
	if _, err = reopened.SaveTarget(context.Background(), target); err != nil {
		t.Fatal(err)
	}
	if history := reopened.Records("disk"); len(history) != 3 || history[0].IsDefaultTarget {
		t.Fatal("变更目录后应保留历史，但不能当作新目录的成功记录")
	}
}
