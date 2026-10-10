"""只读蓝牙设备缓存，为手机选择显示名称，不扫描、配对或改变无线电状态。"""
import copy
import json
import os
import re
import selectors
import subprocess
import threading
import time

LIMIT = 131072
ADDRESS = re.compile(r"(?:[A-F0-9]{2}:){5}[A-F0-9]{2}")


def parse_devices(body):
    """仅接收当前适配器的Device1白名单，不把总线任意属性传播到页面或日志。"""
    if not isinstance(body, bytes) or len(body) > LIMIT:
        raise ValueError("蓝牙设备缓存超过预算")
    value = json.loads(body.decode("utf-8"))
    if not isinstance(value, dict) or value.get("type") != "a{oa{sa{sv}}}" or not isinstance(value.get("data"), list) or len(value["data"]) != 1 or not isinstance(value["data"][0], dict) or len(value["data"][0]) > 256:
        raise ValueError("蓝牙设备缓存字段不合法")
    devices = []
    for path, interfaces in value["data"][0].items():
        if not isinstance(interfaces, dict):
            continue
        properties = interfaces.get("org.bluez.Device1")
        if not isinstance(properties, dict):
            continue
        def field(name, signature, kind):
            item = properties.get(name)
            return item["data"] if isinstance(item, dict) and item.get("type") == signature and type(item.get("data")) is kind else None
        address = field("Address", "s", str)
        if not address or not ADDRESS.fullmatch(address) or path != "/org/bluez/hci0/dev_" + address.replace(":", "_") or field("Adapter", "o", str) != "/org/bluez/hci0":
            continue
        name = field("Alias", "s", str) or field("Name", "s", str) or ""
        if len(name.encode("utf-8")) > 128 or not name.isprintable():
            name = ""
        devices.append(dict(id=address, name=name, paired=field("Paired", "b", bool) is True,
                            connected=field("Connected", "b", bool) is True))
        if len(devices) > 32:
            raise ValueError("蓝牙设备缓存数量超过预算")
    return devices


def read_devices():
    """命令输出与时间都有限，失败只结束自己发起的只读子进程。"""
    child = subprocess.Popen(["busctl", "--system", "--json=short", "call", "org.bluez", "/", "org.freedesktop.DBus.ObjectManager", "GetManagedObjects"],
                             stdin=subprocess.DEVNULL, stdout=subprocess.PIPE, stderr=subprocess.DEVNULL)
    deadline = time.monotonic() + 3
    body = bytearray()
    try:
        with selectors.DefaultSelector() as selector:
            selector.register(child.stdout, selectors.EVENT_READ)
            while True:
                remaining = deadline - time.monotonic()
                if remaining <= 0 or not selector.select(remaining):
                    raise TimeoutError("蓝牙设备缓存读取超时")
                chunk = os.read(child.stdout.fileno(), min(4096, LIMIT + 1 - len(body)))
                if not chunk:
                    break
                body.extend(chunk)
                if len(body) > LIMIT:
                    raise ValueError("蓝牙设备缓存超过预算")
        if child.wait(timeout=max(.01, deadline - time.monotonic())) != 0:
            raise ValueError("蓝牙设备缓存不可用")
        return parse_devices(bytes(body))
    finally:
        if child.poll() is None:
            child.kill()
        child.wait(timeout=2)
        child.stdout.close()


class Inventory:
    """异步读取缓存，设备名称查询不会挡住AP启动或手机交接。"""
    def __init__(self, stopped, reader=read_devices):
        self.stopped, self.reader = stopped, reader
        self.closed = threading.Event()
        self.thread = None
        self.devices = ()

    def start(self):
        """每个常驻实例仅创建一个只读工作线程。"""
        if self.thread is not None:
            raise RuntimeError("蓝牙设备缓存线程已启动")
        self.thread = threading.Thread(target=self._run, daemon=True)
        self.thread.start()

    def _run(self):
        while not self.closed.is_set() and not self.stopped.is_set():
            try:
                self.devices = tuple(self.reader())
            except (OSError, ValueError, TimeoutError, subprocess.TimeoutExpired):
                self.devices = ()
            self.closed.wait(30)

    def annotate(self, state):
        """名称只用于展示，不能改变已登记身份、选择结果或配对授权。"""
        result = copy.deepcopy(state)
        names = {phone["id"]: phone["name"] for phone in self.devices if phone["name"]}
        for phone in result["phones"]:
            if phone["id"] in names:
                phone["name"] = names[phone["id"]]
        return result

    def close(self):
        """退出时等待自己的只读调用结束，不终止系统蓝牙服务。"""
        self.closed.set()
        if self.thread is not None:
            self.thread.join(timeout=6)
            if self.thread.is_alive():
                raise RuntimeError("蓝牙设备缓存线程未结束")
