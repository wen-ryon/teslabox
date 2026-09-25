#!/bin/sh
set -eu

# 供已安装 TeslaBox 的 ROCK 3A 单独补装 MT7922 内核与固件。
ARCHIVE="teslabox-mt7922-bluetooth-rock3a-5.10.160teslabox2-fw20260916.tar.gz"
URL="https://gitee.com/TecheN1/teslabox-releases/releases/download/v0.1.31-rc.2/${ARCHIVE}"
SHA256="39ebb602bde400ce1baf93c61fee8bb17d0ad9535ac4ce736f2d5181054d7fad"
WIFI_FIRMWARE_KERNEL_BASE="https://git.kernel.org/pub/scm/linux/kernel/git/firmware/linux-firmware.git/plain/mediatek"
WIFI_FIRMWARE_GITLAB_BASE="https://gitlab.com/kernel-firmware/linux-firmware/-/raw/20260916/mediatek"
WIFI_PATCH_SHA256="7fc9075d6d54b31e0539734614ca971fa52caf41b642a3000c8a4501591a4d98"
WIFI_RAM_SHA256="d100e5e04c324fc8da6302112d1e0b4fac952286447d2419eabcd8fb0a1f5b93"
WORK_DIR=""

cleanup() {
    if [ -n "${WORK_DIR}" ] && [ -d "${WORK_DIR}" ]; then
        rm -rf -- "${WORK_DIR}"
    fi
}
trap cleanup EXIT HUP INT TERM

prepare_wifi_firmware() {
    firmware_name="$1"
    expected_sha256="$2"
    firmware_path="${WORK_DIR}/package/firmware/mediatek/${firmware_name}"
    if [ -s "${firmware_path}" ]; then
        actual_sha256="$(sha256sum "${firmware_path}" | awk '{print tolower($1)}')"
        if [ "${actual_sha256}" != "${expected_sha256}" ]; then
            echo "MT7922 Wi-Fi 固件校验失败：${firmware_name}。" >&2
            exit 1
        fi
        return
    fi
    # 旧公开附件漏打了 Wi-Fi 固件，补取后先校验再安装。
    install -d -m 0755 "${WORK_DIR}/package/firmware/mediatek"
    for firmware_url in \
        "${WIFI_FIRMWARE_KERNEL_BASE}/${firmware_name}?h=20260916" \
        "${WIFI_FIRMWARE_GITLAB_BASE}/${firmware_name}"; do
        if curl --fail --location --retry 3 --connect-timeout 15 --max-time 120 \
            --output "${firmware_path}" "${firmware_url}" && [ -s "${firmware_path}" ]; then
            actual_sha256="$(sha256sum "${firmware_path}" | awk '{print tolower($1)}')"
            if [ "${actual_sha256}" = "${expected_sha256}" ]; then
                return
            fi
        fi
    done
    echo "MT7922 Wi-Fi 固件下载或校验失败：${firmware_name}。" >&2
    exit 1
}

if [ "$(id -u)" -ne 0 ]; then
    echo "请使用 sudo 运行。" >&2
    exit 1
fi
if [ "$(dpkg --print-architecture)" != "arm64" ] || [ "$(uname -m)" != "aarch64" ]; then
    echo "仅支持 ARM64 ROCK 3A。" >&2
    exit 1
fi
. /etc/os-release
case "${ID:-}:${VERSION_ID:-}" in
    debian:11|debian:11.*) ;;
    *) echo "仅支持 Radxa Debian 11。" >&2; exit 1 ;;
esac
BOARD_MODEL="$(tr '\000' '\n' </proc/device-tree/model 2>/dev/null | sed -n '1p' || true)"
case "${BOARD_MODEL}" in
    *ROCK3*Model*A*|*ROCK*3A*) ;;
    *) echo "仅支持 ROCK 3A，当前为 ${BOARD_MODEL:-unknown}。" >&2; exit 1 ;;
esac
for command_name in curl sha256sum tar dpkg apt-get; do
    command -v "${command_name}" >/dev/null 2>&1 || {
        echo "缺少命令：${command_name}" >&2
        exit 1
    }
done

if command -v lspci >/dev/null 2>&1 &&
    ! lspci -Dn 2>/dev/null | awk 'tolower($0) ~ /14c3:(0616|7922)/ { found=1 } END { exit !found }'; then
    echo "警告：当前未检测到 MT7922 PCI ID 14c3:0616 或 14c3:7922。" >&2
fi
if [ ! -r /dev/tty ]; then
    echo "需要交互式终端确认测试内核安装。" >&2
    exit 1
fi
printf '即将安装 5.10.160teslabox2 测试内核及 MT7922 Wi-Fi/蓝牙驱动。输入 INSTALL MT7922 继续：' >/dev/tty
IFS= read -r confirmation </dev/tty || confirmation=""
if [ "${confirmation}" != "INSTALL MT7922" ]; then
    echo "已取消安装。"
    exit 0
fi

WORK_DIR="$(mktemp -d /tmp/teslabox-mt7922-recovery.XXXXXX)"
curl --fail --location --retry 3 --connect-timeout 15 \
    --output "${WORK_DIR}/${ARCHIVE}" "${URL}"
actual_sha256="$(sha256sum "${WORK_DIR}/${ARCHIVE}" | awk '{print tolower($1)}')"
if [ "${actual_sha256}" != "${SHA256}" ]; then
    echo "MT7922 归档 SHA-256 校验失败。" >&2
    exit 1
fi

mkdir "${WORK_DIR}/package"
tar -C "${WORK_DIR}/package" -xzf "${WORK_DIR}/${ARCHIVE}"
if [ ! -s "${WORK_DIR}/package/SHA256SUMS" ] || \
    [ ! -s "${WORK_DIR}/package/linux-image-5.10.160teslabox2_5.10.160teslabox2-1_arm64.deb" ] || \
    [ ! -s "${WORK_DIR}/package/bluetooth/btusb.ko" ] || \
    [ ! -s "${WORK_DIR}/package/bluetooth/btrtl.ko" ] || \
    [ ! -s "${WORK_DIR}/package/bluetooth/BT_RAM_CODE_MT7922_1_1_hdr.bin" ] || \
    [ ! -s "${WORK_DIR}/package/wifi-updates/drivers/net/wireless/mediatek/mt76/mt7921/mt7921e.ko" ]; then
    echo "MT7922 归档结构不完整。" >&2
    exit 1
fi
(
    cd "${WORK_DIR}/package"
    sha256sum -c SHA256SUMS
)
prepare_wifi_firmware "WIFI_MT7922_patch_mcu_1_1_hdr.bin" "${WIFI_PATCH_SHA256}"
prepare_wifi_firmware "WIFI_RAM_CODE_MT7922_1.bin" "${WIFI_RAM_SHA256}"

apt-get -o Acquire::Retries=3 install -y --no-install-recommends bluez
kernel_status="$(dpkg-query -W -f='${Status}' linux-image-5.10.160teslabox2 2>/dev/null || true)"
if [ "${kernel_status}" != "install ok installed" ]; then
    dpkg -i "${WORK_DIR}/package/linux-image-5.10.160teslabox2_5.10.160teslabox2-1_arm64.deb"
fi
KERNEL="5.10.160teslabox2"
install -d -m 0755 "/lib/modules/${KERNEL}/updates" "/lib/modules/${KERNEL}/updates/teslabox-bluetooth"
cp -a "${WORK_DIR}/package/wifi-updates/." "/lib/modules/${KERNEL}/updates/"
install -m 0644 "${WORK_DIR}/package/bluetooth/btusb.ko" "/lib/modules/${KERNEL}/updates/teslabox-bluetooth/btusb.ko"
install -m 0644 "${WORK_DIR}/package/bluetooth/btrtl.ko" "/lib/modules/${KERNEL}/updates/teslabox-bluetooth/btrtl.ko"
install -d -m 0755 /lib/firmware/mediatek
install -m 0644 "${WORK_DIR}/package/bluetooth/BT_RAM_CODE_MT7922_1_1_hdr.bin" /lib/firmware/mediatek/BT_RAM_CODE_MT7922_1_1_hdr.bin
install -m 0644 "${WORK_DIR}/package/firmware/mediatek/WIFI_MT7922_patch_mcu_1_1_hdr.bin" /lib/firmware/mediatek/WIFI_MT7922_patch_mcu_1_1_hdr.bin
install -m 0644 "${WORK_DIR}/package/firmware/mediatek/WIFI_RAM_CODE_MT7922_1.bin" /lib/firmware/mediatek/WIFI_RAM_CODE_MT7922_1.bin
if [ -s "${WORK_DIR}/package/firmware/LICENCE.mediatek" ]; then
    install -d -m 0755 /usr/share/doc/teslabox
    install -m 0644 "${WORK_DIR}/package/firmware/LICENCE.mediatek" /usr/share/doc/teslabox/LICENCE.mediatek
fi
depmod -a "${KERNEL}"
update-initramfs -u -k "${KERNEL}"
for module_name in btusb btrtl mt7921e; do
    if ! modinfo -k "${KERNEL}" "${module_name}" >/dev/null 2>&1; then
        echo "安装后缺少内核模块：${module_name}" >&2
        exit 1
    fi
done
if [ ! -s /lib/firmware/mediatek/BT_RAM_CODE_MT7922_1_1_hdr.bin ]; then
    echo "安装后缺少 MT7922 蓝牙固件。" >&2
    exit 1
fi
if [ ! -s /lib/firmware/mediatek/WIFI_MT7922_patch_mcu_1_1_hdr.bin ] || \
    [ ! -s /lib/firmware/mediatek/WIFI_RAM_CODE_MT7922_1.bin ]; then
    echo "安装后缺少 MT7922 Wi-Fi 固件。" >&2
    exit 1
fi
systemctl enable teslabox-ap.service >/dev/null
echo "MT7922 Wi-Fi/蓝牙驱动已安装。请确认车机未写入后手工执行 sudo reboot。"
