# Phase 0 Research: Control-Plane UI End-to-End Tests

**Feature**: `specs/002-control-plane-ui-e2e` | **Date**: 2026-09-27

Every NEEDS CLARIFICATION in the Technical Context was resolved by reading the
repository rather than by choosing a default. Findings below cite the code that
settled them.

---

## R-001. How does the suite start a control plane with a test provider?

**Question**: NEEDS CLARIFICATION — is a production injection point already
available, or does this feature add one?

**Decision**: An injection point already exists and is used by the Go tests.
`serverDeps.newProvider func(ctx, cfg) (provider.Provider, error)` at
`cmd/dawnbx-server/main.go:116`, defaulted to `nil` at line 139 and consumed at
line 357. `wireCloud` fills it in per process. `TestWireCloudPrefersAnInjectedProvider`
(`control_plane_test.go:242`) already overrides it, which proves the seam works
and is the pattern to follow.

**Rationale**: The spec's assumption that "the product's provider seam already
accepts an injected implementation" is verified, not assumed. No new
architectural seam is needed — the suite supplies a provider to an existing hook.

**Alternatives considered**:
- *A new `provider.Provide` env var or CLI flag* — rejected. It would put a
  test-only code path into the shipped binary's reachable surface, and a
  mis-set variable in production would silently swap the cloud adapter.
- *A separate test-only `main`* — rejected. Duplicates the real startup path,
  so the suite would test a binary no user runs.

**Consequence**: the change to `main.go` is a build-tagged file, not a flag.
Build tags are not used anywhere in the repo today, so this is the first; the
plan records the constraint rather than assuming precedent.

---

## R-002. What makes the suite deterministic?

**Question**: NEEDS CLARIFICATION — clock, polling, and timing.

**Decision**: The orchestrator's clock is already injectable
(`Registry.SetClock`, `internal/cluster/cluster.go:139`) and the poll interval
is a field (`Provisioner.PollEvery`, `internal/cluster/provision.go:29`,
defaulting to 5 s at line 60). Both are settable in a test without touching
product behaviour.

**Rationale**: The determinism requirement (FR-002) is satisfiable without
sleeping on wall-clock time, because the two knobs that would otherwise force a
sleep are both injectable.

**Alternatives considered**:
- *Fixed `sleep(N)` in every test* — rejected. It is the flake source this
  feature exists to avoid; it encodes a guess about machine speed.
- *Waiting on observable state instead of time* — **chosen**, per the
  clarification that a test asserts on an operator-visible state rather than an
  elapsed duration. The registry's injectable clock supports measuring without
  waiting.

**Consequence**: tests poll for the expected visible state with a bounded
timeout. A timeout means failure, never a pass.

---

## R-003. Why does lifecycle progression cost ~5 s per observed phase?

**Question**: The clarification asked for progression over "a short real delay"
so the interface visibly advances. How long is that, in practice?

**Decision**: The delay the *test provider* uses should be ~1 s. The delay an
*operator observes* is ~5 s, and that is set by the dashboard, not the suite:
`refetchInterval: in-flight ? 5000 : false` at
`web/src/pages/clusters.tsx:492, 719, 862`. The orchestrator polls at the same
5 s cadence (`provision.go:60`).

**Rationale**: Two distinct timers exist, and conflating them is how a
deterministic suite becomes flaky. If the provider transitions at 5 s, every
phase observation races the UI's own refetch. Transitioning faster than the UI
polls makes the phase reliably *present* when the UI next looks, so a test that
waits for a visible state succeeds on the first poll rather than by luck.

A cluster crosses about four phases — `validating`, `bootstrapping`,
`verifying`, `ready` (`internal/cluster/cluster.go:37-43`).

**Alternatives considered**:
- *Provider at 5 s, matching the poll* — rejected: makes each observation a
  race.
- *Test provider at ~1 s and assert only terminal state* — rejected: loses the
  visible progression the clarification asked for.

**Consequence**: NFR-001's 3-minute budget is at risk, and the risk is
arithmetic, not guesswork — ~5 s per *observed* phase, times the tests that
observe phases. Only User Story 1 observes progression; Stories 2 and 3 assert
terminal states. The plan makes measuring this an acceptance check with a
documented fallback (assert terminal state plus one intermediate phase).

---

## R-004. How is the dashboard served, and must the suite build it?

**Question**: Does the e2e suite need a web build step?

**Decision**: No. The committed bundle at `internal/api/ui/` (2 entries:
`index.html`, `assets/`) is embedded and served by the Go binary, so
`go build ./cmd/dawnbx-server` produces a binary that serves the real
dashboard. The suite starts that binary.

**Rationale**: Principle III requires the committed bundle to match the sources,
and the gate already enforces staleness. Building from the committed bundle
means the suite tests what ships.

**Alternatives considered**:
- *Run a dev server for `web/`* — rejected: tests a different artefact than the
  one users get, and needs a second process plus a proxy for `/v1`.

**Consequence**: the suite's only prerequisite is a Go build. A change to
`web/` that forgot to rebuild the bundle is caught by the gate before the suite
ever runs.

---

## R-005. Where does the suite hook into the gate and CI?

**Question**: FR-007 says the suite joins the existing gate. Where exactly?

**Decision**: A new step in `hack/check.sh` (alongside the existing ones, never
replacing any), plus the browser install in `.github/workflows/ci.yml` before
the gate step. The CI job already runs `bash hack/check.sh` and already
installs `web`, `sdk/typescript`, and `docs` dependencies.

**Rationale**: One command locally and in CI satisfies NFR-004 and NFR-006.
Playwright's browser download is large, so it belongs in the workflow, not in
`check.sh` — `check.sh` must not silently fetch a browser.

**Alternatives considered**:
- *A separate CI job* — rejected: the suite is part of "the gate is the
  definition of done" (Principle I), and a separate job weakens that.
- *Browser install inside `check.sh`* — rejected: a local `npm ci` should not
  silently download a browser; it would make the gate's network behaviour
  surprising.

**Consequence**: `check.sh` invokes the suite; the workflow installs the browser
first. Locally, a developer who has not installed the browser gets a clear
message rather than a slow silent download.

---

## R-006. Where do recordings live, and how does retention differ?

**Question**: FR-025/FR-023 require a git-ignored directory and
environment-dependent retention. What is the mechanism?

**Decision**: Playwright's `outputDir` points at `.e2e/` in the repository
root, with `video: 'on'` and `recordVideo` retained per test. Retention is
applied after the run by `support/retention.ts`, which reads whether the run is
in CI: locally it keeps everything; in CI it deletes recordings for tests that
passed. `.e2e/` is added to `.gitignore`.

**Rationale**: Deleting after the run rather than configuring per-environment
video modes keeps one code path. A green CI run then produces no artefacts at
all (NFR-007), which is what makes it cheap enough to run on every push.

**Alternatives considered**:
- *Configure Playwright differently per environment* — rejected: two
  configurations drift, and the "green run uploads nothing" property becomes
  something to remember rather than something enforced.
- *Retain in CI with a cap* — rejected per the operator's decision; a passing
  recording has no reader in CI.

**Consequence**: retention is testable — a test asserts that a passing test's
recording exists locally and is absent in CI mode.

---

## R-007. How is the run's administrator established?

**Question**: FR-021 requires signing in through the real form with a known
administrator. How is that credential created?

**Decision**: The control plane already "generates an admin password once"
(`TestControlPlaneGeneratesAnAdminPasswordOnce`,
`control_plane_test.go:171`). The run reads the generated credential from the
isolated data directory it created, and the suite signs in through the form
with it. No credential is invented, and none is a real secret.

**Rationale**: This satisfies FR-021 without adding a seeding path to production
code, and it matches how an operator's first sign-in actually works.

**Alternatives considered**:
- *Seed a known password via flag or env* — rejected: adds a production input
  that overrides credential generation, which is a security-sensitive change for
  no test benefit.
- *Bypass sign-in* — rejected by FR-021 and by the clarification.

**Consequence**: the credential is read from the run's own data directory, never
committed, never in a report (FR-021's secrecy clause).

---

## R-008. PostgreSQL as the control plane's database

**Question**: The operator requires PostgreSQL for the control-plane UI. What
does that actually cost, and what has never been verified?

**Decision**: PostgreSQL 16 in a container, reached through the existing
`--database-url` path. The server already implements it; no driver work is
needed. The cost is entirely in the migration and in what has never run.

**What the code shows.** `internal/auth/auth.go:101-110` accepts either a
`postgres://`/`postgresql://` URL or a file path, and migrates through one shared
`migrate()`. `pgx/v5` is already a dependency and the driver is registered by a
blank import (`auth.go:20`), so `sql.Open("pgx", …)` is live. Against SQLite the
DSN carries `journal_mode(WAL)`, `busy_timeout(5000)` and `foreign_keys(1)` and
sets `SetMaxOpenConns(1)`; those pragmas are connection settings on the SQLite
DSN, not schema statements, so they do not affect what `migrate()` executes. The
probe below confirms the migration runs unmodified on PostgreSQL.

**What has never been verified — and what probing actually found.** Every
`postgres://` string in the repository's tests is `postgres://nobody@127.0.0.1:1/none`
— port 1, where nothing listens. Those tests assert that a database error is
*reported cleanly*; they do not assert that PostgreSQL works. So the engine had
never been exercised for real by the test suite.

A throwaway probe was run against a real PostgreSQL 15 in a container
(2026-09-27). It reached a data round-trip and failed:

```
EnsureAdmin OK — admin row written
CreateUser FAILED: insert or update on table "users" violates
  foreign key constraint "users_org_id_fkey" (SQLSTATE 23503)
```

**That was the probe's bug, not the product's.** The probe called
`CreateUser("acme", …)` for an org named `acme` that it had never created.
PostgreSQL was correctly enforcing the foreign key. `EnsureAdmin` — the real
startup path, and the thing that actually matters — had already succeeded one
line earlier.

A second probe established the truth, and its output is the version worth
keeping:

```
EnsureAdmin OK on a virgin database — the claimed defect is NOT real
CreateUser OK once the org exists — the FK was correct all along
FK is enforced: a user in a missing org is rejected, as it should be
```

**Corrected findings:**

- **`migrate()` is sound.** It completes against PostgreSQL and creates all
  eleven tables, including the `default` org row. The shared migration needs no
  engine-specific branches; the SQLite-only pragmas are connection settings on
  the SQLite DSN, not schema, not statements.
- **The auth write path is sound.** `EnsureAdmin` works on a virgin PostgreSQL
  database, which is the first thing a control plane does. The existing
  `DAWNBX_TEST_DATABASE_URL` convention (`internal/auth/auth_test.go:17-47`)
  already runs the entire auth suite against either engine, so this path was
  covered — it had simply never been pointed at a real server in CI.
- **There is no product defect to fix.** The first version of this section
  claimed otherwise, and two commits were made on that basis before a second
  probe disproved it. Both were reverted rather than amended, so the record
  shows the mistake and its correction.

**The lesson, recorded because it is the point of this feature**: a probe is a
test written by the same reasoning that produced the code under test, and it
agreed with its own blind spot — it assumed a user belongs to a pre-existing org
and wrote a test that never created one. The failure looked exactly like a real
constraint violation, which is what made it persuasive enough to write two
commits on. A green suite is not the only kind of false confidence; a red one
invented by your own fixture is the same failure wearing the opposite colour.
This is also why the *second* probe, not the first, is the evidence — a claim
this consequential needs a check that could have gone the other way.


**Why the database holds auth state** (which is what makes the engine matter):
the dashboard keeps nothing in the browser — no `localStorage`, no
`sessionStorage`, no `indexedDB`, no JS-readable cookie. Sign-in writes a user
and a session row to the database, and the `dawnbx_session` cookie
(`internal/api/api.go:106-107`, `HttpOnly`, `Secure` under TLS, `SameSite=Strict`)
is an opaque pointer to that row. The database also holds the audit trail,
cluster records, and cluster credentials sealed with AES-256-GCM.

**Consequence, corrected**: there is no product bug to fix, so this feature
carries no fix. What it does carry is the first *sustained* execution of the
PostgreSQL path in CI — a smaller claim than it sounds, since the auth suite
already runs on either engine and was merely never pointed at a real server
automatically. The remaining unknown is the rest of the control plane's own
behaviour on PostgreSQL, which the suite's sign-in test (T020) exercises for
real. FR-015 still forbids reading any of it as evidence about cloud
provisioning.

**Alternatives considered**:
- *Keep SQLite, add PostgreSQL as an option* — rejected by the operator's
  decision. Recorded here only to note what it would have cost: nothing, and it
  would have avoided an unverified engine entirely. The operator's requirement
  stands.
- *Migrate SQLite data into PostgreSQL* — deferred, and deliberately so. An
  existing control plane holds session and credential state; converting it is a
  separate concern from running the control plane on PostgreSQL, and bundling a
  data migration would widen this feature further. Fresh deployments are the
  case this feature covers.

**Required before release**: a live control-plane run against PostgreSQL, using
the real-account lifecycle, because the suite cannot stand in for it (Principle
VI). This is a release gate, not a follow-up.

---

## R-009. What does Docker Compose isolate, and what must still reach the host?

**Question**: The operator asked for container isolation so a run does not
"mess with my laptop". What does that cover, and what is deliberately excluded?

**Decision**: Everything except the recordings. Compose runs three services —
PostgreSQL, the control-plane server, and the browser driver. `.e2e/` is
bind-mounted to the host so a developer can play a video.

**Rationale**, against what a bare run actually touches:

| Touches the host today | In a container? |
|---|---|
| `~/Library/Caches/ms-playwright` (~300 MB of browsers) | yes |
| `node_modules` for `e2e/` | yes |
| A compiled `dawnbx-server` binary | yes |
| Per-run temporary data directories | yes |
| The developer's PostgreSQL, if any | yes — the container's own |
| **`.e2e/` recordings** | **no — bind-mounted out, deliberately** |

The recordings are the feature's output (FR-005, SC-003). Keeping them in the
container would make them unplayable without a copy step, which defeats the
purpose. Everything else is disposable and `docker compose down -v` returns the
machine to its prior state.

**Alternatives considered**:
- *A `Dockerfile` alone, no Compose* — rejected: the suite needs at least two
  services (database and server) plus the driver, with start-order dependencies.
  Compose expresses that; a single image would have to run a database in-process
  or a supervisor, both of which are worse.
- *Bind-mounting the source into the container* — rejected as the default: it
  would couple the container to the host's Go and Node versions, which is the
  coupling this decision exists to remove. A dev override that mounts source is
  acceptable for iteration, and is not the supported way to run the suite.

**Consequence**: Docker becomes a prerequisite for running the suite, including
locally. That is a real cost, and it is the operator's explicit trade: nothing
installs on the host except Docker itself.

---

## Open items for implementation, not clarification

These are decisions the implementation must make and measure. They are recorded
here so they are not mistaken for resolved.

1. **The retention cap's value** (FR-024/NFR-005). The requirement is that a
   cap exists and applies automatically; the number is an implementation choice
   to be picked so that local storage stays bounded while keeping enough failed
   runs to debug a recurring failure.
2. **The 3-minute budget** (NFR-001), to be measured per R-003 with the
   documented fallback if it cannot hold.
3. **Whether the test provider needs a Go build tag or can be selected purely by
   the existing `newProvider` seam** — R-001 concludes the seam exists; the
   build tag is the safe way to keep a test provider out of a shipped binary,
   and the final form is settled at implementation.
4. ~~**How `migrate()` becomes engine-aware**~~ — **settled by execution, and
   the premise was wrong twice over.** `migrate()` needs no change, and neither
   does the auth write path. The real open item is operational, not structural:
   whether CI points `DAWNBX_TEST_DATABASE_URL` at a real PostgreSQL service so
   the suite that already supports both engines actually runs on both.
5. **What happens to an existing control plane's SQLite data** — deferred by
   R-008, and it must be decided before any deployment that has one.
