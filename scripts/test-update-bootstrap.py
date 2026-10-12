"""只操作临时根目录，systemctl 和助手执行均为测试替身。"""

import importlib.util
import json
from pathlib import Path
import tempfile
import unittest
from unittest.mock import patch

SPEC = importlib.util.spec_from_file_location("bootstrap", Path(__file__).parents[1] / "deploy/scripts/bootstrap-update-rollback.py")
bootstrap = importlib.util.module_from_spec(SPEC)
SPEC.loader.exec_module(bootstrap)


class BootstrapTests(unittest.TestCase):
    def fixture(self):
        temporary = tempfile.TemporaryDirectory()
        self.addCleanup(temporary.cleanup)
        root = Path(temporary.name)
        helper = root / bootstrap.HELPER
        helper.parent.mkdir(parents=True)
        helper.write_bytes(b"old helper")
        helper.chmod(0o755)
        source = root / "candidate-helper"
        source.write_bytes(b"new helper")
        source.chmod(0o700)
        return root, helper, source

    def run_ok(self, command):
        if "ActiveState" in " ".join(command):
            return "inactive"
        if "--protocol" in command:
            return bootstrap.PROTOCOL
        if "ExecStartPre" in " ".join(command):
            return "/usr/local/libexec/teslabox-update-helper --recover"
        return ""

    def install(self, root, source, run=None):
        return bootstrap.install(root, source, bootstrap.digest(b"new helper"), bootstrap.digest(b"old helper"), run or self.run_ok)

    def test_success_preserves_main_program_and_state(self):
        root, helper, source = self.fixture()
        program = helper.parent / "teslabox"
        program.write_bytes(b"old program and no migration")
        backup = self.install(root, source)
        self.assertEqual(helper.read_bytes(), b"new helper")
        self.assertEqual(program.read_bytes(), b"old program and no migration")
        self.assertEqual((backup / "0.before").read_bytes(), b"old helper")
        self.assertEqual((root / bootstrap.MAIN_GUARD).read_bytes(), bootstrap.MAIN_CONTENT)
        manifest = json.loads((backup / "manifest.json").read_text(encoding="utf-8"))
        self.assertEqual(manifest["files"][0]["mode"], 0o755)
        self.assertTrue(manifest["files"][0]["exists"])
        self.assertEqual(manifest["files"][0]["sha256"], bootstrap.digest((backup / "0.before").read_bytes()))
        self.assertFalse(manifest["files"][1]["exists"])
        self.assertIsNone(manifest["files"][1]["mode"])
        self.assertFalse(manifest["files"][2]["exists"])

    def test_failed_reload_restores_existing_units_and_helper(self):
        root, helper, source = self.fixture()
        guard = root / bootstrap.MAIN_GUARD
        guard.parent.mkdir(parents=True)
        guard.write_bytes(b"previous guard")
        guard.chmod(0o644)
        calls = 0
        def run(command):
            nonlocal calls
            if "daemon-reload" in command:
                calls += 1
                if calls == 1:
                    raise RuntimeError("injected reload failure")
            return self.run_ok(command)
        with self.assertRaisesRegex(RuntimeError, "reload failure"):
            self.install(root, source, run)
        self.assertEqual(helper.read_bytes(), b"old helper")
        self.assertEqual(guard.read_bytes(), b"previous guard")
        self.assertFalse((root / bootstrap.UPDATE_GUARD).exists())

    def test_refuses_wrong_digest_or_running_service(self):
        for reason in ("digest", "running"):
            with self.subTest(reason=reason):
                root, helper, source = self.fixture()
                if reason == "digest":
                    source.write_bytes(b"tampered")
                def run(command):
                    if "ActiveState" in " ".join(command):
                        return "active"
                    return self.run_ok(command)
                with self.assertRaises(RuntimeError):
                    self.install(root, source, run)
                self.assertEqual(helper.read_bytes(), b"old helper")
                self.assertFalse((root / bootstrap.MAIN_GUARD).exists())

    def test_power_loss_at_each_write_is_boot_safe(self):
        # BaseException 模拟断电，绕过进程内异常回滚；只验证文件组合，不冒充真实掉电实机。
        for stop_after in (bootstrap.HELPER, bootstrap.UPDATE_GUARD, bootstrap.MAIN_GUARD):
            with self.subTest(stop_after=stop_after):
                root, helper, source = self.fixture()
                original = bootstrap.durable_write
                def write(path, data, mode):
                    original(path, data, mode)
                    if path == root / stop_after:
                        raise KeyboardInterrupt("power loss")
                with patch.object(bootstrap, "durable_write", write):
                    with self.assertRaises(KeyboardInterrupt):
                        self.install(root, source)
                if (root / bootstrap.MAIN_GUARD).exists():
                    self.assertEqual(helper.read_bytes(), b"new helper")
                else:
                    self.assertIn(helper.read_bytes(), (b"old helper", b"new helper"))

    def test_preserves_new_helper_if_guard_rollback_fails(self):
        root, helper, source = self.fixture()
        guard = root / bootstrap.MAIN_GUARD
        guard.parent.mkdir(parents=True)
        guard.write_bytes(b"previous guard")
        guard.chmod(0o644)
        original = bootstrap.durable_write
        def write(path, data, mode):
            if path == guard and data == b"previous guard":
                raise RuntimeError("restore failed")
            original(path, data, mode)
        def run(command):
            if "daemon-reload" in command:
                raise RuntimeError("reload failed")
            return self.run_ok(command)
        with patch.object(bootstrap, "durable_write", write):
            with self.assertRaisesRegex(RuntimeError, "restore failed"):
                self.install(root, source, run)
        self.assertEqual(helper.read_bytes(), b"new helper")
        self.assertEqual(guard.read_bytes(), bootstrap.MAIN_CONTENT)

    def test_rejects_link_and_unfinished_transaction(self):
        for kind in ("symlink", "hardlink", "pending"):
            with self.subTest(kind=kind):
                root, helper, source = self.fixture()
                if kind == "symlink":
                    alias = source.with_suffix(".link")
                    alias.symlink_to(source)
                    source = alias
                elif kind == "hardlink":
                    import os
                    os.link(source, source.with_suffix(".link"))
                else:
                    journal = root / "var/lib/teslabox-system/update-backups/active/transaction.json"
                    journal.parent.mkdir(parents=True)
                    journal.write_text('{"version":1,"phase":"activating"}', encoding="utf-8")
                    journal.chmod(0o600)
                with self.assertRaises(RuntimeError):
                    self.install(root, source)
                self.assertEqual(helper.read_bytes(), b"old helper")

    def test_backup_directory_sync_failure_never_replaces_helper(self):
        for suffix in ("var", "var/lib", "var/lib/teslabox-system", "var/lib/teslabox-system/update-bootstrap-backups"):
            with self.subTest(suffix=suffix):
                root, helper, source = self.fixture()
                original = bootstrap.sync_dir
                fail_at = root / suffix
                def sync(path):
                    if path == fail_at:
                        raise OSError("injected directory fsync failure")
                    original(path)
                with patch.object(bootstrap, "sync_dir", sync):
                    with self.assertRaises(OSError):
                        self.install(root, source)
                self.assertEqual(helper.read_bytes(), b"old helper")
                self.assertFalse((root / bootstrap.MAIN_GUARD).exists())
                # 重试不能因为目录已经存在就跳过上一次失败的同步。
                seen = []
                def retry_sync(path):
                    seen.append(path)
                    original(path)
                with patch.object(bootstrap, "sync_dir", retry_sync):
                    self.install(root, source)
                self.assertIn(fail_at, seen)

    def test_dropin_sync_failure_rolls_back_with_durable_modes(self):
        root, helper, source = self.fixture()
        guard = root / bootstrap.MAIN_GUARD
        guard.parent.mkdir(parents=True)
        guard.write_bytes(b"original guard")
        guard.chmod(0o640)
        original = bootstrap.sync_dir
        failed = False
        def sync(path):
            nonlocal failed
            if path == guard.parent and not failed:
                failed = True
                raise OSError("dropin directory sync failed")
            original(path)
        with patch.object(bootstrap, "sync_dir", sync):
            with self.assertRaises(OSError):
                self.install(root, source)
        self.assertEqual(helper.read_bytes(), b"old helper")
        self.assertEqual(guard.read_bytes(), b"original guard")
        backups = list((root / "var/lib/teslabox-system/update-bootstrap-backups").glob("bootstrap-*"))
        manifest = json.loads((backups[0] / "manifest.json").read_text(encoding="utf-8"))
        self.assertEqual(manifest["files"][2]["mode"], 0o640)
        self.assertEqual(guard.stat().st_mode & 0o777, 0o640)


if __name__ == "__main__":
    unittest.main()
