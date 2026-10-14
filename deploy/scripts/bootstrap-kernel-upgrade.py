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
# 只接替已审核的内核升级候选，不接受任意同版本或脏工作区产物。
AUDITED_RC1_MAINS = {
    "a446e06b338ff99f3f62719e007e8cbfb613c972d194023c7a5f5a89b3b8eec5",
    "058c1369d46ed0b96971187a59b79ce2367511563dc073b02403ba1044944084",
}
INPUTS = {
    "teslabox-kernel-upgrade.py": "usr/local/libexec/teslabox-kernel-upgrade.py",
    "kernel-rc1.json": "usr/local/share/teslabox/kernel/rc1.json",
    "kernel-rc1.json.sig": "usr/local/share/teslabox/kernel/rc1.json.sig",
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
    manifest = run([str(helper_source), "--kernel-manifest", str(source / "kernel-rc1.json")])
    policy = json.loads(manifest)
    main, _ = b.read_regular(root / "usr/local/bin/teslabox")
    if b.digest(main) not in {OLD_MAIN, policy["main_sha256"]} | AUDITED_RC1_MAINS:
        raise RuntimeError("主程序不是正式 0.1.31 或本次固定 RC1，未修改文件")
    lock = root / "var/lib/teslabox-system/update-backups/install.lock"
    b.durable_mkdir(lock.parent)
    b.safe_path(lock)
    fd = os.open(lock, os.O_CREAT | os.O_RDWR | os.O_NOFOLLOW, 0o600)
    try:
        b.read_regular(lock)
        fcntl.flock(fd, fcntl.LOCK_EX | fcntl.LOCK_NB)
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
        if b.digest(original_helper) not in OLD_HELPERS | {expected}:
            raise RuntimeError("已安装助手不属于审核过的维护基线")
        content = {}
        for name, target in INPUTS.items():
            content[target] = b.read_regular(source / name)[0]
        content[b.HELPER] = helper
        content[b.UPDATE_GUARD] = b"[Service]\nEnvironmentFile=-/etc/teslabox/teslabox.env\nTimeoutStartSec=900\n"
        content[b.MAIN_GUARD] = b.MAIN_CONTENT
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
        # 所有跨重启输入先落盘，新助手随后落盘，守卫最后启用；未安装内核和主程序。
        try:
            for name, value in content.items():
                b.durable_write(root / name, value, 0o755 if name == b.HELPER else 0o644)
            run(["systemctl", "daemon-reload"])
            run(["systemctl", "enable", "teslabox-kernel-resume.service"])
            guard = run(["systemctl", "show", "teslabox.service", "--property=ExecStartPre", "--value"])
            if "/usr/local/libexec/teslabox-update-helper --recover" not in guard:
                raise RuntimeError("程序启动恢复守卫未生效")
        except Exception:
            # 只撤销本次新增的续装入口，旧入口的使能状态不在失败时擅自关闭。
            if previous[INPUTS["teslabox-kernel-resume.service"]] is None:
                run(["systemctl", "disable", "teslabox-kernel-resume.service"])
            for name, value in reversed(list(previous.items())):
                path = root / name
                if value:
                    b.durable_write(path, value[0], value[1])
                else:
                    path.unlink(missing_ok=True)
                    if path.parent.exists():
                        b.sync_dir(path.parent)
            run(["systemctl", "daemon-reload"])
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
