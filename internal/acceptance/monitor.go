// Package acceptance 提供不修改设备状态的 TeslaUSB 持续写入验收监控。
package acceptance

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"time"

	"github.com/wen-ryon/teslabox/internal/service"
	"github.com/wen-ryon/teslabox/internal/teslacam"
)

const maxResponseBytes = 1024 * 1024

// Options 定义验收时长和必须满足的实车条件。
type Options struct {
	BaseURL                 string
	Duration                time.Duration
	Interval                time.Duration
	RequireHost             bool
	RequireGrowth           bool
	RequireSnapshotRotation bool
}

// Sample 保存一次状态、索引和 Range 探测结果，便于实时显示进度。
type Sample struct {
	CheckedAt     time.Time `json:"checked_at"`
	Phase         string    `json:"phase,omitempty"`
	HostConnected bool      `json:"host_connected"`
	CurrentSpeed  string    `json:"current_speed,omitempty"`
	UDCState      string    `json:"udc_state,omitempty"`
	SnapshotID    string    `json:"snapshot_id,omitempty"`
	VideoCount    int       `json:"video_count"`
	RangeStatus   int       `json:"range_status,omitempty"`
	RangeBytes    int64     `json:"range_bytes,omitempty"`
	Error         string    `json:"error,omitempty"`
}

// Report 汇总监控期间可复核的通过条件和失败原因。
type Report struct {
	StartedAt         time.Time `json:"started_at"`
	FinishedAt        time.Time `json:"finished_at"`
	Checks            int       `json:"checks"`
	RangeChecks       int       `json:"range_checks"`
	InitialVideoCount int       `json:"initial_video_count"`
	MaxVideoCount     int       `json:"max_video_count"`
	SnapshotIDs       []string  `json:"snapshot_ids"`
	Passed            bool      `json:"passed"`
	Failures          []string  `json:"failures"`
}

// Monitor 通过公开 HTTP API 验证主链路，不触发扫描或设备状态变更。
type Monitor struct {
	baseURL *url.URL
	client  *http.Client
	options Options
	now     func() time.Time
	wait    func(context.Context, time.Duration) error
}

// NewMonitor 校验监控参数，拒绝非 HTTP 地址和可能造成忙循环的间隔。
func NewMonitor(client *http.Client, options Options) (*Monitor, error) {
	if client == nil {
		return nil, errors.New("HTTP 客户端不能为空")
	}
	baseURL, err := url.Parse(strings.TrimSpace(options.BaseURL))
	if err != nil || baseURL.Host == "" || (baseURL.Scheme != "http" && baseURL.Scheme != "https") || baseURL.User != nil {
		return nil, errors.New("base URL 必须是不含用户信息的 HTTP 或 HTTPS 地址")
	}
	if options.Duration <= 0 || options.Interval <= 0 {
		return nil, errors.New("验收时长和采样间隔必须大于零")
	}
	if options.Interval > options.Duration {
		return nil, errors.New("采样间隔不能大于验收时长")
	}
	return &Monitor{
		baseURL: baseURL,
		client:  client,
		options: options,
		now:     time.Now,
		wait:    waitContext,
	}, nil
}

// Run 持续采样直到时长结束或上下文取消，验收失败通过 Report 返回。
func (m *Monitor) Run(ctx context.Context, onSample func(Sample)) (Report, error) {
	startedAt := m.now()
	report := Report{StartedAt: startedAt}
	deadline := startedAt.Add(m.options.Duration)
	failures := make(map[string]struct{})
	snapshotIDs := make(map[string]struct{})

	for {
		sample := m.collect(ctx)
		report.Checks++
		if report.Checks == 1 {
			report.InitialVideoCount = sample.VideoCount
		}
		report.MaxVideoCount = max(report.MaxVideoCount, sample.VideoCount)
		if sample.SnapshotID != "" {
			snapshotIDs[sample.SnapshotID] = struct{}{}
		}
		if sample.RangeStatus == http.StatusPartialContent && sample.RangeBytes > 0 {
			report.RangeChecks++
		}
		m.evaluateSample(sample, failures)
		if onSample != nil {
			onSample(sample)
		}

		now := m.now()
		if !now.Before(deadline) {
			break
		}
		remaining := deadline.Sub(now)
		if err := m.wait(ctx, min(m.options.Interval, remaining)); err != nil {
			report.FinishedAt = m.now()
			return report, err
		}
	}

	for id := range snapshotIDs {
		report.SnapshotIDs = append(report.SnapshotIDs, id)
	}
	slices.Sort(report.SnapshotIDs)
	if m.options.RequireGrowth && report.MaxVideoCount <= report.InitialVideoCount {
		failures["验收期间没有观察到视频数量增长"] = struct{}{}
	}
	if m.options.RequireSnapshotRotation && len(report.SnapshotIDs) < 2 {
		failures["验收期间没有观察到快照轮换"] = struct{}{}
	}
	for failure := range failures {
		report.Failures = append(report.Failures, failure)
	}
	slices.Sort(report.Failures)
	report.FinishedAt = m.now()
	report.Passed = len(report.Failures) == 0
	return report, nil
}

func (m *Monitor) collect(ctx context.Context) Sample {
	sample := Sample{CheckedAt: m.now()}
	var state service.Snapshot
	if err := m.getJSON(ctx, "/api/v1/storage", &state); err != nil {
		sample.Error = fmt.Sprintf("读取存储状态: %v", err)
		return sample
	}
	sample.Phase = string(state.Phase)
	sample.HostConnected = state.GadgetStatus.HostConnected
	sample.CurrentSpeed = state.GadgetStatus.CurrentSpeed
	sample.UDCState = state.GadgetStatus.UDCState
	sample.VideoCount = state.VideoCount
	if state.MediaSnapshot != nil {
		sample.SnapshotID = state.MediaSnapshot.ID
	}
	if state.SnapshotError != "" {
		sample.Error = "快照降级: " + state.SnapshotError
		return sample
	}
	if !state.SnapshotSpace.Ready {
		sample.Error = "快照空间状态未就绪"
		return sample
	}
	if !state.GadgetStatus.Bound {
		sample.Error = "Gadget 未绑定 UDC"
		return sample
	}

	var videos videoListResponse
	if err := m.getJSON(ctx, "/api/v1/videos?limit=1", &videos); err != nil {
		sample.Error = fmt.Sprintf("读取视频列表: %v", err)
		return sample
	}
	if len(videos.Items) == 0 {
		sample.Error = "视频列表为空"
		return sample
	}
	status, size, err := m.checkRange(ctx, videos.Items[0].ID)
	sample.RangeStatus = status
	sample.RangeBytes = size
	if err != nil {
		sample.Error = fmt.Sprintf("Range 探测: %v", err)
	}
	return sample
}

func (m *Monitor) evaluateSample(sample Sample, failures map[string]struct{}) {
	if sample.Error != "" {
		failures[sample.Error] = struct{}{}
	}
	if sample.Phase == string(service.PhaseFault) || sample.Phase == string(service.PhaseDegraded) || sample.Phase == string(service.PhaseWaitingForDataDrive) {
		failures["检测到不可接受的服务阶段 "+sample.Phase] = struct{}{}
	}
	if m.options.RequireHost && !sample.HostConnected {
		failures["检测到车机 USB 主机断开"] = struct{}{}
	}
	if sample.HostConnected && (sample.CurrentSpeed == "" || strings.EqualFold(sample.CurrentSpeed, "UNKNOWN")) {
		failures["车机已连接但 USB 速率未知"] = struct{}{}
	}
	if m.options.RequireHost && !strings.EqualFold(sample.UDCState, "configured") {
		failures["车机 USB 未保持 configured 状态"] = struct{}{}
	}
	if sample.RangeStatus != 0 && sample.RangeStatus != http.StatusPartialContent {
		failures[fmt.Sprintf("Range 请求返回 HTTP %d", sample.RangeStatus)] = struct{}{}
	}
}

func (m *Monitor) getJSON(ctx context.Context, path string, target any) error {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, m.resolve(path), nil)
	if err != nil {
		return fmt.Errorf("创建请求: %w", err)
	}
	response, err := m.client.Do(request)
	if err != nil {
		return fmt.Errorf("发送请求: %w", err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return fmt.Errorf("HTTP %d", response.StatusCode)
	}
	decoder := json.NewDecoder(io.LimitReader(response.Body, maxResponseBytes))
	if err := decoder.Decode(target); err != nil {
		return fmt.Errorf("解析 JSON: %w", err)
	}
	return nil
}

func (m *Monitor) checkRange(ctx context.Context, id string) (int, int64, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, m.resolve("/api/v1/videos/"+url.PathEscape(id)+"/content"), nil)
	if err != nil {
		return 0, 0, fmt.Errorf("创建请求: %w", err)
	}
	request.Header.Set("Range", "bytes=0-1023")
	response, err := m.client.Do(request)
	if err != nil {
		return 0, 0, fmt.Errorf("发送请求: %w", err)
	}
	defer response.Body.Close()
	size, readErr := io.Copy(io.Discard, io.LimitReader(response.Body, 1025))
	if readErr != nil {
		return response.StatusCode, size, fmt.Errorf("读取响应: %w", readErr)
	}
	if response.StatusCode != http.StatusPartialContent {
		return response.StatusCode, size, fmt.Errorf("期望 HTTP 206，实际为 %d", response.StatusCode)
	}
	if contentRange := response.Header.Get("Content-Range"); !strings.HasPrefix(contentRange, "bytes 0-1023/") {
		return response.StatusCode, size, fmt.Errorf("Content-Range 无效 %q", contentRange)
	}
	if contentType := response.Header.Get("Content-Type"); !strings.HasPrefix(strings.ToLower(contentType), "video/mp4") {
		return response.StatusCode, size, fmt.Errorf("Content-Type 无效 %q", contentType)
	}
	if size != 1024 {
		return response.StatusCode, size, fmt.Errorf("期望 1024 字节，实际为 %d", size)
	}
	return response.StatusCode, size, nil
}

func (m *Monitor) resolve(path string) string {
	reference, _ := url.Parse(path)
	return m.baseURL.ResolveReference(reference).String()
}

func waitContext(ctx context.Context, duration time.Duration) error {
	timer := time.NewTimer(duration)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

type videoListResponse struct {
	Items []teslacam.Video `json:"items"`
}
