// Package runtimecomponents 按主程序绑定的不可变描述交付公开运行时，离线时不阻断录像。
package runtimecomponents

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"
)

const maximumArchive = 256 << 20

var digestPattern = regexp.MustCompile(`^[a-f0-9]{64}$`)

// Component 的摘要受签名主 ELF 保护，下载地址不能在设备上替换为 latest。
type Component struct {
	ID      string `json:"id"`
	Version string `json:"version"`
	URL     string `json:"url"`
	Size    int64  `json:"size"`
	SHA256  string `json:"sha256"`
	Format  string `json:"format"`
}

// Descriptor 将一个主程序绑定到有限的固定组件集合。
type Descriptor struct {
	Schema     int         `json:"schema"`
	Components []Component `json:"components"`
}

// Parse 拒绝未知字段和不完整发行描述，避免空配置冒充产品交付。
func Parse(data []byte) (Descriptor, error) {
	var d Descriptor
	if len(data) > 32768 {
		return d, errors.New("组件描述过大")
	}
	dec := json.NewDecoder(strings.NewReader(string(data)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&d); err != nil {
		return d, errors.New("组件描述无效")
	}
	var extra any
	if dec.Decode(&extra) != io.EOF {
		return d, errors.New("组件描述存在多余内容")
	}
	if d.Schema != 1 || len(d.Components) != 3 {
		return d, errors.New("发行组件必须完整包含媒体、NAS 和 CarPlay")
	}
	seen := map[string]bool{}
	for _, c := range d.Components {
		if err := validate(c); err != nil {
			return d, err
		}
		if seen[c.ID] {
			return d, errors.New("组件重复")
		}
		seen[c.ID] = true
	}
	return d, nil
}
func validate(c Component) error {
	formats := map[string]string{"media": "tar.gz", "carplay": "tar.gz", "nas": "zip"}
	if formats[c.ID] == "" || c.Format != formats[c.ID] || c.Version == "" || len(c.Version) > 128 || strings.ContainsAny(c.Version, "\x00\r\n/\\ ") || !digestPattern.MatchString(c.SHA256) || c.Size <= 0 || c.Size > maximumArchive {
		return errors.New("组件版本、摘要或长度无效")
	}
	u, err := url.Parse(c.URL)
	if err != nil || u.Scheme != "https" || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || u.Path == "" || u.Path == "/" || strings.ContainsAny(c.URL, "\x00\r\n\t ") {
		return errors.New("组件必须绑定固定 HTTPS 下载地址")
	}
	return nil
}

// Status 只暴露有限阶段，不将可能含凭据的网络错误传到日志或页面。
type Status struct {
	ID      string `json:"id"`
	Version string `json:"version"`
	SHA256  string `json:"sha256"`
	State   string `json:"state"`
	Error   string `json:"error,omitempty"`
}

// Installer 必须在完整摘要验证后预检并原子激活，失败保留旧组件。
type Installer func(context.Context, Component, string) error

// Config 支持受控离线输入和可注入 HTTP 客户端，不接受设备侧浮动描述。
type Config struct {
	Root          string
	Components    []Component
	Install       map[string]Installer
	HTTPClient    *http.Client
	RetryInterval time.Duration
	RequireRoot   bool
}

// Manager 的就绪状态与核心健康分开，缺网不能触发整板回滚。
type Manager struct {
	cfg    Config
	mu     sync.RWMutex
	states map[string]Status
}

// New 在任何下载或安装前检查完整描述与所有安装适配器。
func New(cfg Config) (*Manager, error) {
	if !filepath.IsAbs(cfg.Root) || filepath.Clean(cfg.Root) == string(filepath.Separator) {
		return nil, errors.New("组件缓存目录无效")
	}
	if cfg.RequireRoot && os.Geteuid() != 0 {
		return nil, errors.New("组件安装需要 root")
	}
	if len(cfg.Components) == 0 {
		return nil, errors.New("组件配置不能为空")
	}
	if cfg.RetryInterval <= 0 {
		cfg.RetryInterval = 15 * time.Second
	}
	if cfg.HTTPClient == nil {
		cfg.HTTPClient = &http.Client{Timeout: 15 * time.Minute}
	}
	client := *cfg.HTTPClient
	if client.Timeout <= 0 {
		client.Timeout = 15 * time.Minute
	}
	client.CheckRedirect = func(req *http.Request, via []*http.Request) error {
		if len(via) > 3 || req.URL.Scheme != "https" || req.URL.Host != via[0].URL.Host {
			return errors.New("组件下载重定向越界")
		}
		return nil
	}
	cfg.HTTPClient = &client
	cfg.Components = append([]Component(nil), cfg.Components...)
	installers := map[string]Installer{}
	for k, v := range cfg.Install {
		installers[k] = v
	}
	cfg.Install = installers
	m := &Manager{cfg: cfg, states: map[string]Status{}}
	for _, c := range cfg.Components {
		if err := validate(c); err != nil {
			return nil, err
		}
		if _, ok := m.states[c.ID]; ok || cfg.Install[c.ID] == nil {
			return nil, errors.New("组件重复或安装器缺失")
		}
		m.states[c.ID] = Status{ID: c.ID, Version: c.Version, SHA256: c.SHA256, State: "pending"}
	}
	return m, nil
}

// Status 返回副本，调用方不能改变后台状态。
func (m *Manager) Status() []Status {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make([]Status, 0, len(m.cfg.Components))
	for _, c := range m.cfg.Components {
		out = append(out, m.states[c.ID])
	}
	return out
}

// Ready 只在所需组件都完成本地自检及激活时返回 true。
func (m *Manager) Ready(ids ...string) bool {
	m.mu.RLock()
	defer m.mu.RUnlock()
	for _, id := range ids {
		if m.states[id].State != "ready" {
			return false
		}
	}
	return true
}
func (m *Manager) state(id, phase, reason string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	s := m.states[id]
	s.State = phase
	s.Error = reason
	m.states[id] = s
}
func archiveName(c Component) string { return c.SHA256 + "." + c.Format }

// Seed 从明确的离线输入校验完整归档后入库，与在线路径共用缓存核验。
func (m *Manager) Seed(ctx context.Context, id, source string) error {
	for _, c := range m.cfg.Components {
		if c.ID == id {
			return m.seed(ctx, c, source)
		}
	}
	return errors.New("未知组件")
}
func (m *Manager) seed(ctx context.Context, c Component, source string) error {
	if err := secureDirectory(m.cfg.Root); err != nil {
		return err
	}
	unlock, err := lock(ctx, m.cfg.Root, c.ID)
	if err != nil {
		return err
	}
	defer unlock()
	if err := checkFile(source); err != nil {
		return err
	}
	in, err := os.Open(source)
	if err != nil {
		return err
	}
	defer in.Close()
	return m.store(ctx, c, in)
}
func (m *Manager) store(ctx context.Context, c Component, in io.Reader) error {
	f, err := os.CreateTemp(m.cfg.Root, ".archive-")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	defer f.Close()
	if err = f.Chmod(0600); err != nil {
		return err
	}
	hash := sha256.New()
	n, err := io.Copy(io.MultiWriter(f, hash), io.LimitReader(contextReader{ctx, in}, c.Size+1))
	if err != nil {
		return err
	}
	if n != c.Size || hex.EncodeToString(hash.Sum(nil)) != c.SHA256 {
		return errors.New("组件长度或 SHA256 不匹配")
	}
	if err = f.Sync(); err != nil {
		return err
	}
	if err = f.Close(); err != nil {
		return err
	}
	target := filepath.Join(m.cfg.Root, archiveName(c))
	if _, err = os.Lstat(target); err == nil {
		if err = checkFile(target); err != nil {
			return err
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if err = replaceArchive(f.Name(), target); err != nil {
		return err
	}
	return syncDirectory(m.cfg.Root)
}
func verifyArchive(ctx context.Context, c Component, p string) error {
	if err := checkFile(p); err != nil {
		return err
	}
	f, err := os.Open(p)
	if err != nil {
		return err
	}
	defer f.Close()
	stat, err := f.Stat()
	if err != nil {
		return err
	}
	if stat.Size() != c.Size {
		return errors.New("组件归档长度改变")
	}
	hash := sha256.New()
	if _, err = io.Copy(hash, contextReader{ctx, f}); err != nil {
		return err
	}
	if hex.EncodeToString(hash.Sum(nil)) != c.SHA256 {
		return errors.New("组件缓存内容改变")
	}
	return nil
}

// Ensure 可在首装和后台重试共用；已有完整归档不再访问公网。
func (m *Manager) Ensure(ctx context.Context, id string) error {
	for _, c := range m.cfg.Components {
		if c.ID == id {
			return m.ensure(ctx, c)
		}
	}
	return errors.New("未知组件")
}
func (m *Manager) ensure(ctx context.Context, c Component) error {
	if err := secureDirectory(m.cfg.Root); err != nil {
		m.state(c.ID, "failed", "组件缓存目录不可用")
		return err
	}
	unlock, err := lock(ctx, m.cfg.Root, c.ID)
	if err != nil {
		return err
	}
	defer unlock()
	archive := filepath.Join(m.cfg.Root, archiveName(c))
	if err = verifyArchive(ctx, c, archive); err != nil {
		// 不复用损坏或由链接替换的缓存；可信下载也不能覆盖不安全对象。
		if _, e := os.Lstat(archive); e == nil {
			if e = checkFile(archive); e != nil {
				m.state(c.ID, "failed", "组件缓存不安全")
				return e
			}
		} else if !errors.Is(e, os.ErrNotExist) {
			return e
		}
		m.state(c.ID, "downloading", "")
		req, e := http.NewRequestWithContext(ctx, http.MethodGet, c.URL, nil)
		if e != nil {
			return e
		}
		req.Header.Set("Accept-Encoding", "identity")
		response, e := m.cfg.HTTPClient.Do(req)
		if e != nil {
			m.state(c.ID, "waiting_network", "组件下载暂不可用")
			return errors.New("组件下载暂不可用")
		}
		if response.StatusCode != http.StatusOK || response.Uncompressed || response.ContentLength >= 0 && response.ContentLength != c.Size {
			response.Body.Close()
			m.state(c.ID, "failed", "组件下载响应无效")
			return errors.New("组件下载响应无效")
		}
		e = m.store(ctx, c, response.Body)
		closeErr := response.Body.Close()
		if e = errors.Join(e, closeErr); e != nil {
			m.state(c.ID, "failed", "组件下载校验失败")
			return errors.New("组件下载校验失败")
		}
	}
	m.state(c.ID, "installing", "")
	if err = m.cfg.Install[c.ID](ctx, c, archive); err != nil {
		m.state(c.ID, "failed", "组件本地安装或自检失败")
		return errors.New("组件本地安装或自检失败")
	}
	m.state(c.ID, "ready", "")
	return nil
}

// Run 组件各自有限重试，取消后等待全部工作退出，避免遗留安装进程。
func (m *Manager) Run(ctx context.Context) {
	var wg sync.WaitGroup
	for _, c := range m.cfg.Components {
		wg.Add(1)
		go func(c Component) {
			defer wg.Done()
			for ctx.Err() == nil {
				if m.Ensure(ctx, c.ID) == nil {
					return
				}
				timer := time.NewTimer(m.cfg.RetryInterval)
				select {
				case <-ctx.Done():
					timer.Stop()
					return
				case <-timer.C:
				}
			}
		}(c)
	}
	wg.Wait()
}

// Gate 只阻断依赖缺失的请求；外层登录保护仍然生效。
func (m *Manager) Gate(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet && r.URL.Path == "/api/v1/runtime-components" {
			w.Header().Set("Content-Type", "application/json")
			w.Header().Set("Cache-Control", "no-store")
			_ = json.NewEncoder(w).Encode(m.Status())
			return
		}
		ids := required(r)
		if len(ids) > 0 && !m.Ready(ids...) {
			w.Header().Set("Content-Type", "application/json")
			w.Header().Set("Retry-After", "15")
			w.WriteHeader(http.StatusServiceUnavailable)
			_ = json.NewEncoder(w).Encode(map[string]string{"error": "相关运行时尚未就绪，联网后会自动安装", "reason_code": "runtime_pending"})
			return
		}
		next.ServeHTTP(w, r)
	})
}
func required(r *http.Request) []string {
	p := r.URL.Path
	if strings.HasPrefix(p, "/api/v1/carplay/") && !strings.HasPrefix(p, "/api/v1/carplay/settings") && !strings.HasPrefix(p, "/api/v1/carplay/pairing") {
		return []string{"carplay"}
	}
	if strings.HasPrefix(p, "/api/v1/upload-targets") || strings.HasPrefix(p, "/api/v1/uploads") || strings.HasPrefix(p, "/api/v1/automatic-uploads") {
		if r.Method == http.MethodGet || r.Method == http.MethodDelete {
			return nil
		}
		return []string{"media", "nas"}
	}
	if (strings.HasPrefix(p, "/api/v1/media/") || strings.HasPrefix(p, "/api/v1/thumbnails/") || strings.HasPrefix(p, "/api/v1/videos/") && strings.HasSuffix(p, "/metadata") || strings.HasPrefix(p, "/api/v1/video-groups/") && strings.HasSuffix(p, "/composite") || p == "/api/v1/exports" && r.Method == http.MethodPost) && r.Method != http.MethodDelete {
		return []string{"media"}
	}
	return nil
}

type contextReader struct {
	ctx context.Context
	r   io.Reader
}

func (r contextReader) Read(p []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	return r.r.Read(p)
}

// ArchivePath 只用于首装预缓存，名称由绑定摘要生成，不读取外部清单路径。
func ArchivePath(root string, c Component) string { return filepath.Join(root, archiveName(c)) }

// WaitReady 供依赖工作进程等待，不延迟 HTTP、USB 和缓存引用恢复。
func (m *Manager) WaitReady(ctx context.Context, ids ...string) bool {
	timer := time.NewTicker(200 * time.Millisecond)
	defer timer.Stop()
	for !m.Ready(ids...) {
		select {
		case <-ctx.Done():
			return false
		case <-timer.C:
		}
	}
	return true
}

// ValidateFile 供交付工具在发布前校验固定输入，避免依赖仅存在于测试板。
func ValidateFile(ctx context.Context, c Component, p string) error {
	if err := validate(c); err != nil {
		return err
	}
	return verifyArchive(ctx, c, p)
}
