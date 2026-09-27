---

description: "Task list for the control-plane UI end-to-end test suite"
---

# Tasks: Control-Plane UI End-to-End Tests

**Input**: Design documents from `specs/002-control-plane-ui-e2e/`

**Prerequisites**: plan.md, spec.md, research.md, data-model.md,
contracts/test-provider.md, quickstart.md

**Tests**: Required. This feature *is* a test tier, and Principle IV requires a
behaviour change to ship with a test that fails before and passes after. The
PostgreSQL migration in Phase 2 is the critical case: it has never executed
successfully in this repository (research.md R-008), so it is written
test-first.

**Organization**: Tasks are grouped by user story so each story can be
implemented, tested, and delivered as an independent increment.

## Format: `[ID] [P?] [Story] Description`

- **[P]**: Can run in parallel (different files, no dependencies)
- **[Story]**: Which user story this task belongs to (US1, US2, US3)
- Include exact file paths in descriptions

## Path Conventions

- `e2e/` — the suite, sibling to `web/`, `sdk/`, `docs/`. Deliberately **not**
  inside `web/`, so it does not inherit that package's Vitest config, coverage
  thresholds, or Biome rules.
- `.e2e/` — run output (recordings, report), git-ignored, bind-mounted out of
  the container.
- Go product code stays in its existing locations; this feature changes
  `internal/auth/` and `cmd/dawnbx-server/` and nothing else.

---

## Phase 1: Setup (Shared Infrastructure)

**Purpose**: Create the `e2e/` package and the ignore rule. No product code is
touched here.

- [ ] T001 Create `e2e/package.json` with `@playwright/test` as the only runtime dependency and `playwright.config.ts` as the test script
- [ ] T002 [P] Add `.e2e/` to `.gitignore` so recordings are never committed (FR-025)
- [ ] T003 [P] Create `e2e/tsconfig.json` extending the repo's TypeScript settings for the suite
- [ ] T004 Create `e2e/playwright.config.ts` with `outputDir` pointing at `../.e2e`, `video: 'on'`, `use.baseURL` for the control plane, and `retries: 0` (a retry would mask the determinism claim in SC-002)

---

## Phase 2: Foundational (Blocking Prerequisites)

**Purpose**: Make the control plane start on PostgreSQL inside a container, with
a test provider selectable only in a test build. No user story can run until
this is complete.

**⚠️ CRITICAL**: No user story work can begin until this phase is complete.

### PostgreSQL as the control plane's database

- [ ] T005 [P] Write a failing test in `internal/auth/auth_postgres_test.go` that opens a PostgreSQL connection, runs the migration, and round-trips a user, a session, an audit row, and a cluster record through `pgx`
- [ ] T006 Make `migrate()` in `internal/auth/auth.go` engine-aware: SQLite keeps its `journal_mode(WAL)`, `busy_timeout(5000)`, `foreign_keys(1)` pragmas and `SetMaxOpenConns(1)`; PostgreSQL gets statements valid on it. The current shared migration has never run on PostgreSQL
- [ ] T007 Run `go test ./internal/auth/ -run Postgres` and confirm T005 now passes — this is the first successful execution of the PostgreSQL path in this repository (R-008)
- [ ] T008 [P] Add a Compose-independent check in `internal/auth/auth_postgres_test.go` that skips with a clear message when no `DAWNBX_TEST_POSTGRES_URL` is set, so a developer without a database is not blocked

### Test provider, selectable only in a test build

- [ ] T009 [P] Create `internal/provider/e2e/e2e.go` implementing the full provider interface from `internal/provider/provider.go` — `Capabilities`, `Regions`, `InstanceTypes`, `Estimate`, `Create`, `Status`, `Nodes`, `RemoveNode`, `Destroy` — driven by the `TestOutcome` fields in `data-model.md`
- [ ] T010 [P] Create `cmd/dawnbx-server/provider_e2e.go` behind `//go:build e2e`, wiring `serverDeps.newProvider` (`cmd/dawnbx-server/main.go:116`) to the test provider
- [ ] T011 Verify with `go build ./cmd/dawnbx-server` and `strings dawnbx-server | grep -i e2e` that a **default** build contains no test-provider symbol, then with `go build -tags e2e` that it does. A default build that can select a test provider is a security failure, not a style issue
- [ ] T012 [P] Create `e2e/fixtures/test-provider.ts` mapping each test's declared outcome to the provider configuration, validating that `failureReason` is non-empty when the outcome is `fail` and that `advanceAfter` is positive (data-model.md validation rules)

### Container environment

- [ ] T013 [P] Create `e2e/Dockerfile` with a build stage for `dawnbx-server` (embedded dashboard bundle included) and a test stage carrying `@playwright/test` and its browser
- [ ] T014 [P] Create `e2e/docker-compose.yml` with three services — `postgres` (PostgreSQL 16, named volume, health check), `server` (`--control-plane`, pointed at `postgres` via `--database-url`, its own files on a volume), `driver` (the suite) — bind-mounting only `../.e2e` to the host (R-009)
- [ ] T015 Make the `server` service depend on the `postgres` health check, never on a sleep, and confirm the migration completes before the server starts serving
- [ ] T016 [P] Create `e2e/fixtures/control-plane.ts` that waits for the database, waits for the server's health endpoint, and reads the generated administrator credential from `<data-dir>/server/admin.env` (R-007). The credential must never be logged, asserted on, or written to a report (FR-021)

### Retention and reporting

- [ ] T017 Create `e2e/support/retention.ts` — locally keep every recording, in CI delete recordings for tests that passed, apply the cap automatically (FR-023, FR-024, NFR-005, NFR-007)
- [ ] T018 [P] Create `e2e/fixtures/expect.ts` with helpers that wait for an operator-visible state with a bounded timeout, where a timeout fails the test and never passes it
- [ ] T019 [P] Create `e2e/support/summary.ts` reporting tests executed against tests defined, so a skipped test is visible and fails the run (FR-022)

**Checkpoint**: `docker compose -f e2e/docker-compose.yml run --rm driver` starts a
control plane on PostgreSQL with a test provider, and `docker compose -f
e2e/docker-compose.yml down -v` leaves the machine unchanged. Foundation ready.

---

## Phase 3: User Story 1 - Review a cluster request end to end (Priority: P1) 🎯 MVP

**Goal**: An operator signs in, sees a price before anything is created,
confirms, and watches the cluster advance to ready — proven in a real browser.

**Independent Test**: Run the suite in Compose with no cloud credentials. The
sign-in, quote, confirmation, phase progression, and ready state are all observed
in the recorded video, and the cluster reached `ready`.

### Tests for User Story 1 ⚠️

> Write these first; they must fail before the fixture supports them.

- [ ] T020 [P] [US1] Sign-in spec in `e2e/specs/sign-in.spec.ts` — reach the signed-in shell through the real form, using the credential from the fixture (FR-021)
- [ ] T021 [P] [US1] Price-before-create spec in `e2e/specs/cluster-request.spec.ts` — assert a price is rendered, and that no cluster is created without one (FR-008)
- [ ] T022 [P] [US1] Progression spec in `e2e/specs/cluster-request.spec.ts` — confirm the quote, then wait for an intermediate phase and then `ready` (FR-009)

### Implementation for User Story 1

- [ ] T023 [US1] Configure the success outcome in `e2e/fixtures/test-provider.ts` with `advanceAfter` around 1 second — deliberately faster than the dashboard's 5s refetch at `web/src/pages/clusters.tsx:492`, so a phase is present when the UI next polls (R-003)
- [ ] T024 [US1] Add a stable selector for the request form, the price, the confirm control, and each phase badge in `e2e/support/selectors.ts`, matching what `web/src/pages/clusters.tsx` already renders
- [ ] T025 [US1] Assert in `e2e/specs/cluster-request.spec.ts` that the cluster reaches `ready` and that its URL is visible
- [ ] T026 [US1] Measure this phase's wall-clock cost and record it against NFR-001 in `specs/002-control-plane-ui-e2e/plan.md`. Only this story observes progression, at ~5s per observed phase because the dashboard's refetch — not the fixture — sets that floor (R-003)
- [ ] T027 [US1] If T026 shows the budget cannot hold, narrow the assertion to the terminal state plus one observed intermediate phase, and note the change in `specs/002-control-plane-ui-e2e/plan.md`. Do **not** shorten `advanceAfter` below the UI's poll interval — that trades speed for flake

**Checkpoint**: User Story 1 passes on its own in a container with no cloud
credentials, and the video shows the operator's session.

---

## Phase 4: User Story 2 - Recover from a failure without reading source (Priority: P2)

**Goal**: A failed cluster shows a specific reason on screen, an unreachable
cluster is distinguishable from one with no workers, and a failed run leaves a
playable recording.

**Independent Test**: Drive a cluster to a failed state and an unreachable
state; the interface shows a specific reason in each case and the recordings
exist for both.

### Tests for User Story 2 ⚠️

- [ ] T028 [P] [US2] Failure-reason spec in `e2e/specs/failure.spec.ts` — a failed cluster shows the specific reason, not a generic message (FR-010)
- [ ] T029 [P] [US2] Unreachable-versus-empty spec in `e2e/specs/failure.spec.ts` — a cluster that cannot be reached is not presented as having no workers (FR-010)
- [ ] T030 [P] [US2] Provider-unavailable spec in `e2e/specs/failure.spec.ts` — an unavailable provider is visible in the roster and visibly unavailable, not absent (FR-013)
- [ ] T031 [P] [US2] Stale-quote spec in `e2e/specs/failure.spec.ts` — a quote that went stale is refused with an explanation and can be re-quoted without re-entering the form (FR-014)
- [ ] T032 [P] [US2] Recording-artefact spec in `e2e/specs/retention.spec.ts` — a failed test's recording exists and is playable; a passing test's recording is absent under `CI=1` (FR-023)

### Implementation for User Story 2

- [ ] T033 [US2] Add the `fail`, `unreachable`, and `providerAvailable: false` outcomes to `e2e/fixtures/test-provider.ts`, each producing the specific value the interface must render
- [ ] T034 [US2] Add selectors in `e2e/support/selectors.ts` for the failure detail, the unreachable notice, and the unavailable-provider marker
- [ ] T035 [US2] Verify in `e2e/specs/retention.spec.ts` that no recording contains the run's administrator credential or any secret (FR-021's secrecy clause)

**Checkpoint**: User Story 2 passes on its own. A failing run is diagnosable by
playing the video (SC-003, SC-006).

---

## Phase 5: User Story 3 - Manage a worker's life in the browser (Priority: P3)

**Goal**: An operator adds a worker, sees it ready, is refused a removal that
would strand workloads, and deletes a cluster cleanly.

**Independent Test**: Add a worker, wait for ready, attempt a refused removal,
remove the worker, delete the cluster — all observed in the browser.

### Tests for User Story 3 ⚠️

- [ ] T036 [P] [US3] Add-worker spec in `e2e/specs/workers.spec.ts` — a worker is added and reaches `ready` (FR-011)
- [ ] T037 [P] [US3] Refused-removal spec in `e2e/specs/workers.spec.ts` — removing a node holding workloads is refused with a reason naming the cause (FR-012)
- [ ] T038 [P] [US3] Remove-and-delete spec in `e2e/specs/workers.spec.ts` — the worker is removed and the cluster deleted and disappears from the list (FR-011)

### Implementation for User Story 3

- [ ] T039 [US3] Add the `holdWorkers` outcome to `e2e/fixtures/test-provider.ts`, returning `ErrNodeBusy` from `RemoveNode` when set
- [ ] T040 [US3] Add selectors in `e2e/support/selectors.ts` for the node list, the add-worker control, the refusal notice, and the delete confirmation
- [ ] T041 [US3] Add a selector in `e2e/support/selectors.ts` asserting the unreachable case is never rendered as an empty node list, so a future change cannot regress it silently

**Checkpoint**: All three user stories pass independently.

---

## Phase 6: Polish & Cross-Cutting Concerns

**Purpose**: Wire the suite into the gate and CI, and satisfy the release
requirement this feature creates.

- [ ] T042 [P] Add `.e2e/` verification to `hack/check-harness-cites.py` or a sibling check, so a committed recording fails the gate
- [ ] T043 [P] Update `docs/content/docs/guide/` with how to run the suite in Compose and where the recordings land
- [ ] T044 Add the suite to `hack/check.sh` as a step alongside the existing checks, replacing nothing (FR-007)
- [ ] T045 Update `.github/workflows/ci.yml` to run the suite in Compose. The browser install moves into the image, so the workflow no longer installs Playwright itself
- [ ] T046 [P] Update `AGENTS.md` with the Compose-based suite, the per-environment retention rule, and the fact that the suite is not evidence about cloud provisioning
- [ ] T047 [P] Add `e2e/` to the coverage-floor consideration in `hack/coverage-floor.txt` — the suite is excluded from the dashboard's Vitest coverage, and this must be stated rather than left implicit
- [ ] T048 Run the full `bash hack/check.sh` and confirm it is green with the suite included
- [ ] T049 Run the determinism check from `quickstart.md` — 20 consecutive runs, all agreeing (FR-002, SC-002)
- [ ] T050 Run the container-isolation check from `quickstart.md` — no `node_modules`, no browser cache on the host (R-009)
- [ ] T051 Verify `strings dawnbx-server | grep -i e2e` returns nothing for a default build, closing out the T011 security requirement
- [ ] T052 **Release gate**: run the real-account lifecycle via `hack/verify.sh` against a control plane on PostgreSQL and record the result in `specs/002-control-plane-ui-e2e/quickstart.md`. The suite cannot stand in for this (Principle VI, R-008). This is a release blocker, not a follow-up
- [ ] T053 Decide what happens to an existing control plane's SQLite data and record the decision in `docs/content/docs/guide/`, before deploying to any instance that has one (deferred by R-008, open item 5)

---

## Dependencies & Execution Order

### Phase Dependencies

- **Setup (Phase 1)**: No dependencies — can start immediately
- **Foundational (Phase 2)**: Depends on Setup — **BLOCKS all user stories**
- **User Stories (Phase 3–5)**: All depend on Foundational completion
  - Stories can then proceed in parallel
  - Or sequentially in priority order (P1 → P2 → P3)
- **Polish (Phase 6)**: Depends on all desired user stories being complete,
  **except T052**, which depends on Phase 2 alone and may run in parallel with
  the stories.

### User Story Dependencies

- **User Story 1 (P1)**: Starts after Foundational — no dependencies on other stories
- **User Story 2 (P2)**: Starts after Foundational — shares the fixture with US1
- **User Story 3 (P3)**: Starts after Foundational — shares the fixture with US1/US2

All three share `e2e/fixtures/` and `e2e/support/`, which is why the fixture work
is Foundational rather than per-story. Stories are otherwise independent and each
is independently testable.

### Within Each User Story

- Tests are written and MUST fail before implementation
- Fixture support before specs that depend on it
- Story complete before moving to the next priority

### Parallel Opportunities

- All Setup tasks marked [P] can run in parallel
- All Foundational tasks marked [P] can run in parallel within Phase 2
- Once Foundational completes, all three stories can start in parallel
- All spec files for a story marked [P] can run in parallel
- T052 (the live release gate) can run as soon as Phase 2 completes

---

## Parallel Example: User Story 1

```bash
# All US1 specs in parallel — different files:
Task: "Sign-in spec in e2e/specs/sign-in.spec.ts"
Task: "Price-before-create spec in e2e/specs/cluster-request.spec.ts"

# Provider fixture and selectors are independent of the specs:
Task: "Configure the success outcome in e2e/fixtures/test-provider.ts"
Task: "Add stable selectors in e2e/support/selectors.ts"
```

---

## Implementation Strategy

### MVP First (User Story 1 Only)

1. Complete Phase 1: Setup
2. Complete Phase 2: Foundational (CRITICAL — blocks all stories, and includes
   the PostgreSQL migration, which is itself the riskiest task here)
3. Complete Phase 3: User Story 1
4. **STOP and VALIDATE**: run the suite in Compose, play the video, confirm the
   journey is real
5. Run T052 — the live control-plane run on PostgreSQL. This is the first time
   this engine runs outside a test, and it is what makes the rest credible

### Incremental Delivery

1. Setup + Foundational → Foundation ready, PostgreSQL proven in a container
2. Add US1 → Test independently → the MVP: one real journey, recorded
3. Add US2 → Test independently → failures become diagnosable
4. Add US3 → Test independently → node lifecycle covered
5. Polish → gate, CI, docs

### Parallel Team Strategy

1. Team completes Setup + Foundational together — this includes the migration
   and should not be split, since the schema work and the container are coupled
2. Then in parallel:
   - Developer A: User Story 1
   - Developer B: User Story 2
   - Developer C: User Story 3
3. One developer takes T052, the live release gate, as soon as Foundational lands

---

## Notes

- [P] tasks = different files, no dependencies
- [Story] label maps task to specific user story for traceability
- Each user story is independently completable and testable
- Verify tests fail before implementing — especially T005, which asserts the
  PostgreSQL path works and has never passed
- Commit after each task or logical group
- Stop at any checkpoint to validate a story independently
- Avoid: vague tasks, same-file conflicts, cross-story dependencies that break
  independence
- **T011 and T052 are the two tasks that protect something real.** T011 keeps a
  test-only provider out of a shipped binary that would otherwise accept an admin
  password and report a ready cluster that does not exist. T052 is the only
  evidence that the database migration works outside a test container. Neither
  may be deferred, and a green suite does not substitute for either
