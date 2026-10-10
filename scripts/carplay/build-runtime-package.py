"""从显式审核清单组装完整运行包，不扫描脏工作区，不构建业务程序或签名。"""
import hashlib
import gzip
import io
import json
from pathlib import Path
import re
import tarfile
import importlib.util

spec = importlib.util.spec_from_file_location('prepare', Path(__file__).with_name('prepare-install.py'))
prepare = importlib.util.module_from_spec(spec)
spec.loader.exec_module(prepare)


def build(source_root, input_manifest, output, package_kind='complete'):
    """先验证所有输入及角色，再生成只含白名单成员的归档。"""
    root, output = Path(source_root).resolve(), Path(output)
    info = json.loads(Path(input_manifest).read_text(encoding='utf-8'))
    if info.get('schema') != 1 or info.get('approved_runtime') is not True or not re.fullmatch(r'\d+\.\d+\.\d+-rc\.[1-9]\d*', info.get('version', '')):
        raise ValueError('需要显式审核的 RC 运行时输入清单')
    files = info.get('files')
    if package_kind not in {'complete', 'inner'}:
        raise ValueError('未知包模式')
    if package_kind == 'inner' and not re.fullmatch(r'[0-9a-f]{40}', info.get('release_commit') or ''):
        raise ValueError('内层包必须绑定完整发行提交')
    roles = {'main', 'native', 'node', 'service', 'unit', 'license', 'python', 'node_module', 'asset', 'reference_source', 'recovery'}
    required = {'native', 'node', 'service', 'unit', 'license'}
    if package_kind == 'complete':
        required.add('main')
    if not isinstance(files, list) or not 1 <= len(files) <= 4095 or any(not isinstance(item, dict) for item in files) or not required <= {item.get('role') for item in files}:
        raise ValueError('输入角色不完整')
    if package_kind == 'inner' and any(item.get('role') == 'main' for item in files):
        raise ValueError('内层包禁止包含主程序')
    if package_kind == 'inner':
        fixed = {'native': 'teslabox-carplay', 'node': 'runtime/bin/node', 'service': 'python/carplay-service.py', 'unit': 'systemd/teslabox-carplay-experiment.service'}
        for role, path in fixed.items():
            matching = [item for item in files if item.get('role') == role]
            if len(matching) != 1 or matching[0].get('path') != path or matching[0].get('mode') != (0o755 if role in {'native', 'node'} else 0o644):
                raise ValueError('内层核心角色路径、数量或权限不符合固定契约')
        readiness = [item for item in files if item.get('path') == 'python/carplay-runtime-ready.py']
        if len(readiness) != 1 or readiness[0].get('role') != 'python' or readiness[0].get('mode') != 0o644:
            raise ValueError('内层缺少固定启动就绪辅助程序')
    if output.exists() or output.is_symlink() or output.parent.resolve() != output.parent.absolute():
        raise ValueError('输出已存在或祖先路径包含链接')
    payload, members = {}, []
    for item in files:
        name = prepare.member_name(item['path'])
        source_name = prepare.member_name(item['source'])
        if package_kind == 'inner':
            for candidate in (name, source_name):
                basename = Path(candidate).name.lower()
                if basename in {'teslabox', 'teslabox-linux-arm64', 'latest.json', 'history.json'} or basename.startswith(('teslabox-update-helper', 'teslabox-bootstrap', 'manifest-')):
                    raise ValueError('内层包包含外层发行物或循环依赖')
        source = root / source_name
        if name == 'PACKAGE.json' or name in payload or item['role'] not in roles or item['mode'] not in (0o600, 0o644, 0o755):
            raise ValueError('重复成员、未知角色或不合法权限')
        if not source.is_file() or source.is_symlink() or source.resolve() != source or source.stat().st_size > prepare.MAX_BYTES:
            raise ValueError('输入必须是根目录内的普通文件')
        data = source.read_bytes()
        if hashlib.sha256(data).hexdigest() != item['sha256']:
            raise ValueError('输入摘要漂移')
        if item['role'] in {'main', 'native', 'node'} and (len(data) < 64 or data[:6] != b'\x7fELF\x02\x01' or int.from_bytes(data[18:20], 'little') != 183):
            raise ValueError('程序不是 ARM64 ELF')
        payload[name] = data
        members.append(dict(path=name, size=len(data), sha256=item['sha256'], mode=item['mode'], role=item['role']))
    if sum(map(len, payload.values())) > prepare.MAX_BYTES:
        raise ValueError('运行包超过预算')
    package_info = dict(schema=1, package_kind=package_kind, version=info['version'], files=members, publishable=False, signed=False, release_commit=info.get('release_commit'), requires_signed_install_controller=True)
    payload['PACKAGE.json'] = (json.dumps(package_info, ensure_ascii=False, indent=2) + '\n').encode('utf-8')
    with output.open('xb') as target:
        # gzip 文件名和时间也固定，避免同一输入仅因输出路径或时钟不同而改变摘要。
        with gzip.GzipFile(filename='', mode='wb', fileobj=target, mtime=0) as compressed:
            with tarfile.open(fileobj=compressed, mode='w') as archive:
                for name, data in sorted(payload.items()):
                    entry = tarfile.TarInfo(name)
                    entry.size, entry.mtime, entry.uid, entry.gid = len(data), 0, 0, 0
                    entry.mode = next((item['mode'] for item in members if item['path'] == name), 0o644)
                    archive.addfile(entry, io.BytesIO(data))
    return hashlib.sha256(output.read_bytes()).hexdigest()


if __name__ == '__main__':
    import argparse
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--source-root', type=Path, required=True)
    parser.add_argument('--input-manifest', type=Path, required=True)
    parser.add_argument('--output', type=Path, required=True)
    parser.add_argument('--package-kind', choices=('complete', 'inner'), default='complete')
    args = parser.parse_args()
    print(build(args.source_root, args.input_manifest, args.output, args.package_kind))
