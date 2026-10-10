// Package automation 提供仅限本机脚本使用的受限设备自动化接口。
package automation

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/user"
	pathpkg "path"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/wen-ryon/teslabox/internal/deviceadmin"
	"github.com/wen-ryon/teslabox/internal/system"
)

const (
	DefaultSocketPath       = "/run/teslabox/automation/control.sock"
	DefaultStatePath        = "/run/teslabox/automation/ap-state.json"
	DefaultCarPlayStatePath = "/var/lib/teslabox/carplay-network-state.json"
	DefaultGroup            = "teslabox-automation"
	defaultLeaseTTL         = 5 * time.Minute
	minimumLeaseTTL         = 15 * time.Second
	maximumLeaseTTL         = time.Hour
)

var ownerPattern = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9._-]{0,63}$`)

// NetworkAdmin 是自动化接口允许使用的最小网络管理能力，不暴露任意系统命令。
type NetworkAdmin interface {
	APSettings(context.Context) (deviceadmin.APSettings, error)
	ConfigureAPRadio(context.Context, string, int) (deviceadmin.APSettings, error)
	SetAPRuntime(context.Context, bool) error
	WiFiStatus(context.Context) (deviceadmin.WiFiStatus, error)
	Scan(context.Context) ([]deviceadmin.AccessPoint, error)
	Connect(context.Context, deviceadmin.ConnectRequest) error
	DisconnectWiFi(context.Context) error
	SetAutoConnect(context.Context, deviceadmin.AutoConnectUpdate) error
}

// Config 定义本机接口与恢复记录；CarPlay 状态需在服务重启或掉电后保留。
type Config struct {
	SocketPath             string
	StatePath              string
	CarPlayStatePath       string
	CarPlayAccessDirectory string
	Group                  string
	Logger                 *slog.Logger
	Now                    func() time.Time
}

type lease struct {
	Owner     string    `json:"owner"`
	ExpiresAt time.Time `json:"expires_at"`
}

type persistedState struct {
	ManagedActivation bool    `json:"managed_activation"`
	Leases            []lease `json:"leases"`
}

// Service 管理自动化租约；同一时刻只允许一个热点状态变更。
type Service struct {
	admin                   NetworkAdmin
	config                  Config
	mu                      sync.Mutex
	leases                  map[string]time.Time
	managed                 bool
	carPlayLease            *deviceadmin.CarPlayNetworkLease
	carPlayRecoveryRequired bool
	httpServer              *http.Server
}

// EnsureAccessGroup 确保 OTA 升级后的旧系统也具备固定的最小权限用户组。
func EnsureAccessGroup(ctx context.Context, runner system.Runner) error {
	if runner == nil {
		return errors.New("自动化用户组初始化器不能为空")
	}
	if _, err := user.LookupGroup(DefaultGroup); err == nil {
		return nil
	}
	if _, err := runner.Run(ctx, "groupadd", "--system", DefaultGroup); err != nil {
		return fmt.Errorf("创建自动化用户组: %w", err)
	}
	return nil
}

// New 创建仅暴露白名单能力的本机自动化服务。
func New(admin NetworkAdmin, config Config) (*Service, error) {
	if admin == nil {
		return nil, errors.New("自动化 AP 管理器不能为空")
	}
	if config.SocketPath == "" {
		config.SocketPath = DefaultSocketPath
	}
	if config.StatePath == "" {
		config.StatePath = DefaultStatePath
	}
	if config.CarPlayStatePath == "" {
		config.CarPlayStatePath = DefaultCarPlayStatePath
	}
	if config.CarPlayAccessDirectory == "" {
		config.CarPlayAccessDirectory = "/run/teslabox/carplay"
	}
	if config.Group == "" {
		config.Group = DefaultGroup
	}
	if config.Logger == nil {
		config.Logger = slog.Default()
	}
	if config.Now == nil {
		config.Now = time.Now
	}
	if !safeRuntimePath(config.SocketPath) || !safeRuntimePath(config.StatePath) {
		return nil, errors.New("自动化 Socket 和状态文件必须位于 /run/teslabox")
	}
	if !safeRuntimePath(config.CarPlayAccessDirectory) {
		return nil, errors.New("CarPlay 私有交接目录必须位于 /run/teslabox")
	}
	if !strings.HasPrefix(pathpkg.Clean(config.CarPlayStatePath), "/var/lib/teslabox/") {
		return nil, errors.New("CarPlay 恢复记录必须位于 /var/lib/teslabox")
	}
	service := &Service{admin: admin, config: config, leases: make(map[string]time.Time)}
	if err := service.restoreCarPlayProtection(); err != nil {
		return nil, fmt.Errorf("恢复 CarPlay 网络保护: %w", err)
	}
	return service, nil
}

func safeRuntimePath(path string) bool {
	clean := pathpkg.Clean(path)
	const root = "/run/teslabox"
	return pathpkg.IsAbs(clean) && clean != root && strings.HasPrefix(clean, root+"/")
}

// Run 创建 Unix Socket 并运行服务，直到上下文取消。
func (s *Service) Run(ctx context.Context) error {
	runtimeDirectory := filepath.Dir(s.config.SocketPath)
	if err := os.MkdirAll(runtimeDirectory, 0o700); err != nil {
		return fmt.Errorf("创建自动化运行目录: %w", err)
	}
	if err := os.Chmod(runtimeDirectory, 0o700); err != nil {
		return fmt.Errorf("收紧自动化运行目录权限: %w", err)
	}
	if err := removeStaleSocket(s.config.SocketPath); err != nil {
		return err
	}
	listener, err := net.Listen("unix", s.config.SocketPath)
	if err != nil {
		return fmt.Errorf("监听自动化 Unix Socket: %w", err)
	}
	defer func() {
		_ = listener.Close()
		_ = os.Remove(s.config.SocketPath)
	}()
	if err := s.applySocketPermissions(); err != nil {
		return err
	}
	if err := s.restore(ctx); err != nil {
		s.config.Logger.Warn("恢复 AP 自动化租约失败", "error", err)
	}

	s.httpServer = &http.Server{
		Handler:           s.Handler(),
		ReadHeaderTimeout: 3 * time.Second,
		ReadTimeout:       5 * time.Second,
		WriteTimeout:      55 * time.Second,
		IdleTimeout:       30 * time.Second,
		MaxHeaderBytes:    8 * 1024,
	}
	go s.expireLoop(ctx)
	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		_ = s.httpServer.Shutdown(shutdownCtx)
	}()
	if err := s.httpServer.Serve(listener); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return fmt.Errorf("运行自动化 Unix Socket: %w", err)
	}
	return nil
}

func removeStaleSocket(path string) error {
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("检查自动化 Unix Socket: %w", err)
	}
	if info.Mode()&os.ModeSocket == 0 {
		return errors.New("自动化 Socket 路径已被非 Socket 文件占用")
	}
	if err := os.Remove(path); err != nil {
		return fmt.Errorf("清理旧自动化 Unix Socket: %w", err)
	}
	return nil
}

func (s *Service) applySocketPermissions() error {
	mode := os.FileMode(0o600)
	directoryMode := os.FileMode(0o700)
	group, err := user.LookupGroup(s.config.Group)
	if err == nil {
		gid, parseErr := strconv.Atoi(group.Gid)
		if parseErr != nil {
			return fmt.Errorf("解析自动化用户组: %w", parseErr)
		}
		if err := os.Chown(s.config.SocketPath, 0, gid); err != nil {
			return fmt.Errorf("设置自动化 Socket 用户组: %w", err)
		}
		if err := os.Chown(filepath.Dir(s.config.SocketPath), 0, gid); err != nil {
			return fmt.Errorf("设置自动化运行目录用户组: %w", err)
		}
		mode = 0o660
		directoryMode = 0o750
	} else {
		s.config.Logger.Warn("未找到自动化用户组，接口暂时仅允许 root 使用", "group", s.config.Group)
	}
	if err := os.Chmod(s.config.SocketPath, mode); err != nil {
		return fmt.Errorf("设置自动化 Socket 权限: %w", err)
	}
	if err := os.Chmod(filepath.Dir(s.config.SocketPath), directoryMode); err != nil {
		return fmt.Errorf("设置自动化运行目录权限: %w", err)
	}
	return nil
}

// Handler 返回便于测试的自动化 HTTP 路由。
func (s *Service) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/capabilities", s.handleCapabilities)
	mux.HandleFunc("GET /v1/network/ap/state", s.handleAPState)
	mux.HandleFunc("GET /v1/network/ap/config", s.handleAPConfig)
	mux.HandleFunc("PUT /v1/network/ap/config", s.handleAPConfig)
	mux.HandleFunc("PUT /v1/network/ap/leases/{owner}", s.handleAcquire)
	mux.HandleFunc("DELETE /v1/network/ap/leases/{owner}", s.handleRelease)
	mux.HandleFunc("GET /v1/network/wifi/status", s.handleWiFiStatus)
	mux.HandleFunc("POST /v1/network/carplay/plan", s.handleCarPlayPlan)
	mux.HandleFunc("GET /v1/network/carplay/state", s.handleCarPlayState)
	mux.HandleFunc("PUT /v1/network/carplay/leases/{owner}", s.handleCarPlayAcquire)
	mux.HandleFunc("DELETE /v1/network/carplay/leases/{owner}", s.handleCarPlayRelease)
	mux.HandleFunc("POST /v1/network/carplay/leases/{owner}/ap", s.handleCarPlayAPStart)
	mux.HandleFunc("POST /v1/network/carplay/leases/{owner}/upstream", s.handleCarPlayUpstream)
	mux.HandleFunc("POST /v1/network/wifi/scan", s.handleWiFiScan)
	mux.HandleFunc("POST /v1/network/wifi/connect", s.handleWiFiConnect)
	mux.HandleFunc("POST /v1/network/wifi/disconnect", s.handleWiFiDisconnect)
	mux.HandleFunc("PUT /v1/network/wifi/saved/{uuid}/autoconnect", s.handleWiFiAutoConnect)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		w.Header().Set("Cache-Control", "no-store")
		mux.ServeHTTP(w, r)
	})
}

func (s *Service) handleCapabilities(w http.ResponseWriter, _ *http.Request) {
	capabilities := []map[string]any{{
		"name": "network.ap.lease", "actions": []string{"read", "acquire", "renew", "release"},
	}, {
		"name": "network.wifi", "actions": []string{"list", "scan", "connect", "disconnect", "set_autoconnect"},
	}}
	if _, supported := s.admin.(carPlayNetworkPlanner); supported {
		capabilities = append(capabilities, map[string]any{
			"name": "network.carplay.plan", "actions": []string{"read"},
		})
	}
	if _, supported := s.admin.(carPlayNetworkCoordinator); supported {
		capabilities = append(capabilities, map[string]any{
			"name": "network.carplay.lease", "actions": []string{"read", "acquire", "renew", "release"},
		})
	}
	if _, supported := s.admin.(carPlayAPStarter); supported {
		capabilities = append(capabilities, map[string]any{"name": "network.carplay.ap", "actions": []string{"start"}})
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"api_version":  "v1",
		"capabilities": capabilities,
	})
}

func (s *Service) handleWiFiStatus(w http.ResponseWriter, r *http.Request) {
	status, err := s.admin.WiFiStatus(r.Context())
	if err != nil {
		s.writeOperationError(w, "读取 WiFi 列表失败", err)
		return
	}
	writeJSON(w, http.StatusOK, status)
}

func (s *Service) handleWiFiScan(w http.ResponseWriter, r *http.Request) {
	items, err := s.admin.Scan(r.Context())
	if err != nil {
		s.writeOperationError(w, "扫描 WiFi 失败", err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": items})
}

func (s *Service) handleWiFiConnect(w http.ResponseWriter, r *http.Request) {
	var input deviceadmin.ConnectRequest
	if !decodeRequest(w, r, &input) {
		return
	}
	if (input.UUID == "") == (input.SSID == "") {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "必须且只能指定连接 UUID 或 SSID"})
		return
	}
	if err := s.admin.Connect(r.Context(), input); err != nil {
		s.writeOperationError(w, "连接 WiFi 失败", err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"connected": true})
}

func (s *Service) handleWiFiDisconnect(w http.ResponseWriter, r *http.Request) {
	if r.ContentLength > 0 {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "断开 WiFi 不接受请求内容"})
		return
	}
	if err := s.admin.DisconnectWiFi(r.Context()); err != nil {
		s.writeOperationError(w, "断开 WiFi 失败", err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"disconnected": true})
}

func (s *Service) handleWiFiAutoConnect(w http.ResponseWriter, r *http.Request) {
	var input struct {
		Enabled *bool `json:"enabled"`
	}
	if !decodeRequest(w, r, &input) {
		return
	}
	if input.Enabled == nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "必须指定 enabled"})
		return
	}
	if err := s.admin.SetAutoConnect(r.Context(), deviceadmin.AutoConnectUpdate{
		UUID: r.PathValue("uuid"), Enabled: *input.Enabled,
	}); err != nil {
		s.writeOperationError(w, "更新 WiFi 自动连接设置失败", err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"updated": true})
}

func decodeRequest(w http.ResponseWriter, r *http.Request, target any) bool {
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "请求参数无效"})
		return false
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "请求包含多余内容"})
		return false
	}
	return true
}

func (s *Service) handleAPState(w http.ResponseWriter, r *http.Request) {
	settings, err := s.admin.APSettings(r.Context())
	if err != nil {
		s.writeOperationError(w, "读取热点状态失败", err)
		return
	}
	s.mu.Lock()
	s.purgeExpiredLocked(s.config.Now())
	activeLeases := len(s.leases)
	s.mu.Unlock()
	writeJSON(w, http.StatusOK, map[string]any{
		"available": settings.Available, "enabled": settings.Enabled,
		"persistent_enabled": settings.AutoConnect, "state": settings.State,
		"active_leases": activeLeases,
	})
}

func (s *Service) handleAPConfig(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodGet {
		settings, err := s.admin.APSettings(r.Context())
		if err != nil {
			s.writeOperationError(w, "读取热点配置失败", err)
			return
		}
		writeJSON(w, http.StatusOK, apConfigResponse(settings))
		return
	}
	var input struct {
		Band    string `json:"band"`
		Channel int    `json:"channel"`
	}
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&input); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "请求参数无效"})
		return
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "请求包含多余内容"})
		return
	}
	settings, err := s.admin.ConfigureAPRadio(r.Context(), input.Band, input.Channel)
	if err != nil {
		s.writeOperationError(w, "设置热点频段和信道失败", err)
		return
	}
	writeJSON(w, http.StatusOK, apConfigResponse(settings))
}

func apConfigResponse(settings deviceadmin.APSettings) map[string]any {
	return map[string]any{
		"available": settings.Available, "enabled": settings.Enabled,
		"band": settings.Band, "channel": settings.Channel,
		"active_band": settings.ActiveBand, "active_channel": settings.ActiveChannel,
		"concurrency_mode":        settings.ConcurrencyMode,
		"max_concurrent_channels": settings.MaxConcurrentChannels,
		"capability_notice":       settings.CapabilityNotice,
		"allowed_channels":        settings.AllowedChannels,
	}
}

func (s *Service) handleAcquire(w http.ResponseWriter, r *http.Request) {
	owner := r.PathValue("owner")
	if !ownerPattern.MatchString(owner) {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "租约名称只能包含字母、数字、点、下划线和连字符，且最长 64 字符"})
		return
	}
	var input struct {
		TTLSeconds int64 `json:"ttl_seconds"`
	}
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&input); err != nil && !errors.Is(err, io.EOF) {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "请求参数无效"})
		return
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "请求包含多余内容"})
		return
	}
	ttl := defaultLeaseTTL
	if input.TTLSeconds != 0 {
		ttl = time.Duration(input.TTLSeconds) * time.Second
	}
	if ttl < minimumLeaseTTL || ttl > maximumLeaseTTL {
		writeJSON(w, http.StatusUnprocessableEntity, map[string]string{"error": "租约有效期必须在 15 秒到 1 小时之间"})
		return
	}
	expiresAt, settings, err := s.acquire(r.Context(), owner, ttl)
	if err != nil {
		s.writeOperationError(w, "开启热点失败", err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"owner": owner, "expires_at": expiresAt, "enabled": settings.Enabled,
		"persistent_enabled": settings.AutoConnect,
	})
}

func (s *Service) handleRelease(w http.ResponseWriter, r *http.Request) {
	owner := r.PathValue("owner")
	if !ownerPattern.MatchString(owner) {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "租约名称无效"})
		return
	}
	released, settings, err := s.release(r.Context(), owner)
	if err != nil {
		s.writeOperationError(w, "释放热点租约失败", err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"owner": owner, "released": released, "enabled": settings.Enabled,
		"persistent_enabled": settings.AutoConnect,
	})
}

func (s *Service) acquire(ctx context.Context, owner string, ttl time.Duration) (time.Time, deviceadmin.APSettings, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.carPlayRecoveryRequired || s.carPlayLease != nil {
		return time.Time{}, deviceadmin.APSettings{}, carPlayLeaseBusy()
	}
	now := s.config.Now()
	s.purgeExpiredLocked(now)
	settings, err := s.admin.APSettings(ctx)
	if err != nil {
		return time.Time{}, settings, err
	}
	if !settings.Available {
		return time.Time{}, settings, deviceadmin.ErrWiFiUnavailable
	}
	previousExpiry, existed := s.leases[owner]
	previousManaged := s.managed
	s.leases[owner] = now.Add(ttl)
	if !settings.Enabled {
		if err := s.admin.SetAPRuntime(ctx, true); err != nil {
			if existed {
				s.leases[owner] = previousExpiry
			} else {
				delete(s.leases, owner)
			}
			return time.Time{}, settings, err
		}
		s.managed = true
	}
	if err := s.persistLocked(); err != nil {
		persistErr := err
		if !settings.Enabled && !previousManaged {
			if rollbackErr := s.admin.SetAPRuntime(ctx, false); rollbackErr != nil {
				persistErr = errors.Join(persistErr, fmt.Errorf("回滚临时热点: %w", rollbackErr))
			}
		}
		s.managed = previousManaged
		if existed {
			s.leases[owner] = previousExpiry
		} else {
			delete(s.leases, owner)
		}
		return time.Time{}, settings, persistErr
	}
	settings, err = s.admin.APSettings(ctx)
	return now.Add(ttl), settings, err
}

func (s *Service) release(ctx context.Context, owner string) (bool, deviceadmin.APSettings, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.purgeExpiredLocked(s.config.Now())
	if _, exists := s.leases[owner]; !exists {
		settings, err := s.admin.APSettings(ctx)
		return false, settings, err
	}
	previousExpiry := s.leases[owner]
	delete(s.leases, owner)
	if len(s.leases) == 0 {
		if err := s.stopManagedAPLocked(ctx); err != nil {
			s.leases[owner] = previousExpiry
			return false, deviceadmin.APSettings{}, err
		}
	}
	if err := s.persistLocked(); err != nil {
		s.leases[owner] = previousExpiry
		return false, deviceadmin.APSettings{}, err
	}
	settings, err := s.admin.APSettings(ctx)
	return true, settings, err
}

func (s *Service) stopManagedAPLocked(ctx context.Context) error {
	if !s.managed {
		return nil
	}
	settings, err := s.admin.APSettings(ctx)
	if err != nil {
		return err
	}
	if settings.AutoConnect {
		s.managed = false
		return nil
	}
	if settings.Enabled {
		if err := s.admin.SetAPRuntime(ctx, false); err != nil {
			return err
		}
		s.managed = false
		return nil
	}
	s.managed = false
	return nil
}

func (s *Service) expireLoop(ctx context.Context) {
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			s.expire(ctx)
		}
	}
}

func (s *Service) expire(ctx context.Context) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.carPlayRecoveryRequired {
		return
	}
	if s.carPlayLease != nil && !s.carPlayLease.ExpiresAt.After(s.config.Now()) {
		operationCtx, cancel := context.WithTimeout(ctx, 50*time.Second)
		err := s.releaseCarPlayLocked(operationCtx, s.carPlayLease.Owner)
		cancel()
		if err != nil {
			s.config.Logger.Warn("CarPlay 网络租约到期后恢复失败，将保留记录重试", "error", err)
		}
	}
	changed := s.purgeExpiredLocked(s.config.Now())
	if !changed && !(len(s.leases) == 0 && s.managed) {
		return
	}
	if len(s.leases) == 0 {
		operationCtx, cancel := context.WithTimeout(ctx, 25*time.Second)
		defer cancel()
		if err := s.stopManagedAPLocked(operationCtx); err != nil {
			s.config.Logger.Warn("AP 自动化租约到期后关闭热点失败", "error", err)
			return
		}
	}
	if err := s.persistLocked(); err != nil {
		s.config.Logger.Warn("保存 AP 自动化租约状态失败", "error", err)
	}
}

func (s *Service) purgeExpiredLocked(now time.Time) bool {
	changed := false
	for owner, expiresAt := range s.leases {
		if !expiresAt.After(now) {
			delete(s.leases, owner)
			changed = true
		}
	}
	return changed
}

func (s *Service) restore(ctx context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.carPlayRecoveryRequired || s.carPlayLease != nil {
		// 保留旧 AP 记录，不在 CarPlay 恢复期间拉起或关闭另一个热点。
		return nil
	}
	content, err := os.ReadFile(s.config.StatePath)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if len(content) > 64*1024 {
		return errors.New("自动化租约状态文件过大")
	}
	decoder := json.NewDecoder(strings.NewReader(string(content)))
	decoder.DisallowUnknownFields()
	var state persistedState
	if err := decoder.Decode(&state); err != nil {
		return err
	}
	s.managed = state.ManagedActivation
	for _, item := range state.Leases {
		if ownerPattern.MatchString(item.Owner) {
			s.leases[item.Owner] = item.ExpiresAt
		}
	}
	s.purgeExpiredLocked(s.config.Now())
	if len(s.leases) == 0 {
		if err := s.stopManagedAPLocked(ctx); err != nil {
			return err
		}
	} else {
		settings, settingsErr := s.admin.APSettings(ctx)
		if settingsErr != nil {
			return settingsErr
		}
		if !settings.Enabled {
			if err := s.admin.SetAPRuntime(ctx, true); err != nil {
				return err
			}
			s.managed = true
		}
	}
	return s.persistLocked()
}

func (s *Service) persistLocked() error {
	state := persistedState{ManagedActivation: s.managed, Leases: make([]lease, 0, len(s.leases))}
	for owner, expiresAt := range s.leases {
		state.Leases = append(state.Leases, lease{Owner: owner, ExpiresAt: expiresAt})
	}
	content, err := json.Marshal(state)
	if err != nil {
		return err
	}
	return writeRuntimeState(s.config.StatePath, content)
}

func writeRuntimeState(path string, content []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	temporary, err := os.CreateTemp(filepath.Dir(path), ".automation-state-*")
	if err != nil {
		return err
	}
	temporaryPath := temporary.Name()
	defer os.Remove(temporaryPath)
	if err := temporary.Chmod(0o600); err != nil {
		temporary.Close()
		return err
	}
	if _, err := temporary.Write(content); err != nil {
		temporary.Close()
		return err
	}
	if err := temporary.Sync(); err != nil {
		temporary.Close()
		return err
	}
	if err := temporary.Close(); err != nil {
		return err
	}
	if err := os.Rename(temporaryPath, path); err != nil {
		return err
	}
	return syncStateDirectory(filepath.Dir(path))
}

func (s *Service) writeOperationError(w http.ResponseWriter, message string, err error) {
	s.config.Logger.Warn(message, "error", err)
	status := http.StatusServiceUnavailable
	response := map[string]string{"error": message}
	var inputError *deviceadmin.InputError
	if errors.As(err, &inputError) {
		status = http.StatusUnprocessableEntity
		response["error"] = inputError.Error()
	}
	if errors.Is(err, deviceadmin.ErrAPActive) {
		status = http.StatusConflict
		response["error"] = "热点正在占用无线网卡，当前设备不支持 AP+STA 并发"
	}
	if errors.Is(err, deviceadmin.ErrWiFiUnavailable) {
		response["error"] = deviceadmin.ErrWiFiUnavailable.Error()
	}
	if errors.Is(err, deviceadmin.ErrUnsupportedSecurity) {
		status = http.StatusUnprocessableEntity
		response["error"] = deviceadmin.ErrUnsupportedSecurity.Error()
	}
	var operationError *deviceadmin.APOperationError
	if errors.As(err, &operationError) {
		status = http.StatusConflict
		response["error"] = operationError.Message
		response["code"] = operationError.Code
		response["suggestion"] = operationError.Suggestion
	}
	writeJSON(w, status, response)
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}
