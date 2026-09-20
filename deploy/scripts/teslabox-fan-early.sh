#!/bin/sh
# 在常规系统服务启动前接管 ROCK 3A PWM 风扇，避免风扇功耗与开机峰值叠加。
set -u

chip=/sys/class/pwm/pwmchip8
pwm="${chip}/pwm0"
period=10000

warn() {
	printf '%s\n' "teslabox-fan-early: $*" >&2
}

# 未启用风扇 overlay 的设备直接跳过，不得阻塞 TeslaBox 或系统启动。
[ -d "${chip}" ] || exit 0

if [ ! -d "${pwm}" ]; then
	if ! printf '0' >"${chip}/export" 2>/dev/null; then
		[ -d "${pwm}" ] || { warn "无法导出 PWM 通道，交由主程序稍后重试"; exit 0; }
	fi
	tries=0
	while [ ! -d "${pwm}" ] && [ "${tries}" -lt 20 ]; do
		sleep 0.01
		tries=$((tries + 1))
	done
fi
[ -d "${pwm}" ] || { warn "PWM 通道尚未就绪，交由主程序稍后重试"; exit 0; }

current_period=$(cat "${pwm}/period" 2>/dev/null || printf '0')
current_duty=$(cat "${pwm}/duty_cycle" 2>/dev/null || printf '0')

if [ "${current_period}" != "${period}" ]; then
	printf '0' >"${pwm}/enable" 2>/dev/null || true
	case "${current_period}:${current_duty}" in
		*[!0-9:]*|:*) warn "PWM 当前参数无效，交由主程序稍后重试"; exit 0 ;;
	esac
	if [ "${current_period}" -gt 0 ] && [ "${current_duty}" -gt "${period}" ]; then
		printf '0' >"${pwm}/duty_cycle" 2>/dev/null || { warn "无法重置 PWM 占空比"; exit 0; }
	fi
	printf '%s' "${period}" >"${pwm}/period" 2>/dev/null || { warn "无法设置 PWM 周期"; exit 0; }
fi

# ROCK 3A 风扇口低电平有效；保持 PWM 使能并输出持续高电平表示逻辑 0%。
printf '%s' "${period}" >"${pwm}/duty_cycle" 2>/dev/null || { warn "无法设置停转占空比"; exit 0; }
printf '1' >"${pwm}/enable" 2>/dev/null || { warn "无法启用 PWM"; exit 0; }
exit 0
