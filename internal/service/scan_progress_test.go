package service

import "testing"

func TestScanProgressIsIndependentOfPublishedIndex(t *testing.T) {
	manager := &Manager{state: Snapshot{VideoCount: 99}}
	manager.setScanProgress("scanning", 64)
	result := manager.Snapshot()
	if result.VideoCount != 99 || result.ScanProgress.Discovered != 64 {
		t.Fatalf("state=%+v", result)
	}
	result.ScanProgress.Discovered = 1000
	if manager.Snapshot().ScanProgress.Discovered != 64 {
		t.Fatal("进度副本可修改内部状态")
	}
	manager.setScanProgress("", 0)
	if manager.Snapshot().ScanProgress != nil {
		t.Fatal("扫描结束仍有进度")
	}
}
