# TeslaBox 第一次安装教程

本文用于一块尚未部署 TeslaBox 的 Radxa ROCK 3A。第一次安装完成后，日常 TeslaBox 主程序升级应直接使用设置中心的签名在线升级，不再重复运行首装脚本。

## 适用范围

首装包当前只支持：

- Radxa ROCK 3A（设备树型号 `Radxa ROCK3 Model A`）
- ARM64
- Radxa 官方 Debian 11
- 有线网络可以访问 Gitee 和 Debian 软件源
- 独立 Type-C 供电

安装时必须断开连接车机或电脑的 OTG 线，不要在 Tesla 正在写入录像时安装。建议先不插外接数据盘；首装脚本不会格式化磁盘，数据盘应在管理页面中单独确认并初始化。

Debian 12、Ubuntu、其他 RK356x 板型和非 ARM64 系统不会绕过检查强行安装。Rockchip FFmpeg 包以 Debian 11 为运行基线，不能直接假设兼容其他发行版。

## 下载并运行

登录 ROCK 3A 后执行：

```sh
curl -fL https://gitee.com/TecheN1/teslabox-releases/raw/master/stable/install.sh -o teslabox-install.sh
chmod +x teslabox-install.sh
sudo ./teslabox-install.sh
```

不要使用未经检查的第三方镜像地址，也不建议直接执行 `curl | sudo sh`。分开下载和运行可以先用 `less teslabox-install.sh` 审查脚本内容。

脚本可以安全重跑。已有 `/etc/teslabox/teslabox.env`、Web 认证状态和存储登记册会保留；主程序覆盖前会创建带时间戳的备份。脚本不会删除录像、初始化外接盘或自动重启整块板子。

## 自动安装内容

基础流程会完成：

1. 核对 ROCK 3A、Debian 11 和 ARM64。
2. 从 Gitee Release 下载首装包并校验归档及包内每个文件的 SHA-256。
3. 安装 XFS、exFAT/FUSE、FAT32 格式化工具 `dosfstools`、分区、NetworkManager、mDNS 和 USB 管理依赖。
4. 安装 `teslabox`、`teslabox-init`、`teslabox-image-init` 和 `teslabox-gadget`。
5. 安装 Rockchip 专用 FFmpeg、MPP 和 RGA 到 `/opt/teslabox/media`，不覆盖系统自带的 `ffmpeg`。
6. 安装 `teslabox.service`、按需启动的 `teslabox-update.service`、udev 规则和 AP 恢复组件。
7. 配置 Gitee 稳定通道和 Ed25519 公钥，后续只接受签名匹配的 ARM64 主程序。
8. 停用并屏蔽 Radxa 原厂 `radxa-adbd.service`、`radxa-usbnet.service`，避免它们延迟启动后争用 DWC3 的 EP0 控制端点。
9. 禁用已经退役的常驻 `teslabox-gadget.service`，由 Go 主服务单独管理 UDC。
10. 启动服务并等待 `/healthz` 通过；失败时恢复安装前的主程序。

如果安装时 NetworkManager 已识别到无线客户端接口，脚本会自动发现它，不要求名称必须是 `wlan0`，随后创建 `TeslaBox-5G` AP 配置并尝试启用热点。上游 WiFi 已连接时，热点会先同步到同一信道；没有上游时优先选择网卡和当前监管域允许的 5GHz 非 DFS 信道，必要时回退到 2.4GHz。初始热点密码随机生成，只在安装结束时显示；请记录并在设置中心修改。如果没有识别到无线接口，或网卡/驱动不支持 AP，脚本保持有线管理可用，不会让 AP 失败阻断 TeslaUSB 基础安装。

`radxa-usbnet.service` 只会让 OTG Device 口模拟 `ecm.usb0` USB 有线网卡，不负责 USB Host 口上的无线网卡，也不管理 NetworkManager 或 MT7922。TeslaBox 屏蔽它不会影响上游 WiFi 或 AP 热点。

无线接口默认自动识别，热点接口优先读取 `TeslaBox-5G` 连接配置，未配置时才回退到 `ap0`。只有自动识别错误或接口名冲突时，才需要在 `/etc/teslabox/teslabox.env` 中设置 `TESLABOX_WIFI_INTERFACE` 或 `TESLABOX_AP_INTERFACE`；Linux 接口名最长 15 个字符。

## MT7922 是可选测试内核

基础安装结束后，脚本会显示醒目警告，并要求完整输入：

```text
INSTALL MT7922
```

直接回车或输入其他内容都会跳过。默认跳过不会影响有线管理、TeslaUSB、录像浏览和后续在线升级。

选择安装会加入以下两个包：

- `linux-image-5.10.160teslabox1`
- `linux-mt7922-backports-5.10.160teslabox1`

它们提供基于 Radxa 5.10 BSP 的 `5.10.160teslabox1` 测试内核、完整 `cfg80211/mac80211/mt76/mt7921e` backports 栈和 MT7922 固件。该内核已经在本项目的 ROCK 3A + MT7922A（PCI ID `14c3:0616`）上验证，但不由 Radxa 官方维护。

已有无线网卡在原厂内核下正常工作时没有必要安装。安装完成后脚本不会自动重启；请先确认车机 OTG 已断开，再手工执行：

```sh
sudo reboot
```

原厂 `5.10.160-12-rk356x` 包不会被卸载。若测试内核无法启动，应通过串口或启动介质回退原厂内核，因此不具备恢复手段的用户不应选择此项。

## 第一次登录与存储初始化

安装结束后，使用脚本显示的局域网地址打开管理页面。也可以查询：

```sh
hostname -I
```

全新安装的管理密码为：

```text
teslabox
```

第一次登录必须修改默认密码。完成改密后进入“设置中心 → 外接存储”，插入准备用作 TeslaBox 数据盘的 USB U 盘、USB SSD 或 USB 硬盘，再由页面扫描并初始化。

存储初始化会擦除选中的整块盘。页面会再次核对 USB by-id、序列号、容量、挂载和 swap 状态，并要求先物理拔掉车机/电脑 OTG 线、输入序列号和确认擦除。TF 卡只运行 Debian 和 TeslaBox；外接数据盘保存 TeslaCam 虚拟磁盘、快照、导出和媒体索引。

初始化页面会尽可能利用外接盘容量，同时为快照和运行数据预留 5% 且不少于 20GB。若希望车机同时读取本地音乐，可以勾选“创建 MUSIC 音乐分区（FAT32）”，再调整 TeslaCam 与音乐容量比例；不需要音乐时保持未勾选即可。已初始化盘也可以通过“重新初始化”更改布局，但该操作同样会删除全部录像、音乐和导出文件。

初始化完成后先插好 OTG，再到“日志中心”执行“温重启恢复”。系统会保存恢复标记并自动重启；若目标主机仍不能识别，关闭设备并完全断电，保持 OTG 预插后重新上电。

不要手工把 `/dev/sdX` 写入配置，也不要把普通 exFAT U 盘直接当作 TeslaBox backing store。最终数据盘需要 XFS reflink，TeslaCam 则位于数据盘内的 MBR/exFAT 虚拟磁盘镜像中。

## 安装后验证

```sh
systemctl is-active teslabox.service
systemctl is-enabled teslabox.service
systemctl is-enabled teslabox-update.service
systemctl is-enabled radxa-adbd.service
systemctl is-enabled radxa-usbnet.service
curl -fsS http://127.0.0.1/healthz
/opt/teslabox/media/bin/ffmpeg -hide_banner -hwaccels
/opt/teslabox/media/bin/ffmpeg -hide_banner -decoders | sed -n '/_rkmpp/p'
/opt/teslabox/media/bin/ffmpeg -hide_banner -encoders | sed -n '/_rkmpp/p'
/opt/teslabox/media/bin/ffmpeg -hide_banner -filters | sed -n '/ass\|subtitles\|_rkrga/p'
```

预期结果：

- `teslabox.service` 为 `active`、`enabled`。
- `teslabox-update.service` 为 `static`；它只在页面确认安装时临时运行。
- 两个 Radxa 原厂 Gadget 服务为 `masked`，不能在 TeslaBox 之后异步接管同一个 UDC。
- 健康接口返回 `{"status":"ok"}`。
- FFmpeg 能列出 H.264/HEVC RKMPP 编解码器、RGA 和 ASS/subtitles 滤镜。

## 后续升级

进入“设置中心 → 系统更新”执行检查、下载和安装。设备会校验 Ed25519 签名、板型、架构、文件大小和 SHA-256；在线更新默认会自动安全断开 USB Gadget，并在安装健康检查通过后重启系统恢复。安装健康检查失败时自动恢复旧主程序。

正式在线包仍是单个经过签名校验的 ARM64 ELF。需要媒体能力升级的版本会在 ELF 内携带固定媒体包，新程序启动时在暂存目录验证 H.264/HEVC RKMPP、RGA、ASS/subtitles 和字体后原子更新 `/opt/teslabox/media`。内核、MT7922 驱动、systemd unit 和首装脚本不会通过网页更新；MT7922 Wi-Fi 与蓝牙驱动只在首装脚本中经过两次明确确认后安装。

## 常见问题

### 80 端口被占用

首装脚本会在覆盖服务前停止并提示。检查占用者：

```sh
sudo ss -ltnp '( sport = :80 )'
```

### 没有热点

先保留网线并检查：

```sh
nmcli -t -f DEVICE,TYPE,STATE device status
systemctl status teslabox-ap.service --no-pager
nmcli connection show TeslaBox-5G
```

如果输出中没有 `wifi` 类型设备，通常表示当前内核没有对应无线驱动，或无线硬件未枚举。不要仅因热点未出现就盲目安装 MT7922 测试内核；先用 `lspci -nn` 或 `lsusb` 确认实际无线硬件。

### 服务启动失败

```sh
sudo systemctl status teslabox.service --no-pager
sudo journalctl -u teslabox.service -n 100 --no-pager
```

首装脚本在健康检查失败时会恢复之前的主程序。配置、数据盘或端口问题仍需要根据日志处理。

### 创建 MUSIC 分区提示缺少 `mkfs.vfat`

这通常说明设备曾通过早期版本安装，缺少 FAT32 格式化工具。先安装依赖，再在“外接存储”刷新页面并优先选择“继续初始化”：

```sh
sudo apt-get update
sudo apt-get install -y dosfstools
command -v mkfs.vfat
```

新版本会在擦除数据盘前检查 `mkfs.vfat`；不要在缺少该命令时反复执行重新初始化。
