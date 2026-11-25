# TeslaBox

TeslaBox 是面向 Radxa ROCK 3A 的特斯拉辅助设备项目，使用 Go 开发。它通过 USB Gadget 向车机提供 TeslaCam 存储，并提供浏览器中的录像浏览、网络设置和签名更新等功能。

**项目已停止维护。** 此仓库按现状提供，可能存在未修复问题；请自行评估设备、数据和网络安全风险。TeslaBox 与 Tesla、Apple 均无关联。

## 当前范围

- 使用外接存储保存 TeslaCam 镜像，通过 ROCK 3A 的 USB OTG 向车机提供虚拟磁盘。
- 通过快照浏览和播放录像；正在写入的片段不保证可以播放。
- 提供网络、热点、系统设置和签名程序更新等管理功能。
- [`experiment/carplay-demo`](experiment/carplay-demo/README.md) 是独立实验模块，成熟度和许可状态见下文，不应当作已完成的正式功能。

## 使用与开发

目标环境为 ARM64 的 Radxa ROCK 3A 和 Radxa Debian 11。历史安装流程见[首次安装教程](docs/zh-CN/FIRST_INSTALL.md)；安装前请备份数据并自行核实外部下载地址及组件是否仍可用。不要在行车过程中操作设备。

开发环境需要 Go 1.26.3。运行主模块测试：

```sh
go test ./...
```

CarPlay 实验模块有独立的 `go.mod`，须在其目录单独测试。构建与部署参考[部署说明](docs/zh-CN/DEPLOYMENT.md)；其他文档从[中文文档索引](docs/zh-CN/README.md)进入。历史版本说明保留在 [release-notes](release-notes/)；它们描述的是发布当时的情况，不代表当前服务仍可用。

## 许可证与第三方组件

本项目自有代码当前按 [GNU GPL 3.0](LICENSE) 许可（SPDX：`GPL-3.0-only`）。仓库历史中曾使用 MIT 许可文本；项目此前为闭源，历史实际分发及授权情况仍需核实。[界面第三方组件声明](internal/webui/assets/THIRD_PARTY_NOTICES.txt)继续适用。

CarPlay 实验代码引用了 [xcertplay](https://github.com/shilapi/xcertplay) 和 [LIVI](https://github.com/f-io/LIVI) 的实现，两者上游标注 GPL 3.0 系列；具体代码来源、对应源码和必要署名**尚待核对**，根目录许可证的修改不表示第三方分发条件已全部满足。详见[开源审查记录](docs/OPEN_SOURCE_REVIEW.md)。

媒体工具、驱动、固件和其他外部下载组件可能有独立许可证，分发时需保留对应版权、许可和适用的源码材料。汇总见 [NOTICE.md](NOTICE.md)。
