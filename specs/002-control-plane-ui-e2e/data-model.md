# Data Model: Control-Plane UI End-to-End Tests

**Feature**: `specs/002-control-plane-ui-e2e` | **Date**: 2026-09-27

This feature adds a test tier, so its "data model" is the set of artefacts a run
produces and the entities the test provider presents. **No entity or field
changes.** The control plane's own entities — user, session, audit row, cluster,
node, quote — are unchanged and are not redefined here; the suite observes them.

**The store changes engine, not shape.** The control plane's backing store moves
to PostgreSQL 16 (R-008). The same tables, columns, and rows are written; the
difference is the engine underneath. This is why the entity definitions below
are unchanged, and why the migration itself is listed as changed product code
in the plan rather than as a data-model item here.

What the database holds is worth stating because it is what the dashboard's
apparently stateless UI actually depends on: the browser stores nothing — no
`localStorage`, no `sessionStorage`, no `indexedDB`, no JS-readable cookie.
Sign-in writes a user row and a session row, and the `dawnbx_session` cookie
(`HttpOnly`, `Secure` under TLS, `SameSite=Strict`) is an opaque pointer to that
row. Audit rows, cluster records, and cluster credentials sealed with
AES-256-GCM live there too. Relocating the store therefore relocates auth state,
which is why the engine is a real decision rather than a preference.

## Entity: TestOutcome

The per-test instruction to the test provider. This is the mechanism behind
FR-019 and FR-020, and it is what makes the failure journeys reachable.

| Field | Type | Required | Rules |
|---|---|---|---|
| `cluster` | `succeed` \| `fail` \| `unreachable` | yes | The terminal behaviour a cluster reaches. Drives User Stories 1, 2, 3. |
| `failureReason` | string | when `cluster = fail` | Must be specific and operator-readable. FR-010 requires the interface to show a reason, so a generic string would make that test pass vacuously. |
| `providerAvailable` | boolean | default `true` | When `false`, the provider appears in the roster as unavailable. Covers FR-013. |
| `holdWorkers` | boolean | default `false` | When `true`, a node removal is refused for holding workloads. Covers FR-012. |
| `advanceAfter` | duration | default ~1s | How long before an in-flight cluster changes phase. See R-003: deliberately shorter than the dashboard's 5 s refetch so a phase is present when the UI next polls. |

**Validation**:
- `failureReason` MUST be non-empty when `cluster = fail`; the fixture rejects
  the combination at construction rather than letting a test assert nothing.
- `advanceAfter` MUST be positive. A zero or negative value would make
  progression non-observable and silently defeat Q1's answer.
- `cluster = unreachable` and `holdWorkers = true` are independent; both may hold.

**Relationship to product entities**: `TestOutcome` shapes what the test
provider reports through the existing `provider.Status` and
`provider.Capabilities` returns. It introduces no new field into any product
type, satisfying Principle III by construction.

## Entity: RunArtifact

One file on disk per recorded test, plus the run report.

| Field | Type | Rules |
|---|---|---|
| `path` | file path | Under `.e2e/` in the repository root (FR-025). |
| `testName` | string | Which test produced it. Attributability is required by FR-005. |
| `passed` | boolean | Decides retention under FR-023. |
| `retained` | boolean | Derived: locally always true; in CI, `passed ? false : true`. |
| `kind` | `video` \| `report` \| `trace` | The report is always retained; only video is subject to environment-dependent retention. |

**Retention rules** (FR-023, FR-024):
- Local run → every recording retained, pass or fail.
- CI run → recordings for failed tests retained; recordings for passed tests
  deleted.
- All retained artefacts are subject to a cap applied automatically (NFR-005).
  The cap's value is an implementation choice (see research open items).

**Invariants**:
- A retained recording MUST remain playable (FR-017).
- A recording MUST NOT contain the run's administrator credential, or any secret
  (FR-021).
- A green CI run MUST leave no recording artefact behind (NFR-007).

## Entity: TestRunState

The ephemeral per-run state the fixture holds while the suite is executing.

| Field | Type | Rules |
|---|---|---|
| `dataDir` | path | A container volume, or a temporary directory outside a container. Isolated per run so runs share no state (FR-002). |
| `databaseURL` | connection string | Points at the Compose `postgres` service, via the server's existing `--database-url`. The control plane waits on the database health check before starting. |
| `baseURL` | URL | The control plane's address inside the Compose network. |
| `credential` | secret | Read from `<dataDir>/server/admin.env` per R-007. Never logged, never asserted on, never written to a report. |
| `provider` | test provider | The injected implementation, configured per test by `TestOutcome`. |

**Invariants**:
- `dataDir` is removed when the run ends, successfully or not.
- `databaseURL` is a container-local address; no run may reach a developer's own
  PostgreSQL instance, and no credential is embedded in it (FR-016).
- `credential` never leaves the process that read it, except by being typed into
  the sign-in form — which is the point of FR-021.

## State transitions observed

The suite does not define these; it asserts that the product performs them.
Recorded here because they are what the tests wait for.

```text
(validating) → (bootstrapping) → (verifying) → (ready)
      │
      └────────────→ (failed)
```

Plus `deleting` during removal. Sources:
`internal/cluster/cluster.go:37-43` for phases, lines 26-29 for statuses.

**Timing note (R-003)**: the provider advances in ~1 s; the operator observes a
change no sooner than 5 s, because the dashboard refetches in-flight clusters
every 5 s (`web/src/pages/clusters.tsx:492,719,862`) and the orchestrator polls
at the same cadence. A test therefore waits for a *visible* state with a
bounded timeout; a timeout is a failure and never a pass.

## No data-model changes to the product

The suite adds no table, column, field, or route. `internal/api/openapi.yaml` is
unchanged (Principle III). The only production change is how a provider is
supplied in a test build, which is a wiring concern recorded in the contract,
not a data concern.
