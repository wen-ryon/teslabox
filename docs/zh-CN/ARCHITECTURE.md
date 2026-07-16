# TeslaBox 系统架构

---

# 总体架构

```text
                Internet
                    │
     ┌──────────────┼──────────────┐
     │              │              │
 Local AP      Tailscale      Relay Server
     │              │              │
     └──────────────┼──────────────┘
                    │
             TeslaBox HTTP API
                    │
        ┌───────────┼───────────┐
        │           │           │
    TeslaCam    Future Modules  OTA
        │
     Storage
```

第一项核心功能的权威边界见 [TeslaUSB 模拟模式需求](TESLAUSB_REQUIREMENTS.md)。

---

# 已确认硬件架构

```text
Tesla 行车记录仪 USB 口
          │
          │ USB Mass Storage
          ▼
ROCK 3A USB OTG Device 口
          │
          │ configfs USB Gadget
          ▼
外接 128GB U 盘中的 TeslaCam 虚拟磁盘镜像

ROCK 3A TF 卡 ── Debian 和 TeslaBox 程序
```

TF 卡不保存 TeslaCam 镜像、录像快照和媒体索引。外接 U 盘通过 USB Host 口连接并作为固定数据盘；未来把 Debian 迁移到 NVMe 不改变此数据模型。

---

# 网络架构

TeslaBox 支持三种传输层。

## AP

Phone
    │
TeslaBox AP
    │
HTTP

---

## Tailscale

Phone
    │
100.x.x.x
    │
TeslaBox

无需公网 IP。

---

## Relay（中继）

TeslaBox
      │
持久化 WebSocket
      │
Relay Server
      │
Browser

无需开放入站端口。

---

# 软件分层

```text
┌──────────────────────────────┐
│          Web UI              │
├──────────────────────────────┤
│         REST API             │
├──────────────────────────────┤
│     TeslaBox Services        │
├──────────────────────────────┤
│ USB │ OTA │ Future  │ NAS    │
├──────────────────────────────┤
│ Gadget │ Snapshot │ TeslaCam │
├──────────────────────────────┤
│ configfs │ udev │ Linux BSP  │
└──────────────────────────────┘
```

---

# 目录结构

```text

teslabox/

├── cmd/
│   └── server/
│
├── internal/
│   ├── api/
│   ├── usb/
│   ├── gadget/
│   ├── mount/
│   ├── snapshot/
│   ├── teslacam/
│   ├── network/
│   ├── transport/
│   ├── media/
│   ├── ota/
│   ├── carplay/
│   ├── monitor/
│   └── backup/
│
├── web/
│
├── configs/
│
├── scripts/
│
└── docs/
```

---

# Tesla USB 工作流程

```text
发现并验证外接数据盘
          ↓
准备 TeslaCam 虚拟磁盘镜像
          ↓
通过 configfs 创建 Mass Storage Gadget
          ↓
绑定 UDC，Tesla 开始写入虚拟磁盘
          ↓
对仍在写入的镜像创建写时复制快照
          ↓
检查快照文件系统并只读挂载
          ↓
扫描完整 TeslaCam 片段并原子更新索引
          ↓
REST API / Web UI
```

外接数据盘第一次初始化是独立的显式操作，正常服务启动不得自动分区或格式化。

---

# 视频播放

```text
Tesla 持续写入原始镜像
          │
          └── 一致性快照 ──> 已完成 MP4 ──> HTTP Range ──> 浏览器
```

网页不得直接读取或挂载正在向 Tesla 暴露的可写镜像。当前仍在写入的 MP4 不保证播放；“边录边播放”表示录像不中断，同时播放快照中已经完成的片段。

远程模式：

只读快照

↓

硬件转码（MPP）

↓

低码率流

↓

浏览器

---

# 传输层

传输接口

↓

AP 传输

↓

Tailscale 传输

↓

中继传输

所有传输层暴露相同的 HTTP API。

---

# OTA 更新

云端

↓

检查更新

↓

下载

↓

校验

↓

应用

↓

回滚

---

# 监控

CPU

内存

温度

存储

网络

日志

---

# 未来规划

- BLE
- AI 视频搜索
- 人脸识别
- 云同步
- 插件系统
