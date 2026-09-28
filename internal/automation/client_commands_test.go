package automation

import (
	"bytes"
	"context"
	"io"
	"net"
	"net/http"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

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
		{name: "扫描", method: http.MethodPost, path: "/v1/network/wifi/scan", args: []string{"wifi", "scan"}},
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
		{name: "同时提供 UUID 和 SSID", args: []string{"wifi", "connect", "--uuid", "a", "--ssid", "b"}},
		{name: "加密网络缺少标准输入", args: []string{"wifi", "connect", "--ssid", "Home", "--security", "wpa"}},
		{name: "开放网络不能输入密码", args: []string{"wifi", "connect", "--ssid", "Guest", "--security", "open", "--password-stdin"}},
		{name: "不接受命令行密码", args: []string{"wifi", "connect", "--ssid", "Home", "--security", "wpa", "--password", "secret"}},
		{name: "自动连接缺少 UUID", args: []string{"wifi", "autoconnect", "enable"}},
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
