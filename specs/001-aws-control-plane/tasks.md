---
description: "Task list for the SSH-Free AWS Control Plane"
---

# Tasks: SSH-Free AWS Control Plane

**Input**: Design documents from `specs/001-aws-control-plane/`

**Prerequisites**: plan.md, spec.md, research.md, data-model.md, contracts/, quickstart.md

**Tests**: included, not optional. Constitution Principle IV makes a behaviour change ship with a
test, and every success criterion in `spec.md` is phrased as a scripted test (SC-001 … SC-010).

**Organization**: grouped by user story so each is independently implementable and testable.

## Format: `[ID] [P?] [Story] Description`

- **[P]**: different files, no dependencies on each other
- **[Story]**: US1–US4 from `spec.md`
- Every task touches a real path. New files are created; nothing else is invented.

**Standing rules for every task**

- Run `bash hack/check.sh` before the commit that closes a phase. It MUST be green.
- Any change under `web/` ships `npm run build --prefix web` and the rebuilt `internal/api/ui` in
  the same commit (`hack/check.sh:101-107` fails otherwise).
- Any route change touches all five contract files in the same commit (Principle III).
- `hack/coverage-floor.txt` may only rise. Raise a floor in the commit that raises the coverage.
- Never edit or commit in the primary checkout; this branch only.

---

## Phase 1: Setup (Shared Infrastructure)

**Purpose**: dependencies and the two files everything else builds on.

- [X] T001 Add AWS SDK for Go v2 to `go.mod`/`go.sum`: `config`, `service/cloudformation`,
      `service/ec2`, `service/ssm`, `service/pricing`, `service/sts`. Six modules, no others
      (research.md D3). `go get` then `go mod tidy`.
- [X] T002 [P] Create `internal/provider/provider.go` — the neutral interface and nothing else:
      `ClusterSpec`, `Bootstrap`, `Handle`, `Status`, the `Provider` interface, and the registry.
      No import of any adapter package, ever (research.md D10).
- [X] T003 [P] Create `internal/cluster/secret.go` — AES-256-GCM at rest. Key from
      `--control-plane-key` / `DAWNBX_CONTROL_PLANE_KEY`, else generated once to
      `<data-dir>/server/control-plane.key` mode `0600` (research.md D5).

---

## Phase 2: Foundational (Blocking Prerequisites)

**Purpose**: storage, the cluster-less startup path, and the capability surface. Nothing below
this phase can be tested without it.

**⚠️ CRITICAL**: no user story work begins until this phase is green.

### Tests for Foundational

> Written first; they must fail before the implementation lands.

- [X] T004 `internal/provider/provider_test.go` — import-purity test: no non-test file in
      `internal/provider` may import an adapter package. Plus a `fakeProvider` usable by
      `internal/cluster` tests with no AWS dependency. This is the mechanical form of FR-013.
- [X] T005 [P] `internal/cluster/secret_test.go` — round-trip, wrong-key rejection, nonce
      uniqueness, and that ciphertext never contains the plaintext.
- [X] T006 [P] `internal/auth/auth_test.go` — the four new tables migrate cleanly on a fresh DB
      and on an existing one, and cascade delete fires.

### Implementation for Foundational

- [X] T007 `internal/auth/auth.go` — append `clusters`, `cluster_credentials`, `cluster_nodes`,
      `cluster_ops` to the existing `migrations` slice (auth.go:36-51) and add their accessors.
      Columns exactly as `data-model.md`. No provider resource is a column; `provider_state` is
      one JSON column.
- [X] T008 `internal/cluster/cluster.go` — the registry over the auth DB: create row, load, list,
      set status/phase, record an op, load and store credentials, nodes. Validation lives here so
      a rejected request costs no AWS call: name pattern, instance type and disk against the
      provider's advertised catalogue, domain charset, region membership.
- [X] T009 `internal/cluster/cluster_test.go` — registry CRUD, the two-secret lifecycle
      (`api_key` NULL until verified), and validation rejections.
- [X] T010 `internal/api/api.go` — add `ControlPlaneHandler()`. Same `h` closure and `s.auth`;
      registers `s.manage(h)` plus the capability routes; registers an explicit
      `503 cluster_unavailable` for `/v1/sandboxes`, `/v1/sandboxes/`, `/v1/status`, `/v1/nodes`,
      `/v1/nodes/` so the JSON envelope survives. **Do not touch `s.auth`'s `{id}` guard**
      (api.go:264-274) — the new routes use `{name}` and `{node}` so it never fires.
- [X] T011 `internal/api/api_test.go` — control-plane mux: 503 envelope shape for the
      cluster-bound families, identity routes still working, and a member session against a
      cluster route that must not panic.
- [X] T012 `internal/api/api.go` — guard `writeErr` so a `sandbox.Error` with `Status == 0` cannot
      reach `WriteHeader(0)`; default to 500 at the sink rather than trusting five new
      constructors (api.go:319-326).
- [X] T013 `cmd/dawnbx-server/main.go` — add `--control-plane`, `--control-plane-key`,
      `--release-url`; add `serveControlPlane(ctx, cfg, deps)` that skips `store.Open` and the k8s
      client, `MkdirAll`s `<data-dir>/server`, opens the same auth DB, and serves
      `ControlPlaneHandler()`. Admin password from `--admin-password` / env / `admin.env`; if
      none, generate once, write `admin.env` `0600`, and log the *path*, never the value.
- [X] T014 `cmd/dawnbx-server/serve_test.go` — control-plane startup: no `.dawnbx-volume` marker
      needed, no k8s client built, `serve()` unchanged by default (still fails without the marker).

### Capability contract (Principle III — all five files in one change)

- [X] T015 `internal/api/openapi.yaml` — add `/v1/control-plane` and `/v1/providers` plus the
      `ControlPlane` and `Provider` schemas. Merge from
      `specs/001-aws-control-plane/contracts/openapi-delta.yaml`; the delta is not a contract.
- [X] T016 `npm run gen --prefix web` and commit the regenerated `web/src/lib/schema.d.ts`.
- [X] T017 [P] `sdk/python/src/dawnbx/__init__.py` — read-only `control_plane()` and
      `providers()` helpers, same error handling and retry rule as the existing client.
- [X] T018 [P] `sdk/typescript/src/index.ts` — the same two helpers; the `cluster_unavailable`
      retry rule must cover them.
- [X] T019 [P] `sdk/python/tests/test_sdk.py` and `sdk/typescript/test/sdk.test.mjs` — tests for
      both new helpers, keeping their coverage floors met.
- [X] T020 `docs/content/docs/guide/api.mdx` — new `## Clusters` section, and **close the
      pre-existing `/v1/nodes` drift**: those three routes are in `openapi.yaml:293-316` and in
      neither doc table.
- [X] T021 [P] `README.md` — the hand-kept route list, including the same `/v1/nodes` rows.

### Dashboard shell (capability only; the wizard comes in Phase 3)

- [X] T022 `web/src/pages/shell.tsx` — read `GET /v1/control-plane`; hide the Sandboxes link and
      the `/v1/status` poll in control-plane mode. Keep the existing `<nav>` pattern
      (shell.tsx:48-63).
- [X] T023 `web/src/pages/shell.test.tsx` — both modes render, and the status poll is not issued
      in control-plane mode.
- [X] T024 `npm run build --prefix web` and commit `internal/api/ui`.

**Checkpoint**: a control plane starts with no cluster, no marker, and no k3s; the dashboard loads
and knows which mode it is in. `bash hack/check.sh` green.

---

## Phase 3: User Story 1 — Create an AWS Cluster Without SSH (Priority: P1) 🎯 MVP

**Goal**: an operator configures an AWS cluster in the dashboard, sees the price, confirms, and
receives a usable cluster URL with its credentials, without SSH.

**Independent test**: `quickstart.md` Tier 2 then Tier 3, then Tier 4 on a throwaway account.

### Tests for User Story 1

- [X] T025 [P] `internal/provider/aws/credentials_test.go` — `config.LoadDefaultConfig` over an
      `httptest` endpoint; STS identity failure maps to a non-secret message.
- [X] T026 [P] `internal/provider/aws/parameters_test.go` — `PutParameter` / `DeleteParameter`
      against `httptest`; `SecureString` type; the value never enters a log line.
- [X] T027 [P] `internal/provider/aws/cloudformation_test.go` — `CreateStack`, `DescribeStacks`,
      `DeleteStack`; a `WaitCondition` `FAILURE` becomes `Status{State: failed, Reason: …}` with
      the reason sanitised.
- [X] T028 [P] `internal/provider/aws/estimate_test.go` + `internal/provider/aws/pricing_test.go` — hourly and monthly
      compute/storage/public-IPv4 lines; the excluded list is present.
- [X] T029 [P] `internal/provider/aws/aws_test.go` — the adapter satisfies `provider.Provider`
      and returns the right catalogue.
- [X] T030 [P] `internal/cluster/client_test.go` — TLS pinning: wrong pin is refused; first fetch
      pins; `POST /v1/login` then `POST /v1/keys` against a local `httptest` TLS server.
- [X] T031 [P] `internal/cluster/provision_test.go` — the whole create path against a fake
      provider: phases in order, `failed` triggers provider cleanup, a previously `ready` cluster
      that later fails is **not** deleted.
- [X] T032 [P] `internal/api/clusters_test.go` — every US1 route: `adminOnly`, an API key refused,
      `quote_stale`, `credentials_not_ready` before ready, credentials route audited.
- [X] T033 [P] `internal/api/authmatrix_test.go` — add the cluster routes to the existing auth
      matrix table (member 403, key 403, admin 200).
- [X] T034 [P] `web/src/pages/clusters.test.tsx` — the wizard: AWS selectable, GCP/Azure disabled
      and unselectable, estimate shown before confirm, no confirm without a quote.

### AWS adapter (the only place an AWS package may appear)

- [X] T035 `internal/provider/aws/credentials.go` — standard credential chain + STS identity.
- [X] T036 `internal/provider/aws/handle.go` — the AWS blob shape (stack, parameter, security
      group, launch template, public ip) and its encode/decode. Private to the adapter.
- [X] T037 `internal/provider/aws/parameters.go` — `ssm:PutParameter` / `DeleteParameter`.
- [X] T038 `internal/provider/aws/cloudformation.go` — `CreateStack` / `DescribeStacks` /
      `DescribeStackEvents` / `DeleteStack`; maps the wait condition to `Status`.
- [X] T039 `internal/provider/aws/estimate.go` + `internal/provider/aws/pricing.go` — `pricing:GetProducts` and the
      hourly/monthly breakdown, exclusions included.
- [X] T040 `internal/provider/aws/aws.go` — assemble the adapter, expose the catalogue (regions,
      instance types) read from the template it ships, so the API and the template cannot drift.
- [X] T041 `cmd/dawnbx-server/main.go` — construct the adapter and inject it; the flagless default
      path must not load any AWS package.

### Installer and template

- [X] T042 `install.sh` — `--bootstrap-parameter NAME`; when set, install `awscli` on the existing
      apt line (install.sh:177-187) and `export DAWNBX_ADMIN_PASSWORD=…` before the existing
      server-identity block (install.sh:254-256). No new file under `<data-dir>/server`, so
      `verify.sh:13-14` still holds. Flag absent ⇒ byte-identical behaviour.
- [X] T043 `deploy/aws/dawnbx.yaml` — add `BootstrapParameter` (`String`, the parameter *name*),
      an `AWS::IAM::Role` + `InstanceProfile` replacing the "No IAM role either" comment at line 95,
      the role policy scoped to that one parameter ARN, and `Server.IamInstanceProfile`.
- [X] T044 `cfn-lint deploy/aws/dawnbx.yaml` and `shellcheck -S error install.sh` — both clean.

### Orchestration and routes

- [X] T045 `internal/cluster/client.go` — the client for a provisioned cluster's own API: TOFU TLS
      pin, `POST /v1/login`, `POST /v1/keys`. Never a generic proxy.
- [X] T046 `internal/cluster/provision.go` — the state machine, single-flight per cluster, the
      neutral phases, provider polling, and failure cleanup delegated to the provider.
- [X] T047 `internal/api/clusters.go` — `GET /v1/providers/{provider}/regions`,
      `/instance-types`, `/estimate`, `GET`/`POST /v1/clusters`, `GET /v1/clusters/{name}`,
      `GET /v1/clusters/{name}/credentials` (audited), `POST /v1/clusters/{name}/rotate`,
      `DELETE /v1/clusters/{name}`. `adminOnly` throughout; `{name}` and `{node}` only.
- [X] T048 openapi.yaml merge for the US1 paths, then `npm run gen`, both SDK read-only helpers,
      `docs/…/api.mdx` and `README.md` — one change, all five files.

### Dashboard

- [X] T049 `web/src/pages/clusters.tsx` — the wizard: provider step, configuration, price with
      its excluded-charges line, confirmation, live status, credential reveal and rotate.
      Reuse the `Grid` / `Section` / `CopyValue` idioms from `settings.tsx:80-141`.
- [X] T050 `web/src/main.tsx` **and** `web/src/test-support.tsx` — register `/clusters` in
      **both** route trees. They are duplicated with nothing checking they agree
      (main.tsx:22-38, test-support.tsx:55-75); one tree alone is a silent failure.
- [X] T051 `web/src/pages/shell.tsx` — add the Clusters nav link.
- [X] T052 `web/src/test-support.tsx` — a guard test that the two route trees declare the same
      paths, so the next route cannot land in only one.
- [X] T053 `npm run build --prefix web` and commit `internal/api/ui`.

**Checkpoint**: `quickstart.md` Tier 2, 3, 4 pass. A cluster exists, is `ready`, has a URL and a
pin, and its credentials came back over the API.

---

## Phase 4: User Story 2 — Understand and Recover From Provisioning Status (Priority: P2)

**Goal**: an operator follows phase, reads a real failure reason, and takes the next action —
without SSH and without a secret.

**Independent test**: `quickstart.md` Tier 5, plus the failure rows in the Tier 4 phase table.

### Tests for User Story 2

- [X] T054 [P] `internal/cluster/provision_test.go` — extend: every `cluster_ops` phase is
      appended in order; a 5 s poll reflects a transition within 60 s (SC-003); the wait
      condition's `Reason` lands in `detail` with no secret.
- [X] T055 [P] `internal/api/clusters_test.go` — extend: `GET /v1/clusters/{name}` exposes
      `status`, `phase`, `detail`; the dashboard never receives a secret in any of them.
- [X] T056 [P] `web/src/pages/clusters.test.tsx` — extend: each phase renders, a failure shows its
      reason and next action, and SSH appears only under an explicit rescue condition.

### Implementation for User Story 2

- [X] T057 `internal/provider/aws/cloudformation.go` — **own the rollback race.** Distinguish
      "the install reported failure" from "the stack is already rolling back", and settle the
      teardown before returning `Status{failed}`. The orchestrator must never learn the
      difference (research.md D10).
- [X] T058 `deploy/aws/dawnbx.yaml` — decide the create posture so the adapter owns teardown
      alone: `DisableRollback` on create, or an explicit wait for `ROLLBACK_COMPLETE`. Whichever
      is chosen, note it in the header comment at lines 6-8, which currently tells a *human* to
      re-create with rollback disabled.
- [X] T059 `internal/cluster/cluster.go` — expose the op history the dashboard reads; the last row
      per `(cluster, kind)` is the current phase.
- [X] T060 `web/src/pages/clusters.tsx` — phase timeline, failure reason, and the next supported
      action; a rescue hint for an unusable host.
- [X] T061 openapi.yaml / `npm run gen` / both SDKs / `api.mdx` / `README.md` — the `phase` and
      `detail` fields reach every consumer in one change.
- [X] T062 `npm run build --prefix web` and commit `internal/api/ui`.

**Checkpoint**: US1 and US2 both work; a forced failure is explained in the dashboard and cleaned
up by the provider.

---

## Phase 5: User Story 3 — Scale a Cluster From the Dashboard (Priority: P3)

**Goal**: add and remove a worker from the dashboard, with the removal guard.

**Independent test**: `quickstart.md` Tier 4 node steps, including the 409.

### Tests for User Story 3

- [X] T063 [P] `internal/provider/aws/compute_test.go` — `RunInstances` reuses the stack's launch
      template; `TerminateInstances`; a node carrying sandboxes is refused before any terminate
      call is made.
- [X] T064 [P] `internal/cluster/nodes_test.go` — add/remove against a fake provider; the 409
      from the cluster's own `DELETE /v1/nodes/{name}` is surfaced as `node_holds_sandboxes`
      with the count, and is never overridden by the cached count.
- [X] T065 [P] `internal/api/clusters_test.go` — extend: `503 cluster_unavailable` while the
      cluster is not ready; `409` on removing a busy node.
- [X] T066 [P] `web/src/pages/clusters.test.tsx` — extend: the node panel adds, polls, and refuses
      a busy removal with the reason shown.

### Implementation for User Story 3

- [X] T067 `internal/provider/aws/compute.go` — EC2 worker lifecycle, launch template and security
      group taken from the cluster's own `Handle`.
- [X] T068 `deploy/aws/dawnbx.yaml` — add the `LaunchTemplateId` output, and a worker-to-server
      6443 rule from the VPC CIDR (`SgK3s` is self-referencing only today, lines 74-76).
- [X] T069 `internal/cluster/nodes.go` — add and remove, going through the cluster's own
      `GET /v1/nodes/join` and `GET /v1/nodes` so no k3s knowledge enters the provider layer.
- [X] T070 `internal/api/clusters.go` — the four node routes.
- [X] T071 openapi.yaml / `npm run gen` / both SDKs / `api.mdx` / `README.md` — one change.
- [X] T072 `web/src/pages/clusters.tsx` — the node panel, following the `Nodes` tab precedent at
      `settings.tsx:515-581`.
- [X] T073 `npm run build --prefix web` and commit `internal/api/ui`.

---

## Phase 6: User Story 4 — Choose a Supported Provider (Priority: P3)

**Goal**: AWS is available, GCP and Azure are visible and unselectable, and no non-AWS resource can
be created.

**Independent test**: `quickstart.md` Tier 2 provider list; SC-006 in T077.

### Tests for User Story 4

- [X] T074 [P] `internal/provider/provider_test.go` — extend: the registry reports only `aws` as
      available, and `GET /v1/providers` returns the other two with `available:false`.
- [X] T075 [P] `internal/api/clusters_test.go` — extend: `{provider}` = `gcp` on every
      provider-scoped route is `400 provider_unavailable`, never an empty list and never a
      partial result.
- [X] T076 [P] `web/src/pages/clusters.test.tsx` — extend: GCP and Azure render disabled and
      cannot be activated.

### Implementation for User Story 4

- [X] T077 `internal/api/clusters.go` — resolve `{provider}` through the registry; an unavailable
      provider is `400 provider_unavailable` on every route, and `POST /v1/clusters` rejects any
      non-`aws` provider id (SC-006).
- [X] T078 `web/src/pages/clusters.tsx` — the provider step renders from `GET /v1/providers`, not a
      hard-coded list.
- [X] T079 openapi.yaml / `npm run gen` / both SDKs / `api.mdx` / `README.md` — one change.
- [X] T080 `npm run build --prefix web` and commit `internal/api/ui`.

---

## Phase 7: Polish & Cross-Cutting Concerns

- [X] T081 `AGENTS.md` — the cluster-management paragraph, and the note that
  `.specify/memory/constitution.md` v1.1.0 now carries a provider-neutrality constraint every
  change is checked against.
- [X] T082 `docs/content/docs/guide/nodes.mdx` — point at the new node routes instead of only
  the UI, and close its dangling "Next" link.
- [X] T083 Raise `hack/coverage-floor.txt` in whatever commit raised the measured coverage. Never
      lower a floor.
- [X] T084 `CHECK_LIVE=1 bash hack/check.sh` — required, because `install.sh` changed. Confirms
  the flag-absent path is untouched (`verify.sh:13-14`).
- [X] T085 Run `quickstart.md` Tier 2, 3, 4, 5 end to end on a throwaway account, and record the
  result in the PR body. Tier 4 is the only proof of SC-001…SC-010.
- [X] T086 Final `bash hack/check.sh`, green, on a clean tree.

---

## Dependencies & Execution Order

### Phase Dependencies

- **Setup (Phase 1)**: nothing blocks it.
- **Foundational (Phase 2)**: depends on Phase 1 and **blocks every user story**.
- **User Stories (3–6)**: depend only on Phase 2. US1 is the MVP; US2 hardens it; US3 and US4 are
  independent of each other and of US2.
- **Polish (Phase 7)**: depends on the stories being implemented. T084 additionally depends on
  T042 and T043.

### Within Each User Story

- Tests first, and they must fail before the implementation.
- Adapter before orchestration, orchestration before routes, routes before the dashboard.
- Contract changes land in the same commit as the routes they describe.

### Parallel Opportunities

- T002 and T003 (Phase 1).
- T005, T006 and the T017–T021 contract group in Phase 2.
- All `*_test.go` tasks inside a story are independent of each other, but each depends on the
  interface in T002 existing.
- US3 and US4 can run concurrently once Phase 2 is green.
- T049, T050 and T051 touch different files but T050 must be one edit across two files.

### MVP Cut

Phases 1 + 2 + 3. That is a control plane, an AWS adapter, and a working SSH-free create flow
with credentials — enough to stop using SSH for the happy path.

---

## Notes

- **[P]** = different files, no dependencies. T050 is the exception: it must be one edit across
  `web/src/main.tsx` **and** `web/src/test-support.tsx`.
- Four traps, all cited in `plan.md`, that a task that skips its citation will walk into: the
  `{id}` path-parameter trap, the duplicated route tree, `NoEcho` not protecting `UserData`, and
  the `/v1/nodes` doc drift.
- The `contracts/openapi-delta.yaml` file is a merge source, not a contract. It is never shipped
  and never served.
- If a task turns out to need a change to `research.md` D1 or D10, stop: that is a design change,
  not an implementation one, and it goes back through the spec.

---

## Phase 8: Convergence

Produced by `/speckit.converge` after the implementation. Every item below is a gap between
`spec.md` / `plan.md` / the contract and the code as it stands, found by reading the code rather
than by trusting a checked box. Ordered CRITICAL, then HIGH, then MEDIUM, then LOW.

### CRITICAL

- [X] T087 Serve the phase-one provider roster from `GET /v1/providers` so `aws` is available
      and `gcp`/`azure` are listed as unavailable, with no adapter behind them (FR-002, US4/AC1-2,
      SC-006) (missing). Today `internal/api/clusters.go:149-153` returns one entry built from the
      single injected adapter, so User Story 4 never occurs against a real server: the picker shows
      one button, and only a test fixture ever supplies the disabled rows. Also decide whether
      `provider.Registry` is the mechanism or is removed (C25).

### HIGH

- [X] T088 Delete the stray `clusterName` and `provider` keys that sit as direct children of
      `components` in `internal/api/openapi.yaml:574-575` — the document is not valid OpenAPI 3.1
      — then validate it with a spec linter rather than relying on `npm run gen` surviving it
      (Constitution III, one contract) (contradicts).
- [X] T089 Reconcile `InstanceTypePrice.region` with the code: either add `region` to
      `provider.HostSize` and set it in the adapter, or drop it from `required` and document the
      top-level `region` the handler already returns. Today the contract requires a field nothing
      marshals, which makes `web/src/pages/clusters.tsx:392` dead (contract, SC-009) (contradicts).
- [X] T090 Remove `public_ip` from the `Cluster` schema in `internal/api/openapi.yaml` and in
      `contracts/openapi-delta.yaml`, and delete the dead branch at
      `web/src/pages/clusters.tsx:481-483`. The value lives only inside the adapter's opaque
      handle, and the contract states the only provider facts the API exposes are `provider` and
      `tls_pin` (Constitution, provider neutrality) (contradicts).
- [X] T091 Render a provisioning cluster's `detail` whenever it is non-empty and the cluster has
      not failed, and add a needs-attention affordance for a cluster whose phase has stopped
      advancing. `internal/cluster/provision.go:143,193` write that detail while still
      provisioning, and the status panel never shows it, so an unreachable cluster reads as a
      healthy one (US2/AC1, FR-008) (partial).

### MEDIUM

- [X] T092 Read `Provisioner.StaleAfter`, which is declared and defaulted but read nowhere, and
      mark a cluster whose phase has not advanced as needing attention; or delete the field and
      the sentence that promises it (edge case, "exceeds its expected duration") (missing).
- [X] T093 Revoke the superseded API key on the cluster during rotation, or amend the spec edge
      case to say the old key is left in place and say so in the dashboard. Rotation currently
      mints a new key under the same name and revokes nothing, and `Remote` has no revoke method
      although the cluster exposes `DELETE /v1/keys/{key}` (edge case, FR-006) (partial).
- [X] T094 Expose `Capabilities.Delivery` on a response and render it on the cluster status page,
      or delete the field and the claim that the dashboard reports the mechanism. It is
      write-only today (research D10) (partial).
- [X] T095 Record the `add_node`, `remove_node`, `delete` and `rotate` operation kinds the data
      model defines, and expose the history it says the "what happened" view reads; or drop `kind`
      and the unused `Registry.Ops` reader. Only `create` is ever written and nothing reads the
      history in production (data-model.md, FR-008) (partial).
- [X] T096 Reject `0.0.0.0/0` as an SSH CIDR in `awsprov.New` and tighten the template's
      `SshCidr` pattern. The adapter's own documentation says that value would make the rescue
      path a permanent one, and only non-emptiness is checked (Operational Constraints, host
      exposure) (missing).
- [X] T097 Gate the Settings **Nodes** tab on the control-plane capability probe, or surface its
      503. In control-plane mode it polls a cluster-bound route every five seconds and renders an
      empty grid with no error, which reads as a working feature (FR-001, US1/AC1) (partial).
- [X] T098 Keep polling while a cluster is `deleting`, not only while it is `provisioning`, or the
      screen sits on "deleting" for ever because the first refetch ends the poll (SC-003)
      (partial).
- [X] T099 Decide the dashboard delete-cluster flow explicitly: either scope it into the spec with
      a requirement, or remove the button and rewrite the failure panel's next step, which
      currently depends on it. The spec called this flow out of scope and no FR asked for it, so
      leaving it undocumented makes a later contract review treat it as intended surface (spec
      Assumptions) (unrequested).
- [X] T100 Document `409 quote_stale` and `400 provider_unavailable` on `POST /v1/clusters` in
      `internal/api/openapi.yaml`, which is the only cluster operation whose description omits its
      non-2xx outcomes (contract) (partial).
- [X] T101 Add the nine missing cluster routes to the `README.md` table, or state that the cluster
      section is a deliberate subset. The file calls itself a copy of the docs with nothing keeping
      the two equal, and the copy has drifted (contract) (partial).
- [X] T102 Correct `docs/content/docs/guide/api.mdx` to say a dashboard session, with admin
      required for everything except the capability probe and the provider list; or add
      `adminOnly` to those two handlers. Both routes call `signedIn` only (contract, docs vs
      code) (contradicts).

### LOW

- [X] T103 Add `phase`, `detail`, `url` and `tls_pin` to the `Cluster` `required` list: the struct
      always marshals them, and leaving them optional forces guards in the dashboard for fields
      that are always present (contract) (partial).
- [X] T104 Narrow the `ClusterNode.status` enum to the states the API can return, or earn the
      other three. The dashboard polls on `removing`, which is never written (contract) (partial).
- [X] T105 Drop `enum: [aws]` from the `provider` path parameter, which makes the contract's own
      documented `400 provider_unavailable` unreachable from a generated client and forces a cast in
      the dashboard (contract) (contradicts).
- [X] T106 Either add the `internal/cluster` import scan that `AGENTS.md:17` claims exists, or
      correct that sentence: only `TestPackageImportsNoAdapter` reads imports today, and it covers
      `internal/provider` alone (Constitution, provider neutrality) (contradicts).
- [X] T107 Qualify `AGENTS.md:12`'s blanket "never a provider's resource names": a worker is
      identified by the value the adapter returned, which is rendered in the dashboard. Use the
      wording `contracts/cluster-routes.md` already uses (Constitution, provider neutrality)
      (contradicts).
- [X] T108 Document `DAWNBX_BOOTSTRAP_PARAMETER` in `install.sh --help`, or drop the env default so
      `--bootstrap-parameter` is the only gate. Research D2 says the flag alone gates the path
      (research D2) (contradicts).
- [X] T109 Fix the comment at `internal/provider/aws/compute.go:30-33`: a worker inherits the
      launch template's IMDS settings, not an instance profile, because the profile is set on the
      instance rather than the template (contradicts).
- [X] T110 Take the lowest matching on-demand price rather than the first, or amend research D9 to
      say "the first matching term" (research D9) (partial).
- [X] T111 Resolve the dead code Principle V forbids: `provider.Registry`, `Provider.StatusNode`,
      `nodeFailed` and `nodeRemoving` have no production caller (Constitution V) (unrequested).
- [X] T112 Move worker add/remove orchestration out of the HTTP layer into `internal/cluster`, or
      amend `plan.md`'s Project Structure, which lists a `nodes.go` that does not exist and
      contradicts its own rationale for where orchestration lives (plan, structure) (partial).
- [X] T113 Make the route-tree guard test compare components as well as paths, or say in its
      comment that it compares only paths, so a route present in both trees with a different
      component is not mistaken for agreement (web, tests) (partial).

### Defects found while implementing, not gaps in this spec

These were found by running the thing rather than by reading the artifacts, so they have no
`FR-###` to trace to. Three are in code this feature added; one is pre-existing and unrelated to
it. Appended here so they are fixed in the same pass rather than carried into another session.

- [X] T114 Stop a store failure looking like a missing cluster. `Registry.Get`
      (`internal/cluster/cluster.go:280`) wraps every store error as `ErrNotFound`, so a locked or
      unreadable database becomes a `404 not_found` at the API. Wrap the cause with `%w` and let
      the handler distinguish absence from failure, so a database problem is not reported to an
      operator as "no such cluster" (defect).
- [X] T115 Return the record as it stands after a failed create. `Provisioner.Begin`
      (`internal/cluster/provision.go:100-101`) calls `p.fail`, which marks the cluster failed, and
      then returns `c`, the record read before the failure — so a caller that renders the returned
      value shows `provisioning` for a cluster that is already failed with a reason attached. Re-read
      after `fail`, and cover it with a test (defect).
- [X] T116 Keep the SDK's `Client` exported, and say why. The finding was wrong: this is not an
      unrequested public change made to serve a test, it is a broken API this feature introduced.
      The cluster helpers `listClusters`, `getCluster` and `listClusterNodes` are exported and
      take a `Client`, and there is no factory that hands one out — so with `Client` package-private
      they were uncallable from outside the package. The export is now documented with that reason
      at its definition, and `Sandbox` stays the way to *use* a cluster (defect, real).
- [ ] T117 Enforce the per-sandbox disk cap, or state honestly that it is not enforced.
      `hack/verify.sh`'s `quota: 80MB write into 50MB cap fails` fails, and the cause is larger
      than the check: `store.Meta.ProjectID` is declared (`internal/store/store.go:60`) and read
      nowhere in production, and every `setquota`/`repquota` call in the repository is inside
      `hack/verify.sh` itself. Nothing in the product ever assigns a project id or sets a quota, so
      the cap `AGENTS.md` and the docs promise is not applied. Diagnose first — assign a project id
      per sandbox, set the quota, and make the pod write into a directory carrying it, or amend the
      isolation constraint and the docs to say what actually happens. Note the installer already
      documents a fallback, "a 30 s du check" (`install.sh:242-243`), and this feature changed
      nothing in that path (defect, pre-existing).
