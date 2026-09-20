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
	_ "time/tzdata"

	"github.com/wen-ryon/teslabox/internal/api"
	"github.com/wen-ryon/teslabox/internal/backing"
	"github.com/wen-ryon/teslabox/internal/config"
	"github.com/wen-ryon/teslabox/internal/datamount"
	"github.com/wen-ryon/teslabox/internal/deviceadmin"
	"github.com/wen-ryon/teslabox/internal/diagnostics"
	"github.com/wen-ryon/teslabox/internal/driveinit"
	"github.com/wen-ryon/teslabox/internal/fancontrol"
	"github.com/wen-ryon/teslabox/internal/gadget"
	"github.com/wen-ryon/teslabox/internal/hostmigration"
	"github.com/wen-ryon/teslabox/internal/imageinit"
	"github.com/wen-ryon/teslabox/internal/maintenanceguard"
	"github.com/wen-ryon/teslabox/internal/maintenancemode"
	"github.com/wen-ryon/teslabox/internal/media"
	"github.com/wen-ryon/teslabox/internal/mediaassets"
	"github.com/wen-ryon/teslabox/internal/mediabundle"
	"github.com/wen-ryon/teslabox/internal/onboarding"
	"github.com/wen-ryon/teslabox/internal/platform"
	"github.com/wen-ryon/teslabox/internal/service"
	mediasnapshot "github.com/wen-ryon/teslabox/internal/snapshot"
	"github.com/wen-ryon/teslabox/internal/storage"
	"github.com/wen-ryon/teslabox/internal/storageadmin"
	"github.com/wen-ryon/teslabox/internal/storagebench"
	"github.com/wen-ryon/teslabox/internal/storagepolicy"
	"github.com/wen-ryon/teslabox/internal/system"
	"github.com/wen-ryon/teslabox/internal/teslacam"
	"github.com/wen-ryon/teslabox/internal/updater"
	buildversion "github.com/wen-ryon/teslabox/internal/version"
	"github.com/wen-ryon/teslabox/internal/webauth"
	"github.com/wen-ryon/teslabox/internal/webui"
	"github.com/wen-ryon/teslabox/internal/writeback"
)

const teslaboxFSCKExFATPath = "/opt/teslabox/media/bin/teslabox-fsck.exfat"

func main() {
	processStartedAt := time.Now()
	reloadCh, stopReloadSignals := subscribeReloadSignals()
	defer stopReloadSignals()

	logger := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	cfg, err := config.Load()
	if err != nil {
		if errors.Is(err, storagepolicy.ErrPolicyInvalid) {
			logger.Error("加载配置失败", "reason_code", storagepolicy.ReasonPolicyInvalid, "error", err)
		} else {
			logger.Error("加载配置失败", "error", err)
		}
		os.Exit(1)
	}
	// 风扇控制必须先于存储、网络和媒体初始化接管硬件，避免启动负载叠加。
	fanManager, err := fancontrol.New(fancontrol.Config{
		StatePath:       "/var/lib/teslabox-system/fan-settings.json",
		PWMChipPath:     "/sys/class/pwm/pwmchip8",
		TemperaturePath: "/sys/class/thermal/thermal_zone0/temp",
		PeriodNS:        10000,
		ActiveLow:       true,
		PollInterval:    5 * time.Second,
		StartupDelay:    20 * time.Second,
		Logger:          logger,
	})
	if err != nil {
		logger.Error("初始化风扇设置失败", "error", err)
		os.Exit(1)
	}
	spacePolicy, err := storagepolicy.NewSnapshotReservePolicy(cfg.MinFreeBytes, cfg.MinFreePercent, cfg.InitializationHeadroomBytes)
	if err != nil {
		logger.Error("初始化快照空间策略失败", "error", err)
		os.Exit(1)
	}
	// 主状态机已经在同一临界区维护 gadget-paused；独立命令才需要额外读取该文件。
	maintenanceGuard, err := maintenanceguard.New(cfg.MaintenanceLockPath, cfg.MaintenanceOperationPath)
	if err != nil {
		logger.Error("初始化存储维护守卫失败", "error", err)
		os.Exit(1)
	}
	if state, exists, recoverErr := maintenanceGuard.RecoverInterrupted(); recoverErr != nil {
		logger.Error("恢复存储维护状态失败，将拒绝重新绑定 Gadget", "error", recoverErr)
	} else if exists && state.State == maintenanceguard.StateInterrupted {
		logger.Warn("检测到上次未完成的存储维护，将保持 Gadget 暂停", "operation", state.Operation)
	}
	teslaTimeLocation, err := time.LoadLocation(cfg.TeslaTimeZone)
	if err != nil {
		logger.Error("加载车辆录像时区失败", "time_zone", cfg.TeslaTimeZone, "error", err)
		os.Exit(1)
	}
	if err := writeback.ApplyDefault(); err != nil {
		logger.Warn("无法应用稳定写入阈值，将使用系统默认写回策略", "error", err)
	} else {
		logger.Info(
			"已应用稳定写入阈值",
			"background_bytes", writeback.DefaultBackgroundBytes,
			"dirty_bytes", writeback.DefaultDirtyBytes,
		)
	}

	runner := system.NewOSRunner()
	fanMigrationCtx, fanMigrationCancel := context.WithTimeout(context.Background(), 10*time.Second)
	fanComponentsChanged, fanMigrationErr := hostmigration.EnsureFanEarlyComponents(
		fanMigrationCtx,
		hostmigration.FanComponentPaths{
			Script: "/usr/local/libexec/teslabox-fan-early.sh",
			Unit:   "/etc/systemd/system/teslabox-fan-early.service",
		},
		runner,
	)
	fanMigrationCancel()
	if fanMigrationErr != nil {
		logger.Warn("无法安装早期风扇控制组件，主程序内风扇控制继续生效", "error", fanMigrationErr)
	} else if fanComponentsChanged {
		logger.Info("已安装早期风扇控制组件，将在下次开机提前停止风扇")
	}
	migrationCtx, migrationCancel := context.WithTimeout(context.Background(), 10*time.Second)
	udevMigrated, migrationErr := hostmigration.RemoveLegacyUdevSignal(
		migrationCtx,
		"/etc/udev/rules.d/99-teslabox.rules",
		runner,
	)
	migrationCancel()
	if migrationErr != nil {
		logger.Warn("无法迁移旧版 udev 信号规则，主服务继续启动", "error", migrationErr)
	} else if udevMigrated {
		logger.Info("已迁移旧版 udev 规则并重新加载")
	}
	apMigrationCtx, apMigrationCancel := context.WithTimeout(context.Background(), 10*time.Second)
	apMigration, apMigrationErr := hostmigration.UpgradeLegacyAPComponents(
		apMigrationCtx,
		hostmigration.APComponentPaths{
			Script: "/usr/local/libexec/teslabox-ap-setup.sh",
			Unit:   "/etc/systemd/system/teslabox-ap.service",
		},
		runner,
	)
	apMigrationCancel()
	if apMigrationErr != nil {
		logger.Warn("无法迁移旧版 AP 系统组件，主服务继续启动", "error", apMigrationErr)
	} else if apMigration.Script || apMigration.Unit {
		logger.Info(
			"已迁移旧版 AP 系统组件，下次开机或无线网卡热插拔时生效",
			"script", apMigration.Script,
			"unit", apMigration.Unit,
		)
	}
	guardCtx, guardCancel := context.WithTimeout(context.Background(), 15*time.Second)
	protectedServices, guardErr := gadget.DisableConflictingServices(guardCtx, runner)
	guardCancel()
	if guardErr != nil {
		logger.Error("无法停用原厂 USB Gadget 服务", "error", guardErr)
		os.Exit(1)
	}
	if len(protectedServices) > 0 {
		logger.Info("已停用并屏蔽原厂 USB Gadget 服务", "services", protectedServices)
	}
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
		ImagePath:    filepath.Join(cfg.DataMountPath, filepath.FromSlash(cfg.ImagePath)),
		SnapshotPath: filepath.Join(cfg.DataMountPath, filepath.FromSlash(cfg.SnapshotPath)),
		MountRoot:    cfg.SnapshotMountPath,
		FSCKPath:     teslaboxFSCKExFATPath,
		Policy:       spacePolicy,
	})
	guardedGadget := gadget.NewGuardedLifecycle(gadgetManager, maintenanceGuard)
	manager := service.NewManager(cfg, detector, inspector, prober, mounter, guardedGadget, snapshotManager, teslacam.NewScanner(teslaTimeLocation), logger, maintenanceGuard)
	manager.ConfigureSpaceRecovery("/var/lib/teslabox-system/space-recovery.once", func(parent context.Context) error {
		rebootCtx, cancel := context.WithTimeout(parent, 10*time.Second)
		defer cancel()
		_, err := runner.Run(rebootCtx, "systemctl", "--no-block", "reboot")
		return err
	})
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// TeslaUSB 是车辆启动时最有时效性的链路；Web、网络和媒体管理组件不应占用其首次绑定机会。
	go manager.Run(ctx)
	logger.Info("TeslaUSB 核心状态机已启动", "startup_elapsed_ms", time.Since(processStartedAt).Milliseconds())
	if err := mediabundle.EnsureEmbedded(ctx, logger); err != nil {
		logger.Error("安装正式版本内置媒体运行时失败", "error", err)
		os.Exit(1)
	}

	driveInspector := driveinit.NewLSBLKInspector(runner)
	storageAdmin := storageadmin.New(storageadmin.Config{
		DataRoot:   cfg.DataMountPath,
		ImagePath:  filepath.Join(cfg.DataMountPath, filepath.FromSlash(cfg.ImagePath)),
		DataLabel:  cfg.DataLabel,
		ImageLabel: "TESLACAM",
		FSCKPath:   teslaboxFSCKExFATPath,
		Policy:     spacePolicy,
	}, runner, registry, storageadmin.NewOSDeviceLister(driveInspector), driveinit.New(driveInspector, runner), imageinit.New(imageinit.NewOSInspector(runner), runner), manager, storagebench.New(storagebench.DefaultConfig()))
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
	maintenanceSettings, err := maintenancemode.New(cfg.MaintenanceStatePath)
	if err != nil {
		logger.Error("初始化自动维护设置失败", "error", err)
		os.Exit(1)
	}
	onboardingManager, err := onboarding.New(cfg.OnboardingStatePath)
	if err != nil {
		logger.Error("初始化首次引导状态失败", "error", err)
		os.Exit(1)
	}
	deviceAdmin := deviceadmin.New(runner, logger, deviceadmin.NetworkConfig{
		WiFiInterface: cfg.WiFiInterface,
		APInterface:   cfg.APInterface,
	})
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
	mediaCompositor.SetActivityHook(func() func() { return fanManager.Begin("multi") })
	mediaExporter, err := media.NewExporter(runner, media.ExportConfig{
		FFmpegPath:    cfg.FFmpegPath,
		OutputDir:     filepath.Join(cfg.DataMountPath, filepath.FromSlash(cfg.ExportPath)),
		StatePath:     cfg.ExportStatePath,
		QueueSize:     cfg.ExportQueueSize,
		MaxDuration:   cfg.ExportMaxDuration,
		Logger:        logger,
		BeginActivity: func() func() { return fanManager.Begin("export") },
	})
	if err != nil {
		logger.Error("初始化视频导出器失败", "error", err)
		os.Exit(1)
	}
	mediaAssets, err := mediaassets.New(mediaassets.Config{
		ImagePath:      filepath.Join(cfg.DataMountPath, filepath.FromSlash(cfg.ImagePath)),
		StatePath:      filepath.Join(cfg.DataMountPath, filepath.FromSlash(cfg.StatePath)),
		StagingPath:    filepath.Join(cfg.DataMountPath, filepath.FromSlash(cfg.StatePath), "media-staging"),
		MountPath:      "/run/teslabox/media-assets",
		FSCKPath:       teslaboxFSCKExFATPath,
		Policy:         spacePolicy,
		OperationGuard: maintenanceGuard,
	}, runner, manager, manager)
	if err != nil {
		logger.Error("初始化媒体中心失败，拒绝以不完整写入保护启动", "error", err)
		os.Exit(1)
	}
	// 升级或服务启动后只读取外层 XFS；不足时提前提示，但不在线扫描车机可能正在写入的 exFAT 位图。
	if reserveStatus, reserveErr := mediaAssets.SnapshotReserveRepairStatus(); reserveErr != nil {
		logger.Warn("启动阶段检查快照预留空间失败", "reason_code", "space_measurement_failed", "error", reserveErr)
	} else if reserveStatus.OuterTotalBytes > 0 && !reserveStatus.RuntimeReady {
		logger.Warn(
			"快照预留空间低于运行期门槛，等待用户进入维护窗口修复",
			"reason_code", reserveStatus.ReasonCode,
			"free_bytes", reserveStatus.OuterFreeBytes,
			"required_bytes", reserveStatus.RequiredBytes,
			"deficit_bytes", reserveStatus.DeficitBytes,
		)
	}
	updateManager, err := updater.New(updater.Config{
		ManifestURL: cfg.UpdateManifestURL, CatalogURL: cfg.UpdateCatalogURL, PublicKey: cfg.UpdatePublicKey,
		BetaManifestURL: cfg.UpdateBetaManifestURL, BetaCatalogURL: cfg.UpdateBetaCatalogURL,
		Channel: cfg.UpdateChannel, ChannelStatePath: cfg.UpdateChannelStatePath, Board: cfg.UpdateBoard,
		CurrentVersion: buildversion.Version, CurrentCommit: buildversion.Commit,
		DownloadDir: cfg.UpdateDownloadDir, RequestPath: cfg.UpdateRequestPath,
		ResultPath: cfg.UpdateResultPath, MaximumSize: cfg.UpdateMaxBytes, Logger: logger,
		CanInstall: func() error {
			if manager.Snapshot().GadgetStatus.Bound {
				return errors.New("USB Gadget 尚未安全断开，请确认已拔掉 OTG 后重试")
			}
			if mediaExporter.Busy() {
				return errors.New("仍有视频导出任务，完成或取消后再安装更新")
			}
			return nil
		},
		// 在线更新应独立于媒体写入策略自动重启，避免更新后 Gadget 停留在安全断开状态。
		AutoReboot: func() bool { return maintenanceSettings.Settings().UpdateAutoRecovery },
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
		api.WithGadgetAdmin(manager),
		api.WithMediaInspector(mediaInspector),
		api.WithMediaAssetsAdmin(mediaAssets),
		api.WithMediaCompositor(mediaCompositor),
		api.WithExportAdmin(mediaExporter),
		api.WithUpdateAdmin(updateManager),
		api.WithMaintenanceAutomation(maintenanceSettings),
		api.WithDiagnostics(diagnostics.New(runner)),
		api.WithFanAdmin(fanManager),
		api.WithOnboardingAdmin(onboardingManager),
	)
	handler := webui.NewHandler(apiHandler, authManager, logger)
	go deviceAdmin.Run(ctx)
	go mediaExporter.Run(ctx)
	go fanManager.Run(ctx)
	watchReloadSignal(ctx, manager, reloadCh)

	server := &http.Server{
		Addr:              cfg.HTTPAddr,
		Handler:           handler,
		ReadHeaderTimeout: 5 * time.Second,
		// 音乐批量上传可能经过车载热点，使用受认证、总大小受限的较长读取窗口避免大文件被错误中断。
		ReadTimeout: 15 * time.Minute,
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
	if err := mediaExporter.Wait(shutdownCtx); err != nil {
		logger.Error("等待视频导出器停止失败", "error", err)
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
