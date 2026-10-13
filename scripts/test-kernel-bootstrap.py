"""只验证维护归档的安装与撤销；所有 systemctl/助手调用为替身。"""

import importlib.util
import json
from pathlib import Path
import tempfile
import unittest
from unittest.mock import patch

spec = importlib.util.spec_from_file_location("kernel_bootstrap", Path(__file__).parents[1] / "deploy/scripts/bootstrap-kernel-upgrade.py")
k = importlib.util.module_from_spec(spec)
spec.loader.exec_module(k)


class MaintenanceTests(unittest.TestCase):
    def fixture(self):
        temp = tempfile.TemporaryDirectory()
        self.addCleanup(temp.cleanup)
        root = Path(temp.name)
        source = root / "source"
        source.mkdir()
        helper = root / k.b.HELPER
        helper.parent.mkdir(parents=True)
        helper.write_bytes(b"old helper")
        helper.chmod(0o755)
        main = root / "usr/local/bin/teslabox"
        main.parent.mkdir(parents=True)
        main.write_bytes(b"old main")
        main.chmod(0o755)
        for name in (*k.INPUTS, "teslabox-update-helper-linux-arm64"):
            (source / name).write_bytes(name.encode())
        return root, source, helper, main

    def commands(self, command):
        if "ActiveState" in " ".join(command):
            return "inactive"
        if "--kernel-protocol" in command:
            return "teslabox-kernel-first-v1"
        if "--kernel-manifest" in command:
            return json.dumps({"main_sha256": "x"})
        if "ExecStartPre" in " ".join(command):
            return "/usr/local/libexec/teslabox-update-helper --recover"
        return ""

    def install(self, root, source, run):
        with patch.object(k, "OLD_HELPERS", {k.b.digest(b"old helper")}), patch.object(k, "OLD_MAIN", k.b.digest(b"old main")):
            return k.install(root, source, k.b.digest(b"teslabox-update-helper-linux-arm64"), run)

    def test_installs_guard_and_resume_without_touching_main_kernel_or_state(self):
        root, source, helper, main = self.fixture()
        backup = self.install(root, source, self.commands)
        self.assertEqual(main.read_bytes(), b"old main")
        self.assertEqual(helper.read_bytes(), b"teslabox-update-helper-linux-arm64")
        self.assertIn(b"900", (root / k.b.UPDATE_GUARD).read_bytes())
        self.assertTrue((root / k.INPUTS["teslabox-kernel-resume.service"]).is_file())
        self.assertFalse((root / "var/lib/teslabox-system/kernel-upgrade").exists())
        self.assertTrue((backup / "manifest.json").is_file())

    def test_failed_enable_restores_old_helper_and_removes_only_new_entries(self):
        root, source, helper, main = self.fixture()
        def fail(command):
            if command[:2] == ["systemctl", "enable"]:
                raise RuntimeError("enable failed")
            return self.commands(command)
        with self.assertRaisesRegex(RuntimeError, "enable failed"):
            self.install(root, source, fail)
        self.assertEqual(helper.read_bytes(), b"old helper")
        self.assertEqual(main.read_bytes(), b"old main")
        self.assertFalse((root / k.INPUTS["kernel-rc1.json"]).exists())
        self.assertFalse((root / k.b.MAIN_GUARD).exists())

    def test_wrong_old_helper_or_pending_kernel_request_refused(self):
        for reason in ("helper", "pending"):
            with self.subTest(reason=reason):
                root, source, helper, main = self.fixture()
                if reason == "helper":
                    helper.write_bytes(b"unknown helper")
                else:
                    pending = root / "var/lib/teslabox-system/kernel-upgrade/request.json"
                    pending.parent.mkdir(parents=True)
                    pending.write_bytes(b"{}")
                before = helper.read_bytes()
                with self.assertRaises(RuntimeError):
                    self.install(root, source, self.commands)
                self.assertEqual(helper.read_bytes(), before)


if __name__ == "__main__":
    unittest.main()
