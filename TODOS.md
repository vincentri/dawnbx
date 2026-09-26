# TODOS

Shipped since these were written (2026-09-25): dashboard, Python SDK, warm pool
for the default image, DB-backed keys/users/orgs (replaces `key rotate`: make a
new key, revoke the old one).

Also shipped since (2026-09-26, the harness pass):

- **`AGENTS.md`**: the agent rules for this repo — worktree-mandatory workflow,
  the checks, the per-area contracts, and the `openapi.yaml` chain that five
  files have to agree on.
- **A pre-push gate** (`hack/check.sh` + `.githooks/`): gofmt, staticcheck,
  golangci-lint, biome, eslint/prettier, ruff, both builds, all four test
  suites, and coverage floors in `hack/coverage-floor.txt`. Floors are a
  ratchet: they only rise.
- **Linters everywhere.** There were none before: no eslint, prettier, ruff,
  golangci-lint or staticcheck, and no lint script in any package.json.
- **Tests on every area.** Go 59.9% -> 84.9%, web 6.4% -> 94.6%, both SDKs
  over 95%. The web tests caught a real bug a linter autofix had introduced.
- Fixed: `Authorization` now accepts any-case `Bearer` (RFC 7235); the docs no
  longer claim the server serves `/openapi.yaml`.
- `cmd/dawnbx-server` is being made testable by extracting a callable startup
  path out of `main()`, so its coverage is no longer stuck at 22%.
- The live tier (`hack/verify.sh` in a Lima VM) is now reachable from the gate
  as `CHECK_LIVE=1 bash hack/check.sh` instead of being folklore.

## Next: release + one-click EC2 + --join (CEO review 2026-09-25, plan B)
Review notes: ~/.gstack/projects/sandbox/ceo-plans/2026-09-25-control-plane-aws.md
- **(0) Release:** GitHub remote, CI (now `bash hack/check.sh` in .github/workflows, plus cfn-lint), GoReleaser linux amd64+arm64 (DONE locally: `.goreleaser.yaml`, `install.sh --release-url` with sha256 check; template uses it). Left: remote, CI, first published release (owner/name/visibility: ask). Publish install time and sandboxes per 4 GB, measured on t4g.medium.
- **(1) `deploy/aws/dawnbx.yaml` written 2026-09-25, cfn-lint clean, NOT launched (paid; ask first).** Default VPC, t4g.medium, 30 GB gp3 encrypted, SG 22 (SshCidr)/80/443 + intra-SG 6443/10250/UDP 51820, IMDSv2 hop 1 via launch template, no IAM role, EIP + sslip.io or Domain, WaitCondition 20 min + `--report`, allow-join = VPC CIDR from IMDS.
  - Credit spec decided: `standard` default (fixed bill; throttles to baseline), `unlimited` as a param. README documents it.
  - Not one-click yet: `ReleaseUrl` param has no default until (0) exists; then default it to the release URL and pin the version.
  - Left: amd64 instance types (needs ImageId + binary arch switch); non-default VPC/subnet params; worker template mode; the one paid test launch (2am test: fresh stack, then SDK exec over the real cert).
- **(2) DONE 2026-09-25:** `install.sh --join` + `--allow-join` + WireGuard, `/v1/nodes` (+ join, delete) + Settings Nodes section. Proven on two Lima VMs: wg peer handshake, sandbox on worker, exec/files/fork/kill, pod recreate on the same node, 10250 drops non-peers.
  - Worker disk headroom DONE 2026-09-26: `df` rides the per-sandbox `du` exec; below 10% free, reconcile deletes expiring then stops keep-forever sandboxes on that node only (verified live on dawnbx2).
  - DONE 2026-09-26: `no_room` (507) when no node can schedule a sandbox; workers under 15% free are avoided by new sandboxes and warm claims, and refuse forks; Node column in `dawnbx ls` and the dashboard (only with 2+ nodes).
  - Left: server taint once workers exist (deferred); a full server disk still blocks all creates even with roomy workers; low-disk list is only as fresh as the last reconcile (30 s); remote `du` via exec per sandbox per tick (fine to ~100 remote sandboxes) runs inside the tenant's pod, so a tenant can under-report its own usage and dodge the 10 GiB cap (fix: du from the helper pod too); template worker mode.
  - Eng review 2026-09-26 fixes DONE: worker free space from our own `df` helper pod (not the tenant's df), restart/fork check the sandbox's own node, warm sandboxes on low workers deleted, `no_room` hint names the pinned node.
  - Hostile tests DONE 2026-09-25: install.sh now checks the server CA hash and token (`/v1-k3s/readyz`) before installing anything, so wrong token, wrong server and unreachable server fail in seconds. Worker reboot: node Ready in <1 min, firewall and sandboxes back, files intact.

## Control plane + clusters on AWS (deferred 2026-09-25: build when users ask to manage several clusters)
- **Shape:** control plane = dashboard + DB, runs anywhere (`dawnbx-server --control-plane`, single binary or Docker, no k3s). Buttons: Create cluster (server EC2), Add node (worker EC2), Delete cluster. Sandboxes run only inside clusters; SDK/CLI talk to the cluster URL directly, never through the control plane.
- **install.sh stays** as the machine bootstrap: passed as EC2 user-data (`install.sh` for the server, `install.sh --join <url> <token>` for workers). Add `--report <url>` so boot progress/failures show in the UI. AMI with k3s+gVisor pre-baked only if ~2 min boot hurts.
- **AWS access:** default credential chain (profile/SSO/env/instance role), no pasted keys. "Set up AWS" button makes one scoped role via CloudFormation: RunInstances + SG + CreateTags, terminate/stop only on `dawnbx-cluster`-tagged resources. Delete cluster removes everything with the tag. Show price before launch.
- **Per cluster:** own users/keys/audit (v1); central login later. Server node tainted once workers exist, so sandboxes only run on workers. Workers have no IAM role. IMDSv2 + hop limit 1 everywhere, plus the existing 169.254.169.254 NetworkPolicy block. 6443 closed publicly; HTTPS on the cluster endpoint (R12) before internet exposure.
- **Multi-node behavior:** workspace on the node's local disk; fork is same-node (cross-node needs snapshots); node removal blocked while it holds keep-forever sandboxes; "no room: add a node" when full; Nodes page + Node column.
- **No EKS** (~$73/mo control plane). One EC2 per click in v1; ASG/cluster-autoscaler later.
- **Build order once triggered:** control-plane mode with a Lima provider, then EC2 provider + Set up AWS (reuses the Launch Stack template, `--join` and `--report`). EC2 costs money: ask before launching.

## Warm pool for other images
- **What:** per-image pool size (`--pool python:3.12-slim=2,node:22-slim=1`); today only the default image is pre-started.
- **Why:** non-default images pay the ~2-3 s cold start.
- **Cons:** RAM per idle pod.

## Kata runtime option
- **What:** `runtime: kata` per sandbox beside gVisor, for workloads gVisor can't run (odd syscalls, GPU later).
- **Cons:** needs KVM: bare-metal or nested-virt hosts; Lima on Mac likely can't test it.

## Org rename/delete
- **What:** `PATCH/DELETE /v1/orgs/{id}`; delete refuses while the org owns live sandboxes, revokes its keys, removes its users.
- **Trigger:** first tenant leaves.

## Shared login lockout
- **What:** move the 5-miss lockout from memory into the DB.
- **Trigger:** more than one API node behind a round-robin load balancer.

## Async Python SDK
- **What:** `AsyncSandbox` with the same methods (`await Sandbox.create()`).
- **Why:** agent frameworks (LangGraph, OpenAI Agents SDK, pydantic-ai) are async-first; a sync SDK blocks their event loop.
- **Pros:** fits how agents are built; parallel `exec` across forks becomes natural.
- **Cons:** second code path unless generated from the sync one.
- **Context:** SDK is on httpx, which has sync + async clients, so this is ~1 extra module (human: ~1 day / CC: ~20 min). From /plan-devex-review D11, 2026-09-25.
- **Depends on:** sync SDK shipped and `/v1` API shape frozen.

## Workspace snapshots to S3-compatible storage (phase 2)
- **What:** `sb.snapshot()`, `Sandbox.create(from_snapshot=...)`, optional auto-snapshot every N min and before TTL kill; target S3/R2/B2/MinIO.
- **Why:** data volume (D15) covers VPS loss, not volume loss or zone outage; also enables restore on another node/provider and cross-node fork.
- **Pros:** provider-neutral disaster recovery; foundation for multi-node migration.
- **Cons:** data since last snapshot lost; credentials + scheduling + restore code.
- **Context:** from /plan-devex-review D15, 2026-09-25. Workspace = `<data-dir>/ws/<id>/` + `.dawnbx.json`; snapshot = tar of that dir.
- **Depends on:** D15 data-dir + re-adopt shipped.

## Visibility for forgotten keep-forever sandboxes
- **What:** `last_activity` in `meta.json` (bumped on exec/file calls, throttled to 1/min), shown in `dawnbx ls`; `dawnbx doctor` warns on sandboxes idle >7 days; optional `--stop-idle-after` reusing the `stopped` state from R17.
- **Why:** `ttl=None` sandboxes left behind by crashed agent runs hold disk/RAM silently; R17 disk guard only acts under 10% free and stops the largest, not the stalest.
- **Pros:** operator sees waste before disk pressure; auto-stop is cheap since `stopped` + `sb.start()` already exist.
- **Cons:** extra meta write per call; one more knob.
- **Context:** from /plan-eng-review 3 (D11), 2026-09-25 (human: ~1 day / CC: ~20 min).
- **Depends on:** R16 `writeMeta()` + R17 `stopped` state shipped. Trigger: first report of stale sandboxes.

## Evaluate agent-sandbox v1beta1 at multi-node milestone
... (+46 lines) [see remaining: rtk proxy git show '0c21693:TODOS.md' | tail -n +72]
