# TeslaBox AP 自动化控制使用教程

本文介绍如何在 TeslaBox 板内脚本中安全地临时开启和关闭 AP 热点，适用于 USB GPS、GPIO、蓝牙信标或其他本地传感器自动化场景。

本功能从 TeslaBox `0.1.31` 开始提供。`teslaboxctl` 是正式入口，`/usr/local/bin/teslabox automation` 仅保留为兼容和诊断入口。

## 一、工作原理

自动化脚本不会直接执行 `nmcli`，也不需要保存 TeslaBox 管理页面密码。脚本通过板内 Unix Socket 向 TeslaBox 主服务申请 AP 租约：

```text
GPS 或其他自动化脚本
        ↓
teslaboxctl
        ↓
/run/teslabox/automation/control.sock
        ↓
TeslaBox 主服务
        ↓
NetworkManager
```

自动化接口不会通过热点、局域网或 FRP 暴露。

安全边界：

- 接口只监听 `/run/teslabox/automation/control.sock`，不会监听 TCP；
- Socket 正常情况下属于 `root:teslabox-automation`，权限为 `0660`；如果系统中没有该用户组，则降级为 `root:root`、`0600`；
- 接口只接受预定义的 AP 状态、配置和租约操作，不接受命令、程序路径或 Shell 参数；
- 频段和信道只接受固定枚举及整数，并复用 TeslaBox 的硬件能力、监管域和 AP+STA 并发校验；
- 自动化租约不会改变 AP 的开机自动连接策略，也不会关闭用户在设置中心手动开启的热点。

## 二、授权脚本账号

TeslaBox 使用 `teslabox-automation` 用户组限制接口访问。假设 GPS 脚本使用 `rock` 账号运行，执行：

```sh
sudo usermod -aG teslabox-automation rock
```

重新登录账号或重启对应的 systemd 服务后，组权限才会生效。

检查账号权限：

```sh
id rock
```

输出中应包含：

```text
teslabox-automation
```

## 三、查看 AP 状态

先确认客户端与主程序版本：

```sh
teslaboxctl -v
```

输出示例：

```text
TeslaBox 0.1.31 (commit abc123def456)
```

只有版本号为 `0.1.31` 或更高版本时才支持本文的 `teslaboxctl` AP 自动化功能。低于 `0.1.31`、显示 `dev`，或者系统提示找不到 `teslaboxctl` 时，不应假设该接口可用；请先升级到正式的 `0.1.31` 或更高版本。

开发测试包可能显示类似下面的版本号，其中 `0.1.31-dev-...` 只用于测试，不等同于正式版 `0.1.31`：

```text
TeslaBox 0.1.31-dev-ap-automation-r7 (commit 4f0e9fb9acbb)
```

`teslaboxctl` 是指向 `/usr/local/bin/teslabox` 的符号链接，因此两者始终使用同一个版本。TeslaBox 启动时只会在路径不存在时创建链接，不会覆盖已有普通文件或指向其他程序的链接。

查看 AP 状态：

```sh
teslaboxctl ap status
```

开发阶段兼容命令：

```sh
/usr/local/bin/teslabox automation ap status
```

返回字段说明：

| 字段 | 含义 |
| --- | --- |
| `available` | 当前是否检测到可用的 AP 硬件和配置 |
| `enabled` | AP 当前是否实际运行 |
| `persistent_enabled` | AP 配置的开机自动连接策略，不代表 AP 此刻是否正在运行 |
| `active_leases` | 当前有效的自动化租约数量 |
| `state` | AP 当前运行状态 |

## 四、配置 AP 频段和信道

在自动化脚本启用热点前，可以查看或设置热点的持久频段和信道：

```sh
teslaboxctl ap config
teslaboxctl ap config set --band 2.4g --channel 6
teslaboxctl ap config set --band 5g --channel 149
teslaboxctl ap config set --band 5g --channel auto
```

配置输出会同时显示首选配置、当前实际频段和信道、并发模式及允许信道。配置修改不会改变开机自动连接策略，也不会创建或释放租约。

无线能力约束：

- `exclusive` 表示无线设备不能同时运行 STA 和 AP；连接上游 WiFi 时，开启或重配热点会被拒绝；
- `same_channel` 表示 STA 与 AP 必须使用同一频段、同一信道；热点运行时只能选择上游当前信道或 `auto`；
- `multi_channel` 表示驱动报告允许 AP 与 STA 使用不同信道；
- `independent_radio` 表示热点使用独立无线设备；
- `unknown` 会按同信道安全模式处理；
- DFS、`no IR`、禁用或硬件未报告的信道会被拒绝，不会强行写入 NetworkManager。

修改正在运行的热点会使已连接客户端短暂断开并重新连接，但现有自动化租约不会丢失。

## 五、开启并续期 AP

到达目标区域后，为 `gps-geofence` 申请一个 5 分钟的 AP 租约：

```sh
teslaboxctl ap enable --source gps-geofence --ttl 5m
```

开发阶段兼容命令：

```sh
/usr/local/bin/teslabox automation ap enable \
  --source gps-geofence \
  --ttl 5m
```

在租约到期前使用相同的 `source` 重复执行，就是续期。续期不会反复重启已经运行的 AP。

续期和释放必须使用与首次申请完全相同的 `source`。

租约有效期限制：

- 最短 15 秒；
- 最长 1 小时；
- 未指定时默认 5 分钟。

有效示例：

```sh
teslaboxctl ap enable --source gps-geofence --ttl 30s
teslaboxctl ap enable --source gps-geofence --ttl 2m
teslaboxctl ap enable --source gps-geofence --ttl 5m
teslaboxctl ap enable --source gps-geofence --ttl 1h
```

`source` 用于标识调用方，必须保持稳定且唯一，只能包含字母、数字、点、下划线和连字符，最长 64 个字符。

## 六、释放 AP 租约

离开目标区域后，释放 `gps-geofence` 自己申请的租约：

```sh
teslaboxctl ap disable --source gps-geofence
```

开发阶段兼容命令：

```sh
/usr/local/bin/teslabox automation ap disable \
  --source gps-geofence
```

释放租约时：

- 不会撤销其他脚本申请的租约；
- 不会关闭用户在设置中心手动开启的 AP；
- 多个租约并存时，要等最后一个租约被释放或到期后，临时 AP 才会关闭；
- 释放最后一个临时租约时，命令会等待 AP 实际关闭后再返回；
- 脚本崩溃、GPS 被拔出或停止续期时，租约到期后会自动失效。

## 七、GPS 脚本推荐逻辑

以下为逻辑示例，`gps_is_inside_target_area` 需要替换为实际的 GPS 判断函数：

```sh
#!/bin/sh
set -eu

SOURCE="gps-geofence"

if gps_is_inside_target_area; then
    # 每次检测到位于目标区域时续期，不需要判断 AP 当前是否已经开启。
    teslaboxctl ap enable --source "${SOURCE}" --ttl 5m
else
    teslaboxctl ap disable --source "${SOURCE}"
fi
```

建议：

- 每 1 至 2 分钟检测并续期一次 5 分钟租约；
- 设置进入和离开两个不同距离，避免 GPS 漂移导致 AP 频繁开关；
- 例如进入目标点 100 米内开启，离开目标点 150 米外关闭；
- 定位无效时不要申请新租约，让旧租约自然过期。

## 八、systemd 服务示例

创建 `/etc/systemd/system/gps-ap-controller.service`：

```ini
[Unit]
Description=TeslaBox GPS AP automation
After=teslabox.service
Requires=teslabox.service

[Service]
Type=simple
User=rock
SupplementaryGroups=teslabox-automation
ExecStart=/home/rock/gps-ap-controller.sh
Restart=on-failure
RestartSec=10

[Install]
WantedBy=multi-user.target
```

加载并启动：

```sh
sudo systemctl daemon-reload
sudo systemctl enable --now gps-ap-controller.service
```

查看运行状态：

```sh
systemctl status gps-ap-controller.service --no-pager
```

查看最近日志：

```sh
journalctl -u gps-ap-controller.service -n 100 --no-pager
```

## 九、直接调用 Unix Socket

通常建议使用 `teslaboxctl`。如果程序自带支持 Unix Socket 的 HTTP 客户端，也可以直接调用：

```sh
curl --unix-socket /run/teslabox/automation/control.sock \
  -H 'Content-Type: application/json' \
  -X PUT \
  -d '{"ttl_seconds":300}' \
  http://localhost/v1/network/ap/leases/gps-geofence
```

当前接口：

| 方法 | 路径 | 作用 |
| --- | --- | --- |
| `GET` | `/v1/capabilities` | 查询允许使用的自动化能力 |
| `GET` | `/v1/network/ap/state` | 查询 AP 实际状态和租约数量 |
| `GET` | `/v1/network/ap/config` | 查询频段、信道和并发能力 |
| `PUT` | `/v1/network/ap/config` | 设置持久频段和信道 |
| `PUT` | `/v1/network/ap/leases/{source}` | 申请或续期 AP 租约 |
| `DELETE` | `/v1/network/ap/leases/{source}` | 释放指定来源的 AP 租约 |

## 十、故障排查

### 1. 提示 Permission denied

检查当前账号是否属于自动化权限组：

```sh
id
getent group teslabox-automation
```

检查 Socket 权限：

```sh
stat /run/teslabox/automation/control.sock
```

正常情况下应属于：

```text
root:teslabox-automation
```

### 2. 提示 No such file or directory

检查 TeslaBox 主服务：

```sh
systemctl status teslabox.service --no-pager
```

查看服务日志：

```sh
journalctl -u teslabox.service -n 100 --no-pager
```

### 3. 命令成功但 AP 没有广播

查看 AP 状态：

```sh
teslaboxctl ap status
```

再检查 TeslaBox 服务日志中是否存在无线网卡、监管信道、AP 接口冲突或 NetworkManager 错误。

## 十一、安全注意事项

- 不要在用户脚本中直接执行 `nmcli` 或修改 NetworkManager 配置；
- 不要把 TeslaBox 管理页面密码写入脚本；
- 不要把 Unix Socket 代理到 TCP、局域网或公网；
- 不要以 root 身份运行 GPS 解析脚本，使用独立低权限账号和 `teslabox-automation` 附加组；
- 每个自动化程序使用不同的 `source`，不要互相覆盖租约。
- 后续扩展应继续使用独立资源、固定参数和最小权限，禁止增加任意命令执行接口。
