#!/usr/bin/env python3
"""Fail a commit when the agent rules cite a file that no longer exists.

The generic harness-eval Track A checker only resolves cites that start with a
known directory prefix, so a bare filename like `textarea.tsx` slips through
even after the file is deleted. This checks the other half: bare filenames,
globs, and directory cites. That is the defect class that actually shipped once.

Stdlib only, no config. Run via .githooks/pre-commit, or by hand:

    python3 hack/check-harness-cites.py

Exits 1 with one line per stale cite. A cite passes when it is tracked, exists
on disk, is gitignored (build output such as docs/out/), or is a suffix of some
tracked path (`src/` -> sdk/python/src/...).
"""

import re
import subprocess
import sys
from pathlib import Path

# Files that live on a host at runtime, never in the repo. A harness mention of
# one of these is correct; a checker that flags them teaches people to ignore it.
RUNTIME_FILES = {"api-keys.json", "install.json"}

# Only these extensions count as file mentions. Everything else in backticks is
# a flag, env var, identifier, route, or prose.
EXTS = (".tsx", ".ts", ".jsx", ".mjs", ".cjs", ".go", ".py", ".sh", ".md",
        ".mdx", ".yaml", ".yml", ".json", ".css", ".html", ".toml")

RULE_FILES = ["AGENTS.md", "CLAUDE.md", ".cursorrules"]

CODE = re.compile(r"`([^`\n]+)`")
FENCE = re.compile(r"```.*?```", re.S)

SKIP_HEAD = ("/", "~", "$", "<", "@", "-", "+", "#")
SKIP_WORD = ("npm", "go ", "cd ", "git ", "kill ", "sudo", "bash", "python",
             "http", "class=", "output:", "ttl", "timeout", "base")


def git(*args):
    return subprocess.run(("git",) + args, capture_output=True,
                          text=True).stdout


def ignored(probe):
    # A gitignore pattern like `node_modules/` only matches a directory, and git
    # cannot tell a path is a directory when it does not exist, so try both.
    return bool(git("check-ignore", probe) or git("check-ignore", probe + "/"))


def main():
    root = Path(git("rev-parse", "--show-toplevel").strip() or ".")
    tracked = [p for p in git("ls-files").split("\n") if p]
    by_name = {}
    for p in tracked:
        by_name.setdefault(Path(p).name, p)

    problems = []
    for rel in RULE_FILES:
        path = root / rel
        if not path.is_file():
            continue
        # Mask fenced blocks so teaching examples are skipped, keeping the line
        # numbers of everything else intact.
        text = FENCE.sub(lambda m: "\n" * m.group(0).count("\n"),
                         path.read_text())
        for n, line in enumerate(text.split("\n"), 1):
            for token in CODE.findall(line):
                why = stale(token, root, tracked, by_name)
                if why:
                    problems.append(f"{rel}:{n}  {token}  {why}")

    for p in problems:
        print(p, file=sys.stderr)
    if problems:
        print(f"\n{len(problems)} stale cite(s) in the agent rules. Fix the cite, "
              f"drop the sentence, or — if the name is a runtime file — add it to "
              f"RUNTIME_FILES.", file=sys.stderr)
        return 1
    return 0


def stale(token, root, tracked, by_name):
    t = token.strip()
    if (t.startswith(SKIP_HEAD) or " " in t or ":" in t
            or t.startswith(SKIP_WORD)):
        return None

    if t.endswith("/"):
        probe = t.rstrip("/")
        if not probe or probe.startswith("."):
            return None
        if any(p == probe or p.startswith(probe + "/") for p in tracked):
            return None
        if (root / probe).is_dir():
            return None
        if any(f"/{probe}/" in "/" + p for p in tracked):
            return None  # a directory cite such as src/
        if ignored(probe):
            return None  # build output, e.g. docs/out/
        return "no such directory in the repo"

    if "/" in t:
        probe = t.rstrip("/")
        if any(p == probe or p.endswith("/" + probe) for p in tracked):
            return None
        if (root / probe).exists():
            return None
        if not probe.startswith(".") and any(
                p.endswith("/" + probe) for p in tracked):
            return None  # a suffix cite such as guide/api.mdx
        if ignored(probe):
            return None
        return "no such path in the repo"

    name = Path(t).name
    if name in RUNTIME_FILES:
        return None
    if "*" in name:  # ahead of the extension test: Tooltip* has no extension
        head = name.split("*")[0]
        if any(n.startswith(head) for n in by_name):
            return None
        return "no tracked file matches this glob"
    if not t.endswith(EXTS):
        return None
    if name in by_name:
        return None
    return "no tracked file with this name"


if __name__ == "__main__":
    sys.exit(main())
