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
    TeslaCam      CarPlay      OTA
        │
     Storage
```

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
│ USB │ OTA │ CarPlay │ NAS    │
├──────────────────────────────┤
│ Mount │ TeslaCam │ Network   │
├──────────────────────────────┤
│ udev │ systemd │ Linux BSP   │
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
│   ├── mount/
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

插入 Tesla USB

↓

udev 事件

↓

USB 管理器

↓

文件系统检查

↓

自动挂载

↓

TeslaCam 检测

↓

媒体扫描

↓

缩略图

↓

元数据

↓

REST API

↓

Web UI

---

# 视频播放

Tesla USB

↓

原始 MP4

↓

HTTP Range

↓

浏览器

远程模式：

Tesla USB

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