#!/usr/bin/env python3
"""Fail when the two /v1 route tables and the OpenAPI contract disagree.

README.md and docs/content/docs/guide/api.mdx each carry a table of routes, and
internal/api/openapi.yaml is the contract. All three are hand-maintained and
nothing kept them equal, which is not a hypothetical: the two tables had drifted
in four places, and README documented GET /v1/sandboxes/{id}/terminal, which the
Go server serves but the contract never declared.

The tables are deliberately different in shape - one is a flat quick reference,
the other is sectioned and longer - so this does not try to make them identical.
It checks the thing that breaks a caller:

  1. the same set of routes is named in both
  2. every route either names is declared in the contract
  3. every route the contract declares is named in at least one of them

(1) catches a route added to one table. (2) catches a route documented after it
was removed, or never declared. (3) catches a route added to the contract and
forgotten in the docs. The descriptions are prose and are deliberately left
alone; only the routes are checked.
"""

import re
import sys
from pathlib import Path

ROOT = Path(__file__).resolve().parent.parent

README = ROOT / "README.md"
GUIDE = ROOT / "docs/content/docs/guide/api.mdx"
OPENAPI = ROOT / "internal/api/openapi.yaml"


FENCE = re.compile(r"```.*?```", re.S)


# A method, a full /v1 path, or a backticked sibling written after a comma.
# The sibling form is anchored on the comma and the backtick so that a slash in
# prose - "under /workspace" - is not read as a path.
TOKEN = re.compile(
    r"\b(GET|POST|PUT|DELETE)\b"
    r"|(/v1/[A-Za-z0-9_{}/.?=-]+)"
    r"|,\s*`(/[A-Za-z0-9_{}/.?=-]+)`",
    re.I,
)


def cell_routes(cell):
    """Every (method, path) in one table cell.

    A cell is read as a stream rather than as method-then-path pairs, because
    the tables write several methods against one path - `GET` / `POST
    /v1/clusters` - and a path with no method of its own. Each path takes every
    method named since the previous path, so that cell yields both.

    A trailing `, `/start`` is a second operation on the same path's parent,
    which is how the sandbox rows spell /extend and /start.
    """
    out, pending, base, last = set(), [], None, "GET"
    for tok in TOKEN.finditer(cell):
        verb, full, sibling = tok.group(1), tok.group(2), tok.group(3)
        if verb:
            pending.append(verb.upper())
            continue
        if full:
            base = full.split("?")[0]
        else:
            # `, `/start`` is a second operation on the previous path's parent,
            # which is how the tables spell /extend and /start. It carries no
            # method of its own, so it takes the one the previous path used.
            base = (base or "").rsplit("/", 1)[0] + sibling
        for v in pending or [last]:
            out.add((v, norm(base)))
        if pending:
            last = pending[-1]
        pending = []
    return out


def routes_in(text):
    """(method, path) pairs named in a document, with {param} names normalised."""
    text = FENCE.sub("", text)
    found = set()
    for m in re.finditer(r"\|([^|\n]*)\|", text):
        found |= cell_routes(m.group(1))
    return found


def norm(path):
    path = path.strip().strip("`").split("?")[0]
    return re.sub(r"\{[^}]+\}", "{}", path)


def contract_operations():
    """(method, path) declared in openapi.yaml."""
    out, current = set(), None
    for line in OPENAPI.read_text().split("\n"):
        m = re.match(r"^  (/\S+):\s*$", line)
        if m:
            current = m.group(1)
            continue
        m = re.match(r"^    (get|post|put|delete):\s*$", line)
        if m and current:
            out.add((m.group(1).upper(), norm(current)))
    return out


def main():
    if not (README.exists() and GUIDE.exists() and OPENAPI.exists()):
        print("check-api-docs: a route table or the contract is missing")
        return 2

    readme, guide = routes_in(README.read_text()), routes_in(GUIDE.read_text())
    contract = contract_operations()
    problems = []

    only_readme = sorted(readme - guide)
    only_guide = sorted(guide - readme)
    if only_readme:
        problems.append("in README.md but not api.mdx: " + fmt(only_readme))
    if only_guide:
        problems.append("in api.mdx but not README.md: " + fmt(only_guide))

    undocumented = sorted((readme | guide) - contract)
    if undocumented:
        problems.append(
            "documented but not declared in internal/api/openapi.yaml (the server "
            "may not even serve them): " + fmt(undocumented)
        )

    unmentioned = sorted(contract - (readme | guide))
    if unmentioned:
        problems.append(
            "declared in openapi.yaml but in neither table: " + fmt(unmentioned)
        )

    if problems:
        print("check-api-docs: the route tables and the contract disagree\n")
        for p in problems:
            print("  - " + p)
        print("\nFix the tables and internal/api/openapi.yaml together; see "
              "AGENTS.md section 3.")
        return 1
    return 0


def fmt(pairs):
    return ", ".join(f"{m} {p}" for m, p in pairs)


if __name__ == "__main__":
    sys.exit(main())
