# TeslaBox 文档

项目已停止维护。当前主线是开发快照，发布版本可能来自其他分支或标签；历史计划、验收记录和外部服务不保证适用于当前环境。

## 使用和部署

- [历史首次安装教程](FIRST_INSTALL.md)：使用前核实外部下载、设备兼容性并备份数据。
- [历史签名在线更新说明](ONLINE_UPDATE.md)：描述当时的更新设计和发布流程。
- [部署说明](DEPLOYMENT.md)、[teslaboxctl AP 指南](TESLABOXCTL_AP_GUIDE.md)、[Wi-Fi 指南](TESLABOXCTL_WIFI_GUIDE.md)。

## 设计和接口

- [架构](ARCHITECTURE.md)、[TeslaUSB 需求](TESLAUSB_REQUIREMENTS.md)、[设置中心说明](SETTINGS_CENTER_HANDOFF.md)。
- [索引扫描进度](../scan-progress-handoff.md)、[录像时长接口](../duration-poll-handoff.md)、[持久化索引](../persistent-index-handoff.md)。
- [多视角导出](MULTI_VIEW_EXPORT_PIPELINE.md)、[存储空间保护](SPACE_GUARD.md)、[USB Gadget](USB_GADGET_SUPERSPEED.md)。
- [开发计划](DEVELOPMENT_PLAN.md)和[远程播放与导出计划](REMOTE_PLAYBACK_EXPORT_PLAN.md)是历史记录，不代表承诺继续开发。
- [RTL8852BE 内核崩溃排查资料](RTL8852BE内核崩溃根因分析及修复手册.md)保留了历史诊断思路；其中的假设和操作不代表当前驱动状态。

## 许可证与公开前审查

当前工作树自有代码的许可及第三方声明见[项目主页](../../README.md)和[版权与第三方项目](../../NOTICE.md)。实际发布版本和第三方组件可能适用不同许可；尚待解决的事项见[开源审查记录](../OPEN_SOURCE_REVIEW.md)。
