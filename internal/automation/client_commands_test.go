package automation

import (
	"bytes"
	"context"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestCarPlayStatusSurvivesSlowSerializedUpstream(t *testing.T) {
	if os.Getenv("TESLABOX_CARPLAY_SLOW_STATUS_TEST") != "1" {
		t.Skip("慢供网回归需要显式启用，执行约 31 秒")
	}
	listener, err := net.Listen("unix", filepath.Join(t.TempDir(), "slow.sock"))
	if err != nil {
		t.Skipf("当前平台不支持 Unix Socket：%v", err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	server := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/network/carplay/state" {
			t.Errorf("请求路径错误: %s", r.URL.Path)
			return
		}
		// 模拟健康检查等待供网操作释放共享锁，响应本身仍是健康热点。
		timer := time.NewTimer(31 * time.Second)
		defer timer.Stop()
		select {
		case <-timer.C:
			_, _ = io.WriteString(w, `{"ap_healthy":true}`)
		case <-r.Context().Done():
		}
	})}
	go func() { _ = server.Serve(listener) }()
	t.Cleanup(func() { _ = server.Close() })
	var stdout, stderr bytes.Buffer
	code := runCLI(context.Background(), listener.Addr().String(), []string{"carplay", "network", "status"}, strings.NewReader(""), &stdout, &stderr)
	if code != 0 || !strings.Contains(stdout.String(), `"ap_healthy": true`) {
		t.Fatalf("健康检查因等待上游而失败: code=%d stderr=%q", code, stderr.String())
	}
}

func TestReadWiFiPassword(t *testing.T) {
	for _, test := range []struct {
		name, input, want string
		wantError         bool
	}{
		{name: "带换行", input: "safe-passphrase\n", want: "safe-passphrase"},
		{name: "Windows 换行", input: "safe-passphrase\r\n", want: "safe-passphrase"},
		{name: "末尾空格保留", input: "safe-passphrase \n", want: "safe-passphrase "},
		{name: "密码过短", input: "short\n", wantError: true},
		{name: "密码过长", input: strings.Repeat("x", 129), wantError: true},
		{name: "只读取首行", input: "first-line\nsecond-line\n", want: "first-line"},
		{name: "拒绝无效编码", input: string([]byte{0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff}), wantError: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			got, err := readWiFiPassword(strings.NewReader(test.input))
			if (err != nil) != test.wantError || got != test.want {
				t.Fatalf("readWiFiPassword() = (%q, %v), want (%q, error=%v)", got, err, test.want, test.wantError)
			}
		})
	}
}

func TestRunWiFiCLIMapsCommandsToLocalSocket(t *testing.T) {
	socketPath := filepath.Join(t.TempDir(), "control.sock")
	listener, err := net.Listen("unix", socketPath)
	if err != nil {
		t.Skipf("当前平台不支持测试 Unix Socket：%v", err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	var mu sync.Mutex
	var method, path, body string
	server := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		method, path = r.Method, r.URL.Path
		content, _ := io.ReadAll(r.Body)
		body = string(content)
		mu.Unlock()
		_, _ = io.WriteString(w, `{}`)
	})}
	go func() { _ = server.Serve(listener) }()
	t.Cleanup(func() { _ = server.Close() })

	for _, test := range []struct {
		name, method, path, bodyPart, stdin string
		args                                []string
	}{
		{name: "列表", method: http.MethodGet, path: "/v1/network/wifi/status", args: []string{"wifi", "list"}},
		{name: "CarPlay 网络规划", method: http.MethodPost, path: "/v1/network/carplay/plan", bodyPart: `"channels":[149,153,157,161]`, args: []string{"carplay", "network", "plan", "--channels", "149,153,157,161"}},
		{name: "CarPlay 2.4 GHz 规划", method: http.MethodPost, path: "/v1/network/carplay/plan", bodyPart: `"channels":[1,6,11]`, args: []string{"carplay", "network", "plan", "--channels", "1,6,11"}},
		{name: "CarPlay 蓝牙供网", method: http.MethodPost, path: "/v1/network/carplay/leases/carplay/upstream", bodyPart: `"phone":"AA:BB:CC:DD:EE:FF"`, args: []string{"carplay", "network", "upstream", "--source", "carplay", "--phone", "AA:BB:CC:DD:EE:FF"}},
		{name: "CarPlay 释放旧供网", method: http.MethodPost, path: "/v1/network/carplay/leases/carplay/upstream", bodyPart: `"reset_pan":true`, args: []string{"carplay", "network", "pan-reset", "--source", "carplay", "--phone", "AA:BB:CC:DD:EE:FF"}},
		{name: "CarPlay 租约状态", method: http.MethodGet, path: "/v1/network/carplay/state", args: []string{"carplay", "network", "status"}},
		{name: "CarPlay 申请", method: http.MethodPut, path: "/v1/network/carplay/leases/carplay", bodyPart: `"ttl_seconds":60`, args: []string{"carplay", "network", "acquire", "--source", "carplay", "--channels", "149,157", "--ttl", "1m"}},
		{name: "CarPlay 续租", method: http.MethodPut, path: "/v1/network/carplay/leases/carplay", bodyPart: `"channels":[149,157]`, args: []string{"carplay", "network", "renew", "--source", "carplay", "--channels", "149,157"}},
		{name: "CarPlay 释放", method: http.MethodDelete, path: "/v1/network/carplay/leases/carplay", args: []string{"carplay", "network", "release", "--source", "carplay"}},
		{name: "CarPlay 热点启动", method: http.MethodPost, path: "/v1/network/carplay/leases/carplay/ap", args: []string{"carplay", "network", "start", "--source", "carplay"}},
		{name: "扫描", method: http.MethodPost, path: "/v1/network/wifi/scan", args: []string{"wifi", "scan"}},
		{name: "断开", method: http.MethodPost, path: "/v1/network/wifi/disconnect", args: []string{"wifi", "disconnect"}},
		{name: "已保存连接", method: http.MethodPost, path: "/v1/network/wifi/connect", bodyPart: `"uuid":"550e8400-e29b-41d4-a716-446655440000"`, args: []string{"wifi", "connect", "--uuid", "550e8400-e29b-41d4-a716-446655440000"}},
		{name: "密码标准输入", method: http.MethodPost, path: "/v1/network/wifi/connect", bodyPart: `"password":"safe-password"`, stdin: "safe-password\n", args: []string{"wifi", "connect", "--ssid", "Home", "--security", "wpa", "--password-stdin"}},
		{name: "关闭自动连接", method: http.MethodPut, path: "/v1/network/wifi/saved/550e8400-e29b-41d4-a716-446655440000/autoconnect", bodyPart: `"enabled":false`, args: []string{"wifi", "autoconnect", "disable", "--uuid", "550e8400-e29b-41d4-a716-446655440000"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			code := runCLI(context.Background(), socketPath, test.args, strings.NewReader(test.stdin), &stdout, &stderr)
			if code != 0 || stderr.Len() != 0 {
				t.Fatalf("code = %d, stderr = %q", code, stderr.String())
			}
			mu.Lock()
			if method != test.method || path != test.path || !strings.Contains(body, test.bodyPart) {
				t.Fatalf("request = %s %s %s, want %s %s contains %q", method, path, body, test.method, test.path, test.bodyPart)
			}
			mu.Unlock()
		})
	}
}

func TestRunWiFiCLIRejectsUnsafeArguments(t *testing.T) {
	for _, test := range []struct {
		name string
		args []string
	}{
		{name: "缺少连接目标", args: []string{"wifi", "connect"}},
		{name: "CarPlay 缺少车辆信道", args: []string{"carplay", "network", "plan"}},
		{name: "CarPlay 缺少租约来源", args: []string{"carplay", "network", "acquire", "--channels", "149"}},
		{name: "CarPlay 超长租约", args: []string{"carplay", "network", "acquire", "--source", "carplay", "--channels", "149", "--ttl", "24h"}},
		{name: "CarPlay 状态拒绝额外参数", args: []string{"carplay", "network", "status", "extra"}},
		{name: "CarPlay 释放不接受信道", args: []string{"carplay", "network", "release", "--source", "carplay", "--channels", "149"}},
		{name: "CarPlay 拒绝重复信道", args: []string{"carplay", "network", "plan", "--channels", "149,149"}},
		{name: "CarPlay 拒绝信道间隙", args: []string{"carplay", "network", "plan", "--channels", "1,14,35"}},
		{name: "CarPlay 拒绝命令参数", args: []string{"carplay", "network", "plan", "--channels", "149", "--command", "reboot"}},
		{name: "同时提供 UUID 和 SSID", args: []string{"wifi", "connect", "--uuid", "a", "--ssid", "b"}},
		{name: "加密网络缺少标准输入", args: []string{"wifi", "connect", "--ssid", "Home", "--security", "wpa"}},
		{name: "开放网络不能输入密码", args: []string{"wifi", "connect", "--ssid", "Guest", "--security", "open", "--password-stdin"}},
		{name: "不接受命令行密码", args: []string{"wifi", "connect", "--ssid", "Home", "--security", "wpa", "--password", "secret"}},
		{name: "自动连接缺少 UUID", args: []string{"wifi", "autoconnect", "enable"}},
		{name: "断开拒绝额外参数", args: []string{"wifi", "disconnect", "--uuid", "a"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			code := runCLI(context.Background(), "/missing/control.sock", test.args, strings.NewReader(""), &stdout, &stderr)
			if code != 2 || stdout.Len() != 0 || stderr.Len() == 0 {
				t.Fatalf("code = %d, stdout = %q, stderr = %q", code, stdout.String(), stderr.String())
			}
		})
	}
}
