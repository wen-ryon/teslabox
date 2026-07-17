#!/bin/sh
set -eu

# mt7921e 会在 NetworkManager 初始化 wlan0 时清理预先创建的虚拟接口，
# 因此必须等客户端接口就绪后再创建 ap0 并显式激活热点配置。
attempt=0
while [ ! -e /sys/class/net/wlan0 ]; do
	attempt=$((attempt + 1))
	if [ "$attempt" -ge 30 ]; then
		echo "teslabox-ap-setup: wlan0 未出现" >&2
		exit 1
	fi
	sleep 1
done

# 优先等待上游自动连接完成，让单射频热点直接落在相同信道；
# 上游不可用时最多等待 20 秒，不能因此阻止本地管理热点启动。
attempt=0
while [ "$attempt" -lt 20 ]; do
	wlan_state=$(/usr/bin/nmcli -g GENERAL.STATE device show wlan0 2>/dev/null || true)
	case "$wlan_state" in
		100*) break ;;
	esac
	attempt=$((attempt + 1))
	sleep 1
done

if [ ! -e /sys/class/net/ap0 ]; then
	phy_name=$(basename "$(readlink -f /sys/class/net/wlan0/phy80211)")
	/usr/sbin/iw phy "$phy_name" interface add ap0 type __ap
fi

# 虚拟接口默认继承 wlan0 的 MAC；两者同时启用会被内核以地址不唯一拒绝。
base_mac=$(cat /sys/class/net/wlan0/address)
ap_mac="02:${base_mac#*:}"
/usr/sbin/ip link set ap0 down
/usr/sbin/ip link set ap0 address "$ap_mac"

/usr/bin/nmcli device set ap0 managed yes
/usr/bin/nmcli --wait 20 connection up id TeslaBox-5G ifname ap0
