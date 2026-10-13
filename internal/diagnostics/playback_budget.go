package diagnostics

import (
	"encoding/json"
	"fmt"
	"strings"
)

const playbackTraceMarker = `"event":"playback_trace"`

// writePlaybackTraceBudget 仅已知重复采样可降级，新增事件默认按核心证据处理。
func writePlaybackTraceBudget(report *strings.Builder, output string, budget int) {
	type record struct {
		line                    string
		auxiliary               bool
		seq                     int64
		operation, span, reason string
	}
	var records []record
	unknown, sourceLoss, malformed := 0, 0, 0
	for _, line := range strings.Split(output, "\n") {
		if !strings.Contains(line, playbackTraceMarker) {
			continue
		}
		var envelope struct {
			Trace struct {
				Phase     string `json:"phase"`
				Seq       int64  `json:"event_seq"`
				Operation string `json:"operation_id"`
				Span      string `json:"span_id"`
				Reason    string `json:"reason_code"`
			} `json:"trace"`
			Dropped int `json:"dropped_events"`
		}
		start := strings.IndexByte(line, '{')
		valid := start >= 0 && json.Unmarshal([]byte(line[start:]), &envelope) == nil
		phase := envelope.Trace.Phase
		known := false
		switch phase {
		case "buffer_sample", "lifecycle", "metadata", "seek", "play", "stream_prepare", "hls_response", "media_event", "first_frame", "frame_progress", "snapshot_check", "snapshot_refresh", "user_action", "pause_request", "switch_baseline", "switch_result", "source_result", "history_baseline", "progress_state":
			known = valid
		}
		if !known {
			unknown++
		}
		sourceLoss += envelope.Dropped
		clean := redactLine(sanitizeLine(line))
		if !valid || len(line) > maximumLine {
			malformed++
		}
		if !known {
			clean = "[unclassified/core] " + clean
		}
		records = append(records, record{clean, valid && phase == "buffer_sample", envelope.Trace.Seq, envelope.Trace.Operation, envelope.Trace.Span, envelope.Trace.Reason})
	}
	selected := make([]bool, len(records))
	remaining, coreDropped, samplesDropped := max(0, budget-2048), 0, 0
	// 先保留最近核心，再用剩余额度容纳采样，输出仍按原始日志顺序。
	for _, auxiliary := range []bool{false, true} {
		for i := len(records) - 1; i >= 0; i-- {
			r := records[i]
			if r.auxiliary != auxiliary {
				continue
			}
			if len(r.line)+1 <= remaining {
				selected[i] = true
				remaining -= len(r.line) + 1
			} else if auxiliary {
				samplesDropped++
			} else {
				coreDropped++
			}
		}
	}
	status := "collected_window_only"
	if coreDropped > 0 || sourceLoss > 0 || malformed > 0 {
		status = "incomplete"
	}
	fmt.Fprintf(report, "\n===== 最近 30 分钟逐路播放阶段诊断 =====\n完整性: %s；采集=%d；核心裁剪=%d；辅助采样裁剪=%d；未分类核心=%d；客户端已报告丢失=%d。\n", status, len(records), coreDropped, samplesDropped, unknown, sourceLoss)
	report.WriteString("采集边界: 当前启动、最近 30 分钟、journal 最多 20000 行；不保证窗口之前或未上报历史完整。序号按 operation_id 分组核对，不跨页面相减；缺失配对不能推断阶段耗时。\n")
	first, last := int64(0), int64(0)
	for i, r := range records {
		if selected[i] {
			if first == 0 {
				first = r.seq
			}
			last = r.seq
		}
	}
	fmt.Fprintf(report, "保留首/末记录 event_seq=%d/%d（可能来自不同操作）；逐行身份和序号供复核。\n", first, last)
	pairs := map[string]int{}
	for i, r := range records {
		if !selected[i] || r.span == "" {
			continue
		}
		key := r.operation + ":" + r.span
		if r.reason == "started" {
			pairs[key] |= 1
		} else {
			pairs[key] |= 2
		}
	}
	unpaired := 0
	for _, mask := range pairs {
		if mask != 3 {
			unpaired++
		}
	}
	fmt.Fprintf(report, "保留窗口未配对 span=%d（进行中或缺失，不能直接判超时）；损坏/超长记录=%d。旧版无 span_id 的阶段不做自动配对。\n", unpaired, malformed)
	for i, r := range records {
		if selected[i] {
			report.WriteString(r.line)
			report.WriteByte('\n')
		}
	}
	if len(records) == 0 {
		report.WriteString("[没有逐路阶段记录，请确认刷新了诊断版页面并实际播放]\n")
	}
}
