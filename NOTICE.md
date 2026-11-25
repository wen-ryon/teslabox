# 版权与第三方项目

TeslaBox 项目自有代码：Copyright (c) 2026 TecheN1。当前工作树按 [GNU GPL 3.0](LICENSE)（`GPL-3.0-only`）许可。项目此前为闭源，历史实际分发及授权情况仍需核实。

- `experiment/carplay-demo` 的代码及说明提及参考 [xcertplay](https://github.com/shilapi/xcertplay) 和 [LIVI](https://github.com/f-io/LIVI)。历史 RC2 分支的 CarPlay 运行时加载固定 LIVI 提交的代码。Based on LIVI by Lasse Heitgres. 这项署名不代替逐文件版权审查；具体上游版本、复制或改动范围及其他适用声明仍须确认。
- 实验模块中可见的上游参考包括 iAP2 的 `endpoint`、`link`、`session`、`csm`，`transport/usbmux`，以及 `i2c`、`mfi` 和 `tools/mfi_tool.py`；各文件内保留了更具体的参考注释。对照的 xcertplay 快照为 [`1298fb35`](https://github.com/shilapi/xcertplay/tree/1298fb35e29ee30e7bd849cb0e751b52aa22a13f)，其 GPL 3.0 全文与本仓库根许可证一致。这个对照不能确定开发时所用的确切提交或全部代码来源。
- 历史 CarPlay 运行时使用的 LIVI 代码固定在 [`a956223`](https://github.com/f-io/LIVI/tree/a9562234429fc9d19d9f9804b6b288f9d435a922)，其上游参考项目和第三方组件说明见该版本的 [CREDITS.md](https://github.com/f-io/LIVI/blob/a9562234429fc9d19d9f9804b6b288f9d435a922/CREDITS.md)。该上游列表不表示 TeslaBox 使用了其中所有组件。
- 历史 CarPlay 原生运行时的依赖清单引用 [carplayd 固定提交](https://github.com/lvalen91/carplayd/tree/4eb254499aa00f456aea91d00755cf150c7d18a4)中的 `carplay-iap2-core` 和 `carplay-metadata`；相应发行源码材料所附上游根许可证为 Unlicense。历史组件的完整依赖及对应源码状态见[开源审查记录](docs/OPEN_SOURCE_REVIEW.md)。
- 历史 RC1 CarPlay 运行时使用 Node.js v22.23.1，其版权及第三方组件许可见 [Node.js 许可证](https://github.com/nodejs/node/blob/v22.23.1/LICENSE)。
- 维护者说明，使用过的 MFi 身份材料来源于第三方 DiPlay APK。公开源码不应包含该 APK、从中提取的身份私钥或证书。相关功能需要使用者自行合法取得所需材料；其授权和分发条件独立于本项目及开源依赖的许可。公开前的检查范围和证据见开源审查记录。
- 网页图标的版权及许可见 [THIRD_PARTY_NOTICES.txt](internal/webui/assets/THIRD_PARTY_NOTICES.txt)。
- 主线 Go 依赖 `github.com/godbus/dbus/v5` v5.2.2 的许可见 [godbus 许可](third_party/licenses/godbus-dbus-v5-LICENSE)；`golang.org/x/sys` v0.27.0 的许可见 [x/sys 许可](third_party/licenses/golang-x-sys-v0.27.0-LICENSE)。其他分支的依赖版本可能不同，须按相应 `go.mod` 核对。
- 历史 RC2 还声明 `golang.org/x/sys` v0.49.0 和 `golang.org/x/net` v0.61.0；本机这两个版本的顶层许可证与上述 x/sys 许可文件逐字节一致。各版本的子目录、传递依赖及二进制分发仍须单独核对。
- Go Authors 组件附带的专利授权文本见 [PATENTS](third_party/licenses/golang-x-PATENTS)。
- 编译依赖、安装脚本下载的程序、媒体工具、内核、模块与固件可能有独立许可，不能仅以本文件替代各自的许可、版权声明及源码要求。
- 历史 RTL8852BE 驱动源码中有文件保留独立 BSD 许可提示，这些文件与 [Radxa 8852be-dkms 的一个历史版本](https://github.com/radxa-pkg/8852be-dkms/tree/c0c830527f3b607c985286f1f196fefe53eed407)一致，其中部分也与 hostapd 上游一致；[hostapd 2.10 的 README 及 BSD 许可](third_party/licenses/hostapd-2.10-README)已保留供核对。历史 MT7922 固件也附有独立许可；完整来源和再分发条件尚未核实完毕，见[开源审查记录](docs/OPEN_SOURCE_REVIEW.md)。

更多待完成项目和审查边界见[开源审查记录](docs/OPEN_SOURCE_REVIEW.md)。
