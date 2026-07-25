// Package systeminfo 采集管理页面需要的轻量系统状态。
package systeminfo

import (
	"context"
	"errors"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

// internetCacheDuration 平衡状态栏的新鲜度与车载待机功耗。
// 网络连接变化由 NetworkManager 负责，页面只需低频确认公共网络出口仍可访问。
const internetCacheDuration = time.Minute

// Usage 描述容量型资源的当前占用情况。
type Usage struct {
	TotalBytes     uint64  `json:"total_bytes"`
	UsedBytes      uint64  `json:"used_bytes"`
	AvailableBytes uint64  `json:"available_bytes"`
	UsedPercent    float64 `json:"used_percent"`
}

// Status 是设备运行状态的轻量快照。
type Status struct {
	InternetAvailable bool      `json:"internet_available"`
	InternetCheckedAt time.Time `json:"internet_checked_at"`
	TemperatureC      *float64  `json:"temperature_c,omitempty"`
	CPUUsedPercent    *float64  `json:"cpu_used_percent,omitempty"`
	Memory            *Usage    `json:"memory,omitempty"`
	SystemDisk        *Usage    `json:"system_disk,omitempty"`
	UptimeSeconds     *float64  `json:"uptime_seconds,omitempty"`
}

type cpuSample struct {
	total uint64
	idle  uint64
}

// Collector 保留上一份 CPU 计数器和短时互联网探测缓存，避免高频外部连接。
type Collector struct {
	mu                sync.Mutex
	previousCPU       *cpuSample
	internetAvailable bool
	internetCheckedAt time.Time
	now               func() time.Time
	dialContext       func(context.Context, string, string) (net.Conn, error)
}

// NewCollector 创建只读状态采集器，构造时不发起互联网连接。
func NewCollector() *Collector {
	dialer := &net.Dialer{}
	collector := &Collector{now: time.Now, dialContext: dialer.DialContext}
	if sample, err := readCPUSample(); err == nil {
		collector.previousCPU = &sample
	}
	return collector
}

// Collect 读取本机资源，并按缓存周期探测一次公共网络出口。
func (c *Collector) Collect(ctx context.Context) Status {
	c.mu.Lock()
	defer c.mu.Unlock()

	now := c.now()
	if c.internetCheckedAt.IsZero() || now.Sub(c.internetCheckedAt) >= internetCacheDuration {
		c.internetAvailable = c.checkInternet(ctx)
		c.internetCheckedAt = now
	}
	status := Status{
		InternetAvailable: c.internetAvailable,
		InternetCheckedAt: c.internetCheckedAt,
		TemperatureC:      readTemperature(),
		Memory:            readMemoryUsage(),
		SystemDisk:        readDiskUsage("/"),
		UptimeSeconds:     readUptime(),
	}
	if current, err := readCPUSample(); err == nil {
		status.CPUUsedPercent = calculateCPUUsage(c.previousCPU, current)
		c.previousCPU = &current
	}
	return status
}

func (c *Collector) checkInternet(parent context.Context) bool {
	ctx, cancel := context.WithTimeout(parent, 1200*time.Millisecond)
	defer cancel()
	for _, address := range []string{"223.5.5.5:53", "119.29.29.29:53"} {
		connection, err := c.dialContext(ctx, "tcp", address)
		if err == nil {
			connection.Close()
			return true
		}
		if ctx.Err() != nil {
			return false
		}
	}
	return false
}

func readCPUSample() (cpuSample, error) {
	content, err := os.ReadFile("/proc/stat")
	if err != nil {
		return cpuSample{}, err
	}
	return parseCPUSample(string(content))
}

func parseCPUSample(content string) (cpuSample, error) {
	line, _, _ := strings.Cut(content, "\n")
	fields := strings.Fields(line)
	if len(fields) < 5 || fields[0] != "cpu" {
		return cpuSample{}, errors.New("无效的 /proc/stat")
	}
	values := make([]uint64, 0, len(fields)-1)
	for _, field := range fields[1:] {
		value, err := strconv.ParseUint(field, 10, 64)
		if err != nil {
			return cpuSample{}, err
		}
		values = append(values, value)
	}
	var total uint64
	for index, value := range values {
		if index >= 8 {
			break
		}
		total += value
	}
	idle := values[3]
	if len(values) > 4 {
		idle += values[4]
	}
	return cpuSample{total: total, idle: idle}, nil
}

func calculateCPUUsage(previous *cpuSample, current cpuSample) *float64 {
	if previous == nil || current.total <= previous.total || current.idle < previous.idle {
		return nil
	}
	totalDelta := current.total - previous.total
	idleDelta := current.idle - previous.idle
	if idleDelta > totalDelta {
		return nil
	}
	used := float64(totalDelta-idleDelta) / float64(totalDelta) * 100
	return &used
}

func readMemoryUsage() *Usage {
	content, err := os.ReadFile("/proc/meminfo")
	if err != nil {
		return nil
	}
	values := make(map[string]uint64)
	for _, line := range strings.Split(string(content), "\n") {
		fields := strings.Fields(line)
		if len(fields) < 2 {
			continue
		}
		value, err := strconv.ParseUint(fields[1], 10, 64)
		if err == nil {
			values[strings.TrimSuffix(fields[0], ":")] = value * 1024
		}
	}
	total := values["MemTotal"]
	available := values["MemAvailable"]
	if total == 0 || available > total {
		return nil
	}
	return newUsage(total, available)
}

func readTemperature() *float64 {
	paths, _ := filepath.Glob("/sys/class/thermal/thermal_zone*/temp")
	var selected *float64
	for _, path := range paths {
		content, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		value, err := strconv.ParseFloat(strings.TrimSpace(string(content)), 64)
		if err != nil {
			continue
		}
		if value > 1000 {
			value /= 1000
		}
		if value < -20 || value > 150 {
			continue
		}
		if selected == nil || value > *selected {
			copy := value
			selected = &copy
		}
	}
	return selected
}

func readUptime() *float64 {
	content, err := os.ReadFile("/proc/uptime")
	if err != nil {
		return nil
	}
	fields := strings.Fields(string(content))
	if len(fields) == 0 {
		return nil
	}
	value, err := strconv.ParseFloat(fields[0], 64)
	if err != nil {
		return nil
	}
	return &value
}

func newUsage(total, available uint64) *Usage {
	used := total - available
	return &Usage{
		TotalBytes: total, UsedBytes: used, AvailableBytes: available,
		UsedPercent: float64(used) / float64(total) * 100,
	}
}

func newFilesystemUsage(total, free, available uint64) *Usage {
	if total == 0 {
		return nil
	}
	if free > total {
		free = total
	}
	if available > free {
		available = free
	}
	used := total - free
	return &Usage{
		TotalBytes: total, UsedBytes: used, AvailableBytes: available,
		UsedPercent: float64(used) / float64(total) * 100,
	}
}
