#!/usr/bin/env bash
# Pre-push gate: lint, build, test, and coverage across all four areas.
# Every step is reported, so one run shows every failure, not just the first.
#
#   bash hack/check.sh
#
# The live tier is opt-in because it needs a Lima VM and touches real k3s:
#   CHECK_LIVE=1 bash hack/check.sh
# It (re)installs this checkout into a Lima VM and runs hack/verify.sh inside
# it as root, against real gVisor, real quotas and a real sandbox pod. That is
# the only coverage the k8s edges of internal/sandbox get; see the coverage
# floors in hack/coverage-floor.txt for why they are not unit tested.
#
# The control-plane smoke is a second opt-in tier, for the same reason:
#   SMOKE=1 bash hack/check.sh
# It builds dawnbx-server and runs it on a random port, so it belongs with the
# live tier rather than in the default gate. It proves the mode starts and
# serves; it is not a lint and not a per-change signal.
#
# A fresh worktree needs its dependencies first:
#   npm ci --prefix web && npm ci --prefix sdk/typescript && npm ci --prefix docs
#   python3 -m venv .venv && .venv/bin/pip install coverage
#
# Coverage floor is 95% everywhere, per AGENTS.md. Today that floor is not met:
# see the numbers this prints.
set -uo pipefail
cd "$(git rev-parse --show-toplevel)" || exit 2


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
# The one contract, linted. This is the generator the dashboard's types come
# from, run against the file itself: it fails on a YAML mistake, an unresolvable
# $ref, or an operation that cannot be typed, which is the class of drift the
# five hand-kept copies cannot catch. It writes to a temp file, never to the
# committed src/lib/schema.d.ts, so a stale bundle stays its own separate check
# below. CI runs this same script, so it is one step and not two.
run "openapi contract" bash -c '
  out=$(mktemp -d)/schema.d.ts
  cd web && ./node_modules/.bin/openapi-typescript ../internal/api/openapi.yaml -o "$out"
'

# ---- installer pins ---------------------------------------------------------
# A cluster that is not reproducible is not one an operator can reason about,
# and an unpinned dependency is the usual way that happens: `latest` resolves to
# something new under a machine that is already running. install.sh is the only
# place these are decided, and nothing else reads it, so the rule is asserted
# here rather than trusted to review.
run "installer pins its dependencies" bash -c '
  set -e
  if grep -qE "^(K3S_VERSION|GVISOR_RELEASE)=.*:-latest" install.sh; then
    echo "install.sh resolves a dependency from latest; pin it" >&2
    exit 1
  fi
  # And each pin has to name something. Both forms count: K3S_VERSION is a plain
  # assignment, GVISOR_RELEASE is overridable, and a step that only understood
  # one of them would report a pin that is there as missing.
  for v in K3S_VERSION GVISOR_RELEASE; do
    val=$(sed -n -e "s/^$v=\${$v:-\(.*\)}/\1/p" -e "s/^$v=\(.*\)/\1/p" install.sh | head -1)
    [ -n "$val" ] || { echo "install.sh: $v is not set at all" >&2; exit 1; }
    case $val in
      latest|"") echo "install.sh: $v resolves to $val" >&2; exit 1 ;;
    esac
    printf "  %-16s %s\n" "$v" "$val"
  done
'

# ---- build ------------------------------------------------------------------
run "go build" go build ./...
run "web build" npm run build --prefix web

# ---- test -------------------------------------------------------------------
run "go test" go test ./cmd/... ./internal/...
run "web test" bash -c 'cd web && npx vitest run'
run "ts sdk test" npm test --prefix sdk/typescript
run "py sdk test" bash -c 'cd sdk/python && ../../.venv/bin/python -m unittest discover -s tests'

# ---- coverage (floors in hack/coverage-floor.txt) ---------------------------
# Resolve every floor here, while the CWD is still the repo root: the steps
# below run inside per-area subshells that cannot see hack/.
floor() { awk -v k="$1" -F'[= ]' '$1 == k { print $2 }' hack/coverage-floor.txt; }
# All axes for an area, in the order statements branches functions lines.
floors() {
  awk -v k="$1" -F'[= ]' '$1 == k { for (i = 2; i <= NF; i++) if ($i != "-") printf "%s ", $i }' hack/coverage-floor.txt
}
ge() { awk -v v="$1" -v m="$2" 'BEGIN{exit !(v+0 >= m+0)}'; }
GO_FLOOR=$(floor go)
PY_FLOOR=$(floor py_sdk)
read -r WS WB WF WL <<<"$(floors web)"
read -r TS TB TF TL <<<"$(floors ts_sdk)"

printf '\n== go coverage\n'
if go test -coverprofile=/tmp/dawnbx-cov.out ./cmd/... ./internal/... >/dev/null 2>&1; then
  total=$(go tool cover -func=/tmp/dawnbx-cov.out | awk '/^total:/ {gsub("%","",$3); print $3}')
  echo "total ${total}% (floor ${GO_FLOOR}%)"
  ge "$total" "$GO_FLOOR" || { echo "FAILED: go coverage ${total}% < ${GO_FLOOR}%"; fail=1; }
else
  echo "FAILED: go test -coverprofile"; fail=1
fi

printf '\n== web coverage (src/lib)\n'
(cd web && npx vitest run --coverage \
   --coverage.thresholds.statements="$WS" --coverage.thresholds.branches="$WB" \
   --coverage.thresholds.functions="$WF" --coverage.thresholds.lines="$WL" \
   --coverage.thresholds.100=false >/dev/null 2>&1) || {
  echo "FAILED: web coverage below ${WS}/${WB}/${WF}/${WL}"; fail=1; }

printf '\n== ts sdk coverage\n'
(cd sdk/typescript && npx c8 --reporter=text --include=dist/index.js \
   --check-coverage --statements "$TS" --branches "$TB" --functions "$TF" --lines "$TL" \
   node --test test/sdk.test.mjs >/dev/null) || {
  echo "FAILED: ts sdk coverage below ${TS}/${TB}/${TF}/${TL}"; fail=1; }

printf '\n== py sdk coverage\n'
(cd sdk/python && ../../.venv/bin/python -m coverage run -m unittest discover -s tests >/dev/null 2>&1 \
   && ../../.venv/bin/python -m coverage report --fail-under="$PY_FLOOR" --include='src/*' >/dev/null) || {
  echo "FAILED: py sdk coverage below ${PY_FLOOR}%"; fail=1; }

# ---- the dashboard bundle is committed and embedded -------------------------
stale=$(git status --porcelain internal/api/ui)
[ -z "$stale" ] || {
  echo "internal/api/ui is stale — commit the rebuild:"
  echo "$stale"
  fail=1
}

run "agent rules cites" python3 hack/check-harness-cites.py

# ---- optional control-plane smoke --------------------------------------------
if [ "${SMOKE:-0}" = 1 ]; then
  run "control-plane smoke" bash hack/smoke-control-plane.sh
fi

# ---- optional live tier ------------------------------------------------------
if [ "${CHECK_LIVE:-0}" = 1 ]; then
  command -v limactl >/dev/null || { echo "CHECK_LIVE=1 needs limactl"; exit 2; }
  run "live: install into the Lima VM" bash hack/dev-vm.sh
  run "live: hack/verify.sh in the VM" bash -c 'limactl shell dawnbx -- sudo bash -c "cd /root && bash" < hack/verify.sh'
fi

if [ "$fail" -ne 0 ]; then
  printf '\ncheck.sh: FAILED\n'
  exit 1
fi
printf '\ncheck.sh: all green\n'
