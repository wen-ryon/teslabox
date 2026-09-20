# 索引扫描进度

`GET /api/v1/storage` 沿用原鉴权，新增可选字段 `scan_progress`：

```json
{"scan_progress":{"stage":"scanning","discovered":128},"video_count":99}
```

`stage` 为 `preparing`（准备只读快照）、`scanning`（目录扫描）、`repairing`（修复副本）。`discovered` 为本轮实际发现的单路 MP4 数量，每 64 个文件及扫描结束时上报，不代表可播放数量。修复后重扫从零重新统计，不能将两轮累加。总数量未知，不显示百分比。

`video_count` 始终代表已发布索引；有旧快照时保留其数量，另行展示候选进度。无快照且尚未发现文件时显示破折号；成功发布空索引才显示 0。完成或失败后 `scan_progress` 消失，失败继续展示重试信息。

页面扫描中每 2 秒只读查询状态，不触发快照或系统指标采样；平时保留原有轮询。容量的 `total_bytes` 未知时不显示默认零值，使用不确定进度样式，并支持减少动态效果偏好。
