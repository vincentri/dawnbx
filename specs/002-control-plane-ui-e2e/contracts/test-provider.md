# Contract: Test Provider Injection

**Feature**: `specs/002-control-plane-ui-e2e` | **Date**: 2026-09-27

## What this contract governs

The suite starts the real `dawnbx-server` binary and needs it to provision
against something other than a cloud. This contract defines the single seam that
allows that, and — more importantly — the constraint that keeps a test provider
out of a shipped binary.

This is **not** an HTTP contract. No route is added or changed, so
`internal/api/openapi.yaml` is untouched and Principle III is satisfied without
a five-file edit.

---

## The seam

The control plane already resolves its cloud adapter through one injectable
function:

```go
// cmd/dawnbx-server/main.go:114-116
// newProvider builds the cloud adapter in control-plane mode. It is a
// ...
newProvider func(ctx context.Context, cfg config) (provider.Provider, error)
```

- Default: `nil` (line 139).
- Filled per process by `wireCloud`.
- Consumed at line 357: if non-nil, it wins over constructing a real adapter.
- Already exercised by `TestWireCloudPrefersAnInjectedProvider`
  (`cmd/dawnbx-server/control_plane_test.go:242`).

**The suite MUST use this seam.** It does not get a new one.

## The provider interface it must satisfy

The test provider implements the product's existing provider-neutral interface
(`internal/provider/provider.go`). It is a second implementation of the same
contract a real cloud adapter implements — which is the property that makes a
future provider testable by this suite without changes to the suite (FR-018).

Behaviours it must honour, all of which the product already depends on:

| Member | Contract obligation |
|---|---|
| `Capabilities()` | Reports whether this provider is available. Drives the roster's unavailable state (FR-013). |
| `Regions()` | Returns a non-empty, stable list. |
| `InstanceTypes()` | Returns a non-empty, stable list for a region. |
| `Estimate(spec)` | Returns an estimate for a valid spec. Must be deterministic for the same spec, or FR-002 fails. |
| `Create(spec, boot)` | Returns a `Handle`. MUST observe `boot.AdminPassword` being populated — the product's own test asserts this, and a test provider that ignored it would let a bootstrap regression pass. |
| `Status(handle)` | Returns the phase and status the outcome declares, advancing on the declared timer. |
| `Nodes(handle)` | Returns nodes per the outcome; MUST be able to report "unreachable" distinctly from "no nodes" (FR-010). |
| `RemoveNode(handle, id)` | Returns `ErrNodeBusy` when the outcome says workers are held (FR-012). |
| `Destroy(handle)` | Completes the teardown path the product depends on. |

## The security constraint

**MUST**: a test provider MUST be selectable only in a test build. A shipped
`dawnbx-server` binary MUST NOT be able to select one by environment variable,
flag, or any input an operator or attacker controls.

**Why this is a hard requirement and not a preference**: the provider interface
is the exact shape that receives the cluster specification and the bootstrap
administrator password. A production binary that could be pointed at a test
provider would be pointed at one that does not create a host, does not use the
password, and reports a ready cluster that does not exist. The blast radius is
every cluster on the control plane.

**How**: the test provider lives in a build-tagged file. The default build
excludes it, so the symbol is absent from the shipped binary and there is no
runtime switch to flip. No environment variable, no flag, no config key.

**Verification**: a check MUST confirm that a default build does not contain the
test provider symbol, and that selecting it requires the test build tag.

## Determinism obligations

The provider is the suite's only source of nondeterminism, so:

- `Estimate` MUST be deterministic for identical input.
- `Status` MUST advance on the declared timer, not on call count, so repeated
  polling converges rather than oscillating.
- `Create` MUST be safe to call once per test; concurrent calls are not expected
  and the fixture serialises them.
- NO behaviour may depend on wall-clock time, randomness, or network access.

## Database determinism

The control plane runs on PostgreSQL in this feature (R-008), so determinism
extends past the provider:

- Each run MUST start from an empty database. A Compose volume that survives a
  run MUST be recreated, or a test may observe a cluster from a previous run and
  pass for the wrong reason.
- The migration MUST be idempotent across a run, and its completion MUST be
  awaited by a health check rather than a sleep.
- The connection string MUST address the container's database, never a
  developer's own PostgreSQL on the host.
- Test-visible timestamps MUST come from the registry's injectable clock, not
  from database server time, so a run does not depend on the engine's clock.

## What the suite asserts through this contract

The suite asserts only on what an operator sees in the browser. It MUST NOT
assert on the provider's return values directly, because that would test the
fixture rather than the product (FR-009). The provider exists to make a
product-observable state reachable, not to be inspected.

## Out of contract

- Any change to the product's provider interface. If the interface changes, the
  test provider changes with it, and the suite is unaffected.
- Any HTTP route. None is added.
- Any database schema. None changes.
