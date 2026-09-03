#!/bin/sh
set -eu

# 固定源码提交是为了让板端问题可以准确复现，而不受上游分支更新影响。
MPP_COMMIT="${MPP_COMMIT:-a9380ef333102ac318628f83b5f7a460d377749e}"
RGA_COMMIT="${RGA_COMMIT:-1d330cc28551943bed3380261a5a9c6fbd58ff53}"
FFMPEG_COMMIT="${FFMPEG_COMMIT:-388741a3544b92cf525f1cb3746ba9fb8f301d9a}"
EXFATPROGS_VERSION="${EXFATPROGS_VERSION:-1.2.0}"
EXFATPROGS_SHA256="${EXFATPROGS_SHA256:-56d9a49465deafc367d428afc71c8098705a30ee19a3cdf3c5320650b8880742}"

PREFIX="/opt/teslabox/media"
WORK_ROOT="/work"
SOURCE_ROOT="${WORK_ROOT}/sources"
BUILD_ROOT="${WORK_ROOT}/build"
STAGE_ROOT="${WORK_ROOT}/stage"
OUTPUT_ROOT="${WORK_ROOT}/output"

clean_directory() {
    directory="$1"
    mkdir -p "${directory}"
    find "${directory}" -mindepth 1 -delete
}

checkout_commit() {
    repository="$1"
    commit="$2"
    destination="$3"

    mkdir -p "${destination}"
    git -C "${destination}" init --quiet
    git -C "${destination}" remote add origin "${repository}"
    git -C "${destination}" fetch --quiet --depth 1 origin "${commit}"
    git -C "${destination}" checkout --quiet --detach FETCH_HEAD
}

clean_directory "${SOURCE_ROOT}"
clean_directory "${BUILD_ROOT}"
clean_directory "${STAGE_ROOT}"
clean_directory "${OUTPUT_ROOT}"

checkout_commit "https://gitee.com/nyanmisaka/mpp.git" "${MPP_COMMIT}" "${SOURCE_ROOT}/mpp"
checkout_commit "https://gitee.com/nyanmisaka/rga.git" "${RGA_COMMIT}" "${SOURCE_ROOT}/rga"
checkout_commit "https://github.com/nyanmisaka/ffmpeg-rockchip.git" "${FFMPEG_COMMIT}" "${SOURCE_ROOT}/ffmpeg-rockchip"

cmake \
    -S "${SOURCE_ROOT}/mpp" \
    -B "${BUILD_ROOT}/mpp" \
    -G Ninja \
    -DCMAKE_BUILD_TYPE=Release \
    -DCMAKE_INSTALL_PREFIX="${PREFIX}" \
    -DCMAKE_INSTALL_LIBDIR=lib \
    -DBUILD_SHARED_LIBS=ON \
    -DBUILD_TEST=OFF
cmake --build "${BUILD_ROOT}/mpp" --parallel "$(nproc)"
DESTDIR="${STAGE_ROOT}" cmake --install "${BUILD_ROOT}/mpp"

meson setup \
    "${BUILD_ROOT}/rga" \
    "${SOURCE_ROOT}/rga" \
    --prefix="${PREFIX}" \
    --libdir=lib \
    --buildtype=release \
    --default-library=shared \
    -Dcpp_args=-fpermissive \
    -Dlibdrm=false \
    -Dlibrga_demo=false
meson compile -C "${BUILD_ROOT}/rga" -j "$(nproc)"
DESTDIR="${STAGE_ROOT}" meson install -C "${BUILD_ROOT}/rga"

# FFmpeg 同时依赖容器自带的 libdrm 和刚构建的 MPP/RGA；临时复制到最终前缀
# 可以让 pkg-config 正确解析两类依赖，容器退出后不会污染构建板。
mkdir -p "${PREFIX}"
cp -a "${STAGE_ROOT}${PREFIX}/." "${PREFIX}/"
export PKG_CONFIG_PATH="${PREFIX}/lib/pkgconfig"

mkdir -p "${BUILD_ROOT}/ffmpeg-rockchip"
cd "${BUILD_ROOT}/ffmpeg-rockchip"
"${SOURCE_ROOT}/ffmpeg-rockchip/configure" \
    --prefix="${PREFIX}" \
    --enable-version3 \
    --enable-libass \
    --enable-libdrm \
    --enable-rkmpp \
    --enable-rkrga \
    --disable-debug \
    --disable-doc \
    --disable-ffplay \
    --extra-ldflags="-Wl,-rpath,${PREFIX}/lib"
make -j "$(nproc)"
make DESTDIR="${STAGE_ROOT}" install

# Debian 11 自带的 exfatprogs 1.1.0 会在快照包含损坏目录项时直接停止遍历，
# 无法修复车辆持续写入时捕获的中间态。项目专用 1.2.0 只用于快照副本，
# 不覆盖系统工具，也不会接触仍提供给车机的原始镜像。
EXFATPROGS_ARCHIVE="${SOURCE_ROOT}/exfatprogs-${EXFATPROGS_VERSION}.tar.xz"
curl --fail --location --silent --show-error \
    "https://github.com/exfatprogs/exfatprogs/releases/download/${EXFATPROGS_VERSION}/exfatprogs-${EXFATPROGS_VERSION}.tar.xz" \
    --output "${EXFATPROGS_ARCHIVE}"
printf '%s  %s\n' "${EXFATPROGS_SHA256}" "${EXFATPROGS_ARCHIVE}" | sha256sum -c -
tar -xJf "${EXFATPROGS_ARCHIVE}" -C "${SOURCE_ROOT}"
cd "${SOURCE_ROOT}/exfatprogs-${EXFATPROGS_VERSION}"
./configure --prefix="${PREFIX}"
make -j "$(nproc)"
install -m 0755 fsck/fsck.exfat "${STAGE_ROOT}${PREFIX}/bin/teslabox-fsck.exfat"

# ASS 仪表使用固定字体，不能依赖不同用户系统的字体扫描结果。
mkdir -p "${STAGE_ROOT}${PREFIX}/share/fonts"
cp /usr/share/fonts/opentype/noto/NotoSansCJK-Regular.ttc "${STAGE_ROOT}${PREFIX}/share/fonts/"

# 记录源码和目标运行时，便于部署后判断二进制是否来自预期构建。
cat > "${STAGE_ROOT}${PREFIX}/BUILD_INFO" <<EOF
target=debian-11-arm64
prefix=${PREFIX}
mpp_commit=${MPP_COMMIT}
rga_commit=${RGA_COMMIT}
ffmpeg_rockchip_commit=${FFMPEG_COMMIT}
exfatprogs_version=${EXFATPROGS_VERSION}
exfatprogs_source_sha256=${EXFATPROGS_SHA256}
EOF

# 目标板只需要命令行程序和运行库，移除开发文件可以显著缩小部署包。
find "${STAGE_ROOT}${PREFIX}/include" -depth -delete
find "${STAGE_ROOT}${PREFIX}/lib/pkgconfig" -depth -delete
find "${STAGE_ROOT}${PREFIX}/lib" -maxdepth 1 -type f -name '*.a' -delete

LD_LIBRARY_PATH="${STAGE_ROOT}${PREFIX}/lib" "${STAGE_ROOT}${PREFIX}/bin/ffmpeg" -hide_banner -version
LD_LIBRARY_PATH="${STAGE_ROOT}${PREFIX}/lib" "${STAGE_ROOT}${PREFIX}/bin/ffmpeg" -hide_banner -hwaccels
LD_LIBRARY_PATH="${STAGE_ROOT}${PREFIX}/lib" "${STAGE_ROOT}${PREFIX}/bin/ffmpeg" -hide_banner -decoders | sed -n '/_rkmpp/p'
LD_LIBRARY_PATH="${STAGE_ROOT}${PREFIX}/lib" "${STAGE_ROOT}${PREFIX}/bin/ffmpeg" -hide_banner -encoders | sed -n '/_rkmpp/p'
LD_LIBRARY_PATH="${STAGE_ROOT}${PREFIX}/lib" "${STAGE_ROOT}${PREFIX}/bin/ffmpeg" -hide_banner -filters | sed -n '/_rkrga/p'
for capability in h264_rkmpp hevc_rkmpp; do
    for list in decoders encoders; do
        if ! LD_LIBRARY_PATH="${STAGE_ROOT}${PREFIX}/lib" "${STAGE_ROOT}${PREFIX}/bin/ffmpeg" -hide_banner "-${list}" 2>/dev/null |
            awk -v required="${capability}" 'index($0, required) { found=1 } END { exit !found }'; then
            echo "FFmpeg 构建缺少 ${list} 能力：${capability}" >&2
            exit 1
        fi
    done
done
for capability in ass subtitles scale_rkrga overlay_rkrga vpp_rkrga; do
    if ! LD_LIBRARY_PATH="${STAGE_ROOT}${PREFIX}/lib" "${STAGE_ROOT}${PREFIX}/bin/ffmpeg" -hide_banner -filters 2>/dev/null |
        awk -v required="${capability}" 'index($0, required) { found=1 } END { exit !found }'; then
        echo "FFmpeg 构建缺少滤镜：${capability}" >&2
        exit 1
    fi
done
LD_LIBRARY_PATH="${STAGE_ROOT}${PREFIX}/lib" "${STAGE_ROOT}${PREFIX}/bin/ffmpeg" -hide_banner -filters | sed -n '/ ass\| subtitles/p'
test -x "${STAGE_ROOT}${PREFIX}/bin/teslabox-fsck.exfat"
"${STAGE_ROOT}${PREFIX}/bin/teslabox-fsck.exfat" -V 2>&1 | grep -F "${EXFATPROGS_VERSION}" >/dev/null

tar -C "${STAGE_ROOT}" -czf "${OUTPUT_ROOT}/teslabox-media-debian11-arm64.tar.gz" opt
cd "${OUTPUT_ROOT}"
sha256sum teslabox-media-debian11-arm64.tar.gz > teslabox-media-debian11-arm64.tar.gz.sha256
