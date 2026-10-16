"""用真实公开 RC1 政策验证候选助手；只读原件，在临时目录测试篡改。"""
import json
from pathlib import Path
import shutil
import subprocess
import sys
import tempfile

helper, original = map(Path, sys.argv[1:])
def verify(path):
    return subprocess.run([str(helper), "--kernel-manifest", str(path)], capture_output=True, text=True, timeout=10)

result = verify(original)
assert result.returncode == 0, result.stderr
assert json.loads(result.stdout)["main_sha256"] == "5ee5385c2aa82bd7106ea4f861eee947db4379f749fda4d945959bae5ad6a48a"
with tempfile.TemporaryDirectory() as directory:
    target = Path(directory) / "policy.json"
    shutil.copyfile(original, target)
    assert verify(target).returncode != 0, "缺少签名不能通过"
    shutil.copyfile(Path(str(original) + ".sig"), Path(str(target) + ".sig"))
    assert verify(target).returncode == 0
    with target.open("ab") as stream:
        stream.write(b" ")
    assert verify(target).returncode != 0, "政策任何字节改变均不得通过"
print("Real RC1 policy signature, missing-signature rejection, tamper rejection: PASS")
