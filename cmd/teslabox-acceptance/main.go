package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/wen-ryon/teslabox/internal/acceptance"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	os.Exit(run(ctx, os.Args[1:], os.Stdout, os.Stderr))
}

func run(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet("teslabox-acceptance", flag.ContinueOnError)
	flags.SetOutput(stderr)
	baseURL := flags.String("base-url", "http://127.0.0.1:8080", "TeslaBox API 根地址")
	duration := flags.Duration("duration", 10*time.Minute, "持续验收时长")
	interval := flags.Duration("interval", 5*time.Second, "状态和 Range 探测间隔")
	requestTimeout := flags.Duration("request-timeout", 5*time.Second, "单个 HTTP 请求超时")
	useProxy := flags.Bool("use-proxy", false, "使用系统 HTTP 代理；局域网直连默认关闭")
	passwordEnv := flags.String("password-env", "TESLABOX_WEB_PASSWORD", "从指定环境变量读取 Web 管理密码")
	requireHost := flags.Bool("require-host", false, "要求每次采样都检测到车机 USB 主机")
	requireGrowth := flags.Bool("require-growth", false, "要求验收期间视频数量增加")
	requireRotation := flags.Bool("require-snapshot-rotation", true, "要求验收期间至少出现两个快照 ID")
	output := flags.String("output", "text", "输出格式：text 或 json")
	if err := flags.Parse(args); err != nil {
		return 2
	}
	if flags.NArg() != 0 {
		fmt.Fprintln(stderr, "不接受位置参数")
		return 2
	}
	if *requestTimeout <= 0 {
		fmt.Fprintln(stderr, "request-timeout 必须大于零")
		return 2
	}
	if *output != "text" && *output != "json" {
		fmt.Fprintln(stderr, "output 必须是 text 或 json")
		return 2
	}

	client := newHTTPClient(*requestTimeout, *useProxy)
	monitor, err := acceptance.NewMonitor(client, acceptance.Options{
		BaseURL:                 *baseURL,
		Duration:                *duration,
		Interval:                *interval,
		RequireHost:             *requireHost,
		RequireGrowth:           *requireGrowth,
		RequireSnapshotRotation: *requireRotation,
	})
	if err != nil {
		fmt.Fprintf(stderr, "验收参数无效：%v\n", err)
		return 2
	}
	if password := os.Getenv(*passwordEnv); password != "" {
		if err := login(ctx, client, *baseURL, password); err != nil {
			fmt.Fprintf(stderr, "登录 TeslaBox 失败：%v\n", err)
			return 1
		}
	}

	report, err := monitor.Run(ctx, func(sample acceptance.Sample) {
		if *output == "json" {
			return
		}
		if sample.Error != "" {
			fmt.Fprintf(stderr, "[%s] 阶段=%s 视频=%d 快照=%s 错误=%s\n",
				sample.CheckedAt.Format(time.RFC3339), sample.Phase, sample.VideoCount, sample.SnapshotID, sample.Error)
			return
		}
		fmt.Fprintf(stderr, "[%s] 阶段=%s 主机=%t UDC=%s 速率=%s 视频=%d 快照=%s Range=%d/%dB\n",
			sample.CheckedAt.Format(time.RFC3339), sample.Phase, sample.HostConnected, sample.UDCState,
			sample.CurrentSpeed, sample.VideoCount, sample.SnapshotID, sample.RangeStatus, sample.RangeBytes)
	})
	if err != nil {
		if errors.Is(err, context.Canceled) {
			fmt.Fprintln(stderr, "验收已取消")
			return 130
		}
		fmt.Fprintf(stderr, "验收监控失败：%v\n", err)
		return 1
	}
	if err := writeReport(stdout, report, *output); err != nil {
		fmt.Fprintf(stderr, "输出验收报告失败：%v\n", err)
		return 1
	}
	if !report.Passed {
		return 1
	}
	return 0
}

func newHTTPClient(timeout time.Duration, useProxy bool) *http.Client {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	if !useProxy {
		transport.Proxy = nil
	}
	jar, err := cookiejar.New(nil)
	if err != nil {
		panic("创建 HTTP Cookie 容器失败")
	}
	return &http.Client{Transport: transport, Timeout: timeout, Jar: jar}
}

func login(ctx context.Context, client *http.Client, baseURL, password string) error {
	parsed, err := url.Parse(baseURL)
	if err != nil {
		return fmt.Errorf("解析 base URL: %w", err)
	}
	payload, err := json.Marshal(map[string]string{"password": password})
	if err != nil {
		return fmt.Errorf("编码登录请求: %w", err)
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, parsed.ResolveReference(&url.URL{Path: "/api/v1/auth/login"}).String(), bytes.NewReader(payload))
	if err != nil {
		return fmt.Errorf("创建登录请求: %w", err)
	}
	request.Header.Set("Content-Type", "application/json")
	response, err := client.Do(request)
	if err != nil {
		return fmt.Errorf("发送登录请求: %w", err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return fmt.Errorf("HTTP %d", response.StatusCode)
	}
	return nil
}

func writeReport(writer io.Writer, report acceptance.Report, output string) error {
	if output == "json" {
		encoder := json.NewEncoder(writer)
		encoder.SetIndent("", "  ")
		return encoder.Encode(report)
	}
	result := "通过"
	if !report.Passed {
		result = "失败"
	}
	if _, err := fmt.Fprintf(writer,
		"验收结果：%s\n采样次数：%d\nRange 成功：%d\n视频数量：%d -> %d\n快照轮换：%d 个\n",
		result, report.Checks, report.RangeChecks, report.InitialVideoCount, report.MaxVideoCount, len(report.SnapshotIDs)); err != nil {
		return err
	}
	if len(report.Failures) > 0 {
		if _, err := fmt.Fprintf(writer, "失败原因：\n- %s\n", strings.Join(report.Failures, "\n- ")); err != nil {
			return err
		}
	}
	return nil
}
