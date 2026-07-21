#!/bin/sh
set -eu

SNAPSHOT_ROOT="${1:-/run/teslabox/snapshots}"
WORK_ROOT="${2:-/tmp/teslabox-hybrid-benchmark}"
FFMPEG="${TESLABOX_FFMPEG_PATH:-/opt/teslabox/media/bin/ffmpeg}"
FFPROBE="${TESLABOX_FFPROBE_PATH:-/opt/teslabox/media/bin/ffprobe}"
SAMPLE_SECONDS="${TESLABOX_BENCHMARK_SECONDS:-30}"
REPORT_PATH="${WORK_ROOT}/report.txt"

if [ "$(id -u)" -ne 0 ]; then
    echo "请使用 sudo 运行该脚本" >&2
    exit 1
fi

LATEST_SNAPSHOT="$(find "${SNAPSHOT_ROOT}" -mindepth 1 -maxdepth 1 -type d -print | sort | tail -n 1)"
LATEST_FILE="$(find "${LATEST_SNAPSHOT}" -type f -name '*.mp4' -printf '%f\n' | sort | tail -n 1)"
GROUP_PREFIX="$(printf '%s' "${LATEST_FILE}" | sed -E 's/-(front|back|left_repeater|right_repeater|left_pillar|right_pillar)\.mp4$//')"
if [ -z "${LATEST_SNAPSHOT}" ] || [ -z "${LATEST_FILE}" ] || [ "${GROUP_PREFIX}" = "${LATEST_FILE}" ]; then
    echo "无法定位最新一组六路录像" >&2
    exit 1
fi

find_video() {
    camera="$1"
    video="$(find "${LATEST_SNAPSHOT}" -type f -name "${GROUP_PREFIX}-${camera}.mp4" -print -quit)"
    if [ -z "${video}" ]; then
        echo "缺少 ${camera} 视角" >&2
        exit 1
    fi
    printf '%s' "${video}"
}

FRONT="$(find_video front)"
BACK="$(find_video back)"
LEFT_REPEATER="$(find_video left_repeater)"
RIGHT_REPEATER="$(find_video right_repeater)"
LEFT_PILLAR="$(find_video left_pillar)"
RIGHT_PILLAR="$(find_video right_pillar)"

mkdir -p "${WORK_ROOT}"
find "${WORK_ROOT}" -mindepth 1 -delete

read_temperature() {
    highest=0
    for sensor in /sys/class/thermal/thermal_zone*/temp; do
        [ -r "${sensor}" ] || continue
        value="$(cat "${sensor}")"
        if [ "${value}" -gt "${highest}" ] 2>/dev/null; then
            highest="${value}"
        fi
    done
    printf '%s' "${highest}"
}

monitor_process() {
    pid="$1"
    samples="$2"
    while kill -0 "${pid}" 2>/dev/null; do
        cpu="$(ps -p "${pid}" -o %cpu= 2>/dev/null | tr -d ' ' || true)"
        rss="$(ps -p "${pid}" -o rss= 2>/dev/null | tr -d ' ' || true)"
        printf '%s cpu_percent=%s rss_kib=%s temp_millic=%s loadavg=%s\n' \
            "$(date -Iseconds)" "${cpu:-0}" "${rss:-0}" "$(read_temperature)" "$(cut -d ' ' -f 1 /proc/loadavg)" >> "${samples}"
        sleep 1
    done
}

run_case() {
    name="$1"
    shift
    output="${WORK_ROOT}/${name}.mp4"
    log="${WORK_ROOT}/${name}.log"
    samples="${WORK_ROOT}/${name}-samples.txt"
    started="$(date +%s)"
    temperature_before="$(read_temperature)"

    set +e
    "${FFMPEG}" -hide_banner -nostdin -y "$@" \
        -an -c:v h264_rkmpp -rc_mode CBR -b:v 3000k -maxrate 3000k -bufsize 6000k \
        -g 60 -fps_mode cfr -t "${SAMPLE_SECONDS}" -movflags +faststart "${output}" > "${log}" 2>&1 &
    ffmpeg_pid="$!"
    monitor_process "${ffmpeg_pid}" "${samples}" &
    monitor_pid="$!"
    wait "${ffmpeg_pid}"
    status="$?"
    wait "${monitor_pid}" 2>/dev/null
    set -e

    finished="$(date +%s)"
    {
        echo
        echo "[${name}]"
        echo "exit_code=${status}"
        echo "elapsed_seconds=$((finished - started))"
        echo "temperature_before_millic=${temperature_before}"
        echo "temperature_after_millic=$(read_temperature)"
        if [ -s "${samples}" ]; then
            awk '
                BEGIN { max_cpu=0; max_rss=0; max_temp=0 }
                {
                    for (i=1; i<=NF; i++) {
                        split($i, pair, "=")
                        if (pair[1] == "cpu_percent" && pair[2] > max_cpu) max_cpu=pair[2]
                        if (pair[1] == "rss_kib" && pair[2] > max_rss) max_rss=pair[2]
                        if (pair[1] == "temp_millic" && pair[2] > max_temp) max_temp=pair[2]
                    }
                }
                END {
                    printf "max_process_cpu_percent=%s\nmax_rss_kib=%s\nmax_temperature_millic=%s\n", max_cpu, max_rss, max_temp
                }
            ' "${samples}"
        fi
        if [ -s "${output}" ]; then
            "${FFPROBE}" -v error -select_streams v:0 \
                -show_entries stream=codec_name,width,height,avg_frame_rate,bit_rate,nb_frames:format=duration,size,bit_rate \
                -of default=noprint_wrappers=1 "${output}"
        fi
        echo "ffmpeg_progress=$(tr '\r' '\n' < "${log}" | sed -n '/frame=/p' | tail -n 1)"
        if [ "${status}" -ne 0 ]; then
            echo "ffmpeg_error_tail_begin"
            tail -n 40 "${log}"
            echo "ffmpeg_error_tail_end"
        fi
    } >> "${REPORT_PATH}"

    return "${status}"
}

{
    echo "snapshot=$(basename "${LATEST_SNAPSHOT}")"
    echo "group=${GROUP_PREFIX}"
    echo "sample_seconds=${SAMPLE_SECONDS}"
    echo "pipeline=mpp_decode+rga_scale+nv12_xstack+mpp_encode"
    echo "output=1280x720"
    echo "target_bitrate=3000k"
} > "${REPORT_PATH}"

# 在昂贵的缩放之前丢弃多余帧，并只把缩小后的 NV12 帧映射给一次 xstack。
run_case hybrid3_30 \
    -hwaccel rkmpp -hwaccel_output_format drm_prime -c:v h264_rkmpp -i "${FRONT}" \
    -hwaccel rkmpp -hwaccel_output_format drm_prime -c:v h264_rkmpp -i "${RIGHT_REPEATER}" \
    -hwaccel rkmpp -hwaccel_output_format drm_prime -c:v h264_rkmpp -i "${LEFT_REPEATER}" \
    -filter_complex "[0:v]fps=30,scale_rkrga=1280:480:format=nv12:force_original_aspect_ratio=disable,hwmap=mode=read,format=nv12[front];[1:v]fps=30,scale_rkrga=640:240:format=nv12:force_original_aspect_ratio=disable,hwmap=mode=read,format=nv12[right];[2:v]fps=30,scale_rkrga=640:240:format=nv12:force_original_aspect_ratio=disable,hwmap=mode=read,format=nv12[left];[front][right][left]xstack=inputs=3:layout=0_0|0_h0|w1_h0:fill=black:shortest=1[out]" \
    -map "[out]"

run_case hybrid4_30 \
    -hwaccel rkmpp -hwaccel_output_format drm_prime -c:v h264_rkmpp -i "${FRONT}" \
    -hwaccel rkmpp -hwaccel_output_format drm_prime -c:v h264_rkmpp -i "${BACK}" \
    -hwaccel rkmpp -hwaccel_output_format drm_prime -c:v h264_rkmpp -i "${RIGHT_REPEATER}" \
    -hwaccel rkmpp -hwaccel_output_format drm_prime -c:v h264_rkmpp -i "${LEFT_REPEATER}" \
    -filter_complex "[0:v]fps=30,scale_rkrga=640:360:format=nv12:force_original_aspect_ratio=disable,hwmap=mode=read,format=nv12[front];[1:v]fps=30,scale_rkrga=640:360:format=nv12:force_original_aspect_ratio=disable,hwmap=mode=read,format=nv12[back];[2:v]fps=30,scale_rkrga=640:360:format=nv12:force_original_aspect_ratio=disable,hwmap=mode=read,format=nv12[right];[3:v]fps=30,scale_rkrga=640:360:format=nv12:force_original_aspect_ratio=disable,hwmap=mode=read,format=nv12[left];[front][back][right][left]xstack=inputs=4:layout=0_0|w0_0|0_h0|w0_h0:fill=black:shortest=1[out]" \
    -map "[out]"

run_six_case() {
    fps="$1"
    run_case "hybrid6_${fps}" \
        -hwaccel rkmpp -hwaccel_output_format drm_prime -c:v h264_rkmpp -i "${FRONT}" \
        -hwaccel rkmpp -hwaccel_output_format drm_prime -c:v h264_rkmpp -i "${BACK}" \
        -hwaccel rkmpp -hwaccel_output_format drm_prime -c:v h264_rkmpp -i "${LEFT_REPEATER}" \
        -hwaccel rkmpp -hwaccel_output_format drm_prime -c:v h264_rkmpp -i "${RIGHT_REPEATER}" \
        -hwaccel rkmpp -hwaccel_output_format drm_prime -c:v h264_rkmpp -i "${LEFT_PILLAR}" \
        -hwaccel rkmpp -hwaccel_output_format drm_prime -c:v h264_rkmpp -i "${RIGHT_PILLAR}" \
        -filter_complex "[0:v]fps=${fps},scale_rkrga=426:360:format=nv12:force_original_aspect_ratio=disable,hwmap=mode=read,format=nv12[front];[1:v]fps=${fps},scale_rkrga=426:360:format=nv12:force_original_aspect_ratio=disable,hwmap=mode=read,format=nv12[back];[2:v]fps=${fps},scale_rkrga=428:360:format=nv12:force_original_aspect_ratio=disable,hwmap=mode=read,format=nv12[left_repeat];[3:v]fps=${fps},scale_rkrga=426:360:format=nv12:force_original_aspect_ratio=disable,hwmap=mode=read,format=nv12[right_repeat];[4:v]fps=${fps},scale_rkrga=426:360:format=nv12:force_original_aspect_ratio=disable,hwmap=mode=read,format=nv12[left_pillar];[5:v]fps=${fps},scale_rkrga=428:360:format=nv12:force_original_aspect_ratio=disable,hwmap=mode=read,format=nv12[right_pillar];[front][back][left_repeat][right_repeat][left_pillar][right_pillar]xstack=inputs=6:layout=0_0|w0_0|w0+w1_0|0_h0|w3_h0|w3+w4_h0:fill=black:shortest=1[out]" \
        -map "[out]"
}

run_six_case 30
run_six_case 24
run_six_case 20

chmod 0644 "${REPORT_PATH}" "${WORK_ROOT}"/*.log "${WORK_ROOT}"/*-samples.txt "${WORK_ROOT}"/*.mp4
echo "混合合成基准已写入 ${REPORT_PATH}"
