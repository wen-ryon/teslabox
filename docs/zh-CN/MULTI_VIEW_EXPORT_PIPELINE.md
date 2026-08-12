# TeslaBox 多视角合并导出逻辑

本文说明 TeslaBox 当前“合并视角导出”的完整实现思路，重点面向 Rockchip RK3568 / ROCK 3A、Tesla H.264 行车记录仪文件和四路合成场景。内容按实际代码整理，可作为另一个 TeslaBox 实现的设计参考。

> 先说结论：输入是 H.264 时，合并导出必须解码，因为不同画面要经过裁剪、缩放和像素级合成。TeslaBox 的高保真导出链路是 `H.264 -> h264_rkmpp 硬解 -> scale_rkrga 缩放 -> xstack 合成 -> hevc_rkmpp 硬编 -> H.265/HEVC MP4`。它不是先转成四份中间视频再合成，也不会在最终分段拼接时重新编码。

## 1. 两种“导出”必须先区分

TeslaBox 有两种性质完全不同的导出方式。

### 1.1 合并剪辑导出

- `mode=clip`
- 输出单个 MP4。
- 支持精确起止时间、单路或多路布局、时间和 Tesla SEI 仪表叠加。
- 必须解码和重新编码。
- 高保真与单路原画档使用 HEVC；流畅、标准档使用 H.264。

### 1.2 多路原画导出

- `mode=source`
- 输出 ZIP，其中包含未经转码的原始 MP4、清单和离线播放器。
- 不做像素级合并，不解码，也不重新编码。
- 适用于要求保留每路原始画质，并允许在电脑上同时播放多路的场景。

“一个 MP4 内显示四路画面”和“保留四份原始视频”是两个目标。前者必然涉及解码、合成和重新编码；后者可以直接复制原文件，但它不是一条合成后的视频流。

## 2. 总体处理链路

合并剪辑的完整数据流如下：

```text
导出请求
  -> 固定只读媒体快照
  -> 建立统一的绝对录像时间线
  -> 校验范围、布局、视角和磁盘空间
  -> 按 60 秒上限及源文件边界规划分段
  -> 串行处理每个分段
       -> 每个输入使用 h264_rkmpp 硬解
       -> 按绝对时间 trim，并将 PTS 归零
       -> 统一帧率
       -> 使用 scale_rkrga 缩放到布局格子尺寸
       -> 下载为 NV12 系统内存帧
       -> 缺口补帧或补黑画面
       -> xstack 一次合成所有视角
       -> 可选 ASS 时间/仪表叠加
       -> hevc_rkmpp 一次编码为分段 MP4
  -> concat demuxer + -c copy 无重编码拼接
  -> +faststart 生成最终 MP4
```

Go 服务只负责时间线计算、参数生成、任务排队、进度和文件生命周期。像素帧不进入 Go 代码逐帧处理，真正的视频处理都在一个 FFmpeg 滤镜图中完成。

## 3. 为什么 H.264 合并后导出 H.265 必须“先解再编”

H.264 和 H.265 都是有帧间依赖的压缩格式。四路 H.264 文件中的每一路都是独立码流，压缩数据不能直接按屏幕坐标拼成一个新画面。要得到四宫格，必须执行：

1. 解码四路 H.264，得到四组像素帧。
2. 将四路帧对齐到同一时间点和同一帧率。
3. 按目标格子尺寸缩放。
4. 把四张图像放到一张输出画布上。
5. 将新画布重新编码成 H.264 或 H.265。

所以当前高保真链路确实是“解 H.264，再编 H.265”，但解码、缩放和编码均优先使用 Rockchip 硬件路径：

| 阶段 | TeslaBox 当前实现 | 说明 |
| --- | --- | --- |
| 输入解码 | `h264_rkmpp` | Rockchip MPP H.264 硬解 |
| 硬件帧格式 | `drm_prime` | 解码帧先保留在 DRM PRIME 硬件表面 |
| 缩放 | `scale_rkrga` | 使用 RGA 调整每路尺寸 |
| 合成前格式 | `hwdownload,format=nv12` | 当前 `xstack`/ASS 链路需要系统内存帧 |
| 多路布局 | `xstack` | 一次将所有视角放入目标画布 |
| 高保真编码 | `hevc_rkmpp` | Rockchip MPP HEVC 硬编 |
| 封装 | MP4 + `hvc1` | 提高本地播放器对 HEVC MP4 的识别兼容性 |

这里不是完全零拷贝：`scale_rkrga` 后仍有一次 `hwdownload`，而 `xstack` 和 ASS 叠加运行在系统内存帧上。四路或六路时，内存带宽和软件合成仍可能成为瓶颈，不能因为解码器和编码器是硬件实现就假设一定达到实时速度。

## 4. 请求归一化与固定视角顺序

客户端只提交结构化选项，不允许直接传入 FFmpeg 参数。服务端根据布局覆盖视角顺序，避免客户端顺序不同造成画面错位。

当前布局对应关系：

| 布局 | 视角顺序 | 画面含义 |
| --- | --- | --- |
| `composite4a` | `front, back, left_repeater, right_repeater` | 2×2 四宫格 |
| `composite4b` | `front, right_repeater, back, left_repeater` | 前视大图在上，三路小图在下 |
| `composite3` | `front, right_repeater, left_repeater` | 前视大图在上，两路侧后在下 |
| `composite6` | `left_pillar, front, right_pillar, left_repeater, back, right_repeater` | 3×2 六宫格 |
| `single` | 客户端选择的一路 | 单视角 |

这一步还会校验：

- 开始时间和持续时间必须位于完整会话内。
- 至少一个所选视角在导出范围内有真实录像。
- `original` MP4 只允许单路；多路原画应使用 ZIP 方式。
- 时间、SEI 和显示位置的组合必须合法。
- 输入路径必须来自服务端持有的只读快照，不能由客户端指定任意文件。

## 5. 绝对时间线是正确合并的核心

不能简单地把四个文件都从第 0 秒开始播放。不同摄像头的分钟文件可能有数百毫秒偏移，也可能缺少某一分钟。如果忽略偏移，会出现：

- 左右视角与前视不同步。
- 某一路缺片时，后面的画面整体提前。
- 跨分钟处重复帧、跳帧或画面错位。
- 叠加的真实录像时间与画面不一致。

TeslaBox 为每个输入保存：

```text
Camera      摄像头名称
Path        只读快照中的文件路径
RecordedAt  该文件在压缩后会话时间线上的开始时间
WallClockAt 文件对应的真实录像时间
Duration    文件有效时长
```

导出范围转换为绝对时间：

```text
exportStart = sessionStart + startSeconds
exportEnd   = exportStart + durationSeconds
```

对每个摄像头、每个源文件分别求交集：

```text
start     = max(exportStart, video.RecordedAt, cursor)
end       = min(exportEnd, video.RecordedAt + video.Duration)
trimStart = start - video.RecordedAt
duration  = end - start
gap       = start - cursor
```

其中 `cursor` 表示该视角已经覆盖到的绝对时间。它能阻止重叠源文件重复进入输出，也能精确发现前置和中间缺口。

单个输入进入滤镜图时，逻辑近似为：

```text
[输入]
  trim=start=<源文件内偏移>:duration=<交集时长>
  -> setpts=PTS-STARTPTS
  -> fps=<目标帧率>
  -> 必要时 tpad 补起始缺口
  -> scale_rkrga
  -> hwdownload
  -> format=nv12
```

同一视角在一个处理段内若有多个连续输入，则先用滤镜 `concat=n=N:v=1:a=0` 接成该视角的局部时间线。末尾不足时使用 `tpad=stop_mode=clone` 冻结最后一帧，最后再次 `trim` 到分段的精确时长并归零 PTS。

如果某一路在整个分段都不存在，不删除这个格子，而是生成等尺寸、等帧率、等时长的黑画面。这样其他视角不会前移，布局和绝对时间保持稳定。

## 6. 为什么需要分段处理

Tesla 文件通常按分钟切片。一个很长的导出范围如果一次把所有文件都作为 FFmpeg 输入，会创建大量解码上下文。Rockchip MPP 的并行上下文和板端内存有限，跨分钟时甚至可能同时打开前一分钟和后一分钟的多路文件，导致后半段逐路掉帧或硬件解码失败。

当前分段规则：

- 每段最长 60 秒。
- 源视频的开始和结束也是候选边界。
- 相距不超过 500 毫秒的边界合并，避免制造大量极短分段。
- 每个分段、每个摄像头只保留与该段重叠时间最长的一个源文件。
- 边缘少量空缺由补帧逻辑维持时间连续。
- 单个 FFmpeg 分段最多允许 12 个解码输入；正常四路段通常只需 4 个，正常六路段通常只需 6 个。
- 所有分段串行执行，不并发抢占 MPP/RGA。

“只保留覆盖最长的一个源文件”很重要。分钟边界在各摄像头上经常不完全一致；如果一个六路分段同时把边界前后的文件全部打开，瞬时可能超过 10 个 MPP 解码器。TeslaBox 宁可冻结数百毫秒的边缘画面，也不让整段在硬件资源耗尽后持续掉帧。

简化伪代码：

```go
boundaries := mergeNearbyBoundaries(
    every60Seconds,
    allSelectedVideoStarts,
    allSelectedVideoEnds,
    500*time.Millisecond,
)

for each interval in boundaries {
    for each camera {
        input[camera] = videoWithLongestOverlap(camera, interval)
    }
    runOneFFmpegProcess(interval, input)
}

concatSegmentsWithoutReencoding()
```

## 7. 四路画布布局

Tesla 源画面的宽高比约为 `724:469`，约等于 1.544:1。布局优先接近源比例，不把每一路强制压成 16:9。

高保真四路布局尺寸如下。

### 7.1 四路 A：2×2

最终可见画布为 `1680×1088`，宽高均满足 16 像素对齐。

```text
+--------------------+--------------------+
| front              | back               |
| 840×544 @ 0,0      | 840×544 @ 840,0    |
+--------------------+--------------------+
| left_repeater      | right_repeater     |
| 840×544 @ 0,544    | 840×544 @ 840,544  |
+--------------------+--------------------+
```

对应 `xstack` 布局：

```text
0_0|840_0|0_544|840_544
```

### 7.2 四路 B：前视大图 + 底部三路

最终可见画布为 `1264×1088`。

```text
+------------------------------------------------+
| front 1264×816 @ 0,0                           |
+----------------+---------------+---------------+
| right_repeater | back          | left_repeater |
| 422×272        | 420×272       | 422×272       |
| @ 0,816        | @ 422,816     | @ 842,816     |
+----------------+---------------+---------------+
```

宽度的两三个像素差异用于让总画布满足硬件编码对齐，同时使每格尽量接近源宽高比。不要先生成一个未对齐画布，再在右侧或底部补黑边；补齐区可能被硬件编码器当成可见像素。

## 8. 一个分段内的 FFmpeg 滤镜图

下面以四路 A、高保真、四个源文件均完整覆盖当前分段为例。它是便于移植的最小化示意；生产代码还会加入每路独立的 `trimStart`、持续时间、缺口处理和安全路径校验。

```bash
ffmpeg -hide_banner -loglevel warning -nostdin -y \
  -hwaccel rkmpp -hwaccel_output_format drm_prime -c:v h264_rkmpp -i front.mp4 \
  -hwaccel rkmpp -hwaccel_output_format drm_prime -c:v h264_rkmpp -i back.mp4 \
  -hwaccel rkmpp -hwaccel_output_format drm_prime -c:v h264_rkmpp -i left_repeater.mp4 \
  -hwaccel rkmpp -hwaccel_output_format drm_prime -c:v h264_rkmpp -i right_repeater.mp4 \
  -filter_complex "\
[0:v]trim=start=0:duration=60,setpts=PTS-STARTPTS,fps=30,scale_rkrga=w=840:h=544:format=nv12:force_original_aspect_ratio=disable,hwdownload,format=nv12[v0];\
[1:v]trim=start=0:duration=60,setpts=PTS-STARTPTS,fps=30,scale_rkrga=w=840:h=544:format=nv12:force_original_aspect_ratio=disable,hwdownload,format=nv12[v1];\
[2:v]trim=start=0:duration=60,setpts=PTS-STARTPTS,fps=30,scale_rkrga=w=840:h=544:format=nv12:force_original_aspect_ratio=disable,hwdownload,format=nv12[v2];\
[3:v]trim=start=0:duration=60,setpts=PTS-STARTPTS,fps=30,scale_rkrga=w=840:h=544:format=nv12:force_original_aspect_ratio=disable,hwdownload,format=nv12[v3];\
[v0][v1][v2][v3]xstack=inputs=4:layout=0_0|840_0|0_544|840_544:fill=black:shortest=0[out]" \
  -map "[out]" -an \
  -c:v hevc_rkmpp \
  -rc_mode CQP -qp_init 18 -qp_min 16 -qp_max 20 \
  -tag:v hvc1 -g 60 -fps_mode cfr -t 60 \
  -movflags +faststart -progress pipe:1 -nostats \
  segment.mp4
```

关键点：

- 每个 `-i` 前都要声明 `h264_rkmpp`，不要只给第一个输入启用硬解。
- `shortest=0` 防止最短的一路提前结束整个合成输出。
- 输出不包含音频，使用 `-an`。
- 高保真使用低 QP 的 CQP，而不是给四路复杂画面套一个过低的固定码率。
- GOP 为两秒，即 30 FPS 时 `-g 60`。
- `-progress pipe:1` 用于任务进度，不需要扫描日志猜测完成比例。
- `hvc1` 只是 MP4 样本条目标记；实际编码器仍是 `hevc_rkmpp`。

## 9. 分段完成后的合并

每个分段必须使用完全一致的编码器、像素格式、分辨率、帧率和 HEVC 参数。完成后写入 concat 清单：

```text
file '/data/exports/.task.tmp.part-0000.mp4'
file '/data/exports/.task.tmp.part-0001.mp4'
file '/data/exports/.task.tmp.part-0002.mp4'
```

最终命令：

```bash
ffmpeg -hide_banner -loglevel warning -nostdin -y \
  -f concat -safe 0 -i segments.txt \
  -map 0:v:0 -an -c copy \
  -movflags +faststart final.mp4
```

这里必须是 `-c copy`。每个分段已经完成 H.265 编码，最终步骤只重新封装，不能再执行第二遍 H.265 编码。合并进度越过前一个分段后，可以立即删除已经消费的临时分段，使峰值磁盘空间接近“最终文件 + 一个分段”，而不是“最终文件 + 所有分段”。

如果只有一个分段，直接将临时分段原子重命名为最终临时文件，不运行 concat。

## 10. 时间与 Tesla SEI 叠加

叠加不是导出主链路的必要步骤，性能排查时应先关闭，确认基础四路合成速度，再单独测量叠加成本。

当前策略：

- 只叠加时间时，每秒生成一个字幕区间。
- 叠加 Tesla SEI 仪表时按 100 毫秒，即 10 Hz，生成状态区间。
- 优先从前视文件读取 SEI；没有前视时选第一个可用视角。
- 时间依据绝对录像时间线计算，不依据板子的当前时区直接偏移。
- 每个总时间线字幕区间会换算成分段内局部时间。
- FFmpeg 使用 ASS 滤镜和固定字体目录渲染。

SEI 解析会读取源文件元数据，可能增加导出前准备时间。因此性能日志应把“SEI 准备耗时”和“FFmpeg 编码耗时”分别记录，不能把二者统称为“分析耗时”。

## 11. 为什么四路可能需要 200 秒

“四路用了 200 秒”本身还不能判断快慢，必须同时给出输出时长。例如：

| 输出时长 | 墙钟耗时 | 墙钟吞吐 | 含义 |
| ---: | ---: | ---: | --- |
| 60 秒 | 200 秒 | `60/200 = 0.30x` | 生成一秒视频约耗时 3.33 秒 |
| 120 秒 | 200 秒 | `120/200 = 0.60x` | 生成一秒视频约耗时 1.67 秒 |
| 200 秒 | 200 秒 | `1.00x` | 实时速度 |

建议统一记录两个指标：

```text
wall_speed = 输出视频时长 / 从启动 FFmpeg 到退出的墙钟时间
ffmpeg_speed = FFmpeg progress 输出或日志报告的 speed
```

二者可能不同，因为墙钟时间还包含进程启动、输入探测、硬件上下文初始化、MP4 收尾和磁盘同步。项目曾记录三秒六路样本墙钟耗时约 11 秒，按墙钟直接计算约为 `0.27x`；旧记录中的 `0.44x` 更可能是 FFmpeg 运行阶段的瞬时/报告速度，不能把这两个口径混用。

四路 200 秒常见原因按优先级排列如下。

### 11.1 实际走了软件编解码

最常见的严重退化是：

- 输入实际由软件 `h264` 解码，而不是 `h264_rkmpp`。
- 缩放实际使用软件 `scale`，而不是 `scale_rkrga`。
- 输出使用 `libx265`，而不是 `hevc_rkmpp`。

仅写 `-hwaccel rkmpp` 不等于整条链路已硬件化。必须保存并检查 FFmpeg 的完整实际命令和启动日志。

### 11.2 先“分析”四路，又重新完整解码四路

不要为了获得时长、分辨率或帧率，先逐路运行一遍完整解码。基础容器信息使用 `ffprobe` 读取头部即可。需要解析 Tesla SEI 时也应独立计时，并且只读取一条首选视角，不要把四路都完整分析后再进入合成。

TeslaBox 的主合成不存在“四路先各自产生中间视频，再读取四份中间视频合成”的步骤。四路原文件直接进入同一个 FFmpeg 滤镜图，只发生一次最终编码。

### 11.3 同时打开了过多分钟文件

如果四路跨一分钟边界，同时打开前后两批文件，瞬间会变成八个解码上下文；六路会变成十二个。应先按源文件边界分段，并为每个摄像头只选择当前分段覆盖最长的一份文件。

### 11.4 系统内存合成和 DDR 带宽成为瓶颈

当前链路在 RGA 缩放后通过 `hwdownload` 将四路 NV12 帧交给 `xstack`。这一步会消耗 DDR 带宽，`xstack` 本身也需要 CPU/内存搬运。可以分别测试：

1. 单路硬解 + RGA + HEVC 硬编。
2. 四路硬解 + RGA，但只输出其中一路。
3. 四路硬解 + RGA + `xstack`。
4. 在第 3 步基础上增加 ASS。

四组数据能区分瓶颈位于硬件编解码、并行输入、合成还是字幕层。

### 11.5 存储读取或写入不足

四路输入来自同一存储设备，最终文件又写回该设备。USB 盘、快照文件系统、低速 TF 卡、温控降频或同步写入都可能放大耗时。测量时同时记录：

- 输入和输出所在设备。
- 实际读写吞吐与 I/O 等待。
- CPU 利用率和频率。
- SoC 温度以及是否发生降频。
- FFmpeg 报告的处理速度是否稳定，还是运行一段时间后持续下降。

### 11.6 画布、帧率和叠加目标过高

四路 A 高保真输出不是普通 1080p，而是 `1680×1088 @ 30 FPS`；四路 B 是 `1264×1088 @ 30 FPS`。如果将每个格子都保留原始 `1280×960` 后再拼成 `2560×1920`，需要处理的输出像素约为四路 A 的 2.69 倍，性能不可直接比较。

## 12. 建议加入的可观测性

每个任务至少记录：

```text
task_id
layout
output_duration_seconds
segment_index / segment_count
segment_duration_seconds
decoder_input_count
input_codec / input_resolution / input_fps
output_codec / output_resolution / output_fps
overlay_enabled
ffmpeg_started_at / ffmpeg_finished_at
wall_elapsed_seconds
wall_speed
final_file_bytes
```

每个 FFmpeg 分段建议从 `-progress pipe:1` 解析：

```text
frame
fps
out_time_ms
speed
progress
```

不要只打印总任务耗时。总耗时至少拆成：

```text
快照和输入准备
SEI/字幕准备
各分段 FFmpeg 编码
最终 -c copy 拼接
文件重命名和清理
```

## 13. 板端确认命令

以下命令中的 FFmpeg 路径按实际安装位置调整。TeslaBox 默认媒体工具链通常位于 `/opt/teslabox/media/bin`。

### 13.1 查看输入真实参数

```bash
/opt/teslabox/media/bin/ffprobe \
  -v error \
  -select_streams v:0 \
  -show_entries stream=codec_name,width,height,pix_fmt,r_frame_rate,avg_frame_rate \
  -show_entries format=duration,size \
  -of default=noprint_wrappers=1 \
  front.mp4
```

### 13.2 确认硬件组件存在

```bash
/opt/teslabox/media/bin/ffmpeg -hide_banner -decoders 2>&1 | sed -n '/h264_rkmpp/p;/hevc_rkmpp/p'
/opt/teslabox/media/bin/ffmpeg -hide_banner -encoders 2>&1 | sed -n '/h264_rkmpp/p;/hevc_rkmpp/p'
/opt/teslabox/media/bin/ffmpeg -hide_banner -filters 2>&1 | sed -n '/scale_rkrga/p;/xstack/p;/ ass /p'
```

### 13.3 使用墙钟时间测量

```bash
/usr/bin/time -v /opt/teslabox/media/bin/ffmpeg <完整实际参数>
```

至少保留以下结果：

- `Elapsed (wall clock) time`
- `Percent of CPU this job got`
- `Maximum resident set size`
- `File system inputs/outputs`
- FFmpeg `speed` 和 `out_time_ms`
- 完整 FFmpeg 命令及完整错误输出

## 14. 推荐的性能 A/B 测试矩阵

使用完全相同的 60 秒真实录像片段，每组至少运行三次，第一次作为硬件初始化和文件缓存预热结果单独记录，不与后两次直接取平均。

| 组别 | 输入/滤镜/输出 | 目的 |
| --- | --- | --- |
| A | 单路 `h264_rkmpp -> hevc_rkmpp`，不缩放 | 测编解码基础上限 |
| B | 单路 + `scale_rkrga` | 测 RGA 成本 |
| C | 四路硬解 + RGA + `xstack` + HEVC | 测核心四路合成 |
| D | C + ASS 时间 | 测字幕成本 |
| E | C + SEI 仪表 ASS | 测 SEI 读取和高频字幕成本 |
| F | 四路软件 H.264 解码 + 软件缩放 + `libx265` | 仅作退化基线，不作为生产方案 |

每组固定：

- 相同源文件。
- 相同输出时长。
- 相同布局和画布。
- 相同帧率。
- 相同输入、输出磁盘。
- 相同温度起点或记录温度差。
- 禁止同时运行其他导出任务。

## 15. 任务调度、取消与资源保护

硬件视频单元是共享资源。TeslaBox 使用单工作进程串行处理导出任务，避免两个任务同时争抢 MPP/RGA。并发增加通常不会让两条任务都更快，反而更容易出现初始化失败、掉帧、OOM 或整体吞吐下降。

生产实现还应满足：

- 请求创建后持有只读快照租约，任务结束、失败或取消后才释放。
- 运行 FFmpeg 使用可取消的上下文；取消任务和服务退出必须终止子进程。
- 所有分段、ASS、concat 清单使用任务唯一临时文件名。
- 成功后原子重命名，失败时删除全部临时产物。
- 导出前按保守上界检查磁盘空间。
- 高保真 CQP 的码率随画面复杂度变化，空间估计不能直接按普通 8 Mbps CBR 计算。
- 慢于实时不等于进程失控；超时需要按输出时长放大。当前剪辑超时下限为 10 分钟，计算值约为 `输出时长 × 6 + 5 分钟`。

## 16. 移植时不要照抄的部分

`h264_rkmpp`、`hevc_rkmpp` 和 `scale_rkrga` 来自 Rockchip 定制 FFmpeg 工具链，不是所有发行版自带。移植前必须确认：

- FFmpeg 是否包含所需解码器、编码器和滤镜。
- 内核、MPP、RGA、DRM 驱动和 FFmpeg 构建版本是否匹配。
- 目标 SoC 是否也是 RK3568；其他 Rockchip 芯片的并发能力和对齐限制可能不同。
- 若目标不是 Rockchip，应替换为该平台的 VAAPI、V4L2 M2M、NVENC 等硬件路径，不能保留 RKMPP 参数。
- 输入是否始终为 H.264。当前命令明确指定 `h264_rkmpp`；若输入可能是 HEVC，应先探测编码格式并选择对应硬解码器。

同时，当前 `xstack` 前的 `hwdownload` 是已知性能边界。如果未来工具链支持稳定的硬件多路合成，可以针对纯硬件栈做 A/B，但必须验证时间对齐、缺口处理、输出像素格式以及长时间运行稳定性，不能仅凭短样本替换生产链路。

## 17. 给“四路 200 秒”问题的最短排查顺序

1. 先确认输出视频究竟是 60 秒、120 秒还是 200 秒，并计算墙钟倍速。
2. 保存完整实际 FFmpeg 命令，确认四个输入都明确使用 `h264_rkmpp`。
3. 确认缩放是 `scale_rkrga`，输出是 `hevc_rkmpp`，没有 `libx264` 或 `libx265`。
4. 确认没有先把四路各转一遍中间文件，也没有四路完整预解码“分析”。
5. 将测试范围固定为同一段 60 秒，关闭时间和 SEI 叠加。
6. 检查一次 FFmpeg 进程打开了几个输入；正常四路单分钟段应接近 4 个，而不是 8 个或更多。
7. 依次运行单路、单路缩放、四路合成 A/B，找出新增耗时发生在哪一层。
8. 记录 CPU、内存、I/O、温度、频率和 FFmpeg `speed` 曲线，判断是始终慢还是热机后变慢。
9. 基础链路确认后再打开 ASS 时间和 SEI 仪表，单独计算它们的增量成本。

如果朋友提供“完整 FFmpeg 命令、四个输入的 ffprobe 输出、目标画布/帧率、输出视频时长、墙钟 200 秒的 time 输出”，就可以把问题从猜测缩小到具体的解码、缩放、合成、编码或 I/O 阶段。

## 18. TeslaBox 当前相关代码位置

- `internal/api/handler.go`：导出 API、快照租约、会话到绝对时间线输入的转换。
- `internal/media/exporter.go`：请求校验、布局归一化、FFmpeg 参数、硬件编解码和缺口处理。
- `internal/media/export_segments.go`：60 秒分段、源边界合并、每路最长覆盖选择、无重编码拼接。
- `internal/media/layout.go`：Tesla 源比例和通用多视角布局。
- `internal/media/exporter_test.go`：硬件路径、布局尺寸、缺口、跨分钟和 HEVC 参数的回归测试。
