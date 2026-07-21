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
- Wireless CarPlay（已暂缓，不纳入当前开发范围）
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

# V0.1 TeslaUSB 核心闭环

预计目标：

ROCK 3A 使用外接 128GB U 盘作为 backing store，通过 OTG 向 Tesla 模拟 USB Mass Storage，并在车辆持续录像时从快照读取已完成片段。

硬件、存储和一致性边界见 [TeslaUSB 模拟模式需求](TESLAUSB_REQUIREMENTS.md)。

## 功能

- [x] Go 项目初始化
- [x] 配置管理
- [x] 日志模块
- [x] HTTP Server
- [x] REST API
- [x] TeslaCam 文件解析和视频列表 API 原型
- [x] 通过 UUID 或卷标稳定发现外接数据盘
- [x] backing store、configfs 和 UDC 只读预检
- [x] 启动、等待数据盘、准备 backing store、就绪、降级和故障状态模型
- [x] 数据盘、backing store 和 Gadget 环境状态 API
- [x] 独立、显式且防误操作的数据盘初始化工具
- [x] 支持 reflink 的 backing store
- [x] 创建并格式化 TeslaCam 虚拟磁盘镜像
- [x] configfs USB Mass Storage Gadget
- [x] Gadget 绑定、解绑和重启恢复
- [x] Windows 主机枚举、exFAT 读写和冷启动恢复验收
- [x] 缺盘启动、数据盘热插入自动挂载和 Gadget 自动恢复
- [x] Gadget 活动期间数据盘掉线检测、LUN 停止和故障告警
- [x] 写时复制快照、快照文件系统检查和只读挂载（板端实际镜像已验收，Tesla 持续写入并行验收待进行）
- [x] 基于快照的媒体索引原子切换
- [x] HTTP Range、单路 MP4 播放和下载
- [x] Gadget 运行状态 API
- [x] 快照和索引状态 API
- [x] 空间不足时优先保留车端录像和最后一个可用快照，并对连续失败执行指数退避
- [x] 异常断电遗留挂载、loop 设备、快照镜像和临时文件的安全启动回收
- [x] 边录边放只读验收工具（状态、主机连接、快照轮换、视频增长和 HTTP Range）
- [x] TF 系统盘数据盘身份登记册和旧 UUID 平滑迁移
- [x] 设置中心 USB 存储扫描、双重确认初始化、中断继续和取消
- [x] 数据盘拔出后 Gadget、快照、挂载和 loop 状态回收，重新插入后重新校验恢复

完成后：

Tesla 持续录像且 Gadget 不断开时，用户可以通过 API 播放快照中已经写完的 TeslaCam 片段。

---

# V0.2 Web 管理后台

预计目标：

完成基础 Web UI。

## 功能

- [x] 登录页面
- [x] 全新安装默认密码、首次登录强制改密和设置中心修改密码
- [x] 首页
- [x] TeslaCam 浏览
- [x] 视频详情和单路播放
- [x] 下载视频
- [ ] 视频缩略图
- [x] 按类型、日期和事件筛选
- [x] 系统状态页面
- [x] 首页展示互联网连通性、设备温度、CPU、内存、系统盘和数据盘状态
- [x] 纯黑背景和蓝、绿、红强调色切换
- [x] 六路摄像头按录像时刻分组（允许 2 秒时间偏差）
- [x] 连续分钟文件合并为完整录像会话
- [x] 按月、滚动七日周期和日组织纵向录像时间线
- [x] 单窗口六路画面合成、单时间轴和单视角切换
- [x] 解析 `event.json` 并展示事件时间点、地点、原因和时间轴标记
- [x] 录像列表仅在六路视角不完整时警告，并将事件地点展示在录像时长旁
- [x] 外接存储状态、未初始化提示和安全初始化入口

完成后：

可以通过浏览器查看 TeslaCam。

---

# V0.3 高级播放

预计目标：

完善多摄像头同步播放体验。

## 功能

- [x] 单窗口六路同步合成和跨分钟续播
- [x] 六路、四路 A、四路 B、三路和单视角布局切换
- [x] 多路自动画质默认请求一路板端标准合成视频；显式选择原画时只请求布局实际展示的摄像头视频
- [x] 时间轴同步
- [ ] 快进
- [x] 拖动播放
- [x] 全屏
- [x] 浏览器侧链路、媒体吞吐、缓冲和卡顿观测
- [ ] 流畅、标准、高清多码率转码和自动切档
- [x] 自定义时长、播放器同款视角布局和画质的异步导出第一版；多路最高 1080p，单路支持源分辨率原画
- [x] 多路导出复用播放器动态布局宽高比，消除固定 16:9 画布造成的大面积黑边
- [ ] WebRTC P2P 和 TURN 兜底

后续：

详细实施顺序和验收条件见 [远程播放、视频导出与云端访问开发流程](REMOTE_PLAYBACK_EXPORT_PLAN.md)。

---

# V0.4 AP Router

预计目标：

TeslaBox 成为一台路由器。

## 功能

- [x] MT7922 和 backports 驱动
- [x] NetworkManager AP 模式
- [x] NetworkManager DHCP/NAT（`ipv4.method=shared`）
- [x] Web 设置中心第一版
- [x] 已保存网络和缓存扫描结果
- [x] 手动扫描、连接和忘记网络
- [x] AP 的 SSID、密码、频段、信道和自动启动配置
- [x] 带二次确认的重启和关机
- [x] 单射频 AP+STA 同信道并发、BSSID 锁定和热点信道同步
- [x] 上游掉线、BSSID 或信道变化后的周期监听、重新扫描和自动恢复
- [ ] 客户端管理

第一版实现、接口、安全限制和后续交接见 [设置中心第一版需求与交接说明](SETTINGS_CENTER_HANDOFF.md)。当前实际部署使用 NetworkManager，不再把独立 hostapd/dnsmasq 作为既定实现。

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

当前状态：第一版签名主程序更新已提前实现，完整设计和发布流程见 [签名在线更新说明](ONLINE_UPDATE.md)。

第一版只允许用户确认后更新主程序，不远程执行脚本，也不更新内核、驱动和媒体工具链。

## 功能

- [x] 检查更新
- [x] 后台下载和进度展示
- [x] Ed25519 清单签名、长度、SHA-256 和 ARM64 ELF 校验
- [x] 用户确认后安装并重启
- [x] 健康检查失败自动回滚
- [ ] 无人值守自动安装（第一版明确不启用）
- [ ] 内核、驱动和媒体工具链的独立升级方案

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

# V0.9 Wireless CarPlay（暂缓）

由于 MFi 授权、认证硬件和闭源 Receiver SDK 会显著扩大当前项目边界，本阶段不实施；未来获得合法、可量产的 MFi 方案后再单独立项。

原预计目标：

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

- 外接数据盘安全初始化
- TeslaCam backing image
- USB Mass Storage Gadget
- 一致性快照
- 车辆持续录像时播放已完成片段

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

TeslaBox 不会逐行复刻 TeslaUSB 的脚本，但会把可靠的 **USB 存储模拟、快照和异常恢复** 作为第一项核心能力，并使用 Go 重构业务状态机和管理接口。

TeslaBox 更希望成为：

> 一个运行在 Tesla 车内的开源边缘计算平台。

未来不仅服务于 TeslaCam，还可以持续扩展更多与 Tesla USB 生态相关的能力。
