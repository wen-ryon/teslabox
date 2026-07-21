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

## 第一次安装

首次安装仅支持 **Radxa ROCK 3A + ARM64 + Radxa 官方 Debian 11**。安装时请使用网线联网、保持独立 Type-C 供电，并断开连接车机的 OTG 线。脚本不会自动格式化 U 盘，也不会默认安装 MT7922 测试内核。

在板端执行：

```sh
curl -fL https://gitee.com/TecheN1/teslabox-releases/raw/master/stable/install.sh -o teslabox-install.sh
chmod +x teslabox-install.sh
sudo ./teslabox-install.sh
```

脚本会自动安装 TeslaBox、systemd/udev、Rockchip FFmpeg + MPP/RGA 媒体工具链和签名在线升级组件。全新安装的管理页面初始密码为 `teslabox`，首次登录后必须修改。

安装末尾会单独询问是否安装 MT7922。该选项会替换为 `5.10.160teslabox1` **测试内核**并安装 backports 驱动，不属于 Radxa 官方维护范围；已有无线网卡能正常使用时请直接回车跳过。详细前提、安装内容、存储初始化和故障处理见 [第一次安装教程](docs/zh-CN/FIRST_INSTALL.md)。

## 当前开发状态

第一项核心功能是 TeslaUSB 模拟模式：ROCK 3A 通过 USB OTG 向 Tesla 暴露虚拟磁盘，外接 128GB U 盘保存磁盘镜像和一致性快照，TF 卡只运行 Debian。

当前已完成 Go 服务基础框架、数据盘显式初始化、稳定标识自动挂载、TeslaCam backing image、USB Mass Storage Gadget、掉盘保护、reflink 快照、只读挂载、媒体索引原子切换、异常断电遗留资源回收、空间不足降级、失败退避、边录边放验收工具，以及支持 HTTP Range 的播放和下载 API；板端已扫描并播放实际录像，下一步进行 Tesla 持续写入时的并行验收。

- [V0.1 实现与运行说明](docs/zh-CN/V0.1_IMPLEMENTATION.md)
- [TeslaUSB 模拟模式需求](docs/zh-CN/TESLAUSB_REQUIREMENTS.md)
- [设置中心第一版需求与交接说明](docs/zh-CN/SETTINGS_CENTER_HANDOFF.md)
- [Windows 构建与 ROCK 3A 部署说明](docs/zh-CN/DEPLOYMENT.md)
- [远程播放、视频导出与云端访问开发流程](docs/zh-CN/REMOTE_PLAYBACK_EXPORT_PLAN.md)
- [签名在线更新与 Gitee 发布说明](docs/zh-CN/ONLINE_UPDATE.md)
- [第一次安装教程](docs/zh-CN/FIRST_INSTALL.md)
