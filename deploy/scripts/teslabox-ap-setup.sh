#!/bin/sh
set -eu

AP_CONNECTION_NAME="TeslaBox-5G"
AP_AUTOCONNECT=$(
	/usr/bin/nmcli -g connection.autoconnect connection show id "$AP_CONNECTION_NAME" 2>/dev/null |
		sed -n '1p' || true
)
case "$AP_AUTOCONNECT" in
	yes|true|1) AP_AUTO_START=true ;;
	no|false|0) AP_AUTO_START=false ;;
	*)
		echo "teslabox-ap-setup: 未找到可管理的热点配置，跳过热点接口探测" >&2
		exit 0
		;;
esac

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
		awk -F: -v ap="$AP_INTERFACE" '
			$2 == "wifi" && $1 != ap { selected = $1; print selected; exit }
			$2 == "wifi" { fallback = $1 }
			END { if (selected == "" && fallback != "") print fallback }
		'
}

# 驱动可能在 NetworkManager 初始化物理接口时清理虚拟接口，因此必须等实际客户端接口就绪后再创建热点接口。
attempt=0
WIFI_INTERFACE=""
while [ -z "$WIFI_INTERFACE" ]; do
	WIFI_INTERFACE=$(detect_wifi_interface)
	attempt=$((attempt + 1))
	if [ "$attempt" -ge 10 ]; then
		echo "teslabox-ap-setup: 未发现无线网卡，跳过热点且保留有线和 TeslaUSB 功能" >&2
		exit 0
	fi
	sleep 1
done

# 热点需要自动启动时等待上游连接完成，让单射频热点直接落在相同信道；
# 热点已关闭时只读取一次当前状态，接口准备不能拖慢主服务升级。
attempt=0
UPSTREAM_CHANNEL=""
UPSTREAM_WAIT_LIMIT=1
if [ "$AP_AUTO_START" = "true" ]; then
	UPSTREAM_WAIT_LIMIT=20
fi
while [ "$attempt" -lt "$UPSTREAM_WAIT_LIMIT" ]; do
	wlan_state=$(/usr/bin/nmcli -g GENERAL.STATE device show "$WIFI_INTERFACE" 2>/dev/null || true)
	case "$wlan_state" in
		100*)
			UPSTREAM_CHANNEL=$(
				/usr/bin/nmcli -t --escape no -f IN-USE,CHAN device wifi list ifname "$WIFI_INTERFACE" --rescan no 2>/dev/null |
					awk -F: '$1 == "*" && $2 ~ /^[0-9]+$/ { print $2; exit }'
			)
			break
			;;
	esac
	attempt=$((attempt + 1))
	sleep 1
done

wifi_phy=$(readlink -f "/sys/class/net/${WIFI_INTERFACE}/phy80211" 2>/dev/null || true)
if [ -z "$wifi_phy" ]; then
	echo "teslabox-ap-setup: $WIFI_INTERFACE 没有关联可管理的无线 PHY" >&2
	exit 0
fi
phy_name=$(basename "$wifi_phy")
WIFI_PHY_INFO=$(/usr/sbin/iw phy "$phy_name" info 2>/dev/null || true)
if [ -z "$WIFI_PHY_INFO" ]; then
	echo "teslabox-ap-setup: 无法读取 $phy_name 能力，跳过热点且不改变上游 WiFi" >&2
	exit 0
fi

AP_PHY_INFO="$WIFI_PHY_INFO"
AP_PHY_NAME="$phy_name"
SEPARATE_AP_RADIO=false
ap_phy=""
if [ -e "/sys/class/net/${AP_INTERFACE}" ]; then
	ap_phy=$(readlink -f "/sys/class/net/${AP_INTERFACE}/phy80211" 2>/dev/null || true)
	if [ -n "$ap_phy" ] && [ "$ap_phy" != "$wifi_phy" ]; then
		AP_PHY_NAME=$(basename "$ap_phy")
		AP_PHY_INFO=$(/usr/sbin/iw phy "$AP_PHY_NAME" info 2>/dev/null || true)
		if [ -z "$AP_PHY_INFO" ]; then
			echo "teslabox-ap-setup: 无法读取独立 AP 接口 $AP_INTERFACE 的 PHY 能力，跳过热点且不改变上游 WiFi" >&2
			exit 0
		fi
		SEPARATE_AP_RADIO=true
	fi
fi

supports_ap_mode() {
	printf '%s\n' "$AP_PHY_INFO" |
		awk '
			/^[[:space:]]*Supported interface modes:/ { in_modes = 1; next }
			in_modes && /^[[:space:]]*\* AP[[:space:]]*$/ { found = 1 }
			in_modes && /^[[:space:]]*[A-Za-z].*:/ { in_modes = 0 }
			END { exit found ? 0 : 1 }
		'
}

ap_sta_channel_limit() {
	printf '%s\n' "$WIFI_PHY_INFO" |
		awk '
			function combination_supported(value, total) {
				if (value !~ /managed/ || value !~ /(^|[[:space:],{}])AP([[:space:],{}]|$)/ ||
					value !~ /total[[:space:]]*<=[[:space:]]*[0-9]+/) return 0
				total = value
				sub(/^.*total[[:space:]]*<=[[:space:]]*/, "", total)
				sub(/[^0-9].*$/, "", total)
				return total + 0 >= 2
			}
			function record_combination(value, limit) {
				if (!combination_supported(value) || value !~ /#channels[[:space:]]*<=[[:space:]]*[0-9]+/) return
				sub(/^.*#channels[[:space:]]*<=[[:space:]]*/, "", value)
				sub(/[^0-9].*$/, "", value)
				limit = value + 0
				if (limit > max_channels) max_channels = limit
			}
			/^[[:space:]]*valid interface combinations:/ { in_combinations = 1; next }
			in_combinations && /^[[:space:]]*\*/ {
				record_combination(combination)
				combination = $0
				next
			}
			in_combinations && combination != "" && /^[[:space:]]+/ {
				combination = combination " " $0
				next
			}
			END {
				record_combination(combination)
				if (max_channels > 0) print max_channels
				exit max_channels > 0 ? 0 : 1
			}
		'
}

mark_ap_unavailable() {
	for CANDIDATE in ap0 ap1 ap2 teslabox-ap; do
		if [ "$CANDIDATE" != "$WIFI_INTERFACE" ] && [ ! -e "/sys/class/net/${CANDIDATE}" ]; then
			# 旧版本可能把 AP 配置写死到仅支持客户端模式的物理网卡；改绑到保留名后，
			# 设置中心会正确显示热点不可用，后续换入支持 AP 的网卡仍可由热插拔服务重新创建。
			/usr/bin/nmcli connection modify id "$AP_CONNECTION_NAME" connection.interface-name "$CANDIDATE" || true
			return
		fi
	done
}

if ! supports_ap_mode; then
	mark_ap_unavailable
	echo "teslabox-ap-setup: $AP_INTERFACE 所属的 $AP_PHY_NAME 不支持 AP 模式，跳过热点且保留上游 WiFi" >&2
	exit 0
fi

AP_STA_CHANNEL_LIMIT=$(ap_sta_channel_limit || true)
case "$AP_STA_CHANNEL_LIMIT" in
	''|*[!0-9]*) AP_STA_CHANNEL_LIMIT=0 ;;
esac

CONCURRENT_MODE=false
CHANNEL_COUPLED=false
if [ "$SEPARATE_AP_RADIO" = "true" ] || [ "$AP_STA_CHANNEL_LIMIT" -gt 0 ]; then
	CONCURRENT_MODE=true
fi
if [ "$SEPARATE_AP_RADIO" != "true" ] && [ "$AP_STA_CHANNEL_LIMIT" -eq 1 ]; then
	CHANNEL_COUPLED=true
fi

channel_supported() {
	channel="$1"
	printf '%s\n' "$AP_PHY_INFO" |
		awk -v marker="[${channel}]" '
			index($0, marker) > 0 && $0 !~ /\(disabled\)/ && $0 !~ /\(no IR\)/ && tolower($0) !~ /radar/ { found = 1 }
			END { exit found ? 0 : 1 }
		'
}

band_for_channel() {
	if [ "$1" -le 14 ]; then
		printf '%s\n' bg
	else
		printf '%s\n' a
	fi
}

preserve_upstream_with_single_role_ap() {
	# 仍把 AP 配置绑定到真实网卡，使设置中心可在用户明确操作后从 STA 切换为 AP。
	/usr/bin/nmcli connection modify id "$AP_CONNECTION_NAME" connection.interface-name "$WIFI_INTERFACE"
	echo "teslabox-ap-setup: $WIFI_INTERFACE 当前不能并发创建热点，已保留上游 WiFi；可在设置中心手动切换" >&2
	exit 0
}

if [ "$SEPARATE_AP_RADIO" = "true" ]; then
	echo "teslabox-ap-setup: $AP_INTERFACE 与 $WIFI_INTERFACE 使用独立 PHY，热点无需跟随上游 WiFi 信道" >&2
elif [ "$CONCURRENT_MODE" = "true" ]; then
	if [ "$AP_INTERFACE" = "$WIFI_INTERFACE" ]; then
		AP_INTERFACE=""
		for CANDIDATE in ap0 ap1 ap2 teslabox-ap; do
			if [ ! -e "/sys/class/net/${CANDIDATE}" ]; then
				AP_INTERFACE="$CANDIDATE"
				break
			fi
		done
		AP_INTERFACE="${AP_INTERFACE:-teslabox-ap}"
	fi
	if [ ! -e "/sys/class/net/${AP_INTERFACE}" ]; then
		if ! /usr/sbin/iw phy "$phy_name" interface add "$AP_INTERFACE" type __ap; then
			if [ -n "$UPSTREAM_CHANNEL" ]; then
				preserve_upstream_with_single_role_ap
			fi
			AP_INTERFACE="$WIFI_INTERFACE"
			CONCURRENT_MODE=false
		fi
	else
		if [ "$ap_phy" != "$wifi_phy" ]; then
			echo "teslabox-ap-setup: $AP_INTERFACE 属于其他无线设备，跳过热点以避免错误接管" >&2
			exit 0
		fi
	fi
elif [ -n "$UPSTREAM_CHANNEL" ]; then
	preserve_upstream_with_single_role_ap
else
	# 只支持单角色时复用物理接口，NetworkManager 会在热点和客户端模式之间互斥切换。
	AP_INTERFACE="$WIFI_INTERFACE"
fi

if [ "$CONCURRENT_MODE" = "true" ] && [ "$SEPARATE_AP_RADIO" != "true" ]; then
	# 虚拟接口默认继承物理接口的 MAC；两者同时启用会被内核以地址不唯一拒绝。
	base_mac=$(cat "/sys/class/net/${WIFI_INTERFACE}/address")
	ap_mac="02:${base_mac#*:}"
	/usr/sbin/ip link set "$AP_INTERFACE" down
	/usr/sbin/ip link set "$AP_INTERFACE" address "$ap_mac"
fi

/usr/bin/nmcli device set "$AP_INTERFACE" managed yes
/usr/bin/nmcli connection modify id "$AP_CONNECTION_NAME" \
	connection.interface-name "$AP_INTERFACE" \
	connection.autoconnect-retries 1

if [ -n "$UPSTREAM_CHANNEL" ] && [ "$CHANNEL_COUPLED" = "true" ]; then
	# 驱动只允许一个活动信道时先跟随已经连接的上游，避免热点首次启动直接失败。
	if ! channel_supported "$UPSTREAM_CHANNEL"; then
		echo "teslabox-ap-setup: 上游信道 $UPSTREAM_CHANNEL 当前被 disabled/no IR/RADAR 限制，不能在同信道创建热点" >&2
		exit 0
	fi
	/usr/bin/nmcli connection modify id "$AP_CONNECTION_NAME" \
		802-11-wireless.band "$(band_for_channel "$UPSTREAM_CHANNEL")" \
		802-11-wireless.channel "$UPSTREAM_CHANNEL"
else
	PROFILE_CHANNEL=$(
		/usr/bin/nmcli -g 802-11-wireless.channel connection show id "$AP_CONNECTION_NAME" 2>/dev/null |
			sed -n '1p' || true
	)
	case "$PROFILE_CHANNEL" in
		''|*[!0-9]*) PROFILE_CHANNEL=0 ;;
	esac
	if ! channel_supported "$PROFILE_CHANNEL"; then
		PROFILE_CHANNEL=""
		# 优先保留 Tesla 友好的 5GHz 非 DFS 信道；硬件或监管域不允许时再退到 2.4GHz。
		for CANDIDATE_CHANNEL in 161 157 153 149 165 48 44 40 36 11 6 1; do
			if channel_supported "$CANDIDATE_CHANNEL"; then
				PROFILE_CHANNEL="$CANDIDATE_CHANNEL"
				break
			fi
		done
		if [ -z "$PROFILE_CHANNEL" ]; then
			echo "teslabox-ap-setup: $WIFI_INTERFACE 没有可用于 AP 的允许信道" >&2
			exit 0
		fi
	fi
	/usr/bin/nmcli connection modify id "$AP_CONNECTION_NAME" \
		802-11-wireless.band "$(band_for_channel "$PROFILE_CHANNEL")" \
		802-11-wireless.channel "$PROFILE_CHANNEL"
fi

if [ "$AP_AUTO_START" != "true" ]; then
	echo "teslabox-ap-setup: 热点保持关闭，已准备可管理接口 $AP_INTERFACE" >&2
	exit 0
fi

/usr/bin/nmcli --wait 20 connection up id "$AP_CONNECTION_NAME" ifname "$AP_INTERFACE"
