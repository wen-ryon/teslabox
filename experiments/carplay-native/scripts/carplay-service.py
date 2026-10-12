#!/usr/bin/env python3
"""独立常驻资源管理器：保留持久身份，仅管理自己的 AP 和接收进程。"""

from contextlib import ExitStack, nullcontext
import argparse
import hashlib
import importlib.util
import ipaddress
import json
import os
from pathlib import Path
import re
import signal
import socket
import stat
import subprocess
import tempfile
import threading
import time
import uuid


class AuthenticationUnavailable(RuntimeError):
    """首次联网尚未取得身份时等待，不把外部认证缺失当成安装失败。"""


def write_installation_ready(runtime, settings):
    """仅证明本地服务已完成配置及模块初始化，手机、互联网和首帧另行验收。"""
    descriptor, temporary = tempfile.mkstemp(prefix=".installation-", dir=runtime)
    try:
        with os.fdopen(descriptor, "w", encoding="utf-8") as output:
            process = Path("/proc/self/stat").read_text(encoding="ascii").rsplit(")", 1)[1].split()
            json.dump(dict(pid=os.getpid(), root=settings["root"], start_ticks=process[19]), output)
            output.flush()
            os.fsync(output.fileno())
        os.replace(temporary, runtime / "installation-ready.json")
    finally:
        Path(temporary).unlink(missing_ok=True)


def load_module(name, filename):
    """复用已验证的系统命令和签名边界，不调用限时实验的运行入口。"""
    spec = importlib.util.spec_from_file_location(name, Path(__file__).with_name(filename))
    module = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(module)
    return module


def display_observation(display):
    """只暴露实际接收配置的数值字段，不输出私有配置或设备身份。"""
    result = {key: display[key] for key in ("width", "height", "fps", "width_mm", "height_mm")}
    if any(type(value) is not int or value <= 0 for value in result.values()):
        raise ValueError("显示诊断参数不合法")
    return result


def log_display_profile(display, settings):
    """记录接收端声明的尺寸，不能将配置更新当作手机画面验收。"""
    values = display_observation(display)
    fields = " ".join(f"{key}={value}" for key, value in values.items())
    print(f"CARPLAY_DISPLAY receiver_profile_updated=true revision={settings['revision']} {fields}", flush=True)


def info_response_observation(line):
    """只保留已写出报文的摘要与显示数值，不能将发送成功当作手机布局已改变。"""
    if not line.startswith("REFERENCE_CONTROL "):
        return None
    try:
        value = json.loads(line.removeprefix("REFERENCE_CONTROL "))
    except json.JSONDecodeError:
        return None
    if (not isinstance(value, dict) or value.get("route") != "/info"
            or value.get("method") not in ("GET", "POST") or type(value.get("status")) is not int
            or value["status"] != 200 or not isinstance(value.get("info_sha256"), str)
            or not re.fullmatch(r"[a-f0-9]{64}", value["info_sha256"])):
        return None
    result = dict(status=value["status"], info_sha256=value["info_sha256"])
    display = value.get("info_display")
    fields = ("width", "height", "width_mm", "height_mm", "fps")
    if (isinstance(display, dict) and all(type(display.get(key)) is int
            and 1 <= display[key] <= 10000 for key in fields)):
        result["display"] = {key: display[key] for key in fields}
    return result


def replace_private_profile(path, value):
    """原子替换本次私有会话的显示配置，不把已有文件误当成创建冲突。"""
    info = path.lstat()
    parent = path.parent.lstat()
    if (not stat.S_ISREG(info.st_mode) or info.st_uid != os.geteuid()
            or info.st_mode & 0o077 or info.st_nlink != 1
            or not stat.S_ISDIR(parent.st_mode) or parent.st_uid != os.geteuid()
            or parent.st_mode & 0o077):
        raise PermissionError("显示配置不是本次私有普通文件")
    descriptor, temporary = tempfile.mkstemp(prefix=".profile-", dir=path.parent)
    try:
        with os.fdopen(descriptor, "w", encoding="utf-8") as output:
            json.dump(value, output, ensure_ascii=False)
            output.flush()
            os.fsync(output.fileno())
        current = path.lstat()
        if (current.st_dev, current.st_ino) != (info.st_dev, info.st_ino):
            raise PermissionError("显示配置身份改变")
        os.replace(temporary, path)
    finally:
        Path(temporary).unlink(missing_ok=True)


def restart_receiver(receiver, children, arguments, identity, stopped):
    """手机会话恢复复用热点和桥接，避免车机随手机握手失败被踢下线。"""
    if not receiver.close():
        raise RuntimeError("接收会话未正常退出，拒绝原地重启")
    children.remove(receiver)
    recover_identity_owner(identity)
    receiver = OwnedProcess("receiver", arguments, "REFERENCE_STAGE control_ready")
    children.append(receiver)
    receiver.wait_ready(stopped)
    return receiver


def media_has_first_frame(receiver):
    """桥接在每次输入连接时清零计数；只有本轮参数和有效首帧才算就绪。"""
    media = receiver.latest.get("media", {}) if receiver else {}
    return (media.get("source_connected") is True and media.get("codec_ready") is True
            and type(media.get("frames")) is int and media["frames"] > 0)


def update_upstream_readiness(network, preference, receiver=None):
    """先让手机完成无线 CarPlay 接管，避免个人热点角色先被新 PAN 占用。"""
    connected = media_has_first_frame(receiver)
    network.update_media_state(preference != "bluetooth" or connected, connected)


def prepare_handoff_recovery(network, preference):
    """媒体交接失败才释放 PAN，互联网探测失败不能单独触发此恢复。"""
    # 上次探测失败会清空缓存；自动模式仍让管理器按租约归属执行幂等清理。
    if preference in {"bluetooth", "auto"} or network.upstream and network.upstream.get("source") == "bluetooth":
        return network.prepare_media_recovery()
    return True


def handoff_needs_recovery(receiver, now):
    """认证成功不等于视频可用，手机离开后不能因残留 TCP 永久等待。"""
    if receiver.handoff_at is None or media_has_first_frame(receiver):
        return False
    if receiver.latest.get("session_verified"):
        return now >= receiver.handoff_at + 60
    deadline = receiver.handoff_at + 20
    connected_at = getattr(receiver, "control_connected_at", None)
    if connected_at is not None:
        deadline = min(receiver.handoff_at + 45, max(deadline, connected_at + 15))
    return now >= deadline


class HandoffRecovery:
    """连续交接无控制连接才升级恢复，冷却期跨接收端和热点重建保留。"""

    def __init__(self):
        self.failures = 0
        self.last_rebuild = None

    def observe(self, receiver):
        """控制连接有进展就清零，不把视频流恢复问题归为热点交接失败。"""
        if receiver is not None and (getattr(receiver, "control_connected_at", None) is not None
                or receiver.latest.get("session_verified")
                or receiver.latest.get("media", {}).get("source_connected")):
            self.failures = 0

    def record_timeout(self, receiver, now):
        """调用者完成 PAN 清理后记录本轮失败，最多每五分钟重建一次独立链路。"""
        self.observe(receiver)
        if (receiver is None or not handoff_needs_recovery(receiver, now)
                or getattr(receiver, "control_connected_at", None) is not None
                or receiver.latest.get("session_verified")
                or receiver.latest.get("media", {}).get("source_connected")):
            return False
        self.failures = min(self.failures + 1, 2)
        if self.failures < 2 or self.last_rebuild is not None and now - self.last_rebuild < 300:
            return False
        self.last_rebuild = now
        self.failures = 0
        return True


def selected_phone_is_paired(base, phone):
    """手机不存在或未配对只等待登记，不能因此关闭仍供管理页使用的热点。"""
    if phone is None:
        return False
    if not isinstance(phone, str) or not re.fullmatch(r"(?:[A-F0-9]{2}:){5}[A-F0-9]{2}", phone):
        raise ValueError("所选手机身份不合法")
    target = "/org/bluez/hci0/dev_" + phone.replace(":", "_")
    try:
        return base.command("busctl", "--system", "get-property", "org.bluez", target,
                            "org.bluez.Device1", "Paired") == "b true"
    except (OSError, RuntimeError, subprocess.TimeoutExpired):
        return False


def phone_identity_directory(settings):
    """显式持久目录不能落在可替换程序树内；旧配置保持原路径供原机迁移。"""
    root = Path(settings["root"])
    if "identity_directory" not in settings:
        return root / "reference-livi-private"
    value = settings["identity_directory"]
    if not isinstance(value, str) or not value or not Path(value).is_absolute():
        raise ValueError("手机身份目录必须是绝对路径")
    identity = Path(value)
    if ".." in identity.parts or identity == Path(identity.anchor):
        raise ValueError("手机身份目录不能是根目录或包含上级路径")
    # resolve 同时阻止通过祖先链接将私有目录放回公开程序树。
    if identity.resolve() != identity.absolute():
        raise ValueError("手机身份目录及其祖先不能是链接")
    if identity.resolve() == root.resolve() or root in identity.parents or root.resolve() in identity.resolve().parents:
        raise ValueError("手机身份目录必须独立于程序目录")
    return identity


def receiver_arguments_for(node, root, binary, folder, listen, peer_network, phone, legacy, protocol=2, enrollment_seconds=None, identity_directory=None):
    """只为明确手机生成接收参数，旧绑定仅用于无歧义迁移，不要求新用户预置手机。"""
    if not isinstance(phone, str) or not re.fullmatch(r"(?:[A-F0-9]{2}:){5}[A-F0-9]{2}", phone):
        raise ValueError("接收端需要明确手机身份")
    arguments = [str(node), str(root / "experiments/carplay-native/reference-livi/run-reference.mjs"),
        "--binary", str(binary), "--hardware-config", str(folder / "config.json"),
        "--profile", str(folder / "profile.json"), "--host", listen, "--peer-network", peer_network,
        "--phone-bt", phone, "--identity-directory", str(identity_directory if identity_directory is not None else root / "reference-livi-private"),
        "--auto-connect", "--mfi-protocol", str(protocol)]
    if enrollment_seconds is not None:
        if type(enrollment_seconds) is not int or not 1 <= enrollment_seconds <= 120:
            raise ValueError("首次登记接收窗口必须为 1～120 秒")
        arguments.extend(["--enroll", "--seconds", str(enrollment_seconds)])
    else:
        arguments.append("--daemon")
    if legacy and enrollment_seconds is None:
        arguments.extend(["--legacy-phone-bt", legacy])
    return arguments


def authentication_configuration(settings):
    """旧配置继续使用硬件；产品模式从私有下载清单选择完整的软件身份版本。"""
    value = settings.get("authentication", dict(mode="hardware", protocol=2))
    if not isinstance(value, dict) or set(value) - {"mode", "protocol", "credentials_directory", "software_certificate_sha256", "credentials_manifest"}:
        raise ValueError("认证配置字段不合法")
    mode, protocol = value.get("mode"), value.get("protocol")
    if mode == "auto":
        if set(value) == {"mode", "credentials_manifest"}:
            filename = value["credentials_manifest"]
            if (not isinstance(filename, str) or not Path(filename).is_absolute()
                    or ".." in Path(filename).parts or Path(filename).name != "active.json"):
                raise ValueError("自动认证的私有身份清单路径不合法")
            return dict(value)
        if set(value) != {"mode", "credentials_directory", "software_certificate_sha256"}:
            raise ValueError("自动认证必须分别指定软件身份和证书摘要，协议版本来自实际签名器")
        directory, digest = value["credentials_directory"], value["software_certificate_sha256"]
        if (not isinstance(directory, str) or not Path(directory).is_absolute()
                or not isinstance(digest, str) or not re.fullmatch(r"[a-f0-9]{64}", digest)):
            raise ValueError("自动认证的软件身份或证书摘要不合法")
        return dict(value)
    if not isinstance(mode, str) or mode not in {"hardware", "software"} or type(protocol) is not int or protocol not in {2, 3}:
        raise ValueError("认证模式或协议版本不合法")
    if mode == "hardware":
        if set(value) != {"mode", "protocol"}:
            raise ValueError("硬件认证不能指定软件身份")
    else:
        if set(value) != {"mode", "protocol", "credentials_directory"}:
            raise ValueError("软件认证配置字段不合法")
        directory = value.get("credentials_directory")
        if protocol != 3 or not isinstance(directory, str) or not Path(directory).is_absolute():
            raise ValueError("软件认证只接受 v3 和外部身份绝对路径")
    return dict(value)


def downloaded_authentication(filename):
    """只读取下载器原子发布的完整版本，损坏或路径替换不能降级成硬件认证。"""
    manifest = Path(filename)
    for parent in [manifest.parent, *manifest.parent.parents]:
        if parent.is_symlink():
            raise PermissionError("软件认证清单的上级目录不能为符号链接")
    try:
        before = manifest.lstat()
    except FileNotFoundError:
        return None
    parent = manifest.parent.lstat()
    if (not stat.S_ISREG(before.st_mode) or before.st_nlink != 1 or before.st_size > 4096
            or not stat.S_ISDIR(parent.st_mode)
            or (os.name == "posix" and any(info.st_uid != os.geteuid() or info.st_mode & 0o077
                                           for info in [before, parent]))):
        raise PermissionError("软件认证清单必须由当前用户私有保存")
    descriptor = os.open(manifest, os.O_RDONLY | getattr(os, "O_NOFOLLOW", 0))
    with os.fdopen(descriptor, "rb") as stream:
        actual = os.fstat(stream.fileno())
        if (actual.st_dev, actual.st_ino) != (before.st_dev, before.st_ino):
            raise PermissionError("软件认证清单身份改变")
        data = stream.read(4097)
    if len(data) > 4096:
        raise ValueError("软件认证清单超出大小限制")
    value = json.loads(data)
    if (not isinstance(value, dict) or set(value) != {"version", "credentials_directory", "certificate_sha256"}
            or type(value["version"]) is not int or value["version"] <= 0
            or not isinstance(value["certificate_sha256"], str)
            or not re.fullmatch(r"[a-f0-9]{64}", value["certificate_sha256"])
            or not isinstance(value["credentials_directory"], str)):
        raise ValueError("软件认证清单内容不合法")
    expected = manifest.parent / "versions" / f"{value['version']}-{value['certificate_sha256']}"
    if value["credentials_directory"] != str(expected):
        raise ValueError("软件认证清单不能指向版本目录之外")
    for parent in [expected, *expected.parents]:
        if parent.is_symlink():
            raise PermissionError("软件认证版本目录不能为符号链接")
    if not expected.is_dir() or any(not (expected / name).is_file() for name in ("identity.pk8", "certificate.p7b")):
        raise ValueError("软件认证清单指向的版本不完整")
    return dict(mode="auto", credentials_directory=str(expected),
                software_certificate_sha256=value["certificate_sha256"])


def hardware_authentication_present(hardware, usb_root=Path("/sys/bus/usb/devices")):
    """沿用原生 CH341 选择条件；枚举异常和多个匹配不能伪装为未插小板。"""
    config = hardware.get("mfi", {})
    if config.get("transport") != "ch341":
        raise ValueError("自动认证目前只支持 CH341 USB 小板；其他硬件继续使用显式模式")
    for key in ("usb_bus", "usb_address"):
        if config.get(key) is not None and (type(config[key]) is not int or not 1 <= config[key] <= 255):
            raise ValueError("小板 USB 选择条件不合法")
    if not usb_root.is_dir():
        raise RuntimeError("无法读取 USB 枚举，不能判断小板是否存在")
    matches = 0
    for device in usb_root.iterdir():
        if not (device / "idVendor").exists():
            continue
        if (int((device / "idVendor").read_text().strip(), 16) != 0x1a86
                or int((device / "idProduct").read_text().strip(), 16) != 0x5512):
            continue
        if config.get("usb_bus") is not None and int((device / "busnum").read_text()) != config["usb_bus"]:
            continue
        if config.get("usb_address") is not None and int((device / "devnum").read_text()) != config["usb_address"]:
            continue
        matches += 1
    if matches > 1:
        raise ValueError("匹配到多块认证小板，请先明确 USB 选择条件")
    return matches == 1


def select_authentication(settings, hardware, usb_root=Path("/sys/bus/usb/devices")):
    """自动模式优先软件 v3；身份缺失才选择实体签名器，证书始终绑定所选身份。"""
    configured = authentication_configuration(settings)
    if configured["mode"] != "auto":
        return dict(configured, certificate_sha256=settings["certificate_sha256"])
    if "credentials_manifest" in configured:
        downloaded = downloaded_authentication(configured["credentials_manifest"])
        if downloaded is None:
            if hardware_authentication_present(hardware, usb_root):
                return dict(mode="hardware", protocol=None, certificate_sha256=settings.get("certificate_sha256"))
            raise AuthenticationUnavailable("等待登记服务下载软件 v3 身份，且没有硬件兼容签名器")
        configured = downloaded
    directory = Path(configured["credentials_directory"])
    if not directory.is_symlink() and directory.is_dir():
        files = [directory / name for name in ("identity.pk8", "certificate.p7b")]
        if any(path.is_symlink() for path in files):
            raise PermissionError("软件认证身份文件不能为符号链接")
        if all(path.is_file() and not path.is_symlink() for path in files):
            # 格式和公私钥匹配由软件签名器检查；权限错误不能被当作缺失而静默绕过。
            if os.name == "posix" and any(path.stat().st_uid != os.geteuid() or path.stat().st_mode & 0o077 for path in [directory, *files]):
                raise PermissionError("软件认证身份必须由当前用户私有保存")
            return dict(mode="software", protocol=3, credentials_directory=configured["credentials_directory"],
                        certificate_sha256=configured["software_certificate_sha256"])
    elif directory.is_symlink():
        raise PermissionError("软件认证身份目录不能为符号链接")
    if hardware_authentication_present(hardware, usb_root):
        return dict(mode="hardware", protocol=None, certificate_sha256=settings.get("certificate_sha256"))
    raise AuthenticationUnavailable("v3 软件身份不可用，且没有硬件兼容签名器")


def bind_local_hardware_certificate(settings, digest):
    """新机只登记实际本机小板摘要，后续更换需显式处理，不能复制开发板固定身份。"""
    if "credentials_manifest" not in authentication_configuration(settings):
        raise ValueError("旧硬件配置不能自动登记证书")
    parent = Path(settings["hardware_config"]).parent
    for ancestor in [parent, *parent.parents]:
        if ancestor.is_symlink():
            raise PermissionError("硬件证书目录不能包含链接")
    info = parent.stat()
    if info.st_uid != os.geteuid() or info.st_mode & 0o077:
        raise PermissionError("硬件证书目录必须私有")
    path = parent / "hardware-certificate.sha256"
    try:
        descriptor = os.open(path, os.O_WRONLY | os.O_CREAT | os.O_EXCL | os.O_NOFOLLOW, 0o600)
    except FileExistsError:
        before = path.lstat()
        if (not stat.S_ISREG(before.st_mode) or before.st_uid != os.geteuid()
                or before.st_mode & 0o077 or before.st_nlink != 1 or before.st_size != 65):
            raise PermissionError("硬件证书登记文件异常")
        descriptor = os.open(path, os.O_RDONLY | os.O_NOFOLLOW)
        with os.fdopen(descriptor, "rb") as stream:
            current = os.fstat(stream.fileno())
            if (current.st_dev, current.st_ino) != (before.st_dev, before.st_ino) or stream.read(66) != (digest + "\n").encode():
                raise ValueError("本机硬件证书摘要改变")
    else:
        with os.fdopen(descriptor, "wb") as stream:
            stream.write((digest + "\n").encode())
            stream.flush()
            os.fsync(stream.fileno())
        descriptor = os.open(parent, os.O_RDONLY | os.O_DIRECTORY)
        try:
            os.fsync(descriptor)
        finally:
            os.close(descriptor)
    return digest


def authentication_protocol_from_oracle(mfi, authentication, deadline):
    """从固定本机签名器读取版本；硬件自动识别 v2/v3，软件仍严格要求 v3。"""
    with socket.create_connection(("127.0.0.1", 5290), mfi.remaining(deadline, 1)) as stream:
        stream.sendall(b"\x03\x00\x00")
        header = mfi.receive(stream, 3, deadline)
        if header != b"\x00\x00\x02":
            raise ValueError("签名器协议版本响应不合法")
        version = mfi.receive(stream, 2, deadline)
    if version[0] not in {2, 3} or (authentication["protocol"] is not None and version[0] != authentication["protocol"]):
        raise ValueError("签名器协议版本与所选认证方式不一致")
    return version[0]


def oracle_arguments_for(binary, config, authentication):
    """选择一种签名器；软件分支不携带硬件配置，也不启动硬件签名进程。"""
    if authentication["mode"] == "software":
        return [str(binary), "mfi-software-oracle", "--identity-directory",
                authentication["credentials_directory"], "--listen", "127.0.0.1:5290"]
    return [str(binary), "--config", str(config), "mfi-oracle"]


def start_authentication_oracle(binary, config, settings, hardware, mfi, children, stopped):
    """只在手机认证开始前回退；旧签名进程彻底退出后才能占用同一个本机端口。"""
    authentication = select_authentication(settings, hardware)
    automatic = authentication_configuration(settings)["mode"] == "auto"
    while True:
        oracle = OwnedProcess("oracle", oracle_arguments_for(binary, config, authentication))
        children.append(oracle)
        deadline = time.monotonic() + 8
        try:
            while True:
                if oracle.child.poll() is not None or stopped.is_set():
                    raise RuntimeError("签名服务已退出或停止")
                try:
                    certificate = mfi.request(("127.0.0.1", 5290), 1, b"", deadline)
                    break
                except ConnectionRefusedError:
                    if stopped.wait(.05) or time.monotonic() >= deadline:
                        raise TimeoutError("签名服务启动超时")
            if automatic:
                authentication["protocol"] = authentication_protocol_from_oracle(mfi, authentication, deadline)
        except (OSError, RuntimeError, TimeoutError, EOFError):
            if not automatic or authentication["mode"] != "software" or stopped.is_set():
                raise
            if not oracle.close():
                raise RuntimeError("v3 签名进程尚未退出，不能启动兼容签名器")
            children.remove(oracle)
            if not hardware_authentication_present(hardware):
                raise RuntimeError("v3 签名器不可用，且没有硬件兼容签名器") from None
            authentication = dict(mode="hardware", protocol=None, certificate_sha256=settings.get("certificate_sha256"))
            print("CARPLAY_AUTH v3_unavailable=true fallback=hardware", flush=True)
            continue
        # 摘要或协议校验异常不是不可用，不能通过降级吞掉身份不一致。
        actual_digest = hashlib.sha256(certificate).hexdigest()
        if authentication["mode"] == "hardware" and authentication["certificate_sha256"] is None:
            authentication["certificate_sha256"] = bind_local_hardware_certificate(settings, actual_digest)
        if actual_digest != authentication["certificate_sha256"]:
            raise ValueError("签名器证书摘要改变")
        print(f"CARPLAY_AUTH selected={authentication['mode']} protocol={authentication['protocol']}", flush=True)
        return oracle, authentication


def replace_phone_receiver(receiver, children, arguments, identity, stopped):
    """新增、切换或等待手机只操作接收端，不释放 AP、签名服务和浏览器桥接。"""
    if receiver is not None:
        if not receiver.close():
            raise RuntimeError("接收会话未正常退出，拒绝切换手机")
        children.remove(receiver)
    if arguments is None:
        return None
    recover_identity_owner(identity)
    replacement = OwnedProcess("receiver", arguments, "REFERENCE_STAGE control_ready")
    children.append(replacement)
    replacement.wait_ready(stopped)
    return replacement


def load_settings(path):
    """启动配置必须私有且字段明确，拒绝误用其他实验文件。"""
    info = path.lstat()
    if not stat.S_ISREG(info.st_mode) or info.st_uid != os.geteuid() or info.st_mode & 0o077:
        raise PermissionError("服务配置必须是当前用户的私有普通文件")
    if info.st_size > 16384:
        raise ValueError("服务配置超过预算")
    settings = json.loads(path.read_text(encoding="utf-8"))
    expected = {"root", "hardware_config", "binary_sha256", "node_sha256", "vehicle_channels"}
    optional = {"ap_interface", "upstream_interface", "user_settings", "phone_ip", "phone_bt", "authentication", "identity_directory", "certificate_sha256", "ap_uuid"}
    if not isinstance(settings, dict) or not expected <= set(settings) or set(settings) - expected - optional:
        raise ValueError("服务配置字段不匹配")
    for key, default in (("ap_interface", "ap0"), ("upstream_interface", "wlan0")):
        value = settings.get(key, default)
        if not isinstance(value, str) or not re.fullmatch(r"[A-Za-z0-9][A-Za-z0-9_.-]{0,14}", value):
            raise ValueError("服务无线接口名称不合法")
    for key in ("binary_sha256", "node_sha256", "certificate_sha256"):
        if key not in settings:
            if key != "certificate_sha256" or "credentials_manifest" not in authentication_configuration(settings):
                raise ValueError("旧认证配置必须固定本机证书摘要")
            continue
        if not isinstance(settings[key], str) or not re.fullmatch(r"[a-f0-9]{64}", settings[key]):
            raise ValueError("服务配置摘要不合法")
    for key in ("ap_uuid",):
        if key not in settings:
            if "credentials_manifest" not in authentication_configuration(settings):
                raise ValueError("旧服务配置必须固定热点 UUID")
            continue
        if str(uuid.UUID(settings[key])) != settings[key]:
            raise ValueError("服务网络 UUID 不合法")
    if "phone_bt" in settings and (not isinstance(settings["phone_bt"], str)
            or not re.fullmatch(r"(?:[A-F0-9]{2}:){5}[A-F0-9]{2}", settings["phone_bt"])):
        raise ValueError("服务手机蓝牙地址不合法")
    # 旧配置保留格式校验供迁移使用，接收会话不再读取或依赖该地址。
    if "phone_ip" in settings:
        try:
            if not isinstance(settings["phone_ip"], str):
                raise ValueError()
            phone_address = ipaddress.IPv4Address(settings["phone_ip"])
            first, second = phone_address.packed[:2]
            if not (first == 10 or first == 172 and 16 <= second <= 31 or first == 192 and second == 168):
                raise ValueError()
        except ValueError:
            raise ValueError("旧服务手机 IP 必须是单个私有 IPv4 地址") from None
    channels = settings["vehicle_channels"]
    if not isinstance(channels, list) or not 1 <= len(channels) <= 64 or any(type(channel) is not int or not (1 <= channel <= 13 or 36 <= channel <= 177) for channel in channels) or len(set(channels)) != len(channels):
        raise ValueError("必须显式配置车机支持的信道")
    for key in ("root", "hardware_config", "user_settings"):
        if key not in settings:
            continue
        if not isinstance(settings[key], str) or not Path(settings[key]).is_absolute():
            raise ValueError("服务路径必须是绝对路径")
    phone_identity_directory(settings)
    authentication_configuration(settings)
    return settings


def video_input_failure(line):
    """只记录本机视频校验错误的固定类别，原始错误文本不进入常驻日志。"""
    prefix = "CarPlay 视频输入结束: "
    if not line.startswith(prefix):
        return None
    reasons = {
        "视频 NTP 倒退、重复或超出 MPEG-TS 回卷判定范围": "timestamp_discontinuity",
        "视频时间戳间隔低于 90kHz 时间基精度": "timestamp_precision",
        "Canvas 首帧或配置变化必须从 IDR 开始": "configuration_without_idr",
        "Canvas 输入缺少 AVC 图像": "missing_picture",
        "Canvas 输入长度或 IDR 标记不匹配": "frame_metadata_mismatch",
        "Canvas 输入缺少完整 SPS/PPS": "missing_parameters",
        "Canvas 输入混用了不同 AVC profile/level": "mixed_parameters",
        "时间戳输入包含畸形 AVC NAL": "invalid_nal",
        "时间戳输入不是 Annex-B": "invalid_annex_b",
        "Annex-B 之前有未知内容": "invalid_annex_b",
        "Canvas 半包头超时": "partial_header_timeout",
        "Canvas 半帧超时": "partial_frame_timeout",
        "Canvas 首帧超时": "first_frame_timeout",
        "Canvas 事件接收端阻塞": "event_delivery_timeout",
        "Canvas 事件接收端关闭": "event_delivery_closed",
        "Canvas IPC 帧超过上限": "frame_budget",
        "Canvas AVC 参数集超过预算": "parameter_budget",
        "Canvas 参数集补齐后的帧超过上限": "frame_budget",
    }
    return reasons.get(line.removeprefix(prefix), "unclassified_input_failure")


class OwnedProcess:
    """每个资源使用独立进程组，强制清理也不会波及其他功能。"""

    def __init__(self, name, arguments, marker=None):
        self.name, self.marker = name, marker
        self.ready, self.failed = threading.Event(), threading.Event()
        self.latest = {}
        self.handoff_at = None
        self.control_connected_at = None
        self.child = subprocess.Popen(arguments, stdin=subprocess.DEVNULL, stdout=subprocess.PIPE,
                                      stderr=subprocess.PIPE, start_new_session=True)
        self.readers = [threading.Thread(target=self._read, args=(stream,), daemon=True)
                        for stream in (self.child.stdout, self.child.stderr)]
        for reader in self.readers:
            reader.start()

    def _read(self, stream):
        while True:
            body = stream.readline(8193)
            if not body:
                return
            if len(body) > 8192:
                self.failed.set()
                return
            line = body.decode("utf-8", errors="replace").rstrip("\r\n")
            screen_timeout = re.fullmatch(
                r"REFERENCE_SCREEN_TIMEOUT reason=(screen_partial_timeout|screen_partial_budget_timeout) "
                r"header_bytes=(\d{1,3}) body_left=(\d{1,8}) idle_ms=(\d{1,12}) elapsed_ms=(\d{1,12}) blocked=(true|false)", line)
            if self.name == "receiver" and screen_timeout:
                self.latest["screen_timeout"] = dict(reason=screen_timeout[1],
                    header_bytes=int(screen_timeout[2]), body_left=int(screen_timeout[3]),
                    idle_ms=int(screen_timeout[4]), elapsed_ms=int(screen_timeout[5]), blocked=screen_timeout[6] == "true")
                print(line, flush=True)
            information = info_response_observation(line) if self.name == "receiver" else None
            if information and information != self.latest.get("information"):
                self.latest["information"] = information
                fields = "".join(f" sent_{key}={value}" for key, value in information.get("display", {}).items())
                print("CARPLAY_DISPLAY info_response_written=true info_sha256=" + information["info_sha256"] + fields, flush=True)
            failure = video_input_failure(line) if self.name == "bridge" else None
            if failure:
                self.latest["input_failure"] = failure
                print(f"BRIDGE_INPUT reason={failure}", flush=True)
            flow = re.fullmatch(r"BRIDGE_FLOW sent=(\d{1,20}) ack=(\d{1,20}) blocked=(\d{1,20}) pending=(\d{1,2}) ack_max_ms=(\d{1,20})", line)
            if flow:
                self.latest["flow"] = dict(zip(("sent", "ack", "blocked", "pending", "ack_max_ms"), map(int, flow.groups())))
                print(line, flush=True)
            render = re.fullmatch(r"BRIDGE_RENDER rendered=(\d{1,10}) decode_ms=(\d{1,5}) draw_ms=(\d{1,5}) renderer=(webgl2|2d)", line)
            if render:
                self.latest["render"] = dict(rendered=int(render[1]), decode_ms=int(render[2]), draw_ms=int(render[3]), renderer=render[4])
                print(line, flush=True)
            if self.marker and line.startswith(self.marker):
                self.ready.set()
            # 兼容现有二进制的固定通知；不转发原文，也不将可用通知当作视频认证成功。
            wireless = {
                "手机无线 CarPlay 可用状态 available=true": ("carplay_available", "true"),
                "手机无线 CarPlay 可用状态 available=false": ("carplay_available", "false"),
                "iAP2 无线传输更新可用 ready=true": ("wireless_transport_available", "true"),
                "iAP2 无线传输更新不可用 ready=true": ("wireless_transport_available", "false"),
            }.get(line) if self.name == "receiver" else None
            if wireless:
                print(f"CARPLAY_WIRELESS observed={wireless[0]} value={wireless[1]}", flush=True)
            # 只保留固定拨号结果和协议枚举，便于区分蓝牙已连接与 CarPlay 已建立。
            if re.fullmatch(r"REFERENCE_BT_DIAL (requested|retry)", line):
                self.latest["bluetooth_dial"] = line.rsplit(" ", 1)[1]
                print(line, flush=True)
            timeline = re.fullmatch(
                r"跨协议诊断 seq=\d{1,20} elapsed_us=\d{1,20} connection=\d{1,20} "
                r"channel=(bluetooth|iap_tunnel|control|event) "
                r"stage=(opened|link_ready|authenticated|identified|carplay_available|carplay_unavailable|wireless_transport_available|wireless_transport_unavailable|wifi_sent|handoff_sent|info_written|setup_written|record_written|teardown_received|peer_eof|disconnect_requested|closed) "
                r"status=\d{1,5} reason=(none|peer_eof|local_cancelled|completed|connection_reset|connection_aborted|timed_out|invalid_data|broken_pipe|interrupted|unexpected_eof|io_error)", line)
            if timeline:
                self.latest["protocol"] = dict(channel=timeline[1], stage=timeline[2], reason=timeline[3])
                if timeline[1] == "bluetooth" and timeline[2] == "handoff_sent" and self.handoff_at is None:
                    # 重复通知不能无限推迟同一次交接的截止时间。
                    self.handoff_at = time.monotonic()
                print(line, flush=True)
            # 常驻日志只保留固定阶段和最后媒体计数，避免积累原始协议正文。
            if re.fullmatch(r"REFERENCE_STAGE [a-z_]{1,80}", line):
                self.latest["stage"] = line.removeprefix("REFERENCE_STAGE ")
                if self.latest["stage"] in {"session_closed", "owned_resources_stopped", "reconnect_wait", "supervisor_stopped"}:
                    # 累计成功次数只作诊断；上一轮认证不能替代新一轮手机交接。
                    self.latest["session_verified"] = False
                    self.latest.pop("media", None)
                    self.handoff_at = None
                    self.control_connected_at = None
                if line == "REFERENCE_STAGE control_ready":
                    # stdout 的就绪通知可能晚于 stderr 的认证阶段，不能清掉已经建立的当前会话。
                    self.latest.setdefault("session_verified", False)
                if line == "REFERENCE_STAGE control_connected" and getattr(self, "control_connected_at", None) is None:
                    self.control_connected_at = time.monotonic()
                if line == "REFERENCE_STAGE pair_verified":
                    self.latest["verified_sessions"] = self.latest.get("verified_sessions", 0) + 1
                    self.latest["session_verified"] = True
                print(line, flush=True)
            match = re.fullmatch(r"REFERENCE_MEDIA frames=(\d{1,16}) input_bytes=(\d{1,16}) source_connected=(true|false) codec_ready=(true|false)", line)
            if match:
                self.latest["media"] = dict(frames=int(match[1]), input_bytes=int(match[2]),
                                            source_connected=match[3] == "true", codec_ready=match[4] == "true")

    def wait_ready(self, stopped, seconds=15):
        """停止请求和启动失败都可打断等待，不把端口监听当成媒体成功。"""
        deadline = time.monotonic() + seconds
        while not stopped.is_set() and time.monotonic() < deadline:
            if self.child.poll() is not None or self.failed.is_set():
                raise RuntimeError("自有进程在就绪前失败")
            if self.ready.wait(.05):
                return
        raise TimeoutError("服务进程就绪超时或已停止")

    def close(self):
        """先给会话自行回收时间，再只清理本次创建的进程组。"""
        forced = False
        if self.child.poll() is None:
            try:
                self.child.terminate()
            except ProcessLookupError:
                pass
            try:
                self.child.wait(timeout=15)
            except subprocess.TimeoutExpired:
                forced = True
        # 主进程意外退出时也可能留下媒体子进程，进程组不能仅按主 PID 判断。
        try:
            os.killpg(self.child.pid, signal.SIGKILL)
            forced = True
        except ProcessLookupError:
            pass
        self.child.wait(timeout=3)
        for reader in self.readers:
            reader.join(timeout=2)
        for stream in (self.child.stdout, self.child.stderr):
            stream.close()
        print(f"CARPLAY_SERVICE child={self.name} stopped=true forced={str(forced).lower()}", flush=True)
        return not forced


def write_status(runtime, value):
    """原子替换私有状态，读取者不会看到半个 JSON。"""
    target = runtime / "status.json"
    temporary = runtime / ("status-" + uuid.uuid4().hex)
    fd = os.open(temporary, os.O_WRONLY | os.O_CREAT | os.O_EXCL, 0o600)
    try:
        with os.fdopen(fd, "w", encoding="utf-8") as output:
            json.dump(value, output)
        os.replace(temporary, target)
    finally:
        temporary.unlink(missing_ok=True)


def publish_proxy_token(runtime, source):
    """私有原子入口供项目代理读取，不将令牌交给浏览器。"""
    token = source.read_text(encoding="utf-8").strip()
    if len(token) != 64 or any(value not in "0123456789abcdefABCDEF" for value in token):
        raise ValueError("浏览器代理令牌格式无效")
    temporary = runtime / ("browser-token-" + uuid.uuid4().hex)
    try:
        fd = os.open(temporary, os.O_WRONLY | os.O_CREAT | os.O_EXCL, 0o600)
        with os.fdopen(fd, "w", encoding="utf-8") as output:
            output.write(token)
        os.replace(temporary, runtime / "browser-token")
    finally:
        temporary.unlink(missing_ok=True)


def check_listener_available(address):
    """允许刚关闭会话的 TIME_WAIT，仍拒绝其他进程的实际监听端口。"""
    with socket.socket() as guard:
        guard.setsockopt(socket.SOL_SOCKET, socket.SO_REUSEADDR, 1)
        try:
            guard.bind(address)
        except OSError as error:
            print(f"CARPLAY_SERVICE bind_failed_port={address[1]} errno={error.errno}", flush=True)
            raise


def recover_identity_owner(directory):
    """服务锁已独占后，只回收无人持有的旧空锁，保留配对身份和未知锁文件。"""
    lock = directory / ".livi-reference-owner"
    try:
        original = lock.lstat()
    except FileNotFoundError:
        return
    if not stat.S_ISREG(original.st_mode) or original.st_uid != os.geteuid() or stat.S_IMODE(original.st_mode) != 0o600 or original.st_nlink != 1 or original.st_size != 0:
        raise PermissionError("身份所有权锁不是已知私有空文件")
    identity = original.st_dev, original.st_ino
    for process in Path("/proc").iterdir():
        if not process.name.isdecimal():
            continue
        try:
            descriptors = list((process / "fd").iterdir())
        except FileNotFoundError:
            continue
        for descriptor in descriptors:
            try:
                info = descriptor.stat()
            except FileNotFoundError:
                continue
            if (info.st_dev, info.st_ino) == identity:
                raise RuntimeError("身份所有权锁仍被进程持有")
    try:
        current = lock.lstat()
    except FileNotFoundError:
        return
    if (current.st_dev, current.st_ino) != identity or current.st_size != 0 or current.st_nlink != 1:
        raise RuntimeError("身份所有权锁在检查时已改变")
    lock.unlink()
    print("CARPLAY_SERVICE stale_identity_owner_recovered=true", flush=True)


def start_peer_return_path(interface, peer, host, addresses, runtime, receiver, stopped):
    """只为已有明确手机地址观察回包，不根据待登记邻居猜测手机身份。"""
    if not peer:
        return None
    try:
        module = load_module("carplay_peer_return", "carplay_peer_return.py")
        return module.PeerReturnPath(interface, peer, host, addresses, runtime, receiver, stopped).start()
    except Exception:
        print("CARPLAY_PEER_RETURN unavailable=true", flush=True)
        return None


def run(settings, runtime, stopped):
    """围绕常驻接收端持有共享电源使用权，退出时只回收自己的资源。"""
    base = load_module("service_tools", "board-wireless-mfi-check.py")
    mfi = load_module("service_mfi", "board-mfi-sign-check.py")
    power = load_module("service_power", "bluetooth_power_lease.py").BluetoothPowerLease()
    network_module = load_module("service_network", "carplay_network_lease.py")
    root, hardware_path = Path(settings["root"]), Path(settings["hardware_config"])
    identity_directory = phone_identity_directory(settings)
    expected_ap_interface = settings.get("ap_interface")
    binary, node = root / "teslabox-carplay", root / "runtime/bin/node"
    for path, digest in ((binary, settings["binary_sha256"]), (node, settings["node_sha256"])):
        if path.is_symlink() or hashlib.sha256(path.read_bytes()).hexdigest() != digest:
            raise ValueError("服务候选摘要不一致")
    base.private_directory(runtime)
    base.private_directory(identity_directory)
    if base.command("systemctl", "show", "teslabox-bt-trigger.service", "-p", "ActiveState", "--value") != "inactive":
        raise ValueError("旧蓝牙触发器仍在运行，需要停用后再启动 CarPlay")
    # 旧版 CLI 不支持租约时直接失败，不回退到系统网络命令。
    network_module.NetworkLease._command("status")
    phone_module = load_module("carplay_phone_selection", "carplay_phone_selection.py")
    hardware = json.loads(hardware_path.read_text(encoding="utf-8"))
    if hardware["oracle_listen"] != "127.0.0.1:5290" or hardware["adapter"] != "hci0" or hardware["rfcomm_channel"] != 4:
        raise ValueError("签名服务或蓝牙参数不匹配")
    adapter = ("busctl", "--system", "get-property", "org.bluez", "/org/bluez/hci0", "org.bluez.Adapter1")
    original_power = base.command(*adapter, "Powered")
    if original_power not in {"b true", "b false"} or base.command(*adapter, "Discovering") != "b false":
        raise ValueError("蓝牙状态不适合启动服务")
    set_power = ("busctl", "--system", "set-property", "org.bluez", "/org/bluez/hci0", "org.bluez.Adapter1", "Powered", "b")
    user_settings_module = load_module("carplay_user_settings", "carplay_user_settings.py")
    user_settings_path = settings.get("user_settings", "/var/lib/teslabox-system/carplay-settings.json")
    inventory = load_module("carplay_bluetooth_inventory", "carplay_bluetooth_inventory.py").Inventory(stopped)
    provisioning = load_module("carplay_provisioning", "carplay_provisioning.py").Provisioning(runtime)
    authentication_configuration(settings)
    try:
        with power.hold():
            base.command(*set_power, "true")
            handoff_recovery = HandoffRecovery()
            inventory.start()
            write_installation_ready(runtime, settings)
            network_handoff_required = False
            while not stopped.is_set():
                # 暂停时不申请热点、不启动接收端；恢复仍由同一常驻进程负责。
                current_user_settings, current_display = user_settings_module.load(user_settings_path)
                phone_state = inventory.annotate(phone_module.selection(phone_module.read_registry(identity_directory), current_user_settings.get("phone_id", ""), settings.get("phone_bt", "")))
                if not current_user_settings["enabled"]:
                    provisioning.poll(lambda *unused: None, enabled=False)
                    write_status(runtime, dict(running=False, enabled=False, cleanup_complete=True, phones=phone_state,
                        provisioning=provisioning.snapshot()))
                    stopped.wait(2)
                    continue
                try:
                    select_authentication(settings, hardware)
                except AuthenticationUnavailable:
                    write_status(runtime, dict(running=False, enabled=True, authentication_pending=True,
                        cleanup_complete=True, phones=phone_state, provisioning=provisioning.snapshot()))
                    stopped.wait(2)
                    continue
                selected_phone = phone_state["selected"]
                enrollment_network = selected_phone is None or network_handoff_required
                # 新机先保留管理热点；只有明确的登记请求才进入网络交接和视频验收。
                if enrollment_network:
                    provisioning.poll(lambda *unused: None)
                    if not provisioning.active or provisioning.snapshot().get("state") != "enrolling":
                        write_status(runtime, dict(running=False, enabled=True, cleanup_complete=True,
                            onboarding_ready=True, phones=phone_state, provisioning=provisioning.snapshot()))
                        stopped.wait(1)
                        continue
                    selected_phone = provisioning.active["phone_id"]
                    network_handoff_required = False
                paired_phone = selected_phone if selected_phone_is_paired(base, selected_phone) else None
                network = network_module.NetworkLease(settings["vehicle_channels"], stopped, phone=paired_phone)
                update_upstream_readiness(network, current_user_settings["network"])
                settings_disabled = False
                clean = True
                children = []
                peer_return = None
                temporary = tempfile.TemporaryDirectory(prefix="session-", dir=runtime)
                try:
                    # 配置和 token 必须保留到子进程清理完毕，不能先退出目录上下文。
                    with nullcontext(temporary.name) as directory:
                        folder = Path(directory)
                        lease = network.acquire()
                        network_bluetooth = bool(lease["plan"].get("disable_wifi_upstream", False))
                        ap_interface = lease["plan"]["ap_interface"]
                        if settings.get("ap_uuid") and lease["plan"]["ap_uuid"] != settings["ap_uuid"] or expected_ap_interface and ap_interface != expected_ap_interface:
                            raise ValueError("项目热点配置与接收端预期不一致")
                        access = network.start()
                        channel, name = access["channel"], lease["runtime_ap_name"]
                        listen, peer_network = phone_module.ap_network(access["addresses"])
                        for address in ((listen, 7000), ("127.0.0.1", 5290), ("127.0.0.1", 8767), ("127.0.0.1", 9001), ("127.0.0.1", 9002), ("127.0.0.1", 9110)):
                            check_listener_available(address)
                        ssid, password = access["ssid"], access["password"]
                        if not password.isascii() or not 8 <= len(password) <= 63:
                            raise ValueError("热点密码不符合协议")
                        config = dict(hardware, name="TeslaBox CarPlay", wifi=dict(interface=ap_interface, ssid=ssid, passphrase=password, channel=channel))
                        base.private_json(folder / "config.json", config)
                        del password, config
                        bt_address = base.command(*adapter, "Address")
                        if not re.fullmatch(r's "(?:[A-Fa-f0-9]{2}:){5}[A-Fa-f0-9]{2}"', bt_address):
                            raise ValueError("板端蓝牙地址无效")
                        current_user_settings, current_display = user_settings_module.load(user_settings_path)
                        log_display_profile(current_display, current_user_settings)
                        receiver_profile = dict(name="TeslaBox CarPlay Experimental",
                            device_id=access["mac_address"].upper(), bluetooth_id=bt_address[3:-1].upper(),
                            source_version="0.1.0", right_hand_drive=False, keep_alive_low_power=True, omit_audio_fields=True,
                            display=dict(uuid="9383d029-3c08-467a-bb66-96f30a4b964e", **current_display))
                        receiver_profile["auto_play"] = current_user_settings.get("auto_play", False)
                        base.private_json(folder / "profile.json", receiver_profile)
                        oracle, authentication = start_authentication_oracle(binary, folder / "config.json",
                            settings, hardware, mfi, children, stopped)
                        bridge = OwnedProcess("bridge", [str(binary), "bridge", "--listen", "127.0.0.1:8767", "--ingest", "127.0.0.1:9001",
                            "--input-format", "timestamped", "--token-file", str(folder / "browser-token"), "--touch-contacts", "2", "--ui-ingest", "127.0.0.1:8768"], "CarPlay 浏览器桥接就绪")
                        children.append(bridge)
                        bridge.wait_ready(stopped)
                        publish_proxy_token(runtime, folder / "browser-token")
                        receiver_arguments = (receiver_arguments_for(node, root, binary, folder, listen, peer_network,
                            paired_phone, settings.get("phone_bt", ""), authentication["protocol"],
                            enrollment_seconds=max(1, min(120, int(provisioning.deadline - time.monotonic()) + 1)) if provisioning.active and provisioning.state["state"] == "enrolling" else None,
                            identity_directory=identity_directory) if paired_phone else None)
                        receiver = replace_phone_receiver(None, children, receiver_arguments,
                            identity_directory, stopped)
                        def switch_provisioning(mode, target, seconds):
                            nonlocal receiver, receiver_arguments, paired_phone
                            if mode == "enroll":
                                if not selected_phone_is_paired(base, target):
                                    return False
                                arguments = receiver_arguments_for(node, root, binary, folder, listen, peer_network,
                                    target, "", authentication["protocol"], enrollment_seconds=seconds, identity_directory=identity_directory)
                            else:
                                arguments = None
                            receiver = replace_phone_receiver(receiver, children, arguments,
                                identity_directory, stopped)
                            receiver_arguments = arguments
                            paired_phone = target if mode == "enroll" else None
                            network.update_phone(paired_phone)
                            return True
                        peer_return = start_peer_return_path(ap_interface, settings.get("phone_ip"), listen,
                            access["addresses"], runtime, lambda: receiver, stopped)
                        print("CARPLAY_SERVICE ready=true", flush=True)
                        settings_failed = False
                        while not stopped.is_set():
                            # 换频与结果发布必须先完成，才能判断手机超时或重建接收端。
                            with network.operation_lock:
                                try:
                                    next_user_settings, next_display = user_settings_module.load(user_settings_path, current_display)
                                    settings_failed = False
                                except (OSError, ValueError):
                                    # 设置异常不回收正在工作的热点和媒体，下轮重新读取原子保存的文件。
                                    if not settings_failed:
                                        print("CARPLAY_SERVICE user_settings_invalid=true", flush=True)
                                    settings_failed = True
                                    next_user_settings, next_display = current_user_settings, current_display
                                if not next_user_settings["enabled"]:
                                    provisioning.poll(switch_provisioning, enabled=False)
                                    settings_disabled = True
                                    print("CARPLAY_SERVICE enabled=false", flush=True)
                                    break
                                upstream_state = network.upstream or {}
                                update_upstream_readiness(network, next_user_settings["network"], receiver)
                                if upstream_state.get("wifi_paused") is True:
                                    network_bluetooth = True
                                next_channel = upstream_state.get("ap_channel")
                                if type(next_channel) is int and next_channel != channel and next_channel in settings["vehicle_channels"]:
                                    # 管理器保留 PAN 完成换频后，仅重建接收端并更新手机交接信道。
                                    network.lease["plan"]["channel"] = next_channel
                                    access = network.start()
                                    config_path = folder / "config.json"
                                    config = json.loads(config_path.read_text(encoding="utf-8"))
                                    config["wifi"]["channel"] = access["channel"]
                                    replace_private_profile(config_path, config)
                                    del config
                                    channel = access["channel"]
                                    if receiver is not None:
                                        receiver = restart_receiver(receiver, children, receiver_arguments, identity_directory, stopped)
                                    print("CARPLAY_SERVICE bluetooth_handoff_applied=true channel=" + str(channel), flush=True)
                                if network_bluetooth and next_user_settings["network"] != "bluetooth":
                                    # 退出专用模式才释放网络；进入时保留旧连接等待 PAN 实测联网。
                                    network.recovery = None
                                    print("CARPLAY_SERVICE network_policy_changed=true", flush=True)
                                    break
                                provisioning.poll(switch_provisioning)
                                if enrollment_network and not provisioning.active and provisioning.state["state"] != "video_verified":
                                    break
                                phone_state = inventory.annotate(phone_module.selection(phone_module.read_registry(identity_directory), next_user_settings["phone_id"], settings.get("phone_bt", "")))
                                if provisioning.observe(dict(receiver.latest) if receiver else {}, phone_state.get("last_successful")):
                                    # 登记成功后退出限时进程，下一轮转为正常接收；热点和浏览器桥接保持。
                                    switch_provisioning("resume", None, 0)
                                next_selected = phone_state["selected"]
                                if next_selected is None and not provisioning.active:
                                    # 取消与超时只有在 finally 释放自有租约之后才发布终态。
                                    break
                                next_paired = next_selected if selected_phone_is_paired(base, next_selected) else None
                                if provisioning.active:
                                    next_paired = provisioning.active["phone_id"] if provisioning.state["state"] == "enrolling" else None
                                if next_paired != paired_phone:
                                    receiver_arguments = (receiver_arguments_for(node, root, binary, folder, listen, peer_network,
                                        next_paired, settings.get("phone_bt", ""), authentication["protocol"], identity_directory=identity_directory) if next_paired else None)
                                    receiver = replace_phone_receiver(receiver, children, receiver_arguments,
                                        identity_directory, stopped)
                                    paired_phone = next_paired
                                    network.update_phone(paired_phone)
                                    print("CARPLAY_SERVICE phone_selection_changed=true", flush=True)
                                selected_phone = next_selected
                                if next_user_settings["recovery_priority"] == "carplay":
                                    # 已发出的恢复请求也必须服从最新保存的优先级。
                                    network.recovery = None
                                if next_user_settings.get("auto_play", False) != current_user_settings.get("auto_play", False):
                                    # 仅供下次手机会话读取，不为音乐开关打断当前视频和热点。
                                    receiver_profile["auto_play"] = next_user_settings.get("auto_play", False)
                                    replace_private_profile(folder / "profile.json", receiver_profile)
                                    print("CARPLAY_SERVICE auto_play_settings_applied=true", flush=True)
                                if next_display != current_display:
                                    # 只重建手机会话，不释放热点租约、签名服务、浏览器桥接或录像 USB。
                                    receiver_profile["display"] = dict(uuid="9383d029-3c08-467a-bb66-96f30a4b964e", **next_display)
                                    replace_private_profile(folder / "profile.json", receiver_profile)
                                    if receiver is not None:
                                        receiver = restart_receiver(receiver, children, receiver_arguments, identity_directory, stopped)
                                    current_display = next_display
                                    log_display_profile(current_display, next_user_settings)
                                    print("CARPLAY_SERVICE display_settings_applied=true", flush=True)
                                current_user_settings = next_user_settings
                                if network.recovery and next_user_settings["recovery_priority"] != "carplay":
                                    print("CARPLAY_SERVICE wifi_channel_recovery=true", flush=True)
                                    break
                                write_status(runtime, dict(running=True, channel=channel, ap_connection=name,
                                                           receiver=dict(receiver.latest) if receiver is not None else {}, browser=dict(bridge.latest), upstream=network.upstream,
                                                           phone_selection_required=selected_phone is None,
                                                           phone_not_paired=selected_phone is not None and paired_phone is None,
                                                           authentication=dict(mode=authentication["mode"], protocol=authentication["protocol"]),
                                                           display=display_observation(current_display), phones=phone_state,
                                                           provisioning=provisioning.snapshot()))
                                if not network.healthy():
                                    print("CARPLAY_SERVICE ap_disconnected=true", flush=True)
                                    break
                                handoff_recovery.observe(receiver)
                                # 通常只恢复接收会话；连续交接无进展时才受限重建整条独立链路。
                                if not provisioning.active and peer_return is not None and peer_return.consume_rejected_retry(receiver, time.monotonic()):
                                    print("CARPLAY_SERVICE peer_return_retry=true", flush=True)
                                    receiver = restart_receiver(receiver, children, receiver_arguments, identity_directory, stopped)
                                elif receiver is not None and not provisioning.active and handoff_needs_recovery(receiver, time.monotonic()):
                                    print("CARPLAY_SERVICE wifi_handoff_timeout=true", flush=True)
                                    if prepare_handoff_recovery(network, next_user_settings["network"]):
                                        if handoff_recovery.record_timeout(receiver, time.monotonic()):
                                            print("CARPLAY_SERVICE handoff_link_recovery=true failures=2 cooldown_seconds=300", flush=True)
                                            break
                                        receiver = restart_receiver(receiver, children, receiver_arguments, identity_directory, stopped)
                                elif receiver is not None and (receiver.child.poll() is not None or receiver.failed.is_set()):
                                    if provisioning.active:
                                        switch_provisioning("resume", None, 0)
                                        provisioning.fail()
                                    else:
                                        receiver = restart_receiver(receiver, children, receiver_arguments, identity_directory, stopped)
                                        print("CARPLAY_SERVICE receiver_restarted=true", flush=True)
                                if any(child.child.poll() is not None or child.failed.is_set() for child in children if child is not receiver):
                                    raise RuntimeError("服务自有进程失败")
                            stopped.wait(2)
                except network_module.OrdinaryAPActive:
                    network_handoff_required = True
                    print("CARPLAY_SERVICE awaiting_network_handoff=true", flush=True)
                finally:
                    if peer_return is not None:
                        clean = peer_return.close() and clean
                    try:
                        (runtime / "browser-token").unlink(missing_ok=True)
                    except OSError:
                        clean = False
                    for child in reversed(children):
                        try:
                            clean = child.close() and clean
                        except Exception:
                            clean = False
                    try:
                        network.close()
                        provisioning.fail()
                        write_status(runtime, dict(running=False, cleanup_complete=clean, provisioning=provisioning.snapshot()))
                    finally:
                        temporary.cleanup()
                if not clean:
                    raise RuntimeError("强制清理后禁止原地重启")
                if network.recovery and not settings_disabled and not stopped.is_set():
                    try:
                        network.reconnect_upstream()
                        print("CARPLAY_SERVICE wifi_channel_recovered=true", flush=True)
                    except Exception:
                        # 连接失败仍重建本地 AP；管理器冷却期内继续尝试蓝牙供网。
                        print("CARPLAY_SERVICE wifi_channel_recovered=false", flush=True)
                stopped.wait(1)
    finally:
        inventory.close()
        if original_power == "b false":
            with power.power_off() as allowed:
                if allowed:
                    base.command(*set_power, "false")


def main():
    """显式启动独立服务，systemd 或人工调用均通过同一生命周期。"""
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--settings", type=Path, required=True)
    parser.add_argument("--runtime-directory", type=Path, default=Path("/run/teslabox-carplay"))
    parser.add_argument("--run-hardware", action="store_true")
    args = parser.parse_args()
    if os.name != "posix" or os.geteuid() != 0 or not args.run_hardware:
        raise PermissionError("服务只允许 Linux root 显式启动")
    import fcntl
    settings = load_settings(args.settings)
    if not args.runtime_directory.is_absolute():
        raise ValueError("服务运行目录必须是绝对路径")
    stopped = threading.Event()
    for number in (signal.SIGINT, signal.SIGTERM, signal.SIGHUP):
        signal.signal(number, lambda _number, _frame: stopped.set())
    base = load_module("service_lock_tools", "board-wireless-mfi-check.py")
    base.private_directory(args.runtime_directory)
    with ExitStack() as resources:
        fd = os.open(args.runtime_directory / "service.lock", os.O_RDWR | os.O_CREAT | os.O_NOFOLLOW, 0o600)
        resources.callback(os.close, fd)
        info = os.fstat(fd)
        if not stat.S_ISREG(info.st_mode) or info.st_uid != os.geteuid() or info.st_mode & 0o077 or info.st_nlink != 1:
            raise PermissionError("服务锁不是私有普通文件")
        fcntl.flock(fd, fcntl.LOCK_EX | fcntl.LOCK_NB)
        # 取得独占锁后才清理旧标记，避免第二个进程破坏仍在运行的服务就绪状态。
        (args.runtime_directory / "installation-ready.json").unlink(missing_ok=True)
        run(settings, args.runtime_directory, stopped)


if __name__ == "__main__":
    try:
        main()
    except Exception as error:
        # 系统命令可能携带无线密码，只有固定类型进入 journal。
        print("CARPLAY_SERVICE failed=" + type(error).__name__ +
              (" errno=" + str(error.errno) if isinstance(error, OSError) else ""), flush=True)
        raise SystemExit(1)
