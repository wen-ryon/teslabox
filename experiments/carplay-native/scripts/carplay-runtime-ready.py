"""首次安装时等待本机控制接口，避免主程序尚未启动完成就重复重建热点。"""
import socket
import time

deadline = time.monotonic() + 30
while True:
    try:
        with socket.socket(socket.AF_UNIX, socket.SOCK_STREAM) as client:
            client.settimeout(.5)
            client.connect("/run/teslabox/automation/control.sock")
        break
    except OSError:
        if time.monotonic() >= deadline:
            raise RuntimeError("本机网络控制接口未就绪") from None
        time.sleep(.2)
