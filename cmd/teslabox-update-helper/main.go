package main

import (
	"context"
	"encoding/json"
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
	kernelManifest := flag.String("kernel-manifest", "", "只读验证独立内核依赖签名")
	kernelResume := flag.Bool("kernel-resume", false, "新内核启动后继续原来的升级请求")
	kernelProtocol := flag.Bool("kernel-protocol", false, "输出强制内核升级协议")
	kernelVerify := flag.Bool("kernel-verify", false, "启动前只读确认内核强依赖")
	flag.Parse()
	if *kernelVerify {
		if err := updater.VerifyRunningKernel(context.Background()); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		fmt.Println("teslabox-kernel-ready-v1")
		return
	}
	if *kernelProtocol {
		fmt.Println("teslabox-kernel-first-v1")
		return
	}
	if *kernelManifest != "" {
		policy, err := updater.ReadKernelPolicy(*kernelManifest)
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		if err := json.NewEncoder(os.Stdout).Encode(policy); err != nil {
			os.Exit(1)
		}
		return
	}
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
	if *kernelResume {
		pending, err := updater.KernelResumePending()
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		if !pending {
			return
		}
		config.RequestPath = updater.KernelResumeRequestPath()
		config.KernelResume = true
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
