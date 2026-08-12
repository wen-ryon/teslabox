// Package fragmentation 提供 TeslaCam 镜像 XFS extent 的只读诊断。
package fragmentation

import (
	"context"
	"errors"
	"fmt"
	"os"
	"regexp"
	"strconv"
	"strings"

	"github.com/wen-ryon/teslabox/internal/system"
)

const xfsBmapBlockBytes = 512

var extentLinePattern = regexp.MustCompile(`^\s*\d+:\s+\[(\d+)\.\.(\d+)\]:\s+(.+)$`)

// Severity 是只读诊断的稳定分级，不会自动触发整理或修改存储参数。
type Severity string

const (
	SeverityHealthy  Severity = "healthy"
	SeverityWarning  Severity = "warning"
	SeverityCritical Severity = "critical"
)

// Report 汇总镜像的物理 extent 分布，帮助区分 USB 掉线与外层 XFS 碎片写放大。
type Report struct {
	ImagePath          string   `json:"image_path"`
	LogicalBytes       int64    `json:"logical_bytes"`
	AllocatedBytes     int64    `json:"allocated_bytes"`
	HoleBytes          int64    `json:"hole_bytes"`
	ExtentCount        int      `json:"extent_count"`
	TinyExtentCount    int      `json:"tiny_extent_count"`
	LargestExtentBytes int64    `json:"largest_extent_bytes"`
	Severity           Severity `json:"severity"`
	Reason             string   `json:"reason"`
}

// Inspector 通过 xfs_bmap 读取 XFS 元数据；检查过程不打开镜像分区，也不改变 Gadget 生命周期。
type Inspector struct {
	runner system.Runner
}

// New 创建只读碎片检查器，外部命令由调用方统一注入以便限制上下文和测试。
func New(runner system.Runner) *Inspector {
	return &Inspector{runner: runner}
}

// Inspect 校验普通文件后统计 extent；失败时保留具体命令错误供日志中心定位依赖或文件系统问题。
func (i *Inspector) Inspect(ctx context.Context, imagePath string) (Report, error) {
	if i == nil || i.runner == nil {
		return Report{}, errors.New("碎片检查器不可用")
	}
	info, err := os.Lstat(imagePath)
	if err != nil {
		return Report{}, fmt.Errorf("检查 TeslaCam 镜像: %w", err)
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
		return Report{}, errors.New("TeslaCam 镜像不是可信普通文件")
	}
	output, err := i.runner.Run(ctx, "/usr/sbin/xfs_bmap", "-v", "--", imagePath)
	if err != nil {
		return Report{}, fmt.Errorf("读取 TeslaCam 镜像 extent: %w", err)
	}
	report, err := Parse(string(output))
	if err != nil {
		return Report{}, err
	}
	report.ImagePath = imagePath
	report.LogicalBytes = info.Size()
	classify(&report)
	return report, nil
}

// Parse 解析 xfs_bmap -v 的文件偏移区间；按偏移统计可兼容不同 xfsprogs 的物理块列格式。
func Parse(output string) (Report, error) {
	var report Report
	matched := 0
	for _, line := range strings.Split(output, "\n") {
		parts := extentLinePattern.FindStringSubmatch(line)
		if len(parts) != 4 {
			continue
		}
		start, startErr := strconv.ParseInt(parts[1], 10, 64)
		end, endErr := strconv.ParseInt(parts[2], 10, 64)
		if startErr != nil || endErr != nil || end < start {
			return Report{}, fmt.Errorf("解析 xfs_bmap extent 区间失败: %q", strings.TrimSpace(line))
		}
		bytes := (end - start + 1) * xfsBmapBlockBytes
		matched++
		if strings.HasPrefix(strings.TrimSpace(parts[3]), "hole") {
			report.HoleBytes += bytes
			continue
		}
		report.ExtentCount++
		report.AllocatedBytes += bytes
		if bytes <= 64*1024 {
			report.TinyExtentCount++
		}
		if bytes > report.LargestExtentBytes {
			report.LargestExtentBytes = bytes
		}
	}
	if matched == 0 {
		return Report{}, errors.New("xfs_bmap 未返回可识别的 extent")
	}
	classify(&report)
	return report, nil
}

func classify(report *Report) {
	tinyPercent := float64(0)
	if report.ExtentCount > 0 {
		tinyPercent = float64(report.TinyExtentCount) * 100 / float64(report.ExtentCount)
	}
	switch {
	case report.ExtentCount > 4096 || (report.ExtentCount > 1024 && tinyPercent >= 20):
		report.Severity = SeverityCritical
		report.Reason = fmt.Sprintf("镜像已有 %d 个物理 extent，其中 %.1f%% 不超过 64 KiB；持续写入可能出现高 IOPS 和长暂停", report.ExtentCount, tinyPercent)
	case report.ExtentCount > 1024 || (report.ExtentCount > 128 && tinyPercent >= 10):
		report.Severity = SeverityWarning
		report.Reason = fmt.Sprintf("镜像物理 extent 偏多（%d 个，微小 extent %.1f%%），建议安排离线整理", report.ExtentCount, tinyPercent)
	default:
		report.Severity = SeverityHealthy
		report.Reason = fmt.Sprintf("镜像 extent 分布正常（%d 个，微小 extent %.1f%%）", report.ExtentCount, tinyPercent)
	}
}
