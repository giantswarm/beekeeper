"""Lint whole changed packages, including unchanged production and test files.

Compare the working tree (committed, staged, unstaged and untracked files) with
the branch's merge base. This catches package-wide goconst collisions before
publication without repeating CI's full-repository checks on every round.
"""

import os
from pathlib import Path
import subprocess
import sys


def git(*args):
    return subprocess.check_output(["git", *args])


def main():
    root = os.fsdecode(git("rev-parse", "--show-toplevel").strip())
    os.chdir(root)
    base = sys.argv[1] if len(sys.argv) > 1 else "origin/main"
    merge_base = os.fsdecode(git("merge-base", "HEAD", base).strip())
    names = (
        git("diff", "--name-only", "--no-renames", "-z", merge_base)
        + git("ls-files", "--others", "--exclude-standard", "-z")
    )
    paths = {Path(os.fsdecode(name)) for name in names.split(b"\0") if name}
    # Dependency/configuration changes can affect every package.
    if paths.intersection(map(Path, (
        "go.mod", "go.sum", "go.work", "go.work.sum",
        ".golangci.yaml", ".golangci.yml", ".golangci.toml", ".golangci.json",
    ))):
        packages = ["./..."]
    else:
        directories = {path.parent for path in paths if path.suffix == ".go"}
        # A deleted file still selects its surviving package; a deleted package
        # has nothing left to lint.
        packages = sorted(
            "./" + str(directory)
            for directory in directories
            if any(directory.glob("*.go"))
        )
    if not packages:
        print("No changed Go packages to lint.")
        return 0
    print("Linting whole packages, tests included: " + " ".join(packages), flush=True)
    return subprocess.run([
        "golangci-lint", "run", "--concurrency=2", *packages,
    ], check=False).returncode


if __name__ == "__main__":
    try:
        sys.exit(main())
    except (OSError, subprocess.CalledProcessError) as error:
        print(f"Changed-package lint unavailable: {error}", file=sys.stderr)
        sys.exit(1)
