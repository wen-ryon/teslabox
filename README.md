# 特斯拉盒子 TeslaBox

一个 all-in-one 的特斯拉边缘设备。
采用 Go 语言开发，基于 Radxa ROCK 3A。
支持特斯拉摄像头、OTA升级、AP路由器、无线CarPlay、NAS备份等功能。

本项目使用了 ChatGPT 的 Codex 基于 5.6 Sol 模型来辅助开发。


An all-in-one Tesla Edge Device for Tesla vehicles.
Built with Go, powered by Radxa ROCK 3A.
Support TeslaCam, OTA, AP Router, Wireless CarPlay, NAS Backup and more.


This project was developed with the assistance of ChatGPT Codex and the GPT-5.6 Sol model.

---

🌏 Language

- 🇨🇳 [简体中文](docs/zh-CN/README.md)
- 🇺🇸 [English](docs/en/README.md)

## 当前开发状态

第一项核心功能是 TeslaUSB 模拟模式：ROCK 3A 通过 USB OTG 向 Tesla 暴露虚拟磁盘，外接 128GB U 盘保存磁盘镜像和一致性快照，TF 卡只运行 Debian。

当前已完成 Go 服务基础框架、数据盘显式初始化、稳定标识自动挂载、TeslaCam backing image、USB Mass Storage Gadget、掉盘保护、reflink 快照、只读挂载、媒体索引原子切换、异常断电遗留资源回收、空间不足降级、失败退避、边录边放验收工具，以及支持 HTTP Range 的播放和下载 API；板端已扫描并播放实际录像，下一步进行 Tesla 持续写入时的并行验收。

- [V0.1 实现与运行说明](docs/zh-CN/V0.1_IMPLEMENTATION.md)
- [TeslaUSB 模拟模式需求](docs/zh-CN/TESLAUSB_REQUIREMENTS.md)
- [设置中心第一版需求与交接说明](docs/zh-CN/SETTINGS_CENTER_HANDOFF.md)
- [Windows 构建与 ROCK 3A 部署说明](docs/zh-CN/DEPLOYMENT.md)
- [远程播放、视频导出与云端访问开发流程](docs/zh-CN/REMOTE_PLAYBACK_EXPORT_PLAN.md)
- [签名在线更新与 Gitee 发布说明](docs/zh-CN/ONLINE_UPDATE.md)
