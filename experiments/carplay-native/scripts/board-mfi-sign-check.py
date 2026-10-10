#!/usr/bin/env python3
"""显式实体签名诊断；不安装服务，不改变蓝牙/AP/Gadget，不输出认证正文。"""

import argparse
import hashlib
import json
import os
from pathlib import Path
import re
import signal
import socket
import subprocess
import tempfile
import time


def remaining(deadline, limit):
    value = min(limit, deadline - time.monotonic())
    if value <= 0:
        raise TimeoutError("诊断总时限到期")
    return value


def receive(stream, length, deadline):
    result = bytearray()
    while len(result) < length:
        stream.settimeout(remaining(deadline, 4))
        part = stream.recv(length - len(result))
        if not part:
            raise EOFError("实体签名服务响应截断")
        result.extend(part)
    return bytes(result)


def request(address, operation, payload, deadline):
    # 独立实现客户端帧，避免用生产客户端与自己互测掩盖长度错误。
    if (operation, len(payload)) not in ((1, 0), (2, 20)):
        raise ValueError("诊断仅允许公开证书及 SHA-1 签名")
    with socket.create_connection(address, remaining(deadline, 1)) as stream:
        stream.settimeout(remaining(deadline, 1))
        stream.sendall(bytes([operation]) + len(payload).to_bytes(2, "big") + payload)
        header = receive(stream, 3, deadline)
        length = int.from_bytes(header[1:], "big")
        if header[0] != 0 or not 1 <= length <= 2048:
            raise ValueError("实体签名服务拒绝请求或返回非法长度")
        return receive(stream, length, deadline)


def openssl(arguments, deadline):
    return subprocess.run(
        ["openssl", *arguments], stdin=subprocess.DEVNULL, capture_output=True,
        timeout=remaining(deadline, 5), check=False,
    )


def interrupted(_number, _frame):
    raise InterruptedError("诊断被取消")


def run(binary, config, expected_binary, expected_certificate):
    if os.name != "posix" or os.geteuid() != 0:
        raise PermissionError("实体 CH341 诊断需要在 Linux 板端以 root 显式运行")
    if hashlib.sha256(binary.read_bytes()).hexdigest() != expected_binary:
        raise ValueError("实际二进制摘要不匹配")
    settings = json.loads(config.read_text(encoding="utf-8"))
    if settings["oracle_listen"] != "127.0.0.1:5290":
        raise ValueError("诊断仅允许专用 loopback 签名端口")
    hardware = settings["mfi"]
    if (hardware["transport"] != "ch341" or hardware.get("usb_bus") is None
            or hardware.get("usb_address") is None):
        raise ValueError("必须明确选择已核验的小板 USB bus/address")
    address = ("127.0.0.1", 5290)
    # 有既有服务即拒绝，不停止它；短暂预检不是与并行访问者之间的锁。
    with socket.socket() as guard:
        guard.bind(address)
    deadline = time.monotonic() + 35
    handlers = {}
    child = None
    errors_path = None
    stage = "startup"
    try:
        for number in (signal.SIGINT, signal.SIGTERM, signal.SIGHUP):
            handlers[number] = signal.signal(number, interrupted)
        with tempfile.TemporaryDirectory(prefix="mfi-sign-check-", dir=binary.parent) as directory:
            folder = Path(directory)
            folder.chmod(0o700)
            # 错误文件与临时载荷目录分离，异常退出目录后仍能读取脱敏原因。
            descriptor, name = tempfile.mkstemp(prefix="mfi-sign-check-stderr-", dir=binary.parent)
            errors_path = Path(name)
            with os.fdopen(descriptor, "wb") as errors:
                child = subprocess.Popen(
                    [str(binary), "--config", str(config), "mfi-oracle"],
                    stdin=subprocess.DEVNULL, stdout=subprocess.DEVNULL, stderr=errors,
                )
                startup = min(deadline, time.monotonic() + 8)
                while True:
                    if child.poll() is not None:
                        raise RuntimeError("实体签名进程启动失败")
                    try:
                        certificate = request(address, 1, b"", startup)
                        break
                    except ConnectionRefusedError:
                        time.sleep(min(0.05, remaining(startup, 0.05)))
                stage = "certificate"
                digest = hashlib.sha256(certificate).hexdigest()
                if digest != expected_certificate:
                    raise ValueError("实际公开证书摘要不匹配")
                (folder / "certificate.der").write_bytes(certificate)
                certificate_result = openssl([
                    "pkcs7", "-inform", "DER", "-in", str(folder / "certificate.der"),
                    "-print_certs",
                ], deadline)
                certificates = re.findall(
                    rb"-----BEGIN CERTIFICATE-----.*?-----END CERTIFICATE-----",
                    certificate_result.stdout, flags=re.DOTALL,
                )
                # 当前已核验的 908 字节小板包只有一张配件证书；不猜链中哪张是叶子。
                if certificate_result.returncode != 0 or len(certificates) != 1:
                    raise ValueError("公开证书包不是已核验的单配件证书")
                (folder / "certificate.pem").write_bytes(certificates[0] + b"\n")
                public = openssl([
                    "x509", "-in", str(folder / "certificate.pem"), "-pubkey", "-noout",
                ], deadline)
                if public.returncode != 0 or b"BEGIN PUBLIC KEY" not in public.stdout:
                    raise ValueError("公开证书公钥解析失败")
                (folder / "public.pem").write_bytes(public.stdout)
                print(f"CERTIFICATE_OK bytes={len(certificate)} sha256={digest}", flush=True)
                for index in range(1, 4):
                    stage = f"signature-{index}"
                    message = os.urandom(32)
                    signature = request(address, 2, hashlib.sha1(message).digest(), deadline)
                    if len(signature) != 128:
                        raise ValueError("实际签名不是 128 字节")
                    (folder / "message.bin").write_bytes(message)
                    (folder / "signature.bin").write_bytes(signature)
                    arguments = [
                        "dgst", "-sha1", "-verify", str(folder / "public.pem"),
                        "-signature", str(folder / "signature.bin"), str(folder / "message.bin"),
                    ]
                    verified = openssl(arguments, deadline)
                    if verified.returncode != 0 or verified.stdout.strip() != b"Verified OK":
                        raise ValueError("实体签名未通过独立 OpenSSL 验签")
                    (folder / "message.bin").write_bytes(bytes([message[0] ^ 1]) + message[1:])
                    rejected = openssl(arguments, deadline)
                    if rejected.returncode == 0 or rejected.stdout.strip().lower() != b"verification failure":
                        raise ValueError("篡改消息未按预期被拒绝")
                    print(f"SIGNATURE_OK trial={index} bytes=128 tamper_rejected=yes", flush=True)
                # 先关闭自有进程再退出临时目录，失败也经同一 finally 回收。
                stage = "cleanup"
                child.terminate()
                child.wait(timeout=4)
                if child.returncode != 0:
                    raise RuntimeError("实体签名进程未正常回收")
                print("OWNED_ORACLE_STOPPED", flush=True)
    except Exception:
        print(f"MFI_SIGN_CHECK_FAILED stage={stage}", flush=True)
        # 仅保留生产芯片错误白名单；不转储其他 stdout/stderr、证书或挑战。
        if errors_path is not None and errors_path.exists():
            with errors_path.open("rb") as log:
                log.seek(0, 2)
                log.seek(max(0, log.tell() - 16384))
                for line in log.read(16384).decode("utf-8", errors="replace").splitlines():
                    if line.startswith("MFi 请求失败 operation="):
                        print(f"ORACLE_DIAGNOSTIC={line[:512]}", flush=True)
        raise
    finally:
        for number in handlers:
            signal.signal(number, signal.SIG_IGN)
        try:
            if child is not None and child.poll() is None:
                child.terminate()
                try:
                    child.wait(timeout=4)
                except subprocess.TimeoutExpired:
                    child.kill()
                    child.wait(timeout=2)
        finally:
            if errors_path is not None:
                errors_path.unlink(missing_ok=True)
            for number, handler in handlers.items():
                signal.signal(number, handler)


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--run-hardware", action="store_true", required=True)
    parser.add_argument("--binary", type=Path, required=True)
    parser.add_argument("--config", type=Path, required=True)
    parser.add_argument("--expected-binary-sha256", required=True)
    parser.add_argument("--expected-certificate-sha256", required=True)
    arguments = parser.parse_args()
    for value in (arguments.expected_binary_sha256, arguments.expected_certificate_sha256):
        if not re.fullmatch(r"[0-9a-f]{64}", value):
            parser.error("摘要必须是完整 SHA-256")
    run(arguments.binary.resolve(strict=True), arguments.config.resolve(strict=True),
        arguments.expected_binary_sha256, arguments.expected_certificate_sha256)


if __name__ == "__main__":
    main()
