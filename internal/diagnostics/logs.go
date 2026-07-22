// Package diagnostics 提供受限且脱敏的设备诊断日志读取能力。
package diagnostics

import (
	"context"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode"

	"github.com/wen-ryon/teslabox/internal/gadget"
	"github.com/wen-ryon/teslabox/internal/system"
)

const (
	// SourceApplication 表示 TeslaBox 主服务和更新服务日志。
	SourceApplication = "application"
	// SourceUSB 表示与 USB Gadget 枚举有关的本次启动内核日志。
	SourceUSB = "usb"
	// SourceNetwork 表示 NetworkManager 和 TeslaBox AP 服务日志。
	SourceNetwork = "network"

	defaultLimit = 200
	maximumLimit = 500
	maximumLine  = 4096
)

var (
	secretAssignment = regexp.MustCompile(`(?i)(\b(?:password|passwd|psk|token|secret|authorization|cookie)\b["']?)(\s*[:=]\s*|\s+)("[^"]*"|'[^']*'|[^\s,;]+)`)
	usbKeywords      = []string{"usb", "dwc3", "udc", "gadget", "configfs", "mass_storage", "mass storage", "lun", "fcc00000"}
)

// Report 是日志中心可以安全展示的有界日志结果。
type Report struct {
	Source        string                `json:"source"`
	GeneratedAt   time.Time             `json:"generated_at"`
	Lines         []string              `json:"lines"`
	Limited       bool                  `json:"limited"`
	Redacted      int                   `json:"redacted"`
	ServiceStates []gadget.ServiceState `json:"service_states,omitempty"`
}

// Manager 只允许读取预定义的 journalctl 日志集合，避免页面成为任意命令入口。
type Manager struct {
	runner system.Runner
}

// New 创建使用系统 journalctl 的诊断日志管理器。
func New(runner system.Runner) *Manager {
	return &Manager{runner: runner}
}

// Logs 返回指定来源的最新日志，并限制条数、单行长度和敏感字段。
func (m *Manager) Logs(ctx context.Context, source string, limit int) (Report, error) {
	source = strings.ToLower(strings.TrimSpace(source))
	if !ValidSource(source) {
		return Report{}, fmt.Errorf("不支持的日志来源 %q", source)
	}
	limit = normalizeLimit(limit)
	commandLimit := limit + 1
	if source == SourceUSB {
		commandLimit = min(limit*10, 3000)
	}
	args := journalArgs(source, commandLimit)
	output, err := m.runner.Run(ctx, "journalctl", args...)
	if err != nil {
		return Report{}, fmt.Errorf("读取系统日志: %w", err)
	}

	lines := make([]string, 0, limit+1)
	redacted := 0
	for _, raw := range strings.Split(strings.TrimSpace(string(output)), "\n") {
		line := sanitizeLine(raw)
		if line == "" || source == SourceUSB && !containsUSBKeyword(line) {
			continue
		}
		masked := secretAssignment.ReplaceAllString(line, `$1$2[已脱敏]`)
		if masked != line {
			redacted++
		}
		lines = append(lines, masked)
	}
	limited := len(lines) > limit
	if limited {
		lines = lines[len(lines)-limit:]
	}
	report := Report{
		Source: source, GeneratedAt: time.Now(), Lines: lines,
		Limited: limited, Redacted: redacted,
	}
	if source == SourceUSB {
		states, stateErr := gadget.InspectConflictingServices(ctx, m.runner)
		if stateErr == nil {
			report.ServiceStates = states
		}
	}
	return report, nil
}

// ValidSource 判断页面请求是否属于固定的安全日志来源。
func ValidSource(source string) bool {
	switch strings.ToLower(strings.TrimSpace(source)) {
	case SourceApplication, SourceUSB, SourceNetwork:
		return true
	default:
		return false
	}
}

func normalizeLimit(limit int) int {
	if limit <= 0 {
		return defaultLimit
	}
	return min(limit, maximumLimit)
}

func journalArgs(source string, limit int) []string {
	args := []string{
		"--no-pager", "--quiet", "--boot", "--output=short-iso-precise",
		"--lines", strconv.Itoa(limit),
	}
	switch source {
	case SourceApplication:
		return append(args, "--unit", "teslabox.service", "--unit", "teslabox-update.service")
	case SourceNetwork:
		return append(args, "--unit", "NetworkManager.service", "--unit", "teslabox-ap.service")
	default:
		return append(args, "--dmesg")
	}
}

func containsUSBKeyword(line string) bool {
	lower := strings.ToLower(line)
	for _, keyword := range usbKeywords {
		if strings.Contains(lower, keyword) {
			return true
		}
	}
	return false
}

func sanitizeLine(line string) string {
	var result strings.Builder
	for _, char := range strings.TrimSpace(strings.TrimSuffix(line, "\r")) {
		if unicode.IsControl(char) && char != '\t' {
			continue
		}
		result.WriteRune(char)
		if result.Len() >= maximumLine {
			result.WriteString("…")
			break
		}
	}
	return result.String()
}
