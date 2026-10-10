# CarPlay 发行内层组包工具

此目录是维护者的组包与合成测试工具，不是用户首装器，不执行签名、系统激活或联网下载。

## 固定输入

`build-runtime-package.py` 只读取显式审核清单中的普通文件。清单必须包含 schema=1、准确 RC version、approved_runtime=true、完整40位 release_commit 和 files；每个文件明确 path/source/role/sha256/mode。source 相对指定 source-root，path 是包内路径。内层禁止主程序、更新助手、外层 manifest 和设备私有材料。

公开 Python/Node 模块和依赖源必须从已审查提交取得；Node、原生 ELF、媒体等外部输入必须有独立固定摘要与来源证据。不要扫描主仓脏工作区，不使用历史程序包补漏，不从测试板导出配置或身份。

内层角色及路径由 internal/carplaybundle 安装契约约束，运行模块之间的相对路径必须保留。PACKAGE.json 中的发行提交与版本必须和随后嵌入它的主程序构建一致。内层自身不独立签名或发布，最终主 ELF 的签名保护其全部嵌入字节。

## 执行顺序

1. 完成源码范围审查、全部测试及公开运行时来源批准；提交本版源码、脚本和 release-notes/0.2.0-rc.1.md，推送 GitHub，固定最终提交。
2. 在干净发行树生成明确输入清单，填入最终提交并核对每项字节；以本工具的 `--package-kind inner` 生成一次 carplay-runtime.tar.gz。输出已存在时拒绝覆盖。
3. 为 inner 创建只含小写 SHA-256 的 sidecar。使用 build-update-release.ps1 的 ExpectedCommit、ExpectedMediaSHA256、CarPlayArchive 和 CarPlaySHA256，同时提供已获批产品证书参数及路由地址，以准确 RC 版本和 beta 通道构建、签名唯一主包。
4. bootstrap 复用同一主 ELF 与 helper，不重新构建主程序；提供 ExpectedCommit、ExpectedMainSHA256、ExpectedHelperSHA256、ExpectedMediaSHA256。固定安装脚本复用该 bootstrap，禁止覆盖已有产物。
5. 完成公网附件/清单/签名校验、真实旧助手 OTA 与回退、首装和硬件验收。产物生成不代表验收完成。stable 需用户另行授权。

## 本地合成测试

使用 `python -B scripts/carplay/test-packaging.py`。14 项测试覆盖摘要漂移、路径/链接/私有材料拒绝、角色和就绪辅助闭包、完整提交绑定、字节可复现、设置副本与精确回退。临时合成 ELF 不是可运行候选；测试通过不能替代 Debian 11 ARM64 的真实 import、服务启动、OTA 或 USB 验收。
