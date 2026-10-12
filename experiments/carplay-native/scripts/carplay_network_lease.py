"""通过 TeslaBox 的统一入口管理网络，接收端不直接操作 NetworkManager。"""

import json
import os
from pathlib import Path
import stat
import subprocess
import threading
import time
import uuid


class UpstreamNotReady(RuntimeError):
    """仅表示管理器明确返回的上游信道过渡状态，不包含命令正文或凭据。"""


class OrdinaryAPActive(RuntimeError):
    """普通热点仍归用户所有，服务必须等待确认，不能以重启抢占。"""


class NetworkLease:
    """租约续期失败时停止媒体；意外退出由 TeslaBox 到期恢复原网络。"""

    def __init__(self, channels, stopped, command=None, phone=None):
        self.channels = ",".join(str(channel) for channel in channels)
        self.stopped = stopped
        self.command = command or self._command
        self.finished = threading.Event()
        self.upstream_checking = threading.Event()
        self.media_ready = threading.Event()
        self.media_ready.set()
        self.media_online = False
        self.media_recovery = threading.Event()
        self.pan_reset_failed = False
        self.pan_reset_after = 0
        self.operation_lock = threading.RLock()
        self.worker = None
        self.lease = None
        self.phone = phone
        self.phone_lock = threading.Lock()
        self.phone_revision = 0
        self.upstream = None
        self.recovery = None
        self.released = False

    def update_phone(self, phone):
        """切换媒体手机不回收 AP，旧手机的异步供网结果不能覆盖新选择。"""
        with self.phone_lock:
            if phone != self.phone:
                self.phone = phone
                self.phone_revision += 1
                self.upstream = None
                self.recovery = None

    @staticmethod
    def _command(*arguments):
        # 只记录固定操作名，防止命令参数或错误输出泄露手机与热点凭据。
        operation = arguments[0] if arguments and arguments[0] in {
            "acquire", "start", "status", "release", "renew", "upstream", "pan-reset"
        } else "other"

        def failed(reason):
            print(f"CARPLAY_NETWORK operation={operation} failed=true reason={reason}", flush=True)
            return RuntimeError("TeslaBox 网络控制请求失败")

        try:
            result = subprocess.run(["teslaboxctl", "carplay", "network", *arguments],
                                    capture_output=True, text=True, timeout=65, check=False)
        except subprocess.TimeoutExpired:
            raise failed("timeout") from None
        except OSError:
            raise failed("launch_failed") from None
        except UnicodeDecodeError:
            raise failed("invalid_response") from None
        if result.returncode != 0:
            if operation == "acquire" and result.stderr.strip() in {
                "自动化接口返回 409：普通热点正在使用，请在首次登记中确认热点交接",
                "自动化接口返回 409：热点正在占用无线网卡，当前设备不支持 AP+STA 并发",
            }:
                raise OrdinaryAPActive("等待普通热点交接确认") from None
            pending = {
                "自动化接口返回 422：无法确认 CarPlay 续租时的上游信道",
                "自动化接口返回 409：无法读取共享无线设备的上游活动信道",
            }
            if operation == "acquire" and result.stderr.strip() in pending:
                print("CARPLAY_NETWORK operation=acquire failed=true reason=upstream_not_ready", flush=True)
                raise UpstreamNotReady("上游信道尚未稳定") from None
            raise failed("command_failed") from None
        try:
            response = json.loads(result.stdout)
        except json.JSONDecodeError:
            raise failed("invalid_response") from None
        if not isinstance(response, dict):
            raise failed("invalid_response") from None
        return response

    def update_media_state(self, ready, online):
        """恢复失败时保持拨号屏障，只有清理完成后的新视频才能解除它。"""
        # 主循环持续调用此入口；重试不依赖下一次交接，静默等待也能解除失败状态。
        if self.pan_reset_failed and not self.finished.is_set() and not self.stopped.is_set():
            self.prepare_media_recovery()
        self.media_online = online
        if ready and not self.pan_reset_failed and (online or not self.media_recovery.is_set()):
            self.media_ready.set()
            self.media_recovery.clear()
        else:
            self.media_ready.clear()

    def prepare_media_recovery(self):
        """等候正在执行的供网请求完成，再仅释放专属 PAN，不能回收 AP。"""
        self.media_recovery.set()
        self.media_ready.clear()
        self.media_online = False
        if time.monotonic() < self.pan_reset_after:
            return False
        with self.operation_lock:
            with self.phone_lock:
                phone = self.phone
                self.phone_revision += 1
                self.upstream = None
            try:
                if phone:
                    result = self.command("pan-reset", "--source", "carplay-service", "--phone", phone)
                    if result.get("pan_reset") is not True:
                        raise RuntimeError("供网清理未确认")
                self.pan_reset_failed = False
                self.pan_reset_after = 0
                print("CARPLAY_SERVICE pan_recovery_complete=true", flush=True)
                return True
            except Exception:
                self.pan_reset_failed = True
                self.pan_reset_after = time.monotonic() + 5
                print("CARPLAY_SERVICE pan_recovery_failed=true", flush=True)
                return False

    def _upstream_allowed(self):
        existing_pan = self.upstream and self.upstream.get("source") == "bluetooth" and self.upstream.get("interface")
        return not self.media_recovery.is_set() and (self.media_ready.is_set() or bool(existing_pan))

    def acquire(self):
        """固定来源配合服务锁，使同一服务崩溃重启时能够认领恢复记录。"""
        for attempt in range(4):
            try:
                self.lease = self.command("acquire", "--source", "carplay-service", "--channels", self.channels, "--ttl", "5m")
                break
            except UpstreamNotReady:
                # 同一请求的首次快照与最后核验间，Wi-Fi 可能重新关联；不能因此退出并空等 systemd。
                # 仍交由管理器重新确认真实信道，不暂停无线、不猜测信道、不重试其他错误。
                if attempt == 3 or self.stopped.wait(1):
                    raise
                print("CARPLAY_NETWORK acquire_wait=true", flush=True)
        self.worker = threading.Thread(target=self._renew, daemon=True)
        self.worker.start()
        return self.lease

    def _renew(self):
        next_upstream = 0
        observed_revision = -1
        while not self.finished.wait(20):
            try:
                renewed = self.command("renew", "--source", "carplay-service", "--channels", self.channels, "--ttl", "5m")
                if renewed["runtime_ap_uuid"] != self.lease["runtime_ap_uuid"]:
                    raise ValueError("网络租约身份改变")
            except Exception:
                print("CARPLAY_SERVICE network_renew_failed=true", flush=True)
                self.stopped.set()
                return
            with self.phone_lock:
                phone = self.phone
                revision = self.phone_revision
            if revision != observed_revision:
                observed_revision = revision
                next_upstream = 0
            # 先完成手机的 Wi-Fi 角色切换，再新建 PAN；已有供网连接仍需继续维护。
            if phone and self._upstream_allowed() and time.monotonic() >= next_upstream and not self.finished.is_set():
                # 真实媒体在线证明手机仍在附近，不必让失败拨号再空等一分钟。
                next_upstream = time.monotonic() + (20 if self.media_online else 60)
                with self.operation_lock:
                    # 等待操作锁期间可能开始恢复；排队的请求不能重新占用手机热点角色。
                    if not self._upstream_allowed() or self.finished.is_set():
                        continue
                    try:
                        self.upstream_checking.set()
                        state = self.command("upstream", "--source", "carplay-service", "--phone", phone)
                        # 供网失败不终止本地画面；只有租约续期失败才停止接收端。
                        with self.phone_lock:
                            if self.phone_revision != revision or self.media_recovery.is_set():
                                continue
                            changed = state != self.upstream
                            self.upstream = state
                            # 已联网的 PAN 也及时探测，缩短失联发现时间；建链失败仍低频重试。
                            if state.get("source") in {"existing_upstream", "wifi", "wifi_recovery_required"} or state.get("source") == "bluetooth" and state.get("internet") is True:
                                next_upstream = time.monotonic() + 20
                            target = state.get("recovery_uuid")
                            channel = state.get("recovery_channel")
                            if state.get("source") == "wifi_recovery_required" and type(channel) is int and str(channel) in self.channels.split(","):
                                if isinstance(target, str) and str(uuid.UUID(target)) == target:
                                    self.recovery = target
                        if changed:
                            print("CARPLAY_SERVICE upstream=" + str(state.get("source")) + " internet=" + str(state.get("internet") is True).lower(), flush=True)
                    except Exception:
                        with self.phone_lock:
                            if self.phone_revision != revision or self.media_recovery.is_set():
                                continue
                            self.upstream = None
                        print("CARPLAY_SERVICE upstream_check_failed=true", flush=True)
                    finally:
                        self.upstream_checking.clear()

    def start(self):
        """凭据只读自 root 私有普通文件，CLI 响应不能携带密码。"""
        result = self.command("start", "--source", "carplay-service")
        identity = self.lease["runtime_ap_uuid"]
        if str(uuid.UUID(identity)) != identity or result["runtime_ap_uuid"] != identity:
            raise ValueError("热点身份与租约不一致")
        path = Path("/run/teslabox/carplay", "access-" + identity + ".json")
        if result["credential_file"] != str(path):
            raise ValueError("热点凭据路径不符合约定")
        fd = os.open(path, os.O_RDONLY | os.O_NOFOLLOW)
        try:
            info = os.fstat(fd)
            if not stat.S_ISREG(info.st_mode) or info.st_uid != 0 or stat.S_IMODE(info.st_mode) != 0o600 or info.st_nlink != 1 or info.st_size > 16384:
                raise PermissionError("热点凭据文件不是 root 私有普通文件")
            with os.fdopen(fd, "r", encoding="utf-8", closefd=False) as stream:
                access = json.load(stream)
        finally:
            os.close(fd)
        plan = self.lease["plan"]
        if access["runtime_ap_uuid"] != identity or access["interface"] != plan["ap_interface"] or access["channel"] != plan["channel"]:
            raise ValueError("热点凭据与租约不一致")
        return access

    def close(self):
        """媒体进程停止后才释放网络；失败时保留租约交给正式服务重试。"""
        self.finished.set()
        if self.worker:
            self.worker.join(timeout=70)
            if self.worker.is_alive():
                raise TimeoutError("网络续租任务尚未停止")
        self.command("release", "--source", "carplay-service")
        self.released = True

    def reconnect_upstream(self):
        """只能在媒体清理和租约释放成功后使用项目普通 Wi-Fi 入口。"""
        if not self.released or self.worker and self.worker.is_alive() or not self.recovery:
            raise RuntimeError("上游恢复必须先释放网络租约")
        # 释放租约可能已经恢复同一上游，不能再激活一次打断刚取得的连接。
        status = subprocess.run(["teslaboxctl", "wifi", "list"],
                                capture_output=True, text=True, timeout=20, check=False)
        if status.returncode == 0:
            try:
                state = json.loads(status.stdout)
                if (isinstance(state, dict) and state.get("ip_address")
                        and any(isinstance(item, dict) and item.get("uuid") == self.recovery
                                and item.get("active") is True for item in state.get("saved_networks", []))):
                    return
            except (ValueError, TypeError):
                pass
        result = subprocess.run(["teslaboxctl", "wifi", "connect", "--uuid", self.recovery],
                                capture_output=True, text=True, timeout=65, check=False)
        if result.returncode != 0:
            raise RuntimeError("已保存上游恢复失败")

    def healthy(self):
        """共享网络管理器报告实际 AP 状态，避免接收进程绕过项目读取系统状态。"""
        # 管理器在有界切换期间持有网络锁，不能把等待该锁误判成 AP 失效。
        if not self.operation_lock.acquire(blocking=False):
            return True
        try:
            result = self.command("status")
            return result.get("ap_healthy") is True and result.get("lease", {}).get("runtime_ap_uuid") == self.lease["runtime_ap_uuid"]
        finally:
            self.operation_lock.release()
