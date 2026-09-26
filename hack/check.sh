#!/usr/bin/env bash
# Pre-push gate. Runs everything the agent rules list as checks, in one command,
# and refuses the push if any of it fails. Every step is also reported, so one
# run shows every failure instead of only the first.
#
#   bash hack/check.sh
#
# A fresh worktree needs its node_modules first:
#   npm ci --prefix web && npm ci --prefix sdk/typescript && npm ci --prefix docs
#
# There is no eslint, prettier, or ruff in this repo. gofmt and tsc are the whole
# lint story; add a real linter deliberately, do not assume one is missing by
# accident.
set -uo pipefail
cd "$(git rev-parse --show-toplevel)" || exit 2

fail=0
run() { printf '\n== %s\n' "$1"; shift; "$@" || { echo "FAILED: $*"; fail=1; }; }

for tool in go node npm python3; do
  command -v "$tool" >/dev/null || { echo "missing $tool in PATH"; exit 2; }
done
for d in web sdk/typescript docs; do
  [ -d "$d/node_modules" ] || {
    echo "missing $d/node_modules — run: npm ci --prefix $d"
    exit 2
  }
done

# Lint
unformatted=$(gofmt -l cmd internal)
[ -z "$unformatted" ] || { echo "gofmt needed:"; echo "$unformatted"; fail=1; }
run "go vet" go vet ./...
run "web typecheck" npm run typecheck --prefix web
run "docs typecheck" npm run types:check --prefix docs

# Build
run "go build" go build ./...
run "web build" npm run build --prefix web

# Unit tests
run "go test" go test ./...
run "ts sdk" npm test --prefix sdk/typescript
run "py sdk" bash -c 'cd sdk/python && python3 -m unittest discover -s tests'

# The dashboard bundle is committed and embedded, so a build that changes it
# means the binary would ship a different UI than the one in the tree.
stale=$(git status --porcelain internal/api/ui)
[ -z "$stale" ] || {
  echo "internal/api/ui is stale — commit the rebuild:"
  echo "$stale"
  fail=1
}

run "agent rules cites" python3 hack/check-harness-cites.py

if [ "$fail" -ne 0 ]; then
  printf '\ncheck.sh: FAILED\n'
  exit 1
fi
printf '\ncheck.sh: all green\n'
