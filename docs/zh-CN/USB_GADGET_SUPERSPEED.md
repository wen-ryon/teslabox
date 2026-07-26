# TeslaBox USB Gadget SuperSpeed 识别与 High-Speed 排查

这份说明用于解释 TeslaBox 当前如何识别 USB Gadget 的速率，以及为什么“主机、线材或接口标称 USB 3.0”时，设备仍可能只枚举为 High-Speed。

## 结论

TeslaBox 不会根据接口名称、线材型号或 `bcdUSB` 字段猜测速率。页面显示的速率直接来自 Linux UDC（USB Device Controller）的实际协商结果：

```text
/sys/class/udc/<UDC>/current_speed
```

因此，`current_speed=high-speed` 的含义是：本次连接实际以 USB 2.0 High-Speed（最高 480 Mbit/s）完成枚举。它不等于“程序没有开启 USB 3.0”，也不能仅凭主机端口是 USB 3.0 就推断会变成 SuperSpeed。

要得到 `super-speed`，以下链路必须同时满足：

```text
UDC、内核、设备树、PHY 支持 SuperSpeed
        +
Gadget 配置和功能驱动允许 SuperSpeed
        +
设备模式 OTG 物理端口连接了 SuperSpeed 信号
        +
完整支持 SuperSpeed 的数据线、转接器、Hub 和主机端口
        =
current_speed=super-speed
```

任一环节只支持 USB 2.0，或 SuperSpeed 信号对没有接通，最终都会回退到 `high-speed`。Linux 将 `current_speed` 定义为当前协商速率，将 `maximum_speed` 定义为该 UDC 端口可支持的最高速率。[Linux UDC ABI](https://docs.kernel.org/admin-guide/abi-stable.html)

## TeslaBox 当前代码的逻辑

TeslaBox 的速率展示是“如实上报”，不是“强制设置”。核心流程位于 [internal/gadget/manager.go](../../internal/gadget/manager.go)：

| 环节 | 当前行为 | 含义 |
| --- | --- | --- |
| 选择 UDC | 从 `/sys/class/udc` 选择可用控制器 | 决定使用哪个设备模式控制器。 |
| 查询状态 | 读取 `<UDC>/state` | `configured` 仅表示已配置，不能代表 SuperSpeed。 |
| 查询速率 | 读取 `<UDC>/current_speed` | 直接作为 `Status.CurrentSpeed` 返回给 API 和页面。 |
| 创建 Gadget | 写入 configfs 描述符、Mass Storage 功能和 LUN | 准备可被主机识别的存储设备。 |
| 绑定 UDC | 最后写入 `UDC` 文件 | 只有此前步骤成功才开始对主机枚举。 |

当前代码没有根据某个 USB 3.0 端口名称把状态改写为 `super-speed`，也没有把 `current_speed` 作为配置项写回系统。这是正确的：协商结果只能由硬件和内核在实际连线后决定。

> 在 RK3568 上，拔线后 UDC 的 `state` 可能短时间保留为 `configured`。TeslaBox 因而不能把它当作“线仍插着”或“SuperSpeed 成功”的唯一证据；排查速率应看 `current_speed`，并结合重新插拔后的日志和主机侧信息。

### 关于 `bcdUSB=0x0200`

TeslaBox 创建 configfs Gadget 时会写入 `bcdUSB=0x0200`。这个字段是设备描述符版本字段，不是对物理链路的 SuperSpeed 能力检测，也不应单独作为“只能 USB 2.0”的结论。

在 Linux composite gadget 驱动中，实际返回设备描述符时会依据当前枚举速率和控制器能力调整 `bcdUSB`：实际为 SuperSpeed 时会报告更高的 USB 版本；控制器支持 SuperSpeed 但本次协商未达到时也会使用相应兼容版本。[Linux composite 源码](https://codebrowser.dev/linux/linux/drivers/usb/gadget/composite.c.html)

不同内核版本的实现会有差异，所以不要仅凭这个 configfs 初始值修改代码。应先确认本板子的 `maximum_speed`、`current_speed` 和 configfs `max_speed`。

## 只读诊断命令

以下命令不绑定、不解绑、不修改 Gadget，适合在出现问题的板子上执行：

```sh
G=/sys/kernel/config/usb_gadget/teslabox
UDC="$(cat "$G/UDC" 2>/dev/null || true)"

printf 'Gadget UDC: %s\n' "${UDC:-<未绑定>}"
printf 'Gadget max_speed: '
cat "$G/max_speed" 2>/dev/null || echo '<当前内核未提供>'
printf 'Gadget bcdUSB: '
cat "$G/bcdUSB" 2>/dev/null || echo '<不可读取>'

if [ -n "$UDC" ]; then
    printf 'UDC maximum_speed: '
    cat "/sys/class/udc/$UDC/maximum_speed" 2>/dev/null || echo '<当前内核未提供>'
    printf 'UDC current_speed: '
    cat "/sys/class/udc/$UDC/current_speed" 2>/dev/null || echo '<不可读取>'
    printf 'UDC state: '
    cat "/sys/class/udc/$UDC/state" 2>/dev/null || echo '<不可读取>'
fi
```

请在保持同一根线、同一主机端口的前提下执行一次；再更换一根已确认可传输 USB 3.x 数据的短线并直接连接主机端口后执行一次。不要通过普通 USB 2.0 充电线、扩展坞前置接口或未知规格转接器判断板子的能力。

如果主机是 Linux，也可以在主机侧重新插拔时执行：

```sh
lsusb -t
dmesg -w
```

主机日志有助于定位线材、Hub 或端口回退，但 TeslaBox 的 `current_speed` 才是设备端实际协商结果。

## 结果判读

| 现象 | 更可能的原因 | 下一步 |
| --- | --- | --- |
| `maximum_speed=high-speed` | UDC 驱动、设备树、PHY 或该设备模式端口本身只暴露 USB 2.0 | 检查板卡原理图、设备树和内核 UDC/PHY 驱动；修改 Gadget 应用不会提升速率。 |
| `maximum_speed=super-speed`，但 Gadget `max_speed=high-speed` | configfs 对该 Gadget 显式限速 | 仅在未绑定 UDC 时，按目标内核支持的值调整 `max_speed`；必须保留 High-Speed 回退。 |
| 两个 `max_speed` 都允许 SuperSpeed，但 `current_speed=high-speed` | 线材、Hub、主机端口、转接器、OTG 走线、Type-C 方向/复用器导致 SuperSpeed 信号未通 | 用已验证的直连 USB 3.x 数据线和主机后置端口复测；再检查硬件连接与设备树。 |
| `current_speed=super-speed`，实际读写仍慢 | 存储介质、文件系统、主机负载、Gadget Mass Storage 缓冲策略或电源限制 | 单独做顺序读写基准；不要把吞吐问题误判为枚举速率问题。 |
| 不存在 `max_speed` 文件 | 较旧内核没有导出对应 configfs 属性 | 不要盲目写该文件；以 UDC 的 `maximum_speed`/`current_speed` 和内核源码为准。 |

configfs 的 `max_speed` 是一个限速配置，常见有效值包括 `super-speed-plus`、`super-speed`、`high-speed`、`full-speed` 和 `low-speed`；它应在绑定 UDC 前设置，绑定后内核会拒绝修改。[Linux configfs ABI](https://kernel.org/doc/html/next/admin-guide/abi-testing.html)

## 给 TeslaUSB 重构项目的实现建议

1. 将“能力”和“本次协商结果”分开上报：至少同时展示 `maximum_speed`、Gadget `max_speed`（若存在）和 `current_speed`。
2. 不要硬编码“USB 3.0 端口必然 SuperSpeed”，也不要在检测到 High-Speed 后伪造 SuperSpeed 状态。
3. 如需设置 configfs `max_speed`，先检测属性是否存在、目标值是否被该内核支持，并且只在 UDC 未绑定时设置。
4. 支持 SuperSpeed 失败时应保留 High-Speed 可用的降级路径；对 TeslaUSB 而言，能稳定枚举通常比强制失败更重要。
5. 将 configfs 的创建、解绑、清理和重新绑定串行化。任何重建动作都应避免与正在导出数据盘、主机正在访问 LUN 或系统关机流程并发。
6. 对 ROCK 3A 这类存在 OTG 供电时序限制的板子，不能为了刷新速率状态频繁解绑/重绑 Gadget；速率诊断优先读取 sysfs。

## TeslaBox 后续可增强的诊断信息

TeslaBox 当前已经把 `current_speed` 返回到状态接口。若要减少现场误判，可以额外返回：

```json
{
  "udc": "...",
  "udc_maximum_speed": "super-speed",
  "gadget_max_speed": "super-speed",
  "current_speed": "high-speed",
  "state": "configured"
}
```

这样可以直接区分“硬件/内核最高只支持 High-Speed”“Gadget 配置限速”与“链路本次回退”。在没有真实板卡证据前，不建议仅为页面显示而改动 Gadget 生命周期或强制写入 SuperSpeed。

## 参考资料

- [Linux UDC sysfs ABI](https://docs.kernel.org/admin-guide/abi-stable.html)
- [Linux USB Gadget configfs 使用说明](https://docs.kernel.org/usb/gadget_configfs.html)
- [Linux configfs `max_speed` / `bcdUSB` ABI](https://kernel.org/doc/html/next/admin-guide/abi-testing.html)
- [Linux composite gadget 设备描述符实现](https://codebrowser.dev/linux/linux/drivers/usb/gadget/composite.c.html)
