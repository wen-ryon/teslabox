# ROCK 3A + RTL8852BE 内核崩溃根因分析与修复手册

> 历史排查资料：项目已停止维护。本文记录当时的假设、操作和验收条件，不代表当前内核或驱动的已验证状态；执行任何安装、卸载或内核操作前，请先确认设备环境并准备恢复方式。

## 1. 文档用途

本文用于指导维护人员或自动化 Agent 处理以下问题：

- Radxa ROCK 3A 安装 RTL8852BE M.2 E-Key 无线网卡后，在开机早期反复崩溃或重启。
- 拔掉无线网卡后系统可以稳定启动。
- 系统安装了 Radxa `8852be-dkms` 驱动。
- 崩溃日志包含 `RTKM`、`rtl8852be` 和 `spinlock bad magic`。

本文既是故障说明，也是修复任务书。执行者必须区分：

1. 已经由日志确认的事实；
2. 需要进一步验证的根因假设；
3. 只能绕过故障的临时措施；
4. 可以交付给用户的正式修复。

不要因为文中给出某个高概率方向，就跳过取证、回滚和真机验收。

---

## 2. 结论摘要

当前案例不是 TeslaBox AP 脚本主动启动热点导致的崩溃，也不是 Go 服务、NetworkManager 密码或 USB Gadget 生命周期直接导致的崩溃。

已经确认的故障链路是：

```text
ROCK 3A 上电
  -> PCIe 枚举出 10ec:b852
  -> udev 根据 PCI 别名请求加载 8852be.ko
  -> 驱动执行内置 RTKM 预分配初始化
  -> 驱动开始启用 0000:01:00.0
  -> 内核报告 spinlock bad magic
  -> 系统很快失去响应或重新启动
```

目前最可能的根因有两类：

1. `8852be` 驱动 RTKM 或后续 PCIe 探测路径使用了未正确初始化、已经被覆盖或布局不匹配的自旋锁。
2. DKMS 编译时使用的内核头文件或配置与实际运行的 ROCK 3A 内核不一致，尤其是 `CONFIG_DEBUG_SPINLOCK`、锁调试配置或 `Module.symvers` 不匹配。

现有日志没有完整调用栈，因此暂时不能严谨地断言“就是 `rtkm_phy_list` 的锁”。正确修复顺序是：

1. 先隔离原驱动，确保板子插卡后也能稳定启动；
2. 核对运行内核、构建目录、配置和 DKMS 编译环境；
3. 使用完全匹配的内核构建环境重新编译驱动；
4. 优先制作关闭 RTKM、保留并发模式的测试版本；
5. 如果仍然崩溃，再定位非 RTKM 锁或评估完整回移 `rtw89`；
6. 完成 STA、AP、AP+STA、冷启动、Gadget 和长期压力验收后才能发布。

---

## 3. 当前案例的已知环境

| 项目 | 已确认值 |
| --- | --- |
| 主板 | Radxa ROCK 3A |
| SoC | RK3568 |
| 架构 | ARM64 / AArch64 |
| 无线网卡 PCI ID | `10ec:b852` |
| 运行内核 | `5.10.160-12-rk356x` |
| 内核构建标识 | `#fd78925da` |
| 内核构建日期 | 2023-08-29 |
| 驱动包 | `8852be-dkms` |
| DKMS 版本 | `1.15.10.0.5.0-3` |
| 模块文件 | `/lib/modules/5.10.160-12-rk356x/updates/dkms/8852be.ko.xz` |
| 已采集模块 SHA256 | `da78f6e7ba0cf10e29ad4bc199604ece188a5919a141f8e6bb91bc4e107226da` |
| AP 配置 autoconnect | `no` |
| `teslabox-ap.service` | `disabled`、`inactive/dead`、`NRestarts=0` |

必须注意：上述 SHA256 只能标识案例中的模块文件，不能单独证明文件正确或损坏。必须与可信构建产物比较后才能用于完整性判断。

---

## 4. 原始崩溃证据

关键内核日志如下：

```text
RTKM: rtkm_prealloc_init
RTKM: rtkm_init_phy
RTKM: rtkm_init_phy: memory expansion:1
RTKM: rtkm_prealloc_init: done ret=0
rtl8852be 0000:01:00.0: enabling device (0000 -> 0003)
BUG: spinlock bad magic on CPU#1, systemd-udevd/284
lock: 0xffff80000b489080, .magic: 00000000, .owner: <none>/-1, .owner_cpu: 0
```

这段日志可以证明：

1. PCIe 控制器已经枚举出 RTL8852BE，而不是“系统完全看不到网卡”。
2. `8852be` 模块已经实际执行；`systemd-udevd` 只是触发模块自动加载的进程上下文，不是根因。
3. 驱动在 NetworkManager 建立 STA/AP 连接之前已经进入内核探测流程。
4. RTKM 初始化日志已经出现，并且函数返回 `0`。
5. 随后内核锁调试检测到 `.magic=00000000`。
6. AP 当时没有启用，因此修改热点 SSID、信道或 AP 脚本不能修复这个内核崩溃。

`.magic=00000000` 通常说明以下至少一种情况存在：

- 被使用的锁从未按运行内核要求初始化；
- 锁所在内存被清零或覆盖；
- 驱动编译时看到的 `spinlock_t` 布局与运行内核实际布局不同；
- 锁对象的地址或生命周期已经失效。

仅凭这一行不能确定具体是哪一个锁。

---

## 5. 为什么 `modinfo rtkm` 报错不是新问题

案例中执行：

```text
modinfo: ERROR: Module rtkm not found.
```

这不是缺少依赖，也不是需要再安装一个名为 `rtkm` 的包。

Radxa `8852be-dkms` 版本 `1.15.10.0.5.0-3` 的源码中：

- `src/platform/arm_rk3188.mk` 把 `CONFIG_RTKM` 设置为 `y`；
- `src/Makefile` 在 `CONFIG_RTKM=y` 时加入 `core/rtw_prealloc.o`；
- 同时定义 `CONFIG_RTKM_BUILT_IN`；
- `rtkm_prealloc_init()` 因而被直接编入 `8852be.ko`。

只有 `CONFIG_RTKM=m` 时才会生成独立的 `rtkm.ko`。当前构建是内置模式，因此：

```bash
modinfo rtkm
```

报找不到模块完全符合源码设计。

不要尝试从未知来源下载 `rtkm.ko`。独立加载一个版本、内核配置或符号不匹配的模块会进一步增加内核崩溃风险。

---

## 6. 为什么 `dpkg-query` 查不到模块路径

案例中出现：

```text
dpkg-query: no path found matching pattern
/lib/modules/5.10.160-12-rk356x/updates/dkms/8852be.ko.xz
```

这通常也是正常现象。

`8852be-dkms` Debian 包直接管理的是：

- `/usr/src/8852be-<版本>/` 下的驱动源码；
- DKMS 注册信息；
- 构建和安装钩子。

`/lib/modules/<内核版本>/updates/dkms/8852be.ko.xz` 是 DKMS 针对本机内核编译后生成的产物，不一定被 dpkg 记录为包内静态文件。因此应使用：

```bash
dpkg-query -W -f='${db:Status-Abbrev} ${Package} ${Version}\n' 8852be-dkms
dkms status
modinfo 8852be
```

确认包、DKMS 注册和实际模块，而不是用 `dpkg-query -S` 是否认模块来源。

---

## 7. 为什么官方配件仍然可能出问题

Radxa 官方资料把 Wireless Module A8（RTL8852BE）列为 ROCK 3A 支持的无线模块，Linux 支持表也同时列出了 A8 V1.0 和 V2.0。

但“官方支持”不能自动覆盖每一种软件组合：

- ROCK 3A 使用 Rockchip BSP 内核，而不是纯主线内核；
- 当前案例的内核和驱动构建于 2023 年；
- 当前使用的是树外 DKMS 驱动；
- TeslaBox 需要的不只是普通 STA，还需要 5 GHz AP 和可能的 AP+STA 并发；
- 锁调试选项、内核头文件、`Module.symvers` 和驱动编译参数都会影响二进制兼容性；
- 同一块硬件在不同 Radxa OS 版本上可能使用不同驱动。

Radxa 当前文档说明：

- Radxa OS Bookworm 使用 `rtw89`；
- Radxa OS Bullseye 使用旧 `8852be` 驱动；
- 旧驱动提供更多特性，文档还给出了开启 `CONFIG_CONCURRENT_MODE` 的方法。

这正是 TeslaBox 必须单独验证 AP+STA 的原因。普通 Wi-Fi 能连接，不等于并发 AP、Gadget 和车载长期运行可靠。

另外，A8 V1.0 和 V2.0 的 Wi-Fi PCI ID 都是 `10ec:b852`。更换另一张相同型号的 A8 通常仍会加载同一驱动，不能视为有效修复。

也不要把 `8852be-dkms` 从 `1.15.10.0.5.0-3` 升级到 `1.15.10.0.5.0-4` 就视为已经修复。Radxa 的 `-4` 发布记录没有声明修复本问题所见的坏锁或内核崩溃。Agent 必须比较实际驱动源码、构建配置和模块 SHA256，并完成真机验收。

---

## 8. 与 TeslaBox、AP 和 Gadget 的边界

### 8.1 不是 AP 启动造成

案例取证显示：

```text
AP autoconnect=no
teslabox-ap.service=disabled
ActiveState=inactive
SubState=dead
NRestarts=0
```

而 `8852be` 在 udev 处理 PCI 设备时已经崩溃。因此：

- 停止 TeslaBox Go 服务不能阻止模块自动加载；
- 关闭网页中的 AP 开关不能阻止模块探测；
- 修复 AP 信道、BSSID 或 NetworkManager 密码不能修复该崩溃；
- 把 `teslabox-ap.service` 改成更晚启动也不能修复驱动初始化。

### 8.2 TeslaBox 安装包不包含该驱动

当前 TeslaBox 首装脚本安装 NetworkManager、`iw`、`pciutils` 等通用系统依赖，但没有安装或分发：

- `8852be-dkms`
- `8852bu-dkms`
- `rtkm`
- 任意 RTL8852BE `.ko` 文件

因此不能把模块来源直接归因于 TeslaBox 主程序更新。仍需检查系统 APT 操作是否间接触发了一个已经注册的 DKMS 模块重建，但这与“TeslaBox 包内携带驱动”是两件不同的事。

### 8.3 与 ROCK 3A Gadget 安全问题分别处理

ROCK 3A 已知存在 OTG 供电时序风险。无线驱动修复期间必须保持以下约束：

- 使用独立、稳定的 Type-C 电源；
- 初次验证无线驱动时断开连接电脑或车机的 OTG 线；
- 不要把无线驱动崩溃和 OTG 反向供电崩溃混为同一问题；
- 无线驱动通过基础验证后，才加入 Gadget 联合测试。

---

## 9. 最高优先级：先让设备可启动

### 9.1 安全要求

执行以下操作前：

1. 关闭设备；
2. 拔掉 DC/Type-C 电源；
3. 拔掉 OTG；
4. 等待至少 10 秒；
5. 取下 M.2 E-Key 网卡；
6. 再给设备上电。

禁止带电插拔 M.2 PCIe 网卡。

### 9.2 建立可恢复的模块隔离

在拔卡启动的系统上执行：

```bash
printf '%s\n' \
  'blacklist 8852be' \
  'install 8852be /bin/false' |
sudo tee /etc/modprobe.d/teslabox-rtl8852be-quarantine.conf >/dev/null

sudo depmod -a
sudo update-initramfs -u -k "$(uname -r)"
modprobe -n -v 8852be
```

最后一条命令必须显示类似：

```text
install /bin/false
```

`blacklist` 主要阻止按别名自动加载；`install 8852be /bin/false` 还会阻止普通显式 `modprobe 8852be`。两者都应保留。

### 9.3 如果已经显示 `install /bin/false` 但插卡冷启动仍崩溃

这只能证明当前根文件系统中的 modprobe 规则生效，不能证明启动使用的 initramfs 已经包含该规则。

为了进行一次更强的诊断隔离，可以把模块文件改名。此操作可恢复，不要直接删除：

```bash
kver="$(uname -r)"
module="/lib/modules/$kver/updates/dkms/8852be.ko.xz"

sudo test -f "$module"
sudo mv "$module" "$module.disabled"
sudo depmod -a "$kver"
sudo update-initramfs -u -k "$kver"
```

检查所有可能仍可加载的副本：

```bash
find "/lib/modules/$kver" -type f -name '*8852be*.ko*'
sudo lsinitramfs "/boot/initrd.img-$kver" | sed -n '/8852be\.ko/p'
modprobe -n -v 8852be
```

预期结果：

- `find` 最多只看到后缀为 `.disabled` 的备份；
- initramfs 中不再包含 `8852be.ko`；
- `modprobe` 仍显示 `install /bin/false`。

如果 `/boot/initrd.img-$kver` 不存在，必须先确认实际启动项和 initramfs 路径，禁止猜测文件名后覆盖 `/boot`。

### 9.4 冷启动验证

确认模块隔离后：

```bash
sudo poweroff
```

完全断开电源和 OTG，等待至少 10 秒，再安装 M.2 网卡并上电。

验证结果解释：

| 结果 | 说明 |
| --- | --- |
| 插卡后稳定启动，且没有 `8852be`/`RTKM` 日志 | 基本确认崩溃依赖该驱动 |
| 仍崩溃，日志仍包含 `RTKM` | 模块仍从其他位置或其他 initramfs 被加载 |
| 仍崩溃，但完全没有 `8852be`/`RTKM` | 转查 PCIe、电源、复位时序或硬件 |
| 更换另一张相同 A8 后日志相同 | 不能证明两张卡都坏，更支持软件组合问题 |

隔离后 RTL8852BE 的 Wi-Fi 暂时不可用，这是预期行为。

---

## 10. 标准取证命令

所有诊断应在拔掉网卡或确认原模块已被隔离的情况下执行。

### 10.1 内核、驱动和 DKMS

```bash
uname -a
cat /proc/cmdline

dkms status
dpkg-query -W -f='${db:Status-Abbrev} ${Package} ${Version}\n' \
  8852be-dkms 8852bu-dkms rtk-btusb-dkms 2>&1

modinfo 8852be
modinfo -F filename 8852be
modinfo -F vermagic 8852be

sha256sum "/lib/modules/$(uname -r)/updates/dkms/8852be.ko.xz" 2>&1
readlink -f "/var/lib/dkms/8852be/1.15.10.0.5.0-3/source"
```

包名可能因镜像不同而变化。某个 `dpkg-query` 项不存在不应中断其他取证。

### 10.2 运行内核与构建配置

```bash
readlink -f "/lib/modules/$(uname -r)/build"

sed -n \
  '/^CONFIG_DEBUG_SPINLOCK=/p;
   /^CONFIG_DEBUG_LOCK_ALLOC=/p;
   /^CONFIG_LOCKDEP=/p;
   /^CONFIG_PROVE_LOCKING=/p;
   /^CONFIG_MODVERSIONS=/p' \
  "/boot/config-$(uname -r)" 2>/dev/null

sed -n \
  '/^CONFIG_DEBUG_SPINLOCK=/p;
   /^CONFIG_DEBUG_LOCK_ALLOC=/p;
   /^CONFIG_LOCKDEP=/p;
   /^CONFIG_PROVE_LOCKING=/p;
   /^CONFIG_MODVERSIONS=/p' \
  "/lib/modules/$(uname -r)/build/.config" 2>/dev/null

zcat /proc/config.gz 2>/dev/null |
sed -n \
  '/^CONFIG_DEBUG_SPINLOCK=/p;
   /^CONFIG_DEBUG_LOCK_ALLOC=/p;
   /^CONFIG_LOCKDEP=/p;
   /^CONFIG_PROVE_LOCKING=/p;
   /^CONFIG_MODVERSIONS=/p'
```

必须比较三份配置。重点检查：

- 运行内核是否启用 `CONFIG_DEBUG_SPINLOCK`；
- DKMS 构建目录的 `.config` 是否一致；
- `CONFIG_MODVERSIONS` 是否一致；
- 构建目录是否来自内核 `fd78925da` 对应的完整源码或头文件；
- 如果启用了 `CONFIG_MODVERSIONS`，是否具备匹配的 `Module.symvers`。

仅执行 `make modules_prepare` 不一定能生成匹配的 `Module.symvers`。需要符号版本时，应使用与运行内核完全对应的完整构建产物或官方头文件包。

### 10.3 DKMS 构建日志

```bash
find /var/lib/dkms/8852be -type f -name make.log -print

sed -n '1,240p' \
  /var/lib/dkms/8852be/1.15.10.0.5.0-3/build/make.log 2>/dev/null
```

成功构建后，DKMS 可能已经清理 `build` 目录。此时应保留下一次重建的完整日志。

### 10.4 启动和崩溃日志

```bash
journalctl --list-boots --no-pager

sudo journalctl -b -1 -k --no-pager |
sed -n '/RTKM/p;/8852be/p;/spinlock/p;/Call trace/p;/Kernel panic/p;/Oops/p'

sudo journalctl -b 0 -k --no-pager |
sed -n '/RTKM/p;/8852be/p;/spinlock/p;/Call trace/p;/Kernel panic/p;/Oops/p'

sudo find /sys/fs/pstore -maxdepth 1 -type f -print -exec sed -n '1,240p' {} \;
```

不要只截取 `BUG` 一行。完整调用栈、寄存器、模块列表和最后 200 行内核日志对定位具体锁非常重要。

### 10.5 PCIe 与模块别名

在隔离驱动后插卡能够稳定启动时执行：

```bash
lspci -nnk
lspci -vv -s 01:00.0

modprobe -R 'pci:v000010ECd0000B852sv*sd*bc*sc*i*'

find "/lib/modules/$(uname -r)" -type f \
  \( -name '*8852be*.ko' -o \
     -name '*8852be*.ko.gz' -o \
     -name '*8852be*.ko.xz' -o \
     -name '*8852be*.ko.zst' \)
```

如果实际 PCI 地址不是 `01:00.0`，以 `lspci -nn` 输出为准。

---

## 11. 建议生成的完整诊断包

下面的脚本不读取 NetworkManager 密码，也不导出 TeslaBox 密钥。应在驱动隔离状态下执行：

```bash
stamp="$(date +%Y%m%d-%H%M%S)"
out="/tmp/rtl8852be-diagnosis-$stamp"
mkdir -p "$out"

{
  date -Is
  uname -a
  cat /proc/cmdline
  uptime
} >"$out/system.txt" 2>&1

{
  lspci -nnk
  printf '\n=== PCI 01:00.0 ===\n'
  lspci -vv -s 01:00.0
} >"$out/pci.txt" 2>&1

{
  dkms status
  dpkg-query -W -f='${db:Status-Abbrev} ${Package} ${Version}\n' \
    8852be-dkms 8852bu-dkms rtk-btusb-dkms
  modinfo 8852be
  printf '\n=== modprobe dry run ===\n'
  modprobe -n -v 8852be
  printf '\n=== module aliases ===\n'
  modprobe -R 'pci:v000010ECd0000B852sv*sd*bc*sc*i*'
} >"$out/modules.txt" 2>&1

{
  printf '%s\n' '=== running config ==='
  zcat /proc/config.gz 2>/dev/null |
    sed -n '/^CONFIG_DEBUG_SPINLOCK=/p;
            /^CONFIG_DEBUG_LOCK_ALLOC=/p;
            /^CONFIG_LOCKDEP=/p;
            /^CONFIG_PROVE_LOCKING=/p;
            /^CONFIG_MODVERSIONS=/p'

  printf '%s\n' '=== boot config ==='
  sed -n '/^CONFIG_DEBUG_SPINLOCK=/p;
          /^CONFIG_DEBUG_LOCK_ALLOC=/p;
          /^CONFIG_LOCKDEP=/p;
          /^CONFIG_PROVE_LOCKING=/p;
          /^CONFIG_MODVERSIONS=/p' \
    "/boot/config-$(uname -r)" 2>/dev/null

  printf '%s\n' '=== build config ==='
  sed -n '/^CONFIG_DEBUG_SPINLOCK=/p;
          /^CONFIG_DEBUG_LOCK_ALLOC=/p;
          /^CONFIG_LOCKDEP=/p;
          /^CONFIG_PROVE_LOCKING=/p;
          /^CONFIG_MODVERSIONS=/p' \
    "/lib/modules/$(uname -r)/build/.config" 2>/dev/null

  printf '%s\n' '=== build link ==='
  readlink -f "/lib/modules/$(uname -r)/build"
} >"$out/kernel-config.txt" 2>&1

sudo journalctl --list-boots --no-pager >"$out/boots.txt" 2>&1
sudo journalctl -b -1 -k --no-pager >"$out/kernel-previous.txt" 2>&1
sudo journalctl -b 0 -k --no-pager >"$out/kernel-current.txt" 2>&1

find /var/lib/dkms/8852be -type f -name make.log -print \
  >"$out/dkms-build-logs.txt" 2>&1

while IFS= read -r log; do
  printf '\n===== %s =====\n' "$log" >>"$out/dkms-build-logs.txt"
  sed -n '1,320p' "$log" >>"$out/dkms-build-logs.txt"
done < <(find /var/lib/dkms/8852be -type f -name make.log -print 2>/dev/null)

sudo find /sys/fs/pstore -maxdepth 1 -type f -print \
  >"$out/pstore-index.txt" 2>&1

tar -C /tmp -czf "$HOME/rtl8852be-diagnosis-$stamp.tar.gz" \
  "$(basename "$out")"

printf '诊断包：%s\n' "$HOME/rtl8852be-diagnosis-$stamp.tar.gz"
```

如果默认用户 Shell 不支持 `< <(...)`，Agent 应把 DKMS 日志循环改写为兼容 `/bin/sh` 的形式，不能因此跳过日志收集。

---

## 12. 根因验证路线

### 12.1 验证 A：内核头文件或配置不匹配

这是最高优先级验证项，因为它可以解释：

- 运行内核检查到自旋锁 magic；
- 模块初始化代码却没有写入运行内核期待的 magic；
- 模块仍能通过普通 `vermagic` 检查并开始运行；
- 崩溃发生在模块刚进入初始化阶段。

Agent 必须核对：

1. `/lib/modules/$(uname -r)/build` 指向什么目录；
2. 该目录 `.config` 与运行内核配置是否一致；
3. 内核源码或头文件是否对应 `fd78925da`；
4. `Module.symvers` 是否存在并匹配；
5. DKMS 最近一次构建发生在何时，由哪个内核包触发；
6. `modinfo -F vermagic 8852be` 与 `uname -r` 是否一致；
7. 是否发生过相同内核版本、不同内容的头文件包覆盖。

如果发现不匹配：

- 不要在错误头文件上继续尝试编译参数；
- 获取与运行内核完全一致的 Radxa BSP 源码、配置和符号；
- 重新构建驱动；
- 使用新的驱动包版本和新 SHA256；
- 保留旧模块和旧 initramfs 的可启动回滚路径。

### 12.2 验证 B：关闭 RTKM

Radxa `1.15.10.0.5.0-3` 源码提交为：

```text
c0c830527f3b607c985286f1f196fefe53eed407
```

源码中：

```make
# src/platform/arm_rk3188.mk
CONFIG_RTKM = y
EXTRA_CFLAGS += -DCONFIG_CONCURRENT_MODE
```

建议先制作测试补丁：

```diff
-CONFIG_RTKM = y
+CONFIG_RTKM = n
```

不要同时删除：

```make
EXTRA_CFLAGS += -DCONFIG_CONCURRENT_MODE
```

这样可以优先隔离 RTKM 预分配路径，同时保留驱动的并发模式编译选项。关闭 RTKM 是否影响吞吐、内存占用和 AP+STA，必须通过真机测试确认。

推荐的源码准备流程：

```bash
git clone https://github.com/radxa-pkg/8852be-dkms.git
cd 8852be-dkms
git checkout c0c830527f3b607c985286f1f196fefe53eed407
git status --short
```

Agent 应完成以下工作：

1. 修改 `src/platform/arm_rk3188.mk`；
2. 使用独立版本，例如 `1.15.10.0.5.0-3+teslabox1`；
3. 在 `debian/changelog` 记录“关闭 ROCK 3A RTKM 以验证 spinlock 崩溃”；
4. 构建 `.deb`，记录源码提交、补丁和构建日志；
5. 确认目标机 DKMS 使用匹配的内核构建目录；
6. 在拔卡状态安装测试包；
7. 保持黑名单，不立即探测硬件；
8. 检查新模块路径、版本、`vermagic` 和 SHA256；
9. 完整断电后插卡进行受控测试。

不能只以“编译通过”作为修复成功。关闭 RTKM 后如果仍出现 `spinlock bad magic`，说明：

- 崩溃锁可能不属于 RTKM；
- 或者内核头文件/配置仍不匹配；
- 或者存在内存覆盖、PCIe/DMA 或其他驱动缺陷。

### 12.3 验证 C：保留 RTKM 并定位具体锁

只有取得完整调用栈、串口日志或可复现环境后才适合进行这一方向。

Agent 应：

1. 通过 UART 串口捕获从内核启动到崩溃的完整日志；
2. 保留未裁剪的 `Call trace`、PC、LR 和模块偏移；
3. 使用对应的未剥离 `8852be.ko`、`vmlinux` 和符号表解析地址；
4. 确认坏锁属于 RTKM、PCI IRQ、设备对象还是其他路径；
5. 检查锁初始化、释放、重复初始化和对象生命周期；
6. 增加最小必要诊断，不要在生产板上启用大量会改变时序的调试功能；
7. 修复后同时验证开启和关闭 RTKM 的行为差异。

日志中 `rtkm_prealloc_init: done ret=0` 之后才出现崩溃，因此不能仅凭日志顺序认定坏锁就是 `rtkm_phy_list[i].lock`。

### 12.4 验证 D：迁移到 `rtw89`

Radxa 当前文档说明 Bookworm 使用 `rtw89`，这可以作为中长期方案，但不能直接把一份单独的 `rtw89_8852be.ko` 塞进 ROCK 3A 的 5.10 BSP。

如选择该路线，必须：

- 回移完整、相互匹配的 `rtw89` core、PCI 和 8852BE 芯片支持；
- 使用 ROCK 3A 当前 BSP 的 cfg80211/mac80211 接口验证；
- 配套正确固件；
- 避免与旧 `8852be` 同时绑定 `10ec:b852`；
- 验证 5 GHz AP；
- 验证监管域和非 DFS 信道；
- 验证是否支持项目需要的 AP+STA 并发；
- 验证内存占用和吞吐；
- 验证 Gadget、MPP、RGA 和系统电源管理没有回归。

不能为了获得主线 `rtw89` 而盲目升级到任意 6.x 内核。ROCK 3A 的 Gadget、MPP、RGA、设备树和电源管理都依赖 BSP，内核替换必须有独立启动项和完整板端验收。

---

## 13. 测试驱动安装流程

以下流程假设 Agent 已经生成可信测试包，并且当前设备在拔卡状态稳定运行。

### 13.1 安装前

```bash
uname -a
modprobe -n -v 8852be
dkms status
```

必须满足：

- 当前仍为隔离状态；
- 具备有线、USB 网络共享或本地终端中的至少一种救援方式；
- 保留当前模块、initramfs 和安装包备份；
- 明确回滚命令。

### 13.2 安装后但插卡前

检查：

```bash
dkms status
modinfo 8852be
modinfo -F filename 8852be
modinfo -F vermagic 8852be
sha256sum "$(modinfo -F filename 8852be)"
```

确认：

- DKMS 状态是针对当前内核的 `installed`；
- `vermagic` 与当前内核一致；
- 模块来自测试版本；
- 测试模块不是之前改名保存的旧文件；
- 构建日志没有隐含失败或回退。

### 13.3 受控解除隔离

只有在测试模块检查通过后，才允许：

```bash
sudo rm -f /etc/modprobe.d/teslabox-rtl8852be-quarantine.conf
sudo depmod -a
sudo update-initramfs -u -k "$(uname -r)"
```

如果此前存在：

```text
8852be.ko.xz.disabled
```

不要把旧模块恢复到新模块路径上。应把它作为备份保留在不会被 `depmod` 识别的位置。

然后：

```bash
modprobe -n -v 8852be
```

此时应指向明确的新测试模块。随后执行：

```bash
sudo poweroff
```

完全断电、断开 OTG、安装网卡，再进行冷启动。

---

## 14. 回滚流程

测试驱动必须提供与安装同等清晰的回滚流程。回滚应在拔掉网卡后执行。

基本步骤：

1. 恢复隔离配置；
2. 更新 `depmod` 和 initramfs；
3. 卸载测试 DKMS 版本；
4. 恢复原包或保留无线禁用状态；
5. 验证 `modprobe -n -v 8852be`；
6. 完整断电后再决定是否插卡。

重新建立隔离：

```bash
printf '%s\n' \
  'blacklist 8852be' \
  'install 8852be /bin/false' |
sudo tee /etc/modprobe.d/teslabox-rtl8852be-quarantine.conf >/dev/null

sudo depmod -a
sudo update-initramfs -u -k "$(uname -r)"
modprobe -n -v 8852be
```

如果没有网络，不要轻易 `apt purge 8852be-dkms`。保留包和源码更便于恢复；隔离模块比删除整个包更可逆。

---

## 15. 必须完成的验收矩阵

### 15.1 内核基础稳定性

- [ ] 禁用驱动、插卡冷启动至少 10 次，无崩溃。
- [ ] 测试驱动、插卡冷启动至少 30 次，无崩溃。
- [ ] 每次检查 `journalctl -k`，无 `BUG`、`Oops`、`panic`、坏锁或 lockdep 报告。
- [ ] 断电重启和正常重启都通过。
- [ ] 连续运行至少 24 小时。

### 15.2 STA

- [ ] 连接 2.4 GHz WPA2。
- [ ] 连接 5 GHz WPA2。
- [ ] 验证 WPA3 或混合模式。
- [ ] 路由器重启后自动恢复。
- [ ] 热点临时消失后不高频扫描、不反复重置设备。
- [ ] 不锁死已经消失的 BSSID。

### 15.3 AP

- [ ] 2.4 GHz AP。
- [ ] 5 GHz 非 DFS AP。
- [ ] 自动信道不向 NetworkManager 写入非法信道 `0`。
- [ ] AP 可以从设置中心关闭并在重启后保持关闭。
- [ ] 没有 AP 能力时安全降级，不影响 TeslaUSB。

### 15.4 AP+STA

- [ ] 驱动明确报告有效接口组合。
- [ ] 单射频限制下 AP 跟随上游信道。
- [ ] 不支持并发时在 STA 和 AP 之间安全互斥。
- [ ] 上游断开和恢复不会触发内核异常。
- [ ] AP 切换期间无无限接口创建/删除循环。

检查能力：

```bash
iw phy
iw dev
nmcli -t -f DEVICE,TYPE,STATE,CONNECTION device status
```

### 15.5 TeslaBox 和 Gadget 联合验收

无线驱动基础测试通过后，再连接 Gadget：

- [ ] 启动时先保持 OTG 断开。
- [ ] 系统稳定后连接车机或测试主机。
- [ ] Gadget 能枚举为预期速度。
- [ ] Gadget 断开、重新连接不会影响无线驱动。
- [ ] 创建快照和播放视频期间 AP/STA 不掉线。
- [ ] RKMPP、RGA、FFmpeg 并发运行时无内核错误。
- [ ] 系统重启、服务重启和真正内核重启被正确区分。

---

## 16. 是否应该换网卡

### 不建议

- 换另一张相同的 RTL8852BE/A8；
- 在没有驱动验收结果时让用户随机购买 M.2 网卡；
- 只根据“Linux 能识别”选择替代网卡；
- 用桌面 Linux 的 STA 体验推断 ROCK 3A 的 AP+STA 能力。

### 可以作为临时联网手段

- 有线网络；
- Android 手机 USB 网络共享；
- 已在当前 Radxa 5.10 BSP 上验证过的 USB 无线网卡。

### 可以考虑正式换型的条件

候选网卡必须在 ROCK 3A 上验证：

- 对应内核驱动和固件可长期维护；
- 2.4/5 GHz STA；
- 5 GHz AP；
- AP+STA 并发或明确的安全互斥能力；
- 吞吐满足多路视频播放；
- 冷启动、断电和车载电源环境稳定；
- 不破坏 Gadget、MPP、RGA 和休眠/唤醒。

如果这些项目没有通过，换卡只是把已知问题替换成未知问题。

---

## 17. TeslaBox 项目应增加的保护

驱动崩溃发生在 TeslaBox 进程启动之前，因此 Go 代码不能在运行时拦截内核探测。保护必须覆盖安装、系统镜像和更新层。

建议 Agent 为项目增加：

1. 首装前记录 `uname -r`、`lspci -Dn`、`dkms status` 和 `modinfo 8852be`。
2. 识别 `10ec:b852 + 5.10.160-12-rk356x + 8852be 1.15.10.0.5.0-3` 风险组合。
3. 不自动安装、升级或重建 RTL8852BE DKMS。
4. 如果未来分发修补驱动，单独签名、单独版本、单独回滚，不能混在 Go 二进制里静默替换。
5. 安装程序发现无线不可用时继续保留有线和 TeslaUSB 基础功能。
6. AP 服务只在无线接口和 AP 能力存在时运行。
7. 网页明确显示“驱动已隔离”，而不是笼统显示“AP 不可用”。
8. 更新前检查可恢复网络路径，避免用户同时失去 STA 和 AP。
9. 系统级驱动更新必须要求冷启动验收，不能只做服务重启。
10. 发布检查必须验证驱动包非空、SHA256、内核版本范围和回滚包。

首装脚本的预检只能保护已经能够进入安装程序的系统。如果官方镜像首次启动时就自动加载坏驱动，必须在镜像、initramfs 或启动参数层提前隔离。

---

## 18. Agent 的禁止事项

处理本问题的 Agent 不得：

- 带电插拔 M.2 PCIe 网卡；
- 在原驱动仍可能崩溃时直接执行 `modprobe 8852be`；
- 把 `modinfo rtkm` 报错当成缺少依赖；
- 从未知来源下载单独的 `rtkm.ko`；
- 只删除 `.ko.xz` 而不更新 `depmod` 和 initramfs；
- 没有救援网络时直接清除所有 DKMS 包；
- 盲目升级到任意 6.x 内核；
- 覆盖唯一可启动的内核或 initramfs；
- 同时加载旧 `8852be` 和 `rtw89` 驱动绑定同一 PCI ID；
- 只验证 STA 就宣布适合 TeslaBox；
- 因为 AP 脚本曾经有问题，就把本次内核坏锁归因于 AP；
- 因为编译通过，就宣布内核驱动已修复；
- 在没有完整调用栈时声称已经定位到某一把锁。

---

## 19. Agent 最终应交付的内容

修复任务完成时，必须交付：

1. 根因报告，明确“已证实”和“推断”。
2. 完整原始崩溃日志和测试日志。
3. 驱动源码仓库、提交 ID 和补丁。
4. 精确内核源码或头文件来源。
5. 构建配置、`Module.symvers` 来源和构建日志。
6. 带独立版本号的 ARM64 Debian/DKMS 包。
7. 包和模块 SHA256。
8. 安装脚本。
9. 回滚脚本。
10. initramfs 更新和检查结果。
11. STA、AP、AP+STA、Gadget 和 24 小时压力测试报告。
12. 至少一台故障板和一台正常板的对比结果。

只有全部完成后，才能把驱动加入 TeslaBox 正式更新渠道。

---

## 20. 推荐决策树

```text
拔卡能稳定启动？
  ├─ 否 -> 不是单纯 RTL8852BE 问题，先查电源、存储和系统
  └─ 是
      |
      v
隔离 8852be 后，插卡能稳定启动？
  ├─ 否，且仍有 RTKM/8852be 日志
  |    -> 查错误 initramfs、第二份模块或直接 insmod
  |
  ├─ 否，且没有任何 8852be 日志
  |    -> 查 PCIe 供电、复位时序、卡槽和硬件
  |
  └─ 是
      |
      v
运行内核配置与 DKMS 构建配置一致？
  ├─ 否 -> 修正构建环境后重编
  └─ 是
      |
      v
关闭 RTKM 的同源驱动是否稳定？
  ├─ 是 -> 继续 AP+STA/Gadget/压力验收
  └─ 否 -> 获取完整调用栈，定位其他锁或评估 rtw89
```

---

## 21. 参考资料

- [Radxa ROCK 3A 官方产品说明](https://docs.radxa.com/rock3/rock3a)
- [Radxa Wireless Module A8 官方说明与兼容表](https://docs.radxa.com/accessories/network/wireless-a8)
- [Radxa 8852be-dkms 官方源码仓库](https://github.com/radxa-pkg/8852be-dkms)
- [Radxa 8852be-dkms 发布记录](https://github.com/radxa-pkg/8852be-dkms/releases)

本案例使用的 `1.15.10.0.5.0-3` 发布说明包含“修复以 DKMS 构建时的 RTKM 支持”。这说明 RTKM 与 DKMS 的组合本身就是该版本的关键变更点，但仍不能在缺少完整调用栈时把它直接等同于本次崩溃的唯一根因。
