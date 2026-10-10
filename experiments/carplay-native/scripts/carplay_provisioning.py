"""通过本机私有原子请求协调首次登记，不释放 AP、签名器或浏览器桥接。"""
import hashlib
import json
import math
import os
from pathlib import Path
import re
import stat
import time


def decode_request(body):
    """请求不含配对数字；任务标识和期限限制旧请求或错误手机的影响。"""
    if len(body) > 4096:
        raise ValueError("首次登记请求超过预算")
    def fields(pairs):
        result = {}
        for key, value in pairs:
            if key in result:
                raise ValueError("首次登记请求字段重复")
            result[key] = value
        return result
    value = json.loads(body, object_pairs_hook=fields)
    if not isinstance(value, dict) or set(value) != {"request_id", "phone_id", "action", "expires_at_ms"}:
        raise ValueError("首次登记请求字段不匹配")
    if not isinstance(value["request_id"], str) or not re.fullmatch(r"[a-f0-9]{32}", value["request_id"]):
        raise ValueError("首次登记任务标识不合法")
    if not isinstance(value["phone_id"], str) or not re.fullmatch(r"(?:[A-F0-9]{2}:){5}[A-F0-9]{2}", value["phone_id"]) or value["phone_id"] == "00:00:00:00:00:00":
        raise ValueError("首次登记手机标识不合法")
    if value["action"] not in ("pause", "enroll", "cancel") or type(value["expires_at_ms"]) is not int or not 0 < value["expires_at_ms"] <= 9007199254740991:
        raise ValueError("首次登记动作或期限不合法")
    return value


def read_request(path):
    """只读取同用户持有的私有普通文件，原子替换不能绕过文件身份校验。"""
    if os.name != "posix":
        raise OSError("首次登记请求读取只允许 Linux 私有文件")
    descriptor = os.open(path, os.O_RDONLY | os.O_NOFOLLOW)
    try:
        info = os.fstat(descriptor)
        if not stat.S_ISREG(info.st_mode) or info.st_uid != os.geteuid() or info.st_mode & 0o077 or info.st_nlink != 1 or info.st_size > 4096:
            raise PermissionError("首次登记请求不是私有普通文件")
        with os.fdopen(descriptor, "rb", closefd=False) as source:
            body = source.read(4097)
        return decode_request(body), hashlib.sha256(body).digest()
    finally:
        os.close(descriptor)


class Provisioning:
    """任务槽在接收端清理后才转换状态，成功仅认本轮已验证的新视频。"""

    def __init__(self, runtime):
        self.path = Path(runtime) / "provisioning-request.json"
        self.consumed = None
        self.active = None
        self.deadline = None
        self.state = dict(state="idle", running=False)

    def snapshot(self):
        """状态不含数字、IP、私钥或协议控制器身份。"""
        return dict(self.state)

    def poll(self, switch, enabled=True):
        """switch 只允许替换自有接收会话；异常交给服务的已有清理路径处理。"""
        if self.active and (not enabled or time.monotonic() >= self.deadline):
            switch("resume", None, 0)
            self._finish("canceled" if not enabled else "expired")
        try:
            request, digest = read_request(self.path)
        except FileNotFoundError:
            return
        except (OSError, ValueError, TypeError):
            # 不把畸形文件、错误正文或文件路径写入公开状态。
            return
        if digest == self.consumed:
            return
        self.consumed = digest
        self.apply(request, switch, enabled)

    def apply(self, request, switch, enabled=True):
        """接收端停止完成后才确认暂停；过期请求不能唤醒或抢占其他手机任务。"""
        request = decode_request(json.dumps(request).encode("utf-8"))
        matching = self.active and all(request[key] == self.active[key] for key in ("request_id", "phone_id", "expires_at_ms"))
        if self.active and not matching:
            return
        if request["action"] == "cancel":
            if matching:
                switch("resume", None, 0)
                self._finish("canceled")
            elif not self.active and self.state.get("request_id") != request["request_id"]:
                # 取消可能早于 pause 被消费；仍需确认本轮不会稍后启动。
                self.state = dict(state="canceled", running=False, request_id=request["request_id"], phone_id=request["phone_id"])
            return
        remaining = request["expires_at_ms"] / 1000 - time.time()
        if not enabled or not 0 < remaining <= 120:
            if matching:
                switch("resume", None, 0)
                self._finish("expired" if enabled else "canceled")
            return
        if request["action"] == "pause":
            if matching or self.state.get("request_id") == request["request_id"]:
                return
            if switch("paused", request["phone_id"], 0) is False:
                return
            self.active = dict(request)
            self.deadline = time.monotonic() + remaining
            self.state = dict(state="paused", running=True, request_id=request["request_id"], phone_id=request["phone_id"])
        elif matching and self.state["state"] == "paused":
            if switch("enroll", request["phone_id"], max(1, math.ceil(min(remaining, self.deadline - time.monotonic())))) is False:
                self._finish("phone_not_paired")
                return
            self.state["state"] = "enrolling"

    def observe(self, receiver, registered_phone):
        """累计旧帧或单纯蓝牙配对均不能完成首次登记。"""
        if not isinstance(receiver, dict) or not self.active or self.state["state"] != "enrolling" or registered_phone != self.active["phone_id"]:
            return False
        media = receiver.get("media", {}) if isinstance(receiver, dict) else {}
        if receiver.get("stage") != "phone_media_success" or receiver.get("session_verified") is not True or \
                media.get("source_connected") is not True or media.get("codec_ready") is not True or \
                type(media.get("frames")) is not int or media["frames"] <= 0:
            return False
        self._finish("video_verified")
        return True

    def fail(self):
        """异常退出不能重开限时登记；保留终态让管理端解释本轮失败。"""
        if self.active:
            self._finish("failed")

    def _finish(self, state):
        self.active, self.deadline = None, None
        self.state.update(state=state, running=False)
