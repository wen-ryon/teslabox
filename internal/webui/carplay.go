package webui

import (
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httputil"
	"os"
	"path/filepath"
	"strings"
	"time"
)

type handlerSettings struct {
	carplay           http.Handler
	carplayHomeDomain string
}

// HandlerOption 配置可选页面，保持现有管理入口的默认行为。
type HandlerOption func(*handlerSettings)

// WithCarPlay 将本机播放器纳入现有网页认证边界。
func WithCarPlay(handler http.Handler) HandlerOption {
	return func(settings *handlerSettings) { settings.carplay = handler }
}

// WithCarPlayHome 只替换专用 HTTPS 域名首页，保留局域网管理入口。
func WithCarPlayHome(domain string) HandlerOption {
	return func(settings *handlerSettings) { settings.carplayHomeDomain = domain }
}

// NewCarPlayProxy 只代理固定本机接收端，浏览器不能指定目标或取得接收端令牌。
func NewCarPlayProxy(tokenPath string) http.Handler {
	return newCarPlayProxy(tokenPath, "127.0.0.1:8767")
}

func newCarPlayProxy(tokenPath, backend string) http.Handler {
	allowed := map[string]bool{"/api/status": true, "/video": true}
	proxy := &httputil.ReverseProxy{
		Rewrite: func(request *httputil.ProxyRequest) {
			request.Out.URL.Scheme = "http"
			request.Out.URL.Host = backend
			request.Out.URL.Path = strings.TrimPrefix(request.In.URL.Path, "/carplay")
			request.Out.URL.RawPath = ""
			request.Out.Host = backend
			request.Out.Header.Del("Authorization")
			request.Out.Header.Set("Origin", "http://"+backend)
		},
		Transport: &http.Transport{Proxy: nil, DialContext: (&net.Dialer{Timeout: 3 * time.Second}).DialContext, ResponseHeaderTimeout: 5 * time.Second},
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			http.Error(w, "CarPlay 接收端暂时不可用", http.StatusServiceUnavailable)
		},
		ModifyResponse: func(response *http.Response) error {
			response.Header.Del("Set-Cookie")
			response.Header.Set("Cache-Control", "no-store")
			if response.StatusCode == http.StatusUnauthorized || response.StatusCode == http.StatusForbidden {
				// 管理会话已经验证；接收端重启造成的令牌窗口应重试，不要求用户反复登录。
				response.Body.Close()
				response.StatusCode = http.StatusServiceUnavailable
				response.Status = "503 Service Unavailable"
				response.Body = io.NopCloser(strings.NewReader("CarPlay 接收端正在恢复"))
				response.ContentLength = -1
				response.Header.Del("Content-Length")
			}
			return nil
		},
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path := strings.TrimPrefix(r.URL.Path, "/carplay")
		if path == "/api/phones" && r.Method == http.MethodGet && r.URL.RawQuery == "" && r.URL.RawPath == "" && sameOrigin(r) {
			serveCarPlayPhones(w, filepath.Join(filepath.Dir(tokenPath), "status.json"))
			return
		}
		if path == "/api/network" && r.Method == http.MethodGet && r.URL.RawQuery == "" && sameOrigin(r) {
			serveCarPlayNetwork(w, filepath.Join(filepath.Dir(tokenPath), "status.json"))
			return
		}
		if r.Method != http.MethodGet || !allowed[path] || r.URL.RawQuery != "" || r.URL.RawPath != "" {
			http.NotFound(w, r)
			return
		}
		scheme := "http"
		if r.TLS != nil {
			scheme = "https"
		}
		if !sameOrigin(r) || (path == "/video" && r.Header.Get("Origin") != scheme+"://"+r.Host) {
			http.Error(w, "请求来源无效", http.StatusForbidden)
			return
		}
		token, err := readCarPlayToken(tokenPath)
		if err != nil {
			http.Error(w, "CarPlay 尚未就绪", http.StatusServiceUnavailable)
			return
		}
		// 克隆请求，接收端只看到本机令牌，不转发管理会话及用户提交的 Cookie。
		request := r.Clone(r.Context())
		request.Header.Set("Cookie", "tbcp="+token)
		proxy.ServeHTTP(w, request)
	})
}

// 只公开真实上游状态，车机离线连接兼容不能被当成互联网可用。
func serveCarPlayNetwork(w http.ResponseWriter, path string) {
	info, err := os.Lstat(path)
	if err != nil || !info.Mode().IsRegular() || info.Size() > 16384 || time.Since(info.ModTime()) > 90*time.Second {
		http.Error(w, "CarPlay 网络状态暂未就绪", http.StatusServiceUnavailable)
		return
	}
	file, err := os.Open(path)
	if err != nil {
		http.Error(w, "CarPlay 网络状态不可用", http.StatusServiceUnavailable)
		return
	}
	defer file.Close()
	actual, err := file.Stat()
	if err != nil || !os.SameFile(info, actual) {
		http.Error(w, "CarPlay 网络状态已变化", http.StatusServiceUnavailable)
		return
	}
	var value struct {
		Running  bool `json:"running"`
		Upstream *struct {
			Source    string `json:"source"`
			Interface string `json:"interface"`
			Internet  bool   `json:"internet"`
		} `json:"upstream"`
	}
	if json.NewDecoder(io.LimitReader(file, 16385)).Decode(&value) != nil || !value.Running || value.Upstream == nil {
		http.Error(w, "CarPlay 网络检测中", http.StatusServiceUnavailable)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	_ = json.NewEncoder(w).Encode(value.Upstream)
}

func readCarPlayToken(path string) (string, error) {
	info, err := os.Lstat(path)
	if err != nil || !info.Mode().IsRegular() || privateCarPlayToken(path, info) != nil {
		return "", errors.New("CarPlay 私有令牌不可用")
	}
	file, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer file.Close()
	actual, err := file.Stat()
	if err != nil || !os.SameFile(info, actual) {
		return "", errors.New("CarPlay 令牌身份改变")
	}
	data, err := io.ReadAll(io.LimitReader(file, 66))
	defer clear(data)
	if err != nil || len(data) != 64 {
		return "", errors.New("CarPlay 令牌格式无效")
	}
	for _, value := range data {
		if !(value >= '0' && value <= '9' || value >= 'a' && value <= 'f' || value >= 'A' && value <= 'F') {
			return "", errors.New("CarPlay 令牌格式无效")
		}
	}
	return string(data), nil
}
