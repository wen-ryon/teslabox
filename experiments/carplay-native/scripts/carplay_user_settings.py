"""读取正式管理页保存的 CarPlay 参数，不改变录像主服务或 USB 状态。"""
import json
import os
import re
from pathlib import Path
import stat


def read_json(path):
    """有界读取系统盘私有普通文件，拒绝跟随符号链接。"""
    descriptor = os.open(path, os.O_RDONLY | os.O_NOFOLLOW)
    try:
        info = os.fstat(descriptor)
        if not stat.S_ISREG(info.st_mode) or info.st_uid != os.geteuid() or info.st_mode & 0o077 or info.st_size > 4096:
            raise ValueError("CarPlay 用户设置文件不合法")
        with os.fdopen(descriptor, "r", encoding="utf-8", closefd=False) as stream:
            return json.load(stream)
    finally:
        os.close(descriptor)


def load(path, previous_display=None):
    """设置尚未保存时保持当前默认，旧版本尺寸不能覆盖新画质。"""
    path = Path(path)
    try:
        settings = read_json(path)
    except FileNotFoundError:
        settings = dict(revision=1, network="auto", quality=720, fps=30)
    # 旧文件中的无效缩放值直接丢弃，不再返回或用于屏幕协商。
    if isinstance(settings, dict):
        settings = dict(settings)
        settings.pop("scale", None)
    required = {"revision", "network", "quality", "fps"}
    if not isinstance(settings, dict) or not required <= set(settings) or set(settings) - required - {"enabled", "recovery_priority", "phone_id", "dashboard_layout", "auto_play"}:
        raise ValueError("CarPlay 设置字段不匹配")
    # 旧设置沿用已确认的联网优先，不因升级意外停止原会话。
    settings = dict(settings)
    settings.setdefault("auto_play", False)
    if type(settings["auto_play"]) is not bool:
        raise ValueError("CarPlay 自动播放开关不合法")
    settings.setdefault("dashboard_layout", "auto")
    if settings["dashboard_layout"] not in ("auto", "vertical"):
        raise ValueError("CarPlay 主页面布局不合法")
    settings.setdefault("phone_id", "")
    if not isinstance(settings["phone_id"], str) or settings["phone_id"] and not re.fullmatch(r"(?:[A-F0-9]{2}:){5}[A-F0-9]{2}", settings["phone_id"]):
        raise ValueError("CarPlay 选择手机不合法")
    settings.setdefault("enabled", True)
    settings.setdefault("recovery_priority", "internet")
    if type(settings["enabled"]) is not bool or settings["recovery_priority"] not in ("internet", "carplay"):
        raise ValueError("CarPlay 开关或恢复优先级不合法")
    if any(type(settings[key]) is not int for key in ("revision", "quality", "fps")):
        raise ValueError("CarPlay 设置类型不合法")
    if not 1 <= settings["revision"] <= 9007199254740991 or settings["quality"] not in (480, 720, 1080) or settings["fps"] not in (30, 60):
        raise ValueError("CarPlay 画质或帧率不合法")
    if settings["network"] not in ("auto", "wifi", "ethernet", "bluetooth"):
        raise ValueError("CarPlay 网络设置不合法")
    width, height = {480: (854, 480), 720: (1280, 720), 1080: (1920, 1080)}[settings["quality"]]
    try:
        detected = read_json(str(path) + ".display")
    except FileNotFoundError:
        detected = None
    if detected is not None and not isinstance(detected, dict):
        raise ValueError("CarPlay 检测尺寸必须是对象")
    if detected is not None:
        detected = dict(detected)
        detected.pop("scale", None)
    # 保存后等浏览器提交新版本尺寸才重建会话，避免先默认比例、再真实比例的两次重连。
    if previous_display is not None and (not detected or detected.get("revision") != settings["revision"]):
        return settings, dict(previous_display)
    if detected and detected.get("revision") == settings["revision"]:
        if set(detected) != {"revision", "width", "height", "fps"} or any(type(value) is not int for value in detected.values()):
            raise ValueError("CarPlay 检测尺寸字段不合法")
        if not 320 <= detected["width"] <= width or not 240 <= detected["height"] <= height or detected["width"] % 2 or detected["height"] % 2:
            raise ValueError("CarPlay 检测尺寸超出画质范围")
        if detected["fps"] != settings["fps"]:
            raise ValueError("CarPlay 检测尺寸与设置不一致")
        width, height = detected["width"], detected["height"]
    if settings["dashboard_layout"] == "vertical":
        # 浏览器未提交尺寸的首次启动也采用相同边界；不依赖旧页面支持新设置。
        width = min(width, (13 * height - 600) // 20 * 2)
        if width < 320:
            raise ValueError("当前画面比例无法使用竖向地图布局")
    # 固定默认物理宽度，停止使用无效的界面缩放参数。
    width_mm = 280
    display = dict(width=width, height=height, fps=settings["fps"], width_mm=width_mm,
                   height_mm=round(width_mm * height / width))
    return settings, display
