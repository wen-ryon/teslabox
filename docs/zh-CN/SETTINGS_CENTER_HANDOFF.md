# 设置中心第一版需求与交接说明

更新时间：2026-07-16

## 目标

在现有 TeslaBox 管理页面中增加可扩展的设置中心，第一版覆盖：

1. 上游 WiFi：查看已保存网络、查看最近一次扫描缓存、手动扫描、连接网络和忘记网络。
2. AP 热点：查看并修改热点启用状态、SSID、密码、频段、信道和开机自动启动。
3. 系统电源：经过二次确认后重启或关闭 ROCK 3A。
4. 导航：点击左上角 `T` 图标打开侧边导航，品牌文字返回设备概览。

## 板端现状

当前 ROCK 3A 使用 NetworkManager 管理网络，不使用独立的 hostapd/dnsmasq 服务：

| 项目 | 当前值 |
| --- | --- |
| 无线网卡 | MT7922，接口 `wlan0`，驱动 `mt7921e` |
| AP 连接配置 | `TeslaBox-5G` |
| AP 模式 | NetworkManager `802-11-wireless.mode=ap` |
| 频段/信道 | 5GHz / 149 |
| 加密 | WPA-PSK |
| AP 地址 | `192.168.88.1/24` |
| 共享方式 | NetworkManager `ipv4.method=shared` |
| 当前管理地址 | `192.0.2.25:8080`（以实际 DHCP 地址为准） |

已经存在 `TeslaBox-Upstream`、`WendeWiFi_5G` 等客户端连接配置。后端不会读取或返回任何已保存 WiFi 密码。

## 已实现

### 页面

- 左上角 `T` 图标打开侧边导航。
- “TeslaBox / 车载存储中心”品牌文字返回概览。
- 设置中心分为“上游 WiFi”“AP 热点”“系统与电源”三个区域。
- 桌面端和移动端响应式布局。
- 已保存网络展示 SSID、当前连接状态、自动连接状态、连接和忘记操作。
- 可用网络展示 `SSID / SIGNAL / BAND / SECURITY`。
- 相同 SSID 的多个 BSSID 只保留信号最强项。
- 手动扫描按钮；没有主动扫描时展示 NetworkManager 最近一次扫描缓存。
- 新网络通过密码弹窗连接；已保存网络无需再次输入密码。
- 不支持的企业认证或 WEP 网络会禁用连接按钮。
- AP 表单支持 SSID、新密码、频段、信道、启用状态和自动启动。
- AP 密码留空表示保留原密码，读取接口永远不会返回密码。
- 重启、关机、忘记网络、切换无线模式和应用 AP 配置都有二次确认。
- 关机页面明确提示必须重新断电上电才能启动。

### 后端

- 新增 `internal/deviceadmin`，集中封装 NetworkManager 和 systemd 操作。
- 外部命令全部通过 `exec.CommandContext` 的参数数组执行，不拼接 shell 命令。
- 外部命令有 20 秒超时，网络写操作由互斥锁串行执行。
- 所有设置写接口继续受现有登录会话和同源 POST 校验保护。
- 请求体限制为 16KiB，拒绝未知 JSON 字段。
- SSID、WPA 密码、UUID、频段和信道均在服务端校验。
- 电源命令在接口返回 `202 Accepted` 两秒后执行，保证浏览器先收到响应。
- API 不向浏览器返回 `nmcli`、systemd 或文件路径等内部错误细节。

## API

所有接口都位于 `/api/v1`，并要求有效的 Web 登录会话。

| 方法 | 路径 | 说明 |
| --- | --- | --- |
| GET | `/api/v1/network/wifi` | 当前无线状态、已保存网络和缓存的可用网络 |
| POST | `/api/v1/network/wifi/scan` | 手动扫描附近 WiFi |
| POST | `/api/v1/network/wifi/connect` | 连接已保存网络或新网络 |
| POST | `/api/v1/network/wifi/forget` | 删除已保存客户端连接 |
| GET | `/api/v1/network/ap` | 读取 AP 配置，不返回密码 |
| POST | `/api/v1/network/ap` | 校验、保存并应用 AP 配置 |
| POST | `/api/v1/system/power` | 提交 `reboot` 或 `shutdown` |

连接已保存网络：

```json
{"uuid":"77824373-55d5-4594-b2a2-e87020a9d2b5","confirm_ap_disconnect":true}
```

连接新网络：

```json
{"ssid":"ExampleWiFi","password":"example-password","security":"WPA2","confirm_ap_disconnect":true}
```

AP 更新：

```json
{"enabled":true,"ssid":"TeslaBox-5G","password":"","band":"a","channel":149,"auto_connect":true}
```

## 第一版安全限制

### 单无线网卡冲突

当前只有一个 `wlan0`，并正在提供 AP。第一版不假设 MT7922、当前 5.10 backports 驱动和 NetworkManager 能稳定实现 AP+STA 并发：

- AP 运行时，手动扫描按钮禁用。
- 后端收到 AP 运行时的扫描请求会返回 HTTP 409。
- 连接上游 WiFi 必须显式提交 `confirm_ap_disconnect=true`。
- 页面会警告连接客户端 WiFi 可能停止 AP 并立即断开管理页面。
- 后端不会为了扫描而静默停止 AP。

稳定实现热点和上游 WiFi 同时运行的建议方案是增加第二块无线网卡，或者让 4G/5G 设备通过 USB RNDIS/网卡模式提供上游网络。单 MT7922 的 AP+STA 并发只能作为后续实验功能，必须先用 `iw list` 验证 valid interface combinations，并完成长时间稳定性测试。

### 网络和电源断联

- 修改 AP 名称、密码、频段或信道会重启热点。
- 从 AP 切换到客户端 WiFi 后，设备地址由上游 DHCP 决定。
- 重启和关机会先停止 TeslaBox 服务并断开 USB Gadget。
- 目前只做二次确认，尚未阻止车机正在写入时执行电源操作。

## 待实现

按优先级排序：

1. 在电源操作前读取 Gadget `host_connected`，车机连接时增加更强的确认或默认禁止。
2. 增加网络切换任务状态和“新地址发现”，避免切换后用户不知道从哪里重新访问。
3. 引入第二无线网卡后，为 AP 和上游 WiFi 分配明确接口，不再默认固定 `wlan0`。
4. 实机验证开放网络、WPA2、错误密码、隐藏 SSID 和中文 SSID。
5. 增加 AP 已连接客户端列表、主机名、IP、MAC、信号和拉黑操作。
6. 增加默认路由来源、互联网共享状态和 4G/5G 上游切换。
7. 增加 AP 配置失败的事务回滚；当前 NetworkManager 修改成功但重新激活失败时，需要人工恢复配置。
8. 评估直接使用 NetworkManager D-Bus，减少对不同版本 `nmcli` 输出格式的依赖。
9. 增加设置操作审计日志，但日志禁止记录 WiFi 密码。
10. 增加 OTA、存储、管理密码和系统日志设置页面。

## 代码位置

| 路径 | 作用 |
| --- | --- |
| `internal/deviceadmin/manager.go` | NetworkManager、输入校验和电源操作 |
| `internal/deviceadmin/manager_test.go` | nmcli 解析、校验和电源调度测试 |
| `internal/api/handler.go` | 设置中心 REST API |
| `internal/api/deviceadmin_test.go` | 设置接口契约测试 |
| `internal/webui/assets/index.html` | 侧边导航、设置页面和确认弹窗 |
| `internal/webui/assets/app.js` | 设置页面交互和 API 调用 |
| `internal/webui/assets/app.css` | 设置页面响应式样式 |
| `cmd/teslabox/main.go` | 注入设备管理器 |

## 后续接手建议

1. 先执行 `go test ./...`、`go vet ./...` 和 JavaScript 语法检查。
2. 在板端只调用 GET 接口确认 nmcli 字段兼容性。
3. 测试扫描时保留有线连接，不要只通过 TeslaBox AP 管理设备。
4. AP 写入测试先记录现有配置，并保留 `/usr/local/bin/teslabox` 旧版本。
5. 电源接口只验证参数拒绝和 `202` 响应；没有现场人员时不要实际触发关机。
6. 所有新增日志和接口响应继续禁止输出密码、会话 Cookie 和 NetworkManager secret。

## 本次部署与验证

设置中心第一版已部署到 `192.0.2.25`，部署时设备仍通过有线网络管理：

| 项目 | 结果 |
| --- | --- |
| 服务状态 | `teslabox.service` 为 `active` |
| 设置页 | `/settings` 返回 HTTP 200 |
| 页面资源 | `app.js?v=20260716-9` |
| 构建 SHA256 | `178290cc3daa028e4da7a484de266a6609a7fe24cf56ee3b1f339d914853a454` |
| 板端备份 | `/usr/local/bin/teslabox.before-settings-v1` |
| 已保存客户端网络 | 实机读取到 2 个 |
| AP 读取 | `TeslaBox-5G`、5GHz、149、共享开启 |
| 密码泄露检查 | AP JSON 不包含 password/PSK |
| 单网卡保护 | AP 运行时扫描返回 HTTP 409 |
| 电源参数保护 | 非法 action 返回 HTTP 400 |

本次没有执行真实 WiFi 切换、AP 写入、重启或关机，避免无人值守时设备断联或关闭。这些破坏连接的操作仍需现场手工验收。

## 手工验收清单

- [ ] 点击 `T` 图标可以打开/关闭侧边导航。
- [ ] 概览和设置中心切换后，浏览器前进/后退正常。
- [ ] 已保存网络不包含 AP 配置，不显示密码。
- [ ] AP 运行时扫描按钮禁用且有原因说明。
- [ ] 可用网络表格正确展示 SSID、信号、频段和加密方式。
- [ ] 不支持的加密类型不能连接。
- [ ] AP GET 接口不包含 PSK。
- [ ] AP 密码留空不会清空原密码。
- [ ] 所有危险操作都有二次确认。
- [ ] 手机窄屏下表单、表格和按钮可用。
- [ ] 服务重启后概览、录像播放和设置中心均正常。
