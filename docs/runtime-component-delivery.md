# 独立运行时交付

主程序仍使用现有的签名 ELF 在线更新格式。公开媒体、CarPlay、NAS 组件的固定版本、HTTPS 地址、原始字节长度和 SHA-256 编译进主 ELF，不从设备端动态读取 latest 清单。没有改变既有更新签名或 128 MiB 客户端上限。

## 运行行为

- USB 录像状态机和管理 HTTP 不等待组件下载。基础录像检查工具随首装保留。
- 启动时先恢复导出和上传任务的缓存引用，再启动缓存清理；媒体/NAS 工作进程等待所需组件。
- 同一摘要归档复用本机缓存，每次校验完整原字节后才安装；缺网时等待并重试，不重启整板。
- 下载使用有限超时和临时文件，错误摘要、长度、重定向和不安全文件不会进入安装器。未完成的下载重试整项归档，暂不支持断点续传。
- 媒体使用 `/opt/teslabox/media-versions/<sha256>`，保留既有媒体目录，预检后原子切换 `/opt/teslabox/media` 链接，兼容 FFmpeg 的固定 RPATH 和字体路径。旧主程序恢复实体目录时仍保留版本目录。
- NAS 使用 `/opt/teslabox/backup/versions/<sha256>/bin/rclone`，旧目录不覆盖。CarPlay 使用已有摘要版本目录和主程序摘要绑定的激活/恢复事务。
- 缓存和旧版本不自动删除，以免破坏主程序回滚；维护者在确认相关主程序不再使用后另行清理。
- 设置中心显示本机组件状态。依赖缺失的 API 返回 503/runtime_pending，原始录像下载、登录、核心管理和已有任务查询/取消保留；状态接口仍受现有登录保护。

## 发布输入

`-RuntimeDescriptor` 是维护者生成的 schema=1 JSON，必须恰好包含 media/carplay/nas 三项。每项包含 id/version/url/size/sha256/format，format 为 tar.gz/tar.gz/zip。不包含证书私钥、MFi 身份或 DNS API 密钥。

发布流程使用 `scripts/build-update-release.ps1` 的 RuntimeDescriptor/NASArchive 参数，并保持原有固定 commit、媒体/CarPlay 摘要及产品证书参数；脚本拒绝脏树和占位下载地址。componentdelivery 模式不嵌入三个大归档，公开描述作为旁证同时输出，但设备只信任主程序编译绑定的版本。

首装使用 `scripts/build-bootstrap-release.ps1` 的 RuntimeDescriptor/ExpectedDescriptorSHA256/CarPlayArchive/NASArchive 参数。描述必须与固定主 ELF 的内嵌原字节一致；预缓存工具使用同一描述编译。默认只携带 250272 字节基础 fsck 工具，可用 OfflineComponents 明确添加 media/carplay/nas 的完整离线归档。基础工具来自批准媒体包的原字节，安装器在替换主程序前真实执行版本自检。

首装和 OTA 都由主程序的 NewProduct 安装适配器激活组件。teslabox-runtime 只校验发布输入、输出编译描述或为离线输入预缓存，不能以自己的 ELF 摘要激活 CarPlay。

软件 MFi 与统一证书的私下分发保持独立。运行时公开地址的实际托管位置仍由维护者确定；本地 development.invalid 验证描述不能发布。

## 验证边界

Windows 单元测试、Node DOM 模型和 ARM64 交叉编译用于源码回归。Linux 的原子链接替换、文件所有权、flock、CarPlay Python fcntl 安装事务及 Debian ARM64 基础工具真实执行需要隔离 Linux 验证；随后仍需正式首装/旧版 OTA/USB/真实 iPhone 验收。语法通过、核心 healthz 成功和组件就绪不等价于车机实测。

完整发行包含 CarPlay、NAS 手动和自动上传、系统盘录像缓存、事件发现以及备份/导出/设置页面。本轮从已批准 NAS/事件候选逐文件导入，保留 CarPlay 和强制 Wi-Fi 重连代码；没有使用猜测的 Git 合并基点覆盖共享文件。
