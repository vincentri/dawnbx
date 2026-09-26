# Data Model: SSH-Free AWS Control Plane

Phase 1 output. Storage is the existing auth database — SQLite by default, Postgres via
`--database-url` — so one migration mechanism, one auth gate and one connection story are reused
(`internal/auth/auth.go:81-100`). The four new tables are appended to the existing `migrations`
slice at `internal/auth/auth.go:36-51`, which is already the only schema-change mechanism in the
project.

## Design rules

- `cluster` names are DNS-1123 labels, like sandbox ids (`internal/store/store.go:31-34`), because
  they become CloudFormation stack names and appear in URLs.
- Secrets are stored only in `cluster_credentials`, always as AES-256-GCM ciphertext with the nonce
  prepended (D5 in `research.md`). No secret column is ever returned by a list route.
- **No column on a neutral entity names a provider resource.** Stack names, secret-parameter
  names, security groups and launch templates are not product concepts; they are AWS facts the
  adapter needs and the control plane must not interpret. They live in one `provider_state` JSON
  column, handed back to the adapter verbatim (D10 in `research.md`). An earlier draft of this
  model had them as five typed columns, which made the "provider-neutral" entity an AWS cluster.
- Timestamps are `BIGINT` unix seconds, matching `api_keys` and `sessions` (`internal/auth/auth.go:38-43`).

## Entities

### `clusters`

| Column | Type | Notes |
|---|---|---|
| `name` | `TEXT PRIMARY KEY` | DNS-1123 label; the `{name}` path param |
| `provider` | `TEXT NOT NULL` | `aws` in phase one; the registry key from `internal/provider` |
| `region` | `TEXT NOT NULL` | AWS region id |
| `instance_type` | `TEXT NOT NULL` | must be one of the template's `InstanceType` `AllowedValues` |
| `disk_gib` | `INTEGER NOT NULL` | ≥ the template's `DiskGiB` `MinValue` |
| `domain` | `TEXT NOT NULL DEFAULT ''` | empty means the template's sslip.io default |
| `status` | `TEXT NOT NULL` | see state machine below |
| `phase` | `TEXT NOT NULL DEFAULT ''` | current sub-step; empty unless `status = provisioning` |
| `detail` | `TEXT NOT NULL DEFAULT ''` | non-secret human-readable status; never contains a secret |
| `quote_id` | `TEXT NOT NULL` | the `POST /v1/aws/estimate` result that authorised this cluster |
| `hourly_usd` / `monthly_usd` | `REAL NOT NULL` | the confirmed estimate, so the UI can show what was authorised |
| `provider_state` | `TEXT NOT NULL DEFAULT '{}'` | the adapter's opaque JSON handle (D10); the control plane never reads a key out of it |
| `url` | `TEXT NOT NULL DEFAULT ''` | cluster URL; non-empty only when `status = ready` (FR-009) |
| `tls_pin` | `TEXT NOT NULL DEFAULT ''` | SHA-256 SPKI pin (D6); empty until the first successful fetch |
| `created` / `updated` | `BIGINT NOT NULL` | |

Validation, enforced in `internal/cluster` before any AWS call so a rejected request costs nothing:

- `name` matches `^[a-z][a-z0-9-]{1,30}[a-z0-9]$` and is not already taken.
- `instance_type` and `disk_gib` are validated against **the provider's own advertised catalogue**,
 which the adapter returns through `internal/provider`, not against a hard-coded AWS list. The AWS
 adapter reads the current `AllowedValues` and `MinValue` from the template it ships
 (`deploy/aws/dawnbx.yaml:25-27,37-39`), so the API and the template cannot drift.
- `domain` matches `^[A-Za-z0-9.-]+$` or is empty (`install.sh:123` uses the same charset); `region`
 is a member of the provider's advertised region list.

### `cluster_credentials`

| Column | Type | Notes |
|---|---|---|
| `cluster` | `TEXT PRIMARY KEY REFERENCES clusters(name) ON DELETE CASCADE` | one row per cluster |
| `admin_password` | `BLOB NOT NULL` | AES-256-GCM; the only secret injected into the host (D1) |
| `api_key` | `BLOB` | AES-256-GCM; **null until the cluster is verified** — see below |
| `created` | `BIGINT NOT NULL` | |
| `rotated` | `BIGINT` | null until an operator rotates |

The two secrets have deliberately different lifecycles:

- `admin_password` is written **before** `Create`, because it is the credential the provider must
  place on the new host. Its plaintext is used twice: once as the `Bootstrap` value handed to the
  adapter, once for the TLS-pinned login during `verifying`. Then never again except by an explicit
  reveal or rotate.
- `api_key` is written **during** `verifying`, after the control plane has logged into the cluster
  with the admin password and called the cluster's own `POST /v1/keys`
  (`internal/api/manage.go:111-112`). The key therefore never crosses any channel: it is minted by
  the cluster that must accept it, so it is valid by construction and is stored in the cluster's own
  key table as a `sha256` (`internal/auth/auth.go:216-227`) — listable, revocable and auditable
  there, rather than existing only as an imported hash the cluster's dashboard cannot see.

**Consequence for the API.** `GET /v1/clusters/{name}/credentials` returns `409` until the cluster
is `ready`, because there is no API key to hand over before then. That is more honest than
offering a credential for a cluster that does not exist yet.

**Rotation** replaces both together and re-stores the admin password into the provider's secret
channel. A running cluster keeps its old credentials until an operator applies the new ones, so
`detail` says so rather than claiming the rotation took effect.

### `cluster_nodes`

| Column | Type | Notes |
|---|---|---|
| `cluster` | `TEXT NOT NULL REFERENCES clusters(name) ON DELETE CASCADE` | |
| `id` | `TEXT NOT NULL` | EC2 instance id; the `{node}` path param |
| `instance_type` | `TEXT NOT NULL` | |
| `status` | `TEXT NOT NULL` | `provisioning \| ready \| failed \| removing \| removed` |
| `detail` | `TEXT NOT NULL DEFAULT ''` | non-secret |
| `sandboxes` | `INTEGER NOT NULL DEFAULT 0` | last observed count from the cluster's `GET /v1/nodes` |
| `created` | `BIGINT NOT NULL` | |

Primary key `(cluster, id)`. `sandboxes` is a *cache* of the cluster's answer, never the authority:
removal calls the cluster first and surfaces its 409 (FR-011, D7).

### `cluster_ops`

The spec's *Provisioning Operation* entity. Append-only phase history, which is what makes
FR-008's "provisioning phase" and SC-003's 60-second transition observable after the fact.

| Column | Type | Notes |
|---|---|---|
| `id` | `TEXT PRIMARY KEY` | |
| `cluster` | `TEXT NOT NULL REFERENCES clusters(name) ON DELETE CASCADE` | |
| `kind` | `TEXT NOT NULL` | `create \| add_node \| remove_node \| delete \| rotate` |
| `phase` | `TEXT NOT NULL` | neutral phase names: `validating`, `requesting_host`, `bootstrapping`, `verifying`, `minting_key`, `ready`, `failed` |
| `detail` | `TEXT NOT NULL DEFAULT ''` | non-secret diagnostic; the `Reason` string from the `WaitCondition` lands here |
| `created` | `BIGINT NOT NULL` | |

The last row per `(cluster, kind)` is the current phase; the table is what the dashboard's
"what happened" view and the failure diagnostics read.

## State machine

```text
                 ┌───────────────────────── retry with a new name ─┐
                 ▼                                                │
  (create) → provisioning ─→ ready                                │
                 │                                                │
                 ├──→ failed  (auto-delete resources, keep ops) ───┘
                 │
                 └──→ deleting → deleted
```

`provisioning` phases, in order, each appending a `cluster_ops` row:

| Phase | What is happening | Failure behaviour |
|---|---|---|
| `validating` | checking the estimate, the name, and the provider's identity | nothing was created; mark failed, no cleanup |
| `requesting_host` | the provider places the `Bootstrap` credential in its secret channel and creates the host — on AWS that is `ssm:PutParameter` then `cloudformation:CreateStack` | the provider tears down what it created and returns `Status{State: failed}`; the orchestrator runs no cleanup of its own |
| `bootstrapping` | polling `provider.Status` for the host's own verdict; on AWS that is `DescribeStacks` / `DescribeStackEvents` against the `WaitCondition`, elsewhere an HTTP probe of the host | `Status.Reason` becomes `detail` verbatim. The provider has already settled its own teardown before reporting `failed`, so the orchestrator never waits for a rollback it cannot know about. |
| `verifying` | taking `Status.URL`, fetching and pinning its certificate, then `POST /v1/login` against the cluster with the injected admin password | if login fails, the cluster is **not** `ready`; go to `failed` (FR-009) |
| `minting_key` | `POST /v1/keys` on the cluster, then storing the returned key encrypted | the cluster stays running; `failed`, and the operator is told the host needs manual cleanup |

The phase names are the neutral ones the provider interface and the dashboard share. Everything
AWS inside a row — a parameter write, a stack create, a wait condition — is the adapter's business,
so the dashboard renders progress without knowing whether the host came from a stack, a group, or a
single API call. The orchestrator reads only `provider.Status` (D10 in `research.md`): it never
sees a stack, a rollback, or a provider-specific reason code.

`verifying` and `minting_key` are why this is not just host-watching. A cluster reaches `ready` only
after **its own API** accepts the credential the control plane injected and issues a key of its own.
That is the strongest available proof of FR-009 without a human, and it is provider-neutral: every
provider gives a running host an HTTP API, because that is the product contract FR-014 relies on.

`failed` triggers the recorded clarification: the provider deletes everything it created exclusively
for that operation (SC-008) — `DeleteStack` then `ssm:DeleteParameter` on AWS. A cluster that was
previously `ready` and later fails is **not** auto-deleted; the recorded edge case says preserve a
running cluster and report that new provider actions need renewed access.

## Relationships

```text
clusters 1 ──1 cluster_credentials      (cascade delete)
clusters 1 ──* cluster_nodes            (cascade delete)
clusters 1 ──* cluster_ops              (cascade delete, append-only)
clusters * ──1 provider (implicit)      (in-process registry, not a FK)
```

## What is deliberately not modelled

- **Users, orgs and API keys** are reused unchanged. Cluster records are operator-level
  infrastructure, not org-scoped, so they sit outside the org filter at `internal/api/manage.go:52`
  and are gated by `adminOnly` (`internal/api/manage.go:41-49`) like `/v1/orgs` and `/v1/users`.
- **Per-cluster audit events** go to the existing `audit` table via `s.audit`
  (`internal/api/api.go:279-285`) for actions an operator performed; `cluster_ops` records machine
  phases. Mixing them would have made one table serve two lifecycles.
- **Sandbox data** never lives in the control plane. It stays in the cluster's own store
  (`internal/store/store.go:1-7`), which is the whole point of the cluster boundary (FR-014).
- **Provider resources.** No table and no column models a stack, a security group, a launch
  template, a secret parameter or an instance id as a *product* entity. `provider_state` carries
  the adapter's opaque handle and `cluster_nodes.id` is the neutral node handle the adapter returns
  and the control plane hands back. This is D10, and it is the reason the second provider costs one
  adapter rather than a migration.
