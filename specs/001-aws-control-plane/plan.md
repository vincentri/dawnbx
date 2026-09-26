# Implementation Plan: SSH-Free AWS Control Plane

**Branch**: `task/aws-control-plane` | **Date**: 2026-09-26 | **Spec**: [spec.md](./spec.md)

**Input**: Feature specification from `specs/001-aws-control-plane/spec.md`

## Summary

Add a control-plane mode to `dawnbx-server` that runs the dashboard, auth, and database with no
Kubernetes cluster and no `.dawnbx-volume` marker, and drives cloud cluster provisioning entirely
from that dashboard.

**Provider neutrality is the organising constraint.** dawnbx consumes *a server* from a cloud — an
EC2 instance today — and nothing else in that cloud's catalogue is a product concept. The provider
interface carries values and intent (`ClusterSpec`, `Bootstrap`) plus an opaque `Handle`, never a
provider identifier. The AWS SDK, CloudFormation, SSM, EC2 and Pricing live in
`internal/provider/aws`, which `internal/cluster` cannot import (D10 in `research.md`, enforced by
two tests).

The control plane injects exactly one secret — the cluster's admin password — as a `Bootstrap` the
provider delivers by whatever means it has: an SSM Parameter Store `SecureString` on AWS, read at
boot by an instance role scoped to that one parameter, so the password appears in no stack output,
no template and no user-data. The cluster's **API key is never injected**: once the host is up, the
control plane logs into the cluster's own API over a pinned certificate and mints the key there.
Status comes from polling the provider, so the control plane needs no public inbound route. Worker
joins and sandbox-aware node removal go through that same cluster API, so no k3s-specific knowledge
leaks into the provider layer.

## Technical Context

**Language/Version**: Go 1.27 (`go.mod:3`); TypeScript/React 19 (`web/package.json`); bash

**Primary Dependencies**: existing set plus AWS SDK for Go v2 —
`config` (standard credential chain: env, shared config/SSO, web identity), `service/cloudformation`,
`service/ec2`, `service/ssm`, `service/pricing`, `service/sts`. No other new runtime dependency.

**Storage**: existing auth database — SQLite (`modernc.org/sqlite`) by default, Postgres via
`--database-url` — extended with three tables (see `data-model.md`)

**Testing**: `go test ./cmd/... ./internal/...`, `vitest run` (web), `unittest` (Python SDK),
`node --test` (TS SDK), all behind `bash hack/check.sh`; live tier `CHECK_LIVE=1`

**Target Platform**: Linux server for the control plane; arm64 Linux hosts on AWS

**Project Type**: single-binary Go service with an embedded SPA

**Performance Goals**: price estimate returns within 2 s; a status transition reaches the dashboard
within 60 s (SC-003); poll cadence 5 s while a cluster is in flight, and no polling at all once it is
settled (the provisioner asks only about in-flight clusters; the dashboard's `refetchInterval` returns
`false` for a settled one)

**Constraints**: no public inbound route on the control plane; no secret in CloudFormation outputs,
user-data, logs, or audit rows; the Go coverage floor (90.8%) is a ratchet that may only rise; the
dashboard bundle is committed and must be rebuilt in the same commit

**Scale/Scope**: one AWS account, one credential source, a handful of clusters in phase one

## Constitution Check

| Principle / constraint | Status | How the design satisfies it |
|---|---|---|
| I. The gate is the definition of done | PASS | Every task lists the gate; `hack/coverage-floor.txt` is raised, never lowered. New Go packages carry unit tests from the first task so the 90.8% floor is not traded away. |
| II. Isolated work, explicit remote access | PASS | Work happens in `../dawnbx-aws-control-plane` on `task/aws-control-plane`; no push, no PR. |
| III. One contract, five files | PASS | Cluster routes are added to `internal/api/openapi.yaml`, then `npm run gen --prefix web`, both SDKs, `docs/content/docs/guide/api.mdx`, and `README.md`. `contracts/openapi-delta.yaml` is a *delta to be merged*, not a second contract. |
| IV. A behaviour change ships with a test | PASS | New Go code ships with `httptest`/fake-client tests; each new page ships a co-located `*.test.tsx`; any `web/` change commits the rebuilt `internal/api/ui`. |
| V. No speculative infrastructure | PASS with one justified entry | Only AWS exists. GCP/Azure are two rows in a provider table, not stubs. The provider interface is the seam the spec asked for, not speculation. See Complexity Tracking. |
| Operational: cluster boundary | PASS | SDKs and the CLI keep talking to a cluster URL; the control plane never proxies sandbox traffic (FR-014). |
| Operational: secrets | PASS | The admin password is minted in the control plane and delivered as a `Bootstrap` the provider places in its own secret channel — an SSM `SecureString` on AWS, never generated on a host and never read over SSH. The API key is minted by the cluster itself and crosses nothing (FR-006, FR-007, SC-007). |
| Operational: host exposure | PASS | The existing template is reused; its `MetadataOptions` (IMDSv2, hop limit 1) and self-referencing security-group rules are unchanged. The new role grants only `ssm:GetParameters` on one parameter ARN. |
| Operational: isolation | PASS | `DELETE` on a worker holding sandboxes is refused by the cluster's own `DELETE /v1/nodes/{name}` (409); the control plane relays that refusal (FR-011). |
| Operational: cost | PASS | `POST /v1/aws/estimate` is a separate, side-effect-free call; `POST /v1/clusters` requires the returned quote id (FR-005, SC-009). |
| AGENTS.md single-file rule | PASS | `AGENTS.md` gains a short "cluster management" paragraph in the existing API section rather than a new file. |

**Post-design re-check**: the re-check is what produced the two amendments recorded in
`research.md` D1 and D10. An earlier draft injected the API key as well and gave the neutral
`clusters` entity five AWS columns; both leaked provider identity into the neutral layer, which
FR-013 forbids. After the amendment, no gate violation remains, and the single AWS SDK entry below
is the only addition to Principle V.

**Neutrality is verified, not asserted.** Two tests are part of this plan's deliverables:
`internal/provider` asserts its own non-test files import no adapter package, and
`internal/cluster` runs the whole orchestration path against a fake provider with no AWS
dependency. FR-013 is a design constraint, so it gets a mechanical check like any other.

## Project Structure

### Documentation (this feature)

```text
specs/001-aws-control-plane/
├── plan.md                       # this file
├── spec.md                       # /speckit.specify + /speckit.clarify output
├── research.md                   # Phase 0: ten decisions with alternatives
├── data-model.md                 # Phase 1: three tables, states, transitions
├── quickstart.md                 # Phase 1: end-to-end validation guide
├── contracts/
│   ├── cluster-routes.md         # human-readable route table (docs + README source)
│   └── openapi-delta.yaml        # verbatim fragment to merge into internal/api/openapi.yaml
├── checklists/
│   └── requirements.md           # spec quality checklist
└── tasks.md                      # Phase 2 output (/speckit.tasks)
```

### Source Code (repository root)

```text
cmd/dawnbx-server/
├── main.go                       # + --control-plane, --release-url, --control-plane-key;
│                                 #   serveControlPlane() branch; admin password source
└── serve_test.go                 # + control-plane startup tests (no marker, no k3s)

internal/
├── provider/
│   ├── provider.go               # provider-neutral interfaces: ClusterSpec, Bootstrap, Handle,
│   │                             #   Provider, registry. Imports no adapter package, ever.
│   ├── provider_test.go
│   └── aws/
│       ├── aws.go                # adapter: New(ctx, cfg), implements provider.Provider
│       ├── handle.go             # the AWS blob shape: stack, parameter, sg, launch template
│       ├── cloudformation.go     # stack create/poll/delete
│       ├── compute.go            # ec2: RunInstances/TerminateInstances/DescribeInstances
│       ├── parameters.go         # ssm: PutParameter/GetParameters/DeleteParameter
│       ├── pricing.go            # pricing: GetProducts
│       ├── credentials.go        # config.LoadDefaultConfig + sts identity check
│       ├── estimate.go           # hourly/monthly fixed-charge breakdown
│       └── *_test.go             # httptest-backed: no live AWS call in unit tests
├── cluster/
│   ├── cluster.go                # registry: Create/Get/List/SetStatus over the auth DB
│   ├── provision.go              # state machine, single-flight per cluster
│   ├── client.go                 # client for a provisioned cluster's own API (pinned TLS)
│   ├── secret.go                 # AES-256-GCM at-rest encryption of cluster credentials
│   └── *_test.go
├── auth/
│   └── auth.go                   # + clusters / cluster_credentials / cluster_ops / cluster_nodes
│                                 #   tables and their accessors
├── api/
│   ├── clusters.go               # provider-neutral routes, adminOnly
│   ├── api.go                    # + ControlPlaneHandler() for the cluster-less mux
│   └── openapi.yaml              # + cluster paths and schemas (the one contract)
└── store/store.go                # unchanged

web/src/
├── pages/
│   ├── clusters.tsx              # provider picker → config → price → status → nodes
│   └── clusters.test.tsx
├── main.tsx                      # + /clusters route
├── test-support.tsx              # + the same /clusters route (duplicated tree)
└── pages/shell.tsx               # + Clusters nav link; hide sandbox nav in control-plane mode

sdk/python/src/dawnbx/__init__.py # + read-only cluster helpers
sdk/typescript/src/index.ts       # + read-only cluster helpers
docs/content/docs/guide/api.mdx   # + ## Clusters route table
README.md                         # + route table rows (the hand-duplicated copy)
deploy/aws/dawnbx.yaml            # + BootstrapParameter, IAM role/profile, LaunchTemplateId output
install.sh                        # + --bootstrap-parameter; awscli only when it is used
```

**Structure Decision**: the repository is a single Go module with feature packages under
`internal/`, so this feature follows the existing shape rather than introducing a new layout. The
provider seam is its own package because FR-013 requires the interface to exist *before*
provider-specific code, and `internal/api` must not learn anything about AWS. `internal/cluster`
owns the records, the state machine and the client for a provisioned cluster; it depends on
`internal/provider` but never on `internal/provider/aws` (the adapter is injected at startup). The
cluster list, node list and operation history live in the existing auth database so one
`--database-url` story, one auth gate and one migration mechanism are reused instead of a second
store.

**Where node orchestration lives, and why.** The plan originally named an
`internal/cluster/nodes.go`; the build put add/remove in `internal/api/clusters.go` instead. That is
deliberate and this is the record of it. Adding or removing a worker is three steps — ask the cluster
for its join command or for the sandbox counts, call the provider, record the row — and the first
of those is an HTTP conversation with the cluster, which is what the route already owns. A separate
file would have been a second seam for the same three calls with no rule of its own. What
`internal/cluster` keeps is what other callers also need: `Registry.PutNode`, `SetNodeStatus` and
`DropNode`, so the records can be read and written without going through HTTP.

## Cross-Cutting Notes for Implementers

Four traps in the existing code that this feature must not walk into. Each is cited.

1. **Never name a path parameter `id` on a new cluster route.** `s.auth` treats any route with an
   `{id}` parameter as sandbox-scoped: it calls `s.M.Owner(id)` and returns 404 for a non-matching
   org (`internal/api/api.go:264-274`). The guard is `id != "" && !p.Admin`, so it is a
   member-session correctness issue rather than a universal one — and in control-plane mode `s.M`
   is nil, so it would panic for a member. Use `{name}` for clusters and `{node}` for workers,
   matching `/v1/nodes/{name}` (`internal/api/manage.go:316`).
2. **The route tree is duplicated.** A new page must be registered in *both*
   `web/src/main.tsx:22-38` and `web/src/test-support.tsx:55-75`, or it renders in the app but
   cannot be reached by `renderApp` in tests.
3. **CloudFormation `NoEcho` does not protect a value inside `UserData`.** `NoEcho` masks the
   parameter in the template and console views, but the *instance's* user-data attribute returns
   the plaintext to anyone with `ec2:DescribeInstanceAttribute`, and a secret placed on the command
   line also appears in `ps` and the cloud-init log. The password therefore travels as a secret-store
   reference the instance resolves itself, not as a template value.
4. **`/v1/nodes` is in `internal/api/openapi.yaml:293-316` but in neither doc table.** Adding the
   cluster node routes is the moment to close that pre-existing drift in
   `docs/content/docs/guide/api.mdx` and `README.md`, not to widen it.

## Complexity Tracking

| Violation | Why Needed | Simpler Alternative Rejected Because |
|-----------|------------|-------------------------------------|
| AWS SDK for Go v2 as a new direct dependency (Principle V: avoid dependencies) | Provisioning needs typed, retrying, correctly-signed calls to CloudFormation, EC2, SSM Parameter Store, Pricing and STS, exercised in unit tests against `httptest` servers. | Shelling out to the `aws` CLI would require it installed on every control-plane host, would make the orchestrator untestable without a fake binary, and would hand-roll JSON parsing for five services. Hand-rolling SigV4 over `net/http` re-implements the SDK badly. The six modules are the smallest set that covers the six services this feature actually calls; no other AWS service module is added. |

## Deliverable Boundaries

**In scope**: control-plane mode; the provider interface; the AWS adapter; the cluster API and
pages; credential minting, retention, reveal and rotation; worker add/remove; price estimate;
automatic cleanup of a failed cluster's resources; the installer and template changes needed to
deliver a pre-generated credential.

**Out of scope**: GCP and Azure beyond two disabled rows in the provider table; multi-AWS-account
support; a public control-plane endpoint; moving SDK/CLI traffic through the control plane; SSH
automation of any kind.
