# Feature Specification: Control-Plane UI End-to-End Tests

**Feature Branch**: `task/control-plane-ui-e2e`

**Created**: 2026-09-27

**Status**: Draft

**Input**: User description: "I want to build the e2e test as a recording. The reason because now i want to avoid e2e test manually. i also want the recording such as how the interaction work from the ui and how it works" — narrowed to a deterministic control-plane UI end-to-end suite, with per-test video recording, that runs without AI and without a cloud account.

## Clarifications

### Session 2026-09-27

- Q: How should the test provider make a cluster reach `ready`? → A: The test
  provider transitions after a short real delay, so the suite observes the UI
  progressing through its phases the way an operator does.
- Q: Should the provider be driven with a per-test outcome, or a single path? →
  A: The test provider takes the outcome each test needs — succeed, fail,
  unreachable, or a provider that is unavailable — and every test declares the
  outcome it exercises.
- Q: How should the operator sign in, given the suite must run unattended? → A:
  The control plane is started for the run with a known test administrator, and
  the suite signs in through the real sign-in form with those credentials.
- Q: What happens to a failed run's video, and may a run pass with skipped tests?
  → A: The video is kept on failure only, under a retention cap, and any skipped
  test counts as a failure.
- Q: Should recordings be kept for passing runs too, and where should they live?
  → A: Retention depends on where the run happened. Locally, keep every
  recording whether the test passed or failed. In continuous integration, keep
  only the failures. The output directory is inside the repository and is
  excluded from version control. This supersedes the failure-only retention
  above, which is retained here for the skipped-test rule that still stands.

## User Scenarios & Testing *(mandatory)*
<!--
  IMPORTANT: User stories should be PRIORITIZED as user journeys ordered by importance.
  Each user story/journey must be INDEPENDENTLY TESTABLE - meaning if you implement just ONE of them,
  you should still have a viable MVP (Minimum Viable Product) that delivers value.

  Assign priorities (P1, P2, P3, etc.) to each story, where P1 is the most critical.
  Think of each story as a standalone slice of functionality.
-->

The test provider MUST NOT reach a ready cluster instantly. It MUST advance a
cluster through its lifecycle over a short real delay, so the suite observes the
same visible progression an operator sees rather than asserting on a state the
interface would have skipped past. The delay MUST be long enough for the
interface to render at least one intermediate phase and short enough that the
whole suite fits inside NFR-001's budget. A test that needs to observe a specific
phase MUST wait for that phase rather than assume the delay produced it.

### User Story 1 - Review a cluster request end to end (Priority: P1)

An operator opens the control plane, signs in, chooses a provider and region,
sees a price before anything is created, and requests a cluster. They then watch
the cluster's state advance through its lifecycle until it reports ready, and the
suite proves that journey in a real browser rather than by assertion on internal
state.

**Why this priority**: This is the single most expensive path in the product —
money is spent, an instance boots, a certificate is issued. It is the journey a
buyer evaluates, and today nothing exercises it through a real browser at all.

**Independent Test**: Can be fully tested by driving the dashboard in a browser
against a control plane with no cloud credentials, and observing that the
request form, the price, the phase progression, and the ready state all appear.

**Acceptance Scenarios**:

1. **Given** a signed-out operator and a control plane with no cloud credentials, **When** they sign in with valid credentials, **Then** the dashboard is reachable and the signed-in shell renders.
2. **Given** a signed-in operator, **When** they open the cluster request form and choose a region, **Then** a price is shown before any cluster is created.
3. **Given** a quoted cluster, **When** the operator confirms it, **Then** a cluster exists and its state advances from creation through to ready.
4. **Given** a ready cluster, **When** the operator opens it, **Then** its URL and phase history are visible.

---

### User Story 2 - Recover from a failure without reading source (Priority: P2)

A cluster that fails to become ready leaves the operator with a reason they can
read on screen and a recording they can watch. The operator is never required to
interpret logs, and never has to ask an agent what happened.

**Why this priority**: Provisioning is where failures happen, and today the only
evidence is a log line in a terminal. This is what makes a failed run diagnosable
by a human rather than only by someone who can read the codebase.

**Independent Test**: Can be fully tested by driving a cluster to a failed state
in a browser and confirming the failure detail is shown on screen and that a
playable recording of the session exists.

**Acceptance Scenarios**:

1. **Given** a cluster whose creation fails, **When** the operator opens it, **Then** the dashboard shows a failure state with a specific reason.
2. **Given** a cluster that cannot be reached, **When** the operator requests its workers, **Then** the interface distinguishes "cannot reach the cluster" from "the cluster has no workers".
3. **Given** any completed test run, **When** the operator inspects the run's artefacts, **Then** a playable video of the browser session is present.

---

### User Story 3 - Manage a worker's life in the browser (Priority: P3)

An operator adds a worker to a running cluster, sees it become ready, and then
removes it. The interface refuses a removal that would strand workloads, and
deletes a cluster cleanly when asked.

**Why this priority**: Node lifecycle is the day-two operation, and the removal
guard is a safety property an operator must be able to trust visually rather
than infer.

**Independent Test**: Can be fully tested by adding a worker, waiting for it to
report ready, removing it, and confirming the cluster reflects each step.

**Acceptance Scenarios**:

1. **Given** a ready cluster, **When** the operator requests a worker, **Then** a worker is added and reaches ready.
2. **Given** a cluster holding workloads, **When** the operator attempts to remove the node holding them, **Then** the request is refused with a reason naming the cause.
3. **Given** a cluster with no workers holding workloads, **When** the operator deletes it, **Then** the cluster is deleted and disappears from the list.

---

### Edge Cases

- The control plane has no cloud credentials configured: the interface MUST still render and MUST NOT present a cluster-creation path that cannot work.
- The provider roster shows providers that are not available: they MUST be visible and MUST be visibly unavailable rather than absent, so an operator can see what exists.
- A price quote goes stale between display and confirmation: the confirmation MUST be refused with an explanation, and the operator MUST be able to re-quote without re-entering the form.
- A cluster is deleted while it still holds workloads: the deletion MUST be refused with a reason.
- A worker reaches ready but cannot be correlated with anything the operator asked for: the interface MUST NOT report an empty list as though nothing were running.
- The browser reloads mid-flow, or the operator opens the same cluster in two tabs: state MUST be consistent after the reload.
- A test run is interrupted midway: the suite MUST fail, MUST NOT report success, and MUST leave a recording for what ran.

## Requirements *(mandatory)*

### Functional Requirements

- **FR-001**: The suite MUST run end to end with no human present, no AI agent driving it, and no cloud account or credentials of any kind.
- **FR-002**: The suite MUST be deterministic: the same code MUST produce the same result on every run. It MUST NOT depend on network reachability to a cloud or on a previously created resource. It MAY use a short real delay to observe lifecycle progression, provided each such wait is bounded and asserted on an observable state rather than on elapsed time.
- **FR-003**: The suite MUST exercise the dashboard through a real browser, driving the interface as an operator would — by interacting with rendered controls, not by calling the API directly.
- **FR-004**: The suite MUST run against a control plane started with no cloud credentials, with no Kubernetes client, and with no cluster-bound runtime.
- **FR-005**: Every run MUST produce a playable video recording of the browser session for each test, whether the test passed or failed, and each recording MUST be attributable to the run that produced it. What happens to a recording after the run is governed by FR-023.
- **FR-006**: A run MUST be reported as successful only if every assertion held; an interrupted or partially executed run MUST be reported as failed.
- **FR-007**: The suite MUST be integrated into the existing local gate so that it runs on every push alongside the other checks, and MUST NOT replace any existing check.
- **FR-008**: The suite MUST verify that a price is shown before a cluster is created, and MUST fail if a cluster can be created without one.
- **FR-009**: The suite MUST verify that the cluster lifecycle advances to a ready state through observable operator-visible states, and MUST NOT assert on internal data structures.
- **FR-010**: The suite MUST verify that a failed cluster presents a specific reason on screen, and that an unreachable cluster is distinguishable from a cluster with no workers.
- **FR-011**: The suite MUST verify that adding, and then removing, a worker is reflected in the interface.
- **FR-012**: The suite MUST verify that a removal which would strand workloads is refused with a reason naming the cause.
- **FR-013**: The suite MUST verify that an unavailable provider is visible and visibly unavailable.
- **FR-014**: The suite MUST verify that a stale quote is refused with an explanation and that the operator can re-quote without re-entering the form.
- **FR-015**: The suite MUST NOT be presented, documented, or reported as evidence that cloud provisioning works; its scope MUST be stated as the operator-facing interface and its orchestration.
- **FR-016**: The suite MUST NOT require credentials, and MUST run in an environment with no network route to any cloud.
- **FR-017**: The recorded artefacts MUST be locatable from the run's report. A recording the run has kept MUST remain on disk and playable for as long as FR-024 permits.
- **FR-018**: The suite MUST use the same provider-neutral seam the product uses for every cloud, so that a future provider is testable by the same suite without changes to it.
- **FR-019**: The test provider MUST accept the outcome a test requires — succeeding, failing with a reason, present but unreachable, or a provider that is unavailable — so that every journey in the user stories is reachable by at least one test.
- **FR-020**: Each test MUST declare the outcome it exercises, so a reader can tell from the test alone which path it covers.
- **FR-021**: The suite MUST authenticate through the real sign-in form, and a test MUST fail if that form does not accept the run's credentials and reach the signed-in shell.
- **FR-022**: A run MUST fail if any test in the suite is skipped, and the run's report MUST state how many tests were executed out of how many the suite defines.
- **FR-023**: Retention MUST differ by environment. A run on a developer machine MUST keep every recording, whether the test passed or failed. A run in continuous integration MUST keep recordings for failed tests and MUST discard recordings for tests that passed, so that a green run uploads nothing.
- **FR-024**: Retained recordings MUST be subject to a retention cap in both environments, so that stored artefacts cannot grow without bound across runs.
- **FR-025**: Recordings MUST be written inside the repository, under a single dedicated directory for the suite's output, and that directory MUST be excluded from version control.
- **FR-026**: A run started on a developer machine MUST write its recordings to the same output directory a CI run uses, so that the two are interchangeable.

### Non-Functional Requirements

- **NFR-001**: A full suite run MUST complete within 3 minutes on a standard CI runner.
- **NFR-002**: The suite MUST run unattended and MUST NOT require a display server, a manual step, or an interactive login.
- **NFR-003**: A failure MUST produce a report naming the test, the assertion that failed, and the location of its recording.
- **NFR-004**: The suite MUST be runnable on a developer machine and in CI from the same command.
- **NFR-005**: Stored recordings MUST NOT grow without bound; a cap on retained runs MUST be applied automatically, without operator intervention.
- **NFR-006**: A local run and a CI run MUST execute the same tests, and MUST differ only in where their reports are surfaced and which recordings are kept under FR-023.
- **NFR-007**: A continuous-integration run in which every test passed MUST upload no recordings and MUST add no recording artefact to the run.

### Success Criteria

- **SC-001**: A complete suite run finishes in under 3 minutes with no manual intervention.
- **SC-002**: A run of the suite on unchanged code produces the same pass-or-fail result 20 consecutive times.
- **SC-003**: Every run, passing or failing, yields a video an operator can play and understand what was attempted, without reading the test source. On a developer machine a passing run's recording is kept, so a reviewer can confirm the journey actually happened rather than trusting the result; in continuous integration a passing test's recording is discarded and nothing is uploaded.
- **SC-004**: 100% of the operator-facing cluster journeys described in the user stories are covered by at least one browser-driven assertion.
- **SC-005**: The suite runs green in an environment with no cloud credentials and no network route to a cloud provider.
- **SC-006**: An operator watching a recorded failing run can name the step that failed and the reason, having viewed only the video and the failure report.

## Assumptions on the test provider

- The product's provider seam already accepts an injected implementation, so the
  suite supplies its own rather than the suite adding a new seam of its own.
- Lifecycle progression is observed by waiting for an expected operator-visible
  state, not by asserting a fixed elapsed time.
- Every outcome named in FR-019 is producible without network access.

### Out of Scope

- Proving that a cloud provider accepts what is sent to it. That is covered only by
  the real-account lifecycle run, which is manual, recorded, and separate.
- Any automated emulation of a cloud provider. The suite drives the product's own
  provider-neutral seam; it does not stand in for a cloud, and no such emulator
  may be introduced as a substitute for a real run.
- The sandbox-facing interface, which requires a live cluster to be meaningful.
- The recorded real-account lifecycle run itself, which is a separate feature.

## Assumptions

- The control plane can be started in a mode with no cloud credentials, no
  Kubernetes client, and no cluster volume, and will serve the dashboard and the
  identity surface. This is existing behaviour.
- A provider-neutral seam already exists and is exercised by unit tests; the
  suite reuses it rather than introducing a new one.
- The dashboard is a web application served by the control plane, so a single
  process can serve both the interface under test and the data it renders.
- Video recording is an output of the browser-driving tooling used, not a
  separate capture step.
- The suite's output directory is added to the repository's ignore list as part
  of this feature, so recordings are never committed (FR-025).
- A test administrator can be established at startup for the run, so the sign-in
  form can be exercised without an interactive prompt.

## Dependencies

- A browser-driving tool that records video per test and can run headless.
- A way to start the control plane with an isolated data directory per run, so
  runs do not share state.
- The existing gate, which the suite joins rather than replaces.

## Future Enhancements

- The recorded real-account lifecycle run, which would use the same browser
  tooling against a real cluster and produce a recording of a genuine run.
- Assertions over an event log correlated with the video, so a failure report can
  name the failing step in text as well as showing it.
- Extending browser coverage to the sandbox-facing interface once a cluster
  fixture is available.
