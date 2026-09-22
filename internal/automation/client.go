package automation

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	buildversion "github.com/wen-ryon/teslabox/internal/version"
)

const (
	DefaultCLIPath    = "/usr/local/bin/teslaboxctl"
	DefaultTargetPath = "/usr/local/bin/teslabox"
)

// EnsureCLIShortcut 创建指向主程序的稳定控制命令；已有非预期目标时拒绝覆盖。
func EnsureCLIShortcut(linkPath, targetPath string) error {
	if !filepath.IsAbs(linkPath) || !filepath.IsAbs(targetPath) || filepath.Clean(linkPath) == filepath.Clean(targetPath) {
		return errors.New("teslaboxctl 链接路径无效")
	}
	info, err := os.Lstat(linkPath)
	if errors.Is(err, os.ErrNotExist) {
		if err := os.Symlink(targetPath, linkPath); err != nil {
			return fmt.Errorf("创建 teslaboxctl 链接: %w", err)
		}
		return nil
	}
	if err != nil {
		return fmt.Errorf("检查 teslaboxctl 链接: %w", err)
	}
	if info.Mode()&os.ModeSymlink == 0 {
		return errors.New("teslaboxctl 路径已被非符号链接文件占用")
	}
	currentTarget, err := os.Readlink(linkPath)
	if err != nil {
		return fmt.Errorf("读取 teslaboxctl 链接: %w", err)
	}
	if filepath.Clean(currentTarget) != filepath.Clean(targetPath) {
		return fmt.Errorf("teslaboxctl 已指向其他目标 %q，拒绝覆盖", currentTarget)
	}
	return nil
}

// RunCLI 执行内置的本机自动化客户端，并返回进程退出码。
func RunCLI(ctx context.Context, socketPath string, args []string, stdout, stderr io.Writer) int {
	if len(args) == 1 && (args[0] == "-v" || args[0] == "--version" || args[0] == "version") {
		info := buildversion.Current()
		fmt.Fprintf(stdout, "TeslaBox %s (commit %s)\n", info.Version, info.Commit)
		return 0
	}
	if len(args) < 2 || args[0] != "ap" {
		writeCLIUsage(stderr)
		return 2
	}
	action := args[1]
	switch action {
	case "status":
		if len(args) != 2 {
			writeCLIUsage(stderr)
			return 2
		}
		return runCLIRequest(ctx, socketPath, http.MethodGet, "/v1/network/ap/state", nil, stdout, stderr)
	case "enable":
		flags := flag.NewFlagSet("ap enable", flag.ContinueOnError)
		flags.SetOutput(stderr)
		owner := flags.String("source", "", "租约来源，例如 gps-geofence")
		ttl := flags.Duration("ttl", defaultLeaseTTL, "租约有效期，范围 15s 到 1h")
		if err := flags.Parse(args[2:]); err != nil || flags.NArg() != 0 || !ownerPattern.MatchString(*owner) {
			fmt.Fprintln(stderr, "必须提供有效的 --source")
			return 2
		}
		if *ttl < minimumLeaseTTL || *ttl > maximumLeaseTTL {
			fmt.Fprintln(stderr, "--ttl 必须在 15s 到 1h 之间")
			return 2
		}
		body := map[string]int64{"ttl_seconds": int64(*ttl / time.Second)}
		return runCLIRequest(ctx, socketPath, http.MethodPut, leasePath(*owner), body, stdout, stderr)
	case "disable":
		flags := flag.NewFlagSet("ap disable", flag.ContinueOnError)
		flags.SetOutput(stderr)
		owner := flags.String("source", "", "要释放的租约来源")
		if err := flags.Parse(args[2:]); err != nil || flags.NArg() != 0 || !ownerPattern.MatchString(*owner) {
			fmt.Fprintln(stderr, "必须提供有效的 --source")
			return 2
		}
		return runCLIRequest(ctx, socketPath, http.MethodDelete, leasePath(*owner), nil, stdout, stderr)
	case "config":
		if len(args) == 2 || (len(args) == 3 && args[2] == "show") {
			return runCLIRequest(ctx, socketPath, http.MethodGet, "/v1/network/ap/config", nil, stdout, stderr)
		}
		if len(args) < 3 || args[2] != "set" {
			writeCLIUsage(stderr)
			return 2
		}
		flags := flag.NewFlagSet("ap config set", flag.ContinueOnError)
		flags.SetOutput(stderr)
		band := flags.String("band", "", "首选频段：2.4g 或 5g")
		channel := flags.String("channel", "", "首选信道编号或 auto")
		if err := flags.Parse(args[3:]); err != nil || flags.NArg() != 0 {
			return 2
		}
		apiBand, apiChannel, err := parseRadioConfig(*band, *channel)
		if err != nil {
			fmt.Fprintln(stderr, err)
			return 2
		}
		body := map[string]any{"band": apiBand, "channel": apiChannel}
		return runCLIRequest(ctx, socketPath, http.MethodPut, "/v1/network/ap/config", body, stdout, stderr)
	default:
		writeCLIUsage(stderr)
		return 2
	}
}

func parseRadioConfig(band, channel string) (string, int, error) {
	switch strings.ToLower(strings.TrimSpace(band)) {
	case "2.4g", "2.4ghz", "bg":
		band = "bg"
	case "5g", "5ghz", "a":
		band = "a"
	default:
		return "", 0, errors.New("--band 必须是 2.4g 或 5g")
	}
	channel = strings.ToLower(strings.TrimSpace(channel))
	if channel == "auto" {
		return band, 0, nil
	}
	value, err := strconv.Atoi(channel)
	if err != nil || value <= 0 {
		return "", 0, errors.New("--channel 必须是正整数信道或 auto")
	}
	return band, value, nil
}

func leasePath(owner string) string {
	return "/v1/network/ap/leases/" + url.PathEscape(owner)
}

func runCLIRequest(ctx context.Context, socketPath, method, path string, body any, stdout, stderr io.Writer) int {
	var payload io.Reader
	if body != nil {
		content, err := json.Marshal(body)
		if err != nil {
			fmt.Fprintln(stderr, "编码请求失败")
			return 1
		}
		payload = bytes.NewReader(content)
	}
	transport := &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, "unix", socketPath)
		},
	}
	defer transport.CloseIdleConnections()
	client := &http.Client{Transport: transport, Timeout: 30 * time.Second}
	request, err := http.NewRequestWithContext(ctx, method, "http://unix"+path, payload)
	if err != nil {
		fmt.Fprintln(stderr, "创建请求失败")
		return 1
	}
	if body != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	response, err := client.Do(request)
	if err != nil {
		fmt.Fprintf(stderr, "连接 TeslaBox 自动化接口失败：%v\n", err)
		return 1
	}
	defer response.Body.Close()
	content, err := io.ReadAll(io.LimitReader(response.Body, 64*1024+1))
	if err != nil || len(content) > 64*1024 {
		fmt.Fprintln(stderr, "读取自动化接口响应失败")
		return 1
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		message := strings.TrimSpace(string(content))
		var result map[string]any
		if json.Unmarshal(content, &result) == nil {
			if text, ok := result["error"].(string); ok {
				message = text
			}
		}
		fmt.Fprintf(stderr, "自动化接口返回 %s：%s\n", strconv.Itoa(response.StatusCode), message)
		return 1
	}
	var formatted bytes.Buffer
	if json.Indent(&formatted, content, "", "  ") == nil {
		_, _ = fmt.Fprintln(stdout, formatted.String())
	} else {
		_, _ = stdout.Write(content)
	}
	return 0
}

func writeCLIUsage(output io.Writer) {
	_, _ = fmt.Fprintln(output, `用法：
  teslaboxctl -v
  teslaboxctl ap status
  teslaboxctl ap config [show]
  teslaboxctl ap config set --band 2.4g|5g --channel auto|<信道>
  teslaboxctl ap enable --source gps-geofence [--ttl 5m]
  teslaboxctl ap disable --source gps-geofence`)
}
