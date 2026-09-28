#!/usr/bin/env python3
"""打包 android-sshd Magisk 模块为可刷入 zip，并正确写入 unix 权限位。

用法:
  python make_zip.py                 # 读取 module.prop 的 version，输出到 dist/
  python make_zip.py --out <path>    # 指定输出 zip 路径

被 GitHub Actions release 流水线复用；本地开发亦可单独运行。
"""
import argparse
import os
import re
import sys
import zipfile

ROOT = os.path.dirname(os.path.abspath(__file__))
SRC = os.path.join(ROOT, "module", "android-sshd")

# 目标权限：安装脚本与守护进程需可执行；配置文件 0644。
MODES = {
    "META-INF/com/google/android/update-binary": 0o755,
    "META-INF/com/google/android/updater-script": 0o644,
    "customize.sh": 0o644,
    "service.sh": 0o755,
    "module.prop": 0o644,
    "bin/sshd-server": 0o755,
}


def read_version(module_prop):
    """从 module.prop 读取 version=，并做文件名安全化。"""
    with open(module_prop, encoding="utf-8") as f:
        for line in f:
            m = re.match(r"^version=(.+?)\s*$", line.strip())
            if m:
                return re.sub(r"[^0-9A-Za-z._-]", "-", m.group(1))
    sys.exit("module.prop 缺少 version= 字段")


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--out", help="输出 zip 路径（默认 dist/android-sshd-<version>.zip）")
    args = ap.parse_args()

    if not os.path.isdir(SRC):
        sys.exit(f"源目录不存在: {SRC}")
    ver = read_version(os.path.join(SRC, "module.prop"))
    out = args.out or os.path.join(ROOT, "dist", f"android-sshd-{ver}.zip")
    os.makedirs(os.path.dirname(out), exist_ok=True)
    if os.path.exists(out):
        os.remove(out)

    with zipfile.ZipFile(out, "w", zipfile.ZIP_DEFLATED) as zf:
        for name, mode in MODES.items():
            path = os.path.join(SRC, *name.split("/"))
            if not os.path.isfile(path):
                sys.exit(f"缺少文件: {path}")
            zi = zipfile.ZipInfo(name, date_time=(2026, 1, 1, 0, 0, 0))
            zi.compress_type = zipfile.ZIP_DEFLATED
            zi.external_attr = (mode & 0xFFFF) << 16  # unix 权限位
            with open(path, "rb") as f:
                zf.writestr(zi, f.read())
            print(f"added {name}  mode={oct(mode)}  {os.path.getsize(path)} bytes")

    print(f"\nOK -> {out}  ({os.path.getsize(out)} bytes)")


if __name__ == "__main__":
    main()
