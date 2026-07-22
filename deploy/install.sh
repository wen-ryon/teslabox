#!/bin/sh
set -eu

INSTALLER_VERSION="0.1.0"
RELEASE_BASE="https://gitee.com/TecheN1/teslabox-releases/releases/download/v0.1.0"
BOOTSTRAP_ARCHIVE="teslabox-bootstrap-debian11-arm64.tar.gz"
BOOTSTRAP_SHA256="037d51afb7d238203b7643c71176043eaa70a33f67198fa851afd04ed6fb49b9"
MT7922_ARCHIVE="teslabox-mt7922-rock3a-5.10.160teslabox1.tar.gz"
MT7922_SHA256="1cd88ca287eee7b2041684fe8d45f3fb84015ac8fd5cfbebb6e98b2323e9992c"
UPDATE_MANIFEST_URL="https://gitee.com/TecheN1/teslabox-releases/raw/master/stable/latest.json"
UPDATE_PUBLIC_KEY="7THPh+C3xTzkCPWRHYSClGnZasEHkprykDK2kh0rkcU="

WORK_DIR=""
AP_INITIAL_PASSWORD=""
MT7922_MODE="ask"

log() {
    printf '\n[TeslaBox] %s\n' "$*"
}

warn() {
    printf '\n[TeslaBox] 警告：%s\n' "$*" >&2
}

die() {
    printf '\n[TeslaBox] 错误：%s\n' "$*" >&2
    exit 1
}

cleanup() {
    if [ -n "${WORK_DIR}" ] && [ -d "${WORK_DIR}" ]; then
        find "${WORK_DIR}" -depth -delete 2>/dev/null || true
    fi
}

normalize_checksum_file() {
    checksum_file="$1"
    normalized_file="${checksum_file}.lf"

    # 兼容早期由 Windows 构建脚本生成的 CRLF 校验清单，避免回车被当作文件名的一部分。
    tr -d '\r' <"${checksum_file}" >"${normalized_file}"
    mv "${normalized_file}" "${checksum_file}"
}

trap cleanup EXIT HUP INT TERM

for argument in "$@"; do
    case "${argument}" in
        --skip-mt7922) MT7922_MODE="skip" ;;
        --help)
            cat <<'EOF'
用法：sudo ./teslabox-install.sh [--skip-mt7922]

默认安装 TeslaBox、FFmpeg/MPP/RGA、systemd、udev 和在线升级组件。
安装末尾会单独询问是否安装 MT7922 测试内核，默认不安装。
EOF
            exit 0
            ;;
        *) die "未知参数：${argument}" ;;
    esac
done

if [ "$(id -u)" -ne 0 ]; then
    die "请使用 sudo 运行该脚本。"
fi

command -v curl >/dev/null 2>&1 || die "缺少 curl；请先执行 sudo apt-get install curl ca-certificates。"
command -v sha256sum >/dev/null 2>&1 || die "缺少 sha256sum。"
command -v tar >/dev/null 2>&1 || die "缺少 tar。"

if [ ! -r /etc/os-release ]; then
    die "无法识别操作系统。"
fi
. /etc/os-release
case "${ID:-}:${VERSION_ID:-}" in
    debian:11|debian:11.*) ;;
    *) die "首装包只支持 Radxa 官方 Debian 11，当前为 ${PRETTY_NAME:-unknown}。" ;;
esac

ARCHITECTURE="$(dpkg --print-architecture 2>/dev/null || true)"
if [ "${ARCHITECTURE}" != "arm64" ]; then
    die "首装包只支持 arm64，当前架构为 ${ARCHITECTURE:-unknown}。"
fi

BOARD_MODEL="$(tr '\000' '\n' </proc/device-tree/model 2>/dev/null | sed -n '1p' || true)"
case "${BOARD_MODEL}" in
    *ROCK3*Model*A*|*ROCK*3A*) ;;
    *) die "首装包只支持 Radxa ROCK 3A，当前设备为 ${BOARD_MODEL:-unknown}。" ;;
esac

WORK_DIR="$(mktemp -d /tmp/teslabox-install.XXXXXX)"
PAYLOAD_DIR="${WORK_DIR}/payload"
BOOTSTRAP_PATH="${WORK_DIR}/${BOOTSTRAP_ARCHIVE}"

log "检查通过：${BOARD_MODEL} / Debian ${VERSION_ID} / ${ARCHITECTURE}"
log "下载 TeslaBox ${INSTALLER_VERSION} 首装包"
curl --fail --location --retry 3 --connect-timeout 15 \
    --output "${BOOTSTRAP_PATH}" "${RELEASE_BASE}/${BOOTSTRAP_ARCHIVE}"

ACTUAL_BOOTSTRAP_SHA256="$(sha256sum "${BOOTSTRAP_PATH}" | awk '{print tolower($1)}')"
if [ "${ACTUAL_BOOTSTRAP_SHA256}" != "${BOOTSTRAP_SHA256}" ]; then
    die "首装包 SHA-256 校验失败。"
fi

mkdir -p "${PAYLOAD_DIR}"
tar -C "${PAYLOAD_DIR}" -xzf "${BOOTSTRAP_PATH}"
if [ ! -f "${PAYLOAD_DIR}/SHA256SUMS" ]; then
    die "首装包缺少 SHA256SUMS。"
fi
normalize_checksum_file "${PAYLOAD_DIR}/SHA256SUMS"
(
    cd "${PAYLOAD_DIR}"
    sha256sum -c SHA256SUMS
)

log "安装 Debian 运行依赖"
export DEBIAN_FRONTEND=noninteractive
apt-get update
apt-get install -y --no-install-recommends \
    avahi-daemon ca-certificates curl exfat-fuse exfatprogs iproute2 iw \
    libnss-mdns network-manager parted pciutils rfkill udev util-linux xfsprogs

if ! systemctl is-active --quiet teslabox.service 2>/dev/null && command -v ss >/dev/null 2>&1; then
    if ss -H -ltn 'sport = :80' 2>/dev/null | sed -n '1p' | read -r _; then
        die "80 端口已被其他服务占用，请先释放该端口。"
    fi
fi

log "安装 TeslaBox 程序和诊断工具"
install -d -m 0755 /usr/local/bin /usr/local/libexec /etc/teslabox /var/lib/teslabox-system
chmod 0700 /var/lib/teslabox-system

MAIN_BACKUP=""
if [ -x /usr/local/bin/teslabox ]; then
    MAIN_BACKUP="/usr/local/bin/teslabox.before-install.$(date +%Y%m%d%H%M%S)"
    cp -a /usr/local/bin/teslabox "${MAIN_BACKUP}"
fi

install -m 0755 "${PAYLOAD_DIR}/bin/teslabox" /usr/local/bin/teslabox
install -m 0755 "${PAYLOAD_DIR}/bin/teslabox-init" /usr/local/bin/teslabox-init
install -m 0755 "${PAYLOAD_DIR}/bin/teslabox-image-init" /usr/local/bin/teslabox-image-init
install -m 0755 "${PAYLOAD_DIR}/bin/teslabox-gadget" /usr/local/bin/teslabox-gadget
install -m 0755 "${PAYLOAD_DIR}/libexec/teslabox-update-helper" /usr/local/libexec/teslabox-update-helper

log "安装 Rockchip FFmpeg、MPP 和 RGA 媒体工具链"
sh "${PAYLOAD_DIR}/scripts/install-media-toolchain.sh" \
    "${PAYLOAD_DIR}/media/teslabox-media-debian11-arm64.tar.gz"

if [ ! -f /etc/teslabox/teslabox.env ]; then
    MACHINE_SUFFIX="$(tr -d '-' </etc/machine-id 2>/dev/null | cut -c1-16 | tr '[:lower:]' '[:upper:]')"
    [ -n "${MACHINE_SUFFIX}" ] || MACHINE_SUFFIX="ROCK3A"
    sed "s/^TESLABOX_GADGET_SERIAL=.*/TESLABOX_GADGET_SERIAL=TESLABOX-${MACHINE_SUFFIX}/" \
        "${PAYLOAD_DIR}/config/teslabox.env.example" >"${WORK_DIR}/teslabox.env"
    install -m 0640 "${WORK_DIR}/teslabox.env" /etc/teslabox/teslabox.env
fi

ENV_TEMP="${WORK_DIR}/teslabox.env.updated"
awk '!/^TESLABOX_UPDATE_(MANIFEST_URL|PUBLIC_KEY|CHANNEL|BOARD)=/' \
    /etc/teslabox/teslabox.env >"${ENV_TEMP}"
{
    printf '%s\n' "TESLABOX_UPDATE_MANIFEST_URL=${UPDATE_MANIFEST_URL}"
    printf '%s\n' "TESLABOX_UPDATE_PUBLIC_KEY=${UPDATE_PUBLIC_KEY}"
    printf '%s\n' 'TESLABOX_UPDATE_CHANNEL=stable'
    printf '%s\n' 'TESLABOX_UPDATE_BOARD=radxa-rock-3a'
} >>"${ENV_TEMP}"
install -m 0640 "${ENV_TEMP}" /etc/teslabox/teslabox.env

log "安装 systemd、udev 和 AP 恢复组件"
install -m 0644 "${PAYLOAD_DIR}/systemd/teslabox.service" /etc/systemd/system/teslabox.service
install -m 0644 "${PAYLOAD_DIR}/systemd/teslabox-update.service" /etc/systemd/system/teslabox-update.service
install -m 0644 "${PAYLOAD_DIR}/systemd/teslabox-ap.service" /etc/systemd/system/teslabox-ap.service
install -m 0644 "${PAYLOAD_DIR}/udev/99-teslabox.rules" /etc/udev/rules.d/99-teslabox.rules
install -m 0755 "${PAYLOAD_DIR}/scripts/teslabox-ap-setup.sh" /usr/local/libexec/teslabox-ap-setup.sh

log "停用 Radxa 原厂 USB Gadget 服务"
for GADGET_SERVICE in radxa-adbd.service radxa-usbnet.service; do
    GADGET_SERVICE_STATE="$(systemctl is-enabled "${GADGET_SERVICE}" 2>/dev/null || true)"
    case "${GADGET_SERVICE_STATE}" in
        ""|not-found) continue ;;
        masked|masked-runtime) ;;
        *)
            systemctl disable --now "${GADGET_SERVICE}"
            systemctl mask "${GADGET_SERVICE}"
            ;;
    esac
done

systemctl disable --now teslabox-gadget.service >/dev/null 2>&1 || true
systemctl daemon-reload
udevadm control --reload-rules
systemctl enable avahi-daemon.service teslabox.service >/dev/null

network_env_value() {
    sed -n "s/^$1=//p" /etc/teslabox/teslabox.env | sed -n '1p'
}

AP_PROFILE_INTERFACE="$(network_env_value TESLABOX_AP_INTERFACE)"
if [ -z "${AP_PROFILE_INTERFACE}" ]; then
    AP_PROFILE_INTERFACE="$(nmcli -g connection.interface-name connection show id TeslaBox-5G 2>/dev/null | sed -n '1p' || true)"
fi
if [ -z "${AP_PROFILE_INTERFACE}" ]; then
    for CANDIDATE in ap0 ap1 ap2 teslabox-ap; do
        if [ ! -e "/sys/class/net/${CANDIDATE}" ]; then
            AP_PROFILE_INTERFACE="${CANDIDATE}"
            break
        fi
    done
fi
AP_PROFILE_INTERFACE="${AP_PROFILE_INTERFACE:-teslabox-ap}"

detect_wifi_interface() {
    CONFIGURED_WIFI_INTERFACE="$(network_env_value TESLABOX_WIFI_INTERFACE)"
    if [ -n "${CONFIGURED_WIFI_INTERFACE}" ]; then
        if [ -e "/sys/class/net/${CONFIGURED_WIFI_INTERFACE}" ]; then
            printf '%s\n' "${CONFIGURED_WIFI_INTERFACE}"
        fi
        return
    fi
    nmcli -t --escape no -f DEVICE,TYPE device status 2>/dev/null |
        awk -F: -v ap="${AP_PROFILE_INTERFACE}" '$2 == "wifi" && $1 != ap { print $1; exit }'
}

setup_ap_profile() {
    systemctl enable NetworkManager.service >/dev/null || return 1
    systemctl start NetworkManager.service || return 1
    if ! nmcli -t -f NAME connection show | sed 's/\\:/:/g' | sed -n '/^TeslaBox-5G$/p' | read -r _; then
        AP_INITIAL_PASSWORD="tb$(od -An -N 6 -tx1 /dev/urandom | tr -d ' \n')"
        nmcli connection add type wifi ifname "${AP_PROFILE_INTERFACE}" con-name TeslaBox-5G ssid TeslaBox-5G || return 1
        nmcli connection modify TeslaBox-5G \
            802-11-wireless.mode ap \
            802-11-wireless.band a \
            802-11-wireless.channel 161 \
            802-11-wireless-security.key-mgmt wpa-psk \
            802-11-wireless-security.psk "${AP_INITIAL_PASSWORD}" \
            ipv4.method shared \
            ipv4.addresses 192.168.88.1/24 \
            ipv6.method disabled \
            connection.autoconnect yes || return 1
    fi
}

if command -v nmcli >/dev/null 2>&1; then
    if ! setup_ap_profile; then
        AP_INITIAL_PASSWORD=""
        warn "AP 配置创建失败；TeslaUSB 和有线管理仍可继续使用。"
    fi
else
    warn "未找到 nmcli，暂不创建 AP 配置。"
fi

WIFI_INTERFACE="$(detect_wifi_interface)"
if [ -n "${WIFI_INTERFACE}" ]; then
    systemctl enable teslabox-ap.service >/dev/null || warn "teslabox-ap.service 无法设为开机启动。"
    systemctl restart teslabox-ap.service || warn "AP 热点启动失败；请保留网线并在设置中心检查无线网卡。"
else
    systemctl disable teslabox-ap.service >/dev/null 2>&1 || true
    warn "当前没有检测到无线客户端接口，暂不启动 AP；基础功能仍可通过网线使用。"
fi

log "启动 TeslaBox"
systemctl restart teslabox.service
HEALTHY="false"
attempt=0
while [ "${attempt}" -lt 30 ]; do
    if curl --fail --silent --show-error http://127.0.0.1/healthz >/dev/null 2>&1; then
        HEALTHY="true"
        break
    fi
    attempt=$((attempt + 1))
    sleep 1
done

if [ "${HEALTHY}" != "true" ]; then
    if [ -n "${MAIN_BACKUP}" ] && [ -f "${MAIN_BACKUP}" ]; then
        warn "新版服务未通过健康检查，正在恢复旧程序。"
        install -m 0755 "${MAIN_BACKUP}" /usr/local/bin/teslabox
        systemctl restart teslabox.service || true
    fi
    journalctl -u teslabox.service -n 80 --no-pager >&2 || true
    die "TeslaBox 未通过启动健康检查。"
fi

install_mt7922() {
    log "MT7922 可选测试内核"
    cat <<'EOF'

!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!
MT7922 支持会安装 TeslaBox 自编译的 5.10.160teslabox1 测试内核，
以及与该内核绑定的 mt76/mt7921e backports 驱动和固件。

它不是 Radxa 官方维护内核。现有无线网卡能正常工作时没有必要安装；
安装后必须重启，原厂内核会保留，可在启动故障时回退。
!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!
EOF

    if [ "${MT7922_PRESENT}" != "true" ]; then
        warn "当前未检测到 PCI ID 14c3:0616；如不是准备稍后插入 MT7922，请直接回车跳过。"
    fi

    if [ "${MT7922_MODE}" = "skip" ]; then
        warn "已按参数跳过 MT7922 测试内核。"
        return
    fi
    if [ ! -r /dev/tty ]; then
        warn "当前不是交互式终端，默认跳过 MT7922 测试内核。"
        return
    fi

    printf '如确认承担测试内核风险，请输入 INSTALL MT7922；直接回车跳过：' >/dev/tty
    IFS= read -r MT_CONFIRM </dev/tty || MT_CONFIRM=""
    if [ "${MT_CONFIRM}" != "INSTALL MT7922" ]; then
        warn "已跳过 MT7922 测试内核。"
        return
    fi

    MT_PATH="${WORK_DIR}/${MT7922_ARCHIVE}"
    MT_DIR="${WORK_DIR}/mt7922"
    curl --fail --location --retry 3 --connect-timeout 15 \
        --output "${MT_PATH}" "${RELEASE_BASE}/${MT7922_ARCHIVE}"
    ACTUAL_MT_SHA256="$(sha256sum "${MT_PATH}" | awk '{print tolower($1)}')"
    if [ "${ACTUAL_MT_SHA256}" != "${MT7922_SHA256}" ]; then
        die "MT7922 测试内核包 SHA-256 校验失败。"
    fi
    mkdir -p "${MT_DIR}"
    tar -C "${MT_DIR}" -xzf "${MT_PATH}"
    if [ ! -f "${MT_DIR}/SHA256SUMS" ]; then
        die "MT7922 测试内核包缺少 SHA256SUMS。"
    fi
    normalize_checksum_file "${MT_DIR}/SHA256SUMS"
    (
        cd "${MT_DIR}"
        sha256sum -c SHA256SUMS
    )
    dpkg -i \
        "${MT_DIR}/linux-image-5.10.160teslabox1_5.10.160teslabox1-2_arm64.deb" \
        "${MT_DIR}/linux-mt7922-backports-5.10.160teslabox1_5.10.160teslabox1-2_arm64.deb"
    depmod -a 5.10.160teslabox1
    systemctl enable teslabox-ap.service >/dev/null
    warn "MT7922 测试内核已安装。请确认没有车机 USB 写入后手工执行 sudo reboot。"
}

MT7922_PRESENT="false"
if command -v lspci >/dev/null 2>&1; then
    if lspci -Dn 2>/dev/null | sed -n '/14c3:0616/p' | read -r _; then
        MT7922_PRESENT="true"
    fi
fi

if [ "${MT7922_PRESENT}" = "true" ] || [ "${MT7922_MODE}" = "ask" ]; then
    install_mt7922
fi

log "首次安装完成"
printf '%s\n' "管理页面：http://$(hostname -I 2>/dev/null | awk '{print $1}')/"
printf '%s\n' '初始管理密码：teslabox（首次登录后必须修改）'
if [ -n "${AP_INITIAL_PASSWORD}" ]; then
    printf '%s\n' '热点名称：TeslaBox-5G'
    printf '%s\n' "初始热点密码：${AP_INITIAL_PASSWORD}"
    printf '%s\n' '请登录设置中心后修改热点密码。'
fi
printf '%s\n' '外接存储不会被自动格式化；请登录设置中心后手工初始化。'
