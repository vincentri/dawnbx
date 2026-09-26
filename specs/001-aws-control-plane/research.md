# Research: SSH-Free AWS Control Plane

Phase 0 output. Every unknown in the plan's Technical Context is resolved here, with the
alternatives that were considered and why they lost. Citations are `path:line` against
`task/aws-control-plane`.

---

## D1 — How does a control-plane credential reach the new host?

**Decision.** The control plane mints exactly **one** secret — the cluster's administrator
password — and hands it to the provider as an opaque `Bootstrap`. The provider delivers it to the
new host by whatever means that provider offers; on AWS that is an SSM Parameter Store
`SecureString` written before the stack is created, with the instance's scoped IAM role reading it
at boot. The control plane never learns the mechanism, and no provider identifier crosses
`internal/provider`.

The cluster's **API key is not injected at all.** During the `verifying` phase the control plane
logs into the cluster with that password and mints the key through the cluster's own
`POST /v1/keys` (`internal/api/manage.go:111-112`).

**Rationale.** Two independent wins:

1. The key never crosses any channel. It is minted by the cluster that must accept it, so it is
   guaranteed valid and lands in the cluster's own key table as a `sha256`
   (`internal/auth/auth.go:216-227`) — listable, revocable and auditable in the cluster's
   dashboard, rather than existing only as an imported hash the cluster cannot see.
2. `install.sh` needs no credential-injection code at all. `DAWNBX_ADMIN_PASSWORD` is already
   honoured and already written to `$SRV/admin.env` (`install.sh:254-256`) — the same path
   `SshCommand` documents for the human case (`deploy/aws/dawnbx.yaml:141`). We reuse a working
   mechanism instead of adding a second one beside it.

**Alternatives considered.**

| Alternative | Why rejected |
|---|---|
| Inject both the key and the password through the same parameter | Two secrets crossing a channel when one suffices, plus installer code to write `api-keys.json` from a fetched value (`install.sh:239-247` has no such branch today). |
| `NoEcho: true` CloudFormation parameter passed into `UserData` | `NoEcho` masks the parameter in the template and console views, but the **instance's** user-data attribute returns the plaintext to anyone with `ec2:DescribeInstanceAttribute`, and a secret placed on the command line also appears in `ps` and the cloud-init log. That is the "resource outputs" exposure SC-007 forbids. |
| Read the generated secrets back over SSH | This is the behaviour being removed. FR-012 and the whole product premise. |
| Generate on the host, return through the `WaitCondition` payload | `Data` is not retrievable by the stack creator, and cloud-init output is readable via `ec2:GetConsoleOutput` — the exact risk `install.sh:507-514` already avoids by refusing to print secrets without a TTY. |
| Secrets Manager instead of SSM | Same guarantee, more IAM surface and a higher minimum instance-role cost. |

**Consequences to implement (AWS adapter only).** The template gains a `BootstrapParameter`
(`String`, not sensitive), an `AWS::IAM::Role` + `AWS::IAM::InstanceProfile` replacing the current
"No IAM role either" comment at `deploy/aws/dawnbx.yaml:95`, and `Server.IamInstanceProfile`. The
role policy resource is built inside the template from the parameter name, so the control plane
never calls the IAM API. `install.sh` gains `--bootstrap-parameter NAME` whose only job is
`export DAWNBX_ADMIN_PASSWORD=…` before the existing server-identity block runs.

---

## D2 — How does `install.sh` read the parameter?

**Decision.** Add `awscli` to the existing `apt-get install` line, but only when
`--bootstrap-parameter` is passed.

**Rationale.** `install.sh:177-187` already installs five packages from Ubuntu's own repositories
with no version pinning, and already treats that list as a moving target. Adding one more package
to a list that exists is the smallest change that keeps the existing install path and the live Lima
tier byte-identical when the flag is absent. The flag-gated install is what preserves
`hack/verify.sh`'s postconditions (`verify.sh:13-14`) for every existing invocation.

**Alternatives considered.**

| Alternative | Why rejected |
|---|---|
| Download `awscli-exe-linux-aarch64.zip` with a pinned SHA-256 | More faithful to the `install.sh:431-443` release-checksum pattern, but adds architecture branching, a pinned digest to maintain, and a large download for one parameter read. |
| Hand-rolled SigV4 in `curl` against the SSM API | ~50 lines of shell that must get canonical request signing, credential expiry and IMDSv2 token refresh exactly right. Security-critical shell with no test harness. |
| Use the pre-installed SSM agent | `amazon-ssm-agent` is present on the Ubuntu AMIs but exposes no shell API; `aws ssm` is a separate client. |

**Constraint carried forward.** A `SecureString` must be fetched with `--with-decryption`, and the
value must reach `$SRV/admin.env` only through the existing branch at `install.sh:254-256`, under
the `umask 077` set at `install.sh:234-235`, so `verify.sh:13` still holds. No new file is written
in `<data-dir>/server`, so `verify.sh:14`'s `api-key.pending` assertion is untouched.

**Provider neutrality, restated.** Nothing in D1 or D2 is visible above `internal/provider/aws`.
`ssm:GetParameters`, `SecureString`, the instance profile, the parameter ARN and
`--bootstrap-parameter` are all AWS details. A second provider supplies its own delivery — a secret
manager and a scoped service account on GCP, Key Vault and a managed identity on Azure — behind the
same `Bootstrap` value. The alternative, a `NoEcho` parameter, would have put an AWS template
concept into the neutral interface instead.

## D3 — AWS access from the control plane: SDK or CLI?

**Decision.** AWS SDK for Go v2, modules `config`, `service/cloudformation`, `service/ec2`,
`service/ssm`, `service/pricing`, `service/sts`.

**Rationale.** `config.LoadDefaultConfig` *is* the standard AWS credential chain the spec names
(FR-003): environment, shared config files, named profiles, SSO, and web identity. Each client is
configured with an injectable HTTP client, so the whole adapter is exercised against `httptest`
servers in unit tests — which is the only way this keeps the Go coverage floor, since no test may
call AWS.

**Alternatives considered.**

| Alternative | Why rejected |
|---|---|
| Shell out to the `aws` CLI | Requires the CLI on every control-plane host, makes the orchestrator untestable without a fake binary, and hand-parses JSON from five services. The installer needs the CLI on a *fresh* box; the server does not. |
| `aws-sdk-go` v1 | Superseded and in maintenance. |
| Raw HTTPS with hand-written SigV4 | Re-implementing the SDK, badly, in the part of the code that spends money. |

**Bounded to six modules.** EC2 alone is used for workers; IAM is declared inside the CloudFormation
template (D1) so the IAM SDK module is not needed; EC2 Instance Connect and the pricing "savings
plan" APIs are not used.

**Provider neutrality.** This decision is the one place an AWS SDK is justified, and it is
confined to `internal/provider/aws`. `internal/provider` and `internal/cluster` import no AWS
package (D10).

---

## D4 — How does the server run with no cluster?

**Decision.** A separate `serveControlPlane()` branch, and a separate `ControlPlaneHandler()` mux.
The control-plane mux registers the identity surface (`s.manage`) and the new cluster routes, and
registers an explicit `503 cluster_unavailable` for the two cluster-bound path families
(`/v1/sandboxes`, `/v1/sandboxes/`, `/v1/status`, `/v1/nodes`, `/v1/nodes/`).

**Rationale.** `serve()` opens the store at `main.go:122`, whose `store.Open` refuses a data dir
without the `.dawnbx-volume` marker (`internal/store/store.go:71-76`), and builds the k3s client at
`main.go:146`. Both are unconditional. The identity surface is the only part of the API that never
touches `s.M` — `/v1/me`, `/v1/keys`, `/v1/users`, `/v1/orgs`, `/v1/audit` all call `s.Auth` only
(`internal/api/manage.go:67-296`). So a second mux over the same `h` closure reuses the entire
existing auth model, CSRF rule and error envelope with no refactor.

**Alternatives considered.**

| Alternative | Why rejected |
|---|---|
| Make `api.Server.M` an interface | Touches all 29 routes and every `s.M` call site to model a case where the honest answer is "this route does not exist here". |
| Build a `sandbox.Manager` with a nil `Kube` | Any sandbox route would panic instead of returning 503. |
| Omit the routes and let `ServeMux` 404 | Go's default 404 is plain text and breaks the `{code, message, hint}` envelope the SDKs and the dashboard both rely on (`internal/api/api.go:319-326`). |

**Why `cluster_unavailable` specifically.** Both SDKs already branch on that code
(`sdk/typescript/src/index.ts` retry rule; `sdk/python/tests/test_sdk.py:65`), and
`internal/sandbox/sandbox.go:61-68` already defines it for an unreachable cluster. Reusing it means
an unreachable cluster never looks like "zero clusters" — the guarantee asserted at
`internal/sandbox/node_helper_test.go:306-308`.

**Path-parameter rule.** `s.auth` treats *any* route with an `{id}` parameter as sandbox-scoped and
calls `s.M.Owner(id)` (`internal/api/api.go:264-274`). Cluster routes therefore use `{name}` and
`{node}`. This is also why no change to the security-sensitive middleware is needed.

---

## D5 — Retaining cluster credentials in the control plane

**Decision.** AES-256-GCM. The key comes from `--control-plane-key` or `DAWNBX_CONTROL_PLANE_KEY`;
if neither is set, it is generated once into `<data-dir>/server/control-plane.key` with mode `0600`.

**Rationale.** FR-006 requires the control plane to *retain* cluster credentials so an
administrator can reveal or rotate them, which is impossible if only a hash is kept — the existing
`CreateKey` deliberately stores only `hash(tok)` (`internal/auth/auth.go:216-227`). And FR-014
requires the control plane to be able to reach the cluster, which needs the plaintext admin password
(D6). Retention is therefore an explicit product requirement, and reversible encryption is the price.

**Honest limitation, to be documented in the plan and the page:** anyone who can read the control
plane's key file can decrypt every cluster credential. The mitigation is the existing
`adminOnly` gate on the reveal route (`internal/api/manage.go:41-49`) plus an audit row, not
defence in depth beyond the file mode. This is stated rather than hidden because the alternative —
re-minting on every reveal — is not implementable for a value the cluster only ever saw once.

---

## D6 — How does the control plane trust a cluster it just created?

**Decision.** Trust-on-first-use certificate pinning. After the stack reports success the control
plane fetches the cluster's public URL with verification disabled, hashes the leaf's
SHA-256 SPKI, stores it as `clusters.tls_pin`, and shows the fingerprint in the dashboard. Every
later call uses `InsecureSkipVerify: true` with a `VerifyPeerCertificate` callback that compares the
pin.

**Rationale.** The cluster's Go server takes a Let's Encrypt certificate only when `--domain` is
set and issuance succeeds; otherwise `install.sh:265-274` leaves a self-signed P-256 certificate
in `$SRV/tls`. The default domain is `${Ip}.sslip.io` (`deploy/aws/dawnbx.yaml:128`), so a
certificate failure is a realistic outcome, not a hypothetical. Verifying against the public CA
chain would therefore make readiness detection fail on a cluster that is actually healthy.

**Alternatives considered.**

| Alternative | Why rejected |
|---|---|
| Require a real domain and a valid LE certificate | Makes the sslip.io default unusable and puts a DNS/ACME dependency in front of the core flow. |
| Skip TLS verification with no pin | Accepts any MITM of a host that holds production sandboxes. |
| Have `install.sh` report the certificate fingerprint | Requires another machine-to-control-plane channel, and D1 removed them all. |

**The pin is shown, not hidden.** The dashboard renders the fingerprint so an operator can compare
it against the instance out of band; the same reasoning the existing `SshCommand` output applies
("Keep that output out of tickets, screenshots and CI logs", `deploy/aws/dawnbx.yaml:143-147`).

---

## D7 — How is a worker node created and removed?

**Decision.** Create: the control plane calls the cluster's own `GET /v1/nodes/join` to obtain the
join command, then `ec2:RunInstances` reusing the server stack's LaunchTemplate, with
`install.sh --join <url> <token>` as user-data. Remove: ask the cluster `GET /v1/nodes` for the
sandbox count, then `DELETE /v1/nodes/{node}` on the cluster and `ec2:TerminateInstances`.

**Rationale.** The join surface is already exactly one URL plus one k3s token, and both are
reachable through an existing authenticated admin call (`internal/sandbox/node.go:209-219`,
`internal/api/manage.go:305-315`). Reusing it means the k3s-specific knowledge — the CA-digest
binding, the `K10…::server:` token format, the fact that 6443 must be VPC-internal — never enters
the provider layer. Removal gets the existing 409 for a node holding sandboxes
(`internal/api/api_test.go:330-332`) for free, which is FR-011 exactly.

**Why not a stack per worker.** A second CloudFormation stack per worker would need a role
parameter the current template does not have, and would multiply stack-wait timeouts and rollback
paths for a resource that is a single `RunInstances` call.

**Template change required.** One new output, `LaunchTemplateId`, so the control plane can target
the same launch template (and therefore inherit its `MetadataOptions`, which satisfy the
IMDSv2/hop-limit-1 constraint). The worker also needs a public route to reach the cluster's 6443;
`SgK3s` is currently self-referencing only (`deploy/aws/dawnbx.yaml:74-76`), so a worker-to-server
rule from the VPC CIDR is added — matching what `--allow-join` already provisions on the server
(`deploy/aws/dawnbx.yaml:123-126`).

---

## D8 — Where does the operator-facing flow live in the dashboard?

**Decision.** A new `/clusters` route with a `step` search parameter, not another tab in
`settings.tsx`.

**Rationale.** The flow is a multi-step wizard (provider → configuration → price confirmation →
live status → nodes) that must be linkable, and `settings.tsx` is already 628 lines. The existing
tab pattern is one level of navigation deep (`web/src/pages/settings.tsx:44-77`); a wizard with its
own progress is a route. The terminal page (`web/src/pages/terminal.tsx`) is the precedent for a
dedicated route reached from elsewhere in the app.

**The trap.** The route tree is duplicated between `web/src/main.tsx:22-38` and
`web/src/test-support.tsx:55-75`, and nothing checks they agree. A new route registered in only one
of them renders in the app but cannot be reached by `renderApp` in tests. Both edits are one task.

**In control-plane mode the shell hides the sandbox surfaces.** `web/src/pages/shell.tsx:48-63` is
a hand-written `<nav>`; a new `GET /v1/control-plane` capability response tells it which links to
render, and the `/v1/status` poll in the shell header (`shell.tsx:78`) is skipped in that mode since
that route returns 503.

---

## D9 — Where do prices come from?

**Decision.** AWS Pricing `GetProducts`, filtered per instance type and region for
`tenancy=Shared`, `preInstalledSw=NA`, `operatingSystem=Linux`, `capacitystatus=Used`, taking the
lowest on-demand price. Storage from the region's gp3 per-GB-month rate; public IPv4 from the
region's per-hour rate. Monthly = hourly × 730 + diskGiB × rate + ipv4 × 730.

**Rationale.** The clarification requires an hourly and monthly breakdown of compute, storage and
public-IP charges with data transfer, taxes and discounts explicitly excluded. That is a regional
lookup, not a guess, and the AWS price list is the only authoritative source.

**Boundaries stated in the UI.** Data transfer, taxes and discounts are shown as excluded, per the
recorded clarification. The estimate is advisory; the confirmation step is what authorises spend.

---
## D10 — What does the provider boundary actually carry?

**Decision.** The interface carries **values and intent**, never provider identifiers. Concretely:

```go
// ClusterSpec is what to build. No provider resource is named here.
type ClusterSpec struct {
	Region, InstanceType, DiskGiB, Domain string
}

// Bootstrap is a credential the control plane minted and the provider must
// place on the new host. Delivery is the provider's business: SSM on AWS, a
// secret manager elsewhere. The control plane never learns how.
type Bootstrap struct{ AdminPassword string }

// Handle is opaque cluster state the provider needs to track, poll, extend and
// delete what it created. Never rendered, never interpreted, never routed on.
type Handle struct{ raw json.RawMessage }
```

`Handle.raw` is the provider's own JSON. The control plane stores it and hands it back verbatim; it
never reads a field out of it. `internal/provider` imports no AWS package, and `internal/cluster`
imports no adapter — the adapter is injected at startup, so the neutral layer is testable with a
fake provider and AWS is reachable from exactly one place.

**Rationale.** The phase-one product statement is that we consume *a server* from a cloud — an EC2
instance today — and nothing else of that cloud's catalogue is a product concept. Naming AWS
resources at the boundary would make the second provider cost a migration of the neutral model, not
just a new adapter. An earlier draft of this plan had `stack_name`, `credentials_parameter`,
`security_group`, `launch_template` and `public_ip` as columns on the neutral `clusters` entity,
which is exactly the leak; they are now one `provider_state` blob on the same row (see
`data-model.md`).

**The five verbs, and nothing else.** Strip away the cloud and a provider must be able to: make a
host, get one secret onto it, say when it is ready and what its URL is, say what it costs, and
destroy what it made. Those five are the interface. Everything else a provider does is an
implementation detail of how it answers them.

| Verb | AWS, in phase one | Another cloud |
|---|---|---|
| make a host | `cloudformation:CreateStack` from the existing template | its own compute API |
| get one secret onto it | SSM `SecureString` + an instance role reading it | its own secret store, or nothing |
| ready, and its URL | `WaitCondition` + stack outputs, then a TLS-pinned login | poll the host, then the same login |
| what it costs | `pricing:GetProducts` | its own price list |
| destroy it | `DeleteStack` + `DeleteParameter` | its own delete calls |

**`Status` is neutral; rollback is the adapter's.**

```go
// Status is what a provider can say about a host it was asked to create. It is
// deliberately not a stack, an operation, or a job.
type Status struct {
	State  State  // creating | bootstrapping | ready | failed | gone
	Reason string // non-secret, provider-supplied, "" unless State is failed
	URL    string // non-empty only when State is ready
}
```

`State` is the provider's own verdict. `Reason` is whatever non-secret explanation that provider can
produce — on AWS the wait condition's sanitised `Reason` (`install.sh:56-63`), elsewhere an HTTP
probe result or a console-log tail. The orchestrator stores it and shows it; it never interprets it.

**Rollback belongs to the adapter, not the state machine.** An earlier draft put the CloudFormation
rollback race into `data-model.md`, which would have given the orchestrator a `WaitForRollback` that
 only works when the provider has declarative orchestration. A provider without it has nothing to
 wait for, so the call would be a permanent no-op. The AWS adapter therefore owns the distinction
between "the install reported failure" and "the stack is already rolling back", and settles its own
teardown before reporting `failed`. `internal/cluster` only ever sees `Status`.

**Secret-delivery ceilings differ per provider, and that is the honest answer.** SSM is the
 strongest option available on AWS, not a pattern this project invented. A provider with no secret
 store an instance can read must deliver `Bootstrap` in the host's creation payload, which is
 precisely the exposure a `NoEcho` user-data parameter would carry on AWS. The interface does not
 pretend the ceiling does not exist: it lives in the adapter, and the dashboard reports the delivery
 mechanism per cluster so an operator is never told a weaker guarantee than the one they have.

**The contract takes the provider as a path value, never as its own path segment:**
`/v1/providers/{provider}/regions`, `/v1/providers/{provider}/instance-types`,
`/v1/providers/{provider}/estimate`. An earlier draft had `/v1/aws/regions`, `/v1/aws/estimate` and
`/v1/aws/instance-types`, so the contract grew a branch per cloud and the dashboard grew a branch per
 cloud to match. A second provider is now a value and an adapter, not a path family.

**Why a JSON blob and not a key-value table.** One provider is in scope, and a
`cluster_provider_resources(cluster, kind, name, value)` table would model a generality nothing
uses. A single `provider_state` column matches the repo's existing habit with small JSON documents
(`install.sh:279` writes `/etc/dawnbx/install.json`), and the second provider writes a different
blob shape with no migration to the neutral table.

**The AWS blob shape** lives in `internal/provider/aws` and is the adapter's private contract:
`{"stack":"…","parameter":"/dawnbx/cluster/x","security_group":"sg-…","launch_template":"lt-…",
"public_ip":"…","url":"…"}`. The control plane reads none of those keys; it only ever passes the
blob back. `url` is the single exception the *product* genuinely owns — it is a neutral field, so it
lives on the `clusters` row and is surfaced by the adapter rather than parsed out of the blob.

**Enforced by:** a test in `internal/provider` that asserts the package's non-test files import no
adapter package, and one in `internal/cluster` that the whole orchestration path runs against a
fake provider with no AWS dependency at all. Both are the mechanical form of FR-013.

## Resolved Unknowns Checklist

| Technical Context field | Resolved by |
|---|---|
| Language/Version | `go.mod:3`, `web/package.json` |
| Primary Dependencies | D3 |
| Storage | `internal/auth/auth.go:81-100`; `data-model.md` |
| Testing | `hack/check.sh:42-109` |
| Target Platform | `install.sh:27` (`K3S_VERSION`), `deploy/aws/dawnbx.yaml:41-43` (arm64 AMI) |
| Project Type | `cmd/dawnbx-server/main.go`, `internal/api/api.go:24-25` |
| Performance Goals | spec.md SC-003, SC-009 |
| Constraints | constitution Operational Constraints |
| Scale/Scope | recorded clarification: one AWS account, one credential source |
