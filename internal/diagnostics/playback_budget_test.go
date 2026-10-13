package diagnostics

import (
	"fmt"
	"strings"
	"testing"
)

func TestPlaybackBudgetUnknownAndCorePriority(t *testing.T) {
	t.Parallel()
	var input, report strings.Builder
	input.WriteString(`{"event":"playback_trace","trace":{"phase":"future_event","event_seq":1}}` + "\n")
	for i := 2; i < 300; i++ {
		fmt.Fprintf(&input, `{"event":"playback_trace","trace":{"phase":"buffer_sample","event_seq":%d}}`+"\n", i)
	}
	writePlaybackTraceBudget(&report, input.String(), 4096)
	if !strings.Contains(report.String(), "[unclassified/core]") || !strings.Contains(report.String(), "future_event") || strings.Contains(report.String(), "辅助采样裁剪=0") {
		t.Fatal(report.String())
	}
	if report.Len() > 4096 {
		t.Fatal("budget exceeded")
	}
}

func TestPlaybackBudgetCoreOverflowExplicit(t *testing.T) {
	t.Parallel()
	var report strings.Builder
	writePlaybackTraceBudget(&report, strings.Repeat(`{"event":"playback_trace","trace":{"phase":"seek","reason_code":"started","span_id":"s","operation_id":"o"}}`+"\n", 100), 4096)
	if !strings.Contains(report.String(), "完整性: incomplete") || !strings.Contains(report.String(), "未配对 span=1") {
		t.Fatal(report.String())
	}
}

func TestBundleBudgetsPreserveLaterSections(t *testing.T) {
	t.Parallel()
	line := `{"event":"playback_trace","trace":{"phase":"seek"}}`
	bundle, err := New(&fakeRunner{output: strings.Repeat(line+"\n", 2000) + "XFS I/O error\n"}).Bundle(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	text := string(bundle.Content)
	if len(bundle.Content) > maximumBundle || !strings.Contains(text, "上一次启动关键故障日志") || !strings.Contains(text, "当前启动日志: network") || !strings.Contains(text, "XFS I/O error") {
		t.Fatal("later evidence lost")
	}
	if strings.Count(text, line) != 2000 {
		t.Fatalf("trace duplicated or lost: %d", strings.Count(text, line))
	}
}
