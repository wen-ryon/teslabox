"""覆盖旧更新助手回退和掉电恢复，不接触正在运行的服务或真实身份。"""
import importlib.util
import json
import os
from pathlib import Path
import tempfile
import unittest

spec = importlib.util.spec_from_file_location("recovery", Path(__file__).with_name("runtime_recovery.py"))
recovery = importlib.util.module_from_spec(spec)
spec.loader.exec_module(recovery)


class RecoveryTests(unittest.TestCase):
    def setUp(self):
        self.temporary = tempfile.TemporaryDirectory()
        self.addCleanup(self.temporary.cleanup)
        self.root = Path(self.temporary.name)
        self.commands = []
        self.main = self.root / "usr/local/bin/teslabox"
        self.main.parent.mkdir(parents=True)
        self.main.write_bytes(b"new-main")
        self.main_hash = recovery.digest_file(self.main)
        for key, relative in recovery.TARGETS.items():
            path = self.root / relative
            path.parent.mkdir(parents=True, mode=0o700, exist_ok=True)
            path.write_bytes(("old-" + key).encode())
            path.chmod(0o644 if key in ("unit", "main_override") else 0o600)
        self.private = self.root / "var/lib/teslabox-system/carplay/phones/private-state"
        self.private.parent.mkdir(mode=0o700)
        self.private.write_bytes(b"this-device-only")

    def command(self, *arguments):
        self.commands.append(arguments)

    def activate(self):
        journal = recovery.prepare(self.root, self.main_hash, "a" * 64, True, True)
        for relative in recovery.TARGETS.values():
            recovery.atomic_write(self.root / relative, b"new-data")
        journal["phase"] = "activated"
        recovery.save(self.root, journal)
        return journal

    def assert_restored(self):
        for key, relative in recovery.TARGETS.items():
            self.assertEqual((self.root / relative).read_bytes(), ("old-" + key).encode())
        self.assertEqual(self.private.read_bytes(), b"this-device-only")

    def test_old_helper_elf_rollback_restores_components(self):
        self.activate()
        self.main.write_bytes(b"old-main-restored-by-0.1.31-helper")
        self.assertTrue(recovery.recover(self.root, self.command))
        self.assert_restored()
        self.assertIn(("--no-block", "start", recovery.SERVICE), self.commands)
        self.assertFalse(recovery.recover(self.root, self.command))

    def test_successful_main_restart_keeps_runtime(self):
        self.activate()
        self.assertFalse(recovery.recover(self.root, self.command))
        self.assertEqual(self.commands, [])
        self.assertEqual((self.root / recovery.TARGETS["settings"]).read_bytes(), b"new-data")

    def test_next_ota_keeps_current_runtime_until_new_transaction(self):
        self.activate()
        backup = self.root / "var/lib/teslabox-system/update-backups/teslabox.previous.backup"
        backup.parent.mkdir(mode=0o700)
        backup.write_bytes(self.main.read_bytes())
        self.main.write_bytes(b"next-release-installed-by-old-helper")
        self.assertFalse(recovery.recover(self.root, self.command))
        self.assertEqual(self.commands, [])
        self.assertEqual((self.root / recovery.TARGETS["settings"]).read_bytes(), b"new-data")

    def test_power_loss_at_each_config_write_recovers_exact_old_bytes(self):
        for count in range(len(recovery.TARGETS) + 1):
            with self.subTest(writes=count):
                recovery.prepare(self.root, self.main_hash, "a" * 64, False, False)
                for relative in list(recovery.TARGETS.values())[:count]:
                    recovery.atomic_write(self.root / relative, b"half-upgrade")
                self.assertTrue(recovery.recover(self.root, self.command))
                self.assert_restored()

    def test_interrupted_restore_is_reentrant(self):
        self.activate()
        self.main.write_bytes(b"old-main")

        def failing(*args):
            if args == ("daemon-reload",):
                raise RuntimeError("simulated power loss")

        with self.assertRaises(RuntimeError):
            recovery.recover(self.root, failing)
        self.assertEqual(recovery.load(self.root)["phase"], "activated")
        self.assertTrue(recovery.recover(self.root, self.command))
        self.assert_restored()

    def test_missing_previous_config_is_removed_without_touching_identity(self):
        for relative in recovery.TARGETS.values():
            (self.root / relative).unlink()
        recovery.prepare(self.root, self.main_hash, "a" * 64, False, False)
        for relative in recovery.TARGETS.values():
            recovery.atomic_write(self.root / relative, b"first-install")
        recovery.recover(self.root, self.command)
        self.assertTrue(all(not (self.root / relative).exists() for relative in recovery.TARGETS.values()))
        self.assertEqual(self.private.read_bytes(), b"this-device-only")

    def test_damaged_or_unsafe_journal_cannot_redirect_restore(self):
        journal = self.activate()
        journal["previous"]["arbitrary-target"] = None
        with self.assertRaises(ValueError):
            recovery.save(self.root, journal)
        path = self.root / recovery.JOURNAL
        path.write_text(json.dumps({"schema": 1}), encoding="utf-8")
        with self.assertRaises(ValueError):
            recovery.recover(self.root, self.command)
        self.assertEqual(self.commands, [])

    def test_symlink_or_hardlink_snapshot_is_rejected_before_mutation(self):
        path = self.root / recovery.TARGETS["settings"]
        original = path.with_name("other")
        path.rename(original)
        path.symlink_to(original)
        with self.assertRaises(PermissionError):
            recovery.prepare(self.root, self.main_hash, "a" * 64, True, True)
        path.unlink()
        os.link(original, path)
        with self.assertRaises(PermissionError):
            recovery.prepare(self.root, self.main_hash, "a" * 64, True, True)


if __name__ == "__main__":
    unittest.main()
