# carplay-demo

在 Linux 板子（Radxa Rock 3A / RK3568）上用 Go 实现 CarPlay 接收端的**协议栈**。
画面最终在**特斯拉车机浏览器**里显示——车机只能通过浏览器访问，没有 HDMI 直出，也不走 DRM/KMS 渲染。

参考实现是 [shilapi/xcertplay](https://github.com/shilapi/xcertplay)（Android 车机上的 CarPlay 主机，Kotlin + JNI）。本实验使用 Go 实现协议栈，并把显示层设计为「协议栈 + Web 服务」。参考的上游版本和代码对应范围尚未完成审查，不能仅凭语言不同判断其版权或许可关系；参见[开源审查记录](../../docs/OPEN_SOURCE_REVIEW.md)。

> 关于声音：本项目**不做音频**。手机连车机蓝牙播放声音，板子只负责视频与触摸回传。

## 为什么单独一个 module

`experiment/carplay-demo` 带自己的 `go.mod`，不参与主项目构建：
主项目 `go build ./...` 和 golangci-lint 都不会扫到它。等协议栈稳定后再决定要不要并入 `internal/`。

```bash
cd experiment/carplay-demo
go test ./...
```

## 目录结构

```
i2c/                 I2C 传输抽象（Transport 接口 + 错误类型）
i2c/ch341/           沁恒 CH341 USB 转 I2C 桥接器的流协议编解码（含分段读）
i2c/devi2c/         Linux /dev/i2c-N 后端（纯 Go ioctl，无 cgo，量产形态）
mfi/                Apple MFi 认证协处理器访问：寄存器、扫描、证书、签名
iap2/                iAP2 协议栈（按 xcertplay 分层）
  link/             链路层：帧编解码 + 校验和 + 状态机（SYN 握手 / 重传 / 累积 ACK / EAK）
  csm/              CSM 层：帧 + 参数（有序 TLV）+ 标量/字符串编解码 + Framer
  endpoint/         iAP2 端点与参数 ID 常量目录
  session/          会话层：Identification / Authentication / CarPlay 启动 / HID 开启
transport/          传输通道
  usbmux/          USBMUX v2 帧 + 握手 + TCP 隧道，Conn 实现 net.Conn（有线接入）
  wired/           有线链路：marker → USBMUX → Connect(62078)
  wireless/        无线链路：蓝牙可发现/配对 + RFCOMM 监听 + hostapd AP 抽象
carplay/            顶层编排器：状态机 + MFi 认证 + 启动 CarPlay + 触摸回传
web/                给浏览器用的服务层：手写最小 WebSocket（零依赖）+ HTTP + 视频广播
media/              视频/输入通道接口（VideoSink / TouchSource / ButtonSource），为网页端预留接缝
tools/              mfi_tool.py：在 macOS 上直连 CH341 的验证工具
```

## 关键设计：传输层抽象

`i2c.Transport` 只有一次事务的语义（可选写 + 可选读），上层协议全部依赖它。
这样两种后端可随意互换，上层一行不改：

| 后端 | 用途 | 状态 |
|---|---|---|
| CH341 USB 转接板 | 开发调试，板子/PC 都能插 | 流协议编解码已完成，USB 端点访问（cgo+libusb）需在板子上编 |
| 板载 `/dev/i2c-N` | 量产形态，芯片直接焊在板子 I2C 上 | 已实现（纯 Go ioctl，无 cgo） |

## 关键设计：媒体/输入通道接口

`media` 包只定义接口与数据模型，**不实现**解码、渲染和输入采集：

```
视频：iPhone → 板子（协议栈产出 H.264）→ 实现 VideoSink → 浏览器（WebRTC / MSE）
触摸：浏览器（touch 事件）→ 实现 TouchSource → 板子 → iAP2 HID → iPhone
```

后期只要实现这些接口，就能把画面接到网页上，协议栈代码一行不用改。

Web 服务层的接口约定（`web/server.go`）：

- `GET /api/status` → `{state, wireless, videoFormat, viewers}`
- `WS /ws` → 下行二进制帧（一个 H.264 访问单元），上行 JSON 触摸 `{"action":"down|move|up","x":0,"y":0,"viewWidth":..,"viewHeight":..}`

## 已验证的硬件事实

以下是在真实板子上实测得到的，本项目的测试向量直接取自这些数据：

```
USB 设备        1a86:5512  沁恒 CH341（并口/I2C 模式，描述符串 "USB UART-LPT"，12 Mb/s）
MFi 芯片地址    0x11       （0x10 无响应）
device version  0x05       （寄存器 0x00）
protocol major  0x02       （寄存器 0x02）
证书长度        908 字节   （寄存器 0x30，DER PKCS#7 signedData）
证书签发者      CN=Apple iPod Accessories Certification Authority, O=Apple Inc.
证书有效期      2016-11-25 → 2022-02-14（已过期，拆机芯片常态，不影响认证）
```

用 `tools/mfi_tool.py` 可以在 macOS 上复现以上结果：

```bash
pip install pyusb libusb-package
python3 tools/mfi_tool.py scan     # 扫总线
python3 tools/mfi_tool.py info     # 读身份寄存器
python3 tools/mfi_tool.py cert     # 导出证书
```

## 协议要点（对照 xcertplay 源码踩过的坑）

### I2C / MFi
1. **读命令长度语义**：CH341 的 `C0|n` 是读 **n** 字节（n=0 表示读 1 字节），不是 n+1。
2. **写命令两种形态**：裸 `80`（写 1 字节）会回传一个 ACK/NACK 状态字节，批量 `80|n` 不回传。
   寻址阶段必须用裸 `80`，才能检测出「这个地址上没有器件」。
3. **首次 NACK 是正常的**：芯片空闲时休眠，第一次访问往往 NACK，重试即可唤醒。
4. **读寄存器是两段式**：先写寄存器号（带 STOP），间隔约 5ms，再单独纯读。
5. **超过 32 字节要分段**：中间段补齐到 32 字节且**故意不发 STOP**，让 I2C 事务延续到下一段。

### iAP2 链路层
6. **链路帧** `FF 5A | u16 长度 | 控制 | 序号 | 确认 | 会话ID | 头校验 | 载荷 | 尾校验`，校验和是**字节和取负 mod 256**（验证即整段求和看是否 ≡0）。
   控制位 SYN 0x80 / ACK 0x40 / EAK 0x20 / RST 0x10，初始序号 99。
7. **SYN 同步载荷** 错一个字段 iPhone 就不切模式：真实布局是
   `u8 version | u8 maxOutgoing | u16 maxLength | u16 重传超时 | u16 确认超时 | u8 最大重传 | u8 最大累积确认 | 每会话 3 字节(id,kind,version)`。
   会话清单固定为 **控制(10)/ EA(11)/ 文件传输(12)**，文件传输必须 **v2**（否则 iPhone 取不到专辑封面）。
8. **收到 SYN 后必须把期望序号对齐到 `InitialSequence-1`（98）**：iAP2 双方都从 99 开始编号，不对齐对端第一帧会被当成乱序帧缓存。

### USBMUX（有线接入）
9. **不是 NCM 网卡，是 USBMUX 隧道**：iPhone USB bulk 端点 → 发 marker → USBMUX v2 握手 → 拨号到 lockdown 62078 → 得到字节流 → iAP2。
10. **只信任 length 字段**：iPhone 回复不带 `0xFEEDFACE`，按 magic 校验会误判。

## 测试

`go test ./...` 覆盖 i2c/ch341、iap2/link、iap2/csm、iap2/session、mfi、carplay（端到端握手）。
其中 `carplay` 包用内存管道模拟 USBMUX/RFCOMM 流 + 假 MFi 芯片，跑通了
`链路同步 → 身份识别 → 证书 → 挑战签名 → 认证通过 → CarPlay 启动 + HID 开启` 全流程。

交叉编译（目标板）：`GOOS=linux GOARCH=arm64 go build ./...`

## 尚未完成（最后一公里）

- [ ] CH341 的 USB bulk 端点访问（cgo + libusb），需到板子上编译
- [ ] 视频流落点：CarPlay 视频走独立会话，需接进 `Video chan []byte`（建议拿一次真机抓包定通道）
- [ ] 真机校准：HID 报告格式按常见布局填的，接真 iPhone 要按下发的描述符校正
- [ ] 无线链路各步骤的真实执行（蓝牙配对/AP 启动目前是抽象，未接 BlueZ/hostapd 命令）

## 风险提示

RK3568 的 H.264 硬解在 5.10 内核上大概率不可用（mainline 的 `rkvdec2` 要 6.x）。
软解的话 A55 四核解 720p 会很吃力。**在投入解码工作之前，先上板子跑一次硬解基准测试。**
