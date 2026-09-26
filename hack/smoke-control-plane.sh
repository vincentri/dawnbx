#!/usr/bin/env bash
# Smoke test for control-plane mode: the mode must start, serve the dashboard
# and the identity surface, and refuse the cluster-bound routes with the JSON
# envelope — with no k3s, no .dawnbx-volume marker, and no cloud credentials.
#
#   bash hack/smoke-control-plane.sh
#
# Everything here is observable from outside the process, which is the point:
# the claims being checked are "the control plane runs without a cluster" and
# "an unreachable cluster never looks like zero clusters".
set -uo pipefail
cd "$(git rev-parse --show-toplevel)" || exit 2

BIN=$(mktemp -d)/dawnbx-server
DIR=$(mktemp -d)/cp
LOG=$(mktemp)
PORT=${PORT:-$((18000 + RANDOM % 2000))}
cleanup() { kill "$pid" 2>/dev/null; wait "$pid" 2>/dev/null; rm -rf "$(dirname "$BIN")" "$DIR" "$LOG"; }
trap cleanup EXIT

go build -o "$BIN" ./cmd/dawnbx-server || { echo "build failed"; exit 2; }

"$BIN" --control-plane --data-dir "$DIR/data" --listen "127.0.0.1:$PORT" \
  --admin-password 'smoke-test-password' >"$LOG" 2>&1 &
pid=$!

base="http://127.0.0.1:$PORT"
for _ in $(seq 40); do
  curl -fsS -o /dev/null "$base/ui/" 2>/dev/null && break
  sleep 0.25
done

fail=0
check() { # name expected actual
  if [ "$2" = "$3" ]; then printf 'ok    %-46s %s\n' "$1" "$3"
  else printf 'FAIL  %-46s want %s, got %s\n' "$1" "$2" "$3"; fail=1; fi
}

# 1. It started at all, with no marker and no cluster.
check "data dir created without .dawnbx-volume" "0" "$(ls -a "$DIR/data" 2>/dev/null | grep -c dawnbx-volume)"
check "dashboard served" "200" "$(curl -s -o /dev/null -w '%{http_code}' "$base/ui/")"
check "version needs no auth" "200" "$(curl -s -o /dev/null -w '%{http_code}' "$base/v1/version")"

# 2. The cluster-bound routes refuse with the JSON envelope, not a plain 404 and
#    not a panic. An unreachable cluster must never read as an empty list.
#    They sit behind the same auth as every other route, so a session is needed
#    before the 503 is what a caller sees.
check "login" "200" "$(curl -s -o /dev/null -w '%{http_code}' -X POST -H 'X-Dawnbx: 1' \
  -H 'Content-Type: application/json' -d '{"username":"admin","password":"smoke-test-password"}' \
  -c "$DIR/cookies" "$base/v1/login")"
for path in /v1/sandboxes /v1/status /v1/nodes; do
  body=$(curl -s -b "$DIR/cookies" -H 'X-Dawnbx: 1' -w '|%{http_code}' "$base$path")
  check "$path" "503" "${body##*|}"
  case "$body" in
    *'"code":"cluster_unavailable"'*) printf 'ok    %-46s %s\n' "$path envelope" "cluster_unavailable" ;;
    *) printf 'FAIL  %-46s got: %s\n' "$path envelope" "$body"; fail=1 ;;
  esac
done

# 3. The identity surface still works: an API key can never manage a cluster.
check "clusters list, admin session" "200" "$(curl -s -o /dev/null -w '%{http_code}' \
  -b "$DIR/cookies" -H 'X-Dawnbx: 1' "$base/v1/clusters")"
# No cloud credentials are configured here, so the registry is empty and the
# control plane says so rather than refusing to start. That degradation is part
# of the design: a lapsed credential must not stop an operator seeing the clusters
# they already have.
prov=$(curl -s -b "$DIR/cookies" -H 'X-Dawnbx: 1' "$base/v1/providers")
check "providers answers with a providers array" "1" "$(printf '%s' "$prov" | grep -c '"providers"')"
check "an unavailable provider is refused, not empty-listed" "400" \
  "$(curl -s -o /dev/null -w '%{http_code}' -b "$DIR/cookies" -H 'X-Dawnbx: 1' \
     -H 'Content-Type: application/json' -d '{}' "$base/v1/providers/gcp/estimate")"

KEY=$(curl -s -X POST -H 'X-Dawnbx: 1' -b "$DIR/cookies" -H 'Content-Type: application/json' \
  -d '{"name":"smoke"}' "$base/v1/keys" | grep -o '"key":"[^"]*"' | head -1 | cut -d'"' -f4)
if [ -n "$KEY" ]; then
  check "an API key cannot manage clusters" "403" \
    "$(curl -s -o /dev/null -w '%{http_code}' -H "Authorization: Bearer $KEY" "$base/v1/clusters")"
else
  printf 'FAIL  %-46s could not mint a key\n' "api key minted for the refusal check"; fail=1
fi

# 4. The default path is unchanged: without --control-plane the marker is still
#    required, so an unmounted volume cannot read as "all sandboxes gone".
out=$("$BIN" --data-dir "$DIR/data" --listen "127.0.0.1:$((PORT+1))" 2>&1)
case "$out" in
  *"not mounted"*) printf 'ok    %-46s %s\n' "cluster mode still needs the marker" "refused" ;;
  *) printf 'FAIL  %-46s want a marker refusal, got: %s\n' "cluster mode needs the marker" "$out"; fail=1 ;;
esac

if [ "$fail" -ne 0 ]; then
  printf '\nserver log:\n'; sed -n '1,40p' "$LOG"
  echo "smoke: FAILED"
  exit 1
fi
echo "smoke: all green"
