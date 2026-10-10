#!/usr/bin/env python3
"""显式限时无线认证实验；临时克隆 AP，不安装服务或修改原网络配置。"""

import argparse
import hashlib
import importlib.util
import ipaddress
import json
import os
from pathlib import Path
import re
import select
import signal
import socket
import stat
import subprocess
import sys
import tempfile
import time
import uuid


def command(*arguments, allow_failure=False):
    result = subprocess.run(arguments, stdin=subprocess.DEVNULL, capture_output=True,
                            text=True, encoding="utf-8", timeout=25, check=False)
    if result.returncode and not allow_failure:
        # 不回显 nmcli 错误正文，以免它带入保存的网络信息。
        raise RuntimeError(f"系统命令失败 program={Path(arguments[0]).name} code={result.returncode}")
    # SSID/密码首尾空格是有效字节，不能像普通状态文本一样 strip。
    return result.stdout.rstrip("\r\n")


def channel(text, mode):
    if not any(line.strip() == f"type {mode}" for line in text.splitlines()):
        raise ValueError("网卡实际模式不匹配")
    match = re.search(r"^\s*channel (\d+)\b", text, re.MULTILINE)
    if not match:
        raise ValueError("网卡没有实际信道")
    return int(match[1])


def visible(line):
    # 只显示本程序已审计的固定阶段、消息 ID 和长度，不转发任意子进程正文。
    display_scalar = r"(?:absent|integer:(?:0|[1-9]\d{0,5})|integer_out_of_range|boolean:(?:true|false)|real|string|data|array|dictionary|other)"
    return bool(re.fullmatch(
        r"(?:跨协议诊断 seq=\d{1,20} elapsed_us=\d{1,20} connection=\d{1,20} channel=(?:bluetooth|iap_tunnel|control|event) stage=(?:opened|link_ready|authenticated|identified|wifi_sent|handoff_sent|info_written|setup_written|record_written|teardown_received|peer_eof|disconnect_requested|closed) status=(?:0|[1-5]\d{2}) reason=(?:none|peer_eof|local_cancelled|completed|connection_reset|connection_aborted|timed_out|invalid_data|broken_pipe|interrupted|unexpected_eof|io_error)|"
        r"跨协议诊断 budget=(?:detail limit=2048|terminal limit=128)|"
        r"iAP2 TX message=0x[0-9a-f]{4} bytes=\d+|"
        r"iAP2 RX message=0x[0-9a-f]{4} state=Handshake \{ "
        r"authentication: (?:Initial|CertificateSent|SignatureSent|Accepted), "
        r"identification: (?:Initial|Sent|Retried|Accepted), "
        r"excluded: \[(?:\d+(?:, \d+)*)?\], "
        r"failed: (?:true|false), start_requested: (?:true|false), "
        r"wireless_available: (?:true|false) \}|"
        r"iAP2 身份拒绝参数 ids=\[\d+(?:, \d+){0,31}\]|"
        r"iAP2 拒绝参数结构 parameter=[67] value_bytes=\d{1,4}|"
        r"iAP2 (?:认证成功|身份声明成功) ready=(?:true|false)|"
        r"无线会话结束: (?:iPhone 未提供可重试的身份拒绝参数|"
        r"身份拒绝参数不完整或过多|身份拒绝参数长度或重复项不合法|"
        r"iPhone 拒绝必要无线能力，不能降级成不可用的身份声明|"
        r"iPhone 再次拒绝身份声明|iPhone 拒绝认证|iAP2 在认证完成前断开)|"
        r"手机无线 CarPlay 可用状态 available=(?:true|false)|"
        r"手机连接阶段 stage=(?:start_wait|discovery|connect_request)|"
        r"手机控制服务已发现 peer=[0-9.]+:\d{1,5}|"
        r"手机连接请求已获启动交接许可；开始单次发现|"
        r"手机已接受一次连接请求 peer=[0-9.]+:\d{1,5}；仍需回连及 Pair-Verify，不代表媒体就绪|"
        r"手机连接请求失败分类 kind=(?:TimedOut|Interrupted|InvalidInput|InvalidData|ConnectionRefused|ConnectionReset|Other)|"
        r"手机控制已认证；可选连接请求已结束，不中止接收端|"
        r"CarPlay 控制链路实验就绪 listen=[0-9.]+:7000 allow_peer=[0-9.]+ screen_bridge=Some\(127\.0\.0\.1:9001\)；完整手机会话仍待验证|"
        r"CarPlay 浏览器桥接就绪 listen=127\.0\.0\.1:8767 ingest=127\.0\.0\.1:9001；当前需要上游接收层送入 H\.264|"
        r"接收端控制连接建立 peer=[0-9.]+:\d{1,5}|"
        r"接收端拒绝非指定地址 peer=[0-9.]+:\d{1,5} allowed=[0-9.]+ count=(?:[1-9]|1[0-6])|"
        r"接收端控制计数 accepted=\d{1,20} rejected=\d{1,20}|"
        r"加密记录拒绝 reason=(?:buffer|length|authentication) received_bytes=\d{1,20} limit_bytes=\d{1,20}|"
        r"控制诊断 event=request request=\d{1,20} method=(?:GET|POST|OPTIONS|SETUP|RECORD|TEARDOWN|OTHER) path=(?:/info|/auth-setup|/pair-setup|/pair-verify|/fp-setup|/command|/feedback|/reverse|\*|other) body_bytes=\d{1,8} iap_authenticated=(?:true|false)(?: control_authenticated=(?:true|false))? elapsed_ms=\d{1,20}|"
        r"(?:控制|事件)诊断 event=response request=\d{1,20} method=(?:GET|POST|OPTIONS|SETUP|RECORD|TEARDOWN|OTHER) path=(?:/info|/auth-setup|/pair-setup|/pair-verify|/fp-setup|/command|/feedback|/reverse|\*|other) status=[1-5]\d{2} body_bytes=\d{1,8} plain_bytes=\d{1,8} wire_bytes=\d{1,8} records=\d{1,8} max_record_bytes=\d{1,8} authenticated=(?:true|false) elapsed_ms=\d{1,20}|"
        r"(?:控制|事件)诊断 event=records records=\d{1,8} plain_bytes=\d{1,8} max_record_bytes=\d{1,8} elapsed_ms=\d{1,20}|"
        r"控制诊断 event=first_after_info received_bytes=\d{1,8} after_info_ms=\d{1,20} elapsed_ms=\d{1,20}|"
        r"控制诊断 event=initial_setup_fields request_keepalive=(?:absent|true|false|invalid) timing_port=(?:true|false) event_port=(?:true|false) keepalive_port=(?:true|false) features_valid=(?:true|false) enabled_iap=(?:true|false) enabled_view_areas=(?:true|false) other_features=(?:[0-9]|1[0-6]) elapsed_ms=\d{1,20}|"
        r"控制诊断 event=info_fields features=(?:[0-9a-f]{16}|absent|invalid) status_flags=(?:[0-9a-f]{16}|absent|invalid) keepalive=(?:absent|true|false|invalid) displays=(?:[0-2]|absent|invalid) hid_devices=(?:[0-9]|1[0-6]|absent|invalid) audio_formats=(?:[0-9]|1[0-6]|absent|invalid) audio_latencies=(?:[0-9]|1[0-6]|absent|invalid) resources=(?:[0-9]|1[0-6]|absent|invalid) screen_resource=(?:true|false) audio_resource=(?:true|false) app_states=(?:[0-9]|1[0-6]|absent|invalid) extended_features=(?:[0-9]|1[0-6]|absent|invalid) view_areas=(?:[0-8]|absent|invalid) elapsed_ms=\d{1,20}|"
        rf"控制诊断 event=info_display_fields display=[01] dictionary=(?:true|false) type={display_scalar} max_fps={display_scalar} width={display_scalar} height={display_scalar} width_mm={display_scalar} height_mm={display_scalar} features={display_scalar} primary_input={display_scalar} initial_view={display_scalar} refresh_rate={display_scalar} rotation={display_scalar} overscanned={display_scalar} view_areas=(?:[0-4]|absent|invalid) hid_links=(?:[0-9]|1[0-6]|absent|invalid) elapsed_ms=\d{{1,20}}|"
        rf"控制诊断 event=info_view_area display=[01] area=[0-3] dictionary=(?:true|false) width={display_scalar} height={display_scalar} x={display_scalar} y={display_scalar} safe_type={display_scalar} safe_width={display_scalar} safe_height={display_scalar} safe_x={display_scalar} safe_y={display_scalar} draw_outside={display_scalar} elapsed_ms=\d{{1,20}}|"
        r"(?:控制|事件)诊断 event=closed reason=(?:peer_eof|local_cancelled|completed|connection_reset|connection_aborted|timed_out|invalid_data|broken_pipe|interrupted|unexpected_eof|io_error) elapsed_ms=\d{1,20} after_info_ms=(?:none|\d{1,20}) requests=\d{1,20} received_bytes=\d{1,20} sent_bytes=\d{1,20} first_after_info=(?:true|false)|"
        r"(?:控制|事件)诊断 event=budget_exhausted limit=512|"
        r"控制诊断 event=teardown_shape shape=(?:empty_bytes|empty_dict|streams_array|other_dict|invalid_or_unsupported|over_budget) keys=\d{1,4} streams=\d{1,4} elapsed_ms=\d{1,20}|"
        r"事件诊断 event=request request=\d{1,20} method=(?:GET|POST|OPTIONS|SETUP|RECORD|TEARDOWN|OTHER) path=(?:/info|/auth-setup|/pair-setup|/pair-verify|/fp-setup|/command|/feedback|/reverse|\*|other) body_bytes=\d{1,8} elapsed_ms=\d{1,20}|"
        r"事件诊断 event=peer_response status=[1-5]\d{2} body_bytes=\d{1,8} elapsed_ms=\d{1,20}|"
        r"事件诊断 event=command kind=(?:keyframe|hid|iap) plain_bytes=\d{1,8} wire_bytes=\d{1,8} elapsed_ms=\d{1,20}|"
        r"(?:首次登记|已登记重连)阶段 response_state=(?:2|4|6)|"
        r"首次登记已验签并持久保存；登记窗口已关闭|"
        r"已登记手机重新握手已验签；未新增或修改信任记录|"
        r"接收端控制认证完成；已启用加密控制通道|"
        r"会话 RECORD 已接收 screen_ready=(?:true|false) clock_sample_ready=(?:true|false)|"
        r"会话 RECORD 拒绝 reason=(?:ended|initial_setup_missing|channel_failed)|"
        r"会话模式通知已记录 count=\d{1,20} resources=\d{1,2} app_states=\d{1,2} screen_entity=(?:none|-?\d{1,19}) screen_permanent=(?:none|-?\d{1,19}) audio_entity=(?:none|-?\d{1,19}) audio_permanent=(?:none|-?\d{1,19})|"
        r"会话模式通知详细预算已耗尽 limit=16|"
        r"会话模式通知汇总 count=\d{1,20}|"
        r"屏幕关键帧请求已入队；仍待手机确认|"
        r"手机事件端口已连入；仍需加密报文认证|"
        r"手机屏幕端口已连入；仍待认证视频帧|"
        r"媒体时钟已取得有效样本|"
        r"手机已确认关键帧请求 response_bytes=\d{1,8}|"
        r"接收端应答 path=(?:/info|/auth-setup|/command|\*) status=[1-5]\d{2} control_authenticated=(?:true|false) bytes=\d{1,8}|"
        r"接收端控制失败分类 peer=[0-9.]+:\d{1,5} kind=(?:TimedOut|UnexpectedEof|InvalidData|InvalidInput|PermissionDenied|ConnectionReset|ConnectionAborted|BrokenPipe|WouldBlock|Interrupted|Other)|"
        r"接收端请求 method=(?:OPTIONS|GET|POST|SETUP|RECORD|TEARDOWN) path=(?:\*|/info|/auth-setup|/pair-setup|/pair-verify|/command) body_bytes=\d{1,8} iap_authenticated=(?:true|false)|"
        r"无线 iAP2 服务已登记 adapter=hci0 channel=4；请通过系统蓝牙配对 iPhone)", line))


def peer_address(value, addresses):
    # 管理员明确输入手机 IP；不能因为地址在私网就放开整个网段或自身地址。
    peer = ipaddress.IPv4Address(value)
    usable = [ipaddress.IPv4Interface(item) for item in addresses]
    matches = [item for item in usable if peer in item.network and peer != item.ip
               and peer not in (item.network.network_address, item.network.broadcast_address)]
    if len(usable) != 1 or len(matches) != 1 or not peer.is_private or peer.is_loopback or peer.is_link_local or peer.is_multicast:
        raise ValueError("手机 IP 必须是当前 AP 子网内的单个私有地址")
    return str(peer), str(matches[0].ip)


def read_peer(addresses, seconds):
    # 只读取一行有界输入；SSH 断开、过长输入和超时都走原有 finally 回收 AP。
    print(f"AP_TEST_JOIN_REQUIRED seconds={seconds}；请连接现有 TeslaBox 热点并提供 iPhone 的 Wi-Fi IP", flush=True)
    if not select.select([sys.stdin], [], [], seconds)[0]:
        raise TimeoutError("等待明确手机 IP 超时")
    # readline 会在仅收到半行时再次阻塞；单次 read 后拒绝半行，截止时间才有效。
    line = os.read(sys.stdin.fileno(), 33)
    if not line or len(line) > 17 or not line.endswith(b"\n"):
        raise ValueError("手机 IP 输入缺失或超过长度限制")
    return peer_address(line.decode("ascii").strip(), addresses)


def private_directory(path):
    if not path.exists():
        path.mkdir(mode=0o700)
    info = path.lstat()
    if not stat.S_ISDIR(info.st_mode) or info.st_uid != os.geteuid() or info.st_mode & 0o077:
        raise ValueError("实验身份目录必须是 root 私有目录，不能是链接")


def private_json(path, value):
    descriptor = os.open(path, os.O_WRONLY | os.O_CREAT | os.O_EXCL, 0o600)
    with os.fdopen(descriptor, "w", encoding="utf-8") as output:
        json.dump(value, output, ensure_ascii=False)


def receiver_arguments(binary, config, peer, listen, identity, profile, activate=False,
                       enrollment_pin=None, enrollment_seconds=None, reconnect=False, touch_contacts=None):
    arguments = [str(binary), "--config", str(config), "wireless-receiver",
            "--listen", f"{listen}:7000", "--allow-peer", peer,
            "--oracle", "127.0.0.1:5290", "--pairing-file", str(identity),
            "--screen-bridge", "127.0.0.1:9001", "--receiver-profile", str(profile),
            "--advertise-interface", "ap0"]
    if touch_contacts is not None:
        if type(touch_contacts) is not int or touch_contacts not in (1, 2):
            raise ValueError("触控对照只允许明确的一个或两个槽位")
        arguments.extend(["--touch-listen", "127.0.0.1:9110", "--touch-contacts", str(touch_contacts)])
    if activate:
        arguments.append("--activate-phone")
    if enrollment_pin is not None or enrollment_seconds is not None:
        if enrollment_pin is None or enrollment_seconds is None or not 1 <= enrollment_seconds <= 120:
            raise ValueError("首次登记需要私有 PIN 文件和最多 120 秒窗口")
        arguments.extend(["--reconnect-pin-file" if reconnect else "--pairing-pin-file", str(enrollment_pin),
                          "--reconnect-window-secs" if reconnect else "--pairing-window-secs", str(enrollment_seconds)])
    elif reconnect:
        raise ValueError("已登记重连需要私有 PIN 文件和短时窗口")
    return arguments


def enrollment_file(folder):
    # 已核对的公开 CarPlay 实验码；不是蓝牙确认码，仍须验证 SRP 和手机长期签名。
    path = folder / "enrollment-pin"
    descriptor = os.open(path, os.O_WRONLY | os.O_CREAT | os.O_EXCL, 0o600)
    with os.fdopen(descriptor, "wb") as output:
        output.write(b"3939\n")
    return path


def wait_ready(child, log, marker, seconds=12, ready_validator=visible):
    deadline = time.monotonic() + seconds
    while time.monotonic() < deadline:
        if child.poll() is not None:
            # 启动失败也需要可追踪证据；只保留 root 私有日志，不把未知正文回显给终端。
            with log.open("rb") as source:
                body = source.read(262145)
            if len(body) > 262144:
                raise ValueError("实验启动失败日志超过预算")
            report = log.parent.parent / ("receiver-start-failed-" + uuid.uuid4().hex + ".json")
            private_json(report, dict(stage=marker, exit_code=child.returncode,
                                     log=body.decode("utf-8", errors="replace")))
            print(f"PRIVATE_STARTUP_FAILURE_REPORT file={report.name}", flush=True)
            raise RuntimeError("实验进程在就绪前退出")
        if log.stat().st_size > 262144:
            raise ValueError("实验就绪日志超过预算")
        lines = log.read_text(encoding="utf-8", errors="replace").splitlines()
        if any(line.startswith(marker) and ready_validator(line) for line in lines):
            return
        time.sleep(0.05)
    raise TimeoutError("实验进程未实际就绪")


def window_report(log, destination, outcome, keep_alive_low_power=None, omit_audio_fields=None, touch_contacts=None):
    # 保留白名单证据而非整个子进程日志；正常到期也不能在回收目录时丢掉诊断。
    if outcome not in ("expired", "interrupted_or_failed"):
        raise ValueError("未知的实验结束类型")
    with log.open("rb") as source:
        body = source.read(262145)
    if len(body) > 262144:
        raise ValueError("实验运行日志超过报告预算")
    lines = [line for line in body.decode("utf-8", errors="replace").splitlines() if visible(line)]
    if keep_alive_low_power is not None and not isinstance(keep_alive_low_power, bool):
        raise ValueError("未知的低功耗保活实验值")
    if omit_audio_fields is not None and not isinstance(omit_audio_fields, bool):
        raise ValueError("未知的无音频字段实验值")
    if touch_contacts is not None and (type(touch_contacts) is not int or touch_contacts not in (1, 2)):
        raise ValueError("未知的触控槽位实验值")
    report = destination / ("receiver-window-" + uuid.uuid4().hex + ".json")
    value = dict(outcome=outcome, acceptance_claim=False, public_events=lines)
    if keep_alive_low_power is not None or omit_audio_fields is not None or touch_contacts is not None:
        value["variant"] = dict(other_capabilities_unchanged=True)
        if keep_alive_low_power is not None:
            value["variant"]["keep_alive_low_power"] = keep_alive_low_power
        if omit_audio_fields is not None:
            value["variant"]["omit_audio_fields"] = omit_audio_fields
        if touch_contacts is not None:
            value["variant"]["touch_contacts"] = touch_contacts
            value["variant"]["other_capabilities_unchanged"] = False
    private_json(report, value)
    print(f"PRIVATE_WINDOW_REPORT file={report.name} events={len(lines)}", flush=True)
    return report


def stop(child):
    if child is None:
        return
    if child.poll() is None:
        child.terminate()
        try:
            child.wait(timeout=6)
        except subprocess.TimeoutExpired:
            child.kill()
            child.wait(timeout=2)
    print(f"OWNED_PROCESS_EXIT code={child.returncode}", flush=True)


def interrupted(_number, _frame):
    raise InterruptedError("无线实验被取消")


def run(args, receiver_factory=receiver_arguments, report_factory=window_report, ready_waiter=wait_ready,
        window_completed=None):
    if os.name != "posix" or os.geteuid() != 0 or not args.run_hardware:
        raise PermissionError("仅允许 Linux root 显式授权运行")
    binary = args.binary.resolve(strict=True)
    if args.binary.is_symlink() or hashlib.sha256(binary.read_bytes()).hexdigest() != args.sha256:
        raise ValueError("候选摘要不匹配或路径是符号链接")
    long_reference = getattr(args, "reference_long_running", False)
    if long_reference and (not args.receiver_test or not args.reconnect_phone or args.enroll_phone):
        raise ValueError("参考长测仅允许已登记手机")
    if not 30 <= args.seconds <= (3600 if long_reference else 180) or not 1 <= args.channel <= 196:
        raise ValueError("实验窗口或信道不合法")
    if not 30 <= args.peer_wait_seconds <= 180:
        raise ValueError("手机 IP 准备窗口不合法")
    if args.activate_phone and not args.receiver_test:
        raise ValueError("显式手机连接请求需要真实接收端测试")
    if args.disable_low_power_keepalive and not args.receiver_test:
        raise ValueError("keepAliveLowPower A/B 必须使用真实接收端测试")
    if args.legacy_audio_fields and not args.receiver_test:
        raise ValueError("无音频字段 A/B 必须使用真实接收端测试")
    if args.touch_contacts is not None and not args.receiver_test:
        raise ValueError("触控对照必须使用真实接收端测试")
    if args.enroll_phone and (not args.receiver_test or args.seconds > 120):
        raise ValueError("首次登记必须使用真实接收端且实验最多 120 秒")
    if args.reconnect_phone and (args.enroll_phone or not args.receiver_test or args.seconds > (3600 if long_reference else 120)):
        raise ValueError("已登记重连必须与首次登记互斥，使用真实接收端且最多 120 秒")
    for value in (args.ap_uuid, args.upstream_uuid):
        if str(uuid.UUID(value)) != value:
            raise ValueError("只接受已核对的网络 UUID")
    for prop in ("Powered", "Discoverable", "Pairable", "Discovering"):
        if command("busctl", "--system", "get-property", "org.bluez", "/org/bluez/hci0",
                   "org.bluez.Adapter1", prop) != "b false":
            raise ValueError("蓝牙窗口已被其他操作接管")
    if command("systemctl", "show", "teslabox-bt-trigger.service", "-p", "ActiveState",
               "--value") != "inactive":
        raise ValueError("私有热点触发服务必须保持停用")
    if command("busctl", "--system", "get-property", "org.bluez",
               "/org/bluez/hci0/dev_E4_9C_67_18_0B_FB", "org.bluez.Device1", "Paired") != "b true":
        raise ValueError("指定 iPhone 尚未完成持久配对")
    active = command("nmcli", "-t", "-f", "UUID,DEVICE", "connection", "show", "--active")
    if f"{args.upstream_uuid}:wlan0" not in active.splitlines():
        raise ValueError("明确的上游连接不是当前 wlan0 连接")
    if any(line.endswith(":ap0") for line in active.splitlines()):
        raise ValueError("AP 已有连接，不接管其他所有者")
    if channel(command("iw", "dev", "wlan0", "info"), "managed") != args.channel:
        raise ValueError("上游已换信道，需重新核对后再试")
    mode = command("nmcli", "-g", "802-11-wireless.mode", "connection", "show", "uuid", args.ap_uuid)
    security = command("nmcli", "-g", "802-11-wireless-security.key-mgmt", "connection", "show", "uuid", args.ap_uuid)
    if mode != "ap" or security != "wpa-psk":
        raise ValueError("只克隆已核对的 WPA2 AP 配置")
    settings = json.loads(args.config.read_text(encoding="utf-8"))
    if settings["oracle_listen"] != "127.0.0.1:5290" or settings["adapter"] != "hci0" or settings["rfcomm_channel"] != 4:
        raise ValueError("独立签名端口与适配器不匹配")
    module_path = Path(__file__).with_name("board-mfi-sign-check.py")
    spec = importlib.util.spec_from_file_location("mfi_check", module_path)
    client = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(client)
    # 签名端口被占用时拒绝；不能停止未知服务。
    with socket.socket() as guard:
        guard.bind(("127.0.0.1", 5290))
    oracle = wireless = bridge = None
    name = "TeslaBox-CarPlay-test-" + uuid.uuid4().hex
    cloned = False
    old_handlers = {}
    try:
        for number in (signal.SIGINT, signal.SIGTERM, signal.SIGHUP):
            old_handlers[number] = signal.signal(number, interrupted)
        with tempfile.TemporaryDirectory(prefix="wireless-mfi-check-", dir=binary.parent) as folder:
            folder = Path(folder)
            folder.chmod(0o700)
            # 即使命令在返回前失败，也尝试清理自己生成的唯一临时名称。
            cloned = True
            command("nmcli", "connection", "clone", "--temporary", "uuid", args.ap_uuid, name)
            command("nmcli", "connection", "modify", "--temporary", "id", name,
                    "connection.autoconnect", "no", "connection.interface-name", "ap0",
                    "802-11-wireless.band", "bg" if args.channel <= 14 else "a",
                    "802-11-wireless.channel", str(args.channel))
            command("nmcli", "--wait", "20", "connection", "up", "id", name, "ifname", "ap0")
            if channel(command("iw", "dev", "ap0", "info"), "AP") != args.channel:
                raise ValueError("临时 AP 实际信道不匹配")
            if channel(command("iw", "dev", "wlan0", "info"), "managed") != args.channel:
                raise ValueError("上游在 AP 启动期间改变，停止实验")
            ssid = command("nmcli", "--escape", "no", "-g", "802-11-wireless.ssid", "connection", "show", "id", name)
            password = command("nmcli", "--escape", "no", "--show-secrets", "-g", "802-11-wireless-security.psk",
                               "connection", "show", "id", name)
            if not 8 <= len(password) <= 63 or not password.isascii():
                raise ValueError("临时 AP 密码不适用于当前无线协议")
            settings["name"] = "TeslaBox CarPlay"
            settings["wifi"] = dict(interface="ap0", ssid=ssid, passphrase=password, channel=args.channel)
            config = folder / "config.json"
            private_json(config, settings)
            del password
            arguments = [str(binary), "--config", str(config), "wireless"]
            if args.receiver_test:
                addresses = command("nmcli", "-g", "IP4.ADDRESS", "device", "show", "ap0").splitlines()
                peer, listen = read_peer(addresses, args.peer_wait_seconds)
                for address in ((listen, 7000), ("127.0.0.1", 8767), ("127.0.0.1", 9001)):
                    with socket.socket() as guard:
                        guard.bind(address)
                if args.touch_contacts is not None:
                    with socket.socket() as guard:
                        guard.bind(("127.0.0.1", 9110))
                # 自己生成并保留身份，避免下一轮替换已登记公钥；不导入任何盒子密钥。
                identity_dir = binary.parent / "receiver-test-private"
                if receiver_factory is receiver_arguments:
                    private_directory(identity_dir)
                identity = identity_dir / "identity.json"
                if receiver_factory is receiver_arguments and not identity.exists() and not identity.is_symlink():
                    command(str(binary), "pairing-init", "--file", str(identity),
                            "--identifier", "TeslaBox-CarPlay-ROCK3A-Experimental")
                profile = folder / "receiver-profile.json"
                ap_mac = command("cat", "/sys/class/net/ap0/address").upper()
                bt_address = command("busctl", "--system", "get-property", "org.bluez",
                                     "/org/bluez/hci0", "org.bluez.Adapter1", "Address")
                if not re.fullmatch(r's "(?:[0-9a-fA-F]{2}:){5}[0-9a-fA-F]{2}"', bt_address):
                    raise ValueError("实际蓝牙地址格式不合法")
                bt_mac = bt_address[3:-1].upper()
                private_json(profile, dict(name="TeslaBox CarPlay Experimental", device_id=ap_mac,
                             bluetooth_id=bt_mac, source_version="0.1.0", right_hand_drive=False,
                             keep_alive_low_power=not args.disable_low_power_keepalive,
                             omit_audio_fields=not args.legacy_audio_fields,
                             display=dict(uuid="9383d029-3c08-467a-bb66-96f30a4b964e", width=1280,
                                          height=720, width_mm=280, height_mm=158, fps=30)))
                handshake_enabled = args.enroll_phone or args.reconnect_phone
                pin_file = enrollment_file(folder) if handshake_enabled else None
                arguments = receiver_factory(binary, config, peer, listen, identity, profile,
                                               args.activate_phone, pin_file,
                                               args.seconds if handshake_enabled else None,
                                               reconnect=args.reconnect_phone, touch_contacts=args.touch_contacts)
                scope = "first_enrollment_explicit_max120" if args.enroll_phone else "first_enrollment_window_closed"
                if args.reconnect_phone:
                    scope = "known_peer_reconnection_only_max3600_no_enrollment" if long_reference else "known_peer_reconnection_only_max120_no_enrollment"
                if receiver_factory is receiver_arguments:
                    print(f"RECEIVER_TEST_SCOPE actual_endpoint_and_identification; {scope}", flush=True)
                    print(f"RECEIVER_TEST_VARIANT keep_alive_low_power={str(not args.disable_low_power_keepalive).lower()}; other_capabilities_unchanged={str(args.touch_contacts is None).lower()}", flush=True)
                    print(f"RECEIVER_AUDIO_FIELDS omit_audio_fields={str(not args.legacy_audio_fields).lower()}; no_audio_capability_added", flush=True)
                if args.touch_contacts is not None:
                    print(f"RECEIVER_TOUCH_VARIANT contacts={args.touch_contacts}; explicit_hid_and_display_input_only", flush=True)
            with (folder / "oracle.log").open("wb") as oracle_log, (folder / "wireless.log").open("wb") as wireless_log, (folder / "bridge.log").open("wb") as bridge_log:
                oracle = subprocess.Popen([str(binary), "--config", str(config), "mfi-oracle"],
                                          stdin=subprocess.DEVNULL, stdout=subprocess.DEVNULL, stderr=oracle_log)
                startup = time.monotonic() + 8
                while True:
                    if oracle.poll() is not None:
                        raise RuntimeError("实体签名服务提前退出")
                    try:
                        certificate = client.request(("127.0.0.1", 5290), 1, b"", startup)
                        break
                    except ConnectionRefusedError:
                        if time.monotonic() >= startup:
                            raise TimeoutError("签名服务未就绪")
                        time.sleep(0.05)
                if hashlib.sha256(certificate).hexdigest() != args.certificate_sha256:
                    raise ValueError("实体证书摘要改变")
                if args.receiver_test:
                    bridge_arguments = [str(binary), "bridge", "--listen", "127.0.0.1:8767",
                                               "--ingest", "127.0.0.1:9001", "--input-format", "timestamped",
                                               "--token-file", str(folder / "browser-token")]
                    if args.touch_contacts is not None:
                        bridge_arguments.extend(["--touch-contacts", str(args.touch_contacts)])
                    bridge = subprocess.Popen(bridge_arguments,
                                              stdin=subprocess.DEVNULL, stdout=subprocess.DEVNULL, stderr=bridge_log)
                    ready_waiter(bridge, folder / "bridge.log", "CarPlay 浏览器桥接就绪")
                command("busctl", "--system", "set-property", "org.bluez", "/org/bluez/hci0",
                        "org.bluez.Adapter1", "Powered", "b", "true")
                wireless = subprocess.Popen(arguments,
                                            stdin=subprocess.DEVNULL, stdout=subprocess.DEVNULL, stderr=wireless_log)
                ready_waiter(wireless, folder / "wireless.log", "无线 iAP2 服务已登记")
                if args.receiver_test:
                    ready_waiter(wireless, folder / "wireless.log", "CarPlay 控制链路实验就绪")
                print(f"WIRELESS_TEST_WINDOW seconds={args.seconds} channel={args.channel}", flush=True)
                outcome = "interrupted_or_failed"
                try:
                    deadline = time.monotonic() + args.seconds
                    emitted = 0
                    while time.monotonic() < deadline:
                        if (folder / "wireless.log").stat().st_size > (4194304 if long_reference else 262144):
                            raise ValueError("认证诊断日志超过预算")
                        lines = (folder / "wireless.log").read_text(encoding="utf-8", errors="replace").splitlines()
                        for line in lines[emitted:]:
                            if visible(line):
                                print(line, flush=True)
                        emitted = len(lines)
                        if oracle.poll() is not None or (bridge is not None and bridge.poll() is not None):
                            raise RuntimeError("独立签名或无线进程提前退出；不代表认证成功")
                        if wireless.poll() is not None:
                            if window_completed is not None and window_completed(wireless, folder / "wireless.log"):
                                break
                            raise RuntimeError("独立签名或无线进程提前退出；不代表认证成功")
                        time.sleep(0.2)
                    outcome = "expired"
                    print("WIRELESS_TEST_WINDOW_ENDED_NOT_AN_ACCEPTANCE_CLAIM", flush=True)
                finally:
                    # 先等进程退出，让最后的连接计数落盘，再保存；失败仍由外层回收 AP。
                    try:
                        stop(wireless)
                        stop(oracle)
                        stop(bridge)
                    finally:
                        if args.receiver_test:
                            report_factory(folder / "wireless.log", binary.parent, outcome,
                                          keep_alive_low_power=not args.disable_low_power_keepalive,
                                          omit_audio_fields=not args.legacy_audio_fields, touch_contacts=args.touch_contacts)
    finally:
        # SIGTERM/HUP、命令失败、正常到期均回收自有进程及临时 AP；不修改原配置。
        for number in old_handlers:
            signal.signal(number, signal.SIG_IGN)
        stop(wireless)
        stop(oracle)
        stop(bridge)
        try:
            command("busctl", "--system", "set-property", "org.bluez", "/org/bluez/hci0",
                    "org.bluez.Adapter1", "Powered", "b", "false")
        finally:
            if cloned:
                command("nmcli", "--wait", "10", "connection", "down", "id", name, allow_failure=True)
                command("nmcli", "connection", "delete", "id", name, allow_failure=True)
            for number, handler in old_handlers.items():
                signal.signal(number, handler)
            print("TEMPORARY_AP_CLEANUP_FINISHED_VERIFY_ORIGINAL_STATE", flush=True)


if __name__ == "__main__":
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--legacy-audio-fields", action="store_true",
                        help="只恢复旧的两个音频空数组，不开启音频；必须配合 --receiver-test")
    parser.add_argument("--touch-contacts", type=int, choices=(1, 2),
                        help="显式触控对照：两端使用一致槽位；默认不启用 HID，要求 --receiver-test")
    parser.add_argument("--run-hardware", action="store_true")
    parser.add_argument("--binary", type=Path, required=True)
    parser.add_argument("--sha256", required=True)
    parser.add_argument("--config", type=Path, required=True)
    parser.add_argument("--certificate-sha256", required=True)
    parser.add_argument("--ap-uuid", required=True)
    parser.add_argument("--upstream-uuid", required=True)
    parser.add_argument("--channel", type=int, required=True)
    parser.add_argument("--seconds", type=int, default=120)
    parser.add_argument("--receiver-test", action="store_true",
                        help="启用已确认的实验主屏和真实接收端，先通过 stdin 明确指定手机 IP")
    parser.add_argument("--peer-wait-seconds", type=int, default=120)
    parser.add_argument("--activate-phone", action="store_true",
                        help="显式实验：实际启动交接后单次发现并请求手机连接，默认关闭")
    parser.add_argument("--disable-low-power-keepalive", action="store_true",
                        help="独立 A/B：仅将 keepAliveLowPower 声明改为 false，其他能力不变")
    parser.add_argument("--enroll-phone", action="store_true",
                        help="仅获授权的单次首次登记：私有实验码文件，最多 120 秒，成功后关闭")
    parser.add_argument("--reconnect-phone", action="store_true",
                        help="仅原标识和公钥的已登记手机重新握手，最多 120 秒，不新增信任")
    run(parser.parse_args())
