"""修正已关联手机的短暂错误源地址回包路径，控制端继续拒绝错误来源并验签。"""
import ipaddress
import json
import os
from pathlib import Path
import re
import socket
import stat
import struct
import subprocess
import tempfile
import threading
import time

PROTOCOL = '186'
METRIC = '42427'

def transitional_address(value):
    """只处理实测的蜂窝私网范围，不能把任意来源变成热点手机。"""
    address = ipaddress.IPv4Address(value)
    return address in ipaddress.IPv4Network('10.0.0.0/8') or address in ipaddress.IPv4Network('100.64.0.0/10')

def hotspot_address(value):
    """与现有控制端保持相同的私网边界，不能把回环或保留地址当作手机。"""
    address = ipaddress.IPv4Address(value)
    return any(address in ipaddress.IPv4Network(prefix) for prefix in ('10.0.0.0/8', '172.16.0.0/12', '192.168.0.0/16'))

def control_syn(packet, host, network):
    """仅解析 IPv4 控制 SYN 的包头，返回待核对的源地址和二层地址。"""
    if len(packet) < 54 or packet[12:14] != b'\x08\x00':
        return None
    ip = packet[14:]
    length = (ip[0] & 15) * 4
    if ip[0] >> 4 != 4 or length < 20 or len(ip) < length + 20 or ip[9] != 6:
        return None
    if struct.unpack('!H', ip[6:8])[0] & 0x3fff or socket.inet_ntoa(ip[16:20]) != host:
        return None
    tcp = ip[length:]
    total = struct.unpack('!H', ip[2:4])[0]
    tcp_length = (tcp[12] >> 4) * 4
    if tcp_length < 20 or total < length + tcp_length or total > len(ip):
        return None
    if struct.unpack('!H', tcp[2:4])[0] != 7000 or tcp[13] & 0x17 != 2:
        return None
    source = ipaddress.IPv4Address(ip[12:16])
    if source in network or not transitional_address(source):
        return None
    mac = ':'.join(f'{value:02x}' for value in packet[6:12])
    return str(source), mac

def command(*arguments):
    """命令参数全部来自已校验的数值地址，不使用 shell 或覆盖已有路由。"""
    return subprocess.check_output(arguments, text=True, timeout=1)

class PeerReturnPath:
    """只在当前接收端交接阶段短时观察；结束、认证成功或服务退出都回收自己创建的路由。"""

    def __init__(self, interface, peer, host, addresses, runtime, receiver, stopped):
        if not re.fullmatch(r'[A-Za-z0-9][A-Za-z0-9_.-]{0,14}', interface):
            raise ValueError('控制回包接口不合法')
        source = ipaddress.IPv4Address(host)
        target = ipaddress.IPv4Address(peer)
        networks = [ipaddress.ip_interface(value) for value in addresses]
        network = next((value.network for value in networks if value.ip == source), None)
        if not hotspot_address(source) or not hotspot_address(target) or source == target or network is None or target not in network:
            raise ValueError('控制回包必须属于已取得的热点地址')
        self.interface, self.peer, self.host, self.network = interface, str(target), str(source), network
        self.receiver, self.stopped = receiver, stopped
        self.path = Path(runtime) / 'peer-return-routes.json'
        self.routes, self.seen = {}, set()
        self.cancelled = threading.Event()
        self.worker = None
        self.clean = True
        self.retried = False
        self.rejected_observation = None
        self._recover()

    def _save(self):
        if not self.routes:
            self.path.unlink(missing_ok=True)
            return
        data = dict(version=1, interface=self.interface, peer=self.peer, sources=list(self.routes))
        fd, temporary = tempfile.mkstemp(prefix='.peer-return-', dir=self.path.parent)
        try:
            with os.fdopen(fd, 'w', encoding='utf-8') as output:
                json.dump(data, output)
                output.flush()
                os.fsync(output.fileno())
            os.replace(temporary, self.path)
        finally:
            Path(temporary).unlink(missing_ok=True)

    @staticmethod
    def _remove(source, peer, interface):
        # 私有记录只是所有权依据，实际路由必须仍精确匹配，不能误删后来接管的路由。
        # iproute2 默认把 186 显示成 bgp，数字输出才可跨版本核对精确所有权。
        rows = json.loads(command('ip', '-N', '-j', 'route', 'show', 'exact', source + '/32'))
        owned = any(row.get('gateway') == peer and row.get('dev') == interface
                    and str(row.get('protocol')) == PROTOCOL and str(row.get('metric')) == METRIC for row in rows)
        if owned:
            command('ip', 'route', 'del', source + '/32', 'via', peer, 'dev', interface,
                    'proto', PROTOCOL, 'metric', METRIC)

    def _recover(self):
        try:
            fd = os.open(self.path, os.O_RDONLY | getattr(os, 'O_NOFOLLOW', 0))
        except FileNotFoundError:
            return
        with os.fdopen(fd, 'r', encoding='utf-8') as saved:
            info = os.fstat(saved.fileno())
            if not stat.S_ISREG(info.st_mode) or info.st_size > 2048 or info.st_mode & 0o077 or info.st_nlink != 1 or info.st_uid != os.geteuid():
                raise PermissionError('控制回包所有权记录不是私有普通文件')
            data = json.load(saved)
        if not isinstance(data, dict) or set(data) != {'version', 'interface', 'peer', 'sources'} or type(data['version']) is not int or data['version'] != 1:
            raise ValueError('控制回包所有权记录不合法')
        interface, peer, sources = data['interface'], data['peer'], data['sources']
        if not isinstance(interface, str) or not re.fullmatch(r'[A-Za-z0-9][A-Za-z0-9_.-]{0,14}', interface):
            raise ValueError('旧控制回包接口不合法')
        if not isinstance(peer, str) or not hotspot_address(peer) or not isinstance(sources, list) or len(sources) > 2:
            raise ValueError('旧控制回包地址不合法')
        for source in sources:
            if not isinstance(source, str) or not transitional_address(source):
                raise ValueError('旧控制回包源地址不合法')
        if len(set(sources)) != len(sources):
            raise ValueError('旧控制回包源地址重复')
        for source in sources:
            self._remove(source, peer, interface)
        self.path.unlink()

    def start(self):
        """旁路观察不阻塞控制接受循环，不创建第二套热点、认证器或默认路由。"""
        self.worker = threading.Thread(target=self._watch, name='carplay-peer-return', daemon=True)
        self.worker.start()
        return self

    def _associated(self, mac):
        neighbors = json.loads(command('ip', '-j', 'neigh', 'show', 'dev', self.interface))
        matched = any(row.get('dst') == self.peer and row.get('lladdr', '').lower() == mac
                      and not set(row.get('state', [])) & {'FAILED', 'INCOMPLETE'} for row in neighbors)
        return matched and ('Station ' + mac + ' ') in command('iw', 'dev', self.interface, 'station', 'dump')

    def _install(self, source, mac, now):
        if source in self.seen or len(self.seen) >= 2 or not self._associated(mac):
            return
        if json.loads(command('ip', '-j', 'route', 'show', 'exact', source + '/32')):
            return
        local = json.loads(command('ip', '-j', '-4', 'address', 'show'))
        if any(item.get('local') == source for row in local for item in row.get('addr_info', [])):
            return
        self.seen.add(source)
        self.routes[source] = now + 15
        # 先记录预期所有权，异常退出后的下一次启动仍能有界清理。
        self._save()
        command('ip', 'route', 'add', source + '/32', 'via', self.peer, 'dev', self.interface,
                'proto', PROTOCOL, 'metric', METRIC)
        print('CARPLAY_PEER_RETURN temporary_route=true', flush=True)

    def _cleanup(self, now, all_routes=False):
        for source, deadline in list(self.routes.items()):
            if all_routes or now >= deadline:
                self._remove(source, self.peer, self.interface)
                del self.routes[source]
                self._save()

    def _watch(self):
        deadline = time.monotonic() + 45
        try:
            with socket.socket(socket.AF_PACKET, socket.SOCK_RAW, socket.htons(0x0800)) as capture:
                capture.bind((self.interface, 0))
                capture.settimeout(0.3)
                while not self.cancelled.is_set() and not self.stopped.is_set() and time.monotonic() < deadline:
                    receiver = self.receiver()
                    if receiver is not None and receiver.latest.get('session_verified') is True:
                        break
                    self._cleanup(time.monotonic())
                    try:
                        packet, address = capture.recvfrom(2048)
                    except socket.timeout:
                        continue
                    if receiver is None or receiver.handoff_at is None or address[2] == socket.PACKET_OUTGOING:
                        continue
                    value = control_syn(packet, self.host, self.network)
                    if value:
                        self._install(*value, time.monotonic())
        except Exception:
            # 原因固定分类，不把报文、手机地址或命令输出带入日志；原有超时恢复仍生效。
            print('CARPLAY_PEER_RETURN observer_failed=true', flush=True)
        finally:
            try:
                self._cleanup(time.monotonic(), all_routes=True)
            except Exception:
                self.clean = False
                print('CARPLAY_PEER_RETURN cleanup_failed=true', flush=True)

    def close(self):
        """服务回收先等短时观察退出，失败时保留私有记录供下次精确恢复。"""
        self.cancelled.set()
        if self.worker is not None:
            self.worker.join(timeout=10)
        return self.clean and (self.worker is None or not self.worker.is_alive())

    def consume_rejected_retry(self, receiver, now):
        """仅已纠正回包且持续拒绝来源时提前交接一次，不能打断认证进度或在线视频。"""
        if (self.retried or not self.clean or not self.routes or self.cancelled.is_set() or self.stopped.is_set()
                or receiver is None or receiver.handoff_at is None
                or receiver.latest.get('stage') != 'control_peer_rejected'
                or receiver.latest.get('session_verified') is True
                or receiver.latest.get('media', {}).get('source_connected') is True
                or getattr(receiver, 'control_connected_at', None) is not None):
            self.rejected_observation = None
            return False
        # 连续两次观察避免拒绝日志与新的正确连接同时到达时误抢认证。
        if self.rejected_observation is None or self.rejected_observation[0] is not receiver:
            self.rejected_observation = (receiver, now)
            return False
        if now - self.rejected_observation[1] < 1:
            return False
        self.retried = True
        return True
