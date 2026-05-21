#!/usr/bin/env python3
"""Block committed MCPB/native binary payloads under library/.

Published CLI and MCPB release assets must be built from source in GitHub
Actions. This guard catches PRs that add or modify prebuilt payloads in the
catalog tree before those files can be signed or released.
"""
from __future__ import annotations

import argparse
import subprocess
import sys
from pathlib import Path

REPO_ROOT = Path(__file__).resolve().parents[3]

BINARY_SUFFIXES = {
    ".mcpb",
    ".dylib",
    ".so",
    ".dll",
    ".exe",
}

MAGIC_PREFIXES = {
    b"\x7fELF": "ELF executable/shared object",
    b"MZ": "Windows PE executable",
    b"\xca\xfe\xba\xbe": "Mach-O universal binary",
    b"\xbe\xba\xfe\xca": "Mach-O universal binary",
    b"\xfe\xed\xfa\xce": "Mach-O executable",
    b"\xfe\xed\xfa\xcf": "Mach-O executable",
    b"\xce\xfa\xed\xfe": "Mach-O executable",
    b"\xcf\xfa\xed\xfe": "Mach-O executable",
}


def run(args: list[str]) -> str:
    return subprocess.check_output(args, cwd=REPO_ROOT, text=True)


def changed_library_paths(base_ref: str) -> list[Path]:
    diff = run(
        [
            "git",
            "diff",
            "--name-only",
            "--diff-filter=AMR",
            f"{base_ref}...HEAD",
            "--",
            "library/",
        ]
    )
    return [REPO_ROOT / line for line in diff.splitlines() if line]


def binary_kind(path: Path) -> str | None:
    if path.suffix.lower() in BINARY_SUFFIXES:
        return f"{path.suffix} artifact"
    try:
        prefix = path.read_bytes()[:4]
    except OSError:
        return None
    for magic, label in MAGIC_PREFIXES.items():
        if prefix.startswith(magic):
            return label
    return None


def main() -> int:
    parser = argparse.ArgumentParser()
    parser.add_argument(
        "--base-ref",
        default="origin/main",
        help="Base git ref to compare against; defaults to origin/main.",
    )
    args = parser.parse_args()

    problems: list[str] = []
    for path in changed_library_paths(args.base_ref):
        if not path.is_file():
            continue
        kind = binary_kind(path)
        if kind:
            rel = path.relative_to(REPO_ROOT)
            problems.append(
                f"::error file={rel}::Do not commit {kind} payloads under library/. "
                "MCPB and native binaries must be built from source in GitHub Actions before signing."
            )

    if problems:
        for problem in problems:
            print(problem)
        return 1

    print("No newly added or modified MCPB/native binary payloads under library/.")
    return 0


if __name__ == "__main__":
    sys.exit(main())
