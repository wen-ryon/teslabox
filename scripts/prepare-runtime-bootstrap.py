#!/usr/bin/env python3
"""从已批准媒体输入保留录像必需工具，不重新编译或修改归档。"""
import argparse
import hashlib
from pathlib import Path
import tarfile

FSCK_SHA256 = "c54d8c81ca1c2503474eba65c6224ec5b75aa104ac79053a938a39e4b639cd85"
FSCK_MEMBER = "opt/teslabox/media/bin/teslabox-fsck.exfat"

def prepare(media, digest, output):
    """先核对完整归档与工具摘要，确认成功后才写入首装暂存目录。"""
    source = Path(media)
    with source.open("rb") as handle:
        if hashlib.file_digest(handle, "sha256").hexdigest() != digest:
            raise ValueError("Media input digest changed")
    with tarfile.open(source, "r:gz") as archive:
        matches = [m for m in archive.getmembers() if m.name.removeprefix("./") == FSCK_MEMBER]
        if len(matches) != 1 or not matches[0].isfile() or matches[0].size != 250272:
            raise ValueError("Resident fsck member missing or invalid")
        with archive.extractfile(matches[0]) as handle:
            data = handle.read(250273)
    if hashlib.sha256(data).hexdigest() != FSCK_SHA256 or data[:6] != b"\x7fELF\x02\x01" or data[18:20] != b"\xb7\x00":
        raise ValueError("Resident fsck bytes differ from approved ARM64 input")
    target = Path(output)
    with target.open("xb") as handle:
        handle.write(data)
    target.chmod(0o755)

if __name__ == "__main__":
    parser = argparse.ArgumentParser()
    parser.add_argument("--media", required=True)
    parser.add_argument("--sha256", required=True)
    parser.add_argument("--output", required=True)
    args = parser.parse_args()
    prepare(args.media, args.sha256, args.output)
