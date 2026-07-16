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
	"github.com/wen-ryon/teslabox/internal/gadget"
	"github.com/wen-ryon/teslabox/internal/platform"
	"github.com/wen-ryon/teslabox/internal/service"
	mediasnapshot "github.com/wen-ryon/teslabox/internal/snapshot"
	"github.com/wen-ryon/teslabox/internal/storage"
	"github.com/wen-ryon/teslabox/internal/system"
	"github.com/wen-ryon/teslabox/internal/teslacam"
	"github.com/wen-ryon/teslabox/internal/webauth"
	"github.com/wen-ryon/teslabox/internal/webui"
)

func main() {
	logger := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	cfg, err := config.Load()
	if err != nil {
		logger.Error("加载配置失败", "error", err)
		os.Exit(1)
	}

	runner := system.NewOSRunner()
	detector := storage.NewLSBLKDetector(runner, cfg.DataUUID, cfg.DataLabel)
	inspector := backing.NewInspector(backing.Layout{
		Root:         cfg.DataMountPath,
		ImagePath:    cfg.ImagePath,
		SnapshotPath: cfg.SnapshotPath,
		IndexPath:    cfg.IndexPath,
		StatePath:    cfg.StatePath,
	})
	prober := gadget.NewFSProber(cfg.ConfigFSPath, cfg.UDCPath)
	mounter := datamount.New(runner, cfg.DataMountPath, cfg.DataUUID, cfg.DataLabel)
	gadgetManager := gadget.NewManager(gadget.Config{
		ConfigFSPath:  cfg.ConfigFSPath,
		UDCPath:       cfg.UDCPath,
		GadgetName:    cfg.GadgetName,
		UDCName:       cfg.UDCName,
		ImagePath:     filepath.Join(cfg.DataMountPath, filepath.FromSlash(cfg.ImagePath)),
		SerialNumber:  cfg.GadgetSerial,
		ReleaseGadget: cfg.ReleaseGadget,
	})
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
	authManager, err := webauth.New(webauth.Config{
		PasswordSHA256: cfg.WebPasswordSHA256,
		SessionSecret:  cfg.WebSessionSecret,
		SessionTTL:     cfg.WebSessionTTL,
	})
	if err != nil {
		logger.Error("初始化 Web 管理认证失败", "error", err)
		os.Exit(1)
	}
	deviceAdmin := deviceadmin.New(runner, logger)
	apiHandler := api.NewHandler(manager, logger, api.WithDeviceAdmin(deviceAdmin))
	handler := webui.NewHandler(apiHandler, authManager, logger)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	go manager.Run(ctx)
	watchReloadSignal(ctx, manager)

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

func watchReloadSignal(ctx context.Context, manager *service.Manager) {
	signals := platform.ReloadSignals()
	if len(signals) == 0 {
		return
	}

	reloadCh := make(chan os.Signal, 1)
	signal.Notify(reloadCh, signals...)
	go func() {
		defer signal.Stop(reloadCh)
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
