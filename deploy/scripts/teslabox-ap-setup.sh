#!/bin/sh
set -eu

AP_CONNECTION_NAME="TeslaBox-5G"
AP_INTERFACE="${TESLABOX_AP_INTERFACE:-}"
if [ -z "$AP_INTERFACE" ]; then
	AP_INTERFACE=$(/usr/bin/nmcli -g connection.interface-name connection show id "$AP_CONNECTION_NAME" 2>/dev/null | sed -n '1p' || true)
fi
if [ -z "$AP_INTERFACE" ]; then
	for CANDIDATE in ap0 ap1 ap2 teslabox-ap; do
		if [ ! -e "/sys/class/net/${CANDIDATE}" ]; then
			AP_INTERFACE="$CANDIDATE"
			break
		fi
	done
fi
AP_INTERFACE="${AP_INTERFACE:-teslabox-ap}"

detect_wifi_interface() {
	if [ -n "${TESLABOX_WIFI_INTERFACE:-}" ]; then
		if [ -e "/sys/class/net/${TESLABOX_WIFI_INTERFACE}" ]; then
			printf '%s\n' "$TESLABOX_WIFI_INTERFACE"
		fi
		return
	fi
	/usr/bin/nmcli -t --escape no -f DEVICE,TYPE device status 2>/dev/null |
		awk -F: -v ap="$AP_INTERFACE" '$2 == "wifi" && $1 != ap { print $1; exit }'
}

# 驱动可能在 NetworkManager 初始化物理接口时清理虚拟接口，因此必须等实际客户端接口就绪后再创建热点接口。
attempt=0
WIFI_INTERFACE=""
while [ -z "$WIFI_INTERFACE" ]; do
	WIFI_INTERFACE=$(detect_wifi_interface)
	attempt=$((attempt + 1))
	if [ "$attempt" -ge 30 ]; then
		echo "teslabox-ap-setup: 未发现可用的无线客户端接口" >&2
		exit 1
	fi
	sleep 1
done

if [ "$WIFI_INTERFACE" = "$AP_INTERFACE" ]; then
	echo "teslabox-ap-setup: 客户端接口和热点接口不能同为 $WIFI_INTERFACE" >&2
	exit 1
fi

# 优先等待上游自动连接完成，让单射频热点直接落在相同信道；
# 上游不可用时最多等待 20 秒，不能因此阻止本地管理热点启动。
attempt=0
while [ "$attempt" -lt 20 ]; do
	wlan_state=$(/usr/bin/nmcli -g GENERAL.STATE device show "$WIFI_INTERFACE" 2>/dev/null || true)
	case "$wlan_state" in
		100*) break ;;
	esac
	attempt=$((attempt + 1))
	sleep 1
done

wifi_phy=$(readlink -f "/sys/class/net/${WIFI_INTERFACE}/phy80211")
if [ ! -e "/sys/class/net/${AP_INTERFACE}" ]; then
	phy_name=$(basename "$wifi_phy")
	/usr/sbin/iw phy "$phy_name" interface add "$AP_INTERFACE" type __ap
else
	ap_phy=$(readlink -f "/sys/class/net/${AP_INTERFACE}/phy80211" 2>/dev/null || true)
	if [ "$ap_phy" != "$wifi_phy" ]; then
		echo "teslabox-ap-setup: $AP_INTERFACE 已存在且不属于 $WIFI_INTERFACE 的无线设备" >&2
		exit 1
	fi
fi

# 虚拟接口默认继承物理接口的 MAC；两者同时启用会被内核以地址不唯一拒绝。
base_mac=$(cat "/sys/class/net/${WIFI_INTERFACE}/address")
ap_mac="02:${base_mac#*:}"
/usr/sbin/ip link set "$AP_INTERFACE" down
/usr/sbin/ip link set "$AP_INTERFACE" address "$ap_mac"

/usr/bin/nmcli device set "$AP_INTERFACE" managed yes
/usr/bin/nmcli connection modify id "$AP_CONNECTION_NAME" connection.interface-name "$AP_INTERFACE"
/usr/bin/nmcli --wait 20 connection up id "$AP_CONNECTION_NAME" ifname "$AP_INTERFACE"
