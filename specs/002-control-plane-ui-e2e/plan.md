# Implementation Plan: Control-Plane UI End-to-End Tests

**Branch**: `002-control-plane-ui-e2e` | **Date**: 2026-09-27 | **Spec**: [spec.md](./spec.md)

**Input**: Feature specification from `specs/002-control-plane-ui-e2e/spec.md`

## Summary

A browser-driven end-to-end suite for the control-plane dashboard that runs with
no cloud account, no AI agent, and no human present. The whole environment —
PostgreSQL, the control-plane server, and the browser driver — runs in Docker
Compose, so a developer's machine is left untouched. Locally every recording is
kept; in CI only failures are kept, so a green run uploads nothing. The suite
joins the existing gate and replaces no existing check.

**PostgreSQL is the control plane's database.** It runs in a container, and the
control plane connects to it exactly as it would to any external PostgreSQL —
the same `--database-url` path an operator uses. This is a deliberate change of
the control plane's backing store away from SQLite, and it brings with it work
this feature must own: the PostgreSQL path has never successfully executed in
this repository (see R-008), so making it the control plane's store is a
migration with real risk, not a configuration change.

The suite proves the operator-facing interface and its orchestration. It is
explicitly **not** evidence that a cloud accepts what is sent, and FR-015
forbids reporting it as such — that remains the manual real-account run.

## Technical Context

**Language/Version**: TypeScript 5.x for the suite (the dashboard is
TypeScript); the server under test is Go 1.27. Node 24, matching CI.

**Primary Dependencies**: `@playwright/test` for browser driving and per-test
video; the existing `@vitest/coverage-v8` and Biome for the web package's own
unit tests remain unchanged; `pgx/v5` (already a dependency) for PostgreSQL. All
three run inside the container image. Playwright is a new dependency and is
confined to the e2e suite.

**Storage**: **PostgreSQL 16**, in a container. The control plane connects over
`--database-url`, the same external path an operator uses. SQLite remains
supported by the server and is still what a non-containerized run defaults to;
the control plane under this feature uses PostgreSQL. The server's own files
(`admin.env`, `api-keys.json`, the control-plane encryption key) live in a
container volume or a temporary data directory, as
`hack/smoke-control-plane.sh` already does. Recordings are files under `.e2e/`
in the repository root, git-ignored, and are bind-mounted out of the container
so a developer can play them.

**Testing**: `@playwright/test`. The suite is a separate runner from Vitest and
does not replace it.

**Target Platform**: Docker on the developer machine (macOS or Linux) and Linux
CI runners. Headless only; NFR-002 forbids requiring a display server.
Docker Compose is the only supported way to run the suite, so no Go toolchain,
Node install, or browser download touches the host.

**Project Type**: an existing Go server with an embedded web dashboard; this
feature adds a test tier, not a deployable service.

**Performance Goals**: full suite under 3 minutes on a standard CI runner
(NFR-001). Browser startup, server start, and per-test overhead are the
dominant costs.

**Constraints**:
- No credentials and no network route to a cloud (FR-016).
- Sign-in goes through the real form, so a test administrator is seeded per run
  (FR-021).
- A skipped test fails the run (FR-022).
- Retention differs by environment (FR-023): all recordings locally, failures
  only in CI.
- PostgreSQL must be reachable and migrated before the server starts; the
  container waits on a health check, never on a sleep.
- A developer's machine MUST be unchanged by a run except for `.e2e/`.

**Scale/Scope**: 10–15 tests covering three user stories, the control-plane
pages only. The sandbox-facing interface is out of scope.

## The binding constraint found during planning

The clarifications asked for lifecycle progression over "a short real delay" so
the interface visibly advances (Q1). Reading the code shows the delay is **not
the thing under test control** — the dashboard's own refetch interval is:

```
web/src/pages/clusters.tsx:492,719,862
  refetchInterval: in-flight ? 5000 : false
```

So the operator sees a new phase no sooner than 5 s after it is recorded, and
the orchestrator's own poll is also 5 s (`internal/cluster/provision.go:60`,
`PollEvery: 5 * time.Second`). A cluster crosses roughly four phases
(`validating → bootstrapping → verifying → ready`,
`internal/cluster/cluster.go:37-43`).

Consequences, both of which shape the design:

1. **The test provider should transition faster than the UI polls** — on the
   order of 1 s, not 5 s. A provider that matches the poll interval makes every
   phase observation a coin flip against the refetch timer, which is how a
   deterministic suite becomes a flaky one.
2. **Each phase a test wants to *observe* costs ~5 s of wall clock**, because
   that is the UI's refresh cadence, not the suite's. A test that watches three
   phases spends ~15 s. This is the real budget risk against NFR-001, not video
   encoding. Only User Story 1 needs to watch progression; User Stories 2 and 3
   assert on terminal states and cost nothing.

**This means NFR-001 is at risk and must be measured, not assumed.** The plan
therefore treats the 3-minute budget as an acceptance check during
implementation, with the escape hatch of asserting on the terminal state plus one
observed intermediate phase rather than every phase.

## Constitution Check

*GATE: Must pass before Phase 0 research. Re-check after Phase 1 design.*

| Principle | Gate | Status |
|---|---|---|
| I. The Gate Is the Definition of Done | The suite joins `hack/check.sh`; it removes nothing | **PASS** — FR-007 |
| I. Coverage floors are a ratchet | Suite adds no floor below the current one; no check deleted | **PASS** — no requirement to lower or skip |
| II. Isolated Work | Feature is developed in a worktree, deleted after merge | **PASS** — workflow, not product code |
| III. One Contract, Five Files | **No route is added or changed** | **PASS** — the suite consumes the existing contract; `openapi.yaml` untouched |
| IV. A Behaviour Change Ships With a Test | The change *is* a test tier; it asserts observable behaviour through rendered UI, not implementation shape | **PASS** — FR-003, FR-009 |
| V. No Speculative Infrastructure | Two new dependencies (Playwright, a PostgreSQL container). Both have a stated consumer. The real-account run is deferred, not bundled | **PASS**, justified below |
| VI. Only Real Infrastructure Proves Provisioning | The suite is a substitute and is scoped so it never stands in for a real run | **PASS** — FR-015 and Out of Scope; Principle VI's own wording permits a substitute as long as it is not represented as the real thing |
| VI. (new exposure) | The control plane now runs on an engine that has never successfully executed here. A green suite is the *first* evidence the engine works at all, so it cannot also be the evidence that provisioning works | **PASS with a stated limit** — see R-008 and the Complexity note. A live control-plane run on PostgreSQL is required before release, and the plan says so |

**Complexity Tracking**

| Violation | Why Needed | Simpler Alternative Rejected Because |
|---|---|---|
| New dependency: `@playwright/test` (Principle V) | No browser-driving or video-recording tool exists in the repo; hand-rolling a browser driver is not credible | The suite requires a real browser (FR-003) and a video per test (FR-005). Neither is satisfiable with what is installed. The dependency is scoped to the suite and adds nothing to the shipped product. |
| **The control plane's database moves to PostgreSQL** (Principle V: a product change inside a test-tier feature) | The operator requires PostgreSQL for the control-plane UI. The server already speaks it through `--database-url`, so the cost is the migration, not the driver | Keeping SQLite would satisfy the letter of this spec — the suite would still pass — while shipping a control plane on an engine the operator has rejected. Principle V forbids a second store where one was chosen. The change is admitted deliberately, and its cost is paid in full below. |
| Test coverage of an engine that has never run (Principle VI) | The suite is the first thing that will ever execute PostgreSQL successfully | Accepting this means the suite's green run is doing double duty: proving the UI works *and* proving the engine works. That is a weaker signal for the second claim, and the plan does not pretend otherwise — a live control-plane run on PostgreSQL is a release requirement, not a follow-up. |

**Compliance judgement (not mechanical, per the constitution's own wording):**
the gate cannot verify that the suite drives the interface rather than the API.
That is verified by review — the tests must contain no direct HTTP calls to the
server, and no test may assert on internal data structures (FR-009). This is
recorded here because the constitution requires the judgement half to be stated
rather than assumed.

## Project Structure

### Documentation (this feature)

```text
specs/002-control-plane-ui-e2e/
├── plan.md              # This file (/speckit.plan command output)
├── research.md          # Phase 0 output (/speckit.plan command)
├── data-model.md        # Phase 1 output (/speckit.plan command)
├── quickstart.md        # Phase 1 output (/speckit.plan command)
├── contracts/           # Phase 1 output (/speckit.plan command)
└── tasks.md             # Phase 2 output (/speckit.tasks command - NOT created by /speckit.plan)
```

### Source Code (repository root)

```text
e2e/
├── docker-compose.yml       # postgres + server + driver; the supported way to run
├── Dockerfile               # server + Playwright image; nothing installs on the host
├── playwright.config.ts     # baseURL, video settings, outputDir, per-project env
├── fixtures/
│   ├── control-plane.ts     # waits for postgres, starts the server, reads admin.env
│   └── test-provider.ts     # provider that yields a per-test outcome on a fast timer
├── specs/
│   ├── sign-in.spec.ts      # FR-021
│   ├── cluster-request.spec.ts   # P1: quote, confirm, progression, ready
│   ├── failure.spec.ts      # P2: failed cluster, unreachable vs no workers
│   └── workers.spec.ts      # P3: add, refuse-stranded, remove, delete
└── support/
    └── retention.ts         # FR-023: keep-all locally, failures-only in CI

.e2e/                        # recordings + report; git-ignored (FR-025);
                             # bind-mounted out of the container (R-009)
```

**Structure Decision**: a top-level `e2e/` package, sibling to `web/`, `sdk/`
and `docs/`. It is deliberately **not** inside `web/`: the suite tests the Go
server's served dashboard as a whole, not the web package's components, and
keeping it out of `web/` means it does not inherit that package's Vitest
configuration, coverage thresholds, or Biome rules — which would otherwise
count the suite itself in the dashboard's coverage number.

**Everything runs in Compose** (R-009), with three services:

- **postgres** — PostgreSQL 16, a named volume, a health check. The control plane
  waits on that health check, never on a sleep.
- **server** — `dawnbx-server --control-plane`, pointed at postgres via
  `--database-url`, its own files on a volume.
- **driver** — the Playwright image, running the suite against `server`.

The developer's machine gains no Go toolchain requirement, no browser download,
and no `node_modules`. The only thing that reaches the host is `.e2e/`, which is
bind-mounted out because the recordings are the point (R-009).

The test provider is injected via `serverDeps.newProvider`, the existing
injection point at `cmd/dawnbx-server/main.go:116` (`productionDeps`), which the
Go tests already use — so the suite needs **no new production code path** beyond
a build tag that selects the test provider.

### Files that will change outside `e2e/`

```text
.gitignore                   # add .e2e/ (FR-025)
hack/check.sh                # add the e2e step alongside existing ones (FR-007)
.github/workflows/ci.yml     # run the suite in Compose; no browser install (FR-007, FR-026)
cmd/dawnbx-server/main.go    # expose the test provider under a build tag only
internal/auth/auth.go        # engine-aware migration (R-008) — SQLite pragmas have
                             # no PostgreSQL equivalent and migrate() has never run
                             # against PostgreSQL
deploy/                      # the compose file's home if kept out of e2e/
e2e/docker-compose.yml       # postgres + server + driver
e2e/Dockerfile               # server + Playwright image
go.mod                       # no change expected — pgx/v5 and the seam both exist
internal/api/openapi.yaml    # UNCHANGED — Principle III
```

The `main.go` change must be behind a build tag so the shipped binary cannot
select a test provider. That constraint is a security requirement, not a
preference, and is recorded in the contract.

## Complexity Tracking

> One justified exception is recorded in the Constitution Check above: the
> Playwright dependency under Principle V. No further violations.
