package diagnostics

import (
	"context"
	"fmt"
	"strings"
	"time"
)

// BundleForSource 只收集所选类别的两次启动日志，避免分类下载混入其他服务。
func (m *Manager) BundleForSource(ctx context.Context, source string) (Bundle, error) {
	source = strings.ToLower(strings.TrimSpace(source))
	if !ValidSource(source) {
		return Bundle{}, fmt.Errorf("不支持的日志来源 %q", source)
	}
	if !m.bundleMu.TryLock() {
		return Bundle{}, fmt.Errorf("已有诊断报告正在生成")
	}
	defer m.bundleMu.Unlock()
	ctx, cancel := context.WithTimeout(ctx, bundleTimeout)
	defer cancel()
	generatedAt := time.Now()
	var report strings.Builder
	fmt.Fprintf(&report, "TeslaBox 分类诊断报告\n来源: %s\n生成时间: %s\n", source, generatedAt.Format(time.RFC3339))
	for _, boot := range []string{"0", "-1"} {
		writeSourceBootSection(ctx, m, &report, source, boot)
	}
	return Bundle{Filename: "teslabox-" + source + "-" + generatedAt.Format("20060102-150405") + ".txt", Content: []byte(report.String())}, nil
}

func writeSourceBootSection(ctx context.Context, manager *Manager, report *strings.Builder, source, boot string) {
	var keywords []string
	if source == SourceUSB {
		keywords = usbKeywords
	} else if source == SourceMedia {
		keywords = mediaKeywords
	}
	writeSection := writeBudgetSection
	if source == SourceNetwork {
		writeSection = writeRecentNetworkBudgetSection
	}
	writeSection(report, 128<<10, func(out *strings.Builder) {
		fmt.Fprintf(out, "\n===== 启动 %s 日志: %s =====\n", boot, source)
		for journalIndex, args := range journalArgs(source, maximumLimit) {
			for index, arg := range args {
				if arg == "--boot" {
					args[index] = "--boot=" + boot
				}
			}
			output, err := manager.runner.Run(ctx, "journalctl", args...)
			if err != nil {
				fmt.Fprintf(out, "[无法读取] %s\n", redactLine(sanitizeLine(err.Error())))
				continue
			}
			sectionKeywords := keywords
			if source == SourceCarPlay && journalIndex == 1 {
				sectionKeywords = []string{"carplay"}
			}
			writeSanitizedLines(out, string(output), sectionKeywords, maximumLimit)
		}
	})
}

// writeRecentNetworkBudgetSection 保留标题和最近网络记录，防止反复重试挤掉当前故障。
func writeRecentNetworkBudgetSection(report *strings.Builder, budget int, render func(*strings.Builder)) {
	var section strings.Builder
	render(&section)
	text := section.String()
	if len(text) <= budget {
		report.WriteString(text)
		return
	}
	headerEnd := strings.IndexByte(text[1:], '\n') + 2
	header := text[:headerEnd]
	marker := "[本区段预算不足，已丢弃较旧网络日志；保留最近完整记录，窗口不完整]\n"
	remaining := max(0, budget-len(header)-len(marker))
	start := max(headerEnd, len(text)-remaining)
	// 丢弃起始处的半行，确保中文和整条日志的边界完整。
	if start > headerEnd {
		if newline := strings.IndexByte(text[start:], '\n'); newline >= 0 {
			start += newline + 1
		} else {
			start = len(text)
		}
	}
	report.WriteString(header)
	report.WriteString(marker)
	report.WriteString(text[start:])
}
