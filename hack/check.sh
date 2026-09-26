#!/usr/bin/env bash
# Pre-push gate: lint, build, test, and coverage across all four areas.
# Every step is reported, so one run shows every failure, not just the first.
#
#   bash hack/check.sh
#
# A fresh worktree needs its dependencies first:
#   npm ci --prefix web && npm ci --prefix sdk/typescript && npm ci --prefix docs
#   python3 -m venv .venv && .venv/bin/pip install coverage
#
# Coverage floor is 95% everywhere, per AGENTS.md. Today that floor is not met:
# see the numbers this prints.
set -uo pipefail
cd "$(git rev-parse --show-toplevel)" || exit 2

MIN=95
fail=0
run() { printf '\n== %s\n' "$1"; shift; "$@" || { echo "FAILED: $*"; fail=1; }; }

for tool in go node npm python3 staticcheck golangci-lint ruff; do
  command -v "$tool" >/dev/null || { echo "missing $tool in PATH"; exit 2; }
done
for d in web sdk/typescript docs; do
  [ -d "$d/node_modules" ] || { echo "missing $d/node_modules — run: npm ci --prefix $d"; exit 2; }
done
[ -x .venv/bin/python ] || {
  echo "missing .venv for python coverage — run: python3 -m venv .venv && .venv/bin/pip install coverage"
  exit 2
}

atleast() { awk -v v="$1" -v m="$2" 'BEGIN{exit !(v+0 >= m+0)}'; }

# ---- lint -------------------------------------------------------------------
unformatted=$(gofmt -l cmd internal)
[ -z "$unformatted" ] || { echo "gofmt needed:"; echo "$unformatted"; fail=1; }
run "staticcheck" staticcheck ./...
run "golangci-lint" golangci-lint run ./...
run "biome (web)" bash -c 'cd web && ./node_modules/.bin/biome check src'
run "eslint (ts sdk)" bash -c 'cd sdk/typescript && npx eslint .'
run "prettier (ts sdk)" bash -c 'cd sdk/typescript && npx prettier --check .'
run "ruff check" ruff check sdk/python
run "ruff format" ruff format --check sdk/python

# ---- build ------------------------------------------------------------------
run "go build" go build ./...
run "web build" npm run build --prefix web

# ---- test -------------------------------------------------------------------
run "go test" go test ./...
run "web test" bash -c 'cd web && npx vitest run'
run "ts sdk test" npm test --prefix sdk/typescript
run "py sdk test" bash -c 'cd sdk/python && ../../.venv/bin/python -m unittest discover -s tests'

# ---- coverage (95% floor) ---------------------------------------------------
printf '\n== go coverage\n'
if go test -coverprofile=/tmp/dawnbx-cov.out ./... >/dev/null 2>&1; then
  total=$(go tool cover -func=/tmp/dawnbx-cov.out | awk '/^total:/ {gsub("%","",$3); print $3}')
  echo "total ${total}% (floor ${MIN}%)"
  atleast "$total" "$MIN" || { echo "FAILED: go coverage ${total}% < ${MIN}%"; fail=1; }
else
  echo "FAILED: go test -coverprofile"; fail=1
fi

printf '\n== web coverage\n'
(cd web && npx vitest run --coverage >/dev/null 2>&1) || {
  echo "FAILED: web coverage below ${MIN}% (thresholds live in web/vitest.config.ts)"; fail=1; }

printf '\n== ts sdk coverage\n'
(cd sdk/typescript && npx c8 --reporter=text --include=dist/index.js \
   --check-coverage --lines "$MIN" --functions "$MIN" --branches "$MIN" --statements "$MIN" \
   node --test test/sdk.test.mjs >/dev/null) || {
  echo "FAILED: ts sdk coverage below ${MIN}%"; fail=1; }

printf '\n== py sdk coverage\n'
(cd sdk/python && ../../.venv/bin/python -m coverage run -m unittest discover -s tests >/dev/null 2>&1 \
   && ../../.venv/bin/python -m coverage report --fail-under="$MIN" --include='src/*' >/dev/null) || {
  echo "FAILED: py sdk coverage below ${MIN}%"; fail=1; }

# ---- the dashboard bundle is committed and embedded -------------------------
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
