#!/bin/sh
set -eu

SNAPSHOT_ROOT="${1:-/run/teslabox/snapshots}"
REPORT_PATH="${2:-/tmp/teslabox-latest-media-report.txt}"
FFPROBE="${TESLABOX_FFPROBE_PATH:-/opt/teslabox/media/bin/ffprobe}"

if [ "$(id -u)" -ne 0 ]; then
    echo "请使用 sudo 运行该脚本" >&2
    exit 1
fi

if [ ! -x "${FFPROBE}" ]; then
    echo "ffprobe 不可执行：${FFPROBE}" >&2
    exit 1
fi

LATEST_SNAPSHOT="$(find "${SNAPSHOT_ROOT}" -mindepth 1 -maxdepth 1 -type d -print | sort | tail -n 1)"
if [ -z "${LATEST_SNAPSHOT}" ]; then
    echo "没有可用的只读媒体快照" >&2
    exit 1
fi

LATEST_FILE="$(find "${LATEST_SNAPSHOT}" -type f -name '*.mp4' -printf '%f\n' | sort | tail -n 1)"
GROUP_PREFIX="$(printf '%s' "${LATEST_FILE}" | sed -E 's/-(front|back|left_repeater|right_repeater|left_pillar|right_pillar)\.mp4$//')"
if [ -z "${LATEST_FILE}" ] || [ "${GROUP_PREFIX}" = "${LATEST_FILE}" ]; then
    echo "无法从最新录像识别六路文件前缀" >&2
    exit 1
fi

# 固定视角顺序让不同批次报告可以直接对比，不依赖文件系统遍历顺序。
{
    echo "snapshot=$(basename "${LATEST_SNAPSHOT}")"
    echo "group=${GROUP_PREFIX}"
    for CAMERA in front back left_repeater right_repeater left_pillar right_pillar; do
        VIDEO="$(find "${LATEST_SNAPSHOT}" -type f -name "${GROUP_PREFIX}-${CAMERA}.mp4" -print -quit)"
        echo
        echo "[${CAMERA}]"
        if [ -z "${VIDEO}" ]; then
            echo "missing=true"
            continue
        fi
        echo "file=$(basename "${VIDEO}")"
        "${FFPROBE}" -v error -select_streams v:0 \
            -show_entries stream=codec_name,profile,width,height,pix_fmt,has_b_frames,r_frame_rate,avg_frame_rate,time_base,start_pts,start_time,duration_ts,duration,bit_rate,nb_frames \
            -show_entries format=format_name,duration,size,bit_rate \
            -of default=noprint_wrappers=1 "${VIDEO}"
    done
} > "${REPORT_PATH}"

chmod 0644 "${REPORT_PATH}"
echo "最新六路录像报告已写入 ${REPORT_PATH}"
