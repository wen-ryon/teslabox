package main

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/wen-ryon/teslabox/internal/api"
	"github.com/wen-ryon/teslabox/internal/backing"
	"github.com/wen-ryon/teslabox/internal/config"
	"github.com/wen-ryon/teslabox/internal/datamount"
	"github.com/wen-ryon/teslabox/internal/deviceadmin"
	"github.com/wen-ryon/teslabox/internal/diagnostics"
	"github.com/wen-ryon/teslabox/internal/driveinit"
	"github.com/wen-ryon/teslabox/internal/gadget"
	"github.com/wen-ryon/teslabox/internal/imageinit"
	"github.com/wen-ryon/teslabox/internal/media"
	"github.com/wen-ryon/teslabox/internal/platform"
	"github.com/wen-ryon/teslabox/internal/service"
	mediasnapshot "github.com/wen-ryon/teslabox/internal/snapshot"
	"github.com/wen-ryon/teslabox/internal/storage"
	"github.com/wen-ryon/teslabox/internal/storageadmin"
	"github.com/wen-ryon/teslabox/internal/system"
	"github.com/wen-ryon/teslabox/internal/teslacam"
	"github.com/wen-ryon/teslabox/internal/updater"
	buildversion "github.com/wen-ryon/teslabox/internal/version"
	"github.com/wen-ryon/teslabox/internal/webauth"
	"github.com/wen-ryon/teslabox/internal/webui"
)

func main() {
	reloadCh, stopReloadSignals := subscribeReloadSignals()
	defer stopReloadSignals()

	logger := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	cfg, err := config.Load()
	if err != nil {
		logger.Error("加载配置失败", "error", err)
		os.Exit(1)
	}

	runner := system.NewOSRunner()
	registry := storage.NewFileRegistry(cfg.RegistryPath)
	if err := registry.EnsureLegacy(cfg.DataUUID, cfg.DataLabel); err != nil {
		logger.Error("初始化存储设备登记册失败", "error", err)
		os.Exit(1)
	}
	detector := storage.NewRegisteredLSBLKDetector(runner, registry, cfg.DataUUID, cfg.DataLabel)
	if active, exists, registryErr := registry.Active(); registryErr != nil {
		logger.Error("读取活动存储设备登记失败", "error", registryErr)
		os.Exit(1)
	} else if exists && (active.UUID == "" || active.Serial == "" || active.SizeBytes == 0) {
		migrationCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		device, detectErr := detector.Detect(migrationCtx)
		cancel()
		if detectErr == nil {
			if err := registry.Complete(storage.DeviceRecord{
				UUID: device.UUID, Label: device.Label, StablePath: active.StablePath,
				Serial: device.Serial, Model: device.Model, SizeBytes: device.SizeBytes,
			}); err != nil {
				logger.Error("升级旧数据盘登记失败", "error", err)
				os.Exit(1)
			}
		} else if !errors.Is(detectErr, storage.ErrDeviceNotFound) {
			logger.Warn("暂时无法升级旧数据盘登记", "error", detectErr)
		}
	}
	inspector := backing.NewInspector(backing.Layout{
		Root:         cfg.DataMountPath,
		ImagePath:    cfg.ImagePath,
		SnapshotPath: cfg.SnapshotPath,
		IndexPath:    cfg.IndexPath,
		StatePath:    cfg.StatePath,
	})
	prober := gadget.NewFSProber(cfg.ConfigFSPath, cfg.UDCPath)
	mounter := datamount.NewRegistered(runner, cfg.DataMountPath, registry, cfg.DataUUID, cfg.DataLabel)
	gadgetManager := gadget.NewManager(gadget.Config{
		ConfigFSPath:  cfg.ConfigFSPath,
		UDCPath:       cfg.UDCPath,
		GadgetName:    cfg.GadgetName,
		UDCName:       cfg.UDCName,
		ImagePath:     filepath.Join(cfg.DataMountPath, filepath.FromSlash(cfg.ImagePath)),
		SerialNumber:  cfg.GadgetSerial,
		ReleaseGadget: cfg.ReleaseGadget,
	})
	if released, releaseErr := gadgetManager.ReleaseAllowedBinding(); releaseErr != nil {
		logger.Warn("启动阶段无法提前释放原厂 USB Gadget，将由主状态机继续收敛", "error", releaseErr)
	} else if released {
		logger.Info("已提前释放原厂 USB Gadget，等待数据盘就绪后绑定 TeslaBox")
	}
	if err := os.MkdirAll(cfg.SnapshotMountPath, 0o750); err != nil {
		logger.Error("创建快照运行目录失败", "error", err)
		os.Exit(1)
	}
	snapshotManager := mediasnapshot.New(runner, mediasnapshot.Config{
		ImagePath:      filepath.Join(cfg.DataMountPath, filepath.FromSlash(cfg.ImagePath)),
		SnapshotPath:   filepath.Join(cfg.DataMountPath, filepath.FromSlash(cfg.SnapshotPath)),
		MountRoot:      cfg.SnapshotMountPath,
		MinFreeBytes:   cfg.MinFreeBytes,
		MinFreePercent: cfg.MinFreePercent,
	})
	manager := service.NewManager(cfg, detector, inspector, prober, mounter, gadgetManager, snapshotManager, teslacam.NewScanner(), logger)
	driveInspector := driveinit.NewLSBLKInspector(runner)
	storageAdmin := storageadmin.New(storageadmin.Config{
		DataRoot:   cfg.DataMountPath,
		ImagePath:  filepath.Join(cfg.DataMountPath, filepath.FromSlash(cfg.ImagePath)),
		DataLabel:  cfg.DataLabel,
		ImageLabel: "TESLACAM",
	}, runner, registry, storageadmin.NewOSDeviceLister(driveInspector), driveinit.New(driveInspector, runner), imageinit.New(imageinit.NewOSInspector(runner), runner), manager)
	authManager, err := webauth.New(webauth.Config{
		StatePath:      cfg.WebAuthStatePath,
		PasswordSHA256: cfg.WebPasswordSHA256,
		SessionSecret:  cfg.WebSessionSecret,
		SessionTTL:     cfg.WebSessionTTL,
	})
	if err != nil {
		logger.Error("初始化 Web 管理认证失败", "error", err)
		os.Exit(1)
	}
	deviceAdmin := deviceadmin.New(runner, logger)
	mediaInspector, err := media.NewInspector(runner, media.Config{
		FFmpegPath: cfg.FFmpegPath, FFprobePath: cfg.FFprobePath, Timeout: cfg.MediaProbeTimeout,
	})
	if err != nil {
		logger.Error("初始化媒体工具链失败", "error", err)
		os.Exit(1)
	}
	mediaCompositor, err := media.NewCompositor(runner, cfg.FFmpegPath)
	if err != nil {
		logger.Error("初始化实时合成器失败", "error", err)
		os.Exit(1)
	}
	mediaExporter, err := media.NewExporter(runner, media.ExportConfig{
		FFmpegPath:  cfg.FFmpegPath,
		OutputDir:   filepath.Join(cfg.DataMountPath, filepath.FromSlash(cfg.ExportPath)),
		QueueSize:   cfg.ExportQueueSize,
		MaxDuration: cfg.ExportMaxDuration,
		Logger:      logger,
	})
	if err != nil {
		logger.Error("初始化视频导出器失败", "error", err)
		os.Exit(1)
	}
	updateManager, err := updater.New(updater.Config{
		ManifestURL: cfg.UpdateManifestURL, PublicKey: cfg.UpdatePublicKey,
		Channel: cfg.UpdateChannel, Board: cfg.UpdateBoard,
		CurrentVersion: buildversion.Version, CurrentCommit: buildversion.Commit,
		DownloadDir: cfg.UpdateDownloadDir, RequestPath: cfg.UpdateRequestPath,
		ResultPath: cfg.UpdateResultPath, MaximumSize: cfg.UpdateMaxBytes, Logger: logger,
		CanInstall: func() error {
			if manager.Snapshot().GadgetStatus.HostDetected {
				return errors.New("车机 USB 当前已连接，请停车并断开 OTG 后再安装更新")
			}
			if mediaExporter.Busy() {
				return errors.New("仍有视频导出任务，完成或取消后再安装更新")
			}
			return nil
		},
	}, updater.NewSystemdStarter(runner))
	if err != nil {
		logger.Error("在线更新配置无效，TeslaUSB 主链路继续运行", "error", err)
		updateManager, _ = updater.New(updater.Config{
			CurrentVersion: buildversion.Version, CurrentCommit: buildversion.Commit,
			Channel: "stable", Board: "radxa-rock-3a", Logger: logger,
		}, nil)
	}
	apiHandler := api.NewHandler(
		manager,
		logger,
		api.WithDeviceAdmin(deviceAdmin),
		api.WithStorageAdmin(storageAdmin),
		api.WithMediaInspector(mediaInspector),
		api.WithMediaCompositor(mediaCompositor),
		api.WithExportAdmin(mediaExporter),
		api.WithUpdateAdmin(updateManager),
		api.WithDiagnostics(diagnostics.New(runner)),
	)
	handler := webui.NewHandler(apiHandler, authManager, logger)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	go manager.Run(ctx)
	go deviceAdmin.Run(ctx)
	go mediaExporter.Run(ctx)
	watchReloadSignal(ctx, manager, reloadCh)

	server := &http.Server{
		Addr:              cfg.HTTPAddr,
		Handler:           handler,
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       30 * time.Second,
		// 视频 Range 和下载响应可能持续较久，连接生命周期由客户端和服务关闭信号控制。
		WriteTimeout: 0,
		IdleTimeout:  60 * time.Second,
	}

	errCh := make(chan error, 1)
	go func() {
		logger.Info("HTTP 服务已启动", "address", cfg.HTTPAddr)
		if serveErr := server.ListenAndServe(); serveErr != nil && !errors.Is(serveErr, http.ErrServerClosed) {
			errCh <- serveErr
		}
	}()

	select {
	case <-ctx.Done():
		logger.Info("正在停止服务")
	case serveErr := <-errCh:
		logger.Error("HTTP 服务异常退出", "error", serveErr)
		stop()
	}

	shutdownCtx, cancel := context.WithTimeout(context.Background(), cfg.ShutdownTimeout)
	defer cancel()
	if err := server.Shutdown(shutdownCtx); err != nil {
		logger.Error("停止 HTTP 服务失败", "error", err)
	}
	if err := manager.Close(shutdownCtx); err != nil {
		logger.Error("停止 TeslaUSB 状态管理器失败", "error", err)
	}
}

func subscribeReloadSignals() (<-chan os.Signal, func()) {
	signals := platform.ReloadSignals()
	if len(signals) == 0 {
		return nil, func() {}
	}

	reloadCh := make(chan os.Signal, 1)
	signal.Notify(reloadCh, signals...)
	return reloadCh, func() { signal.Stop(reloadCh) }
}

func watchReloadSignal(ctx context.Context, manager *service.Manager, reloadCh <-chan os.Signal) {
	if reloadCh == nil {
		return
	}
	go func() {
		for {
			select {
			case <-ctx.Done():
				return
			case <-reloadCh:
				manager.TriggerRefresh()
			}
		}
	}()
}
