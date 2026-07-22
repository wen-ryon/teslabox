package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/wen-ryon/teslabox/internal/imageinit"
	"github.com/wen-ryon/teslabox/internal/system"
)

const operationTimeout = 10 * time.Minute

func main() {
	options := imageinit.Options{}
	flag.StringVar(&options.DataRoot, "data-root", "/var/lib/teslabox", "已挂载的 XFS backing store")
	flag.StringVar(&options.ImagePath, "image", "/var/lib/teslabox/images/teslacam.img", "TeslaCam 虚拟整盘镜像")
	flag.Int64Var(&options.SizeBytes, "size-bytes", 96_000_000_000, "虚拟磁盘容量，默认保留约 28GB 物理空间")
	flag.StringVar(&options.Label, "label", "TESLACAM", "Tesla 可识别的 exFAT 卷标")
	flag.BoolVar(&options.MusicEnabled, "music", false, "创建独立的 FAT32 音乐分区")
	flag.IntVar(&options.TeslaCamPercent, "teslacam-percent", 80, "启用音乐分区时 TeslaCam 所占比例")
	flag.StringVar(&options.MusicLabel, "music-label", "MUSIC", "FAT32 音乐分区卷标")
	flag.BoolVar(&options.Execute, "execute", false, "实际创建并格式化镜像；默认仅预演")
	flag.Parse()
	if err := imageinit.RequireRoot(options.Execute); err != nil {
		fmt.Fprintf(os.Stderr, "镜像初始化失败: %v\n", err)
		os.Exit(1)
	}

	ctx, cancel := context.WithTimeout(context.Background(), operationTimeout)
	defer cancel()
	runner := system.NewOSRunner()
	result, err := imageinit.New(imageinit.NewOSInspector(runner), runner).Run(ctx, options)
	if err != nil {
		fmt.Fprintf(os.Stderr, "镜像初始化失败: %v\n", err)
		os.Exit(1)
	}

	fmt.Printf("backing store: %s (%s, reflink=%t)\n", result.Environment.DataRoot, result.Environment.FileSystem, result.Environment.Reflink)
	fmt.Printf("镜像: %s\n", result.Environment.ImagePath)
	fmt.Printf("物理容量/镜像容量/预留容量: %d / %d / %d bytes\n", result.Environment.TotalBytes, options.SizeBytes, result.Environment.TotalBytes-options.SizeBytes)
	fmt.Printf("TeslaCam/音乐分区容量: %d / %d bytes\n", result.Layout.TeslaCamBytes, result.Layout.MusicBytes)
	for index, command := range result.Commands {
		fmt.Printf("%02d. %s %s\n", index+1, command.Name, strings.Join(command.Args, " "))
	}
	if result.WasExecuted {
		fmt.Println("TeslaCam 镜像初始化完成")
		return
	}
	fmt.Println("当前为 dry-run，未创建镜像；确认无误后增加 --execute")
}
