#!/usr/bin/env python3
"""先安装签名内核依赖，跨重启确认后才允许主程序事务提交。"""

import hashlib
import json
import os
from pathlib import Path, PurePosixPath
import platform
import shutil
import stat
import subprocess
import sys
import tarfile
import tempfile
import urllib.request


RELEASE = "5.10.160-teslabox3"
VERSION = "0.2.0-rc.1"
APFIX = "31d0551815dc2163db700eb24239d98d003c65f270d05270d079887955b7ebce"
# deb 中 vmlinuz 是封装后的镜像，不能误用构建目录原始 Image 的摘要。
IMAGE = "8fce4f9877dabfd8ad0eef2ee91f5901eb1d51341085834daeb53e8790e32516"
DTB = "81d7869cffcef5b9f5a2e55294f3ffb253453228ecdf6406103de9f0c02bb31d"
POLICY = "/usr/local/share/teslabox/kernel/rc1.json"
HELPER = "/usr/local/libexec/teslabox-update-helper"
MODULE_NAMES = {"8852be", "cfg80211", "mac80211", "compat", "mt7921e", "mt7921-common", "mt76", "mt76-connac-lib", "qrtr", "qrtr-mhi", "mhi"}
FIRMWARE_NAMES = {"BT_RAM_CODE_MT7922_1_1_hdr.bin", "WIFI_MT7922_patch_mcu_1_1_hdr.bin", "WIFI_RAM_CODE_MT7922_1.bin"}


def digest(data):
    return hashlib.sha256(data).hexdigest()


def safe(path):
    for p in (path, *path.parents):
        if p.is_symlink():
            raise RuntimeError("不允许符号链接: " + str(p))
        if p.exists():
            info = p.stat()
            if info.st_uid not in {0, os.geteuid()} or info.st_mode & 0o022 and not info.st_mode & stat.S_ISVTX:
                raise RuntimeError("路径所有权或写权限不安全: " + str(p))


def regular(path):
    safe(path)
    info = path.stat()
    if not stat.S_ISREG(info.st_mode) or info.st_nlink != 1 or info.st_uid != os.geteuid():
        raise RuntimeError("需要普通单链接文件: " + str(path))
    return path.read_bytes()


def sync_dir(path):
    fd = os.open(path, os.O_RDONLY | os.O_DIRECTORY)
    try:
        os.fsync(fd)
    finally:
        os.close(fd)


def mkdir(path):
    safe(path)
    for p in reversed((path, *path.parents)):
        if p.parent == p:
            continue
        p.mkdir(mode=0o700, exist_ok=True)
        sync_dir(p.parent)
    sync_dir(path)


def durable(path, data, mode=0o600):
    mkdir(path.parent)
    safe(path)
    fd, tmp = tempfile.mkstemp(prefix=".kernel-", dir=path.parent)
    try:
        with os.fdopen(fd, "wb") as f:
            os.fchmod(f.fileno(), mode)
            f.write(data)
            f.flush()
            os.fsync(f.fileno())
        os.replace(tmp, path)
        sync_dir(path.parent)
    finally:
        if os.path.exists(tmp):
            os.unlink(tmp)


def write_json(path, value):
    durable(path, (json.dumps(value, sort_keys=True, ensure_ascii=False) + "\n").encode())


def selected_menu(data, target, old):
    """只改默认标签，旧内核入口必须原样保留，不能依赖字典遍历或包排序。"""
    text = data.decode("utf-8")
    label = None
    found = {}
    for line in text.splitlines():
        parts = line.split()
        if not parts:
            continue
        if parts[0].lower() == "label" and len(parts) == 2:
            label = parts[1]
        if parts[0].lower() in ("linux", "kernel") and len(parts) == 2 and label:
            image = PurePosixPath(parts[1]).name
            if image.startswith("vmlinuz-"):
                found.setdefault(image.removeprefix("vmlinuz-"), []).append(label)
    if old not in found or target not in found or len(found[target]) != 1:
        raise RuntimeError("启动菜单缺少唯一新入口或原内核回滚入口")
    lines = [line for line in text.splitlines() if not line.strip().lower().startswith("default ")]
    return ("default " + found[target][0] + "\n" + "\n".join(lines) + "\n").encode()


def validate_archive(path, files):
    """验过签名的精确文件清单仍不允许链接、重复成员或目录穿越。"""
    entries = {}
    with tarfile.open(path, "r:gz") as archive:
        for member in archive:
            if not member.isfile() or member.name not in files or member.name in entries:
                raise RuntimeError("内核归档包含非普通、重复或未签名文件")
            if member.size != files[member.name]["size"] or member.size > 64 << 20:
                raise RuntimeError("内核归档成员长度错误")
            data = archive.extractfile(member).read(member.size + 1)
            if len(data) != member.size or digest(data) != files[member.name]["sha256"]:
                raise RuntimeError("内核归档成员摘要错误")
            entries[member.name] = data
    if set(entries) != set(files):
        raise RuntimeError("内核归档缺少签名成员")
    return entries


class KernelUpgrade:
    # root 和命令注入只供本地隔离测试；生产 CLI 永远固定根目录，不接受用户路径。
    def __init__(self, policy, root=Path("/"), run=None, running=None, boot_id=None):
        self.policy = policy
        self.root = root
        self.run = run or self.command
        self.running = running or platform.release()
        self.boot_id = boot_id or (root / "proc/sys/kernel/random/boot_id").read_text().strip()
        self.base = root / "var/lib/teslabox-system/kernel-upgrade"
        self.journal = self.base / "transaction.json"
        self.menu = root / "boot/extlinux/extlinux.conf"
        self.before = self.base / "extlinux.before"
        self.request_path = self.base / "request.json"

    @staticmethod
    def command(args):
        result = subprocess.run(args, check=True, stdout=subprocess.PIPE, stderr=subprocess.PIPE, timeout=600)
        return result.stdout.decode("utf-8", errors="strict").strip()

    def load(self):
        if not self.journal.exists():
            return None
        data = json.loads(regular(self.journal))
        if data.get("schema") != 1 or data.get("release") != RELEASE or data.get("phase") not in {
            "prepared", "installing", "awaiting_reboot", "boot_verified", "committed", "failed"
        }:
            raise RuntimeError("内核升级事务损坏，拒绝覆盖")
        if data.get("policy_sha256") != digest(json.dumps(self.policy, sort_keys=True).encode()):
            raise RuntimeError("跨重启内核依赖清单发生变化")
        return data

    def save(self, data, phase):
        data["phase"] = phase
        write_json(self.journal, data)

    def destination(self, name):
        if name.startswith("modules/") and name.endswith(".ko") and PurePosixPath(name).stem in MODULE_NAMES and name.count("/") == 1:
            # 不把 lib 目录解到 /；先解析 usrmerge 根，再写固定 ABI 的更新目录。
            modules = (self.root / "lib/modules").resolve(strict=True)
            allowed = {self.root / "lib/modules", self.root / "usr/lib/modules"}
            if modules not in allowed:
                raise RuntimeError("模块根目录不是支持的 usrmerge 路径")
            return modules / RELEASE / "updates/teslabox-wireless" / PurePosixPath(name).name
        if name.startswith("firmware/mediatek/") and PurePosixPath(name).name in FIRMWARE_NAMES and name.count("/") == 2:
            firmware = (self.root / "lib/firmware").resolve(strict=True)
            if firmware not in {self.root / "lib/firmware", self.root / "usr/lib/firmware"}:
                raise RuntimeError("固件根目录无效")
            return firmware / "mediatek" / PurePosixPath(name).name
        if name == "licenses/LICENCE.mediatek":
            return self.root / "usr/share/doc/teslabox-kernel/LICENCE.mediatek"
        raise RuntimeError("不允许的内核依赖目标: " + name)

    def runtime_ok(self):
        if self.running != RELEASE:
            return False
        for name, spec in self.policy["files"].items():
            if name == "kernel.deb":
                continue
            target = self.destination(name)
            if digest(regular(target)) != spec["sha256"]:
                return False
        for module in ("8852be", "cfg80211", "mac80211", "compat", "mt7921e", "btusb", "rtk_btusb"):
            selected = Path(self.run(["modinfo", "-k", RELEASE, "-F", "filename", module]))
            if self.root != Path("/"):
                selected = self.root / str(selected).lstrip("/")
            if RELEASE not in selected.parts:
                raise RuntimeError("模块解析到了其他内核: " + module)
            abi = self.run(["modinfo", "-k", RELEASE, "-F", "vermagic", module]).split()[0]
            if abi != RELEASE:
                raise RuntimeError("模块 ABI 不匹配: " + module)
            if module == "8852be" and digest(regular(selected.resolve(strict=True))) != APFIX:
                raise RuntimeError("最终解析的驱动不是 apfix6，可能被 DKMS 覆盖")
        loaded = self.root / "sys/module/8852be/version"
        if loaded.exists() and not loaded.read_text().strip().endswith("-tbxapfix6"):
            return False
        image = self.root / ("boot/vmlinuz-" + RELEASE)
        if not image.is_file() or not (self.root / ("boot/initrd.img-" + RELEASE)).is_file():
            raise RuntimeError("目标内核或 initramfs 缺失")
        dtb = self.root / ("usr/lib/linux-image-" + RELEASE + "/rockchip/rk3568-rock-3a.dtb")
        if not dtb.is_file():
            raise RuntimeError("ROCK 3A 设备树缺失")
        if digest(regular(image.resolve(strict=True))) != IMAGE or digest(regular(dtb)) != DTB:
            raise RuntimeError("实际内核镜像或设备树不是冻结产物")
        return True

    def download(self):
        mkdir(self.base)
        archive = self.base / "package.tar.gz"
        if archive.exists() and archive.stat().st_size == self.policy["size"] and digest(regular(archive)) == self.policy["sha256"]:
            return archive
        tmp = self.base / "package.download"
        safe(tmp)
        opener = urllib.request.build_opener(urllib.request.ProxyHandler({}))
        with opener.open(self.policy["url"], timeout=25) as response:
            if not response.geturl().startswith("https://"):
                raise RuntimeError("内核下载被重定向到非 HTTPS 地址")
            with tmp.open("wb") as f:
                os.fchmod(f.fileno(), 0o600)
                count = 0
                while True:
                    chunk = response.read(1024 * 1024)
                    if not chunk:
                        break
                    count += len(chunk)
                    if count > self.policy["size"]:
                        raise RuntimeError("下载超过签名长度")
                    f.write(chunk)
                f.flush()
                os.fsync(f.fileno())
        if count != self.policy["size"] or digest(regular(tmp)) != self.policy["sha256"]:
            raise RuntimeError("内核下载长度或摘要校验失败")
        os.replace(tmp, archive)
        sync_dir(self.base)
        return archive

    def remove_request(self):
        safe(self.request_path)
        self.request_path.unlink(missing_ok=True)
        sync_dir(self.base)

    def rollback(self, data):
        # 内核不能启动时可在 U-Boot 菜单选旧入口；能启动时恢复保存的旧默认菜单。
        if "menu_sha256" not in data:
            # 原来已经使用此内核时没有修改启动菜单，不能为程序失败伪造旧内核备份。
            self.save(data, "failed")
            self.remove_request()
            return
        old_menu = regular(self.before)
        if digest(old_menu) != data["menu_sha256"]:
            raise RuntimeError("旧启动菜单备份被修改，保留现场")
        durable(self.menu, old_menu, 0o644)
        for item in reversed(data.get("files_before", [])):
            target = self.destination(item["name"])
            if item["exists"]:
                content = regular(self.base / "before" / item["name"])
                if digest(content) != item["sha256"]:
                    raise RuntimeError("旧模块或固件备份摘要错误")
                durable(target, content, item["mode"])
            else:
                safe(target)
                target.unlink(missing_ok=True)
                if target.parent.exists():
                    sync_dir(target.parent)
        for item in data.get("boot_before", []):
            path = self.boot_target(item["name"])
            if item["exists"]:
                content = regular(self.base / "before-boot" / item["name"])
                if digest(content) != item["sha256"]:
                    raise RuntimeError("原内核启动文件备份摘要错误")
                durable(path, content, item["mode"])
        self.save(data, "failed")
        self.remove_request()

    def prepare(self, request):
        if request["version"] != VERSION or request["sha256"] != self.policy["main_sha256"]:
            raise RuntimeError("内核依赖与升级主程序不匹配")
        data = self.load()
        if data and data["phase"] not in {"committed", "failed"}:
            if data["request"] != request:
                raise RuntimeError("已有不同内核升级请求，不能覆盖")
            if data["phase"] in {"prepared", "installing"}:
                self.rollback(data)
                raise RuntimeError("检测到未完成的内核安装，已恢复旧启动菜单，未升级主程序")
            if data["boot_id"] == self.boot_id and data["phase"] == "awaiting_reboot":
                return {"reboot_required": True}
            try:
                if not self.runtime_ok():
                    raise RuntimeError("重启后未使用 teslabox3 和 apfix6，未升级主程序")
            except Exception:
                self.rollback(data)
                raise
            self.save(data, "boot_verified")
            return {"reboot_required": False}
        try:
            ready = self.runtime_ok()
        except (FileNotFoundError, RuntimeError):
            ready = False
        if ready:
            # 已满足同一签名内核依赖时可复用，但仍需保存与本次程序事务绑定的请求。
            mkdir(self.base)
            data = {"schema": 1, "release": RELEASE, "boot_id": self.boot_id, "request": request,
                    "policy_sha256": digest(json.dumps(self.policy, sort_keys=True).encode()), "files_before": []}
            self.save(data, "boot_verified")
            return {"reboot_required": False}
        if data:
            history = self.base / ("history-" + os.urandom(8).hex())
            history.mkdir(mode=0o700)
            for name in ("transaction.json", "extlinux.before", "before", "before-boot"):
                p = self.base / name
                if p.exists():
                    os.rename(p, history / name)
            sync_dir(history)
            sync_dir(self.base)
        if shutil.disk_usage(self.root / "boot").free < 256 << 20 or shutil.disk_usage(self.root / "var").free < 768 << 20:
            raise RuntimeError("内核安装和回滚备份空间不足")
        old_menu = regular(self.menu)
        # 旧入口必须在任何包脚本执行前确认存在，不能用新生成菜单替代回滚基线。
        selected_menu(old_menu, self.running, self.running)
        # 下载缓存也可能由隔离验证提供，暂存目录不能隐式依赖下载函数创建。
        mkdir(self.base)
        archive = self.download()
        entries = validate_archive(archive, self.policy["files"])
        stage = Path(tempfile.mkdtemp(prefix="stage-", dir=self.base))
        for name, content in entries.items():
            p = stage / name
            durable(p, content)
        for name in entries:
            if name.startswith("modules/"):
                abi = self.run(["modinfo", "-F", "vermagic", str(stage / name)]).split()[0]
                if abi != RELEASE:
                    raise RuntimeError("归档驱动 ABI 不匹配")
        durable(self.before, old_menu)
        data = {"schema": 1, "release": RELEASE, "old_release": self.running, "boot_id": self.boot_id,
                "request": request, "menu_sha256": digest(old_menu), "files_before": [],
                "policy_sha256": digest(json.dumps(self.policy, sort_keys=True).encode()), "boot_before": []}
        for name in ("vmlinuz", "initrd", "dtb"):
            target = self.boot_target(name)
            safe(target)
            item = {"name": name, "exists": target.exists()}
            if item["exists"]:
                content = regular(target)
                item.update(sha256=digest(content), mode=stat.S_IMODE(target.stat().st_mode))
                durable(self.base / "before-boot" / name, content)
            data["boot_before"].append(item)
        for name in entries:
            if name == "kernel.deb":
                continue
            target = self.destination(name)
            safe(target)
            item = {"name": name, "exists": target.exists()}
            if item["exists"]:
                content = regular(target)
                item.update(sha256=digest(content), mode=stat.S_IMODE(target.stat().st_mode))
                durable(self.base / "before" / name, content)
            data["files_before"].append(item)
        self.save(data, "prepared")
        write_json(self.request_path, request)
        try:
            self.save(data, "installing")
            # 不升级发行版、不调用 apt、不卸载驱动；只安装已签名的冻结本地 deb。
            self.run(["dpkg", "-i", str(stage / "kernel.deb")])
            for name, content in entries.items():
                if name != "kernel.deb":
                    durable(self.destination(name), content, 0o644)
            self.run(["depmod", "-a", RELEASE])
            self.run(["update-initramfs", "-u", "-k", RELEASE])
            self.run(["u-boot-update"])
            menu = selected_menu(regular(self.menu), RELEASE, self.running)
            durable(self.menu, menu, 0o644)
            self.save(data, "awaiting_reboot")
        except Exception:
            self.rollback(data)
            raise
        return {"reboot_required": True}

    def boot_target(self, name):
        if name == "vmlinuz":
            return self.root / ("boot/vmlinuz-" + RELEASE)
        if name == "initrd":
            return self.root / ("boot/initrd.img-" + RELEASE)
        if name == "dtb":
            return self.root / ("usr/lib/linux-image-" + RELEASE + "/rockchip/rk3568-rock-3a.dtb")
        raise RuntimeError("未知的启动文件备份角色")

    def commit(self, request):
        data = self.load()
        if not data or data["request"] != request or data["phase"] not in {"boot_verified", "committed"} or not self.runtime_ok():
            raise RuntimeError("新内核尚未确认，不能提交升级成功收据")
        self.save(data, "committed")
        self.remove_request()
        return {}


def main():
    if os.geteuid() != 0 or platform.machine() != "aarch64":
        raise RuntimeError("内核升级仅支持 root 和 ARM64")
    if Path("/proc/device-tree/model").read_bytes().rstrip(b"\0") != b"Radxa ROCK3 Model A":
        raise RuntimeError("设备不是本次内核支持的 ROCK 3A")
    if len(sys.argv) != 2 or sys.argv[1] not in {"prepare", "commit", "rollback", "verify"}:
        raise RuntimeError("未知内核事务操作")
    safe(Path(POLICY))
    safe(Path(HELPER))
    policy = json.loads(KernelUpgrade.command([HELPER, "--kernel-manifest", POLICY]))
    raw = sys.stdin.buffer.read(16385)
    if len(raw) > 16384:
        raise RuntimeError("内核升级请求过长")
    request = json.loads(raw)
    upgrade = KernelUpgrade(policy)
    if sys.argv[1] == "verify":
        if request.get("sha256") != policy["main_sha256"] or not upgrade.runtime_ok():
            raise RuntimeError("强制内核依赖尚未满足")
        result = {"ready": True}
    elif sys.argv[1] == "prepare":
        result = upgrade.prepare(request)
    elif sys.argv[1] == "commit":
        result = upgrade.commit(request)
    else:
        data = upgrade.load()
        if not data or data["request"] != request or data["phase"] == "committed":
            raise RuntimeError("没有可回滚的内核事务")
        upgrade.rollback(data)
        result = {}
    print(json.dumps(result))


if __name__ == "__main__":
    try:
        main()
    except Exception as error:
        print("TeslaBox 强制内核升级失败: " + str(error), file=sys.stderr)
        sys.exit(1)
