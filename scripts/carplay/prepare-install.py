"""离线安装准备：严格解包、设置迁移和回退快照；不激活服务，不冒充签名 OTA。"""
import hashlib
import json
import os
from pathlib import Path, PurePosixPath
import re
import tarfile

MAX_BYTES = 512 * 1024 * 1024


def member_name(value):
    """清单不能通过越界或平台特殊路径改变写入目标。"""
    name = PurePosixPath(value)
    if not value or '\\' in value or ':' in value or name.is_absolute() or '..' in name.parts or name.as_posix() != value:
        raise ValueError('非法成员路径')
    if name.suffix.lower() in {'.pem', '.key', '.pk8', '.p7b', '.p12', '.pfx'} or name.name.lower() in {'identity.json', 'hardware.json', 'carplay-service.json', 'service.json', 'active.json', 'browser-token'} or {'reference-livi-private', 'mfi-offline'}.intersection(part.lower() for part in name.parts):
        raise ValueError('程序包不允许设备私有材料')
    return value


def stage(archive, expected_sha256, destination):
    """摘要应来自上游已经验签的包清单；本工具自身不提供签名或安装授权。"""
    archive, destination = Path(archive), Path(destination)
    if not re.fullmatch('[a-f0-9]{64}', expected_sha256) or archive.is_symlink() or not archive.is_file() or archive.stat().st_size > MAX_BYTES:
        raise ValueError('归档或摘要无效')
    if hashlib.sha256(archive.read_bytes()).hexdigest() != expected_sha256:
        raise ValueError('归档摘要不匹配')
    if destination.exists() or destination.is_symlink() or destination.parent.resolve() != destination.parent.absolute():
        raise ValueError('准备目录已存在或祖先路径包含链接')
    with tarfile.open(archive, 'r:gz') as package:
        entries = package.getmembers()
        names = [member_name(item.name) for item in entries]
        if len(entries) > 4096 or len(names) != len(set(names)) or any(not item.isfile() or item.size < 0 for item in entries) or sum(item.size for item in entries) > MAX_BYTES:
            raise ValueError('归档包含重复、链接、非普通成员或超出预算')
        if 'PACKAGE.json' not in names or entries[names.index('PACKAGE.json')].size > 512000:
            raise ValueError('缺少有效清单')
        info = json.load(package.extractfile('PACKAGE.json'))
        if info.get('schema') != 1 or not re.fullmatch(r'\d+\.\d+\.\d+-rc\.[1-9]\d*', info.get('version', '')):
            raise ValueError('无效 RC 清单')
        records = info.get('files')
        if not isinstance(records, list) or len(records) > 4095:
            raise ValueError('无效文件清单')
        expected = {member_name(item['path']): item for item in records}
        if len(expected) != len(records) or set(names) != set(expected) | {'PACKAGE.json'} or 'PACKAGE.json' in expected:
            raise ValueError('成员与清单不一致')
        contents = {}
        for name, item in expected.items():
            data = package.extractfile(name).read()
            if len(data) != item['size'] or hashlib.sha256(data).hexdigest() != item['sha256'] or item.get('mode') not in (0o600, 0o644, 0o755):
                raise ValueError('成员摘要、长度或权限不符')
            contents[name] = data
        # 全部校验成功后才创建目录，异常不会覆盖现有安装。
        destination.mkdir(mode=0o700)
        for name, data in contents.items():
            path = destination / name
            path.parent.mkdir(mode=0o700, parents=True, exist_ok=True)
            with path.open('xb') as output:
                output.write(data)
            os.chmod(path, expected[name]['mode'])
        return info


def migrate_settings(source, transaction):
    """仅准备设置副本与精确回退快照，不读取身份、配对或证书目录。"""
    source, transaction = Path(source), Path(transaction)
    if source.is_symlink() or not source.is_file() or source.stat().st_size > 16384 or transaction.exists() or transaction.is_symlink():
        raise ValueError('设置来源或事务目录无效')
    if transaction.parent.resolve() != transaction.parent.absolute():
        raise ValueError('事务祖先路径包含链接')
    original = source.read_bytes()
    value = json.loads(original)
    if not isinstance(value, dict):
        raise ValueError('设置必须为对象')
    value.pop('scale', None)
    value.setdefault('dashboard_layout', 'auto')
    value.setdefault('auto_play', False)
    if value['dashboard_layout'] not in ('auto', 'vertical') or type(value['auto_play']) is not bool:
        raise ValueError('设置字段不合法')
    transaction.mkdir(mode=0o700)
    for name, data in (('settings.previous.json', original), ('settings.next.json', (json.dumps(value, ensure_ascii=False, indent=2) + '\n').encode('utf-8'))):
        path = transaction / name
        with path.open('xb') as output:
            output.write(data)
        os.chmod(path, 0o600)
    return value


def rollback_settings(transaction):
    """返回精确旧设置供事务控制器恢复，本工具不覆盖系统文件。"""
    previous = Path(transaction) / 'settings.previous.json'
    if previous.is_symlink() or not previous.is_file() or previous.stat().st_size > 16384:
        raise ValueError('回退快照无效')
    return previous.read_bytes()
