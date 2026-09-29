#!/usr/bin/env python3
"""Talk to an MFi authentication coprocessor through a CH341 USB-to-I2C bridge.

Works on macOS with libusb (no kernel driver needed). The wire format reimplements
shilapi/xcertplay:
  Ch341I2cStreamEncoder      - CH341 I2C stream packet encoding
  Ch341I2cTransport          - status-byte / ACK handling
  MfiDeviceScanner           - probe 0x10 then 0x11 via device-version register 0x00
  MfiAuthenticationClient    - register map

Usage:
  python3 mfi_tool.py scan             probe the bus, print the chip address and version
  python3 mfi_tool.py info             scan and read identity registers
  python3 mfi_tool.py cert [-o FILE]   dump the certificate (default ./mfi_certificate.p7b)

Requires: pip install pyusb libusb-package
"""

import argparse
import hashlib
import os
import sys
import time

import usb.core
import usb.util
from usb.backend import libusb1

VID, PID = 0x1A86, 0x5512  # QinHeng CH341, parallel/I2C mode

STREAM_START, START, STOP, STREAM_END = 0xAA, 0x74, 0x75, 0x00
WRITE, READ = 0x80, 0xC0
SPEED_100K = 0x61
ACK_BIT = 0x80

PACKET = 32  # CH341 answers in whole 32-byte stream packets
WINDOW = 128  # certificate register window, per xcertplay

DEVICE_VERSION_REGISTER = 0x00
PROTOCOL_MAJOR_REGISTER = 0x02
ERROR_REGISTER = 0x05
CERTIFICATE_LENGTH_REGISTER = 0x30
CERTIFICATE_DATA_REGISTER = 0x31

CANDIDATE_ADDRESSES = (0x10, 0x11)
IO_RETRY_TIMEOUT = 2.0
IO_RETRY_DELAY = 0.02
SELECT_TO_READ_GAP = 0.005


def find_libusb():
    for candidate in ("/opt/homebrew/lib/libusb-1.0.dylib",
                      "/usr/local/lib/libusb-1.0.dylib"):
        if os.path.exists(candidate):
            return candidate
    try:
        import libusb_package
        return libusb_package.get_library_path()
    except Exception:
        return "/opt/homebrew/lib/libusb-1.0.dylib"


def build_legacy(addr7, write_data, read_len):
    """Single-segment CH341 I2C stream packet (fits in 32 bytes)."""
    out = bytearray([STREAM_START])
    if write_data:
        out += bytes([START, WRITE, (addr7 << 1) & 0xFF])
        for value in write_data:
            out += bytes([WRITE, value & 0xFF])
    if read_len > 0:
        out += bytes([START, WRITE, ((addr7 << 1) | 1) & 0xFF])
        if read_len > 1:
            out += bytes([READ + read_len - 1])
        out += bytes([READ])
    out += bytes([STOP, STREAM_END])
    return bytes(out)


def build_read_segments(addr7, length):
    """Segmented pure read for length > 32. Returns [(payload, data_bytes)]."""
    segments = []
    # Batched 0x80|1 emits the address with no ACK/NACK status byte.
    segment = bytearray([STREAM_START, START, WRITE + 1, ((addr7 << 1) | 1) & 0xFF])
    remaining = length
    while remaining > PACKET:
        segment.append(READ + PACKET)
        segment.append(STREAM_END)
        while len(segment) < PACKET:
            segment.append(0)
        segments.append((bytes(segment), PACKET))
        segment = bytearray([STREAM_START])
        remaining -= PACKET
    if remaining > 1:
        segment.append(READ + remaining - 1)
    segment.append(READ)
    segment += bytes([STOP, STREAM_END])
    segments.append((bytes(segment), remaining))
    return segments


class Ch341:
    def __init__(self, dev):
        self.dev = dev
        try:
            self.dev.get_active_configuration()
        except usb.core.USBError:
            self.dev.set_configuration()
        self.ep_out = self.ep_in = self.iface = None
        for interface in self.dev.get_active_configuration():
            outs = [e for e in interface
                    if usb.util.endpoint_direction(e.bEndpointAddress) == usb.util.ENDPOINT_OUT
                    and usb.util.endpoint_type(e.bmAttributes) == usb.util.ENDPOINT_TYPE_BULK]
            ins = [e for e in interface
                   if usb.util.endpoint_direction(e.bEndpointAddress) == usb.util.ENDPOINT_IN
                   and usb.util.endpoint_type(e.bmAttributes) == usb.util.ENDPOINT_TYPE_BULK]
            if outs and ins:
                self.ep_out, self.ep_in, self.iface = outs[0], ins[0], interface
                break
        if self.iface is None:
            raise RuntimeError("没有找到同时含 bulk IN/OUT 的接口")
        usb.util.claim_interface(self.dev, self.iface.bInterfaceNumber)
        self.write(bytes([STREAM_START, SPEED_100K, STREAM_END]))  # 100 kHz

    def write(self, data):
        return self.dev.write(self.ep_out, data, 2000)

    def read(self, size):
        return bytes(self.dev.read(self.ep_in, size, 1500))

    def close(self):
        try:
            usb.util.release_interface(self.dev, self.iface.bInterfaceNumber)
        except Exception:
            pass


def exchange(ch, addr7, payload, data_bytes):
    """Write one stream packet, verify the ACK/NACK status bytes, return the data bytes."""
    ch.write(payload)
    written = sum(1 for b in payload if b == WRITE and b & 0x3F == 0)
    want = written + data_bytes
    if want == 0:
        return b""
    answer = ch.read(want)
    if len(answer) < data_bytes:
        raise RuntimeError("应答 %d 字节，少于期望的 %d 数据字节: %s"
                           % (len(answer), data_bytes, answer.hex(" ")))
    split = len(answer) - data_bytes
    for index, status in enumerate(answer[:split]):
        if status & ACK_BIT:
            raise RuntimeError("写字节 %d 被 NACK (状态 0x%02x)" % (index, status))
    return answer[split:]


def read_register(ch, addr7, register, length):
    """Select a register with STOP, then a separate pure read. Retries like xcertplay's retryIo."""
    deadline = time.time() + IO_RETRY_TIMEOUT
    last = None
    while True:
        try:
            exchange(ch, addr7, build_legacy(addr7, [register], 0), 0)
            time.sleep(SELECT_TO_READ_GAP)
            data = exchange(ch, addr7, build_legacy(addr7, b"", length), length)
            if len(data) != length:
                raise RuntimeError("寄存器 0x%02x 返回 %d 字节，期望 %d"
                                   % (register, len(data), length))
            return data
        except Exception as exc:
            last = exc
            if time.time() >= deadline:
                raise last
            time.sleep(IO_RETRY_DELAY)


def read_long(ch, addr7, register, length):
    """Register read longer than one 32-byte CH341 packet."""
    exchange(ch, addr7, build_legacy(addr7, [register], 0), 0)
    time.sleep(SELECT_TO_READ_GAP)
    out = bytearray()
    for payload, data_bytes in build_read_segments(addr7, length):
        ch.write(payload)
        answer = ch.read(data_bytes)
        if len(answer) < data_bytes:
            raise RuntimeError("段应答 %d 字节，期望 %d" % (len(answer), data_bytes))
        out += answer[:data_bytes]
    return bytes(out[:length])


def discover(ch):
    """Probe 0x10 then 0x11 until one returns a plausible device version."""
    deadline = time.time() + IO_RETRY_TIMEOUT
    while True:
        for addr in CANDIDATE_ADDRESSES:
            try:
                exchange(ch, addr, build_legacy(addr, [DEVICE_VERSION_REGISTER], 0), 0)
                time.sleep(SELECT_TO_READ_GAP)
                version = exchange(ch, addr, build_legacy(addr, b"", 1), 1)[0]
                if version not in (0x00, 0xFF):
                    return addr, version
            except Exception:
                pass
        if time.time() >= deadline:
            return None, None
        time.sleep(IO_RETRY_DELAY)


def open_device():
    backend = libusb1.get_backend(find_library=lambda _: find_libusb())
    if backend is None:
        raise RuntimeError("找不到 libusb；先 pip install libusb-package")
    dev = usb.core.find(idVendor=VID, idProduct=PID, backend=backend)
    if dev is None:
        raise RuntimeError("未找到 CH341 设备 1a86:5512；检查接线与供电")
    return Ch341(dev)


def cmd_scan(_args):
    ch = open_device()
    try:
        addr, version = discover(ch)
        if addr is None:
            print("未发现 MFi 协处理器（0x10 / 0x11 均无响应）")
            return 2
        print("MFi 协处理器: 地址 0x%02x, device version 0x%02x" % (addr, version))
        return 0
    finally:
        ch.close()


def cmd_info(_args):
    ch = open_device()
    try:
        addr, version = discover(ch)
        if addr is None:
            print("未发现 MFi 协处理器")
            return 2
        print("I2C 7-bit 地址 : 0x%02x  (写 0x%02x / 读 0x%02x)" % (addr, addr << 1, (addr << 1) | 1))
        print("0x00 device version : 0x%02x" % version)
        for register, label, length in ((PROTOCOL_MAJOR_REGISTER, "0x02 protocol major", 1),
                                        (ERROR_REGISTER, "0x05 error", 1),
                                        (CERTIFICATE_LENGTH_REGISTER, "0x30 证书长度", 2)):
            try:
                raw = read_register(ch, addr, register, length)
                value = raw[0] if length == 1 else (raw[0] << 8) | raw[1]
                print("%s : 0x%02x (%d)" % (label, value, value))
            except Exception as exc:
                print("%s : 读取失败 - %s" % (label, exc))
        return 0
    finally:
        ch.close()


def cmd_cert(args):
    ch = open_device()
    try:
        addr, version = discover(ch)
        if addr is None:
            print("未发现 MFi 协处理器")
            return 2
        print("地址 0x%02x, device version 0x%02x" % (addr, version))
        length_bytes = read_register(ch, addr, CERTIFICATE_LENGTH_REGISTER, 2)
        total = (length_bytes[0] << 8) | length_bytes[1]
        print("证书长度: %d 字节" % total)

        certificate = bytearray()
        register = CERTIFICATE_DATA_REGISTER
        while len(certificate) < total:
            count = min(WINDOW, total - len(certificate))
            deadline = time.time() + 3.0
            while True:
                try:
                    certificate += read_long(ch, addr, register, count)
                    break
                except Exception as exc:
                    if time.time() >= deadline:
                        raise
                    print("  寄存器 0x%02x 重试 (%s)" % (register, exc))
                    time.sleep(IO_RETRY_DELAY)
            register += 1
            print("  窗口 0x%02x: 累计 %d/%d" % (register - 1, len(certificate), total))

        path = args.out
        with open(path, "wb") as handle:
            handle.write(certificate)
        print("\n已保存 %s (%d 字节)" % (path, len(certificate)))
        print("前 16 字节: %s" % bytes(certificate[:16]).hex(" "))
        print("SHA-256   : %s" % hashlib.sha256(bytes(certificate)).hexdigest())
        return 0
    finally:
        ch.close()


def main():
    parser = argparse.ArgumentParser(description=__doc__,
                                     formatter_class=argparse.RawDescriptionHelpFormatter)
    sub = parser.add_subparsers(dest="command", required=True)
    sub.add_parser("scan", help="probe the I2C bus for an MFi chip")
    sub.add_parser("info", help="read identity registers")
    cert = sub.add_parser("cert", help="dump the certificate")
    cert.add_argument("-o", "--out", default="mfi_certificate.p7b", help="output path")
    args = parser.parse_args()
    return {"scan": cmd_scan, "info": cmd_info, "cert": cmd_cert}[args.command](args)


if __name__ == "__main__":
    sys.exit(main())
