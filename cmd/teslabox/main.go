package main

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/wen-ryon/teslabox/internal/api"
	"github.com/wen-ryon/teslabox/internal/backing"
	"github.com/wen-ryon/teslabox/internal/config"
	"github.com/wen-ryon/teslabox/internal/gadget"
	"github.com/wen-ryon/teslabox/internal/platform"
	"github.com/wen-ryon/teslabox/internal/service"
	"github.com/wen-ryon/teslabox/internal/storage"
	"github.com/wen-ryon/teslabox/internal/system"
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
	manager := service.NewManager(cfg, detector, inspector, prober, logger)
	handler := api.NewHandler(manager, logger)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	go manager.Run(ctx)
	watchReloadSignal(ctx, manager)

	server := &http.Server{
		Addr:              cfg.HTTPAddr,
		Handler:           handler,
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       60 * time.Second,
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
