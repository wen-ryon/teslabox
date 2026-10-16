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
    def test_public_pre_kernel_rc1_requires_exact_helper_and_absent_old_components(self):
        for bad in (None, "helper", "controller", "policy", "signature"):
            with self.subTest(bad=bad):
                root, source, helper, main = self.fixture()
                main.write_bytes(b"pre kernel RC1")
                if bad == "helper":
                    helper.write_bytes(b"unknown helper")
                elif bad:
                    targets = {"controller": k.OLD_CONTROLLER,
                               "policy": "usr/local/share/teslabox/kernel/rc1.json",
                               "signature": "usr/local/share/teslabox/kernel/rc1.json.sig"}
                    path = root / targets[bad]
                    path.parent.mkdir(parents=True, exist_ok=True)
                    path.write_bytes(b"unknown")
                def run(command):
                    if "--kernel-verify" in command:
                        self.fail("早期公开 RC1 没有内核守卫，不应要求先装内核")
                    return self.commands(command)
                with patch.object(k, "PRE_KERNEL_PAIRS", {k.b.digest(b"pre kernel RC1"): k.b.digest(b"old helper")}):
                    if bad:
                        with self.assertRaisesRegex(RuntimeError, "早期公开 RC1"):
                            self.install(root, source, run)
                    else:
                        self.install(root, source, run)
                self.assertEqual(main.read_bytes(), b"pre kernel RC1")

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
            return json.dumps({"version": "0.2.0-rc.2", "main_sha256": "x", "startup_main_sha256": [k.b.digest(b"audited RC1")]})
        if "--kernel-verify" in command:
            return "teslabox-kernel-ready-v1"
        if "ExecStartPre" in " ".join(command):
            return "/usr/local/libexec/teslabox-update-helper --recover"
        if "NeedDaemonReload" in " ".join(command):
            return "no"
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
        self.assertFalse((root / k.INPUTS["kernel-rc2.json"]).exists())
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

    def test_only_audited_rc1_main_can_receive_new_maintenance_inputs(self):
        for audited in (True, False):
            with self.subTest(audited=audited):
                root, source, helper, main = self.fixture()
                main.write_bytes(b"audited RC1" if audited else b"unknown RC1")
                controller = root / k.OLD_CONTROLLER
                controller.write_bytes(b"old controller")
                policy = root / "usr/local/share/teslabox/kernel/rc1.json"
                policy.parent.mkdir(parents=True)
                policy.write_bytes(b"old policy")
                policy.with_suffix(".json.sig").write_bytes(b"old signature")
                pair = (k.b.digest(b"old helper"), {k.b.digest(b"old controller")}, k.b.digest(b"old policy"), k.b.digest(b"old signature"))
                with patch.object(k, "AUDITED_PAIRS", {k.b.digest(b"audited RC1"): pair}):
                    if audited:
                        self.install(root, source, self.commands)
                        self.assertEqual(main.read_bytes(), b"audited RC1")
                    else:
                        with self.assertRaisesRegex(RuntimeError, "主程序不是"):
                            self.install(root, source, self.commands)
                        self.assertEqual(helper.read_bytes(), b"old helper")

    def test_each_post_write_failure_restores_entire_previous_pair(self):
        for stage in ("reload", "enable", "guard", "verify"):
            with self.subTest(stage=stage):
                root, source, helper, main = self.fixture()
                main.write_bytes(b"audited RC1")
                controller = root / k.OLD_CONTROLLER
                controller.write_bytes(b"old controller")
                old = root / "usr/local/share/teslabox/kernel/rc1.json"
                old.parent.mkdir(parents=True)
                old.write_bytes(b"old policy")
                old.with_suffix(".json.sig").write_bytes(b"old signature")
                before = {}
                for target in (*k.INPUTS.values(), k.b.HELPER, k.b.UPDATE_GUARD, k.b.MAIN_GUARD):
                    p = root / target
                    before[target] = p.read_bytes() if p.exists() else None
                pair = (k.b.digest(b"old helper"), {k.b.digest(b"old controller")}, k.b.digest(b"old policy"), k.b.digest(b"old signature"))
                failed = False
                def run(command):
                    nonlocal failed
                    match = ((stage == "reload" and command == ["systemctl", "daemon-reload"]) or
                             (stage == "enable" and command[:2] == ["systemctl", "enable"]) or
                             (stage == "guard" and "ExecStartPre" in " ".join(command)) or
                             (stage == "verify" and "--kernel-verify" in command))
                    if match and not failed:
                        failed = True
                        raise RuntimeError("injected " + stage)
                    return self.commands(command)
                with patch.object(k, "AUDITED_PAIRS", {k.b.digest(b"audited RC1"): pair}), self.assertRaisesRegex(RuntimeError, "injected"):
                    self.install(root, source, run)
                for target, expected in before.items():
                    p = root / target
                    self.assertEqual(p.read_bytes() if p.exists() else None, expected)
                self.assertEqual(old.read_bytes(), b"old policy")
                self.assertEqual(main.read_bytes(), b"audited RC1")

    def test_unknown_pair_member_or_missing_signed_permission_refused_before_writes(self):
        for bad in ("helper", "controller", "policy", "signature", "permission"):
            with self.subTest(bad=bad):
                root, source, helper, main = self.fixture()
                main.write_bytes(b"audited RC1")
                paths = {"helper": helper, "controller": root / k.OLD_CONTROLLER,
                         "policy": root / "usr/local/share/teslabox/kernel/rc1.json",
                         "signature": root / "usr/local/share/teslabox/kernel/rc1.json.sig"}
                for name, path in paths.items():
                    path.parent.mkdir(parents=True, exist_ok=True)
                    path.write_bytes(("old " + name).encode())
                pair = (k.b.digest(paths["helper"].read_bytes()), {k.b.digest(paths["controller"].read_bytes())},
                        k.b.digest(paths["policy"].read_bytes()), k.b.digest(paths["signature"].read_bytes()))
                if bad != "permission":
                    paths[bad].write_bytes(b"unknown")
                before = {name: path.read_bytes() for name, path in paths.items()}
                def run(command):
                    if bad == "permission" and "--kernel-manifest" in command:
                        return json.dumps({"version": "0.2.0-rc.2", "main_sha256": "x"})
                    return self.commands(command)
                with patch.object(k, "AUDITED_PAIRS", {k.b.digest(b"audited RC1"): pair}), self.assertRaises(RuntimeError):
                    self.install(root, source, run)
                self.assertEqual({name: path.read_bytes() for name, path in paths.items()}, before)
                self.assertFalse((root / k.INPUTS["kernel-rc2.json"]).exists())

    def test_each_input_write_failure_restores_all_previous_files(self):
        for failed_target in (*k.INPUTS.values(), k.b.HELPER, k.b.MAIN_GUARD, k.b.UPDATE_GUARD):
            with self.subTest(target=failed_target):
                root, source, helper, main = self.fixture()
                previous = {}
                for target in (*k.INPUTS.values(), k.b.HELPER, k.b.MAIN_GUARD, k.b.UPDATE_GUARD):
                    path = root / target
                    path.parent.mkdir(parents=True, exist_ok=True)
                    if path != helper:
                        path.write_bytes(("before " + target).encode())
                    previous[target] = path.read_bytes()
                real_write = k.b.durable_write
                injected = False
                def write(path, content, mode):
                    nonlocal injected
                    if path == root / failed_target and not injected:
                        injected = True
                        raise RuntimeError("injected write")
                    return real_write(path, content, mode)
                with patch.object(k.b, "durable_write", side_effect=write), self.assertRaisesRegex(RuntimeError, "injected write"):
                    self.install(root, source, self.commands)
                self.assertEqual({target: (root / target).read_bytes() for target in previous}, previous)

    def test_abrupt_exit_at_every_write_preserves_complete_active_generation(self):
        class PowerLoss(BaseException):
            pass
        targets = (*k.INPUTS.values(), k.b.UPDATE_GUARD, k.b.HELPER, k.b.MAIN_GUARD)
        for stop in targets:
            with self.subTest(stop=stop):
                root, source, helper, main = self.fixture()
                main.write_bytes(b"audited RC1")
                old_paths = {k.OLD_CONTROLLER: b"old controller",
                             "usr/local/share/teslabox/kernel/rc1.json": b"old policy",
                             "usr/local/share/teslabox/kernel/rc1.json.sig": b"old signature"}
                for target, content in old_paths.items():
                    path = root / target
                    path.parent.mkdir(parents=True, exist_ok=True)
                    path.write_bytes(content)
                pair = (k.b.digest(b"old helper"), {k.b.digest(b"old controller")},
                        k.b.digest(b"old policy"), k.b.digest(b"old signature"))
                writes = []
                real_write = k.b.durable_write
                def interrupt(path, content, mode):
                    real_write(path, content, mode)
                    relative = str(path.relative_to(root))
                    if relative in targets:
                        writes.append(relative)
                        if relative == stop:
                            raise PowerLoss()
                with patch.object(k, "AUDITED_PAIRS", {k.b.digest(b"audited RC1"): pair}):
                    # BaseException 不走异常回退，模拟进程在完成一次耐久写入后直接消失。
                    with patch.object(k.b, "durable_write", side_effect=interrupt), self.assertRaises(PowerLoss):
                        self.install(root, source, self.commands)
                    for target, content in old_paths.items():
                        self.assertEqual((root / target).read_bytes(), content)
                    self.assertEqual(main.read_bytes(), b"audited RC1")
                    if helper.read_bytes() == b"old helper":
                        self.assertNotIn(k.b.HELPER, writes)
                        # 旧审核配对完整，取得已释放的锁后可重新执行维护。
                        self.install(root, source, self.commands)
                    else:
                        self.assertEqual(writes, list(targets[:targets.index(stop) + 1]))
                        for name, target in k.INPUTS.items():
                            self.assertEqual((root / target).read_bytes(), (source / name).read_bytes())
                        # 助手已换但守卫可能尚未写，仍须严格识别本次前置输入并可继续完成。
                        self.install(root, source, self.commands)
                    self.assertEqual((root / k.b.MAIN_GUARD).read_bytes(), k.b.MAIN_CONTENT)

    def test_all_four_baselines_can_restart_at_each_write_and_systemd_boundary(self):
        class PowerLoss(BaseException):
            pass
        targets = (*k.INPUTS.values(), k.b.UPDATE_GUARD, k.b.HELPER, k.b.MAIN_GUARD)
        for cohort in ("stock31", "early018", "public5ee", "frp768"):
            for stop in (*targets, "reload", "enable", "guard", "reload-state"):
                with self.subTest(cohort=cohort, stop=stop):
                    root, source, helper, main = self.fixture()
                    main.write_bytes(cohort.encode())
                    main_hash = k.b.digest(main.read_bytes())
                    old_files = {}
                    if cohort in {"public5ee", "frp768"}:
                        old_files = {k.OLD_CONTROLLER: b"old controller",
                                     "usr/local/share/teslabox/kernel/rc1.json": b"old policy",
                                     "usr/local/share/teslabox/kernel/rc1.json.sig": b"old signature"}
                        for target, value in old_files.items():
                            p = root / target
                            p.parent.mkdir(parents=True, exist_ok=True)
                            p.write_bytes(value)
                    if cohort != "stock31":
                        p = root / k.b.MAIN_GUARD
                        p.parent.mkdir(parents=True, exist_ok=True)
                        p.write_bytes(k.b.MAIN_CONTENT)
                    pair = (k.b.digest(b"old helper"), {k.b.digest(b"old controller")}, k.b.digest(b"old policy"), k.b.digest(b"old signature"))
                    real_write = k.b.durable_write
                    fired = False
                    def write(path, value, mode):
                        nonlocal fired
                        real_write(path, value, mode)
                        if str(path.relative_to(root)) == stop:
                            fired = True
                            raise PowerLoss()
                    def run(command):
                        nonlocal fired
                        if "--kernel-manifest" in command:
                            return json.dumps({"version": "0.2.0-rc.2", "main_sha256": "x", "startup_main_sha256": [main_hash]})
                        matches = ((stop == "reload" and command == ["systemctl", "daemon-reload"]) or
                                   (stop == "enable" and command[:2] == ["systemctl", "enable"]) or
                                   (stop == "guard" and "ExecStartPre" in " ".join(command)) or
                                   (stop == "reload-state" and "NeedDaemonReload" in " ".join(command)))
                        if matches and not fired:
                            fired = True
                            raise PowerLoss()
                        return self.commands(command)
                    completed = []
                    def complete(command):
                        completed.append(command)
                        if "--kernel-manifest" in command:
                            return json.dumps({"version": "0.2.0-rc.2", "main_sha256": "x", "startup_main_sha256": [main_hash]})
                        return self.commands(command)
                    with patch.object(k, "OLD_MAIN", main_hash if cohort == "stock31" else "unused"), patch.object(k, "OLD_HELPERS", {k.b.digest(b"old helper")}), patch.object(k, "PRE_KERNEL_PAIRS", {main_hash: k.b.digest(b"old helper")} if cohort == "early018" else {}), patch.object(k, "AUDITED_PAIRS", {main_hash: pair} if old_files else {}):
                        expected = k.b.digest(b"teslabox-update-helper-linux-arm64")
                        with patch.object(k.b, "durable_write", side_effect=write), self.assertRaises(PowerLoss):
                            k.install(root, source, expected, run)
                        guard = root / k.b.MAIN_GUARD
                        if cohort == "stock31" and helper.read_bytes() == b"old helper":
                            self.assertFalse(guard.exists(), "stock 助手无 recover，不能提前给它装守卫")
                        for path, value in old_files.items():
                            self.assertEqual((root / path).read_bytes(), value)
                        k.install(root, source, expected, complete)
                        self.assertEqual(main.read_bytes(), cohort.encode())
                        self.assertEqual(guard.read_bytes(), k.b.MAIN_CONTENT)
                        self.assertIn(["systemctl", "daemon-reload"], completed)
                        self.assertIn(["systemctl", "enable", "teslabox-kernel-resume.service"], completed)
                        if old_files:
                            self.assertTrue(any("--kernel-verify" in command for command in completed))

    def test_resumed_helper_requires_exact_complete_inputs(self):
        for bad in (*k.INPUTS.values(), k.b.UPDATE_GUARD, k.b.MAIN_GUARD):
            with self.subTest(bad=bad):
                root, source, helper, main = self.fixture()
                self.install(root, source, self.commands)
                (root / bad).write_bytes(b"unknown")
                before = helper.read_bytes()
                with self.assertRaisesRegex(RuntimeError, "已切换助手"):
                    self.install(root, source, self.commands)
                self.assertEqual(helper.read_bytes(), before)


if __name__ == "__main__":
    unittest.main()
