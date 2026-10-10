"""主 ELF 已校验公开版本后，创建本机配置并执行可独立恢复的激活事务。"""
import argparse
import hashlib
import importlib.util
import json
import os
from pathlib import Path
import subprocess

spec = importlib.util.spec_from_file_location("recovery", Path(__file__).with_name("teslabox-carplay-runtime-recovery.py"))
recovery = importlib.util.module_from_spec(spec)
spec.loader.exec_module(recovery)

LEGACY = "var/lib/teslabox-carplay-experiment/carplay-service.json"
RECOVERY_HOOK = "etc/systemd/system/teslabox.service.d/50-carplay-recovery.conf"
RECOVERY_COMMAND = "/usr/local/libexec/teslabox-carplay-runtime-recovery.py"


def encode(value):
    """私有配置写为完整 UTF-8 文档，不将内容打印进安装日志。"""
    return (json.dumps(value, ensure_ascii=False, indent=2) + "\n").encode()


def service_state(operation):
    """仅接受确切状态，不将 systemctl 执行错误当作未安装。"""
    property_name = "ActiveState" if operation == "active" else "UnitFileState"
    result = subprocess.run(["systemctl", "show", recovery.SERVICE, "-p", property_name, "--value"],
                            capture_output=True, text=True, timeout=5)
    if result.returncode != 0:
        raise RuntimeError("无法读取原 CarPlay 服务状态")
    value = result.stdout.strip()
    allowed = {"active", "inactive", "failed", "activating", "deactivating", ""} if operation == "active" else {"enabled", "disabled", "static", "", "not-found"}
    if value not in allowed or value in {"activating", "deactivating"}:
        raise RuntimeError("原 CarPlay 服务处于不适合更新的状态")
    return value == ("active" if operation == "active" else "enabled")


def load_module(runtime, name, filename):
    spec = importlib.util.spec_from_file_location(name, runtime / "python" / filename)
    module = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(module)
    return module


def settings_for(root, runtime):
    """保留原机登记身份原路径，避免复制、重建或在回退时分叉已配对记录。"""
    service = load_module(runtime, "product_service", "carplay-service.py")
    destination = recovery.safe_path(root, recovery.TARGETS["settings"])
    old_data = recovery.read_file(destination, private=True)
    legacy_path = recovery.safe_path(root, LEGACY)
    if old_data is None:
        old_data = recovery.read_file(legacy_path, private=True)
    old = service.load_settings(destination if destination.exists() else legacy_path) if old_data is not None else {}
    # 旧登记目录继续留在原机，不跟随版本目录清理；新机使用独立持久目录。
    identity = service.phone_identity_directory(old) if old else recovery.safe_path(root, "var/lib/teslabox-system/carplay/phones")
    if identity.is_symlink() or identity.resolve() != identity.absolute():
        raise PermissionError("原机手机身份目录不安全")
    if identity.exists():
        info = identity.stat()
        if not identity.is_dir() or info.st_uid != os.geteuid() or info.st_mode & 0o077:
            raise PermissionError("原机手机身份目录权限不安全")
        # 校验登记映射但不创建、修改身份；密钥匹配由接收端原有严格校验处理。
        load_module(runtime, "product_phones", "carplay_phone_selection.py").read_registry(identity)
    hardware_path = recovery.safe_path(root, recovery.TARGETS["hardware"])
    hardware = dict(name="TeslaBox CarPlay", adapter="hci0", rfcomm_channel=4, oracle_listen="127.0.0.1:5290",
                    mfi=dict(transport="ch341", i2c_device="/dev/i2c-5", address=None, usb_bus=None, usb_address=None),
                    wifi=dict(interface="ap0", ssid="TeslaBox", passphrase="replaced-by-network-lease", channel=149))
    # 保留用户原硬件选择，但热点密码不从实验配置复制，由统一租约在每次会话填入。
    if old:
        original_hardware = recovery.read_file(Path(old["hardware_config"]), private=True)
        if original_hardware is None:
            raise ValueError("原硬件配置缺失，拒绝猜测迁移")
        source = json.loads(original_hardware)
        if not isinstance(source, dict) or not isinstance(source.get("mfi"), dict):
            raise ValueError("原硬件配置不合法")
        hardware["mfi"] = source["mfi"]
    value = dict(root=str(runtime), hardware_config=str(hardware_path),
                 binary_sha256=recovery.digest_file(runtime / "teslabox-carplay"),
                 node_sha256=recovery.digest_file(runtime / "runtime/bin/node"),
                 vehicle_channels=old.get("vehicle_channels", [1, 6, 11, 36, 40, 44, 48, 149, 153, 157, 161, 165]),
                 identity_directory=str(identity),
                 user_settings=old.get("user_settings", str(recovery.safe_path(root, "var/lib/teslabox-system/carplay-settings.json"))),
                 authentication=dict(mode="auto", credentials_manifest=str(recovery.safe_path(root, "var/lib/teslabox-system/carplay/mfi/active.json"))))
    # 只继承此台设备已有绑定；新设备不预设手机 IP、蓝牙地址、AP UUID 或开发板证书。
    for key in ("phone_bt", "certificate_sha256", "ap_interface", "upstream_interface"):
        if key in old:
            value[key] = old[key]
    return value, hardware, identity


def activate(root, runtime, main_sha256, runtime_sha256, template, command=recovery.systemctl, state=service_state):
    """先装旧助手也会执行的恢复钩子，再修改任何 CarPlay 组件。"""
    runtime = Path(runtime)
    root = Path(root)
    main = recovery.safe_path(root, "usr/local/bin/teslabox")
    if recovery.digest_file(main) != main_sha256 or runtime.resolve() != runtime:
        raise ValueError("候选主程序或版本目录发生变化")
    with recovery.locked(root):
        old_transaction = recovery.load(root)
        if old_transaction and old_transaction["phase"] != "rolled_back":
            if old_transaction["phase"] == "activated" and old_transaction["main_sha256"] == main_sha256 and old_transaction["runtime_sha256"] == runtime_sha256:
                return False
            if old_transaction["phase"] == "prepared" or old_transaction["main_sha256"] != main_sha256 and not recovery.is_forward_update(root, old_transaction):
                recovery.restore(root, old_transaction, command)
        settings, hardware, identity = settings_for(root, runtime)
        hook = recovery.safe_path(root, RECOVERY_HOOK)
        recovery.atomic_write(hook, ("[Service]\nExecStartPre=/usr/bin/python3 -B " + RECOVERY_COMMAND + "\n").encode(), 0o644)
        command("daemon-reload")
        was_active, was_enabled = state("active"), state("enabled")
        if was_active and recovery.read_file(recovery.safe_path(root, recovery.TARGETS["unit"])) is None:
            raise ValueError("现有服务来源非受控 unit，需先审核，不能直接覆盖")
        journal = recovery.prepare(root, main_sha256, runtime_sha256, was_active, was_enabled)
        try:
            # 先加载有限停止窗口，旧实验 unit 的 120 秒不能耗尽旧助手的健康窗口。
            unit = template.replace(b"@RUNTIME@", str(runtime).encode())
            recovery.atomic_write(recovery.safe_path(root, recovery.TARGETS["unit"]), unit, 0o644)
            command("daemon-reload")
            command("stop", recovery.SERVICE)
            identity.mkdir(parents=True, mode=0o700, exist_ok=True)
            recovery.atomic_write(recovery.safe_path(root, recovery.TARGETS["hardware"]), encode(hardware))
            recovery.atomic_write(recovery.safe_path(root, recovery.TARGETS["settings"]), encode(settings))
            load_module(runtime, "product_validation", "carplay-service.py").load_settings(recovery.safe_path(root, recovery.TARGETS["settings"]))
            recovery.atomic_write(recovery.safe_path(root, recovery.TARGETS["main_override"]),
                                  b"[Service]\nEnvironment=TESLABOX_CARPLAY_SERVICE_SETTINGS=/var/lib/teslabox-system/carplay/service.json\n", 0o644)
            command("daemon-reload")
            command("enable", recovery.SERVICE)
            command("--no-block", "start", recovery.SERVICE)
            journal["phase"] = "activated"
            recovery.save(root, journal)
            return True
        except BaseException:
            recovery.restore(root, journal, command)
            raise


if __name__ == "__main__":
    parser = argparse.ArgumentParser(description="TeslaBox CarPlay 受控运行时安装")
    parser.add_argument("--runtime", required=True)
    parser.add_argument("--main-sha256", required=True)
    parser.add_argument("--runtime-sha256", required=True)
    arguments = parser.parse_args()
    if os.geteuid() != 0:
        raise PermissionError("系统安装需要 root")
    runtime = Path(arguments.runtime)
    if runtime != Path("/opt/teslabox/carplay/versions") / arguments.runtime_sha256 or not recovery.re.fullmatch("[a-f0-9]{64}", arguments.runtime_sha256):
        raise ValueError("公开版本目录不匹配")
    template = Path(__file__).with_name("teslabox-carplay-experiment.service.in").read_bytes()
    if (runtime / "systemd/teslabox-carplay-experiment.service").read_bytes() != template:
        raise ValueError("公开包 unit 必须匹配编译绑定模板")
    activate(Path("/"), runtime, arguments.main_sha256, arguments.runtime_sha256, template)
