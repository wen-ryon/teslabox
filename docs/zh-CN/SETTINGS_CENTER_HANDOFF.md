# 设置中心第一版需求与交接说明

更新时间：2026-07-17

## 目标

在现有 TeslaBox 管理页面中增加可扩展的设置中心，第一版覆盖：

1. 上游 WiFi：查看已保存网络、查看最近一次扫描缓存、手动扫描、连接网络和忘记网络；每个已保存网络独立记忆 DHCP 或静态 IPv4 设置。
2. AP 热点：查看并修改热点启用状态、SSID、密码、频段、信道和开机自动启动。
3. 外接存储：识别当前活动数据盘和未初始化的 USB/NVMe 数据整盘，提供受保护的初始化、重新初始化、继续和取消入口，并可选创建 MUSIC 音乐分区。
4. 安全与账户：修改管理页面密码；全新安装使用默认密码 `teslabox`，首次登录必须修改。
5. 系统电源：经过二次确认后重启或关闭 ROCK 3A。
6. 导航：点击左上角 `T` 图标打开侧边导航，品牌文字返回设备概览。
7. 媒体中心：位于概览和设置中心之间，管理 `Wraps/`、`Boombox/`、`LightShow/` 与可选 MUSIC 分区；上传先暂存，确认 OTG 断开后才统一写入虚拟盘。

## 板端现状

当前 ROCK 3A 使用 NetworkManager 管理网络，不使用独立的 hostapd/dnsmasq 服务：

| 项目 | 当前值 |
| --- | --- |
| 无线网卡 | 当前验证机为 MT7922、`wlan0`/`ap0`、驱动 `mt7921e`；程序不依赖固定接口名 |
| AP 连接配置 | `TeslaBox-5G` |
| AP 模式 | NetworkManager `802-11-wireless.mode=ap` |
| 频段/信道 | 默认优先 5GHz / 161；启动时核对网卡允许信道，连接上游时同步为目标信道 |
| 加密 | WPA-PSK |
| AP 地址 | `192.168.88.1/24` |
| 共享方式 | NetworkManager `ipv4.method=shared` |
| 当前管理地址 | `http://192.0.2.38/`（有线，以实际 DHCP 地址为准） |

已经存在 `TeslaBox-Upstream`、`WendeWiFi_5G` 等客户端连接配置。后端不会读取或返回任何已保存 WiFi 密码。

## 已实现

### 页面

- 左上角 `T` 图标打开侧边导航。
- “TeslaBox / 车载存储中心”品牌文字返回概览。
- 设置中心分为“上游 WiFi”“AP 热点”“系统与电源”三个区域。
- 桌面端和移动端响应式布局。
- 已保存网络展示 SSID、当前连接状态、自动连接状态、连接和忘记操作。
- 已保存网络可单独开启或关闭自动连接；新网络默认自动连接，也可在连接前取消。
- 每个已保存网络可选择 DHCP 或静态 IPv4，并配置地址、子网掩码、网关和最多四个 IPv4 DNS。当前连接网络保存后立即重连，其他网络在下次连接时生效。
- 可用网络展示 `SSID / SIGNAL / BAND / SECURITY`。
- 相同 SSID 的多个 BSSID 优先保留当前连接项；未连接时只保留信号最强项。
- 连接时锁定所选 BSSID，并在需要时把自动识别到的热点接口同步到相同信道。
- 手动扫描按钮；没有主动扫描时展示 NetworkManager 最近一次扫描缓存。
- 新网络通过密码弹窗连接；已保存网络无需再次输入密码。
- 不支持的企业认证或 WEP 网络会禁用连接按钮。
- AP 表单支持 SSID、新密码、频段、信道、启用状态和自动启动。
- AP 密码留空表示保留原密码，读取接口永远不会返回密码。
- 重启、关机、忘记网络、切换无线模式和应用 AP 配置都有二次确认。
- 关机页面明确提示必须重新断电上电才能启动。
- 外接存储只列出具有稳定 by-id 身份和稳定序列号、且未被系统使用的 USB 整盘或 NVMe 数据盘；TF 系统盘、无序列号、已挂载、swap 和容量不足设备不能初始化。
- 初始化要求物理拔掉 OTG、再次输入设备序列号并勾选擦除确认；失败或断电后可以继续，设备已拔出时也可以取消未完成任务。
- 已登记且只有预期 XFS 分区挂载在数据目录的活动盘显示“重新初始化”；该入口与首次初始化使用相同的防误操作确认并清空整盘。
- 虚拟盘容量按物理盘动态计算：预留 5% 且不少于 10GB，车机可见镜像最大为 2TB。128GB 盘预计提供约 118GB，500GB 盘预计提供约 475GB；完整容量保护布局可通过“优化保留空间”无损释放旧版多留的空间。
- 初始化可选创建 FAT32/MUSIC 分区，并用滑块调整 TeslaCam 与 MUSIC 比例；页面展示两个分区的预估容量。
- 设置中心提供“媒体自动维护恢复”开关，默认关闭。用户明确确认风险后，应用媒体或删除直接写入媒体可由 TeslaBox 自动解绑 Gadget；媒体操作完成后安排温重启。在线更新默认自动解绑、健康检查并重启恢复，不受该媒体开关影响。
- 设置中心提供独立“回收旧空间”操作。它不会绑定普通刷新；执行前必须进入媒体维护窗口、检查 exFAT 并解除 loop，只根据 Allocation Bitmap 的空闲簇返还镜像外层 XFS 空间。

### 后端

- 新增 `internal/deviceadmin`，集中封装 NetworkManager 和 systemd 操作。
- 外部命令全部通过 `exec.CommandContext` 的参数数组执行，不拼接 shell 命令。
- 外部命令有 20 秒超时，网络写操作由互斥锁串行执行。
- 所有设置写接口继续受现有登录会话和同源 POST 校验保护。
- 请求体限制为 16KiB，拒绝未知 JSON 字段。
- SSID、WPA 密码、UUID、频段和信道均在服务端校验。
- 电源命令在接口返回 `202 Accepted` 两秒后执行，保证浏览器先收到响应。
- API 不向浏览器返回 `nmcli`、systemd 或文件路径等内部错误细节。
- 后台订阅 NetworkManager 的设备、连接、BSSID 和信道变化事件；只有状态实际变化时才核对并恢复。D-Bus 监听不可用时每 2 分钟只读取一次轻量连接状态，确认断线后才执行完整扫描，避免车载待机时周期性唤醒无线芯片。
- 自动恢复失败采用最长一分钟的指数退避，最近尝试、成功、错误和下次时间通过 WiFi 状态返回。
- 数据盘身份和初始化进度以权限 `0600` 的原子 JSON 文件保存在 TF 系统盘，未完整完成镜像校验前不会切换活动数据盘。
- 破坏性存储初始化始终要求物理拔掉 OTG；媒体自动维护只在用户明确启用后适用于虚拟盘媒体变更。在线更新默认自动独占状态机并解绑 Gadget，完成后在新的 `boot_id` 通过系统重启恢复。

## API

所有接口都位于 `/api/v1`，并要求有效的 Web 登录会话。

| 方法 | 路径 | 说明 |
| --- | --- | --- |
| GET | `/api/v1/network/wifi` | 当前无线状态、已保存网络和缓存的可用网络 |
| POST | `/api/v1/network/wifi/scan` | 手动扫描附近 WiFi |
| POST | `/api/v1/network/wifi/connect` | 连接已保存网络或新网络 |
| POST | `/api/v1/network/wifi/autoconnect` | 修改已保存网络的自动连接策略 |
| POST | `/api/v1/network/wifi/ipv4` | 保存某个已保存网络的 DHCP 或静态 IPv4 设置；当前网络会立即重连 |
| POST | `/api/v1/network/wifi/forget` | 删除已保存客户端连接 |
| GET | `/api/v1/network/ap` | 读取 AP 配置，不返回密码 |
| POST | `/api/v1/network/ap` | 校验、保存并应用 AP 配置 |
| GET | `/api/v1/storage/devices` | 当前非系统 USB/NVMe 整盘、登记和可初始化状态 |
| POST | `/api/v1/storage/initialize` | OTG、序列号和擦除确认后初始化、重新初始化或继续目标存储设备；可携带音乐分区比例 |
| POST | `/api/v1/storage/initialize/cancel` | 放弃未完成初始化并恢复原活动设备选择 |
| POST | `/api/v1/storage/reclaim` | 在独占媒体维护窗口内检查 exFAT 分配位图并回收历史删除后残留的外层镜像占用 |
| GET | `/api/v1/media-assets` | 读取媒体中心受控资源、待应用状态和虚拟盘可用性 |
| POST | `/api/v1/media-assets/upload?category=...` | 流式暂存锁车音效、皮肤、灯光秀或音乐文件 |
| POST | `/api/v1/media-assets/{id}/delete` | 删除暂存文件或标记已应用文件待删除 |
| POST | `/api/v1/media-assets/external-delete` | OTG 断开确认后，立即删除电脑或车机直接写入的媒体文件 |
| POST | `/api/v1/media-assets/apply` | 手动模式要求 OTG 断开确认；自动模式自动暂停 Gadget、写入虚拟盘并提交温重启 |
| GET | `/api/v1/media-assets/{id}/preview` | 仅提供受控锁车音效和皮肤预览 |
| GET | `/api/v1/maintenance` | 读取自动维护恢复开关 |
| POST | `/api/v1/maintenance` | 经风险确认后开启或关闭自动维护恢复 |
| POST | `/api/v1/gadget/reboot-recover` | 确认 OTG 已插入后保存恢复标记并提交温重启 |
| POST | `/api/v1/system/power` | 提交 `reboot` 或 `shutdown` |
| POST | `/api/v1/auth/password` | 校验当前密码后保存新密码、轮换会话密钥并注销其他旧会话 |

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

- `teslabox-ap.service` 从 NetworkManager 自动发现物理无线接口，再创建连接配置指定的热点虚拟接口；也可通过环境变量显式指定二者。
- 后端只有在活动 AP 和客户端接口确实不同时才报告并发模式；如果热点仍占用客户端接口，扫描和连接会恢复安全拦截。
- 扫描结果保留 BSSID 和信道。连接前若热点信道不同，后端会先重启热点到目标信道，再锁定目标 BSSID 并连接。
- 热点换信道时手机连接会短暂中断，通常会自动重连；首次调试应保留有线管理连接。
- 自动连接开启后，运行期监听器会处理上游掉线、BSSID 更换和信道变化。单射频限制决定热点恢复时手机仍可能短暂断开，但无需手动重新扫描和连接。
- 热点虚拟接口必须在 NetworkManager 启动并初始化物理无线接口后创建；提前创建可能被驱动初始化流程清理。
- 热点虚拟接口使用从物理接口派生的本地管理 MAC，避免两个活动接口使用相同 MAC 时被内核拒绝启用。

### 网络和电源断联

- 修改 AP 名称、密码、频段或信道会重启热点。
- 从 AP 切换到客户端 WiFi 后，设备地址由上游 DHCP 决定。
- 重启和关机会先停止 TeslaBox 服务并断开 USB Gadget。
- 页面右上角的在线指示仅表示浏览器能够访问 TeslaBox 服务，使用“设备在线/设备离线”，不再与 OTG 物理连接混用。
- 日志中心同时读取 UDC `configured` 和 composite Gadget `suspended`。`configured` 且 `suspended=1` 时显示“可能已断开”，明确说明它也可能是主机正常休眠，不会据此声称车机仍已连接。
- 在线更新默认自动安全解绑 Gadget、等待稳定断线窗口，更新健康检查通过后自动温重启；这仍不能替代物理拔线的最高安全性。媒体写入则默认保留物理拔线确认，只有“媒体自动维护恢复”开启后才会自动维护。
- Gadget 启动保护保存在 TF 系统盘，并结合 `/proc/sys/kernel/random/boot_id` 保证同一开机周期最多尝试一次 UDC 绑定。服务重启、程序更新和页面操作都不能在原 boot_id 下重新绑定。
- 用户安全断开后，必须先物理插入 OTG 线，再优先点击“温重启恢复”；该操作本次运行只写入下一 `boot_id` 的预约状态并保持解绑，随后自动重启系统。若温重启仍无法枚举，再使用完全断电预插 OTG 的冷启动兜底。
- 板端实测发现 `/sys/class/extcon/extcon0/state` 在 OTG 已拔出时仍可能报告 `USB=1`，因此不得把 extcon 当作设备模式 OTG 物理插线信号，也不得据此运行时恢复 Gadget。
- 目前只做二次确认，尚未阻止车机正在写入时执行电源操作。

## 待实现

按优先级排序：

1. 为电源操作增加与在线更新相同的物理断线确认和 Gadget 安全暂停流程，不能依赖可能残留的 `host_connected`。
2. 增加网络切换任务状态和“新地址发现”，避免切换后用户不知道从哪里重新访问。
3. 继续实机验证开放网络、WPA2、WPA3、错误密码、隐藏 SSID 和中文 SSID。
4. 增加 AP 已连接客户端列表、主机名、IP、MAC、信号和拉黑操作。
5. 增加默认路由来源、互联网共享状态和 4G/5G 上游切换。
6. 增加 AP 配置失败的事务回滚；当前 NetworkManager 修改成功但重新激活失败时，需要人工恢复配置。
7. 评估直接使用 NetworkManager D-Bus，减少对不同版本 `nmcli` 输出格式的依赖。
8. 增加设置操作审计日志，但日志禁止记录 WiFi 密码。
9. 日志中心和签名在线更新第一版已经实现，后续补充历史版本、暂停操作审计和更完整的失败诊断。

## 代码位置

| 路径 | 作用 |
| --- | --- |
| `internal/deviceadmin/manager.go` | NetworkManager、输入校验和电源操作 |
| `internal/deviceadmin/manager_test.go` | nmcli 解析、校验和电源调度测试 |
| `internal/storage/registry.go` | TF 系统盘活动数据盘身份和初始化进度登记册 |
| `internal/storageadmin/manager.go` | USB/NVMe 数据整盘分类、显式初始化、继续和取消 |
| `internal/api/handler.go` | 设置中心 REST API |
| `internal/api/deviceadmin_test.go` | 设置接口契约测试 |
| `internal/webui/assets/index.html` | 侧边导航、设置页面和确认弹窗 |
| `internal/webui/assets/app.js` | 设置页面交互和 API 调用 |
| `internal/webui/assets/app.css` | 设置页面响应式样式 |
| `deploy/scripts/teslabox-ap-setup.sh` | 自动发现客户端/热点接口，跟随上游信道或选择网卡允许信道后激活热点 |
| `deploy/systemd/teslabox-ap.service` | AP 虚拟接口开机恢复服务；失败不影响 TeslaBox 主服务 |
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
- [ ] AP 运行时可以扫描；页面展示实际热点接口名，热点不可用时 TeslaUSB、媒体和有线管理仍可正常使用。
- [ ] 修改已保存网络的自动连接开关后，刷新页面状态保持一致。
- [ ] 可用网络表格正确展示 SSID、信号、频段和加密方式。
- [ ] 不支持的加密类型不能连接。
- [ ] AP GET 接口不包含 PSK。
- [ ] AP 密码留空不会清空原密码。
- [ ] 所有危险操作都有二次确认。
- [ ] 手机窄屏下表单、表格和按钮可用。
- [ ] 服务重启后概览、录像播放和设置中心均正常。
- [x] 上游错误 BSSID、掉线和热点错误信道可由运行期监听器自动恢复。
- [x] 当前已初始化盘经过运行状态核验后提供受保护的重新初始化入口。
- [ ] 使用测试盘验证 500GB 物理盘生成约 475GB 车机可见容量，并验证重新初始化全流程。
- [ ] 在 Windows 和目标 Tesla 车机验证 exFAT/TESLACAM 与 FAT32/MUSIC 双分区同时枚举、录像和音乐播放。
- [ ] 在 OTG 预插条件下验证日志中心“温重启恢复”，并确认失败时完全断电冷启动仍可恢复。
