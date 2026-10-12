#!/bin/sh
set -eu

# 维护者手动安装也必须经过同一个事务助手，不能退回只恢复程序的旧流程。
SOURCE="${1:-/tmp/teslabox.new}"
VERSION="${2:-}"
HELPER="/usr/local/libexec/teslabox-update-helper"
ROOT="/var/lib/teslabox-system"

if [ "$(id -u)" -ne 0 ]; then
    echo "请使用 sudo 运行该脚本" >&2
    exit 1
fi
if [ -z "${VERSION}" ] || ! printf '%s\n' "${VERSION}" | LC_ALL=C awk '/^[0-9]+\.[0-9]+\.[0-9]+(-rc\.[1-9][0-9]*)?$/ {valid=1} END {exit !valid}'; then
    echo "用法：install-teslabox-update.sh 二进制路径 发布版本；旧的助手/unit参数不再接受" >&2
    exit 1
fi
if [ "$("${HELPER}" --protocol)" != "teslabox-paired-state-v1" ]; then
    echo "请先完成 bootstrap-update-rollback.py 助手迁移，主程序未修改" >&2
    exit 1
fi
if ! systemctl show teslabox.service --property=ExecStartPre --value | LC_ALL=C awk '/\/usr\/local\/libexec\/teslabox-update-helper --recover/ {found=1} END {exit !found}'; then
    echo "启动恢复守卫未配置，主程序未修改" >&2
    exit 1
fi
if [ ! -f "${SOURCE}" ] || [ ! -f "${SOURCE}.sha256" ]; then
    echo "缺少新版二进制或来自可信发布物的 SHA-256 校验文件" >&2
    exit 1
fi
EXPECTED="$(awk 'NR == 1 {print tolower($1)}' "${SOURCE}.sha256")"
ACTUAL="$(sha256sum "${SOURCE}" | awk '{print tolower($1)}')"
if [ -z "${EXPECTED}" ] || [ "${EXPECTED}" != "${ACTUAL}" ]; then
    echo "主程序摘要不匹配，未安装" >&2
    exit 1
fi
install -d -m 0700 "${ROOT}/updates"
NAME="teslabox-manual-$$"
install -m 0600 "${SOURCE}" "${ROOT}/updates/${NAME}"
SIZE="$(wc -c < "${ROOT}/updates/${NAME}" | tr -d ' ')"
REQUEST="${ROOT}/manual-update-request-$$.json"
umask 077
printf '{"version":"%s","package_name":"%s","sha256":"%s","size":%s}\n' "${VERSION}" "${NAME}" "${EXPECTED}" "${SIZE}" > "${REQUEST}"
# 有自定义 TESLABOX_EXPORT_STATE 时由操作者显式导出；助手会与运行中的主服务核对。
"${HELPER}" --request "${REQUEST}" --result "${ROOT}/manual-update-result.json"
echo "事务安装完成；失败状态和回滚证据保存在 update-backups。"
