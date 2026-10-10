"""在隔离根目录执行真实配置生成和各激活断点，不调用系统服务。"""
import importlib.util
import json
import os
from pathlib import Path
import shutil
import tempfile
import unittest

HERE = Path(__file__).parent


class InstallTests(unittest.TestCase):
    def setUp(self):
        self.temporary = tempfile.TemporaryDirectory()
        self.addCleanup(self.temporary.cleanup)
        self.root = Path(self.temporary.name)
        tools = self.root / "tools"
        tools.mkdir(mode=0o700)
        shutil.copyfile(HERE / "runtime_recovery.py", tools / "teslabox-carplay-runtime-recovery.py")
        shutil.copyfile(HERE / "runtime_install.py", tools / "install.py")
        spec = importlib.util.spec_from_file_location("isolated_install", tools / "install.py")
        self.install = importlib.util.module_from_spec(spec)
        spec.loader.exec_module(self.install)
        self.recovery = self.install.recovery
        self.runtime = self.root / "opt/teslabox/carplay/versions" / ("a" * 64)
        python = self.runtime / "python"
        python.mkdir(parents=True, mode=0o700)
        # Windows 本地和板端压缩测试夹具都使用明确的源目录，不扫描调试目录。
        source = HERE / "service-fixtures"
        if not source.exists():
            source = HERE.parent.parent / "experiments/carplay-native/scripts"
        for name in ("carplay-service.py", "carplay_phone_selection.py"):
            shutil.copyfile(source / name, python / name)
        self.runtime.joinpath("teslabox-carplay").write_bytes(b"native")
        self.runtime.joinpath("runtime/bin").mkdir(parents=True)
        self.runtime.joinpath("runtime/bin/node").write_bytes(b"node")
        self.main = self.root / "usr/local/bin/teslabox"
        self.main.parent.mkdir(parents=True)
        self.main.write_bytes(b"new-main")
        self.main_hash = self.recovery.digest_file(self.main)
        self.old_root = self.root / "opt/legacy"
        self.identity = self.old_root / "reference-livi-private"
        self.identity.mkdir(parents=True, mode=0o700)
        self.identity.joinpath("livi-reference-state.json").write_bytes(b"existing-pairing-bytes")
        self.identity.joinpath("livi-reference-state.json").chmod(0o600)
        self.originals = {}
        self.template = (HERE / "teslabox-carplay-experiment.service.in").read_bytes()
        settings = dict(root=str(self.old_root), hardware_config=str(self.root / self.recovery.TARGETS["hardware"]),
                        binary_sha256="a" * 64, node_sha256="b" * 64, certificate_sha256="c" * 64,
                        ap_uuid="11111111-1111-4111-8111-111111111111", vehicle_channels=[6, 149],
                        phone_bt="AA:BB:CC:DD:EE:01")
        hardware = dict(mfi=dict(transport="ch341", usb_bus=None, usb_address=None))
        for key, relative in self.recovery.TARGETS.items():
            path = self.root / relative
            data = json.dumps(settings if key == "settings" else hardware).encode() if key in ("settings", "hardware") else ("old-" + key).encode()
            self.recovery.atomic_write(path, data, 0o600 if key in ("settings", "hardware") else 0o644)
            self.originals[key] = data
        self.calls = []

    def command(self, *args):
        self.calls.append(args)

    def activate(self, command=None):
        return self.install.activate(self.root, self.runtime, self.main_hash, "a" * 64,
                                     self.template, command or self.command, lambda unused: True)

    def assert_originals(self):
        for key, relative in self.recovery.TARGETS.items():
            self.assertEqual((self.root / relative).read_bytes(), self.originals[key])
        self.assertEqual(self.identity.joinpath("livi-reference-state.json").read_bytes(), b"existing-pairing-bytes")

    def test_success_preserves_this_device_identity_and_removes_static_network_pin(self):
        self.assertTrue(self.activate())
        value = json.loads((self.root / self.recovery.TARGETS["settings"]).read_bytes())
        self.assertEqual(value["identity_directory"], str(self.identity))
        self.assertEqual(value["phone_bt"], "AA:BB:CC:DD:EE:01")
        self.assertNotIn("phone_ip", value)
        self.assertNotIn("ap_uuid", value)
        self.assertEqual(self.recovery.load(self.root)["phase"], "activated")
        self.assertFalse(self.activate())
        self.assertEqual(self.identity.joinpath("livi-reference-state.json").read_bytes(), b"existing-pairing-bytes")

    def test_first_install_does_not_clone_any_development_identity(self):
        (self.root / self.recovery.TARGETS["settings"]).unlink()
        value, hardware, identity = self.install.settings_for(self.root, self.runtime)
        for key in ("phone_bt", "phone_ip", "ap_uuid", "certificate_sha256"):
            self.assertNotIn(key, value)
        self.assertEqual(identity, self.root / "var/lib/teslabox-system/carplay/phones")
        self.assertEqual(value["authentication"]["mode"], "auto")
        self.assertEqual(hardware["wifi"]["passphrase"], "replaced-by-network-lease")

    def test_failure_at_each_service_operation_restores_old_bytes(self):
        # 第一轮 daemon-reload 尚未修改组件；之后每个失败点都必须走同一恢复器。
        for fail_at in range(1, 7):
            with self.subTest(step=fail_at):
                fired = False
                count = 0

                def fail_once(*args):
                    nonlocal fired, count
                    count += 1
                    if count == fail_at and not fired:
                        fired = True
                        raise RuntimeError("simulated component failure")

                with self.assertRaises(RuntimeError):
                    self.activate(fail_once)
                self.assert_originals()

    def test_old_helper_restores_legacy_configuration_after_complete_activation(self):
        self.activate()
        self.main.write_bytes(b"old-main")
        self.assertTrue(self.recovery.recover(self.root, self.command))
        self.assert_originals()

    def test_private_identity_link_aborts_before_service_mutation(self):
        alternate = self.root / "elsewhere"
        self.identity.rename(alternate)
        self.identity.symlink_to(alternate)
        with self.assertRaises(PermissionError):
            self.activate()
        self.assertEqual(self.calls, [])


if __name__ == "__main__":
    unittest.main()
