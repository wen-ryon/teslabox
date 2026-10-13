"""隔离文件系统与命令替身；不连接板子，不执行真实 dpkg、重启或驱动操作。"""

import importlib.util
import io
import json
import os
from pathlib import Path
import tarfile
import tempfile
import unittest
from unittest.mock import patch

spec = importlib.util.spec_from_file_location("kernel_upgrade", Path(__file__).parents[1] / "deploy/scripts/teslabox-kernel-upgrade.py")
k = importlib.util.module_from_spec(spec)
spec.loader.exec_module(k)


class ArchiveMenuTests(unittest.TestCase):
    def test_default_changes_but_old_entry_remains(self):
        old = b"default old\nlabel old\n linux /boot/vmlinuz-old\nlabel new\n linux /boot/vmlinuz-5.10.160-teslabox3\n"
        menu = k.selected_menu(old, k.RELEASE, "old")
        self.assertTrue(menu.startswith(b"default new\n"))
        self.assertIn(b"linux /boot/vmlinuz-old", menu)
        self.assertEqual(menu.count(b"default "), 1)

    def test_missing_old_and_ambiguous_new_rejected(self):
        for menu in (b"label new\n linux /boot/vmlinuz-5.10.160-teslabox3\n", b"label old\n linux /boot/vmlinuz-old\nlabel new\n linux /boot/vmlinuz-5.10.160-teslabox3\nlabel duplicate\n linux /boot/vmlinuz-5.10.160-teslabox3\n"):
            with self.assertRaises(RuntimeError):
                k.selected_menu(menu, k.RELEASE, "old")

    def test_debian_normal_and_rescue_entries_are_not_ambiguous(self):
        menu = b"label old\n linux /boot/vmlinuz-old\nlabel l0\n linux /boot/vmlinuz-5.10.160-teslabox3\n append root=test quiet\nlabel l0r\n menu label new (rescue target)\n linux /boot/vmlinuz-5.10.160-teslabox3\n append root=test single\n"
        result = k.selected_menu(menu, k.RELEASE, "old")
        self.assertTrue(result.startswith(b"default l0\n"))
        self.assertIn(b"label l0r\n", result)

    def test_exact_members_and_tampered_duplicate_link_extra_rejected(self):
        for mode in ("good", "digest", "duplicate", "link", "extra"):
            with self.subTest(mode=mode), tempfile.TemporaryDirectory() as directory:
                archive = Path(directory) / "p.tar.gz"
                with tarfile.open(archive, "w:gz") as tar:
                    member = tarfile.TarInfo("module")
                    member.size = 3
                    if mode == "link":
                        member.type = tarfile.SYMTYPE
                        member.linkname = "/etc/passwd"
                    tar.addfile(member, io.BytesIO(b"bad" if mode == "digest" else b"abc"))
                    if mode in ("duplicate", "extra"):
                        member.name = "../extra" if mode == "extra" else "module"
                        tar.addfile(member, io.BytesIO(b"abc"))
                files = {"module": {"sha256": k.digest(b"abc"), "size": 3}}
                if mode == "good":
                    self.assertEqual(k.validate_archive(archive, files), {"module": b"abc"})
                else:
                    with self.assertRaises(RuntimeError):
                        k.validate_archive(archive, files)


@unittest.skipUnless(os.name == "posix", "Linux fsync/ownership tests run in CI")
class TransactionTests(unittest.TestCase):
    def fixture(self):
        temporary = tempfile.TemporaryDirectory()
        self.addCleanup(temporary.cleanup)
        root = Path(temporary.name)
        for name in ("boot/extlinux", "var", "usr/lib/modules", "usr/lib/firmware", "lib"):
            (root / name).mkdir(parents=True, exist_ok=True)
        (root / "lib/modules").symlink_to("../usr/lib/modules")
        (root / "lib/firmware").symlink_to("../usr/lib/firmware")
        menu = root / "boot/extlinux/extlinux.conf"
        menu.write_bytes(b"default old\nlabel old\n linux /boot/vmlinuz-old\n")
        entries = {"kernel.deb": b"deb", "modules/8852be.ko": b"apfix6", "firmware/mediatek/BT_RAM_CODE_MT7922_1_1_hdr.bin": b"firmware", "licenses/LICENCE.mediatek": b"license"}
        archive = root / "input.tar.gz"
        with tarfile.open(archive, "w:gz") as tar:
            for name, content in entries.items():
                m = tarfile.TarInfo(name)
                m.size = len(content)
                tar.addfile(m, io.BytesIO(content))
        policy = {"files": {name: {"sha256": k.digest(content), "size": len(content)} for name, content in entries.items()}, "main_sha256": "a" * 64}
        request = {"version": k.VERSION, "sha256": "a" * 64, "size": 1, "package_name": "main"}
        calls = []
        def run(command):
            calls.append(command)
            if command[0] == "modinfo":
                return k.RELEASE + " SMP"
            if command[0] == "u-boot-update":
                menu.write_bytes(menu.read_bytes() + b"label new\n linux /boot/vmlinuz-5.10.160-teslabox3\n")
            return ""
        upgrade = k.KernelUpgrade(policy, root=root, run=run, running="old", boot_id="boot-1")
        upgrade.download = lambda: archive
        upgrade.runtime_ok = lambda: False
        return root, menu, upgrade, request, calls

    def test_installs_without_success_then_resumes_only_after_new_boot(self):
        root, menu, u, request, calls = self.fixture()
        original = menu.read_bytes()
        self.assertEqual(u.prepare(request), {"reboot_required": True})
        self.assertEqual(u.load()["phase"], "awaiting_reboot")
        self.assertEqual(u.before.read_bytes(), original)
        self.assertTrue((root / "lib").is_dir())
        self.assertTrue((root / "lib/modules").is_symlink())
        self.assertEqual(u.destination("modules/8852be.ko").read_bytes(), b"apfix6")
        count = len(calls)
        self.assertEqual(u.prepare(request), {"reboot_required": True})
        self.assertEqual(len(calls), count)
        u.boot_id = "boot-2"
        u.runtime_ok = lambda: True
        self.assertEqual(u.prepare(request), {"reboot_required": False})
        self.assertEqual(u.load()["phase"], "boot_verified")
        u.commit(request)
        self.assertEqual(u.load()["phase"], "committed")
        self.assertFalse(u.request_path.exists())

    def test_each_install_failure_restores_old_default_and_keeps_request_from_success(self):
        for command in ("dpkg", "depmod", "update-initramfs", "u-boot-update"):
            with self.subTest(command=command):
                root, menu, u, request, calls = self.fixture()
                original = menu.read_bytes()
                run = u.run
                def fail(args):
                    if args[0] == command:
                        raise RuntimeError("injected " + command)
                    return run(args)
                u.run = fail
                with self.assertRaisesRegex(RuntimeError, "injected"):
                    u.prepare(request)
                self.assertEqual(menu.read_bytes(), original)
                self.assertEqual(u.load()["phase"], "failed")
                self.assertFalse(u.request_path.exists())
                self.assertFalse(u.destination("modules/8852be.ko").exists())

    def test_failed_boot_and_interrupted_install_do_not_upgrade_program(self):
        for phase in ("awaiting_reboot", "installing"):
            with self.subTest(phase=phase):
                root, menu, u, request, calls = self.fixture()
                original = menu.read_bytes()
                u.prepare(request)
                data = u.load()
                u.save(data, phase)
                u.boot_id = "boot-2"
                with self.assertRaises(RuntimeError):
                    u.prepare(request)
                self.assertEqual(menu.read_bytes(), original)
                self.assertEqual(u.load()["phase"], "failed")
                self.assertFalse(u.request_path.exists())

    def test_changed_request_or_policy_cannot_overwrite_pending_transaction(self):
        root, menu, u, request, calls = self.fixture()
        u.prepare(request)
        with self.assertRaises(RuntimeError):
            u.prepare({**request, "size": 2})
        u.policy["main_sha256"] = "b" * 64
        with self.assertRaisesRegex(RuntimeError, "清单发生变化"):
            u.load()


if __name__ == "__main__":
    unittest.main()
