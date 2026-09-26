# Cluster Management Routes

Human-readable route table for the new provider-neutral cluster surface. The **authoritative**
contract is `internal/api/openapi.yaml`; `openapi-delta.yaml` in this directory is the verbatim
fragment to merge into it. This file is the source for the `## Clusters` table in
`docs/content/docs/guide/api.mdx` and the route list in `README.md` — both of which are hand-kept
copies today, with nothing keeping them equal (`AGENTS.md`, contract section).

## Conventions, inherited not invented

- Every route is registered through the same `h` closure (`internal/api/api.go:38-49`), so all of
  them are authenticated by `s.auth` and every error is the shared envelope
  `{code, message, hint?}` (`internal/api/api.go:319-326`).
- Success is always `200` with a JSON body; the only `204` in the codebase is `DELETE` returning
  nothing (`internal/api/manage.go:316-326`).
- Session-only: an API key cannot call any of these. `signedIn` refuses keys outright
  (`internal/api/manage.go:32-37`); the ones marked admin require `adminOnly`
  (`internal/api/manage.go:41-49`).
- Cookie-authed writes need `X-Dawnbx: 1` (`internal/api/api.go:246-250`). The dashboard client
  already sends it on every request (`web/src/lib/api.ts:8`).
- **Path parameters are never named `id`.** `s.auth` routes any `{id}` parameter through
  `s.M.Owner(id)` for cross-org sandbox scoping (`internal/api/api.go:264-274`); in control-plane
  mode `s.M` is nil. Cluster and worker parameters are `{name}` and `{node}`, matching
  `/v1/nodes/{name}`.
- **Provider resources never appear in a response.** No field of any schema is a stack name, a
  security group, a launch template or a secret-parameter name. Those live in the adapter's opaque
  `provider_state`, which is not projected into the API at all. The only provider facts the API
  exposes are the neutral `provider` id and `tls_pin` (D10 in `research.md`).
- **A node id is a handle, not a resource name.** It comes from the adapter and is returned because
  `DELETE /v1/clusters/{name}/nodes/{node}` needs the operator to be able to name it. The control
  plane never parses it. The schema says "opaque worker handle from the provider" rather than naming
  a cloud's id type, so a second provider's shape is not "corrected" into an EC2 field.

## Routes

| Method | Path | Gate | Purpose |
|---|---|---|---|
| `GET` | `/v1/control-plane` | session | Capability probe: `{"control_plane":true,"providers":["aws"],"version":"v1"}`. The dashboard uses it to decide which nav links to render. Always `control_plane:false` on a cluster. |
| `GET` | `/v1/providers` | session | `{"providers":[{"id":"aws","available":true},{"id":"gcp","available":false},{"id":"azure","available":false}]}` — FR-002. |
| `GET` | `/v1/providers/{provider}/regions` | session, admin | Regions that provider can provision in with the configured credential source. A provider not available in phase one is `400 provider_unavailable`, never an empty list. |
| `GET` | `/v1/providers/{provider}/instance-types` | session, admin | `{"instance_types":[{"id":"t4g.medium","hourly_usd":0.0168,"monthly_usd":12.26},…]}` for the caller's region, from the adapter's own catalogue. |
| `POST` | `/v1/providers/{provider}/estimate` | session, admin | Body `{region, instance_type, disk_gib, domain}` → `{"quote_id":"…","hourly_usd":…,"monthly_usd":…,"lines":[{"label":"compute","hourly_usd":…,"monthly_usd":…},{"label":"storage",…},{"label":"public_ipv4",…}],"excluded":["data_transfer","taxes","provider_discounts"]}`. No side effects. FR-005, SC-009. |
| `GET` | `/v1/clusters` | session, admin | `{"clusters":[…]}` — list, never includes credentials. |
| `POST` | `/v1/clusters` | session, admin | Body `{name, region, instance_type, disk_gib, domain, quote_id}` → the cluster in `provisioning`. `quote_id` must match the configuration and be unexpired; a mismatch is `409 quote_stale` and the operator must re-review the price. Rejects unknown keys, so GCP/Azure cannot be created through this route (FR-002, SC-006). |
| `GET` | `/v1/clusters/{name}` | session, admin | One cluster including `status`, `phase`, `detail`, `url`, `tls_pin`, `created`, `updated`. |
| `GET` | `/v1/clusters/{name}/credentials` | session, admin, **audited** | `{api_key, admin_password}`, and `409 credentials_not_ready` until the cluster is `ready`. The only route that returns plaintext. Audited as `cluster.credentials.view`, matching `node.join-token.view` (`internal/api/manage.go:313`). FR-006. |
| `POST` | `/v1/clusters/{name}/rotate` | session, admin, audited | Mints a new admin password and a new API key, re-delivers the password to the provider's secret channel, and records the rotation. The running cluster keeps both old credentials until an operator applies the new ones; `detail` says so rather than implying it took effect. FR-006. |
| `GET` | `/v1/clusters/{name}/nodes` | session, admin | `{"nodes":[…]}` from `cluster_nodes`, refreshed from the cluster's own `GET /v1/nodes` when it is reachable. |
| `POST` | `/v1/clusters/{name}/nodes` | session, admin | Body `{instance_type, disk_gib}` → a node in `provisioning`. 503 `cluster_unavailable` when the cluster is not `ready`. FR-010. |
| `DELETE` | `/v1/clusters/{name}/nodes/{node}` | session, admin | 204. 409 `node_holds_sandboxes` when the cluster reports sandboxes on it, with the count. FR-011. |
| `DELETE` | `/v1/clusters/{name}` | session, admin | Returns the cluster in `deleting` — the operator is told what is happening. `409 cluster_has_nodes` when workers are still attached. This is the lifecycle capability FR-013 requires the interface to have; it is also what failure auto-cleanup and the next spec's delete-cluster dashboard flow both use. |

## Status codes beyond the shared envelope

| Code | Status | When |
|---|---|---|
| `cluster_has_nodes` | 409 | A cluster was deleted while workers were still attached. |
| `cluster_unavailable` | 503 | Any cluster action against a cluster that is not `ready`, or when the control plane cannot reach AWS. Reuses the existing code both SDKs already retry on (`internal/sandbox/sandbox.go:61-68`). |
| `quote_stale` | 409 | The estimate no longer matches the requested configuration, or has expired. The operator must re-review before confirming. |
| `node_holds_sandboxes` | 409 | FR-011. |
| `provider_unavailable` | 400 | A provider other than `aws` was requested; phase one has exactly one available. |
| `credentials_not_ready` | 409 | The credentials route was called before the cluster reached `ready`. The API key is minted by the cluster during `verifying`, so there is nothing to return earlier (D1). |
| `invalid_request` | 400 | Validation. Built with the existing `bad()` helper (`internal/api/manage.go:24`). |
| `forbidden` | 403 | `signedIn` / `adminOnly` (`internal/api/manage.go:28,41`). |

`forbidden` and `invalid_request` are the codes already in use; the new ones follow the same
`errf` construction at `internal/sandbox/sandbox.go:53`.

## Read-only SDK surface

`AGENTS.md` requires a route change to touch both SDKs, and both are hand-written clients that
today cover only `/v1/sandboxes*`. The cluster surface is operator tooling, not the agent path, so
the SDKs get **read-only** helpers — `list_clusters`, `get_cluster`, `list_nodes` — over the same
three GET routes. No SDK method creates, rotates or deletes anything, because those actions spend
money or destroy infrastructure and belong behind a human at the dashboard (FR-005).

## What is not in this contract

- No route proxies sandbox traffic. The control plane does not forward `/v1/sandboxes*` to a
  cluster; clients connect to the cluster URL directly (FR-014).
- No route returns a CloudFormation template, stack events, or an instance console log. The
  diagnostics an operator gets are the sanitised `detail` from the wait condition
  (`install.sh:56-63`, which is already secret-free by construction).
- No route accepts a long-lived cloud access key. Credentials come from the provider's standard
  chain, so there is nothing for the API to accept (FR-003).
- **No path is named after a provider.** Everything provider-specific is the `{provider}` path
  value under `/v1/providers/{provider}/…`, so a second provider adds a value, not a path family.
  An earlier draft had `/v1/aws/regions`, `/v1/aws/instance-types` and `/v1/aws/estimate`, which
  would have made the contract grow one branch per cloud (D10).
- No route returns a provider's resource identifiers. `provider_state` is adapter-internal and is
  never projected, so the contract cannot grow an AWS-shaped field by accident (D10).
