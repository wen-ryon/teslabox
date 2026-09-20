package teslacam

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestPersistedDurationRequiresCurrentFileProof(t *testing.T) {
	root := t.TempDir()
	directory := filepath.Join(root, "TeslaCam", "RecentClips")
	if err := os.MkdirAll(directory, 0755); err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(directory, "2026-09-10_10-00-00-front.mp4")
	if err := os.WriteFile(file, movieHeaderFixture(0, 1000, 17500), 0644); err != nil {
		t.Fatal(err)
	}
	scanner := NewScanner(time.UTC)
	videos, err := scanner.Scan(root)
	if err != nil {
		t.Fatal(err)
	}
	RefineVideoDurations(root, videos)
	cache := t.TempDir()
	if err := SaveIndexCache(cache, "disk", videos); err != nil {
		t.Fatal(err)
	}
	if err := SaveIndexCache(cache, "disk", videos); err != nil {
		t.Fatalf("原子替换失败: %v", err)
	}
	loaded, _, err := LoadIndexCache(cache, "disk")
	if err != nil {
		t.Fatal(err)
	}
	restarted := NewScanner(time.UTC)
	restarted.SeedCachedDurations(loaded)
	fresh, err := restarted.Scan(root)
	if err != nil {
		t.Fatal(err)
	}
	ApplyCachedDurations(fresh)
	if DurationKnown(fresh[0]) {
		t.Fatal("持久化时长必须按需核验")
	}
	// 未完成后台核验即保存和再次重启，也不能丢失上一轮的证据。
	if err := SaveIndexCache(cache, "disk", fresh); err != nil {
		t.Fatal(err)
	}
	preview, _, err := LoadIndexCache(cache, "disk")
	if err != nil {
		t.Fatal(err)
	}
	if preview[0].Duration != 17.5 || preview[0].DurationState != "cached" || DurationKnown(preview[0]) {
		t.Fatal("待核验缓存丢失或被误认为已核验")
	}
	restarted.SeedCachedDurations(preview)
	fresh, err = restarted.Scan(root)
	if err != nil {
		t.Fatal(err)
	}
	tes := append([]Video(nil), fresh...)
	ApplyPreviewDurations(tes)
	if tes[0].Duration != 17.5 || tes[0].DurationState != "cached" || DurationKnown(fresh[0]) {
		t.Fatal("列表预览污染真实时长状态")
	}
	RefineVideoDurations(root, fresh)
	if fresh[0].DurationState != "verified" || fresh[0].Duration != 17.5 {
		t.Fatalf("未复用核对通过的证据: %+v", fresh[0])
	}
	if err := os.WriteFile(file, movieHeaderFixture(0, 1000, 22500), 0644); err != nil {
		t.Fatal(err)
	}
	restarted.SeedCachedDurations(loaded)
	fresh, err = restarted.Scan(root)
	if err != nil {
		t.Fatal(err)
	}
	ApplyCachedDurations(fresh)
	if fresh[0].DurationState == "verified" {
		t.Fatal("同名同大小覆盖误用旧时长")
	}
	if err := os.Remove(file); err != nil {
		t.Fatal(err)
	}
	fresh, err = restarted.ScanWithProgress(context.Background(), root, nil)
	if err != nil || len(fresh) != 0 {
		t.Fatalf("车机删除的文件仍在新索引: %v %v", fresh, err)
	}
}

func TestIndexCacheRoundTripAndIsolation(t *testing.T) {
	dir := t.TempDir()
	videos := []Video{{ID: "one", Path: "TeslaCam/RecentClips/2026-09-10_10-00-00-front.mp4", RecordedAt: time.Now(), Camera: "front", ClipType: "recent", Duration: 17, DurationState: "verified"}}
	if err := SaveIndexCache(dir, "disk-a", videos); err != nil {
		t.Fatal(err)
	}
	got, _, err := LoadIndexCache(dir, "disk-a")
	if err != nil || len(got) != 1 || got[0].Duration != 17 {
		t.Fatalf("got=%v err=%v", got, err)
	}
	if _, _, err := LoadIndexCache(dir, "disk-b"); err == nil {
		t.Fatal("换盘仍使用旧缓存")
	}
	if err := os.WriteFile(filepath.Join(dir, "index.json"), []byte(`{"version":1}`), 0600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := LoadIndexCache(dir, "disk-a"); err == nil {
		t.Fatal("损坏缓存被接受")
	}
}

func TestIndexCacheRejectsUnsafePath(t *testing.T) {
	if err := SaveIndexCache(t.TempDir(), "disk", []Video{{Path: "../outside", Duration: 60}}); err == nil {
		t.Fatal("接受越界路径")
	}
}
