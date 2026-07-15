package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"

	"github.com/wen-ryon/teslabox/internal/gadget"
)

func main() {
	action := flag.String("action", "status", "操作：status、start 或 stop")
	execute := flag.Bool("execute", false, "实际执行 start 或 stop；默认仅显示状态和计划")
	configFS := flag.String("configfs", "/sys/kernel/config/usb_gadget", "USB Gadget configfs 路径")
	udcPath := flag.String("udc-path", "/sys/class/udc", "UDC 类目录")
	udcName := flag.String("udc", "", "指定 UDC；留空时要求系统只有一个 UDC")
	imagePath := flag.String("image", "/var/lib/teslabox/images/teslacam.img", "TeslaCam 整盘镜像")
	serial := flag.String("serial", "TESLABOX-071C4AAFBA8BEB80", "对主机暴露的 USB 序列号")
	releaseGadget := flag.String("release-gadget", "", "允许在 start 时只解绑的固定冲突 Gadget 名称")
	flag.Parse()

	if *action != "status" && *action != "start" && *action != "stop" {
		fmt.Fprintln(os.Stderr, "Gadget 操作失败: action 必须是 status、start 或 stop")
		os.Exit(2)
	}
	if err := gadget.RequireRoot(*execute && *action != "status"); err != nil {
		fmt.Fprintf(os.Stderr, "Gadget 操作失败: %v\n", err)
		os.Exit(1)
	}

	manager := gadget.NewManager(gadget.Config{
		ConfigFSPath:  *configFS,
		UDCPath:       *udcPath,
		GadgetName:    "teslabox",
		UDCName:       *udcName,
		ImagePath:     *imagePath,
		SerialNumber:  *serial,
		ReleaseGadget: *releaseGadget,
	})
	status, err := manager.Inspect()
	if err != nil {
		fmt.Fprintf(os.Stderr, "Gadget 操作失败: %v\n", err)
		os.Exit(1)
	}
	if *action != "status" && !*execute {
		if *action == "start" {
			status, _, err = manager.PreflightStart()
			if err != nil {
				fmt.Fprintf(os.Stderr, "Gadget 预检失败: %v\n", err)
				os.Exit(1)
			}
		}
		fmt.Printf("dry-run: 将执行 %s，增加 --execute 才会修改 configfs\n", *action)
		printStatus(status)
		return
	}
	if *execute {
		switch *action {
		case "start":
			status, err = manager.Start()
		case "stop":
			status, err = manager.Stop()
		}
		if err != nil {
			fmt.Fprintf(os.Stderr, "Gadget 操作失败: %v\n", err)
			os.Exit(1)
		}
	}
	printStatus(status)
}

func printStatus(status gadget.Status) {
	encoder := json.NewEncoder(os.Stdout)
	encoder.SetIndent("", "  ")
	if err := encoder.Encode(status); err != nil {
		fmt.Fprintf(os.Stderr, "输出 Gadget 状态失败: %v\n", err)
	}
}
