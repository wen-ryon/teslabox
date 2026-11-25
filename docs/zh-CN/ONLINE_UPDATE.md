# TeslaBox 签名在线更新

> 历史文档：本文描述当时的更新机制与发布流程，不保证适用于其他分支、后续版本或当前外部服务。项目已停止维护。

## 更新范围

在线更新仍只下载一个 ARM64 ELF，不从网络执行 Shell、DEB 维护脚本或任意安装命令。正式 ELF 可以内置与该版本绑定的媒体运行时；整个 ELF 的长度、SHA-256 和 Ed25519 签名通过后，内置媒体包才有机会被读取。

更新过程如下：

1. 管理页面下载 `latest.json`，限制清单大小并要求 HTTPS。
2. 设备使用内置公钥校验清单签名，同时核对通道、ROCK 3A 板型、ARM64 架构、程序长度和 SHA-256。
3. 程序下载到 TF 系统盘的私有目录，完成长度和摘要校验后才原子改名。
4. 用户点击“安装并重启”后，主服务先安全解绑 TeslaBox USB Gadget，再写入结构化请求并启动固定的 `teslabox-update.service`。
5. 独立助手再次验证长度、摘要和 ARM64 ELF，备份旧程序，原子替换后重启服务。
6. 新程序先启动 TeslaUSB 核心状态机，再检查内置媒体运行时。需要升级时，只允许从固定的 `opt/teslabox/media/` 前缀解包普通文件，拒绝路径逃逸、符号链接、超量文件和摘要不匹配。
7. 新媒体目录必须同时具备 H.264/HEVC RKMPP 编解码、RGA、ASS/subtitles 和固定 Noto CJK 字体，才会原子替换 `/opt/teslabox/media`。
8. `/healthz` 在 45 秒内未恢复时，助手自动恢复旧程序并再次启动服务；健康检查通过后由主流程提交系统重启，在新的 `boot_id` 中恢复 Gadget。

车机 USB 正在连接或存在排队、运行中的视频导出任务时，服务端会拒绝安装。下载和检查不受此限制。

在线更新只保留最近一次健康检查自动回滚备份：`/var/lib/teslabox-system/update-backups/teslabox.previous.backup`。人工部署脚本同样只保留 `/usr/local/bin/teslabox.backup`。这份备份用于安装失败自动恢复，不作为管理页面的指定版本来源；指定版本只来自维护者签名的历史目录。

## 一次性生成发布密钥

私钥只保存在维护者电脑的安全目录，不能提交到 Git、Gitee、设备或聊天记录。下面命令不会覆盖已有密钥文件：

```powershell
go run .\cmd\teslabox-update-manifest -generate-key `
  -private-key D:\TeslaBoxSecrets\update-private.key `
  -public-key D:\TeslaBoxSecrets\update-public.key
```

将 `update-public.key` 的单行内容配置到设备的 `TESLABOX_UPDATE_PUBLIC_KEY`。私钥只在生成 Release 时使用。

## 首次安装更新助手

正式启用网页更新前，需要通过现有 SSH 部署流程一次性安装独立助手和 systemd unit。Windows 构建 ARM64 文件：

```powershell
$version="0.1.0"
$commit=(git rev-parse --short=12 HEAD).Trim()
$env:GOOS="linux"
$env:GOARCH="arm64"
$env:CGO_ENABLED="0"
go build -trimpath -ldflags "-X github.com/wen-ryon/teslabox/internal/version.Version=$version -X github.com/wen-ryon/teslabox/internal/version.Commit=$commit" -o .\bin\teslabox-linux-arm64 .\cmd\teslabox
go build -trimpath -o .\bin\teslabox-update-helper-linux-arm64 .\cmd\teslabox-update-helper
$mainHash=(Get-FileHash -Algorithm SHA256 .\bin\teslabox-linux-arm64).Hash.ToLowerInvariant()
$helperHash=(Get-FileHash -Algorithm SHA256 .\bin\teslabox-update-helper-linux-arm64).Hash.ToLowerInvariant()
Set-Content -Encoding Ascii .\bin\teslabox-linux-arm64.sha256 "$mainHash  teslabox-linux-arm64"
Set-Content -Encoding Ascii .\bin\teslabox-update-helper-linux-arm64.sha256 "$helperHash  teslabox-update-helper-linux-arm64"
```

上传时分别命名为：

- `/tmp/teslabox.new` 与 `/tmp/teslabox.new.sha256`
- `/tmp/teslabox-update-helper.new` 与 `/tmp/teslabox-update-helper.new.sha256`
- `/tmp/teslabox-update.service`
- `/tmp/install-teslabox-update.sh`

然后在板端显式执行：

```bash
sudo /tmp/install-teslabox-update.sh
systemctl status teslabox.service --no-pager
systemctl cat teslabox-update.service
```

安装脚本只执行 `daemon-reload`，不会启用或立即启动更新 unit；它只会在用户从设置页确认安装时运行。

## Gitee Release 发布

先在 Gitee 建立公开仓库并创建版本 Release。假设版本为 `0.2.0`，附件地址已经确定：

```powershell
.\scripts\build-update-release.ps1 `
  -Version 0.2.0 `
  -PackageUrl "https://gitee.com/你的账号/teslabox/releases/download/v0.2.0/teslabox-linux-arm64" `
  -PrivateKey D:\TeslaBoxSecrets\update-private.key `
  -NotesFile .\release-notes\0.2.0.md
```

把 `dist\update\teslabox-linux-arm64`、`dist\update\latest.json` 和 `dist\update\manifest-0.2.0.json` 上传到该 Release。更新清单中已经包含包体摘要、大小、说明和签名，上传后不要手工修改。`manifest-0.2.0.json` 是该版本固定留档，后续生成历史目录时需要它；每次发布都应保留一份，不要只保留会被覆盖的 `latest.json`。

设备配置示例：

```ini
TESLABOX_UPDATE_MANIFEST_URL=https://gitee.com/你的账号/teslabox/releases/download/latest/latest.json
# 可选：维护者签名的历史主程序目录
TESLABOX_UPDATE_CATALOG_URL=https://gitee.com/你的账号/teslabox-releases/raw/main/stable/history.json
# 内测版使用独立目录；稳定版不会读取或安装其中的任何包。
TESLABOX_UPDATE_BETA_MANIFEST_URL=https://gitee.com/你的账号/teslabox-releases/raw/main/beta/latest.json
TESLABOX_UPDATE_BETA_CATALOG_URL=https://gitee.com/你的账号/teslabox-releases/raw/main/beta/history.json
TESLABOX_UPDATE_PUBLIC_KEY=这里填写Base64公钥
TESLABOX_UPDATE_CHANNEL=stable
TESLABOX_UPDATE_CHANNEL_STATE=/var/lib/teslabox-system/update-channel.json
TESLABOX_UPDATE_BOARD=radxa-rock-3a
```

如果 Gitee 无法为固定 `latest` 标签提供稳定附件地址，可以维护一个固定分支中的 `latest.json` 原始文件地址；包体仍放在带版本号的 Release 中。两种地址都必须使用 HTTPS。

## 稳定版与内测版

设置中心默认选择“稳定版”，它只读取 `stable/latest.json` 和 `stable/history.json`，并且设备会拒绝 `channel` 不是 `stable` 的清单。选择“内测版”后，设备才会读取 `beta/latest.json` 和 `beta/history.json`；切换会保存到系统盘、清空旧通道的待安装候选，并要求重新检查。下载中、已下载或安装中的更新不能切换通道。

发布预览包时，继续使用同一套离线私钥，但必须明确签为 `beta`，上传附件后把固定清单推送到 `teslabox-releases` 仓库的 `beta/` 目录：

```powershell
.\scripts\build-update-release.ps1 `
  -Version 0.1.30 `
  -Channel beta `
  -PackageUrl "https://gitee.com/你的账号/teslabox-releases/releases/download/v0.1.30-beta/teslabox-linux-arm64" `
  -PrivateKey D:\TeslaBoxSecrets\update-private.key `
  -NotesFile .\release-notes\0.1.30-beta.md

go run .\cmd\teslabox-update-catalog `
  -private-key D:\TeslaBoxSecrets\update-private.key `
  -channel beta `
  -manifest .\release-manifests\manifest-0.1.30-beta.json `
  -output .\dist\update\history.json
```

将本次 `latest.json` 和 `history.json` 分别放到 `beta/latest.json`、`beta/history.json`。正式发布时重新用 `-Channel stable` 为同一已验证包生成稳定清单，并更新 `stable/`；不要把 beta 清单复制到 stable 目录，也不要让稳定通道抓取 Gitee 的预览列表。

## 指定版本与回退

管理页面不会接受浏览器传入的下载 URL。用户选择历史版本时，设备会重新下载并校验维护者签名的 `history.json`，再从其中选择完全匹配的单个已签名清单，因此公开 Gitee 页面或中间人不能把任意二进制伪装成回退包。

历史目录只应包含你确认可与当前环境兼容的版本。它不会回退内核、MT7922 驱动或 systemd unit；带内置媒体运行时的版本会在启动时升级媒体组件，但回退主程序时不会主动降级已经验证可用的新版 FFmpeg/MPP/RGA。

维护者在发布后，收集各版本的固定清单，例如：

```powershell
go run .\cmd\teslabox-update-catalog `
  -private-key D:\TeslaBoxSecrets\update-private.key `
  -manifest .\release-manifests\manifest-0.1.6.json `
  -manifest .\release-manifests\manifest-0.1.7.json `
  -output .\dist\update\history.json
```

将 `history.json` 上传到一个固定 HTTPS 地址，例如 `teslabox-releases` 仓库的 `stable/history.json`。更新板子的 `TESLABOX_UPDATE_CATALOG_URL` 后重启 `teslabox.service`；设置中心点击“查看历史版本”即可查看、选择、下载并安装指定版本。`history.json`、其中每条清单和程序包会依次校验签名/摘要；维护者新增、删除或调整目录顺序后都必须重新生成并签名。

回退与普通更新共用 Gadget 自动维护流程：下载不影响车机，点击“回退并重启”才会暂时断开 USB Gadget，健康检查通过后自动重启恢复。回退前会保留当前主程序用于本次健康检查失败自动恢复。

## Gitee Release 附件留存

Gitee Release 附件有容量上限。稳定通道只保留最近 **5** 个可安装版本的附件；更早版本保留 Git Tag 和发布说明，但删除主程序、校验文件、固定清单和首装包附件。不要只删除附件而保留旧版 `history.json` 条目，否则页面会给用户展示无法下载的回退版本。

每次新版本通过测试后按下列顺序执行：

1. 上传新 Release 附件并完成公网下载校验。
2. 使用最近 5 个版本的固定 manifest 重新生成、签名并推送 `stable/history.json`，再更新 `stable/latest.json`。
3. 先运行预览，确认候选删除版本和附件无误：

```powershell
$env:GITEE_ACCESS_TOKEN = "<仅保存在当前终端的 Gitee Token>"
.\scripts\prune-gitee-release-assets.ps1
```

4. 预览无误后才执行删除；这个操作不可恢复：

```powershell
.\scripts\prune-gitee-release-assets.ps1 -Apply -Confirm:$false
```

脚本会再次读取本地 `stable/history.json`。只要待删除版本仍在历史目录中，它就会拒绝删除，避免发布顺序错误造成回退入口失效。

## 暂不支持

- 内核、MT7922 驱动和 systemd unit 的网页远程更新
- DEB、Shell 脚本和带安装钩子的压缩包
- 无人值守自动安装
- 增量包和跨板型更新

这些内容一旦需要升级，应继续使用明确的人工部署流程，不能借用主程序更新通道绕过确认。
