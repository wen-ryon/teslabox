# TeslaBox 开发计划

> 本文档用于记录 TeslaBox 的开发路线图。
>
> 本项目采用 **迭代开发** 模式，每个版本都应该是可运行、可测试、可发布的版本，而不是一次性开发所有功能。

---

# 项目目标

TeslaBox 不仅仅是一个 TeslaCam 查看工具，而是一个基于 Linux 的 Tesla Edge Platform。

最终目标包括：

- Tesla USB 扩展
- TeslaCam 在线查看
- AP 路由器
- 4G / 5G 网络
- Wireless CarPlay
- OTA 更新
- NAS 自动备份
- 在线资源中心
- 插件系统

---

# 开发原则

## 小步迭代

每个版本都应该：

- 可以编译
- 可以运行
- 可以测试
- 可以发布

避免一次开发多个大型功能。

---

## Linux First

充分利用 Linux 已有能力，例如：

- udev
- systemd
- hostapd
- dnsmasq
- NetworkManager（待定）

业务逻辑由 Go 实现，系统能力交给 Linux。

---

## 模块化设计

每个功能模块尽可能独立，例如：

- USB
- TeslaCam
- OTA
- CarPlay
- Backup

彼此之间尽量低耦合。

---

# V0.1 基础框架

预计目标：

完成 Tesla USB 自动识别。

## 功能

- [ ] Go 项目初始化
- [ ] 配置管理
- [ ] 日志模块
- [ ] HTTP Server
- [ ] REST API
- [ ] USB 自动识别（udev）
- [ ] 自动挂载
- [ ] 自动卸载
- [ ] TeslaCam 检测
- [ ] 视频扫描
- [ ] 视频列表 API

完成后：

用户可以通过 API 查看 TeslaCam 文件列表。

---

# V0.2 Web 管理后台

预计目标：

完成基础 Web UI。

## 功能

- [ ] 登录页面
- [ ] 首页
- [ ] TeslaCam 浏览
- [ ] 视频详情
- [ ] 下载视频
- [ ] 视频缩略图
- [ ] 视频搜索
- [ ] 系统状态页面

完成后：

可以通过浏览器查看 TeslaCam。

---

# V0.3 在线播放

预计目标：

支持视频在线播放。

## 功能

- [ ] HTTP Range
- [ ] MP4 播放
- [ ] 四路视频同步播放
- [ ] 时间轴同步
- [ ] 快进
- [ ] 拖动播放
- [ ] 全屏

后续：

支持低码率播放。

---

# V0.4 AP Router

预计目标：

TeslaBox 成为一台路由器。

## 功能

- [ ] MT7922
- [ ] AP 模式
- [ ] hostapd
- [ ] dnsmasq
- [ ] DHCP
- [ ] NAT
- [ ] Web 配置页面
- [ ] 客户端管理

完成后：

手机、Tesla 都可以连接 TeslaBox Wi-Fi。

---

# V0.5 远程访问

预计目标：

支持远程访问。

## 功能

支持三种访问方式：

### Local AP

手机连接 TeslaBox 热点。

### Tailscale

无需公网 IP。

### Relay Server

通过公网服务器进行流量转发。

完成后：

任何地方都可以查看 TeslaCam。

---

# V0.6 4G / 5G

预计目标：

完全脱离手机热点。

## 功能

- [ ] USB Dongle
- [ ] M.2 模块（待定）
- [ ] 自动拨号
- [ ] 网络切换
- [ ] 信号强度
- [ ] 流量统计

---

# V0.7 OTA

预计目标：

支持远程升级。

## 功能

- [ ] 检查更新
- [ ] 下载更新
- [ ] 校验
- [ ] 自动升级
- [ ] 回滚

---

# V0.8 NAS 自动备份

预计目标：

自动同步 TeslaCam。

## 支持

- SMB
- NFS
- SFTP
- WebDAV

支持：

- 自动同步
- 定时同步
- 增量同步

---

# V0.9 Wireless CarPlay

预计目标：

Tesla 浏览器运行 CarPlay。

## 功能

- [ ] Wireless CarPlay
- [ ] 自动连接
- [ ] 音频转发
- [ ] 全屏模式

---

# V1.0 Tesla Resource Center

预计目标：

打造 Tesla USB 资源平台。

## 功能

在线下载：

- 锁车提示音
- Light Show
- 车辆皮肤
- USB 资源

支持：

- 一键安装
- 一键恢复

---

# V1.1 插件系统

预计目标：

开放扩展能力。

插件可以增加：

- 新页面
- 新 API
- 新设备支持

---

# V2.0 AI

长期规划。

可能包括：

- AI 视频搜索
- AI 事件识别
- AI 停车分析
- AI 行车统计

---

# 当前优先级

⭐⭐⭐⭐⭐

- USB 自动识别
- 自动挂载
- TeslaCam 扫描
- 视频 API

⭐⭐⭐⭐

- Web UI
- 在线播放
- 视频缩略图

⭐⭐⭐

- AP Router
- Tailscale
- OTA

⭐⭐

- NAS
- Resource Center

⭐

- AI

---

# 开发理念

TeslaBox 的目标不是复刻 TeslaUSB。

TeslaUSB 专注于 **USB 存储模拟**。

TeslaBox 更希望成为：

> 一个运行在 Tesla 车内的开源边缘计算平台。

未来不仅服务于 TeslaCam，还可以持续扩展更多与 Tesla USB 生态相关的能力。