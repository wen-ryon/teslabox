package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"time"

	"github.com/wen-ryon/teslabox/internal/updater"
)

func main() {
	requestPath := flag.String("request", "/var/lib/teslabox-system/update-request.json", "安装请求文件")
	resultPath := flag.String("result", "/var/lib/teslabox-system/update-result.json", "安装结果文件")
	flag.Parse()
	config := updater.HelperConfig{
		RequestPath: *requestPath, ResultPath: *resultPath,
		DownloadDir: "/var/lib/teslabox-system/updates",
		BackupDir:   "/var/lib/teslabox-system/update-backups",
		TargetPath:  "/usr/local/bin/teslabox",
		Service:     "teslabox.service", HealthURL: "http://127.0.0.1/healthz",
		Timeout: 45 * time.Second,
	}
	if err := updater.RunHelper(context.Background(), config); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
