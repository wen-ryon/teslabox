# 设置中心第一版需求与交接说明

更新时间：2026-07-17

## 目标

在现有 TeslaBox 管理页面中增加可扩展的设置中心，第一版覆盖：

1. 上游 WiFi：查看已保存网络、查看最近一次扫描缓存、手动扫描、连接网络和忘记网络。
2. AP 热点：查看并修改热点启用状态、SSID、密码、频段、信道和开机自动启动。
3. 外接存储：识别当前活动数据盘和未初始化 USB 整盘，提供受保护的初始化、继续和取消入口。
4. 系统电源：经过二次确认后重启或关闭 ROCK 3A。
5. 导航：点击左上角 `T` 图标打开侧边导航，品牌文字返回设备概览。

## 板端现状

当前 ROCK 3A 使用 NetworkManager 管理网络，不使用独立的 hostapd/dnsmasq 服务：

| 项目 | 当前值 |
| --- | --- |
| 无线网卡 | MT7922，STA 接口 `wlan0`，AP 虚拟接口 `ap0`，驱动 `mt7921e` |
| AP 连接配置 | `TeslaBox-5G` |
| AP 模式 | NetworkManager `802-11-wireless.mode=ap` |
| 频段/信道 | 5GHz / 161；连接上游时自动同步为目标信道 |
| 加密 | WPA-PSK |
| AP 地址 | `192.168.88.1/24` |
| 共享方式 | NetworkManager `ipv4.method=shared` |
| 当前管理地址 | `192.0.2.38:8080`（有线，以实际 DHCP 地址为准） |

已经存在 `TeslaBox-Upstream`、`WendeWiFi_5G` 等客户端连接配置。后端不会读取或返回任何已保存 WiFi 密码。

## 已实现

### 页面

- 左上角 `T` 图标打开侧边导航。
- “TeslaBox / 车载存储中心”品牌文字返回概览。
- 设置中心分为“上游 WiFi”“AP 热点”“系统与电源”三个区域。
- 桌面端和移动端响应式布局。
- 已保存网络展示 SSID、当前连接状态、自动连接状态、连接和忘记操作。
- 已保存网络可单独开启或关闭自动连接；新网络默认自动连接，也可在连接前取消。
- 可用网络展示 `SSID / SIGNAL / BAND / SECURITY`。
- 相同 SSID 的多个 BSSID 优先保留当前连接项；未连接时只保留信号最强项。
- 连接时锁定所选 BSSID，并在需要时把 `ap0` 热点同步到相同信道。
- 手动扫描按钮；没有主动扫描时展示 NetworkManager 最近一次扫描缓存。
- 新网络通过密码弹窗连接；已保存网络无需再次输入密码。
- 不支持的企业认证或 WEP 网络会禁用连接按钮。
- AP 表单支持 SSID、新密码、频段、信道、启用状态和自动启动。
- AP 密码留空表示保留原密码，读取接口永远不会返回密码。
- 重启、关机、忘记网络、切换无线模式和应用 AP 配置都有二次确认。
- 关机页面明确提示必须重新断电上电才能启动。
- 外接存储只列出具有稳定 by-id 的 USB 整盘；TF 系统盘、内部 NVMe、无序列号、已挂载、swap 和容量不足设备不能初始化。
- 初始化要求再次输入设备序列号并勾选擦除确认；失败或断电后可以继续，设备已拔出时也可以取消未完成任务。

### 后端

- 新增 `internal/deviceadmin`，集中封装 NetworkManager 和 systemd 操作。
- 外部命令全部通过 `exec.CommandContext` 的参数数组执行，不拼接 shell 命令。
- 外部命令有 20 秒超时，网络写操作由互斥锁串行执行。
- 所有设置写接口继续受现有登录会话和同源 POST 校验保护。
- 请求体限制为 16KiB，拒绝未知 JSON 字段。
- SSID、WPA 密码、UUID、频段和信道均在服务端校验。
- 电源命令在接口返回 `202 Accepted` 两秒后执行，保证浏览器先收到响应。
- API 不向浏览器返回 `nmcli`、systemd 或文件路径等内部错误细节。
- 后台每 15 秒核对自动连接网络。掉线后重新扫描并按优先级选择已保存网络；上游 BSSID 或信道变化时更新 BSSID、让 `ap0` 跟随信道并恢复 `wlan0`。
- 自动恢复失败采用最长一分钟的指数退避，最近尝试、成功、错误和下次时间通过 WiFi 状态返回。
- 数据盘身份和初始化进度以权限 `0600` 的原子 JSON 文件保存在 TF 系统盘，未完整完成镜像校验前不会切换活动数据盘。

## API

所有接口都位于 `/api/v1`，并要求有效的 Web 登录会话。

| 方法 | 路径 | 说明 |
| --- | --- | --- |
| GET | `/api/v1/network/wifi` | 当前无线状态、已保存网络和缓存的可用网络 |
| POST | `/api/v1/network/wifi/scan` | 手动扫描附近 WiFi |
| POST | `/api/v1/network/wifi/connect` | 连接已保存网络或新网络 |
| POST | `/api/v1/network/wifi/autoconnect` | 修改已保存网络的自动连接策略 |
| POST | `/api/v1/network/wifi/forget` | 删除已保存客户端连接 |
| GET | `/api/v1/network/ap` | 读取 AP 配置，不返回密码 |
| POST | `/api/v1/network/ap` | 校验、保存并应用 AP 配置 |
| GET | `/api/v1/storage/devices` | 当前 USB 整盘、登记和可初始化状态 |
| POST | `/api/v1/storage/initialize` | 双重确认后初始化或继续目标存储设备 |
| POST | `/api/v1/storage/initialize/cancel` | 放弃未完成初始化并恢复原活动设备选择 |
| POST | `/api/v1/system/power` | 提交 `reboot` 或 `shutdown` |

连接已保存网络：

```json
{"uuid":"77824373-55d5-4594-b2a2-e87020a9d2b5","bssid":"02:00:00:00:00:01","channel":161}
```

连接新网络：

```json
{"ssid":"ExampleWiFi","bssid":"02:00:00:00:00:01","channel":161,"password":"example-password","security":"WPA2","auto_connect":true}
```

修改自动连接：

```json
{"uuid":"77824373-55d5-4594-b2a2-e87020a9d2b5","enabled":true}
```

AP 更新：

```json
{"enabled":true,"ssid":"TeslaBox-5G","password":"","band":"a","channel":149,"auto_connect":true}
```

## AP+STA 并发约束

MT7922 当前驱动报告支持一个 AP 和多个 managed 接口，但限制为 `#channels <= 1`。因此：

- `teslabox-ap.service` 在 NetworkManager 初始化 `wlan0` 后创建 `ap0`，热点只绑定 `ap0`；`wlan0` 只用于上游 STA。
- 后端只有在活动 AP 确实绑定 `ap0` 时才报告并发模式；如果热点仍在 `wlan0`，扫描和连接会恢复安全拦截。
- 扫描结果保留 BSSID 和信道。连接前若热点信道不同，后端会先重启热点到目标信道，再锁定目标 BSSID 并连接。
- 热点换信道时手机连接会短暂中断，通常会自动重连；首次调试应保留有线管理连接。
- 自动连接开启后，运行期监听器会处理上游掉线、BSSID 更换和信道变化。单射频限制决定热点恢复时手机仍可能短暂断开，但无需手动重新扫描和连接。
- `ap0` 必须在 NetworkManager 启动并初始化 `wlan0` 后创建；提前创建会被 `mt7921e` 初始化流程清理。
- `ap0` 使用从 `wlan0` 派生的本地管理 MAC，避免两个活动接口使用相同 MAC 时被内核拒绝启用。

### 网络和电源断联

- 修改 AP 名称、密码、频段或信道会重启热点。
- 从 AP 切换到客户端 WiFi 后，设备地址由上游 DHCP 决定。
- 重启和关机会先停止 TeslaBox 服务并断开 USB Gadget。
- 目前只做二次确认，尚未阻止车机正在写入时执行电源操作。

## 待实现

按优先级排序：

1. 在电源操作前读取 Gadget `host_connected`，车机连接时增加更强的确认或默认禁止。
2. 增加网络切换任务状态和“新地址发现”，避免切换后用户不知道从哪里重新访问。
3. 继续实机验证开放网络、WPA2、WPA3、错误密码、隐藏 SSID 和中文 SSID。
4. 增加 AP 已连接客户端列表、主机名、IP、MAC、信号和拉黑操作。
5. 增加默认路由来源、互联网共享状态和 4G/5G 上游切换。
6. 增加 AP 配置失败的事务回滚；当前 NetworkManager 修改成功但重新激活失败时，需要人工恢复配置。
7. 评估直接使用 NetworkManager D-Bus，减少对不同版本 `nmcli` 输出格式的依赖。
8. 增加设置操作审计日志，但日志禁止记录 WiFi 密码。
9. 增加 OTA、管理密码和系统日志设置页面。

## 代码位置

| 路径 | 作用 |
| --- | --- |
| `internal/deviceadmin/manager.go` | NetworkManager、输入校验和电源操作 |
| `internal/deviceadmin/manager_test.go` | nmcli 解析、校验和电源调度测试 |
| `internal/storage/registry.go` | TF 系统盘活动数据盘身份和初始化进度登记册 |
| `internal/storageadmin/manager.go` | USB 整盘分类、显式初始化、继续和取消 |
| `internal/api/handler.go` | 设置中心 REST API |
| `internal/api/deviceadmin_test.go` | 设置接口契约测试 |
| `internal/webui/assets/index.html` | 侧边导航、设置页面和确认弹窗 |
| `internal/webui/assets/app.js` | 设置页面交互和 API 调用 |
| `internal/webui/assets/app.css` | 设置页面响应式样式 |
| `deploy/scripts/teslabox-ap-setup.sh` | 等待上游连接后创建 `ap0` 并激活热点 |
| `deploy/systemd/teslabox-ap.service` | `ap0` 开机恢复服务 |
| `cmd/teslabox/main.go` | 注入设备管理器 |

## 后续接手建议

1. 先执行 `go test ./...`、`go vet ./...` 和 JavaScript 语法检查。
2. 在板端只调用 GET 接口确认 nmcli 字段兼容性。
3. 测试扫描时保留有线连接，不要只通过 TeslaBox AP 管理设备。
4. AP 写入测试先记录现有配置，并保留 `/usr/local/bin/teslabox` 旧版本。
5. 电源接口只验证参数拒绝和 `202` 响应；没有现场人员时不要实际触发关机。
6. 所有新增日志和接口响应继续禁止输出密码、会话 Cookie 和 NetworkManager secret。

## 本次部署与验证

设置中心第一版已部署到板端；2026-07-17 的 AP+STA 修复以 `192.0.2.38` 有线地址验证：

| 项目 | 结果 |
| --- | --- |
| 服务状态 | `teslabox.service` 为 `active` |
| 设置页 | `/settings` 返回 HTTP 200 |
| 页面资源 | `app.js?v=20260717-10` |
| 构建 SHA256 | `06cf1f7f20ed18ca0a7fe2fef269ad00b5d568d3a36eae2447911dde70ad520a` |
| 板端备份 | `/usr/local/bin/teslabox.before-wifi-channel-sync` |
| 已保存客户端网络 | 实机读取到 2 个 |
| AP 读取 | `TeslaBox-5G`、`ap0`、5GHz、161、共享开启 |
| 密码泄露检查 | AP JSON 不包含 password/PSK |
| AP+STA | `ap0` 与 `wlan0` 同在信道 161；当前上游 BSSID 为 `02:00:00:00:00:01` |
| 自动连接 | `ExampleWiFi-5G` 已开启，优先级 90，并锁定 BSSID `02:00:00:00:00:01` |
| 运行期自愈 | 人工配置错误 BSSID `00:11:22:33:44:55`、热点信道 149 并断开 `wlan0`；监听器一次恢复到真实 BSSID 和信道 161 |
| 存储登记 | 当前 TESLADRIVE 已按 UUID、序列号、型号和容量登记，文件权限为 `0600` |
| 存储页面 | 当前盘返回 `initialized=true`、`active=true`，初始化、继续和取消按钮均禁用 |
| 开机恢复 | 已由远程执行真实重启；`wlan0`、`ap0`、NetworkManager、TeslaBox 服务均自动恢复 |
| 电源参数保护 | 非法 action 返回 HTTP 400 |

本次已执行真实 WiFi 切换、AP 信道同步和两次重启。最终重启后 `wlan0=192.0.2.28/22`、`ap0=192.168.88.1/24`，NetworkManager 连通性为 `full`。

## 手工验收清单

- [ ] 点击 `T` 图标可以打开/关闭侧边导航。
- [ ] 概览和设置中心切换后，浏览器前进/后退正常。
- [ ] 已保存网络不包含 AP 配置，不显示密码。
- [ ] AP 运行时可以扫描，热点位于 `ap0`；若 `ap0` 缺失则页面显示安全限制。
- [ ] 修改已保存网络的自动连接开关后，刷新页面状态保持一致。
- [ ] 可用网络表格正确展示 SSID、信号、频段和加密方式。
- [ ] 不支持的加密类型不能连接。
- [ ] AP GET 接口不包含 PSK。
- [ ] AP 密码留空不会清空原密码。
- [ ] 所有危险操作都有二次确认。
- [ ] 手机窄屏下表单、表格和按钮可用。
- [ ] 服务重启后概览、录像播放和设置中心均正常。
- [x] 上游错误 BSSID、掉线和热点错误信道可由运行期监听器自动恢复。
- [x] 当前已初始化盘只显示活动状态，不提供重复初始化入口。
