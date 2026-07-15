package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/wen-ryon/teslabox/internal/driveinit"
	"github.com/wen-ryon/teslabox/internal/system"
)

const operationTimeout = 10 * time.Minute

func main() {
	options := driveinit.Options{}
	flag.StringVar(&options.Device, "device", "", "USB 整盘的 /dev/disk/by-id/usb-* 稳定路径")
	flag.StringVar(&options.ConfirmSerial, "confirm-serial", "", "必须与目标整盘序列号完全一致")
	flag.StringVar(&options.Label, "label", "TESLABOX", "XFS 文件系统标签")
	flag.StringVar(&options.MountPath, "mount-path", "/var/lib/teslabox", "数据盘挂载目录")
	flag.BoolVar(&options.Execute, "execute", false, "实际擦除、分区并格式化；默认仅预演")
	flag.Parse()
	if err := driveinit.RequireRoot(options.Execute); err != nil {
		fmt.Fprintf(os.Stderr, "初始化失败: %v\n", err)
		os.Exit(1)
	}

	ctx, cancel := context.WithTimeout(context.Background(), operationTimeout)
	defer cancel()

	runner := system.NewOSRunner()
	result, err := driveinit.New(driveinit.NewLSBLKInspector(runner), runner).Run(ctx, options)
	if err != nil {
		fmt.Fprintf(os.Stderr, "初始化失败: %v\n", err)
		os.Exit(1)
	}

	fmt.Printf("目标设备: %s -> %s\n", result.Device.RequestedPath, result.Device.ResolvedPath)
	fmt.Printf("型号/序列号: %s / %s\n", result.Device.Model, result.Device.Serial)
	fmt.Printf("容量: %d bytes\n", result.Device.Size)
	fmt.Printf("目标分区: %s\n", result.Partition)
	for index, command := range result.Commands {
		fmt.Printf("%02d. %s %s\n", index+1, command.Name, strings.Join(command.Args, " "))
	}
	if result.WasExecuted {
		fmt.Println("数据盘初始化完成")
		return
	}
	fmt.Println("当前为 dry-run，未修改磁盘；确认无误后增加 --execute")
}
