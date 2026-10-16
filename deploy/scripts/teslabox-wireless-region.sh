#!/bin/sh
set -eu

COUNTRY_FILE=/etc/teslabox/wireless-country
LOCK_FILE=/run/lock/teslabox-wireless-region.lock

# 迁移和国内默认值由主程序统一生成；启动脚本绝不猜测或覆盖已有配置。
if [ ! -e "$COUNTRY_FILE" ]; then
    echo "teslabox-wireless-region: 尚未完成地区初始化" >&2
    exit 1
fi
if [ -L "$COUNTRY_FILE" ] || [ ! -f "$COUNTRY_FILE" ] || [ "$(wc -c <"$COUNTRY_FILE")" -gt 16 ]; then
    echo "teslabox-wireless-region: 地区文件无效，拒绝应用" >&2
    exit 1
fi

umask 077
mkdir -p /run/lock
if [ -L "$LOCK_FILE" ]; then
    echo "teslabox-wireless-region: 锁文件不能是符号链接" >&2
    exit 1
fi
exec 9>"$LOCK_FILE"
flock -w 5 9
COUNTRY=$(tr -d '\r\n' <"$COUNTRY_FILE")
case "$COUNTRY" in
    [A-Z][A-Z]|00) ;;
    *) echo "teslabox-wireless-region: 国家码格式无效" >&2; exit 1 ;;
esac

# 国家码只是监管请求，网卡自身的硬件和固件限制仍由内核保留。
iw reg set "$COUNTRY"
attempt=0
while [ "$attempt" -lt 10 ]; do
    ACTUAL=$(iw reg get | awk '/^global$/ { global=1; next } /^phy#/ { global=0 } global && $1=="country" { gsub(":", "", $2); print $2; exit }')
    if [ "$ACTUAL" = "$COUNTRY" ]; then
        echo "teslabox-wireless-region: configured=$COUNTRY effective=$ACTUAL"
        exit 0
    fi
    attempt=$((attempt + 1))
    sleep 0.2
done
echo "teslabox-wireless-region: 地区已保存但实际全局监管域尚未匹配" >&2
exit 1
