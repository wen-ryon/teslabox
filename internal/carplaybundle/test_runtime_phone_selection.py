"""回归新机首次登记边界；只加载纯函数，不访问蓝牙、网络或设备身份文件。"""
import importlib.util
from pathlib import Path
import unittest
from unittest.mock import Mock


SOURCE = Path(__file__).resolve().parents[2] / "experiments/carplay-native/scripts"


def load(name, filename):
    """加载与发行组件相同的源码，禁止调用服务 main 或 run。"""
    spec = importlib.util.spec_from_file_location(name, SOURCE / filename)
    module = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(module)
    return module


phones = load("phone_selection_regression", "carplay_phone_selection.py")
service = load("phone_service_regression", "carplay-service.py")
LEGACY = "AA:BB:CC:DD:EE:01"
OTHER = "AA:BB:CC:DD:EE:02"


class PhoneSelectionTests(unittest.TestCase):
    def assert_unselected(self, state):
        """未登记必须直接进入等待态，不能发起系统调用或触发 MAC 格式错误。"""
        base = Mock()
        base.command.side_effect = AssertionError("禁止系统调用")
        self.assertFalse(service.selected_phone_is_paired(base, state["selected"]))
        base.command.assert_not_called()
        self.assertIsNone(state["selected"])

    def test_missing_registry_without_legacy_waits_for_enrollment(self):
        state = phones.selection(None)
        self.assert_unselected(state)
        self.assertEqual(state["phones"], [])
        self.assertIsNone(state["last_successful"])
        self.assertTrue(state["legacy_pending"])

    def test_missing_registry_requested_phone_is_not_implicitly_registered(self):
        self.assert_unselected(phones.selection(None, requested=OTHER))

    def test_valid_empty_registry_waits_without_bluetooth_call(self):
        state = phones.selection(dict(version=1, phones=[], last_successful=None))
        self.assert_unselected(state)
        self.assertEqual(state["phones"], [])
        self.assertFalse(state["legacy_pending"])

    def test_valid_legacy_preserves_default_and_explicit_selection(self):
        for requested in ("", LEGACY):
            with self.subTest(requested=requested):
                state = phones.selection(None, requested=requested, legacy=LEGACY)
                self.assertEqual(state["selected"], LEGACY)
                self.assertEqual(state["phones"], [dict(id=LEGACY)])
                base = Mock()
                base.command.return_value = "b true"
                self.assertTrue(service.selected_phone_is_paired(base, state["selected"]))
                base.command.assert_called_once_with(
                    "busctl", "--system", "get-property", "org.bluez",
                    "/org/bluez/hci0/dev_AA_BB_CC_DD_EE_01", "org.bluez.Device1", "Paired")

    def test_requested_mismatch_does_not_fall_back_to_legacy(self):
        state = phones.selection(None, requested=OTHER, legacy=LEGACY)
        self.assert_unselected(state)
        self.assertEqual(state["phones"], [dict(id=LEGACY)])

    def test_invalid_selection_inputs_still_fail(self):
        for field in ("requested", "legacy"):
            for value in (None, 42, "invalid", "aa:bb:cc:dd:ee:01"):
                with self.subTest(field=field, value=value):
                    with self.assertRaises(ValueError):
                        phones.selection(None, **{field: value})

    def test_pair_validator_still_rejects_empty_or_invalid_mac(self):
        for value in ("", "invalid", 42):
            with self.subTest(value=value):
                base = Mock()
                base.command.side_effect = AssertionError("禁止系统调用")
                with self.assertRaises(ValueError):
                    service.selected_phone_is_paired(base, value)
                base.command.assert_not_called()


if __name__ == "__main__":
    unittest.main()
