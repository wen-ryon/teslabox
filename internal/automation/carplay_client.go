package automation

import (
	"context"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

func runCarPlayCLI(ctx context.Context, socketPath string, args []string, stdout, stderr io.Writer) int {
	if len(args) < 2 || args[0] != "network" {
		writeCLIUsage(stderr)
		return 2
	}
	action := args[1]
	if action == "status" && len(args) == 2 {
		// 供网操作最多持有同一租约锁 45 秒，状态请求不能先超时并误触发接收端回收热点。
		return runCLIRequest(ctx, socketPath, http.MethodGet, "/v1/network/carplay/state", nil, stdout, stderr, time.Minute)
	}
	if action != "plan" && action != "acquire" && action != "renew" && action != "release" && action != "start" && action != "upstream" && action != "pan-reset" {
		writeCLIUsage(stderr)
		return 2
	}
	flags := flag.NewFlagSet("carplay network "+action, flag.ContinueOnError)
	flags.SetOutput(stderr)
	var value, owner, phone string
	ttl := defaultLeaseTTL
	if action != "release" && action != "start" && action != "upstream" && action != "pan-reset" {
		flags.StringVar(&value, "channels", "", "车机支持的信道，按优先顺序以逗号分隔")
	}
	if action != "plan" {
		flags.StringVar(&owner, "source", "", "CarPlay 租约来源")
	}
	if action == "upstream" || action == "pan-reset" {
		flags.StringVar(&phone, "phone", "", "已配对的 CarPlay 手机蓝牙地址")
	}
	if action == "acquire" || action == "renew" {
		flags.DurationVar(&ttl, "ttl", defaultLeaseTTL, "租约有效期，范围 15s 到 1h")
	}
	if err := flags.Parse(args[2:]); err != nil || flags.NArg() != 0 {
		return 2
	}
	if action != "plan" && !ownerPattern.MatchString(owner) {
		fmt.Fprintln(stderr, "必须提供有效的 --source")
		return 2
	}
	path := "/v1/network/carplay/leases/" + url.PathEscape(owner)
	if action == "pan-reset" {
		return runCLIRequest(ctx, socketPath, http.MethodPost, path+"/upstream", map[string]any{"phone": phone, "reset_pan": true}, stdout, stderr, time.Minute)
	}
	if action == "upstream" {
		return runCLIRequest(ctx, socketPath, http.MethodPost, path+"/upstream", map[string]string{"phone": phone}, stdout, stderr, time.Minute)
	}
	if action == "release" {
		return runCLIRequest(ctx, socketPath, http.MethodDelete, path, nil, stdout, stderr, time.Minute)
	}
	if action == "start" {
		return runCLIRequest(ctx, socketPath, http.MethodPost, path+"/ap", nil, stdout, stderr, time.Minute)
	}
	if ttl < minimumLeaseTTL || ttl > maximumLeaseTTL {
		fmt.Fprintln(stderr, "--ttl 必须在 15s 到 1h 之间")
		return 2
	}
	parts := strings.Split(value, ",")
	if len(parts) > 64 {
		fmt.Fprintln(stderr, "车机信道列表不能超过 64 项")
		return 2
	}
	channels := make([]int, 0, len(parts))
	seen := make(map[int]bool, len(parts))
	for _, part := range parts {
		channel, err := strconv.Atoi(strings.TrimSpace(part))
		if err != nil || !(channel >= 1 && channel <= 13 || channel >= 36 && channel <= 177) || seen[channel] {
			fmt.Fprintln(stderr, "必须提供有效的 --channels，使用互不重复的 1 到 13 或 36 到 177 信道")
			return 2
		}
		seen[channel] = true
		channels = append(channels, channel)
	}
	if action == "plan" {
		return runCLIRequest(ctx, socketPath, http.MethodPost, "/v1/network/carplay/plan", map[string]any{"channels": channels}, stdout, stderr)
	}
	return runCLIRequest(ctx, socketPath, http.MethodPut, path, map[string]any{"channels": channels, "ttl_seconds": int(ttl / time.Second)}, stdout, stderr, time.Minute)
}
