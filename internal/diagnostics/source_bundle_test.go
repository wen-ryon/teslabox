package diagnostics

import (
	"strings"
	"testing"
)

func TestCarPlayCategoryAndDownloadStayScopedAndRedacted(t *testing.T) {
	runner := &fakeRunner{output: "BRIDGE_RENDER rendered=60 token=private\n"}
	manager := New(runner)
	logs, err := manager.Logs(t.Context(), SourceCarPlay, 50)
	if err != nil || len(logs.Lines) != 1 || strings.Contains(logs.Lines[0], "private") {
		t.Fatalf("日志读取或脱敏失败: %#v %v", logs, err)
	}
	bundle, err := manager.BundleForSource(t.Context(), SourceCarPlay)
	if err != nil || strings.Contains(string(bundle.Content), "private") {
		t.Fatalf("分类报告失败: %v", err)
	}
	if !strings.Contains(string(bundle.Content), "启动 -1") || !strings.Contains(bundle.Filename, "carplay") {
		t.Fatal("缺少上次启动日志或分类文件名")
	}
	for _, call := range runner.calls {
		if (!strings.Contains(call, "--unit teslabox-carplay-experiment.service") && !strings.HasSuffix(call, "--unit teslabox.service")) || strings.Contains(call, "--dmesg") {
			t.Fatalf("CarPlay 分类混入其他日志: %s", call)
		}
	}
	if _, err := manager.BundleForSource(t.Context(), "../../etc/passwd"); err == nil {
		t.Fatal("非法来源没有被拒绝")
	}
	full, err := manager.Bundle(t.Context())
	if err != nil || !strings.Contains(string(full.Content), "启动 -1 日志: carplay") || strings.Contains(string(full.Content), "token=private") {
		t.Fatalf("完整报告缺少 CarPlay 上次启动或脱敏失败: %v", err)
	}
}

func TestCarPlayCategoryIncludesWebProbeWithoutUnrelatedMainLogs(t *testing.T) {
	runner := &fakeRunner{output: "CarPlay 车机联网检测已处理 http_status=200 token=private\n普通录像日志\n"}
	manager := New(runner)
	logs, err := manager.Logs(t.Context(), SourceCarPlay, 50)
	if err != nil {
		t.Fatal(err)
	}
	mainArgs := journalArgs(SourceCarPlay, 51)[1]
	if !strings.HasSuffix(strings.Join(mainArgs, " "), "--unit teslabox.service") {
		t.Fatal("分类没有采集主程序的 CarPlay 入口日志")
	}
	bundle, err := manager.BundleForSource(t.Context(), SourceCarPlay)
	if err != nil || strings.Contains(string(bundle.Content), "private") {
		t.Fatalf("分类报告失败: %v", err)
	}
	// 独立服务输出保留；主服务中的普通日志不能再出现第二次。
	if strings.Count(string(bundle.Content), "普通录像日志") != 2 ||
		strings.Count(string(bundle.Content), "CarPlay 车机联网检测已处理") != 4 || len(logs.Lines) != 2 {
		t.Fatalf("主程序日志过滤失效: %#v %s", logs.Lines, bundle.Content)
	}
}
