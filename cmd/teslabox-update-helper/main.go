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
	recoverState := flag.Bool("recover", false, "启动前恢复未提交的更新事务")
	protocol := flag.Bool("protocol", false, "输出安装事务协议")
	flag.Parse()
	if *protocol {
		fmt.Println(updater.TransactionProtocol)
		return
	}
	config := updater.HelperConfig{
		RequestPath: *requestPath, ResultPath: *resultPath,
		DownloadDir:     "/var/lib/teslabox-system/updates",
		BackupDir:       "/var/lib/teslabox-system/update-backups",
		TargetPath:      "/usr/local/bin/teslabox",
		ExportStatePath: os.Getenv("TESLABOX_EXPORT_STATE"),
		Service:         "teslabox.service", HealthURL: "http://127.0.0.1/healthz",
		Timeout: 45 * time.Second,
	}
	var err error
	if *recoverState {
		err = updater.RecoverBeforeStart(context.Background(), config)
	} else {
		err = updater.RunHelper(context.Background(), config)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
