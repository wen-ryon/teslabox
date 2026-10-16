#!/usr/bin/env python3
"""维护窗口安装内核强依赖助手；不预装内核，不替换主程序或用户状态。"""

import argparse
import fcntl
import hashlib
import importlib.util
import json
import os
from pathlib import Path
import subprocess
import tempfile

spec = importlib.util.spec_from_file_location("paired_bootstrap", Path(__file__).with_name("bootstrap-update-rollback.py"))
b = importlib.util.module_from_spec(spec)
spec.loader.exec_module(b)

OLD_HELPERS = {b.OLD_HELPER_SHA256, "95910e01f198a9cb1f0a84110e7d6e4a6ce7588dcad83446b49effceb623817d",
               "a59c885f18edfbcad2a05f9e125698f3eaa78258627caf944a67f575b61fc660"}
OLD_MAIN = "a73f067a7b231c45629f046f609a39494dbd1694e278b55d3e2d84de8813333d"
# 公开早期 RC1 没有内核守卫，只接受对应助手和旧内核维护组件全部缺失的状态。
PRE_KERNEL_PAIRS = {
    "018056f9d73a1cf6030918a4df734c213ff75fa6110d1e1ad85fb35e4c00b01e":
        "95910e01f198a9cb1f0a84110e7d6e4a6ce7588dcad83446b49effceb623817d",
}
# 主程序、助手、控制器、政策及签名必须作为已审核的整套基线出现。
AUDITED_PAIRS = {
    "5ee5385c2aa82bd7106ea4f861eee947db4379f749fda4d945959bae5ad6a48a": (
        "6b9b4047dc6021c1b9e3ddbe56b1afefff3accbf50b776f665e3e783fda1128c",
        {"8f8943ace3b2a20a2cded20f46864df84290dac42e70728c785d5ccbf8d117c5", "f8a9e970fbe1317801b2e809f6776cff78a0fb6ca3344400d28ae7ae97dd8039"},
        "17aea782bc3c24e63f750008038cac9791000f623f35976d139b3bfa99a7152c",
        "5c0e3321caad0a3982b3440a4f53540a056bc544179e88f07da2727c79bac484"),
    "768b24c9b91bd2c2648c94d5d254cb75858dac70a418988e7fa2bc2f565e06d1": (
        "913eccd77ddc04f22d8a62e5aeeee1ddb5b56f0c55793f55006cf645a3b0fe76",
        {"ffcdb7c6a1c428cb98f11661a1eadd54390d45a4b7340c7fa632d9dc3a482669"},
        "c745b4c9c77de2ad5e290ab59858a8552b79b785f7038266c3e5a77679184069",
        "654c09a75775b4d305bf8c3d2c00c631f4bb3831b04d43114cf18d26884af3dd"),
}
OLD_CONTROLLER = "usr/local/libexec/teslabox-kernel-upgrade.py"
INPUTS = {
    "teslabox-kernel-upgrade.py": "usr/local/libexec/teslabox-kernel-upgrade-rc2.py",
    "kernel-rc2.json": "usr/local/share/teslabox/kernel/rc2.json",
    "kernel-rc2.json.sig": "usr/local/share/teslabox/kernel/rc2.json.sig",
    "teslabox-kernel-resume.service": "etc/systemd/system/teslabox-kernel-resume.service",
}


def install(root, source, expected, run):
    # 所有输入必须来自调用者已核对固定摘要/签名的维护归档，不能接受任意网络脚本。
    helper_source = source / "teslabox-update-helper-linux-arm64"
    helper, _ = b.read_regular(helper_source)
    if b.digest(helper) != expected:
        raise RuntimeError("候选维护助手摘要不匹配")
    if run([str(helper_source), "--kernel-protocol"]).strip() != "teslabox-kernel-first-v1":
        raise RuntimeError("候选助手不支持强制内核升级")
    manifest = run([str(helper_source), "--kernel-manifest", str(source / "kernel-rc2.json")])
    policy = json.loads(manifest)
    if policy.get("version") != "0.2.0-rc.2":
        raise RuntimeError("候选政策不是 RC2")
    main, _ = b.read_regular(root / "usr/local/bin/teslabox")
    main_digest = b.digest(main)
    if main_digest not in {OLD_MAIN, policy["main_sha256"]} | AUDITED_PAIRS.keys() | PRE_KERNEL_PAIRS.keys():
        raise RuntimeError("主程序不是正式 0.1.31 或本次审核基线，未修改文件")
    lock = root / "var/lib/teslabox-system/update-backups/install.lock"
    b.durable_mkdir(lock.parent)
    b.safe_path(lock)
    fd = os.open(lock, os.O_CREAT | os.O_RDWR | os.O_NOFOLLOW, 0o600)
    try:
        b.read_regular(lock)
        fcntl.flock(fd, fcntl.LOCK_EX | fcntl.LOCK_NB)
        if b.read_regular(root / "usr/local/bin/teslabox")[0] != main:
            raise RuntimeError("取得维护锁前主程序发生变化，拒绝继续")
        for service in ("teslabox.service", "teslabox-update.service", "teslabox-kernel-resume.service"):
            if run(["systemctl", "show", service, "--property=ActiveState", "--value"]).strip() not in {"inactive", "failed"}:
                raise RuntimeError("服务仍在运行，请先停止并等待维护窗口: " + service)
        for relative in ("active/transaction.json",):
            p = lock.parent / relative
            if p.exists():
                journal, _ = b.read_regular(p)
                state = json.loads(journal)
                if state.get("version") != 1 or state.get("phase") not in {"committed", "rolled_back"}:
                    raise RuntimeError("原程序事务尚未结束，拒绝替换助手")
        if (root / "var/lib/teslabox-system/kernel-upgrade/request.json").exists():
            raise RuntimeError("内核跨重启续装尚未结束，拒绝替换维护输入")
        original_helper, _ = b.read_regular(root / b.HELPER)
        content = {}
        for name, target in INPUTS.items():
            content[target] = b.read_regular(source / name)[0]
        content[b.UPDATE_GUARD] = b"[Service]\nEnvironmentFile=-/etc/teslabox/teslabox.env\nTimeoutStartSec=900\n"
        content[b.HELPER] = helper
        # stock 0.1.31 助手不支持 recover；必须先换助手，再安装该守卫。
        # 新助手自身在守卫完整落盘且 systemd 已加载之前拒绝任何安装事务。
        content[b.MAIN_GUARD] = b.MAIN_CONTENT
        resumed = b.digest(original_helper) == expected
        if resumed:
            # 只接受本次完整前置输入，不允许单凭“新助手摘要”绕过审核或补齐任意半套。
            for target, value in content.items():
                if target == b.MAIN_GUARD:
                    continue
                actual, mode = b.read_regular(root / target)
                expected_mode = 0o755 if target == b.HELPER else 0o644
                if actual != value or mode != expected_mode:
                    raise RuntimeError("已切换助手的维护输入不完整或不属于本次候选")
            guard_path = root / b.MAIN_GUARD
            if os.path.lexists(guard_path) and b.read_regular(guard_path)[0] != b.MAIN_CONTENT:
                raise RuntimeError("已切换助手的恢复守卫不是本次规范内容")
        if main_digest in PRE_KERNEL_PAIRS:
            old_components = (OLD_CONTROLLER, "usr/local/share/teslabox/kernel/rc1.json", "usr/local/share/teslabox/kernel/rc1.json.sig")
            if (not resumed and b.digest(original_helper) != PRE_KERNEL_PAIRS[main_digest]) or any(os.path.lexists(root / name) for name in old_components):
                raise RuntimeError("早期公开 RC1 维护基线不是固定助手及无旧内核组件状态")
        elif main_digest in AUDITED_PAIRS:
            helper_hash, controllers, policy_hash, sig_hash = AUDITED_PAIRS[main_digest]
            old_policy = root / "usr/local/share/teslabox/kernel/rc1.json"
            if ((not resumed and b.digest(original_helper) != helper_hash) or
                b.digest(b.read_regular(root / OLD_CONTROLLER)[0]) not in controllers or
                b.digest(b.read_regular(old_policy)[0]) != policy_hash or
                b.digest(b.read_regular(Path(str(old_policy) + ".sig"))[0]) != sig_hash):
                raise RuntimeError("旧主程序的助手、控制器和签名政策不是审核配对")
            if main_digest not in policy.get("startup_main_sha256", []):
                raise RuntimeError("签名 RC2 政策没有允许当前旧主启动过渡")
        elif b.digest(original_helper) not in OLD_HELPERS | {expected}:
            raise RuntimeError("已安装助手不属于审核过的维护基线")
        resume_enabled = run(["systemctl", "show", "teslabox-kernel-resume.service", "--property=UnitFileState", "--value"]).strip()
        if resume_enabled not in {"", "disabled", "enabled", "static", "not-found"}:
            raise RuntimeError("续装服务使能状态不支持安全恢复")
        previous = {}
        backup_root = root / "var/lib/teslabox-system/update-bootstrap-backups"
        b.durable_mkdir(backup_root)
        backup = Path(tempfile.mkdtemp(prefix="kernel-bootstrap-", dir=backup_root))
        b.sync_dir(backup_root)
        for i, name in enumerate(content):
            path = root / name
            b.safe_path(path)
            previous[name] = b.read_regular(path) if path.exists() else None
            if previous[name]:
                b.durable_write(backup / (str(i) + ".before"), previous[name][0], 0o600)
        receipt = {"schema": 1, "old_helper_sha256": b.digest(original_helper), "new_helper_sha256": expected,
                   "files": [{"path": name, "exists": value is not None, "mode": value[1] if value else None,
                              "sha256": b.digest(value[0]) if value else None, "backup": str(i) + ".before"}
                             for i, (name, value) in enumerate(previous.items())]}
        b.durable_write(backup / "manifest.json", (json.dumps(receipt) + "\n").encode(), 0o600)
        # 新依赖先耐久，助手切换后才写 recover 守卫；守卫加载前新助手拒绝安装。
        try:
            for name, value in content.items():
                b.durable_write(root / name, value, 0o755 if name == b.HELPER else 0o644)
            run(["systemctl", "daemon-reload"])
            run(["systemctl", "enable", "teslabox-kernel-resume.service"])
            guard = run(["systemctl", "show", "teslabox.service", "--property=ExecStartPre", "--value"])
            if "/usr/local/libexec/teslabox-update-helper --recover" not in guard:
                raise RuntimeError("程序启动恢复守卫未生效")
            reload = run(["systemctl", "show", "teslabox.service", "--property=NeedDaemonReload", "--value"]).strip()
            if reload != "no":
                raise RuntimeError("程序启动恢复守卫尚未加载")
            if main_digest != OLD_MAIN and main_digest not in PRE_KERNEL_PAIRS:
                if run([str(root / b.HELPER), "--kernel-verify"]).strip() != "teslabox-kernel-ready-v1":
                    raise RuntimeError("维护后旧主程序启动校验失败")
        except Exception as original:
            # 恢复维护前的使能状态，单个恢复命令失败也不能阻止其余文件回退。
            failures = []
            if resume_enabled != "enabled":
                try:
                    run(["systemctl", "disable", "teslabox-kernel-resume.service"])
                except Exception as error:
                    failures.append(str(error))
            for name, value in reversed(list(previous.items())):
                path = root / name
                try:
                    if value:
                        b.durable_write(path, value[0], value[1])
                    else:
                        path.unlink(missing_ok=True)
                        if path.parent.exists():
                            b.sync_dir(path.parent)
                except Exception as error:
                    failures.append(name + ": " + str(error))
            try:
                run(["systemctl", "daemon-reload"])
            except Exception as error:
                failures.append(str(error))
            if failures:
                raise RuntimeError("维护失败且恢复未完整完成；请保留备份 " + str(backup) + ": " + "; ".join(failures)) from original
            raise
        return backup
    finally:
        os.close(fd)


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--directory", required=True, type=Path)
    parser.add_argument("--helper-sha256", required=True)
    args = parser.parse_args()
    if os.geteuid() != 0:
        parser.error("需要 root 维护权限")
    def run(command):
        return subprocess.check_output(command, text=True, timeout=30)
    backup = install(Path("/"), args.directory, args.helper_sha256, run)
    print("强制内核升级助手已安装；主程序、内核和用户状态未改。备份：" + str(backup))


if __name__ == "__main__":
    main()
