package diagnostics

import (
	"fmt"
	"strings"
)

// writeBudgetSection 为各证据源独立留额，某一路爆量不能吞掉后面的诊断。
func writeBudgetSection(report *strings.Builder, limit int, render func(*strings.Builder)) {
	var section strings.Builder
	render(&section)
	text := section.String()
	if len(text) <= limit {
		report.WriteString(text)
		return
	}
	cut := strings.LastIndexByte(text[:max(0, limit-256)], '\n')
	if cut >= 0 {
		report.WriteString(text[:cut+1])
	}
	fmt.Fprintf(report, "[报告已达到大小上限（本区段预算）；窗口不完整，原始字节=%d，预算=%d；其他区段继续保留]\n", len(text), limit)
}
