#!/usr/bin/env python3
"""在主服务停止的维护窗口安装事务助手；不修改主程序和用户状态。"""

import argparse
import fcntl
import hashlib
import json
import os
from pathlib import Path
import stat
import subprocess
import tempfile


OLD_HELPER_SHA256 = "81262bbe3a2f560d19639fb1b8a314daee668b624b078b772754e0edc7ac3722"
PROTOCOL = "teslabox-paired-state-v1"
HELPER = "usr/local/libexec/teslabox-update-helper"
MAIN_GUARD = "etc/systemd/system/teslabox.service.d/90-update-state-rollback.conf"
UPDATE_GUARD = "etc/systemd/system/teslabox-update.service.d/90-update-state-rollback.conf"
MAIN_CONTENT = b"[Service]\nExecStartPre=/usr/local/libexec/teslabox-update-helper --recover\n"
UPDATE_CONTENT = b"[Service]\nEnvironmentFile=-/etc/teslabox/teslabox.env\nTimeoutStartSec=240\n"


def digest(data):
    return hashlib.sha256(data).hexdigest()


def safe_path(path):
    if not path.is_absolute():
        raise RuntimeError("需要绝对路径")
    for item in (path, *path.parents):
        if item.is_symlink():
            raise RuntimeError("不允许符号链接: " + str(item))
        if item.is_dir():
            info = item.stat()
            if info.st_uid not in (0, os.geteuid()):
                raise RuntimeError("父目录属于其他用户: " + str(item))
            if info.st_mode & 0o022 and not info.st_mode & stat.S_ISVTX:
                raise RuntimeError("父目录允许其他用户替换文件: " + str(item))


def read_regular(path):
    safe_path(path)
    info = path.stat()
    if not stat.S_ISREG(info.st_mode) or info.st_nlink != 1:
        raise RuntimeError("需要无硬链接的普通文件: " + str(path))
    if info.st_mode & 0o022 or info.st_uid != os.geteuid():
        raise RuntimeError("文件所有权或写权限不安全: " + str(path))
    return path.read_bytes(), stat.S_IMODE(info.st_mode)


def sync_dir(path):
    fd = os.open(path, os.O_RDONLY | os.O_DIRECTORY)
    try:
        os.fsync(fd)
    finally:
        os.close(fd)


def durable_write(path, content, mode):
    safe_path(path)
    durable_mkdir(path.parent)
    fd, name = tempfile.mkstemp(prefix=".rollback-bootstrap-", dir=path.parent)
    try:
        with os.fdopen(fd, "wb") as f:
            os.fchmod(f.fileno(), mode)
            f.write(content)
            f.flush()
            os.fsync(f.fileno())
        os.replace(name, path)
        sync_dir(path.parent)
    finally:
        if os.path.exists(name):
            os.unlink(name)


def durable_mkdir(path, mode=0o700):
    safe_path(path)
    # 已存在也同步，避免上次 mkdir 成功但父目录同步失败后，重试跳过耐久性屏障。
    for item in reversed((path, *path.parents)):
        if item.parent == item:
            continue
        item.mkdir(mode=mode, exist_ok=True)
        safe_path(item)
        sync_dir(item.parent)
    sync_dir(path)


def install(root, source, expected, old_expected, run):
    # 与 Go 助手共用同一 inode 的安装锁，防止两个维护命令交叉撤销守卫。
    lock_path = root / "var/lib/teslabox-system/update-backups/install.lock"
    safe_path(lock_path)
    durable_mkdir(lock_path.parent)
    fd = os.open(lock_path, os.O_CREAT | os.O_RDWR | os.O_NOFOLLOW, 0o600)
    try:
        read_regular(lock_path)
        fcntl.flock(fd, fcntl.LOCK_EX | fcntl.LOCK_NB)
        journal = lock_path.parent / "active/transaction.json"
        if journal.exists():
            content, _ = read_regular(journal)
            state = json.loads(content)
            if state.get("version") != 1 or state.get("phase") not in ("committed", "rolled_back"):
                raise RuntimeError("已有未结束事务，必须先恢复，不能替换守卫")
        return install_locked(root, source, expected, old_expected, run)
    finally:
        os.close(fd)


def install_locked(root, source, expected, old_expected, run):
    """root 仅供隔离测试注入；正式命令固定使用根目录。"""
    source_data, _ = read_regular(source)
    if digest(source_data) != expected:
        raise RuntimeError("新助手与可信发布摘要不一致")
    helper = root / HELPER
    old_data, _ = read_regular(helper)
    if digest(old_data) != old_expected:
        raise RuntimeError("原助手不符合选定迁移基线，未修改文件")
    for service in ("teslabox.service", "teslabox-update.service"):
        state = run(["systemctl", "show", service, "--property=ActiveState", "--value"]).strip()
        if state not in ("inactive", "failed"):
            raise RuntimeError("请先停止主程序并等待安装助手退出: " + service)
    # 只有摘要匹配的候选才允许执行协议探测。
    if run([str(source), "--protocol"]).strip() != PROTOCOL:
        raise RuntimeError("候选助手不支持成对状态回滚")
    relative = [HELPER, UPDATE_GUARD, MAIN_GUARD]
    previous = {}
    for name in relative:
        path = root / name
        safe_path(path)
        previous[name] = read_regular(path) if path.exists() else None
    backup_root = root / "var/lib/teslabox-system/update-bootstrap-backups"
    safe_path(backup_root)
    durable_mkdir(backup_root)
    backup = Path(tempfile.mkdtemp(prefix="bootstrap-", dir=backup_root))
    sync_dir(backup_root)
    for index, name in enumerate(relative):
        old = previous[name]
        if old is not None:
            durable_write(backup / (str(index) + ".before"), old[0], 0o600)
    manifest = {"version": 1, "old_helper_sha256": old_expected, "new_helper_sha256": expected, "files": []}
    for index, name in enumerate(relative):
        old = previous[name]
        manifest["files"].append({"path": name, "exists": old is not None, "mode": old[1] if old else None,
                                  "sha256": digest(old[0]) if old else None, "backup": str(index) + ".before" if old else None})
    durable_write(backup / "manifest.json", (json.dumps(manifest, ensure_ascii=False) + "\n").encode("utf-8"), 0o600)
    try:
        # 助手先落盘，守卫最后启用；任一步断电都不能留下旧助手配新守卫。
        durable_write(helper, source_data, 0o755)
        durable_write(root / UPDATE_GUARD, UPDATE_CONTENT, 0o644)
        durable_write(root / MAIN_GUARD, MAIN_CONTENT, 0o644)
        run(["systemctl", "daemon-reload"])
        guards = run(["systemctl", "show", "teslabox.service", "--property=ExecStartPre", "--value"])
        if "/usr/local/libexec/teslabox-update-helper --recover" not in guards:
            raise RuntimeError("启动守卫未生效")
    except Exception:
        # 恢复先撤销守卫，再恢复旧助手；恢复失败则保持新助手，不伪报已回滚。
        for name in reversed(relative):
            path, old = root / name, previous[name]
            if old is None:
                path.unlink(missing_ok=True)
                if path.parent.exists():
                    sync_dir(path.parent)
            else:
                durable_write(path, old[0], old[1])
        run(["systemctl", "daemon-reload"])
        raise
    return backup


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--helper", required=True, type=Path)
    parser.add_argument("--sha256", required=True, help="来自可信签名发布清单的固定摘要")
    parser.add_argument("--old-sha256", default=OLD_HELPER_SHA256)
    args = parser.parse_args()
    if os.geteuid() != 0:
        parser.error("需要 root 维护权限")
    # 外部请求只能选择经过固定摘要校验的输入文件，不能指定系统输出路径。
    def run(command):
        return subprocess.check_output(command, text=True, timeout=30)
    backup = install(Path("/"), args.helper, args.sha256, args.old_sha256, run)
    print("更新助手与启动守卫已安装；主程序保持停止。备份：" + str(backup))


if __name__ == "__main__":
    main()
