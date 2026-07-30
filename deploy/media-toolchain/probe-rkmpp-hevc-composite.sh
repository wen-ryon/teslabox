#!/bin/sh
set -eu

# 该脚本固定六路布局，可选择软件或 Rockchip 硬件流水线并仅切换最终 RKMPP 编码器，便于复现和比较残块、速度与温度。
FFMPEG="${TESLABOX_FFMPEG_PATH:-/opt/teslabox/media/bin/ffmpeg}"
FFPROBE="${TESLABOX_FFPROBE_PATH:-/opt/teslabox/media/bin/ffprobe}"
INPUT="${1:?请提供一个原始 MP4 文件}"
OUTPUT="${2:?请提供输出 MP4 路径}"
DURATION="${TESLABOX_PROBE_SECONDS:-3}"
ENCODER="${TESLABOX_RKMPP_ENCODER:-hevc_rkmpp}"
PIPELINE="${TESLABOX_PROBE_PIPELINE:-software}"
GOP="${TESLABOX_PROBE_GOP:-60}"

if [ ! -x "${FFMPEG}" ] || [ ! -x "${FFPROBE}" ]; then
    echo "FFmpeg 或 ffprobe 不可执行" >&2
    exit 1
fi
if [ ! -f "${INPUT}" ]; then
    echo "输入视频不存在：${INPUT}" >&2
    exit 1
fi
case "${ENCODER}" in
    h264_rkmpp|hevc_rkmpp) ;;
    *) echo "不支持的 RKMPP 测试编码器：${ENCODER}" >&2; exit 1 ;;
esac
case "${PIPELINE}" in
    software|hardware) ;;
    *) echo "不支持的测试流水线：${PIPELINE}" >&2; exit 1 ;;
esac
case "${GOP}" in
    ''|*[!0-9]*|0) echo "GOP 必须是正整数" >&2; exit 1 ;;
esac

mkdir -p "$(dirname "${OUTPUT}")"
temperature_before="$(cat /sys/class/thermal/thermal_zone0/temp 2>/dev/null || printf 'unknown')"
started="$(date +%s)"

if [ "${PIPELINE}" = "hardware" ]; then
    filters="[0:v]trim=start=0:duration=${DURATION},setpts=PTS-STARTPTS,fps=30,scale_rkrga=w=834:h=540:format=nv12:force_original_aspect_ratio=disable,hwdownload,format=nv12[c0];[1:v]trim=start=0:duration=${DURATION},setpts=PTS-STARTPTS,fps=30,scale_rkrga=w=834:h=540:format=nv12:force_original_aspect_ratio=disable,hwdownload,format=nv12[c1];[2:v]trim=start=0:duration=${DURATION},setpts=PTS-STARTPTS,fps=30,scale_rkrga=w=834:h=540:format=nv12:force_original_aspect_ratio=disable,hwdownload,format=nv12[c2];[3:v]trim=start=0:duration=${DURATION},setpts=PTS-STARTPTS,fps=30,scale_rkrga=w=834:h=540:format=nv12:force_original_aspect_ratio=disable,hwdownload,format=nv12[c3];[4:v]trim=start=0:duration=${DURATION},setpts=PTS-STARTPTS,fps=30,scale_rkrga=w=834:h=540:format=nv12:force_original_aspect_ratio=disable,hwdownload,format=nv12[c4];[5:v]trim=start=0:duration=${DURATION},setpts=PTS-STARTPTS,fps=30,scale_rkrga=w=834:h=540:format=nv12:force_original_aspect_ratio=disable,hwdownload,format=nv12[c5];[c0][c1][c2][c3][c4][c5]xstack=inputs=6:layout=0_0|834_0|1668_0|0_540|834_540|1668_540:fill=black:shortest=1[stack];[stack]pad=w=ceil(iw/16)*16:h=ceil(ih/16)*16:x=0:y=0:color=black[out]"
else
    filters="[0:v]trim=start=0:duration=${DURATION},setpts=PTS-STARTPTS,fps=30,scale=w=834:h=540:flags=lanczos:force_original_aspect_ratio=disable,format=nv12[c0];[1:v]trim=start=0:duration=${DURATION},setpts=PTS-STARTPTS,fps=30,scale=w=834:h=540:flags=lanczos:force_original_aspect_ratio=disable,format=nv12[c1];[2:v]trim=start=0:duration=${DURATION},setpts=PTS-STARTPTS,fps=30,scale=w=834:h=540:flags=lanczos:force_original_aspect_ratio=disable,format=nv12[c2];[3:v]trim=start=0:duration=${DURATION},setpts=PTS-STARTPTS,fps=30,scale=w=834:h=540:flags=lanczos:force_original_aspect_ratio=disable,format=nv12[c3];[4:v]trim=start=0:duration=${DURATION},setpts=PTS-STARTPTS,fps=30,scale=w=834:h=540:flags=lanczos:force_original_aspect_ratio=disable,format=nv12[c4];[5:v]trim=start=0:duration=${DURATION},setpts=PTS-STARTPTS,fps=30,scale=w=834:h=540:flags=lanczos:force_original_aspect_ratio=disable,format=nv12[c5];[c0][c1][c2][c3][c4][c5]xstack=inputs=6:layout=0_0|834_0|1668_0|0_540|834_540|1668_540:fill=black:shortest=1[stack];[stack]pad=w=ceil(iw/16)*16:h=ceil(ih/16)*16:x=0:y=0:color=black[out]"
fi

set -- "${FFMPEG}" -hide_banner -loglevel warning -nostdin -y
if [ "${PIPELINE}" = "hardware" ]; then
    set -- "$@" -hwaccel rkmpp -hwaccel_output_format drm_prime -c:v h264_rkmpp -i "${INPUT}"
    set -- "$@" -hwaccel rkmpp -hwaccel_output_format drm_prime -c:v h264_rkmpp -i "${INPUT}"
    set -- "$@" -hwaccel rkmpp -hwaccel_output_format drm_prime -c:v h264_rkmpp -i "${INPUT}"
    set -- "$@" -hwaccel rkmpp -hwaccel_output_format drm_prime -c:v h264_rkmpp -i "${INPUT}"
    set -- "$@" -hwaccel rkmpp -hwaccel_output_format drm_prime -c:v h264_rkmpp -i "${INPUT}"
    set -- "$@" -hwaccel rkmpp -hwaccel_output_format drm_prime -c:v h264_rkmpp -i "${INPUT}"
else
    set -- "$@" -i "${INPUT}" -i "${INPUT}" -i "${INPUT}" -i "${INPUT}" -i "${INPUT}" -i "${INPUT}"
fi
set -- "$@" -filter_complex "${filters}" -map "[out]" -an -c:v "${ENCODER}"
if [ "${ENCODER}" = "hevc_rkmpp" ]; then
    set -- "$@" -tag:v hvc1
fi
set -- "$@" -rc_mode CQP -qp_init 18 -qp_min 16 -qp_max 20 -g "${GOP}" -fps_mode cfr -t "${DURATION}" -movflags +faststart -progress pipe:1 -nostats "${OUTPUT}"
"$@"

finished="$(date +%s)"
temperature_after="$(cat /sys/class/thermal/thermal_zone0/temp 2>/dev/null || printf 'unknown')"

printf 'elapsed_seconds=%s\n' "$((finished - started))"
printf 'temperature_before_millic=%s\n' "${temperature_before}"
printf 'temperature_after_millic=%s\n' "${temperature_after}"
printf 'pipeline=%s\n' "${PIPELINE}"
printf 'encoder=%s\n' "${ENCODER}"
printf 'gop=%s\n' "${GOP}"
"${FFPROBE}" -v error -select_streams v:0 \
    -show_entries stream=codec_name,profile,width,height,avg_frame_rate,pix_fmt,bit_rate,nb_frames:format=duration,size,bit_rate \
    -of default=noprint_wrappers=1 "${OUTPUT}"
