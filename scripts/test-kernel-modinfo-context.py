"""用真实 kmod 28 读取合成 ELF 元数据；不加载模块、不安装内核。"""
import importlib.util
import os
from pathlib import Path
import platform
import struct
import subprocess
import tempfile

spec = importlib.util.spec_from_file_location("kernel_upgrade", Path(__file__).parents[1] / "deploy/scripts/teslabox-kernel-upgrade.py")
k = importlib.util.module_from_spec(spec)
spec.loader.exec_module(k)


def module_bytes():
    """只构造供解析器读取的 AArch64 节，不冒充可加载驱动。"""
    names = b"\0.shstrtab\0.modinfo\0"
    info = ("vermagic=" + k.RELEASE + " SMP mod_unload modversions aarch64\0").encode()
    table = (64 + len(names) + len(info) + 7) & ~7
    ident = b"\x7fELF\x02\x01\x01" + b"\0" * 9
    header = ident + struct.pack("<HHIQQQIHHHHHH", 1, 183, 1, 0, 0, table, 0, 64, 0, 0, 64, 3, 1)
    data = header + names + info
    data += b"\0" * (table - len(data))
    data += b"\0" * 64
    data += struct.pack("<IIQQQQIIQQ", 1, 3, 0, 0, 64, len(names), 0, 0, 1, 0)
    data += struct.pack("<IIQQQQIIQQ", 11, 1, 2, 0, 64 + len(names), len(info), 0, 0, 1, 0)
    return data


assert os.name == "posix", "此回归必须在 Linux 实际执行"
version = subprocess.check_output(["modinfo", "--version"], text=True)
print(version.strip(), flush=True)
assert version.splitlines()[0] == "kmod version 28", "要求 Bullseye 的 kmod 28，不以新版替代旧解析器"
with tempfile.TemporaryDirectory(prefix="teslabox-modinfo-") as directory:
    root = Path(directory)
    current = root / "lib/modules" / platform.release()
    current.mkdir(parents=True)
    (current / "modules.builtin").write_bytes(b"kernel/net/wireless/cfg80211.ko\nkernel/net/mac80211/mac80211.ko\n")
    (current / "modules.builtin.modinfo").write_bytes(b"cfg80211.file=net/wireless/cfg80211\0mac80211.file=net/mac80211/mac80211\0")
    (current / "modules.order").write_bytes(b"")
    subprocess.run(["depmod", "-b", str(root), "-a", platform.release()], check=True)
    assert (current / "modules.builtin.bin").is_file()
    stage = root / "stage"
    stage.mkdir()
    target = root / "lib/modules" / k.RELEASE
    assert not target.exists()
    for name in ("cfg80211", "mac80211"):
        module = stage / (name + ".ko")
        module.write_bytes(module_bytes())
        legacy = subprocess.check_output(["modinfo", "-b", str(root), "-F", "vermagic", str(module)], text=True)
        print(name + " legacy=" + repr(legacy), flush=True)
        # 发行版回补可能改变空输出的表现，但不能把内建记录当作目标 ELF 的 ABI。
        assert not legacy.split() or legacy.split()[0] != k.RELEASE
        def run(args):
            result = subprocess.check_output([args[0], "-b", str(root), *args[1:]], text=True)
            print("target=" + repr(result), flush=True)
            return result
        upgrade = k.KernelUpgrade({}, run=run, running=platform.release(), boot_id="isolated")
        upgrade.verify_module_abi(module)
        assert not target.exists()
print("PASS: two real kmod 28 reads; target module tree absent; no module load or boot write", flush=True)
