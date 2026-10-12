"""从已登记手机记录选择蓝牙目标；IP仅来自当前AP配置和入站控制连接。"""
import ipaddress
import json
import os
from pathlib import Path
import re
import stat

PHONE = re.compile(r"(?:[A-F0-9]{2}:){5}[A-F0-9]{2}")
CONTROLLER = re.compile(r"[A-Za-z0-9_-]{1,128}")


def ap_network(addresses):
    """只使用项目租约给出的唯一私网AP地址，不能扩大到其他网卡或任意私网。"""
    if not isinstance(addresses, list) or len(addresses) != 1:
        raise ValueError("CarPlay AP 必须有唯一 IPv4 子网")
    interface = ipaddress.IPv4Interface(addresses[0])
    address, network = interface.ip, interface.network
    if not address.is_private or address.is_loopback or address.is_link_local or address.is_multicast or not 16 <= network.prefixlen <= 30 or address in (network.network_address, network.broadcast_address):
        raise ValueError("CarPlay AP 子网不合法")
    return str(address), str(network)


def read_registry(directory):
    """有界读取独立私有映射，任何损坏都不能降级成未登记状态。"""
    directory = Path(directory)
    info = directory.lstat()
    if not stat.S_ISDIR(info.st_mode) or stat.S_ISLNK(info.st_mode) or info.st_uid != os.geteuid() or info.st_mode & 0o077:
        raise ValueError("CarPlay 手机目录权限不合法")
    try:
        descriptor = os.open(directory / "carplay-phone-registry.json", os.O_RDONLY | os.O_NOFOLLOW)
    except FileNotFoundError:
        return None
    try:
        info = os.fstat(descriptor)
        if not stat.S_ISREG(info.st_mode) or info.st_uid != os.geteuid() or info.st_mode & 0o077 or not 1 <= info.st_size <= 8192:
            raise ValueError("CarPlay 手机记录权限不合法")
        data = os.read(descriptor, 8193)
        if len(data) > 8192 or data.startswith(b"\xef\xbb\xbf"):
            raise ValueError("CarPlay 手机记录超出预算")
        return validate_registry(json.loads(data.decode("utf-8")))
    finally:
        os.close(descriptor)


def validate_registry(value):
    """字段与接收端保持一致，不把未知身份或额外协议数据传播到设置页面。"""
    if not isinstance(value, dict) or set(value) != {"version", "phones", "last_successful"} or type(value["version"]) is not int or value["version"] != 1 or not isinstance(value["phones"], list) or len(value["phones"]) > 8:
        raise ValueError("CarPlay 手机记录字段不合法")
    bluetooth, controllers = set(), set()
    for phone in value["phones"]:
        if not isinstance(phone, dict) or set(phone) != {"bluetooth", "controller"} or not isinstance(phone["bluetooth"], str) or not PHONE.fullmatch(phone["bluetooth"]) or not isinstance(phone["controller"], str) or not CONTROLLER.fullmatch(phone["controller"]) or phone["bluetooth"] in bluetooth or phone["controller"] in controllers:
            raise ValueError("CarPlay 手机映射不合法")
        bluetooth.add(phone["bluetooth"])
        controllers.add(phone["controller"])
    if value["last_successful"] is not None and (not isinstance(value["last_successful"], str) or value["last_successful"] not in bluetooth):
        raise ValueError("CarPlay 默认手机不合法")
    return value


def selection(value, requested="", legacy=""):
    """多部手机不能按列表顺序选择，手动指定不可用时不自动切到另一部。"""
    if not isinstance(requested, str) or requested and not PHONE.fullmatch(requested) or not isinstance(legacy, str) or legacy and not PHONE.fullmatch(legacy):
        raise ValueError("CarPlay 选择手机不合法")
    if value is None:
        # 新机没有旧身份时必须返回 None，空字符串会被后续配对检查当成非法手机。
        return dict(selected=legacy if legacy and (not requested or requested == legacy) else None,
                    phones=[dict(id=legacy)] if legacy else [], last_successful=None, legacy_pending=True)
    validate_registry(value)
    phones = [dict(id=entry["bluetooth"]) for entry in value["phones"]]
    candidate = requested or value["last_successful"] or (phones[0]["id"] if len(phones) == 1 else None)
    selected = candidate if any(entry["id"] == candidate for entry in phones) else None
    return dict(selected=selected, phones=phones, last_successful=value["last_successful"], legacy_pending=False)
