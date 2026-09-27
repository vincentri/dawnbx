# Quickstart: Control-Plane UI End-to-End Tests

**Feature**: `specs/002-control-plane-ui-e2e` | **Date**: 2026-09-27

How to run the suite and how to tell whether it is working. This is a
validation guide: it describes commands and expected outcomes, not
implementation.

---

## Prerequisites

**Docker, and Docker Compose. That is the whole list.**

| Requirement | Why |
|---|---|
| Docker + Compose | PostgreSQL, the control-plane server, and the browser driver all run in containers |

Nothing is installed on the host. No Go toolchain, no Node, no Playwright
browser download, no `node_modules` — all of it lives in the image. The only
thing written outside the container is `.e2e/`, the recordings.

The suite needs no account, no cluster, and no cloud. If a run asks for a
credential, something is wrong.

---

## Running it

### Through the gate (what CI does)

```bash
bash hack/check.sh
```

The suite runs as one step alongside the existing checks. A failure anywhere
fails the gate.

### On its own (faster loop while developing a test)

```bash
docker compose -f e2e/docker-compose.yml run --rm driver \
  npx playwright test --config e2e/playwright.config.ts
```

### One test, to watch it happen

```bash
docker compose -f e2e/docker-compose.yml run --rm driver \
  npx playwright test --config e2e/playwright.config.ts -g "sign in"
```

### Tearing the environment down

```bash
docker compose -f e2e/docker-compose.yml down -v
```

Removes the containers and the PostgreSQL volume. Your machine is back to
exactly its prior state.

### Rebuilding after a product change

```bash
docker compose -f e2e/docker-compose.yml build --no-cache
```

Needed after changing Go or dashboard code, because the image carries both.

---

## Where the recordings go

```text
.e2e/
├── <test-name>/
│   └── video.webm
└── report/
```

`.e2e/` is git-ignored (FR-025). It is safe to delete at any time.

**Locally**, every recording is kept, pass or fail — so you can watch a passing
run to confirm the journey actually happened.

**In CI**, only failed tests keep a recording. A green run leaves nothing
behind (NFR-007).

---

## What a healthy run looks like

```text
Running 12 tests using 1 worker

  ✓  1 sign-in.spec.ts:8   signs in and reaches the signed-in shell
  ✓  2 cluster-request.spec.ts:14  shows a price before creating a cluster
  ...
  12 passed (2m 41s)
```

A healthy run:

- takes **under 3 minutes** (NFR-001) — if it does not, see the budget note below
- reports **12 passed, 0 skipped** — a skip is a failure (FR-022)
- leaves a recording for every test locally
- creates no cloud resource and needs no credential

## What a failing run looks like

```text
  1 failed
    cluster-request.spec.ts:31:5 › confirms a quote and reaches ready
      Error: Timed out 15000ms waiting for text "ready"

  1 failed, 11 passed (2m 12s)
  video: .e2e/cluster-request/confirms-a-quote-and-reaches-ready/video.webm
```

Play the video. It shows what the operator was attempting, which is the point of
the feature (SC-003): the failure should be diagnosable by watching it, without
reading the test source.

---

## Verifying the requirements this feature exists to satisfy

These are the checks worth running by hand once, after implementation.

### Determinism (FR-002, SC-002)

```bash
for i in $(seq 1 20); do
  docker compose -f e2e/docker-compose.yml run --rm driver \
    npx playwright test --config e2e/playwright.config.ts || echo "run $i FAILED"
done
```

All 20 must agree. A run that differs from its neighbours is the flake this
suite is meant to make visible rather than hide.

### No credentials, no network (FR-016, SC-005)

```bash
docker compose -f e2e/docker-compose.yml run --rm -e AWS_ACCESS_KEY_ID= \
  -e AWS_SECRET_ACCESS_KEY= -e AWS_PROFILE= driver \
  npx playwright test --config e2e/playwright.config.ts
```

Must be green. If anything asks for a cloud, the fixture is wrong.

### Unattended (NFR-002)

```bash
CI=1 docker compose -f e2e/docker-compose.yml run --rm driver \
  npx playwright test --config e2e/playwright.config.ts
```

Must be green with no display and no prompt. This is what CI does.

### Retention differs by environment (FR-023)

```bash
docker compose -f e2e/docker-compose.yml run --rm driver \
  npx playwright test --config e2e/playwright.config.ts
find .e2e -name '*.webm' | wc -l     # every test has a recording

CI=1 docker compose -f e2e/docker-compose.yml run --rm driver \
  npx playwright test --config e2e/playwright.config.ts
find .e2e -name '*.webm' | wc -l     # 0 on an all-passing run
```

The recordings appear on the host even though the suite ran in a container —
`.e2e/` is bind-mounted out for exactly this reason (R-009).

### The shipped binary cannot select a test provider

```bash
go build ./cmd/dawnbx-server
strings dawnbx-server | grep -i 'testprovider'    # no match
```

A match means a test provider is reachable in a default build, which is a
security failure — see `contracts/test-provider.md`.

---

## The budget note

NFR-001 allows 3 minutes, and that budget is the feature's real risk.

Lifecycle progression is observable no sooner than the dashboard's own 5 s
refetch (`web/src/pages/clusters.tsx:492`), regardless of how fast the test
provider advances. A test that watches three phases therefore spends ~15 s
waiting on the interface, not on the fixture.

Only User Story 1 watches progression; Stories 2 and 3 assert terminal states
and cost almost nothing. If the suite overruns:

1. Measure first — which test is slow, and is it waiting on a phase or on
   something else?
2. If it is phase-waiting, assert the terminal state plus **one** observed
   intermediate phase rather than every phase. This is the documented fallback
   and it still satisfies FR-009's "advances through observable states".
3. Do **not** shorten the provider's advance time below the UI's poll — that
   reintroduces the race described in `research.md` R-003, and the suite becomes
   flaky rather than fast.

---

## Verifying the container really is isolated

```bash
docker compose -f e2e/docker-compose.yml run --rm driver \
  npx playwright test --config e2e/playwright.config.ts
ls node_modules/e2e 2>/dev/null && echo "LEAKED to host" || echo "clean"
ls ~/Library/Caches/ms-playwright >/dev/null 2>&1 && echo "browser on host" || echo "clean"
```

Both should report clean. The browsers and dependencies belong to the image, not
to your machine (R-009).

## What this suite does not prove

Two things, and conflating them is the trap this section exists to prevent.

**It does not prove a cloud accepts what is sent.** The suite runs against a
test provider. FR-015 forbids reporting it as evidence that provisioning works.
The real account lifecycle run remains the only thing that does that, and it
stays manual.

**It is also the first successful run of PostgreSQL in this repository** (R-008).
Every pre-existing `postgres://` test points at port 1, where nothing listens —
they assert clean error handling, not a working engine. So a green suite means
both "the operator-facing flows work" and "the database engine works", and one
signal covering two claims is weaker than two separate ones. Before any release,
a live control-plane run on PostgreSQL is required, using the real-account
lifecycle. That is a release gate, not a follow-up.
