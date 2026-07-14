# 功能特性

## Tesla USB

- 远程 TeslaCam 浏览器
- 在线 TeslaCam 回放
- 下载 TeslaCam 视频
- 多摄像头同步回放
- 视频缩略图
- 按日期/事件搜索
- 最近片段 / 已保存片段管理

未来 USB 功能：

- 锁车提示音管理器
- 灯光秀管理器
- 车辆皮肤管理器
- 在线资源商店

---

## 网络

### AP 路由器

TeslaBox 可作为 Wi-Fi 6 接入点工作。

- 共享 4G/5G 移动网络
- Tesla 浏览器上网
- 手机上网
- 本地 TeslaCam 回放

### 远程访问

支持三种访问模式：

- 本地 AP
- Tailscale（VPN）
- 公共中继服务器

无需公网 IP。

---

## 无线 CarPlay

运行在 Linux 上的无线 CarPlay 接收器。

Tesla 浏览器自动启动 CarPlay 网页界面。

---

## OTA 更新

- 一键 OTA 升级
- 回滚支持
- 增量升级
- 版本管理

---

## 监控

- CPU 温度
- 存储健康状态
- 内存使用情况
- 网络状态
- 设备日志

---

## 备份

自动备份到：

- SMB NAS
- NFS
- WebDAV
- SFTP

---

# 硬件

## 当前硬件

- Radxa ROCK 3A
- RK3568
- 2GB 内存
- microSD / NVMe
- 原装 Tesla USB 硬盘（128GB）

## 扩展

### Wi-Fi

MediaTek MT7922

- Wi-Fi 6E
- AP 模式
- Linux 友好

### 蜂窝网络

4G / 5G 模块（计划中）

- 移动数据
- 始终在线
- 远程访问

---

# 软件栈

Go

Linux

Systemd

udev

FFmpeg

Rockchip MPP

Tailscale

Hostapd

Dnsmasq

---

# 路线图

## 第一阶段

- [ ] Tesla USB 检测
- [ ] 自动挂载
- [ ] TeslaCam 扫描器
- [ ] 视频 API

## 第二阶段

- [ ] 网页界面
- [ ] 视频播放
- [ ] 缩略图
- [ ] 下载

## 第三阶段

- [ ] AP 路由器
- [ ] MT7922
- [ ] Tailscale
- [ ] OTA 更新

## 第四阶段

- [ ] 无线 CarPlay
- [ ] NAS 备份
- [ ] 资源中心

---

# 硬件架构图

```text
            Tesla
              │
              │
       Tesla USB Drive
              │
              ▼
        Radxa ROCK 3A
              │
   ┌──────────┴──────────┐
   │                     │
MT7922              4G/5G Module
   │                     │
AP WiFi           Mobile Internet
   │                     │
   └──────────┬──────────┘
              │
           TeslaBox
```

---

# 许可证

MIT