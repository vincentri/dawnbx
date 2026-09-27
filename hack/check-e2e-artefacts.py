#!/usr/bin/env python3
"""Fail if a recorded e2e artefact has been committed.

The suite writes video into .e2e/, which is bind-mounted out of the container and
git-ignored. This check exists because a git-ignore rule is a convention and a
convention is one `git add -f` away from being wrong: a recording is a binary
blob that grows every run, and a repository carrying them is one nobody can clone.

It also catches the subtler version - a recording committed under a different
name, or the report directory - because it looks for the artefacts by what they
are rather than by where they were expected to be.
"""

from __future__ import annotations

import subprocess
import sys
from pathlib import Path

# Suffixes the suite produces. Anything tracked with one of these is a mistake.
ARTEFACT_SUFFIXES = (".webm", ".zip")
# The report is a directory of generated HTML; its name is distinctive enough.
ARTEFACT_NAMES = ("report", "playwright-report", "test-results")


def tracked(repo: Path) -> list[str]:
    """Every path git is tracking, one per line."""
    out = subprocess.run(
        ["git", "ls-files", "-z"],
        cwd=repo,
        capture_output=True,
        text=True,
        check=True,
    ).stdout
    return [p for p in out.split("\0") if p]


def offenders(paths: list[str]) -> list[str]:
    bad: list[str] = []
    for p in paths:
        name = Path(p).name
        if p.endswith(ARTEFACT_SUFFIXES):
            bad.append(p)
        elif name in ARTEFACT_NAMES and ("e2e" in p or "playwright" in p):
            bad.append(p)
    return bad


def main() -> int:
    repo = Path(__file__).resolve().parent.parent
    bad = offenders(tracked(repo))
    if not bad:
        return 0
    print(
        f"FAILED: {len(bad)} committed e2e recording(s). These are run output, "
        "not source:",
        file=sys.stderr,
    )
    for p in bad[:20]:
        print(f"  {p}", file=sys.stderr)
    if len(bad) > 20:
        print(f"  ... and {len(bad) - 20} more", file=sys.stderr)
    print(
        "\nDelete them and confirm .e2e/ is ignored: git rm --cached <path>",
        file=sys.stderr,
    )
    return 1


if __name__ == "__main__":
    raise SystemExit(main())
