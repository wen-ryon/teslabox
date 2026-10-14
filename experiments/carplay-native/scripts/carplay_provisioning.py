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
    required = {"request_id", "phone_id", "action", "expires_at_ms"}
    if not isinstance(value, dict) or not required <= set(value) or set(value) - required - {"total_expires_at_ms"}:
        raise ValueError("首次登记请求字段不匹配")
    if not isinstance(value["request_id"], str) or not re.fullmatch(r"[a-f0-9]{32}", value["request_id"]):
        raise ValueError("首次登记任务标识不合法")
    if not isinstance(value["phone_id"], str) or not re.fullmatch(r"(?:[A-F0-9]{2}:){5}[A-F0-9]{2}", value["phone_id"]) or value["phone_id"] == "00:00:00:00:00:00":
        raise ValueError("首次登记手机标识不合法")
    if value["action"] not in ("pause", "enroll", "cancel") or type(value["expires_at_ms"]) is not int or not 0 < value["expires_at_ms"] <= 9007199254740991:
        raise ValueError("首次登记动作或期限不合法")
    total = value.get("total_expires_at_ms")
    if total is not None and (type(total) is not int or not value["expires_at_ms"] <= total <= 9007199254740991):
        raise ValueError("首次登记总截止不合法")
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
        self.total_deadline = None
        self.state = dict(state="idle", running=False)
        try:
            previous, digest = read_request(self.path)
        except (OSError, ValueError, TypeError):
            pass
        else:
            if previous.get("total_expires_at_ms") is not None:
                # 新服务必须先供管理端核对旧任务终态，不能重放进程退出前的分阶段请求。
                self.consumed = digest
                self.state = dict(state="canceled", running=False, request_id=previous["request_id"], phone_id=previous["phone_id"])

    def snapshot(self):
        """状态不含数字、IP、私钥或协议控制器身份。"""
        return dict(self.state)

    def enrollment_seconds(self):
        """网络与桥接准备耗时后重新核对预算，不为过期接收端强行补一秒。"""
        if not self.active or self.state["state"] != "enrolling":
            raise ValueError("当前没有活动视频登记")
        remaining = min(self.deadline, self.total_deadline) - time.monotonic()
        if remaining <= 0:
            raise TimeoutError("首次登记视频期限已到")
        return min(120, math.ceil(remaining))

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
        identity_matches = self.active and all(request[key] == self.active[key] for key in ("request_id", "phone_id"))
        matching = identity_matches and request["expires_at_ms"] == self.active["expires_at_ms"]
        phased = identity_matches and self.active.get("total_expires_at_ms") is not None
        transition = phased and request["action"] == "enroll" and self.state["state"] == "paused" and \
            request.get("total_expires_at_ms") == self.active["total_expires_at_ms"]
        if self.active and not identity_matches:
            return
        if request["action"] == "cancel":
            if identity_matches:
                switch("resume", None, 0)
                self._finish("canceled")
            elif not self.active and self.state.get("request_id") != request["request_id"]:
                # 取消可能早于 pause 被消费；仍需确认本轮不会稍后启动。
                self.state = dict(state="canceled", running=False, request_id=request["request_id"], phone_id=request["phone_id"])
            return
        wall_now = time.time()
        mono_now = time.monotonic()
        remaining = request["expires_at_ms"] / 1000 - wall_now
        if self.active and time.monotonic() >= self.deadline:
            switch("resume", None, 0)
            self._finish("expired")
            return
        if self.active and not (matching or transition):
            return
        if not enabled or not 0 < remaining <= 120:
            if matching:
                switch("resume", None, 0)
                self._finish("expired" if enabled else "canceled")
            return
        if request["action"] == "pause":
            if matching or self.state.get("request_id") == request["request_id"]:
                return
            total = request.get("total_expires_at_ms")
            total_remaining = total / 1000 - wall_now if total is not None else remaining
            # 总截止只在首次 pause 接收时固定，重复请求或进程重启不能续期。
            if total is not None and not (0 < remaining <= 60 and remaining <= total_remaining <= 180):
                return
            if switch("paused", request["phone_id"], 0) is False:
                return
            self.active = dict(request)
            self.deadline = mono_now + remaining
            self.total_deadline = mono_now + total_remaining
            if time.monotonic() >= min(self.deadline, self.total_deadline):
                switch("resume", None, 0)
                self._finish("expired")
                return
            self.state = dict(state="paused", running=True, request_id=request["request_id"], phone_id=request["phone_id"])
        elif (matching or transition) and self.state["state"] == "paused":
            next_deadline = self.deadline
            if transition:
                if request["expires_at_ms"] > self.active["total_expires_at_ms"] or time.monotonic() >= self.total_deadline:
                    return
                next_deadline = min(mono_now + remaining, self.total_deadline)
            # 最后副作用前统一读取时间；处理请求期间跨截止不能被 ceil/max 强行复活一秒。
            effect_now = time.monotonic()
            budget = min(next_deadline, self.total_deadline) - effect_now
            if effect_now >= self.deadline or budget <= 0:
                switch("resume", None, 0)
                self._finish("expired")
                return
            if transition:
                self.deadline = next_deadline
                self.active = dict(request)
            if switch("enroll", request["phone_id"], math.ceil(budget)) is False:
                self._finish("phone_not_paired")
                return
            self.state["state"] = "enrolling"

    def observe(self, receiver, registered_phone):
        """累计旧帧或单纯蓝牙配对均不能完成首次登记。"""
        if not isinstance(receiver, dict) or not self.active or self.state["state"] != "enrolling" or \
                registered_phone != self.active["phone_id"] or time.monotonic() >= self.deadline:
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
            self._finish("expired" if time.monotonic() >= min(self.deadline, self.total_deadline) else "failed")

    def _finish(self, state):
        if self.active:
            self.state.update(request_id=self.active["request_id"], phone_id=self.active["phone_id"])
        self.active, self.deadline, self.total_deadline = None, None, None
        self.state.update(state=state, running=False)
