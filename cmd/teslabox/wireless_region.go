package main

import (
	"context"
	"fmt"
	"os"
	"time"

	"github.com/wen-ryon/teslabox/internal/regulatory"
	"github.com/wen-ryon/teslabox/internal/system"
)

// 首装在启动 AP 前调用这个受限入口，不加载存储、Gadget 或媒体服务。
func initializeWirelessRegion() int {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	manager := regulatory.New(system.NewOSRunner(), regulatory.DefaultPaths())
	if err := manager.Initialize(ctx); err != nil {
		fmt.Fprintln(os.Stderr, "无线地区初始化失败，未覆盖既有配置:", err)
		return 1
	}
	if err := manager.Apply(ctx); err != nil {
		fmt.Fprintln(os.Stderr, "无线地区已保存，当前尚未生效:", err)
		return 1
	}
	fmt.Fprintln(os.Stdout, manager.DiagnosticSummary(ctx))
	return 0
}
