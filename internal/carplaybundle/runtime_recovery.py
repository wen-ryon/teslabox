"""独立恢复器在旧主程序启动前运行，不依赖被回退的新版 ELF 或公开运行时。"""
import argparse
import base64
import fcntl
import hashlib
import json
import os
from pathlib import Path
import re
import stat
import subprocess
import tempfile
from contextlib import contextmanager

SERVICE = "teslabox-carplay-experiment.service"
TARGETS = {
    "unit": "etc/systemd/system/" + SERVICE,
    "settings": "var/lib/teslabox-system/carplay/service.json",
    "hardware": "var/lib/teslabox-system/carplay/hardware.json",
    "main_override": "etc/systemd/system/teslabox.service.d/60-carplay-product.conf",
}
JOURNAL = "var/lib/teslabox-system/carplay/install-transaction.json"
MAX_CONFIG = 16384
MAX_JOURNAL = 128 * 1024


def safe_path(root, relative):
    """固定目标也检查所有祖先，避免系统目录链接将写入导向另一处。"""
    root = Path(root).absolute()
    target = root / relative
    if root.resolve() != root or target.resolve() != target:
        raise PermissionError("安装目标及祖先不能包含链接")
    for parent in [target.parent, *target.parent.parents]:
        if parent.exists():
            info = parent.stat()
            if not stat.S_ISDIR(info.st_mode) or info.st_uid not in (0, os.geteuid()) or info.st_mode & 0o022 and not info.st_mode & stat.S_ISVTX:
                raise PermissionError("安装目标祖先权限不安全")
        if parent == root:
            break
    return target


def read_file(path, private=False, limit=MAX_CONFIG):
    """有界读取自有普通文件，不把硬链接或权限异常当成缺失。"""
    if not path.exists():
        if path.is_symlink():
            raise PermissionError("文件不能是悬空链接")
        return None
    before = path.lstat()
    if not stat.S_ISREG(before.st_mode) or before.st_nlink != 1 or before.st_uid != os.geteuid() or before.st_size > limit or private and before.st_mode & 0o077:
        raise PermissionError("安装文件类型、权限或大小不合法")
    fd = os.open(path, os.O_RDONLY | os.O_NOFOLLOW)
    with os.fdopen(fd, "rb") as stream:
        opened = os.fstat(stream.fileno())
        if (opened.st_dev, opened.st_ino) != (before.st_dev, before.st_ino):
            raise PermissionError("安装文件被替换")
        data = stream.read(limit + 1)
    if len(data) > limit:
        raise ValueError("安装文件超过预算")
    return data


def atomic_write(path, data, mode=0o600):
    """先同步内容再替换并同步父目录，断电后只选择完整版本。"""
    path.parent.mkdir(parents=True, mode=0o700, exist_ok=True)
    fd, temporary = tempfile.mkstemp(prefix=".carplay-install-", dir=path.parent)
    try:
        os.fchmod(fd, mode)
        with os.fdopen(fd, "wb") as stream:
            stream.write(data)
            stream.flush()
            os.fsync(stream.fileno())
        os.replace(temporary, path)
        sync_directory(path.parent)
    finally:
        Path(temporary).unlink(missing_ok=True)


def sync_directory(path):
    descriptor = os.open(path, os.O_RDONLY | os.O_DIRECTORY)
    try:
        os.fsync(descriptor)
    finally:
        os.close(descriptor)


@contextmanager
def locked(root):
    """主程序安装与 ExecStartPre 恢复不能同时修改事务。"""
    parent = safe_path(root, JOURNAL).parent
    parent.mkdir(parents=True, mode=0o700, exist_ok=True)
    if parent.stat().st_uid != os.geteuid() or parent.stat().st_mode & 0o077:
        raise PermissionError("安装事务目录必须私有")
    fd = os.open(parent / "install.lock", os.O_CREAT | os.O_RDWR | os.O_NOFOLLOW, 0o600)
    try:
        info = os.fstat(fd)
        if not stat.S_ISREG(info.st_mode) or info.st_nlink != 1 or info.st_uid != os.geteuid() or info.st_mode & 0o077:
            raise PermissionError("事务锁不是私有普通文件")
        fcntl.flock(fd, fcntl.LOCK_EX)
        yield
    finally:
        os.close(fd)


def save(root, journal):
    validate(journal)
    atomic_write(safe_path(root, JOURNAL), (json.dumps(journal, separators=(",", ":")) + "\n").encode())


def validate(journal):
    """事务只能恢复明确的四个目标，不接受清单自定义的系统路径或命令。"""
    if not isinstance(journal, dict) or set(journal) != {"schema", "phase", "main_sha256", "runtime_sha256", "was_active", "was_enabled", "previous"}:
        raise ValueError("安装事务字段不合法")
    if journal["schema"] != 1 or journal["phase"] not in ("prepared", "activated", "rolled_back"):
        raise ValueError("安装事务版本或阶段不合法")
    if any(not isinstance(journal[key], str) or not re.fullmatch("[a-f0-9]{64}", journal[key]) for key in ("main_sha256", "runtime_sha256")):
        raise ValueError("安装事务摘要不合法")
    if any(type(journal[key]) is not bool for key in ("was_active", "was_enabled")) or not isinstance(journal["previous"], dict) or set(journal["previous"]) != set(TARGETS):
        raise ValueError("安装事务快照不完整")
    for value in journal["previous"].values():
        if value is None:
            continue
        if not isinstance(value, dict) or set(value) != {"data", "mode"} or value["mode"] not in (0o600, 0o644):
            raise ValueError("安装事务快照权限不合法")
        if not isinstance(value["data"], str) or len(base64.b64decode(value["data"], validate=True)) > MAX_CONFIG:
            raise ValueError("安装事务快照超过预算")


def load(root):
    data = read_file(safe_path(root, JOURNAL), private=True, limit=MAX_JOURNAL)
    if data is None:
        return None
    journal = json.loads(data)
    validate(journal)
    return journal


def digest_file(path):
    """流式核对安装中的主 ELF，恢复依据真实字节而不是进程名称。"""
    if path.is_symlink() or not path.is_file():
        raise PermissionError("主程序不是普通文件")
    result = hashlib.sha256()
    with path.open("rb") as stream:
        for chunk in iter(lambda: stream.read(1024 * 1024), b""):
            result.update(chunk)
    return result.hexdigest()


def is_forward_update(root, journal):
    """旧助手先备份当前主程序；这能区分下一次升级和上一版真正被回退。"""
    backup = safe_path(root, "var/lib/teslabox-system/update-backups/teslabox.previous.backup")
    return backup.exists() and digest_file(backup) == journal["main_sha256"]


def systemctl(*arguments):
    """恢复只操作独立 CarPlay 服务；不重启 TeslaUSB 或主服务。"""
    result = subprocess.run(["systemctl", *arguments], capture_output=True, timeout=20)
    if result.returncode != 0:
        if arguments[0] in ("stop", "disable"):
            loaded = subprocess.run(["systemctl", "show", SERVICE, "-p", "LoadState", "--value"],
                                    capture_output=True, timeout=5)
            if loaded.returncode == 0 and loaded.stdout.strip() == b"not-found":
                return
        raise RuntimeError("CarPlay 服务管理失败")


def prepare(root, main_sha256, runtime_sha256, was_active, was_enabled):
    """所有旧字节落盘后才允许激活，私有身份和用户设置不属于替换目标。"""
    previous = {}
    for key, relative in TARGETS.items():
        path = safe_path(root, relative)
        data = read_file(path, private=key in ("settings", "hardware"))
        mode = 0o644 if key in ("unit", "main_override") else 0o600
        if data is not None and stat.S_IMODE(path.stat().st_mode) != mode:
            raise PermissionError("待替换文件权限不符合约定")
        previous[key] = None if data is None else dict(data=base64.b64encode(data).decode(), mode=mode)
    journal = dict(schema=1, phase="prepared", main_sha256=main_sha256, runtime_sha256=runtime_sha256,
                   was_active=was_active, was_enabled=was_enabled, previous=previous)
    save(root, journal)
    return journal


def restore(root, journal, command=systemctl):
    """恢复过程可重入；直到文件与服务状态均恢复才完成事务。"""
    validate(journal)
    command("stop", SERVICE)
    for key, relative in TARGETS.items():
        path = safe_path(root, relative)
        value = journal["previous"][key]
        if value is None:
            path.unlink(missing_ok=True)
            if path.parent.exists():
                sync_directory(path.parent)
        else:
            atomic_write(path, base64.b64decode(value["data"], validate=True), value["mode"])
    command("daemon-reload")
    if journal["previous"]["unit"] is not None:
        command("enable" if journal["was_enabled"] else "disable", SERVICE)
        if journal["was_active"]:
            # ExecStartPre 中等待 After=teslabox.service 的旧服务会死锁，因此只入队。
            command("--no-block", "start", SERVICE)
    else:
        command("disable", SERVICE)
    journal["phase"] = "rolled_back"
    save(root, journal)
    return True


def recover(root=Path("/"), command=systemctl):
    """主 ELF 回退或激活期间掉电时恢复原组件；成功记录保留供旧助手回退使用。"""
    with locked(root):
        journal = load(root)
        if journal is None or journal["phase"] == "rolled_back":
            return False
        current = digest_file(safe_path(root, "usr/local/bin/teslabox"))
        if journal["phase"] == "activated" and current == journal["main_sha256"]:
            return False
        if journal["phase"] == "activated" and is_forward_update(root, journal):
            return False
        return restore(root, journal, command)


if __name__ == "__main__":
    parser = argparse.ArgumentParser(description="TeslaBox CarPlay 独立安装恢复")
    parser.parse_args()
    if os.geteuid() != 0:
        raise PermissionError("系统恢复需要 root")
    recover()
