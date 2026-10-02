# TeslaBox 本机 WiFi 自动化命令

本功能从 `0.1.31-rc.4` 内测版开始提供，并包含在 `0.1.31` 正式版中。安装后先运行 `teslaboxctl -v` 确认版本，再使用下列命令。

`teslaboxctl` 通过板内 `/run/teslabox/automation/control.sock` 调用 TeslaBox 服务，不会把这些接口暴露到局域网或 FRP。仅 root 和 `teslabox-automation` 用户组可访问；该组成员也将获得 WiFi 扫描、连接及修改自动连接策略的权限，应只加入可信脚本账号。

## 查询与扫描

```sh
teslaboxctl wifi list
teslaboxctl wifi scan
```

`list` 返回当前连接、已保存网络和缓存的可见网络，不主动发起扫描，也不返回密码。`scan` 主动让 NetworkManager 扫描，返回当前发现的接入点。无无线网卡、热点占用唯一无线接口等情况会明确报错，不会强行断开热点。

## 连接 WiFi

优先使用 `list` 中已保存网络的 `uuid`，避免同名 SSID 混淆：

```sh
teslaboxctl wifi connect --uuid 550e8400-e29b-41d4-a716-446655440000
```

首次连接开放网络：

```sh
teslaboxctl wifi connect --ssid Guest --security open
```

首次连接 WPA/WPA2 网络时，密码只能从标准输入读取，不能放在命令行参数里：

```sh
read -r -s WIFI_PASSWORD
printf '%s\n' "$WIFI_PASSWORD" | teslaboxctl wifi connect --ssid Home --security wpa --password-stdin
unset WIFI_PASSWORD
```

如已保存的配置缺少系统可读取的密码，也可在 `--uuid` 连接时增加 `--password-stdin`。密码不在 `teslaboxctl` 命令行参数和正常输出中出现；不要使用会记录输入的 `set -x`。当前底层 NetworkManager 连接逻辑沿用管理页面实现，部署脚本的账号应视为具有网络配置权限。

如果硬件不支持 AP+STA 并发，连接请求默认拒绝断开正在运行的热点。只有确认管理连接有其他路径时，才可显式增加 `--confirm-ap-disconnect`；这可能使通过热点连接的客户端掉线。

## 自动连接开关

只针对**已保存**的客户端 WiFi 配置。先从 `wifi list` 获取 UUID，再执行：

```sh
teslaboxctl wifi autoconnect enable --uuid 550e8400-e29b-41d4-a716-446655440000
teslaboxctl wifi autoconnect disable --uuid 550e8400-e29b-41d4-a716-446655440000
```

开关只改变该配置以后是否自动连接，不会立即断开或连接当前 WiFi，也不会修改 TeslaBox 热点的自动启动策略。SSID 可能重名，因此不接受仅凭 SSID 修改自动连接。

## 实机验证建议

先确认有线 SSH 可用，记录当前 WiFi UUID 与自动连接状态；再测试扫描、列表及已保存网络连接。最后切换自动连接开关并用 `wifi list` 核对，测试结束恢复原值。不要在仅通过同一 WiFi 管理板子时试验连接切换。
