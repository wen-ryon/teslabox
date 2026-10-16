"""只验证冻结 stock 助手不支持 recover，不执行任何实际恢复。"""
import hashlib
from pathlib import Path
import subprocess
import sys
import tempfile

helper = Path(sys.argv[1])
assert hashlib.sha256(helper.read_bytes()).hexdigest() == "81262bbe3a2f560d19639fb1b8a314daee668b624b078b772754e0edc7ac3722"
with tempfile.TemporaryDirectory() as directory:
    result = subprocess.run([str(helper), "--recover"], cwd=directory, capture_output=True, text=True, timeout=10)
    assert result.returncode == 2, result
    assert "flag provided but not defined" in result.stderr and "recover" in result.stderr, result.stderr
    assert not list(Path(directory).iterdir())
print("Frozen stock 0.1.31 helper rejects --recover before execution: PASS")
