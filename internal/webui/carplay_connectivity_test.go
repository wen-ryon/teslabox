package webui

import (
	"bytes"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestCarPlayConnectivityDiagnosticsAreBoundedAndPrivate(t *testing.T) {
	var logs bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&logs, nil))
	handler := NewHandler(http.NotFoundHandler(), nil, logger, WithCarPlay(http.NotFoundHandler()))
	for _, target := range []string{
		"https://connman.vn.cloud.tesla.cn/online/status.html",
		"http://other.test/online/status.html",
	} {
		handler.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, target, nil))
	}
	if logs.Len() != 0 {
		t.Fatal("没有匹配明文检测却记录在线响应")
	}
	for range 3 {
		request := httptest.NewRequest(http.MethodGet, "http://connman.vn.cloud.tesla.cn/online/status.html?token=private", nil)
		request.Header.Set("Cookie", "password=private")
		handler.ServeHTTP(httptest.NewRecorder(), request)
	}
	if strings.Count(logs.String(), "CarPlay 车机联网检测已处理") != 1 ||
		!strings.Contains(logs.String(), `"http_status":200`) || strings.Contains(logs.String(), "private") {
		t.Fatalf("日志未限频或泄露请求字段: %s", logs.String())
	}
}

func TestCarPlayConnectivityMatchesVehicleProbeOnly(t *testing.T) {
	for _, test := range []struct {
		name, method, url string
		enabled           bool
		online            bool
	}{
		{"get", "GET", "http://connman.vn.cloud.tesla.cn/online/status.html", true, true},
		{"head", "HEAD", "http://connman.vn.cloud.tesla.cn/online/status.html", true, true},
		{"port", "GET", "http://connman.vn.cloud.tesla.cn:80/online/status.html", true, true},
		{"wrong host", "GET", "http://api-prd.vn.cloud.tesla.cn/online/status.html", true, false},
		{"wrong path", "GET", "http://connman.vn.cloud.tesla.cn/api", true, false},
		{"tls", "GET", "https://connman.vn.cloud.tesla.cn/online/status.html", true, false},
		{"disabled", "GET", "http://connman.vn.cloud.tesla.cn/online/status.html", false, false},
		{"post", "POST", "http://connman.vn.cloud.tesla.cn/online/status.html", true, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			var options []HandlerOption
			if test.enabled {
				options = append(options, WithCarPlay(http.NotFoundHandler()))
			}
			handler := NewHandler(http.NotFoundHandler(), nil, slog.Default(), options...)
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, httptest.NewRequest(test.method, test.url, nil))
			online := len(response.Header()["X-ConnMan-Status"]) == 1 && response.Header()["X-ConnMan-Status"][0] == "online"
			if online != test.online {
				t.Fatalf("status=%d headers=%v", response.Code, response.Header())
			}
			if test.online {
				if response.Code != 200 || response.Header().Get("Content-Length") != "46" {
					t.Fatalf("probe response=%v", response.Result())
				}
				if test.method == "HEAD" && response.Body.Len() != 0 {
					t.Fatal("HEAD returned a body")
				}
				if test.method == "GET" && response.Body.String() != carPlayOnlineBody {
					t.Fatal("response differs from measured probe")
				}
				if strings.Contains(response.Body.String(), "管理") {
					t.Fatal("probe returned management page")
				}
			}
		})
	}
}

func TestCarPlayConnectivityPreservesLegacyHeaderOnWire(t *testing.T) {
	handler := NewHandler(http.NotFoundHandler(), nil, slog.Default(), WithCarPlay(http.NotFoundHandler()))
	server := httptest.NewServer(handler)
	defer server.Close()
	connection, err := net.DialTimeout("tcp", server.Listener.Addr().String(), 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer connection.Close()
	if err := connection.SetDeadline(time.Now().Add(5 * time.Second)); err != nil {
		t.Fatal(err)
	}
	_, err = io.WriteString(connection, "GET /online/status.html HTTP/1.1\r\nHost: connman.vn.cloud.tesla.cn\r\nConnection: close\r\n\r\n")
	if err != nil {
		t.Fatal(err)
	}
	wire, err := io.ReadAll(connection)
	if err != nil {
		t.Fatal(err)
	}
	// 检查网络上的原始字节，避免 Go 客户端规范化头名称掩盖兼容问题。
	if strings.Count(string(wire), "\r\nX-ConnMan-Status: online\r\n") != 1 || strings.Contains(string(wire), "X-Connman-Status:") {
		t.Fatalf("旧版 ConnMan 无法识别在线响应头：%q", wire)
	}
}
