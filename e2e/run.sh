#!/usr/bin/env bash
# The supported way to run the suite.
#
# It starts from a fresh stack every time. That is deliberate and un-clever: an
# earlier version tried to clear the database in place and kept losing a race
# with the server it was preparing, so a run inherited state from the last one
# and could not reuse a cluster name - which reads as a product bug and is not
# one. A fresh volume has no such ordering to get wrong.
set -euo pipefail
cd "$(dirname "$0")/.."

COMPOSE=(docker compose -f e2e/docker-compose.yml)

# -v drops the volumes as well as the containers: a leftover cluster is exactly
# what makes a second run collide.
"${COMPOSE[@]}" down -v --remove-orphans >/dev/null 2>&1 || true
"${COMPOSE[@]}" up -d postgres server >/dev/null

for _ in $(seq 1 90); do
  if [ "$(docker inspect --format '{{.State.Health.Status}}' dawnbx-e2e-server-1 2>/dev/null)" = healthy ]; then
    break
  fi
  sleep 1
done

"${COMPOSE[@]}" run --rm driver "$@"
