<!--
Sync Impact Report — 2026-09-27

Version change: 1.2.0 → 1.3.0 (MINOR)

Modified principles:
- II. Isolated Work, Explicit Remote Access — title unchanged. Body expanded:
  worktrees are created under `.worktree/` inside the primary checkout, and
  deletion of the worktree and branch after merge is now mandatory. The stated
  path `../dawnbx-<task>` no longer matched practice and had come to contradict
  AGENTS.md, which Principle II's own workflow section requires to be corrected
  in the same change as this file.

Added sections:
- VI. Only Real Infrastructure Proves Provisioning — new principle. Records that
  the gate executes nothing, that no cloud emulator may stand in for a real
  account, and that cutting a release requires the real-account lifecycle.
- Development Workflow — two steps added: delete the worktree and branch after
  merge; run the lifecycle in Principle VI before cutting a release.

Removed sections: none

Deferred:
- none
-->

# dawnbx Constitution

## Core Principles

### I. The Gate Is the Definition of Done

`bash hack/check.sh` MUST exit 0 before a commit is pushed. It runs lint for
every area (gofmt, staticcheck, golangci-lint, biome, eslint, prettier, ruff),
both builds, all four test suites, the coverage floors, and the dashboard-bundle
staleness check. A failing step MUST be fixed at its cause; it MUST NOT be
skipped, narrowed, or bypassed with `--no-verify`.

The coverage floors in `hack/coverage-floor.txt` are a ratchet. They MAY rise
and MUST NOT fall. Raising a floor is part of the same commit that raises the
coverage. Deleting a check to obtain a green push is forbidden.

Rationale: the gate has caught four defects that would otherwise have shipped —
a React effect whose dependency array a linter autofix silently emptied, stale
dashboard bundles, test files leaking utilities into the shipped stylesheet, and
a missing argument to an API method. None were visible by reading the code.

The gate is necessary and not sufficient. Principle VI is the other half.

### II. Isolated Work, Explicit Remote Access

Every development task runs in its own git worktree, branched from `main` and
created under `.worktree/` inside the primary checkout, which is gitignored.
Work MUST NOT be edited, built, tested, or committed in the primary checkout,
and MUST NOT be committed directly to `main`. A hotfix straight to `main` is the
only exception and MUST say why in the commit body.

A task's worktree and its branch MUST both be deleted once the work is merged.
Governance changes travel in that same merge and MUST be made before the
deletion: a change committed to a branch that is then removed never reached
`main`.

`git push` and opening a pull request MUST NOT happen without an explicit
instruction in the current session. Branches and merges stay local by default;
the remote is the operator's decision, every time.

Rationale: an agent working in the primary checkout leaves unreviewable state
where the operator's own uncommitted work lives — which happened twice in one
session, both times while the agent believed it was working correctly. Remote
writes are irreversible in effect and cheap to defer.

### III. One Contract, Five Files

`internal/api/openapi.yaml` is hand-written and is the ONLY shared contract
between the server, the dashboard, both SDKs, and the docs. Adding or changing a
route means, in one change: edit the schema, regenerate the dashboard's types,
update the Python SDK, update the TypeScript SDK, and update the docs page.

The generated dashboard types are committed, and the built dashboard bundle is
committed because the Go server embeds it. Generated artifacts MUST be
regenerated and committed, never hand-edited. No CI step can verify this
agreement, so it is the author's responsibility at review time.

Rationale: the five surfaces are updated by hand and nothing checks them, so
drift is silent. Every copy of the route table is a place the product can lie.

### IV. A Behaviour Change Ships With a Test

New behaviour MUST arrive with a test that asserts observable behaviour, not
implementation shape. A bug fix MUST arrive with a test that fails before the
fix and passes after it. Snapshot assertions, existence-only assertions, and
tests that merely prove a mock was called are not acceptable substitutes.

Any change under `web/` MUST be accompanied by `npm run build --prefix web` and
the rebuilt `internal/api/ui` in the same commit, because the server ships the
committed bundle.

Rationale: two of the three regressions found in this project were invisible to
review and to every linter, and were found only because a test existed. The
third, a linter autofix changing behaviour, was found by the gate noticing a
stale bundle.

### V. No Speculative Infrastructure; Pin What You Verify

Build what was asked. Infrastructure with no consumer MUST be deleted, and a
second way to do something the primary path already does MUST NOT be added. Where
a path exists but has never been executed, that fact MUST be written where a
reader will find it — in the README, the docs, or `AGENTS.md` — not only in a
commit message.

Tool and action versions MUST be pinned to what was verified locally. CI pins
linters to the versions the gate was developed against, deliberately, so a
green run means the same thing on a runner as on the operator's machine.

Rationale: an unlaunched deployment path and a floating linter version both
produce false confidence. The CloudFormation template was labelled correctly
only after it was nearly deleted as dead weight.

### VI. Only Real Infrastructure Proves Provisioning

`bash hack/check.sh` executes nothing. A green gate MUST NOT be reported as
evidence that provisioning works. Every defect found in the control plane was
found by running it against a real account: a launch request missing an image id,
a price read from the wrong field of a price dimension, user-data that was not
base64-encoded, an ingress controller that answered on 443 through iptables
while the intended server still listened on its socket, and a worker that its
cluster and its provider named differently with no way to correlate the two.

A test double, or a cloud emulator adopted alongside the code, MUST NOT stand in
for a real account. Such a substitute encodes a model of the cloud, and a model
shared with the implementation agrees with the implementation's bugs. No local
AWS, GCP, Azure, or OCI emulator may be adopted as an integration tier, and no
tier that finds a strict subset of what a mandatory run already finds may
displace that run.

Only a real account exercises the provisioning seam: the installer on a fresh
distribution, the sandbox runtime loading, contention for ports 80 and 443,
certificate issuance, pinning against a served certificate, public name
resolution and address assignment, and user-data crossing the instance boundary.
These are not a coverage gap to be closed by more tests. They are the product.

Before a release is cut, the lifecycle MUST be executed against a real account:
create a cluster, wait for it to report ready, confirm the pinned URL serves and
its pin matches, add a worker, wait for it to report ready, remove the worker,
delete the cluster, and confirm no billable resource remains. A release MUST NOT
be cut on a green gate alone. Until that lifecycle is scripted, running it is a
manual obligation and MUST be stated when the release is cut.

Rationale: ten defects in a single session, none visible to a green gate, and
all cheap to fix once found. A faster tier that finds four of them is a way to
skip the run that finds all of them while feeling quick.

## Operational Constraints

- **Cluster boundary.** The control plane is the dashboard, API, and database.
  Sandboxes run only inside clusters. SDKs and the CLI talk to a cluster URL
  directly and MUST NOT route through the control plane. A sandbox call against
  a control plane is `503 cluster_unavailable`, never a `404` and never a proxied
  request.
- **Secrets.** API keys and admin passwords MUST be generated by the control
  plane and passed in, never generated on a host and read back over SSH. Secret
  values MUST NOT appear in stack outputs, CI logs, tickets, or screenshots.
- **Host exposure.** Instance metadata MUST be unreachable from workloads —
  IMDSv2 with hop limit 1 on AWS today. k3s (6443), kubelet (10250), and
  WireGuard (51820) are reachable only between cluster nodes, never from the
  public internet. Public 6443 is forbidden.
- **Isolation.** Each sandbox gets gVisor, a NetworkPolicy that blocks the
  node's instance metadata endpoint, and a 5 GB disk cap. The cap is enforced by
  a 30 s measurement, so a sandbox MAY overshoot briefly before it is stopped;
  where the data volume was formatted with ext4 project quotas the kernel
  refuses the write instead. A sandbox is removed with its files; a node cannot
  be removed while it holds sandboxes.
- **Cost.** Launching a host costs money, on every cloud. The operator MUST see
  the price before a cluster is created, and no paid resource is created without
  instruction.
- **Provider neutrality.** dawnbx consumes *a server* from a cloud. That host is
  the entire product concept; nothing else in a provider's catalogue is. The
  provider boundary MUST carry values and intent, never provider identifiers.
  A cloud's resource names, orchestration model, and secret-delivery mechanism
  MUST NOT appear in a shared interface, a shared contract, a stored entity, or
  a response body. A provider is a value in an interface parameter or a path
  segment, never a path family of its own. Every capability the control plane
  needs MUST be expressed in terms the provider can satisfy: make a host,
  deliver one secret to it, report whether it is ready and its URL, report what
  it costs, and destroy it. A provider's own rollback, wait conditions, and
  failure reporting MUST stay inside its adapter, and the orchestrator MUST NOT
  wait for a mechanism the next provider may not have. Where a provider cannot
  offer a guarantee another can, that ceiling MUST be documented where an
  operator will read it, and the operator MUST NOT be told a stronger
  guarantee than the one in force.
  One exception is deliberate and is the only one: a **worker** is named by the
  handle its adapter returned, because the route that removes it needs an id the
  operator can type. That handle MUST stay opaque — never parsed, never
  interpreted, never given a meaning above the provider boundary — and wherever
  it appears the contract MUST say so in the same words.

## Development Workflow

1. Read `AGENTS.md` for the per-area contracts. It is the operational expression
   of this constitution; where the two disagree, the constitution wins and both
   MUST be corrected in the same change.
2. Create a worktree: `git worktree add .worktree/<task> -b task/<slug> main`.
3. Install dependencies once per worktree: `npm ci --prefix web`,
   `npm ci --prefix sdk/typescript`, `npm ci --prefix docs`, and
   `python3 -m venv .venv && .venv/bin/pip install coverage`.
4. Implement, with tests. Run the affected suites while iterating.
5. Run `bash hack/check.sh`. It MUST be green.
6. For sandbox, k8s, or quota changes, run the live tier on demand:
   `CHECK_LIVE=1 bash hack/check.sh`. It installs into a Lima VM and runs
   `hack/verify.sh` inside it against real gVisor.
7. Commit on the task branch. Merge to `main` locally. Push only on instruction.
8. Delete the worktree and its branch: `git worktree remove .worktree/<task>`
   then `git branch -d task/<slug>`.
9. Before cutting a release, run the lifecycle in Principle VI against a real
   account and state in the release that it was run.
10. CI runs the same gate on every push and pull request, so a hook-less clone
    is held to the same floors.

## Governance

This constitution supersedes informal practice, prior habits, and convenience.
`AGENTS.md` and `hack/check.sh` are its operational expression: they MUST be
updated in the same change that amends the principles they encode.

**Amendment procedure.** Amend this file directly, with the rationale in the
commit body, and regenerate the Sync Impact Report. Amendments that weaken a
principle, remove one, or redefine a term are backward incompatible and MUST
carry a migration plan for existing code.

**Versioning.** Semantic versioning on the constitution itself:
- MAJOR — a principle removed, redefined, or weakened.
- MINOR — a principle or section added, or guidance materially expanded.
- PATCH — clarification, wording, or typo fixes that change no requirement.

**Compliance.** Every review MUST verify the change against Principles I
through VI and the Operational Constraints. The gate is the mechanical half of
compliance and MUST be green; the judgement half — whether the tests assert
behaviour, whether the five contract files agree, whether new infrastructure
has a consumer, whether anything provider-specific has leaked above the provider
boundary, and whether a claim about provisioning rests on something other than a
real run — is the reviewer's, and MUST be stated in the review rather than
assumed.

**Review expectations.** A reviewer who cannot verify a principle from the diff
and the gate output MUST say so instead of approving. Complexity that violates
Principle V MUST be justified in writing at the time it is introduced. A reviewer
MUST NOT accept a stated test tier as coverage for the provisioning seam, on the
strength of a passing run, without asking what that tier actually executed.

**Version**: 1.3.0 | **Ratified**: 2026-09-26 | **Last Amended**: 2026-09-27
