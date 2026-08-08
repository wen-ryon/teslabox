#!/bin/sh
set -eu

ARCHIVE="${1:-/tmp/teslabox-media-debian11-arm64.tar.gz}"
CHECKSUM_FILE="${ARCHIVE}.sha256"
INSTALL_PARENT="/opt/teslabox"
TARGET="${INSTALL_PARENT}/media"
STAGING="${INSTALL_PARENT}/.media.new.$$"
BACKUP=""

if [ "$(id -u)" -ne 0 ]; then
    echo "请使用 sudo 运行该脚本" >&2
    exit 1
fi

if [ ! -f "${ARCHIVE}" ] || [ ! -f "${CHECKSUM_FILE}" ]; then
    echo "缺少构建包或 SHA-256 校验文件：${ARCHIVE}" >&2
    exit 1
fi

# 安装前再次校验是为了避免上传中断后把不完整的二进制放进系统目录。
cd "$(dirname "${ARCHIVE}")"
sha256sum -c "$(basename "${CHECKSUM_FILE}")"

mkdir -p "${INSTALL_PARENT}" "${STAGING}"
tar -C "${STAGING}" --strip-components=3 -xzf "${ARCHIVE}"

if [ ! -x "${STAGING}/bin/ffmpeg" ] || [ ! -x "${STAGING}/bin/ffprobe" ]; then
    echo "构建包缺少 ffmpeg 或 ffprobe" >&2
    find "${STAGING}" -depth -delete
    exit 1
fi

require_capability() {
    capability="$1"
    shift
    if ! LD_LIBRARY_PATH="${STAGING}/lib" "${STAGING}/bin/ffmpeg" -hide_banner "$@" 2>/dev/null |
        awk -v required="${capability}" 'index($0, required) { found=1 } END { exit !found }'; then
        echo "构建包缺少必需能力：${capability}" >&2
        find "${STAGING}" -depth -delete
        exit 1
    fi
}

for capability in h264_rkmpp hevc_rkmpp; do
    require_capability "${capability}" -decoders
    require_capability "${capability}" -encoders
done
for capability in ass subtitles scale_rkrga overlay_rkrga vpp_rkrga; do
    require_capability "${capability}" -filters
done
if [ ! -s "${STAGING}/share/fonts/NotoSansCJK-Regular.ttc" ]; then
    echo "构建包缺少固定的 Noto CJK 字体，拒绝安装" >&2
    find "${STAGING}" -depth -delete
    exit 1
fi

# 先在暂存目录运行，避免替换旧版本后才发现动态库不兼容。
LD_LIBRARY_PATH="${STAGING}/lib" "${STAGING}/bin/ffmpeg" -hide_banner -version >/dev/null
chown -R root:root "${STAGING}"

if [ -e "${TARGET}" ]; then
    BACKUP="${INSTALL_PARENT}/media.backup.$(date +%Y%m%d%H%M%S)"
    mv "${TARGET}" "${BACKUP}"
fi

if ! mv "${STAGING}" "${TARGET}"; then
    if [ -n "${BACKUP}" ] && [ -e "${BACKUP}" ]; then
        mv "${BACKUP}" "${TARGET}"
    fi
    exit 1
fi

# 使用项目专用命令名，避免覆盖系统包管理器以后安装的标准 FFmpeg。
ln -sfn "${TARGET}/bin/ffmpeg" /usr/local/bin/teslabox-ffmpeg
ln -sfn "${TARGET}/bin/ffprobe" /usr/local/bin/teslabox-ffprobe

"${TARGET}/bin/ffmpeg" -hide_banner -hwaccels
"${TARGET}/bin/ffmpeg" -hide_banner -decoders | sed -n '/_rkmpp/p'
"${TARGET}/bin/ffmpeg" -hide_banner -encoders | sed -n '/_rkmpp/p'
"${TARGET}/bin/ffmpeg" -hide_banner -filters | sed -n '/_rkrga/p'
"${TARGET}/bin/ffmpeg" -hide_banner -filters | sed -n '/ ass\| subtitles/p'

echo "媒体工具链已安装到 ${TARGET}"
if [ -n "${BACKUP}" ]; then
    echo "旧版本已备份到 ${BACKUP}"
fi
