package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"mime"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/wen-ryon/teslabox/internal/deviceadmin"
	"github.com/wen-ryon/teslabox/internal/diagnostics"
	"github.com/wen-ryon/teslabox/internal/maintenancemode"
	"github.com/wen-ryon/teslabox/internal/media"
	"github.com/wen-ryon/teslabox/internal/mediaassets"
	"github.com/wen-ryon/teslabox/internal/service"
	"github.com/wen-ryon/teslabox/internal/storageadmin"
	"github.com/wen-ryon/teslabox/internal/system"
	"github.com/wen-ryon/teslabox/internal/teslacam"
	"github.com/wen-ryon/teslabox/internal/updater"
)

// Controller 定义 API 所需的最小状态和刷新能力。
type Controller interface {
	Snapshot() service.Snapshot
	AcquireMediaSnapshot() (service.Snapshot, func())
	Refresh(ctx context.Context) error
	Scan(ctx context.Context) error
	ActivateMedia(ctx context.Context) error
}

// DeviceAdmin 定义设置中心需要的无线和电源管理能力。
type DeviceAdmin interface {
	WiFiStatus(context.Context) (deviceadmin.WiFiStatus, error)
	Scan(context.Context) ([]deviceadmin.AccessPoint, error)
	Connect(context.Context, deviceadmin.ConnectRequest) error
	SetAutoConnect(context.Context, deviceadmin.AutoConnectUpdate) error
	UpdateIPv4(context.Context, deviceadmin.IPv4Update) error
	Forget(context.Context, string) error
	APSettings(context.Context) (deviceadmin.APSettings, error)
	UpdateAP(context.Context, deviceadmin.APUpdate) error
	SchedulePower(string) error
}

// StorageAdmin 定义设置中心的外接存储发现和显式初始化能力。
type StorageAdmin interface {
	Status(context.Context) (storageadmin.Status, error)
	Initialize(context.Context, storageadmin.InitializeRequest) (storageadmin.Candidate, error)
	Cancel(context.Context, storageadmin.CancelRequest) error
}

// GadgetAdmin 只开放受确认保护的安全断开和重新枚举能力。
type GadgetAdmin interface {
	PauseGadget(context.Context) (service.Snapshot, error)
	ResumeGadget(context.Context) (service.Snapshot, error)
}

// PowerAdmin 只暴露温重启恢复所需的延迟电源操作。
type PowerAdmin interface {
	SchedulePower(string) error
}

// MaintenanceAutomation 保存是否允许由 TeslaBox 自动断开并冷启动恢复 Gadget 的设备策略。
type MaintenanceAutomation interface {
	Settings() maintenancemode.Settings
	SetAutoRecovery(bool) (maintenancemode.Settings, error)
}

// MediaInspector 定义 API 所需的媒体能力和单文件探测边界。
type MediaInspector interface {
	Capabilities(context.Context) (media.Capabilities, error)
	Probe(context.Context, string) (media.Metadata, error)
}

// MediaAssetsAdmin 定义媒体中心受控上传、应用、预览与删除能力。
type MediaAssetsAdmin interface {
	Status(context.Context) (mediaassets.Status, error)
	UploadStream(context.Context, mediaassets.Category, func(func(mediaassets.UploadFile) error) error) ([]mediaassets.Asset, error)
	Delete(context.Context, string) (mediaassets.Status, error)
	DeleteExternal(context.Context, mediaassets.Asset) (mediaassets.Status, error)
	DeleteRecordings(context.Context, mediaassets.RecordingDeleteRequest) (mediaassets.RecordingDeleteResult, error)
	Apply(context.Context) (mediaassets.Status, error)
	OpenPreview(context.Context, string) (*os.File, mediaassets.Asset, error)
}

// MediaCompositor 定义请求取消后必须停止的实时合成边界。
type MediaCompositor interface {
	Stream(context.Context, media.CompositeRequest, io.Writer) error
}

// ExportAdmin 定义异步导出的任务边界，创建成功后由实现负责释放媒体快照租约。
type ExportAdmin interface {
	Create(media.ExportRequest, media.ExportSource, func()) (media.ExportTask, error)
	Task(string) (media.ExportTask, error)
	Cancel(string) (media.ExportTask, error)
	Open(string) (*os.File, media.ExportTask, error)
}

// UpdateAdmin 定义设置中心所需的签名更新能力。
type UpdateAdmin interface {
	Status() updater.Status
	Check(context.Context) (updater.Status, error)
	History(context.Context) (updater.ReleaseHistory, error)
	Select(context.Context, string) (updater.Status, error)
	StartDownload() (updater.Status, error)
	Install(context.Context) (updater.Status, error)
}

// Diagnostics 定义日志中心只能读取预设诊断来源的能力边界。
type Diagnostics interface {
	Logs(context.Context, string, int) (diagnostics.Report, error)
	Bundle(context.Context) (diagnostics.Bundle, error)
}

type handlerOptions struct {
	deviceAdmin     DeviceAdmin
	storageAdmin    StorageAdmin
	gadgetAdmin     GadgetAdmin
	mediaInspector  MediaInspector
	mediaAssets     MediaAssetsAdmin
	mediaCompositor MediaCompositor
	exportAdmin     ExportAdmin
	updateAdmin     UpdateAdmin
	diagnostics     Diagnostics
	automation      MaintenanceAutomation
}

// Option 配置可选的 API 能力。
type Option func(*handlerOptions)

// WithDeviceAdmin 启用设置中心的设备管理接口。
func WithDeviceAdmin(admin DeviceAdmin) Option {
	return func(options *handlerOptions) { options.deviceAdmin = admin }
}

// WithStorageAdmin 启用外接存储登记和初始化接口。
func WithStorageAdmin(admin StorageAdmin) Option {
	return func(options *handlerOptions) { options.storageAdmin = admin }
}

// WithGadgetAdmin 启用受确认保护的 USB Gadget 安全断开和重新枚举接口。
func WithGadgetAdmin(admin GadgetAdmin) Option {
	return func(options *handlerOptions) { options.gadgetAdmin = admin }
}

// WithMediaInspector 启用 Rockchip 媒体能力和录像技术元数据接口。
func WithMediaInspector(inspector MediaInspector) Option {
	return func(options *handlerOptions) { options.mediaInspector = inspector }
}

// WithMediaAssetsAdmin 启用媒体中心接口；所有写入仍由实现统一控制 Gadget 维护窗口。
func WithMediaAssetsAdmin(admin MediaAssetsAdmin) Option {
	return func(options *handlerOptions) { options.mediaAssets = admin }
}

// WithMediaCompositor 启用单分钟多视角服务端实时合成。
func WithMediaCompositor(compositor MediaCompositor) Option {
	return func(options *handlerOptions) { options.mediaCompositor = compositor }
}

// WithExportAdmin 启用跨分钟、自定义视角的异步视频导出接口。
func WithExportAdmin(admin ExportAdmin) Option {
	return func(options *handlerOptions) { options.exportAdmin = admin }
}

// WithUpdateAdmin 启用受保护的在线更新接口。
func WithUpdateAdmin(admin UpdateAdmin) Option {
	return func(options *handlerOptions) { options.updateAdmin = admin }
}

// WithDiagnostics 启用受限、脱敏的日志中心接口。
func WithDiagnostics(manager Diagnostics) Option {
	return func(options *handlerOptions) { options.diagnostics = manager }
}

// WithMaintenanceAutomation 启用设备级自动维护恢复设置。
func WithMaintenanceAutomation(automation MaintenanceAutomation) Option {
	return func(options *handlerOptions) { options.automation = automation }
}

// NewHandler 创建版本化 REST 路由并限制允许的方法。
func NewHandler(controller Controller, logger *slog.Logger, options ...Option) http.Handler {
	settings := handlerOptions{}
	for _, option := range options {
		option(&settings)
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
	})
	mux.HandleFunc("GET /api/v1/storage", func(w http.ResponseWriter, r *http.Request) {
		state := controller.Snapshot()
		state.Videos = nil
		writeJSON(w, http.StatusOK, state)
	})
	mux.HandleFunc("GET /api/v1/videos", func(w http.ResponseWriter, r *http.Request) {
		activateMedia(controller, logger, r.Context())
		writeVideoList(w, r, controller.Snapshot().Videos)
	})
	mux.HandleFunc("GET /api/v1/video-groups", func(w http.ResponseWriter, r *http.Request) {
		activateMedia(controller, logger, r.Context())
		writeVideoGroupList(w, r, controller.Snapshot().Videos)
	})
	mux.HandleFunc("GET /api/v1/video-sessions", func(w http.ResponseWriter, r *http.Request) {
		activateMedia(controller, logger, r.Context())
		writeVideoSessionList(w, r, controller.Snapshot().Videos)
	})
	mux.HandleFunc("GET /api/v1/video-sessions/{id}", func(w http.ResponseWriter, r *http.Request) {
		activateMedia(controller, logger, r.Context())
		writeVideoSession(w, r, controller.Snapshot().Videos)
	})
	mux.HandleFunc("GET /api/v1/thumbnails/{id}", func(w http.ResponseWriter, r *http.Request) {
		activateMedia(controller, logger, r.Context())
		state, release := controller.AcquireMediaSnapshot()
		defer release()
		serveThumbnail(w, r, state)
	})
	mux.HandleFunc("GET /api/v1/videos/{id}/content", func(w http.ResponseWriter, r *http.Request) {
		activateMedia(controller, logger, r.Context())
		state, release := controller.AcquireMediaSnapshot()
		defer release()
		serveVideo(w, r, state, false)
	})
	mux.HandleFunc("GET /api/v1/videos/{id}/download", func(w http.ResponseWriter, r *http.Request) {
		activateMedia(controller, logger, r.Context())
		state, release := controller.AcquireMediaSnapshot()
		defer release()
		serveVideo(w, r, state, true)
	})
	refresh := func(w http.ResponseWriter, r *http.Request) {
		if err := controller.Refresh(r.Context()); err != nil {
			logger.Warn("API 触发状态刷新失败", "error", err)
			writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "TeslaUSB 数据盘暂不可用"})
			return
		}
		state := controller.Snapshot()
		state.Videos = nil
		writeJSON(w, http.StatusOK, state)
	}
	mux.HandleFunc("POST /api/v1/refresh", refresh)
	mux.HandleFunc("POST /api/v1/scan", func(w http.ResponseWriter, r *http.Request) {
		if err := controller.Scan(r.Context()); err != nil {
			logger.Warn("API 触发媒体快照失败", "error", err)
			writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "TeslaCam 快照暂不可用"})
			return
		}
		state := controller.Snapshot()
		state.Videos = nil
		writeJSON(w, http.StatusOK, state)
	})
	if settings.deviceAdmin != nil {
		registerDeviceAdminRoutes(mux, settings.deviceAdmin, logger)
	}
	if settings.storageAdmin != nil {
		registerStorageAdminRoutes(mux, settings.storageAdmin, logger)
	}
	if settings.gadgetAdmin != nil {
		registerGadgetAdminRoutes(mux, settings.gadgetAdmin, settings.deviceAdmin, logger)
	}
	if settings.mediaInspector != nil {
		registerMediaRoutes(mux, controller, settings.mediaInspector, logger)
	}
	if settings.mediaAssets != nil {
		registerMediaAssetRoutes(mux, controller, settings.mediaAssets, settings.gadgetAdmin, settings.deviceAdmin, settings.automation, logger)
	}
	if settings.mediaCompositor != nil {
		registerCompositeRoutes(mux, controller, settings.mediaCompositor, logger)
	}
	if settings.exportAdmin != nil {
		registerExportRoutes(mux, controller, settings.exportAdmin, logger)
	}
	if settings.updateAdmin != nil {
		registerUpdateRoutes(mux, settings.updateAdmin, settings.gadgetAdmin, settings.deviceAdmin, settings.automation, logger)
	}
	if settings.automation != nil {
		registerMaintenanceRoutes(mux, settings.automation, logger)
	}
	if settings.diagnostics != nil {
		registerDiagnosticsRoutes(mux, settings.diagnostics, logger)
	}
	return requestLogger(mux, logger)
}

func activateMedia(controller Controller, logger *slog.Logger, ctx context.Context) {
	if err := controller.ActivateMedia(ctx); err != nil && !errors.Is(err, service.ErrSnapshotUnavailable) {
		logger.Warn("API 激活媒体浏览失败", "error", err)
	}
}

func registerGadgetAdminRoutes(mux *http.ServeMux, admin GadgetAdmin, power PowerAdmin, logger *slog.Logger) {
	mux.HandleFunc("POST /api/v1/gadget/disconnect", func(w http.ResponseWriter, r *http.Request) {
		var input struct {
			ConfirmPhysicalDisconnect bool `json:"confirm_physical_disconnect"`
		}
		if !decodeJSON(w, r, &input) {
			return
		}
		if !input.ConfirmPhysicalDisconnect {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "请先确认物理 OTG 已断开"})
			return
		}
		state, err := admin.PauseGadget(r.Context())
		if err != nil {
			logger.Warn("安全断开 USB Gadget 失败", "error", err)
			writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "USB Gadget 暂时无法安全断开"})
			return
		}
		state.Videos = nil
		writeJSON(w, http.StatusOK, state)
	})
	mux.HandleFunc("POST /api/v1/gadget/reconnect", func(w http.ResponseWriter, r *http.Request) {
		var input struct {
			ConfirmPhysicalConnection bool `json:"confirm_physical_connection"`
		}
		if !decodeJSON(w, r, &input) {
			return
		}
		if !input.ConfirmPhysicalConnection {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "请先物理插入 OTG 线，再确认准备下次冷启动"})
			return
		}
		state, err := admin.ResumeGadget(r.Context())
		if err != nil {
			logger.Warn("准备 USB Gadget 下次冷启动失败", "error", err)
			writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "USB Gadget 暂时无法准备下次冷启动"})
			return
		}
		state.Videos = nil
		writeJSON(w, http.StatusOK, state)
	})
	if power != nil {
		mux.HandleFunc("POST /api/v1/gadget/reboot-recover", func(w http.ResponseWriter, r *http.Request) {
			var input struct {
				ConfirmPhysicalConnection bool `json:"confirm_physical_connection"`
			}
			if !decodeJSON(w, r, &input) {
				return
			}
			if !input.ConfirmPhysicalConnection {
				writeJSON(w, http.StatusBadRequest, map[string]string{"error": "请先物理插入 OTG 线，再确认温重启恢复"})
				return
			}
			if _, err := admin.ResumeGadget(r.Context()); err != nil {
				logger.Warn("准备 USB Gadget 温重启恢复失败", "error", err)
				writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "USB Gadget 暂时无法准备温重启恢复"})
				return
			}
			if err := power.SchedulePower("reboot"); err != nil {
				logger.Warn("提交 USB Gadget 温重启失败", "error", err)
				writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "恢复状态已保存，但自动重启提交失败，请手工重启设备"})
				return
			}
			writeJSON(w, http.StatusAccepted, map[string]bool{"accepted": true})
		})
	}
}

func registerDiagnosticsRoutes(mux *http.ServeMux, manager Diagnostics, logger *slog.Logger) {
	mux.HandleFunc("GET /api/v1/logs", func(w http.ResponseWriter, r *http.Request) {
		source := strings.ToLower(strings.TrimSpace(r.URL.Query().Get("source")))
		if source == "" {
			source = diagnostics.SourceUSB
		}
		if !diagnostics.ValidSource(source) {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "不支持的日志来源"})
			return
		}
		limit := boundedInt(r.URL.Query().Get("lines"), 200, 1, 500)
		report, err := manager.Logs(r.Context(), source, limit)
		if err != nil {
			logger.Warn("读取诊断日志失败", "source", source, "error", err)
			writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "暂时无法读取系统日志"})
			return
		}
		writeJSON(w, http.StatusOK, report)
	})
	mux.HandleFunc("GET /api/v1/diagnostics/report", func(w http.ResponseWriter, r *http.Request) {
		bundle, err := manager.Bundle(r.Context())
		if err != nil {
			logger.Warn("生成完整诊断报告失败", "error", err)
			writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "暂时无法生成完整诊断报告"})
			return
		}
		w.Header().Set("Content-Disposition", mime.FormatMediaType("attachment", map[string]string{"filename": bundle.Filename}))
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.Header().Set("Cache-Control", "no-store")
		_, _ = w.Write(bundle.Content)
	})
	mux.HandleFunc("POST /api/v1/diagnostics/client-event", func(w http.ResponseWriter, r *http.Request) {
		var event clientDiagnosticEvent
		if !decodeJSON(w, r, &event) {
			return
		}
		if !event.valid() {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "不支持的客户端诊断事件"})
			return
		}
		event.normalize()
		logger.Warn("客户端播放诊断",
			"event", event.Event,
			"reason", event.Reason,
			"session_id", event.SessionID,
			"group_id", event.GroupID,
			"display", event.Display,
			"quality", event.Quality,
			"effective_quality", event.EffectiveQuality,
			"access_path", event.AccessPath,
			"network_type", event.NetworkType,
			"downlink_mbps", event.DownlinkMbps,
			"rtt_ms", event.RTTMS,
			"media_mbps", event.MediaMbps,
			"buffer_seconds", event.BufferSeconds,
			"stalls", event.Stalls,
			"media_error_code", event.MediaErrorCode,
			"ready_state", event.ReadyState,
			"user_agent", normalizeDiagnosticText(r.UserAgent(), 240),
		)
		writeJSON(w, http.StatusAccepted, map[string]bool{"accepted": true})
	})
}

type clientDiagnosticEvent struct {
	Event            string  `json:"event"`
	Reason           string  `json:"reason"`
	SessionID        string  `json:"session_id"`
	GroupID          string  `json:"group_id"`
	Display          string  `json:"display"`
	Quality          string  `json:"quality"`
	EffectiveQuality string  `json:"effective_quality"`
	AccessPath       string  `json:"access_path"`
	NetworkType      string  `json:"network_type"`
	DownlinkMbps     float64 `json:"downlink_mbps"`
	RTTMS            int     `json:"rtt_ms"`
	MediaMbps        float64 `json:"media_mbps"`
	BufferSeconds    float64 `json:"buffer_seconds"`
	Stalls           int     `json:"stalls"`
	MediaErrorCode   int     `json:"media_error_code"`
	ReadyState       int     `json:"ready_state"`
}

func (e *clientDiagnosticEvent) valid() bool {
	switch strings.ToLower(strings.TrimSpace(e.Event)) {
	case "media_error", "load_failed", "fallback_requested", "playback_stall":
		return true
	default:
		return false
	}
}

func (e *clientDiagnosticEvent) normalize() {
	e.Event = normalizeDiagnosticText(strings.ToLower(e.Event), 32)
	e.Reason = normalizeDiagnosticText(e.Reason, 180)
	e.SessionID = normalizeDiagnosticText(e.SessionID, 96)
	e.GroupID = normalizeDiagnosticText(e.GroupID, 96)
	e.Display = normalizeDiagnosticText(e.Display, 32)
	e.Quality = normalizeDiagnosticText(e.Quality, 32)
	e.EffectiveQuality = normalizeDiagnosticText(e.EffectiveQuality, 32)
	e.AccessPath = normalizeDiagnosticText(e.AccessPath, 64)
	e.NetworkType = normalizeDiagnosticText(e.NetworkType, 32)
	e.DownlinkMbps = max(0, min(e.DownlinkMbps, 10000))
	e.RTTMS = max(0, min(e.RTTMS, 600000))
	e.MediaMbps = max(0, min(e.MediaMbps, 10000))
	e.BufferSeconds = max(0, min(e.BufferSeconds, 86400))
	e.Stalls = max(0, min(e.Stalls, 100000))
	e.MediaErrorCode = max(0, min(e.MediaErrorCode, 10))
	e.ReadyState = max(0, min(e.ReadyState, 4))
}

func normalizeDiagnosticText(value string, limit int) string {
	value = strings.Join(strings.Fields(strings.TrimSpace(value)), " ")
	runes := []rune(value)
	if len(runes) > limit {
		return string(runes[:limit]) + "…"
	}
	return value
}

func registerUpdateRoutes(mux *http.ServeMux, admin UpdateAdmin, gadget GadgetAdmin, power PowerAdmin, automation MaintenanceAutomation, logger *slog.Logger) {
	mux.HandleFunc("GET /api/v1/update", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, admin.Status())
	})
	mux.HandleFunc("POST /api/v1/update/check", func(w http.ResponseWriter, r *http.Request) {
		status, err := admin.Check(r.Context())
		if err != nil {
			logger.Warn("检查在线更新失败", "error", err)
			writeJSON(w, http.StatusBadGateway, map[string]string{"error": err.Error()})
			return
		}
		writeJSON(w, http.StatusOK, status)
	})
	mux.HandleFunc("GET /api/v1/update/history", func(w http.ResponseWriter, r *http.Request) {
		history, err := admin.History(r.Context())
		if err != nil {
			logger.Warn("读取历史版本目录失败", "error", err)
			writeJSON(w, http.StatusBadGateway, map[string]string{"error": err.Error()})
			return
		}
		writeJSON(w, http.StatusOK, history)
	})
	mux.HandleFunc("POST /api/v1/update/select", func(w http.ResponseWriter, r *http.Request) {
		var request struct {
			Version string `json:"version"`
		}
		if !decodeJSON(w, r, &request) {
			return
		}
		status, err := admin.Select(r.Context(), request.Version)
		if err != nil {
			writeJSON(w, http.StatusConflict, map[string]string{"error": err.Error()})
			return
		}
		writeJSON(w, http.StatusOK, status)
	})
	mux.HandleFunc("POST /api/v1/update/download", func(w http.ResponseWriter, _ *http.Request) {
		status, err := admin.StartDownload()
		if err != nil {
			writeJSON(w, http.StatusConflict, map[string]string{"error": err.Error()})
			return
		}
		writeJSON(w, http.StatusAccepted, status)
	})
	mux.HandleFunc("POST /api/v1/update/install", func(w http.ResponseWriter, r *http.Request) {
		automatic := automaticUpdateRecoveryEnabled(automation)
		if automatic {
			if admin.Status().State != updater.StateDownloaded {
				writeJSON(w, http.StatusConflict, map[string]string{"error": "请先完成更新包下载后再执行自动维护安装"})
				return
			}
			if power == nil {
				writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "自动维护重启不可用；请关闭自动恢复或手工拔掉 OTG 后更新"})
				return
			}
			if err := beginAutomaticMaintenance(r.Context(), gadget); err != nil {
				logger.Warn("更新前自动断开 USB Gadget 失败", "error", err)
				writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "无法自动断开 USB Gadget；请关闭自动恢复或手工拔掉 OTG 后重试"})
				return
			}
			if _, err := gadget.ResumeGadget(r.Context()); err != nil {
				logger.Warn("更新前准备自动温重启恢复失败", "error", err)
				writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "无法准备自动温重启恢复；请在日志中心手工恢复"})
				return
			}
		}
		status, err := admin.Install(r.Context())
		if err != nil {
			logger.Warn("提交在线更新安装失败", "error", err)
			if automatic {
				if restartErr := power.SchedulePower("reboot"); restartErr != nil {
					logger.Warn("更新提交失败后自动温重启恢复失败", "error", restartErr)
					writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "更新未提交，且自动温重启恢复失败；请在日志中心手工执行温重启恢复"})
					return
				}
				writeJSON(w, http.StatusConflict, map[string]string{"error": "更新未提交，但已安排温重启恢复 USB Gadget；请等待设备重新上线"})
				return
			}
			writeJSON(w, http.StatusConflict, map[string]string{"error": err.Error()})
			return
		}
		writeJSON(w, http.StatusAccepted, status)
	})
}

func registerExportRoutes(mux *http.ServeMux, controller Controller, admin ExportAdmin, logger *slog.Logger) {
	mux.HandleFunc("POST /api/v1/exports", func(w http.ResponseWriter, r *http.Request) {
		var request media.ExportRequest
		if !decodeJSON(w, r, &request) {
			return
		}
		state, release := controller.AcquireMediaSnapshot()
		if state.MediaSnapshot == nil {
			release()
			writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "媒体快照暂不可用"})
			return
		}
		session, found := findVideoSession(state.Videos, request.SessionID)
		if !found {
			release()
			http.NotFound(w, r)
			return
		}
		if !exportSpaceAvailable(state, request) {
			release()
			writeJSON(w, http.StatusInsufficientStorage, map[string]string{"error": "数据盘剩余空间不足，无法在保留车端录像安全余量的同时完成导出"})
			return
		}
		source, err := exportSource(state.MediaSnapshot.MountPath, session)
		if err != nil {
			release()
			logger.Warn("准备导出录像失败", "session_id", request.SessionID, "error", err)
			writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "导出录像文件暂不可用"})
			return
		}
		task, err := admin.Create(request, source, release)
		if err != nil {
			writeExportError(w, logger, "创建导出任务失败", err)
			return
		}
		writeJSON(w, http.StatusAccepted, task)
	})
	mux.HandleFunc("GET /api/v1/exports/{id}", func(w http.ResponseWriter, r *http.Request) {
		task, err := admin.Task(r.PathValue("id"))
		if err != nil {
			writeExportError(w, logger, "读取导出任务失败", err)
			return
		}
		writeJSON(w, http.StatusOK, task)
	})
	mux.HandleFunc("DELETE /api/v1/exports/{id}", func(w http.ResponseWriter, r *http.Request) {
		task, err := admin.Cancel(r.PathValue("id"))
		if err != nil {
			writeExportError(w, logger, "取消导出任务失败", err)
			return
		}
		writeJSON(w, http.StatusOK, task)
	})
	mux.HandleFunc("GET /api/v1/exports/{id}/download", func(w http.ResponseWriter, r *http.Request) {
		file, task, err := admin.Open(r.PathValue("id"))
		if err != nil {
			writeExportError(w, logger, "打开导出文件失败", err)
			return
		}
		defer file.Close()
		info, err := file.Stat()
		if err != nil {
			logger.Warn("读取导出文件失败", "task_id", task.ID, "error", err)
			writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "导出文件暂不可用"})
			return
		}
		filename := "teslabox-" + task.ID + ".mp4"
		w.Header().Set("Content-Disposition", mime.FormatMediaType("attachment", map[string]string{"filename": filename}))
		w.Header().Set("Content-Type", "video/mp4")
		http.ServeContent(w, r, filename, info.ModTime(), file)
	})
}

func exportSpaceAvailable(state service.Snapshot, request media.ExportRequest) bool {
	estimated := media.EstimateExportBytes(request)
	space := state.SnapshotSpace
	if estimated == 0 || space.TotalBytes == 0 {
		return true
	}
	if space.FreeBytes < estimated || space.FreeBytes-estimated < space.MinFreeBytes {
		return false
	}
	projectedPercent := float64(space.FreeBytes-estimated) * 100 / float64(space.TotalBytes)
	return projectedPercent >= float64(space.MinFreePercent)
}

func writeExportError(w http.ResponseWriter, logger *slog.Logger, message string, err error) {
	if errors.Is(err, media.ErrExportNotFound) {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "导出任务不存在"})
		return
	}
	if errors.Is(err, media.ErrExportQueueFull) {
		writeJSON(w, http.StatusTooManyRequests, map[string]string{"error": "导出队列已满，请稍后重试"})
		return
	}
	var inputError *media.InputError
	if errors.As(err, &inputError) {
		writeJSON(w, http.StatusUnprocessableEntity, map[string]string{"error": inputError.Error()})
		return
	}
	logger.Warn(message, "error", err)
	writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "视频导出暂不可用"})
}

func registerCompositeRoutes(mux *http.ServeMux, controller Controller, compositor MediaCompositor, logger *slog.Logger) {
	mux.HandleFunc("GET /api/v1/video-groups/{id}/composite", func(w http.ResponseWriter, r *http.Request) {
		activateMedia(controller, logger, r.Context())
		state, release := controller.AcquireMediaSnapshot()
		defer release()
		if state.MediaSnapshot == nil {
			writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "媒体快照暂不可用"})
			return
		}
		var selected *videoGroup
		for _, group := range groupVideos(state.Videos, videoGroupTolerance) {
			if group.ID == r.PathValue("id") {
				copy := group
				selected = &copy
				break
			}
		}
		if selected == nil {
			http.NotFound(w, r)
			return
		}
		inputs := make(map[string]string, len(selected.Videos))
		for _, video := range selected.Videos {
			name, err := snapshotVideoPath(state.MediaSnapshot.MountPath, video.Path)
			if err != nil {
				logger.Warn("解析合成视频路径失败", "video_id", video.ID, "error", err)
				writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "视频文件暂不可用"})
				return
			}
			inputs[video.Camera] = name
		}
		tracked := &countingWriter{ResponseWriter: w}
		w.Header().Set("Content-Type", "video/mp4")
		w.Header().Set("Cache-Control", "no-store")
		request := media.CompositeRequest{
			Layout: r.URL.Query().Get("layout"), Quality: r.URL.Query().Get("quality"), Camera: r.URL.Query().Get("camera"), Inputs: inputs,
		}
		startedAt := time.Now()
		logger.Info("实时合成录像开始",
			"group_id", selected.ID,
			"layout", request.Layout,
			"quality", request.Quality,
			"camera", request.Camera,
			"input_count", len(request.Inputs),
		)
		err := compositor.Stream(r.Context(), request, tracked)
		duration := time.Since(startedAt)
		if err == nil {
			logger.Info("实时合成录像完成",
				"group_id", selected.ID,
				"layout", request.Layout,
				"quality", request.Quality,
				"bytes", tracked.written,
				"duration_ms", duration.Milliseconds(),
			)
			return
		}
		if errors.Is(err, context.Canceled) {
			logger.Info("实时合成录像由客户端中止",
				"group_id", selected.ID,
				"layout", request.Layout,
				"quality", request.Quality,
				"bytes", tracked.written,
				"duration_ms", duration.Milliseconds(),
			)
			return
		}
		logValues := []any{
			"group_id", selected.ID,
			"layout", request.Layout,
			"quality", request.Quality,
			"camera", request.Camera,
			"input_count", len(request.Inputs),
			"bytes", tracked.written,
			"duration_ms", duration.Milliseconds(),
			"error", normalizeDiagnosticText(err.Error(), 2048),
		}
		var commandError *system.CommandError
		if errors.As(err, &commandError) {
			logValues = append(logValues, "command", commandError.Name, "exit_code", commandError.Code)
		}
		logger.Warn("实时合成录像失败", logValues...)
		if tracked.written > 0 {
			return
		}
		var inputError *media.InputError
		if errors.As(err, &inputError) {
			writeJSON(w, http.StatusUnprocessableEntity, map[string]string{"error": inputError.Error()})
			return
		}
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "实时合成暂不可用"})
	})
}

func registerMediaRoutes(mux *http.ServeMux, controller Controller, inspector MediaInspector, logger *slog.Logger) {
	mux.HandleFunc("GET /api/v1/media/capabilities", func(w http.ResponseWriter, r *http.Request) {
		capabilities, err := inspector.Capabilities(r.Context())
		if err != nil {
			logger.Warn("读取媒体工具链能力失败", "error", err)
			writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "媒体工具链暂不可用"})
			return
		}
		writeJSON(w, http.StatusOK, capabilities)
	})
	mux.HandleFunc("GET /api/v1/videos/{id}/metadata", func(w http.ResponseWriter, r *http.Request) {
		activateMedia(controller, logger, r.Context())
		state, release := controller.AcquireMediaSnapshot()
		defer release()
		if state.MediaSnapshot == nil {
			writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "媒体快照暂不可用"})
			return
		}
		video, found := findVideo(state.Videos, r.PathValue("id"))
		if !found {
			http.NotFound(w, r)
			return
		}
		name, err := snapshotVideoPath(state.MediaSnapshot.MountPath, video.Path)
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				http.NotFound(w, r)
				return
			}
			logger.Warn("解析快照视频路径失败", "video_id", video.ID, "error", err)
			writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "视频文件暂不可用"})
			return
		}
		metadata, err := inspector.Probe(r.Context(), name)
		if err != nil {
			logger.Warn("读取视频技术元数据失败", "video_id", video.ID, "error", err)
			writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "暂时无法读取视频技术元数据"})
			return
		}
		writeJSON(w, http.StatusOK, metadata)
	})
}

const maximumMediaUploadRequestBytes int64 = 512 * 1024 * 1024

func registerMediaAssetRoutes(mux *http.ServeMux, controller Controller, admin MediaAssetsAdmin, gadget GadgetAdmin, power PowerAdmin, automation MaintenanceAutomation, logger *slog.Logger) {
	mux.HandleFunc("GET /api/v1/media-assets", func(w http.ResponseWriter, r *http.Request) {
		status, err := admin.Status(r.Context())
		if err != nil {
			logger.Warn("读取媒体中心状态失败", "error", err)
			writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "暂时无法读取媒体中心状态"})
			return
		}
		writeJSON(w, http.StatusOK, status)
	})
	mux.HandleFunc("POST /api/v1/media-assets/upload", func(w http.ResponseWriter, r *http.Request) {
		category := mediaassets.Category(strings.TrimSpace(r.URL.Query().Get("category")))
		r.Body = http.MaxBytesReader(w, r.Body, maximumMediaUploadRequestBytes)
		reader, err := r.MultipartReader()
		if err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "请选择要上传的文件"})
			return
		}
		assets, err := admin.UploadStream(r.Context(), category, func(add func(mediaassets.UploadFile) error) error {
			for {
				part, nextErr := reader.NextPart()
				if errors.Is(nextErr, io.EOF) {
					return nil
				}
				if nextErr != nil {
					return fmt.Errorf("读取上传文件: %w", nextErr)
				}
				if part.FileName() == "" {
					_ = part.Close()
					continue
				}
				role := strings.TrimSpace(part.FormName())
				if role != "file" && role != "sequence" && role != "audio" {
					_ = part.Close()
					return errors.New("上传字段不受支持")
				}
				err := add(mediaassets.UploadFile{Role: role, FileName: part.FileName(), Content: part})
				closeErr := part.Close()
				if err != nil {
					return err
				}
				if closeErr != nil {
					return fmt.Errorf("关闭上传文件: %w", closeErr)
				}
			}
		})
		if err != nil {
			logger.Warn("暂存媒体上传失败", "category", category, "error", err)
			writeMediaAssetError(w, err)
			return
		}
		writeJSON(w, http.StatusCreated, map[string]any{"assets": assets})
	})
	mux.HandleFunc("POST /api/v1/media-assets/{id}/delete", func(w http.ResponseWriter, r *http.Request) {
		status, err := admin.Delete(r.Context(), r.PathValue("id"))
		if err != nil {
			logger.Warn("标记删除媒体资源失败", "error", err)
			writeMediaAssetError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, status)
	})
	mux.HandleFunc("POST /api/v1/media-assets/external-delete", func(w http.ResponseWriter, r *http.Request) {
		var input struct {
			ID                     string               `json:"id"`
			Category               mediaassets.Category `json:"category"`
			Files                  []mediaassets.File   `json:"files"`
			ConfirmOTGDisconnected bool                 `json:"confirm_otg_disconnected"`
		}
		if !decodeJSON(w, r, &input) {
			return
		}
		automatic := automaticMediaRecoveryEnabled(automation)
		if !input.ConfirmOTGDisconnected && !automatic {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "请先物理拔掉车机或电脑的 OTG 线，再确认删除直接写入的媒体"})
			return
		}
		if automatic {
			if err := beginAutomaticMaintenance(r.Context(), gadget); err != nil {
				logger.Warn("自动断开 USB Gadget 失败", "error", err)
				writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "无法自动断开 USB Gadget；请关闭自动恢复或手工拔掉 OTG 后重试"})
				return
			}
		}
		status, err := admin.DeleteExternal(r.Context(), mediaassets.Asset{
			ID:       input.ID,
			Category: input.Category,
			Files:    input.Files,
			Applied:  true,
			External: true,
		})
		if automatic {
			if recoveryErr := finishAutomaticMaintenance(r.Context(), gadget, power); recoveryErr != nil {
				logger.Warn("提交自动温重启恢复失败", "error", recoveryErr)
				writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "媒体删除结束，但自动温重启恢复提交失败；请在日志中心手工执行温重启恢复"})
				return
			}
		}
		if err != nil {
			logger.Warn("删除直接写入媒体资源失败", "error", err)
			if automatic {
				writeJSON(w, http.StatusBadRequest, map[string]string{"error": "删除直接写入媒体失败，但已安排温重启恢复 USB Gadget；请等待设备重新上线后查看结果"})
				return
			}
			writeMediaAssetError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, status)
	})
	mux.HandleFunc("POST /api/v1/video-sessions/{id}/delete", func(w http.ResponseWriter, r *http.Request) {
		var input struct {
			ConfirmOTGDisconnected bool `json:"confirm_otg_disconnected"`
		}
		if !decodeJSON(w, r, &input) {
			return
		}
		automatic := automaticMediaRecoveryEnabled(automation)
		if !input.ConfirmOTGDisconnected && !automatic {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "请先物理拔掉车机或电脑的 OTG 线，再确认删除录像"})
			return
		}

		state, release := controller.AcquireMediaSnapshot()
		if state.MediaSnapshot == nil {
			release()
			writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "媒体快照暂不可用，请刷新录像列表后重试"})
			return
		}
		session, found := findVideoSession(state.Videos, r.PathValue("id"))
		if !found {
			release()
			http.NotFound(w, r)
			return
		}
		paths, err := recordingSessionPaths(session)
		release()
		if err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
			return
		}

		operationCtx := r.Context()
		cancelOperation := func() {}
		if automatic {
			// 用户在维护开始后关闭网页也必须完成卸载和温重启预约，避免 Gadget 永久停在半维护状态。
			operationCtx, cancelOperation = context.WithTimeout(context.WithoutCancel(r.Context()), 15*time.Minute)
		}
		defer cancelOperation()
		if automatic {
			if err := beginAutomaticMaintenance(operationCtx, gadget); err != nil {
				logger.Warn("删除录像前自动断开 USB Gadget 失败", "session_id", session.ID, "error", err)
				writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "无法自动断开 USB Gadget；请关闭自动恢复或手工拔掉 OTG 后重试"})
				return
			}
		}
		result, deleteErr := admin.DeleteRecordings(operationCtx, mediaassets.RecordingDeleteRequest{Paths: paths})
		if automatic {
			if recoveryErr := finishAutomaticMaintenance(operationCtx, gadget, power); recoveryErr != nil {
				logger.Warn("删除录像后提交自动温重启恢复失败", "session_id", session.ID, "error", recoveryErr)
				writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "录像删除结束，但自动温重启恢复提交失败；请在日志中心手工执行温重启恢复"})
				return
			}
		}
		if deleteErr != nil {
			logger.Warn("删除录像失败", "session_id", session.ID, "clip_type", session.ClipType, "error", deleteErr)
			if automatic {
				writeJSON(w, http.StatusBadRequest, map[string]string{"error": "录像删除失败，但已安排温重启恢复 USB Gadget；请等待设备重新上线后查看日志"})
				return
			}
			writeMediaAssetError(w, deleteErr)
			return
		}
		logger.Info("录像删除完成",
			"session_id", session.ID,
			"clip_type", session.ClipType,
			"deleted_files", result.DeletedFiles,
			"deleted_events", result.DeletedEvents,
			"deleted_bytes", result.DeletedBytes,
		)
		writeJSON(w, http.StatusOK, result)
	})
	mux.HandleFunc("POST /api/v1/media-assets/apply", func(w http.ResponseWriter, r *http.Request) {
		var input struct {
			ConfirmOTGDisconnected bool `json:"confirm_otg_disconnected"`
		}
		if !decodeJSON(w, r, &input) {
			return
		}
		automatic := automaticMediaRecoveryEnabled(automation)
		if !input.ConfirmOTGDisconnected && !automatic {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "请先物理拔掉车机或电脑的 OTG 线，再确认应用媒体变更"})
			return
		}
		if automatic {
			if err := beginAutomaticMaintenance(r.Context(), gadget); err != nil {
				logger.Warn("自动断开 USB Gadget 失败", "error", err)
				writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "无法自动断开 USB Gadget；请关闭自动恢复或手工拔掉 OTG 后重试"})
				return
			}
		}
		status, err := admin.Apply(r.Context())
		if automatic {
			if recoveryErr := finishAutomaticMaintenance(r.Context(), gadget, power); recoveryErr != nil {
				logger.Warn("提交自动温重启恢复失败", "error", recoveryErr)
				writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "媒体应用结束，但自动温重启恢复提交失败；请在日志中心手工执行温重启恢复"})
				return
			}
		}
		if err != nil {
			logger.Warn("应用媒体变更失败", "error", err)
			if automatic {
				writeJSON(w, http.StatusBadRequest, map[string]string{"error": "应用媒体变更失败，但已安排温重启恢复 USB Gadget；请等待设备重新上线后查看日志"})
				return
			}
			writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "应用媒体变更失败；请确认 OTG 已物理断开、没有视频播放或导出任务，并在日志中心查看详情"})
			return
		}
		writeJSON(w, http.StatusOK, status)
	})
	mux.HandleFunc("GET /api/v1/media-assets/{id}/preview", func(w http.ResponseWriter, r *http.Request) {
		file, asset, err := admin.OpenPreview(r.Context(), r.PathValue("id"))
		if err != nil {
			if errors.Is(err, mediaassets.ErrPreviewUnavailable) {
				http.NotFound(w, r)
				return
			}
			logger.Warn("打开媒体预览失败", "error", err)
			writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "媒体预览暂不可用"})
			return
		}
		defer file.Close()
		if len(asset.Files) == 0 {
			http.NotFound(w, r)
			return
		}
		contentType := mime.TypeByExtension(strings.ToLower(filepath.Ext(asset.Files[0].Name)))
		if contentType == "" {
			contentType = "application/octet-stream"
		}
		w.Header().Set("Content-Type", contentType)
		w.Header().Set("Cache-Control", "private, max-age=300")
		http.ServeContent(w, r, asset.Files[0].Name, asset.CreatedAt, file)
	})
}

func writeMediaAssetError(w http.ResponseWriter, err error) {
	if errors.Is(err, mediaassets.ErrPreviewUnavailable) {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "媒体预览不可用"})
		return
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		writeJSON(w, http.StatusRequestTimeout, map[string]string{"error": "媒体操作已取消或超时"})
		return
	}
	writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
}

var automaticMaintenanceDisconnectDelay = 2 * time.Second

func automaticMediaRecoveryEnabled(automation MaintenanceAutomation) bool {
	return automation != nil && automation.Settings().AutoRecovery
}

func automaticUpdateRecoveryEnabled(automation MaintenanceAutomation) bool {
	return automation != nil && automation.Settings().UpdateAutoRecovery
}

func beginAutomaticMaintenance(ctx context.Context, gadget GadgetAdmin) error {
	if gadget == nil {
		return errors.New("USB Gadget 自动维护不可用")
	}
	if _, err := gadget.PauseGadget(ctx); err != nil {
		return err
	}
	timer := time.NewTimer(automaticMaintenanceDisconnectDelay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

func finishAutomaticMaintenance(ctx context.Context, gadget GadgetAdmin, power PowerAdmin) error {
	if gadget == nil || power == nil {
		return errors.New("USB Gadget 自动温重启恢复不可用")
	}
	if _, err := gadget.ResumeGadget(ctx); err != nil {
		return err
	}
	return power.SchedulePower("reboot")
}

func registerMaintenanceRoutes(mux *http.ServeMux, automation MaintenanceAutomation, logger *slog.Logger) {
	mux.HandleFunc("GET /api/v1/maintenance", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, automation.Settings())
	})
	mux.HandleFunc("POST /api/v1/maintenance", func(w http.ResponseWriter, r *http.Request) {
		var input struct {
			AutoRecovery bool `json:"auto_recovery"`
			ConfirmRisk  bool `json:"confirm_risk"`
		}
		if !decodeJSON(w, r, &input) {
			return
		}
		if input.AutoRecovery && !input.ConfirmRisk {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "启用自动恢复前请确认车机或电脑会在维护期间短暂断开 USB 存储"})
			return
		}
		settings, err := automation.SetAutoRecovery(input.AutoRecovery)
		if err != nil {
			logger.Warn("保存自动维护设置失败", "error", err)
			writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "暂时无法保存自动维护设置"})
			return
		}
		writeJSON(w, http.StatusOK, settings)
	})
}

func registerStorageAdminRoutes(mux *http.ServeMux, admin StorageAdmin, logger *slog.Logger) {
	mux.HandleFunc("GET /api/v1/storage/devices", func(w http.ResponseWriter, r *http.Request) {
		status, err := admin.Status(r.Context())
		if err != nil {
			logger.Warn("读取外接存储设备失败", "error", err)
			writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "暂时无法读取外接存储设备"})
			return
		}
		writeJSON(w, http.StatusOK, status)
	})
	mux.HandleFunc("POST /api/v1/storage/initialize", func(w http.ResponseWriter, r *http.Request) {
		var input storageadmin.InitializeRequest
		if !decodeJSON(w, r, &input) {
			return
		}
		device, err := admin.Initialize(r.Context(), input)
		if err != nil {
			logger.Warn("初始化外接存储设备失败", "device", input.StablePath, "error", err)
			if errors.Is(err, storageadmin.ErrUnsafeState) {
				writeJSON(w, http.StatusConflict, map[string]string{"error": "当前数据盘或车机 USB 正在使用，必须先安全移除原数据盘"})
				return
			}
			var inputError *storageadmin.InputError
			if errors.As(err, &inputError) {
				writeJSON(w, http.StatusBadRequest, map[string]string{"error": inputError.Error()})
				return
			}
			writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "存储设备初始化失败，请查看设备状态后重试"})
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"initialized": true, "device": device})
	})
	mux.HandleFunc("POST /api/v1/storage/initialize/cancel", func(w http.ResponseWriter, r *http.Request) {
		var input storageadmin.CancelRequest
		if !decodeJSON(w, r, &input) {
			return
		}
		if err := admin.Cancel(r.Context(), input); err != nil {
			logger.Warn("放弃外接存储初始化失败", "device", input.StablePath, "error", err)
			var inputError *storageadmin.InputError
			if errors.As(err, &inputError) {
				writeJSON(w, http.StatusBadRequest, map[string]string{"error": inputError.Error()})
				return
			}
			writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "暂时无法放弃初始化任务"})
			return
		}
		writeJSON(w, http.StatusOK, map[string]bool{"cancelled": true})
	})
}

func registerDeviceAdminRoutes(mux *http.ServeMux, admin DeviceAdmin, logger *slog.Logger) {
	mux.HandleFunc("GET /api/v1/network/wifi", func(w http.ResponseWriter, r *http.Request) {
		status, err := admin.WiFiStatus(r.Context())
		if err != nil {
			writeDeviceAdminError(w, logger, "读取 WiFi 状态失败", err)
			return
		}
		writeJSON(w, http.StatusOK, status)
	})
	mux.HandleFunc("POST /api/v1/network/wifi/scan", func(w http.ResponseWriter, r *http.Request) {
		items, err := admin.Scan(r.Context())
		if err != nil {
			writeDeviceAdminError(w, logger, "扫描 WiFi 失败", err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"items": items})
	})
	mux.HandleFunc("POST /api/v1/network/wifi/connect", func(w http.ResponseWriter, r *http.Request) {
		var input deviceadmin.ConnectRequest
		if !decodeJSON(w, r, &input) {
			return
		}
		if err := admin.Connect(r.Context(), input); err != nil {
			writeDeviceAdminError(w, logger, "连接 WiFi 失败", err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]bool{"connected": true})
	})
	mux.HandleFunc("POST /api/v1/network/wifi/autoconnect", func(w http.ResponseWriter, r *http.Request) {
		var input deviceadmin.AutoConnectUpdate
		if !decodeJSON(w, r, &input) {
			return
		}
		if err := admin.SetAutoConnect(r.Context(), input); err != nil {
			writeDeviceAdminError(w, logger, "更新 WiFi 自动连接设置失败", err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]bool{"updated": true})
	})
	mux.HandleFunc("POST /api/v1/network/wifi/ipv4", func(w http.ResponseWriter, r *http.Request) {
		var input deviceadmin.IPv4Update
		if !decodeJSON(w, r, &input) {
			return
		}
		if err := admin.UpdateIPv4(r.Context(), input); err != nil {
			writeDeviceAdminError(w, logger, "更新 WiFi IPv4 设置失败", err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]bool{"updated": true})
	})
	mux.HandleFunc("POST /api/v1/network/wifi/forget", func(w http.ResponseWriter, r *http.Request) {
		var input struct {
			UUID string `json:"uuid"`
		}
		if !decodeJSON(w, r, &input) {
			return
		}
		if err := admin.Forget(r.Context(), input.UUID); err != nil {
			writeDeviceAdminError(w, logger, "忘记 WiFi 失败", err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]bool{"forgotten": true})
	})
	mux.HandleFunc("GET /api/v1/network/ap", func(w http.ResponseWriter, r *http.Request) {
		settings, err := admin.APSettings(r.Context())
		if err != nil {
			writeDeviceAdminError(w, logger, "读取 AP 配置失败", err)
			return
		}
		writeJSON(w, http.StatusOK, settings)
	})
	mux.HandleFunc("POST /api/v1/network/ap", func(w http.ResponseWriter, r *http.Request) {
		var input deviceadmin.APUpdate
		if !decodeJSON(w, r, &input) {
			return
		}
		if err := admin.UpdateAP(r.Context(), input); err != nil {
			writeDeviceAdminError(w, logger, "更新 AP 配置失败", err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]bool{"updated": true})
	})
	mux.HandleFunc("POST /api/v1/system/power", func(w http.ResponseWriter, r *http.Request) {
		var input struct {
			Action string `json:"action"`
		}
		if !decodeJSON(w, r, &input) {
			return
		}
		if err := admin.SchedulePower(input.Action); err != nil {
			writeDeviceAdminError(w, logger, "提交电源操作失败", err)
			return
		}
		writeJSON(w, http.StatusAccepted, map[string]bool{"accepted": true})
	})
}

func decodeJSON(w http.ResponseWriter, r *http.Request, target any) bool {
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, 16*1024))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "请求参数无效"})
		return false
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "请求参数无效"})
		return false
	}
	return true
}

func writeDeviceAdminError(w http.ResponseWriter, logger *slog.Logger, message string, err error) {
	logger.Warn(message, "error", err)
	if errors.Is(err, deviceadmin.ErrAPActive) {
		writeJSON(w, http.StatusConflict, map[string]string{"error": "当前无线网卡正在提供 TeslaBox 热点。第一版不会自动关闭热点，请先准备有线网络或第二块无线网卡。"})
		return
	}
	if errors.Is(err, deviceadmin.ErrWiFiUnavailable) {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "未检测到可用的无线网卡；TeslaUSB、媒体与有线管理不受影响"})
		return
	}
	if errors.Is(err, deviceadmin.ErrUnsupportedSecurity) {
		writeJSON(w, http.StatusUnprocessableEntity, map[string]string{"error": "第一版仅支持开放网络和 WPA/WPA2 个人网络"})
		return
	}
	var inputError *deviceadmin.InputError
	if errors.As(err, &inputError) {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": inputError.Error()})
		return
	}
	writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "设备网络管理暂不可用"})
}

func serveVideo(w http.ResponseWriter, r *http.Request, state service.Snapshot, download bool) {
	if state.MediaSnapshot == nil {
		http.Error(w, "媒体快照暂不可用", http.StatusServiceUnavailable)
		return
	}
	selected, found := findVideo(state.Videos, r.PathValue("id"))
	if !found {
		http.NotFound(w, r)
		return
	}

	file, err := openSnapshotVideo(state.MediaSnapshot.MountPath, selected.Path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			http.NotFound(w, r)
			return
		}
		http.Error(w, "视频文件暂不可用", http.StatusServiceUnavailable)
		return
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		http.Error(w, "视频文件暂不可用", http.StatusServiceUnavailable)
		return
	}
	if download {
		w.Header().Set("Content-Disposition", mime.FormatMediaType("attachment", map[string]string{"filename": filepath.Base(selected.Path)}))
	}
	w.Header().Set("Content-Type", "video/mp4")
	http.ServeContent(w, r, filepath.Base(selected.Path), info.ModTime(), file)
}

func openSnapshotVideo(root, relative string) (*os.File, error) {
	name, err := snapshotVideoPath(root, relative)
	if err != nil {
		return nil, err
	}
	return os.Open(name)
}

func snapshotVideoPath(root, relative string) (string, error) {
	return snapshotFilePath(root, relative)
}

func snapshotFilePath(root, relative string) (string, error) {
	cleaned := filepath.Clean(filepath.FromSlash(relative))
	if filepath.IsAbs(cleaned) || cleaned == "." || cleaned == ".." || strings.HasPrefix(cleaned, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("媒体路径越过快照根目录")
	}
	name := filepath.Join(filepath.Clean(root), cleaned)
	info, err := os.Stat(name)
	if err != nil {
		return "", err
	}
	if !info.Mode().IsRegular() {
		return "", fmt.Errorf("媒体路径不是普通文件")
	}
	return name, nil
}

func serveThumbnail(w http.ResponseWriter, r *http.Request, state service.Snapshot) {
	if state.MediaSnapshot == nil {
		http.Error(w, "媒体快照暂不可用", http.StatusServiceUnavailable)
		return
	}
	relative, found := findThumbnail(state.Videos, r.PathValue("id"))
	if !found {
		http.NotFound(w, r)
		return
	}
	name, err := snapshotFilePath(state.MediaSnapshot.MountPath, relative)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			http.NotFound(w, r)
			return
		}
		http.Error(w, "缩略图暂不可用", http.StatusServiceUnavailable)
		return
	}
	file, err := os.Open(name)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			http.NotFound(w, r)
			return
		}
		http.Error(w, "缩略图暂不可用", http.StatusServiceUnavailable)
		return
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		http.Error(w, "缩略图暂不可用", http.StatusServiceUnavailable)
		return
	}
	w.Header().Set("Content-Type", "image/png")
	w.Header().Set("Cache-Control", "private, max-age=30")
	http.ServeContent(w, r, filepath.Base(relative), info.ModTime(), file)
}

func findThumbnail(videos []teslacam.Video, id string) (string, bool) {
	for _, video := range videos {
		if video.ThumbnailID == id && video.ThumbnailPath != "" {
			return video.ThumbnailPath, true
		}
	}
	return "", false
}

func findVideo(videos []teslacam.Video, id string) (teslacam.Video, bool) {
	for _, video := range videos {
		if video.ID == id {
			return video, true
		}
	}
	return teslacam.Video{}, false
}

type videoListResponse struct {
	Items  []teslacam.Video `json:"items"`
	Total  int              `json:"total"`
	Limit  int              `json:"limit"`
	Offset int              `json:"offset"`
}

const (
	videoGroupTolerance    = 2 * time.Second
	defaultSegmentDuration = 60 * time.Second
	sessionContinuityGap   = 5 * time.Second
)

type videoGroup struct {
	ID            string                  `json:"id"`
	ClipType      string                  `json:"clip_type"`
	Event         string                  `json:"event,omitempty"`
	EventMetadata *teslacam.EventMetadata `json:"event_metadata,omitempty"`
	ThumbnailID   string                  `json:"-"`
	RecordedAt    time.Time               `json:"recorded_at"`
	EndAt         time.Time               `json:"end_at"`
	Duration      float64                 `json:"duration_seconds"`
	TimelineStart float64                 `json:"timeline_start_seconds"`
	TimeSpreadMS  int64                   `json:"time_spread_ms"`
	TotalSize     int64                   `json:"total_size"`
	Complete      bool                    `json:"complete"`
	Videos        []teslacam.Video        `json:"videos"`
}

type videoSession struct {
	ID            string                  `json:"id"`
	ClipType      string                  `json:"clip_type"`
	Event         string                  `json:"event,omitempty"`
	EventMetadata *teslacam.EventMetadata `json:"event_metadata,omitempty"`
	ThumbnailID   string                  `json:"-"`
	ThumbnailURL  string                  `json:"thumbnail_url,omitempty"`
	StartAt       time.Time               `json:"start_at"`
	EndAt         time.Time               `json:"end_at"`
	Duration      float64                 `json:"duration_seconds"`
	SegmentCount  int                     `json:"segment_count"`
	TotalSize     int64                   `json:"total_size"`
	Complete      bool                    `json:"complete"`
	Cameras       []string                `json:"cameras"`
	Moments       []videoGroup            `json:"moments,omitempty"`
}

type videoSessionListResponse struct {
	Items  []videoSession `json:"items"`
	Total  int            `json:"total"`
	Limit  int            `json:"limit"`
	Offset int            `json:"offset"`
}

type videoGroupListResponse struct {
	Items     []videoGroup `json:"items"`
	Total     int          `json:"total"`
	Limit     int          `json:"limit"`
	Offset    int          `json:"offset"`
	Tolerance int64        `json:"tolerance_ms"`
}

func writeVideoList(w http.ResponseWriter, r *http.Request, videos []teslacam.Video) {
	query := r.URL.Query()
	filtered := make([]teslacam.Video, 0, len(videos))
	for _, video := range videos {
		if value := query.Get("clip_type"); value != "" && video.ClipType != value {
			continue
		}
		if value := query.Get("camera"); value != "" && video.Camera != value {
			continue
		}
		if value := query.Get("event"); value != "" && video.Event != value {
			continue
		}
		if value := query.Get("date"); value != "" && video.RecordedAt.Format("2006-01-02") != value {
			continue
		}
		filtered = append(filtered, video)
	}

	limit := boundedInt(query.Get("limit"), 200, 1, 1000)
	offset := boundedInt(query.Get("offset"), 0, 0, len(filtered))
	end := min(offset+limit, len(filtered))
	items := append([]teslacam.Video(nil), filtered[offset:end]...)
	if items == nil {
		items = []teslacam.Video{}
	}
	writeJSON(w, http.StatusOK, videoListResponse{
		Items: items, Total: len(filtered), Limit: limit, Offset: offset,
	})
}

func writeVideoGroupList(w http.ResponseWriter, r *http.Request, videos []teslacam.Video) {
	query := r.URL.Query()
	groups := groupVideos(videos, videoGroupTolerance)
	filtered := make([]videoGroup, 0, len(groups))
	for _, group := range groups {
		if value := query.Get("clip_type"); value != "" && group.ClipType != value {
			continue
		}
		if value := query.Get("event"); value != "" && group.Event != value {
			continue
		}
		if value := query.Get("date"); value != "" && group.RecordedAt.Format("2006-01-02") != value {
			continue
		}
		if value := query.Get("camera"); value != "" && !groupHasCamera(group, value) {
			continue
		}
		filtered = append(filtered, group)
	}

	limit := boundedInt(query.Get("limit"), 100, 1, 500)
	offset := boundedInt(query.Get("offset"), 0, 0, len(filtered))
	end := min(offset+limit, len(filtered))
	items := append([]videoGroup(nil), filtered[offset:end]...)
	if items == nil {
		items = []videoGroup{}
	}
	writeJSON(w, http.StatusOK, videoGroupListResponse{
		Items: items, Total: len(filtered), Limit: limit, Offset: offset,
		Tolerance: videoGroupTolerance.Milliseconds(),
	})
}

func writeVideoSessionList(w http.ResponseWriter, r *http.Request, videos []teslacam.Video) {
	query := r.URL.Query()
	sessions := groupVideoSessions(groupVideos(videos, videoGroupTolerance), sessionContinuityGap)
	filtered := make([]videoSession, 0, len(sessions))
	for _, session := range sessions {
		if value := query.Get("clip_type"); value != "" && session.ClipType != value {
			continue
		}
		if value := query.Get("event"); value != "" && session.Event != value {
			continue
		}
		if value := query.Get("date"); value != "" && session.StartAt.Format("2006-01-02") != value {
			continue
		}
		if value := query.Get("camera"); value != "" && !slices.Contains(session.Cameras, value) {
			continue
		}
		filtered = append(filtered, session)
	}
	limit := boundedInt(query.Get("limit"), 200, 1, 1000)
	offset := boundedInt(query.Get("offset"), 0, 0, len(filtered))
	end := min(offset+limit, len(filtered))
	items := append([]videoSession(nil), filtered[offset:end]...)
	for index := range items {
		items[index].Moments = nil
	}
	if items == nil {
		items = []videoSession{}
	}
	writeJSON(w, http.StatusOK, videoSessionListResponse{
		Items: items, Total: len(filtered), Limit: limit, Offset: offset,
	})
}

func writeVideoSession(w http.ResponseWriter, r *http.Request, videos []teslacam.Video) {
	session, found := findVideoSession(videos, r.PathValue("id"))
	if found {
		writeJSON(w, http.StatusOK, session)
		return
	}
	http.NotFound(w, r)
}

func findVideoSession(videos []teslacam.Video, id string) (videoSession, bool) {
	for _, session := range groupVideoSessions(groupVideos(videos, videoGroupTolerance), sessionContinuityGap) {
		if session.ID == id {
			return session, true
		}
	}
	return videoSession{}, false
}

func recordingSessionPaths(session videoSession) ([]string, error) {
	switch session.ClipType {
	case "recent", "saved", "sentry":
	default:
		return nil, errors.New("该录像类型不允许从网页删除")
	}
	seen := make(map[string]struct{})
	paths := make([]string, 0, session.SegmentCount*len(session.Cameras))
	eventDirectory := ""
	for _, moment := range session.Moments {
		for _, video := range moment.Videos {
			if session.ClipType == "saved" || session.ClipType == "sentry" {
				parts := strings.Split(video.Path, "/")
				if len(parts) != 4 {
					return nil, errors.New("录像事件路径不完整，拒绝删除")
				}
				currentDirectory := strings.Join(parts[:3], "/")
				if eventDirectory != "" && currentDirectory != eventDirectory {
					return nil, errors.New("同一录像会话包含多个事件目录，拒绝批量删除")
				}
				eventDirectory = currentDirectory
			}
			if _, found := seen[video.Path]; found {
				continue
			}
			seen[video.Path] = struct{}{}
			paths = append(paths, video.Path)
		}
	}
	if len(paths) == 0 {
		return nil, errors.New("当前快照没有可删除的录像文件")
	}
	return paths, nil
}

func exportSource(snapshotRoot string, session videoSession) (media.ExportSource, error) {
	sessionEnd := session.EndAt
	if session.Duration > 0 {
		sessionEnd = session.StartAt.Add(time.Duration(session.Duration * float64(time.Second)))
	}
	source := media.ExportSource{SessionStart: session.StartAt, SessionEnd: sessionEnd}
	for _, moment := range session.Moments {
		momentStart := session.StartAt.Add(time.Duration(moment.TimelineStart * float64(time.Second)))
		for _, video := range moment.Videos {
			name, err := snapshotVideoPath(snapshotRoot, video.Path)
			if err != nil {
				return media.ExportSource{}, err
			}
			duration := video.Duration
			if duration <= 0 {
				duration = defaultSegmentDuration.Seconds()
			}
			// 最近录像会压缩掉缺失时间段，导出必须沿用页面时间轴，否则点击的范围会落到错误片段。
			recordedAt := momentStart.Add(video.RecordedAt.Sub(moment.RecordedAt))
			source.Videos = append(source.Videos, media.ExportVideo{
				Camera: video.Camera, Path: name, RecordedAt: recordedAt, Duration: duration,
			})
		}
	}
	return source, nil
}

func groupVideos(videos []teslacam.Video, tolerance time.Duration) []videoGroup {
	sorted := append([]teslacam.Video(nil), videos...)
	slices.SortFunc(sorted, func(left, right teslacam.Video) int {
		return right.RecordedAt.Compare(left.RecordedAt)
	})
	groups := make([]videoGroup, 0, len(sorted)/6+1)
	for _, video := range sorted {
		groupIndex := matchingGroup(groups, video, tolerance)
		if groupIndex < 0 {
			groups = append(groups, videoGroup{
				ID: video.ID, ClipType: video.ClipType, Event: video.Event, EventMetadata: video.EventMetadata,
				ThumbnailID: video.ThumbnailID,
				RecordedAt:  video.RecordedAt, Videos: []teslacam.Video{video}, TotalSize: video.Size,
			})
			continue
		}
		group := &groups[groupIndex]
		group.Videos = append(group.Videos, video)
		group.TotalSize += video.Size
		if group.EventMetadata == nil {
			group.EventMetadata = video.EventMetadata
		}
		if group.ThumbnailID == "" {
			group.ThumbnailID = video.ThumbnailID
		}
		if video.RecordedAt.Before(group.RecordedAt) {
			group.RecordedAt = video.RecordedAt
		}
	}
	for index := range groups {
		group := &groups[index]
		slices.SortFunc(group.Videos, func(left, right teslacam.Video) int {
			return cameraRank(left.Camera) - cameraRank(right.Camera)
		})
		latest := group.RecordedAt
		var endAt time.Time
		for _, video := range group.Videos {
			if video.RecordedAt.After(latest) {
				latest = video.RecordedAt
			}
			duration := time.Duration(video.Duration * float64(time.Second))
			if duration > 0 {
				videoEnd := video.RecordedAt.Add(duration)
				if videoEnd.After(endAt) {
					endAt = videoEnd
				}
			}
		}
		if endAt.IsZero() {
			endAt = latest.Add(defaultSegmentDuration)
		}
		group.TimeSpreadMS = latest.Sub(group.RecordedAt).Milliseconds()
		group.EndAt = endAt
		group.Duration = endAt.Sub(group.RecordedAt).Seconds()
		group.Complete = len(group.Videos) == 6
	}
	return groups
}

func groupVideoSessions(groups []videoGroup, continuityGap time.Duration) []videoSession {
	ordered := append([]videoGroup(nil), groups...)
	slices.SortFunc(ordered, func(left, right videoGroup) int {
		return left.RecordedAt.Compare(right.RecordedAt)
	})
	sessions := make([]videoSession, 0, len(ordered))
	for _, group := range ordered {
		sessionIndex := matchingSession(sessions, group, continuityGap)
		if sessionIndex < 0 {
			sessions = append(sessions, videoSession{
				ID: group.ID, ClipType: group.ClipType, Event: group.Event, EventMetadata: group.EventMetadata,
				ThumbnailID: group.ThumbnailID,
				StartAt:     group.RecordedAt, EndAt: group.EndAt,
				TotalSize: group.TotalSize, Complete: group.Complete,
				Moments: []videoGroup{group},
			})
			continue
		}
		session := &sessions[sessionIndex]
		session.Moments = append(session.Moments, group)
		session.TotalSize += group.TotalSize
		session.Complete = session.Complete && group.Complete
		if session.EventMetadata == nil {
			session.EventMetadata = group.EventMetadata
		}
		if session.ThumbnailID == "" {
			session.ThumbnailID = group.ThumbnailID
		}
		if group.EndAt.After(session.EndAt) {
			session.EndAt = group.EndAt
		}
	}
	for index := range sessions {
		session := &sessions[index]
		assignSessionTimeline(session)
		session.SegmentCount = len(session.Moments)
		if session.ThumbnailID != "" {
			session.ThumbnailURL = "/api/v1/thumbnails/" + session.ThumbnailID
		}
		seen := make(map[string]bool, 6)
		for _, group := range session.Moments {
			for _, video := range group.Videos {
				seen[video.Camera] = true
			}
		}
		for _, camera := range []string{"left_pillar", "front", "right_pillar", "left_repeater", "back", "right_repeater"} {
			if seen[camera] {
				session.Cameras = append(session.Cameras, camera)
			}
		}
	}
	slices.SortFunc(sessions, func(left, right videoSession) int {
		return right.StartAt.Compare(left.StartAt)
	})
	return sessions
}

func matchingSession(sessions []videoSession, group videoGroup, continuityGap time.Duration) int {
	if group.ClipType == "recent" {
		for index := range sessions {
			if sessions[index].ClipType == "recent" {
				return index
			}
		}
		return -1
	}
	for index := len(sessions) - 1; index >= 0; index-- {
		if sessionAcceptsGroup(sessions[index], group, continuityGap) {
			return index
		}
	}
	return -1
}

func assignSessionTimeline(session *videoSession) {
	if session.ClipType != "recent" {
		for index := range session.Moments {
			session.Moments[index].TimelineStart = session.Moments[index].RecordedAt.Sub(session.StartAt).Seconds()
		}
		session.Duration = session.EndAt.Sub(session.StartAt).Seconds()
		return
	}

	var playbackDuration float64
	for index := range session.Moments {
		moment := &session.Moments[index]
		moment.TimelineStart = playbackDuration
		playbackDuration += groupPlaybackDuration(*moment)
	}
	session.Duration = playbackDuration
}

func groupPlaybackDuration(group videoGroup) float64 {
	if group.Duration > 0 {
		return group.Duration
	}
	if group.EndAt.After(group.RecordedAt) {
		return group.EndAt.Sub(group.RecordedAt).Seconds()
	}
	return defaultSegmentDuration.Seconds()
}

func sessionAcceptsGroup(session videoSession, group videoGroup, continuityGap time.Duration) bool {
	if session.ClipType != group.ClipType {
		return false
	}
	// RecentClips 本质是循环缓冲区。即使中间缺一分钟，也保留为同一条可跳转时间轴，
	// 让用户看到“最近录像”的完整窗口，而不是被视角缺失或写入间隔切成多段。
	if session.ClipType == "recent" {
		return true
	}
	if session.Event != group.Event {
		return false
	}
	return !group.RecordedAt.After(session.EndAt.Add(continuityGap))
}

func matchingGroup(groups []videoGroup, video teslacam.Video, tolerance time.Duration) int {
	for index := len(groups) - 1; index >= 0; index-- {
		group := groups[index]
		if group.ClipType != video.ClipType || group.Event != video.Event || groupHasCamera(group, video.Camera) {
			continue
		}
		earliest := video.RecordedAt
		latest := video.RecordedAt
		for _, member := range group.Videos {
			if member.RecordedAt.Before(earliest) {
				earliest = member.RecordedAt
			}
			if member.RecordedAt.After(latest) {
				latest = member.RecordedAt
			}
		}
		if latest.Sub(earliest) <= tolerance {
			return index
		}
	}
	return -1
}

func groupHasCamera(group videoGroup, camera string) bool {
	return slices.ContainsFunc(group.Videos, func(video teslacam.Video) bool {
		return video.Camera == camera
	})
}

func cameraRank(camera string) int {
	for index, known := range []string{"left_pillar", "front", "right_pillar", "left_repeater", "back", "right_repeater"} {
		if camera == known {
			return index
		}
	}
	return 100
}

func boundedInt(value string, fallback, minimum, maximum int) int {
	parsed, err := strconv.Atoi(value)
	if err != nil {
		return fallback
	}
	if parsed < minimum {
		return minimum
	}
	if parsed > maximum {
		return maximum
	}
	return parsed
}

func writeJSON(w http.ResponseWriter, status int, payload any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(payload); err != nil {
		return
	}
}

func requestLogger(next http.Handler, logger *slog.Logger) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		startedAt := time.Now()
		tracked := &statusWriter{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(tracked, r)
		logger.Info("HTTP 请求完成",
			"method", r.Method,
			"path", strings.TrimSpace(r.URL.Path),
			"status", tracked.status,
			"duration_ms", time.Since(startedAt).Milliseconds(),
		)
	})
}

type statusWriter struct {
	http.ResponseWriter
	status int
}

// Unwrap 让流式响应和可选 HTTP 接口穿过日志包装器。
func (w *statusWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

func (w *statusWriter) WriteHeader(status int) {
	w.status = status
	w.ResponseWriter.WriteHeader(status)
}

type countingWriter struct {
	http.ResponseWriter
	written int64
}

func (w *countingWriter) Write(value []byte) (int, error) {
	count, err := w.ResponseWriter.Write(value)
	w.written += int64(count)
	return count, err
}
