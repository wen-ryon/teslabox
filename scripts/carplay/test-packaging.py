"""只用临时合成文件回归组包、安装准备与迁移，不模拟实机验收。"""
import hashlib
import importlib.util
import io
import json
from pathlib import Path
import tarfile
import tempfile
import unittest


def load(name, filename):
    """隔离加载打包工具，不加载 CarPlay 运行服务。"""
    spec = importlib.util.spec_from_file_location(name, Path(__file__).with_name(filename))
    module = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(module)
    return module


prepare = load('prepare', 'prepare-install.py')
builder = load('builder', 'build-runtime-package.py')


class PackagingTests(unittest.TestCase):
    def fixture(self, root):
        """合成 ELF 只用于格式校验，不作为发行候选。"""
        records = []
        for role in ('main', 'native', 'node', 'service', 'unit', 'license'):
            data = bytearray(64) if role in ('main', 'native', 'node') else bytearray(b'fixture')
            if role in ('main', 'native', 'node'):
                data[:6], data[18:20] = b'\x7fELF\x02\x01', (183).to_bytes(2, 'little')
            (root / role).write_bytes(data)
            records.append(dict(path=role, source=role, role=role, sha256=hashlib.sha256(data).hexdigest(), mode=0o755))
        manifest = root / 'input.json'
        manifest.write_text(json.dumps(dict(schema=1, version='0.2.0-rc.1', approved_runtime=True, release_commit=None, files=records)))
        return manifest

    def test_build_and_prepare_preserve_exact_members(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            digest = builder.build(root, self.fixture(root), root / 'runtime.tar.gz')
            info = prepare.stage(root / 'runtime.tar.gz', digest, root / 'prepared')
            self.assertEqual(len(info['files']), 6)
            self.assertFalse(info['publishable'])
            self.assertEqual((root / 'prepared/main').read_bytes(), (root / 'main').read_bytes())

    def test_unapproved_runtime_is_rejected(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            path = self.fixture(root)
            info = json.loads(path.read_text())
            info['approved_runtime'] = False
            path.write_text(json.dumps(info))
            with self.assertRaises(ValueError):
                builder.build(root, path, root / 'out.tar.gz')
            self.assertFalse((root / 'out.tar.gz').exists())

    def test_source_drift_is_rejected_before_writing(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            manifest = self.fixture(root)
            (root / 'service').write_bytes(b'changed')
            with self.assertRaises(ValueError):
                builder.build(root, manifest, root / 'out.tar.gz')
            self.assertFalse((root / 'out.tar.gz').exists())

    def test_tar_links_and_path_escape_are_rejected(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            for index, name in enumerate(('../escape', 'symlink')):
                archive = root / f'bad-{index}.tar.gz'
                with tarfile.open(archive, 'w:gz') as package:
                    entry = tarfile.TarInfo(name)
                    entry.type = tarfile.SYMTYPE
                    entry.linkname = '../escape'
                    package.addfile(entry)
                with self.assertRaises(ValueError):
                    prepare.stage(archive, hashlib.sha256(archive.read_bytes()).hexdigest(), root / f'stage-{index}')

    def test_archive_digest_failure_does_not_create_stage(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            builder.build(root, self.fixture(root), root / 'runtime.tar.gz')
            with self.assertRaises(ValueError):
                prepare.stage(root / 'runtime.tar.gz', '0' * 64, root / 'prepared')
            self.assertFalse((root / 'prepared').exists())

    def test_settings_migration_and_exact_rollback(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            original = b'{"revision":7,"scale":120,"phone_id":"kept","network":"wifi"}'
            (root / 'old.json').write_bytes(original)
            value = prepare.migrate_settings(root / 'old.json', root / 'transaction')
            self.assertNotIn('scale', value)
            self.assertEqual(value['phone_id'], 'kept')
            self.assertEqual(value['dashboard_layout'], 'auto')
            self.assertFalse(value['auto_play'])
            self.assertEqual(prepare.rollback_settings(root / 'transaction'), original)
            self.assertEqual((root / 'old.json').read_bytes(), original)

    def test_private_material_names_are_rejected(self):
        for name in ('identity.json', 'tls/server.pem', 'reference-livi-private/state.json', 'hardware.json', 'mfi/active.json', 'service.json', 'MFI-OFFLINE/hidden.bin', 'TLS/SECRET.PFX'):
            with self.assertRaises(ValueError):
                prepare.member_name(name)

    def inner_fixture(self, root):
        """内层夹具移除主 ELF，不伪造占位主程序。"""
        path = self.fixture(root)
        value = json.loads(path.read_text())
        value['files'] = [item for item in value['files'] if item['role'] != 'main']
        value['release_commit'] = 'a' * 40
        fixed = {'native': 'teslabox-carplay', 'node': 'runtime/bin/node', 'service': 'python/carplay-service.py', 'unit': 'systemd/teslabox-carplay-experiment.service', 'license': 'licenses/LICENSE'}
        for item in value['files']:
            item['path'] = fixed[item['role']]
            item['mode'] = 0o755 if item['role'] in {'native', 'node'} else 0o644
        value['files'].append(dict(path='python/carplay-runtime-ready.py', source='service', role='python', sha256=hashlib.sha256((root / 'service').read_bytes()).hexdigest(), mode=0o644))
        path.write_text(json.dumps(value))
        return path

    def test_inner_build_and_prepare_without_main(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            digest = builder.build(root, self.inner_fixture(root), root / 'inner.tar.gz', 'inner')
            info = prepare.stage(root / 'inner.tar.gz', digest, root / 'prepared')
            self.assertEqual(info['package_kind'], 'inner')
            self.assertEqual(len(info['files']), 6)
            self.assertFalse((root / 'prepared/main').exists())

    def test_inner_rejects_main_role(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            with self.assertRaises(ValueError):
                builder.build(root, self.fixture(root), root / 'out.tar.gz', 'inner')
            self.assertFalse((root / 'out.tar.gz').exists())

    def test_inner_requires_full_release_commit(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            for commit in (None, '', 'a' * 12, 'g' * 40):
                path = self.inner_fixture(root)
                value = json.loads(path.read_text())
                value['release_commit'] = commit
                path.write_text(json.dumps(value))
                with self.assertRaises(ValueError):
                    builder.build(root, path, root / 'out.tar.gz', 'inner')
                self.assertFalse((root / 'out.tar.gz').exists())

    def test_inner_rejects_outer_artifact_names(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            for filename in ('teslabox-linux-arm64', 'latest.json', 'teslabox-bootstrap.tar.gz', 'teslabox-update-helper', 'manifest-0.2.0-rc.1.json'):
                path = self.inner_fixture(root)
                value = json.loads(path.read_text())
                value['files'][0]['path'] = filename
                path.write_text(json.dumps(value))
                with self.assertRaises(ValueError):
                    builder.build(root, path, root / 'out.tar.gz', 'inner')
                self.assertFalse((root / 'out.tar.gz').exists())

    def test_same_inner_input_is_byte_reproducible(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            path = self.inner_fixture(root)
            first = builder.build(root, path, root / 'first.tar.gz', 'inner')
            second = builder.build(root, path, root / 'second.tar.gz', 'inner')
            self.assertEqual(first, second)
            self.assertEqual((root / 'first.tar.gz').read_bytes(), (root / 'second.tar.gz').read_bytes())

    def test_inner_rejects_wrong_fixed_layout_or_mode(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            for field, replacement in (('path', 'bin/native'), ('mode', 0o644)):
                path = self.inner_fixture(root)
                value = json.loads(path.read_text())
                value['files'][0][field] = replacement
                path.write_text(json.dumps(value))
                with self.assertRaises(ValueError):
                    builder.build(root, path, root / 'out.tar.gz', 'inner')
                self.assertFalse((root / 'out.tar.gz').exists())

    def test_inner_requires_readiness_dependency(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            path = self.inner_fixture(root)
            value = json.loads(path.read_text())
            value['files'] = [item for item in value['files'] if item['path'] != 'python/carplay-runtime-ready.py']
            path.write_text(json.dumps(value))
            with self.assertRaises(ValueError):
                builder.build(root, path, root / 'out.tar.gz', 'inner')
            self.assertFalse((root / 'out.tar.gz').exists())


if __name__ == '__main__':
    unittest.main(verbosity=2)
