"""为真实 Go 安装事务核对旧组件恢复器和新激活器；系统服务调用仅记录在隔离根。"""
import hashlib
import importlib.util
import json
from pathlib import Path
import shutil
import sys

root, source, action = Path(sys.argv[1]), Path(sys.argv[2]), sys.argv[3]
tools = root / "tools"
tools.mkdir(mode=0o700, exist_ok=True)
for original, name in (("runtime_recovery.py", "teslabox-carplay-runtime-recovery.py"), ("runtime_install.py", "install.py")):
    shutil.copyfile(source / "internal/carplaybundle" / original, tools / name)
spec = importlib.util.spec_from_file_location("isolated_install", tools / "install.py")
install = importlib.util.module_from_spec(spec)
spec.loader.exec_module(install)
recovery = install.recovery
main = root / "usr/local/bin/teslabox"
saved = root / "fixture-expected.json"


def command(*args):
    """不调用宿主 systemd，文件和事务处理仍使用未修改的生产 Python 实现。"""
    pass


if action == "setup":
    # 初态模拟已部署组件：原 journal 的 previous 为空，缺前进证据会真实删除现有 unit/config。
    journal = recovery.prepare(root, recovery.digest_file(main), "a" * 64, True, True)
    identity = root / "opt/legacy/reference-livi-private"
    identity.mkdir(mode=0o700, parents=True)
    identity.joinpath("livi-reference-state.json").write_bytes(b"existing-phone-identity")
    identity.joinpath("livi-reference-state.json").chmod(0o600)
    settings = dict(root=str(root / "opt/legacy"), hardware_config=str(root / recovery.TARGETS["hardware"]),
                    binary_sha256="a" * 64, node_sha256="b" * 64, certificate_sha256="c" * 64,
                    phone_bt="AA:BB:CC:DD:EE:01", vehicle_channels=[6, 149],
                    ap_interface="ap0", upstream_interface="wlan0")
    hardware = dict(mfi=dict(transport="ch341", usb_bus=None, usb_address=None))
    originals = {}
    for key, relative in recovery.TARGETS.items():
        value = json.dumps(settings if key == "settings" else hardware).encode() if key in ("settings", "hardware") else ("existing-" + key).encode()
        recovery.atomic_write(root / relative, value, 0o600 if key in ("settings", "hardware") else 0o644)
        originals[key] = value.hex()
    saved.write_text(json.dumps(originals), encoding="utf-8")
    saved.chmod(0o600)
    journal["phase"] = "activated"
    recovery.save(root, journal)
    runtime = root / "opt/teslabox/carplay/versions" / ("b" * 64)
    scripts = runtime / "python"
    scripts.mkdir(mode=0o700, parents=True)
    for name in ("carplay-service.py", "carplay_phone_selection.py"):
        shutil.copyfile(source / "experiments/carplay-native/scripts" / name, scripts / name)
    runtime.joinpath("runtime/bin").mkdir(parents=True)
    runtime.joinpath("runtime/bin/node").write_bytes(b"node")
    runtime.joinpath("teslabox-carplay").write_bytes(b"native")
    print("INITIAL_OLD_COMPONENT_FIXTURE_READY")
elif action in ("recover_forward", "activate", "recover_rollback"):
    restored = recovery.recover(root, command)
    originals = json.loads(saved.read_text(encoding="utf-8"))
    for key, value in originals.items():
        path = root / recovery.TARGETS[key]
        if not path.is_file() or path.read_bytes() != bytes.fromhex(value):
            raise RuntimeError("旧恢复器误删或改写本机组件：" + key)
    if action == "recover_forward" and restored:
        raise RuntimeError("正常前进被误判为回滚")
    if action == "recover_rollback" and not restored:
        raise RuntimeError("真正程序回滚没有恢复组件")
    if action == "activate":
        runtime = root / "opt/teslabox/carplay/versions" / ("b" * 64)
        template = (source / "internal/carplaybundle/teslabox-carplay-experiment.service.in").read_bytes()
        install.activate(root, runtime, recovery.digest_file(main), "b" * 64, template, command, lambda unused: True)
        settings = json.loads((root / recovery.TARGETS["settings"]).read_text())
        if settings["phone_bt"] != "AA:BB:CC:DD:EE:01" or settings["vehicle_channels"] != [6, 149] or \
                settings["ap_interface"] != "ap0" or settings["upstream_interface"] != "wlan0":
            raise RuntimeError("新激活器未继承本机配置")
        if Path(settings["identity_directory"]).joinpath("livi-reference-state.json").read_bytes() != b"existing-phone-identity":
            raise RuntimeError("手机私有身份丢失")
        print("NEW_ACTIVATE_INHERITED_LOCAL_CONFIGURATION")
    print("OLD_PYTHON_RECOVERY", "restored" if restored else "preserved")
else:
    raise RuntimeError("未知隔离测试动作")
