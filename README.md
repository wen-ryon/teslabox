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

当前已完成 Go 服务基础框架、稳定标识数据盘发现、backing store/UDC/configfs 只读预检、TeslaUSB 状态 API、TeslaCam 扫描器和视频列表 API；数据盘初始化、USB Gadget、backing image 和快照读取链路尚待实现。

- [V0.1 实现与运行说明](docs/zh-CN/V0.1_IMPLEMENTATION.md)
- [TeslaUSB 模拟模式需求](docs/zh-CN/TESLAUSB_REQUIREMENTS.md)
