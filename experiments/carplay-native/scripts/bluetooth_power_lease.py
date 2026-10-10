"""协调多个本机功能的蓝牙电源使用权，不操作蓝牙设备本身。"""

from contextlib import contextmanager
import fcntl
import os
from pathlib import Path
import stat


class BluetoothPowerLease:
    """持有共享锁代表仍需蓝牙；关电必须持有排他的短期锁。"""

    def __init__(self, directory=Path("/run/teslabox-bluetooth")):
        self.directory = Path(directory)

    def _open(self):
        self.directory.mkdir(mode=0o700, exist_ok=True)
        info = self.directory.lstat()
        if not stat.S_ISDIR(info.st_mode) or info.st_uid != os.geteuid() or info.st_mode & 0o077:
            raise PermissionError("蓝牙使用权目录不是当前用户的私有目录")
        parent = os.open(self.directory, os.O_RDONLY | os.O_DIRECTORY | os.O_NOFOLLOW | os.O_CLOEXEC)
        try:
            fd = os.open("controller.lock", os.O_RDWR | os.O_CREAT | os.O_NOFOLLOW | os.O_CLOEXEC,
                         0o600, dir_fd=parent)
        finally:
            os.close(parent)
        try:
            info = os.fstat(fd)
            if not stat.S_ISREG(info.st_mode) or info.st_uid != os.geteuid() or info.st_mode & 0o077 or info.st_nlink != 1:
                raise PermissionError("蓝牙使用权锁文件不合法")
            return fd
        except BaseException:
            os.close(fd)
            raise

    @contextmanager
    def hold(self):
        """会话整个生命周期持有使用权，进程退出时内核自动释放。"""
        fd = self._open()
        try:
            fcntl.flock(fd, fcntl.LOCK_SH)
            yield
        finally:
            os.close(fd)

    @contextmanager
    def power_off(self):
        """只有没有任何使用者时才允许关电，检查和实际写电源必须处于同一锁内。"""
        fd = self._open()
        try:
            try:
                fcntl.flock(fd, fcntl.LOCK_EX | fcntl.LOCK_NB)
            except BlockingIOError:
                yield False
            else:
                yield True
        finally:
            os.close(fd)
