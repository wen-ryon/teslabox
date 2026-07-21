#!/bin/sh
set -eu

SOURCE="${1:-/tmp/teslabox.new}"
CHECKSUM_FILE="${SOURCE}.sha256"
TARGET="/usr/local/bin/teslabox"
SERVICE="teslabox.service"
HELPER_SOURCE="${2:-/tmp/teslabox-update-helper.new}"
HELPER_TARGET="/usr/local/libexec/teslabox-update-helper"
UNIT_SOURCE="${3:-/tmp/teslabox-update.service}"
UNIT_TARGET="/etc/systemd/system/teslabox-update.service"

if [ "$(id -u)" -ne 0 ]; then
    echo "请使用 sudo 运行该脚本" >&2
    exit 1
fi

if [ ! -f "${SOURCE}" ] || [ ! -f "${CHECKSUM_FILE}" ]; then
    echo "缺少新版二进制或 SHA-256 校验文件" >&2
    exit 1
fi

# 只读取校验文件第一列，允许上传后按板端约定重命名文件。
EXPECTED_SHA256="$(awk 'NR == 1 {print tolower($1)}' "${CHECKSUM_FILE}")"
ACTUAL_SHA256="$(sha256sum "${SOURCE}" | awk '{print tolower($1)}')"
if [ -z "${EXPECTED_SHA256}" ] || [ "${EXPECTED_SHA256}" != "${ACTUAL_SHA256}" ]; then
    echo "新版主程序 SHA-256 校验失败" >&2
    exit 1
fi

# 在线更新助手属于首次引导组件；没有同时上传助手和 unit 时仍兼容旧的仅主程序更新。
if [ -f "${HELPER_SOURCE}" ] || [ -f "${UNIT_SOURCE}" ]; then
    if [ ! -f "${HELPER_SOURCE}" ] || [ ! -f "${HELPER_SOURCE}.sha256" ] || [ ! -f "${UNIT_SOURCE}" ]; then
        echo "更新助手、助手校验文件和 systemd unit 必须同时提供" >&2
        exit 1
    fi
    EXPECTED_HELPER_SHA256="$(awk 'NR == 1 {print tolower($1)}' "${HELPER_SOURCE}.sha256")"
    ACTUAL_HELPER_SHA256="$(sha256sum "${HELPER_SOURCE}" | awk '{print tolower($1)}')"
    if [ -z "${EXPECTED_HELPER_SHA256}" ] || [ "${EXPECTED_HELPER_SHA256}" != "${ACTUAL_HELPER_SHA256}" ]; then
        echo "更新助手 SHA-256 校验失败" >&2
        exit 1
    fi
    install -d -m 0755 "$(dirname "${HELPER_TARGET}")"
    install -m 0755 "${HELPER_SOURCE}" "${HELPER_TARGET}"
    install -m 0644 "${UNIT_SOURCE}" "${UNIT_TARGET}"
    systemctl daemon-reload
fi

BACKUP="${TARGET}.backup.$(date +%Y%m%d%H%M%S)"
cp -a "${TARGET}" "${BACKUP}"
install -m 0755 "${SOURCE}" "${TARGET}.new"
mv "${TARGET}.new" "${TARGET}"

# 用户显式运行本脚本即表示允许本次重启；脚本不会启用或关闭开机自启。
if ! systemctl restart "${SERVICE}" || ! systemctl is-active --quiet "${SERVICE}"; then
    echo "新版启动失败，正在恢复 ${BACKUP}" >&2
    install -m 0755 "${BACKUP}" "${TARGET}"
    systemctl restart "${SERVICE}"
    exit 1
fi

echo "TeslaBox 已更新，旧版本备份在 ${BACKUP}"
