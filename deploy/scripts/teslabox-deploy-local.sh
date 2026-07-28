#!/bin/sh
# 此部署器要求调用方提供包体摘要，避免把误传到 /tmp 的任意文件作为 TeslaBox 服务程序安装。
set -eu

installer=/usr/local/sbin/teslabox-deploy-local
current=/usr/local/bin/teslabox
service=teslabox.service

usage() {
  echo "用法：$0 --install" >&2
  echo "      $installer <候选包绝对路径> <SHA-256>" >&2
  exit 2
}

if [ "${1:-}" = "--install" ]; then
  [ "$#" -eq 1 ] || usage
  if [ ! -f "$0" ] || [ -L "$0" ]; then
    echo "[TeslaBox] 安装源脚本不是可信普通文件：$0" >&2
    exit 1
  fi
  echo "[TeslaBox] 正在请求 sudo 授权以安装持久部署器…"
  sudo -v
  sudo install -o root -g root -m 0755 "$0" "$installer"
  echo "[TeslaBox] 持久部署器已安装：$installer"
  exit 0
fi

[ "$#" -eq 2 ] || usage
candidate=$1
expected_sha256=$(printf '%s' "$2" | tr '[:upper:]' '[:lower:]')

case "$candidate" in
  /*) ;;
  *)
    echo "[TeslaBox] 候选包必须使用绝对路径：$candidate" >&2
    exit 1
    ;;
esac
if [ "${#expected_sha256}" -ne 64 ]; then
  echo "[TeslaBox] SHA-256 必须是 64 位十六进制字符串。" >&2
  exit 1
fi
case "$expected_sha256" in
  *[!0123456789abcdef]*)
    echo "[TeslaBox] SHA-256 必须是 64 位十六进制字符串。" >&2
    exit 1
    ;;
esac
if [ ! -f "$candidate" ] || [ -L "$candidate" ]; then
  echo "[TeslaBox] 候选包不存在或不是普通文件：$candidate" >&2
  exit 1
fi
if [ ! -x "$current" ] || [ -L "$current" ]; then
  echo "[TeslaBox] 当前 TeslaBox 程序不存在或不可信：$current" >&2
  exit 1
fi

actual_sha256=$(sha256sum "$candidate" | awk '{print $1}')
if [ "$actual_sha256" != "$expected_sha256" ]; then
  echo "[TeslaBox] 候选包 SHA-256 不匹配：$actual_sha256" >&2
  exit 1
fi

echo "[TeslaBox] 正在请求 sudo 授权…"
sudo -v

stamp=$(date +%Y%m%d-%H%M%S)
backup="/usr/local/bin/teslabox.backup-$stamp"
staged="/usr/local/bin/.teslabox-deploy-$stamp.new"

rollback() {
  echo "[TeslaBox] 新版本健康检查失败，正在恢复备份：$backup" >&2
  sudo install -o root -g root -m 0755 "$backup" "$staged"
  sudo mv -f "$staged" "$current"
  sudo systemctl restart "$service"
  echo "[TeslaBox] 已恢复旧版本，请检查服务日志。" >&2
  exit 1
}

sudo cp -a "$current" "$backup"
sudo install -o root -g root -m 0755 "$candidate" "$staged"
sudo mv -f "$staged" "$current"

echo "[TeslaBox] 已替换程序，正在重启服务…"
if ! sudo systemctl restart "$service"; then
  rollback
fi

sleep 6
if ! systemctl is-active --quiet "$service"; then
  rollback
fi
if ! curl --silent --show-error --fail --max-time 8 http://127.0.0.1/healthz >/dev/null; then
  rollback
fi

echo "[TeslaBox] 部署成功。"
echo "[TeslaBox] 当前程序 SHA-256：$(sha256sum "$current" | awk '{print $1}')"
echo "[TeslaBox] 备份文件：$backup"
echo "[TeslaBox] 如需手动回滚：sudo install -o root -g root -m 0755 '$backup' '$current' && sudo systemctl restart '$service'"
