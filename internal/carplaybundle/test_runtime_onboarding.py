"""执行真实服务外层循环，验证无手机登记时不会提前创建网络租约；不访问硬件。"""
import contextlib
import hashlib
import importlib.util
import json
from pathlib import Path
import tempfile
import threading
from types import SimpleNamespace
import unittest
from unittest.mock import Mock, patch

SOURCE = Path(__file__).resolve().parents[2] / "experiments/carplay-native/scripts"


def load(name, filename):
    """加载发行源码，不执行硬件入口。"""
    spec = importlib.util.spec_from_file_location(name, SOURCE / filename)
    module = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(module)
    return module


service = load("onboarding_service_test", "carplay-service.py")
phones = load("onboarding_phones_test", "carplay_phone_selection.py")
provisioning = load("onboarding_flow_test", "carplay_provisioning.py")


class OnboardingTests(unittest.TestCase):
    def test_unregistered_service_keeps_management_ap_and_reports_ready(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            (root / "runtime/bin").mkdir(parents=True)
            for name in ("teslabox-carplay", "runtime/bin/node"):
                (root / name).write_bytes(b"test-candidate")
            hardware = root / "hardware.json"
            hardware.write_text(json.dumps(dict(oracle_listen="127.0.0.1:5290", adapter="hci0", rfcomm_channel=4)))
            digest = hashlib.sha256(b"test-candidate").hexdigest()
            settings = dict(root=str(root), hardware_config=str(hardware), binary_sha256=digest, node_sha256=digest)
            stopped = threading.Event()
            base = Mock()
            base.command.side_effect = lambda *args: "inactive" if args[0] == "systemctl" else "b false" if args[-1] == "Discovering" else "b true"
            inventory = Mock()
            inventory.annotate.side_effect = lambda value: value
            lease = Mock()
            flow = provisioning.Provisioning(root)
            modules = {
                "service_tools": base, "service_mfi": Mock(),
                "service_power": SimpleNamespace(BluetoothPowerLease=lambda: SimpleNamespace(hold=contextlib.nullcontext)),
                "service_network": SimpleNamespace(NetworkLease=lease),
                "carplay_phone_selection": phones,
                "carplay_user_settings": SimpleNamespace(load=lambda _: (dict(enabled=True, phone_id=""), {})),
                "carplay_bluetooth_inventory": SimpleNamespace(Inventory=lambda _: inventory),
                "carplay_provisioning": SimpleNamespace(Provisioning=lambda _: flow),
            }
            states = []

            def capture(_runtime, value):
                states.append(value)
                stopped.set()

            with patch.object(service, "load_module", side_effect=lambda name, _: modules[name]), \
                    patch.object(phones, "read_registry", return_value=None), \
                    patch.object(service, "write_installation_ready"), \
                    patch.object(service, "authentication_configuration"), \
                    patch.object(service, "select_authentication"), \
                    patch.object(service, "write_status", side_effect=capture):
                service.run(settings, root, stopped)
            lease.assert_not_called()
            lease._command.assert_called_once_with("status")
            self.assertEqual(len(states), 1)
            self.assertTrue(states[0]["onboarding_ready"])
            self.assertFalse(states[0]["running"])
            self.assertTrue(states[0]["cleanup_complete"])
            inventory.close.assert_called_once()


if __name__ == "__main__":
    unittest.main()
