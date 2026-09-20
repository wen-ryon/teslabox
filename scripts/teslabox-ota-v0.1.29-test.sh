#!/bin/sh
# 仅用于测试板的 v0.1.29 OTA 验证；配置变更前保留原始更新地址，便于完成后恢复。
set -eu

CONFIG_PATH=/etc/teslabox/teslabox.env
BACKUP_PATH=/var/lib/teslabox/teslabox.env.before-v0.1.29-ota-test
MANIFEST_URL=https://gitee.com/TecheN1/teslabox-releases/releases/download/v0.1.29/latest.json
CATALOG_URL=https://gitee.com/TecheN1/teslabox-releases/raw/master/stable/history.json
MODE=${1:-enable}

if [ "$(id -u)" -ne 0 ]; then
  echo "请使用 sudo 运行此脚本。" >&2
  exit 1
fi

if [ ! -f "$CONFIG_PATH" ]; then
  echo "未找到 TeslaBox 运行配置：$CONFIG_PATH" >&2
  exit 1
fi

restore_config() {
  if [ ! -f "$BACKUP_PATH" ]; then
    echo "未找到测试前备份：$BACKUP_PATH" >&2
    exit 1
  fi
  cp -p "$BACKUP_PATH" "$CONFIG_PATH"
}

enable_test_manifest() {
  if [ ! -f "$BACKUP_PATH" ]; then
    cp -p "$CONFIG_PATH" "$BACKUP_PATH"
  fi

  temp_path=$(mktemp "${CONFIG_PATH}.v0.1.29-test.XXXXXX")
  trap 'rm -f "$temp_path"' EXIT HUP INT TERM
  awk -v manifest_url="$MANIFEST_URL" -v catalog_url="$CATALOG_URL" '
    !/^TESLABOX_UPDATE_(MANIFEST_URL|CATALOG_URL)=/ { print }
    END {
      print "TESLABOX_UPDATE_MANIFEST_URL=" manifest_url
      print "TESLABOX_UPDATE_CATALOG_URL=" catalog_url
    }
  ' "$CONFIG_PATH" >"$temp_path"
  chmod --reference="$CONFIG_PATH" "$temp_path"
  chown --reference="$CONFIG_PATH" "$temp_path"
  mv "$temp_path" "$CONFIG_PATH"
  trap - EXIT HUP INT TERM
}

case "$MODE" in
  enable)
    enable_test_manifest
    ;;
  restore)
    restore_config
    ;;
  *)
    echo "用法：$0 [enable|restore]" >&2
    exit 1
    ;;
esac

systemctl restart teslabox.service
attempt=0
while [ "$attempt" -lt 30 ]; do
  if curl -fsS http://127.0.0.1/healthz >/dev/null; then
    echo "TeslaBox 已重启；当前测试模式：$MODE"
    exit 0
  fi
  attempt=$((attempt + 1))
  sleep 1
done

echo "TeslaBox 重启后未在 30 秒内通过健康检查；请查看 teslabox.service 日志。" >&2
exit 1
