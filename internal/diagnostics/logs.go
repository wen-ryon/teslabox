// Package diagnostics 提供受限且脱敏的设备诊断日志读取能力。
package diagnostics

import (
	"context"
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode"
	"unicode/utf8"

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
	// SourceMedia 表示视频播放、FFmpeg 和媒体硬件相关日志。
	SourceMedia = "media"

	defaultLimit  = 200
	maximumLimit  = 500
	maximumLine   = 4096
	maximumBundle = 1536 * 1024
	bundleTimeout = 45 * time.Second
)

var (
	secretAssignment = regexp.MustCompile(`(?i)(\b(?:password|passwd|psk|token|secret|authorization|cookie)\b["']?)(\s*[:=]\s*)("[^"]*"|'[^']*'|[^\s,;]+)`)
	usbKeywords      = []string{
		"usb", "dwc3", "udc", "gadget", "configfs", "mass_storage", "mass storage", "lun",
		"fcc00000", "teslausb", "状态切换", "数据盘", "媒体快照", "快照挂载", "快照修复",
		"snapshot", "radxa-adbd", "radxa-usbnet",
	}
	mediaKeywords = []string{
		"ffmpeg", "实时合成", "合成录像", "转码", "视频流", "播放诊断", "客户端播放",
		"rkmpp", "mpp", "rga", "vpu", "decoder", "encoder", "h264", "hevc", "avc",
		"out of memory", "oom", "killed process", "watchdog", "thermal", "overheat",
		"媒体快照", "snapshot",
	}
	incidentKeywords = append(append([]string{}, usbKeywords...), append(mediaKeywords,
		"panic", "oops", "call trace", "segfault", "reboot", "shutdown", "reset",
		"voltage", "undervoltage", "regulator", "i/o error", "filesystem error")...)
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

// Bundle 是用户可以直接下载并发送的有界文本诊断报告。
type Bundle struct {
	Filename string
	Content  []byte
}

// Manager 只允许读取预定义的 journalctl 日志集合，避免页面成为任意命令入口。
type Manager struct {
	runner   system.Runner
	bundleMu sync.Mutex
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
	} else if source == SourceMedia {
		commandLimit = min(limit*12, 4000)
	}
	outputs := make([]string, 0, 2)
	for _, args := range journalArgs(source, commandLimit) {
		output, err := m.runner.Run(ctx, "journalctl", args...)
		if err != nil {
			return Report{}, fmt.Errorf("读取系统日志: %w", err)
		}
		outputs = append(outputs, string(output))
	}

	lines := make([]string, 0, limit+1)
	for _, output := range outputs {
		for _, raw := range strings.Split(strings.TrimSpace(output), "\n") {
			line := sanitizeLine(raw)
			if line == "" || !matchesSource(source, line) {
				continue
			}
			lines = append(lines, redactLine(line))
		}
	}
	if len(outputs) > 1 {
		sort.Strings(lines)
	}
	lines = compactDuplicateLines(lines)
	redacted := 0
	for _, line := range lines {
		if strings.Contains(line, "[已脱敏]") {
			redacted++
		}
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

// Bundle 收集当前状态、当前启动和上一次启动的关键日志，避免故障重启后丢失根因。
func (m *Manager) Bundle(ctx context.Context) (Bundle, error) {
	if !m.bundleMu.TryLock() {
		return Bundle{}, fmt.Errorf("已有诊断报告正在生成")
	}
	defer m.bundleMu.Unlock()

	ctx, cancel := context.WithTimeout(ctx, bundleTimeout)
	defer cancel()
	generatedAt := time.Now()
	var report strings.Builder
	fmt.Fprintf(&report, "TeslaBox 诊断报告\n生成时间: %s\n报告说明: 常见密码和令牌字段已自动脱敏；仍可能包含设备名称、SSID、IP 和硬件标识。\n",
		generatedAt.Format(time.RFC3339))

	commands := []diagnosticCommand{
		{title: "内核与系统", name: "uname", args: []string{"-a"}},
		{title: "运行时间与负载", name: "uptime"},
		{title: "内存", name: "free", args: []string{"-h"}},
		{title: "文件系统", name: "df", args: []string{"-hT"}},
		{title: "块设备", name: "lsblk", args: []string{"-o", "NAME,TYPE,SIZE,FSTYPE,MOUNTPOINTS,MODEL,TRAN"}},
		{title: "TeslaBox 挂载点", name: "findmnt", args: []string{
			"-rn", "-o", "TARGET,SOURCE,FSTYPE,OPTIONS",
		}, keywords: []string{"teslabox", "loop"}},
		{title: "TeslaBox Loop 设备", name: "losetup", args: []string{
			"--list", "--output", "NAME,BACK-FILE,SIZELIMIT,OFFSET,AUTOCLEAR,RO",
		}, keywords: []string{"name", "teslabox", "loop"}},
		{title: "SoC 温度", name: "cat", args: []string{"/sys/class/thermal/thermal_zone0/temp"}},
		{title: "TeslaBox 服务状态", name: "systemctl", args: []string{
			"show", "teslabox.service", "--no-pager",
			"--property=ActiveState,SubState,Result,NRestarts,ExecMainCode,ExecMainStatus,MainPID,ActiveEnterTimestamp",
		}},
		{title: "TeslaBox 与 FFmpeg 进程", name: "ps", args: []string{
			"-eo", "pid,ppid,etimes,pcpu,pmem,rss,stat,comm",
		}, keywords: []string{"pid", "teslabox", "ffmpeg"}},
		{title: "Gadget UDC 绑定", name: "cat", args: []string{"/sys/kernel/config/usb_gadget/teslabox/UDC"}},
		{title: "Gadget 枚举状态", name: "cat", args: []string{"/sys/class/udc/fcc00000.dwc3/state"}},
		{title: "Gadget 当前速率", name: "cat", args: []string{"/sys/class/udc/fcc00000.dwc3/current_speed"}},
		{title: "LUN 后端文件", name: "cat", args: []string{"/sys/kernel/config/usb_gadget/teslabox/functions/mass_storage.0/lun.0/file"}},
		{title: "LUN nofua", name: "cat", args: []string{"/sys/kernel/config/usb_gadget/teslabox/functions/mass_storage.0/lun.0/nofua"}},
		{title: "LUN 只读状态", name: "cat", args: []string{"/sys/kernel/config/usb_gadget/teslabox/functions/mass_storage.0/lun.0/ro"}},
		{title: "LUN 可移动状态", name: "cat", args: []string{"/sys/kernel/config/usb_gadget/teslabox/functions/mass_storage.0/lun.0/removable"}},
	}
	for _, command := range commands {
		writeCommandSection(ctx, m.runner, &report, command)
		if report.Len() >= maximumBundle {
			break
		}
	}

	for _, source := range []string{SourceUSB, SourceMedia, SourceApplication, SourceNetwork} {
		writeLogSection(ctx, m, &report, source)
		if report.Len() >= maximumBundle {
			break
		}
	}
	writePreviousBootSection(ctx, m.runner, &report)

	content := report.String()
	if len(content) > maximumBundle {
		cut := maximumBundle
		for cut > 0 && !utf8.RuneStart(content[cut]) {
			cut--
		}
		content = content[:cut] + "\n\n[报告已达到大小上限，后续内容被截断]\n"
	}
	return Bundle{
		Filename: "teslabox-diagnostics-" + generatedAt.Format("20060102-150405") + ".txt",
		Content:  []byte(content),
	}, nil
}

// ValidSource 判断页面请求是否属于固定的安全日志来源。
func ValidSource(source string) bool {
	switch strings.ToLower(strings.TrimSpace(source)) {
	case SourceApplication, SourceUSB, SourceNetwork, SourceMedia:
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

func journalArgs(source string, limit int) [][]string {
	args := []string{
		"--no-pager", "--quiet", "--boot", "--output=short-iso-precise",
		"--lines", strconv.Itoa(limit),
	}
	switch source {
	case SourceApplication:
		return [][]string{append(args, "--unit", "teslabox.service", "--unit", "teslabox-update.service")}
	case SourceNetwork:
		return [][]string{append(args, "--unit", "NetworkManager.service", "--unit", "teslabox-ap.service")}
	case SourceMedia:
		return [][]string{
			append(append([]string{}, args...), "--unit", "teslabox.service"),
			append(append([]string{}, args...), "--dmesg"),
		}
	default:
		return [][]string{
			append(append([]string{}, args...), "--dmesg"),
			append(append([]string{}, args...), "--unit", "teslabox.service", "--unit", "teslabox-update.service"),
		}
	}
}

func matchesSource(source, line string) bool {
	switch source {
	case SourceUSB:
		return containsKeyword(line, usbKeywords)
	case SourceMedia:
		return containsKeyword(line, mediaKeywords)
	default:
		return true
	}
}

func containsKeyword(line string, keywords []string) bool {
	lower := strings.ToLower(line)
	for _, keyword := range keywords {
		if strings.Contains(lower, keyword) {
			return true
		}
	}
	return false
}

func redactLine(line string) string {
	return secretAssignment.ReplaceAllString(line, `$1$2[已脱敏]`)
}

func compactDuplicateLines(lines []string) []string {
	if len(lines) < 2 {
		return lines
	}
	compacted := lines[:1]
	for _, line := range lines[1:] {
		if line != compacted[len(compacted)-1] {
			compacted = append(compacted, line)
		}
	}
	return compacted
}

type diagnosticCommand struct {
	title    string
	name     string
	args     []string
	keywords []string
}

func writeCommandSection(ctx context.Context, runner system.Runner, report *strings.Builder, command diagnosticCommand) {
	fmt.Fprintf(report, "\n===== %s =====\n", command.title)
	output, err := runner.Run(ctx, command.name, command.args...)
	if err != nil {
		fmt.Fprintf(report, "[无法读取] %s\n", redactLine(sanitizeLine(err.Error())))
		return
	}
	writeSanitizedLines(report, string(output), command.keywords, 500)
}

func writeLogSection(ctx context.Context, manager *Manager, report *strings.Builder, source string) {
	fmt.Fprintf(report, "\n===== 当前启动日志: %s =====\n", source)
	logs, err := manager.Logs(ctx, source, maximumLimit)
	if err != nil {
		fmt.Fprintf(report, "[无法读取] %s\n", redactLine(sanitizeLine(err.Error())))
		return
	}
	if len(logs.Lines) == 0 {
		report.WriteString("[没有匹配日志]\n")
		return
	}
	for _, line := range logs.Lines {
		report.WriteString(line)
		report.WriteByte('\n')
	}
}

func writePreviousBootSection(ctx context.Context, runner system.Runner, report *strings.Builder) {
	report.WriteString("\n===== 上一次启动关键故障日志 =====\n")
	args := []string{
		"--no-pager", "--quiet", "--boot=-1", "--output=short-iso-precise", "--lines", "5000",
	}
	output, err := runner.Run(ctx, "journalctl", args...)
	if err != nil {
		fmt.Fprintf(report, "[无法读取或没有持久化的上一启动日志] %s\n", redactLine(sanitizeLine(err.Error())))
		return
	}
	count := writeSanitizedLines(report, string(output), incidentKeywords, maximumLimit)
	if count == 0 {
		report.WriteString("[没有匹配日志]\n")
	}
}

func writeSanitizedLines(report *strings.Builder, output string, keywords []string, limit int) int {
	lines := make([]string, 0, limit)
	for _, raw := range strings.Split(strings.TrimSpace(output), "\n") {
		line := sanitizeLine(raw)
		if line == "" || len(keywords) > 0 && !containsKeyword(line, keywords) {
			continue
		}
		lines = append(lines, redactLine(line))
	}
	if len(lines) > limit {
		lines = lines[len(lines)-limit:]
	}
	for _, line := range lines {
		report.WriteString(line)
		report.WriteByte('\n')
	}
	return len(lines)
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
