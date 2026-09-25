# TODOS

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

## `dawnbx key rotate` with grace period
- **What:** `dawnbx key rotate [--grace 1h | --now]`: server accepts old + new key hash until grace ends; new key printed once.
- **Why:** v1 has one key in `<data>/server/`; a leaked key or fresh install after volume loss means manual file edit + restart + client outage.
- **Pros:** zero-downtime rotation; standard leak response.
- **Cons:** second valid-key path + expiry in auth; tests for expiry and `--now`.
- **Context:** from /plan-eng-review 3 (D12), 2026-09-25 (human: ~1 day / CC: ~20 min). v1 ships a 3-line manual rotation in docs instead.
- **Depends on:** auth + `<data>/server/` layout shipped. Trigger: second operator or multi-key need.

## Evaluate agent-sandbox v1beta1 at multi-node milestone
- **What:** Lima probe of kubernetes-sigs/agent-sandbox (v1beta1) under dawnbx-server: gVisor RuntimeClass, local-path PVC workspace, suspend/resume, ext4 project quota on PVC dir, controller RAM on 4GB. Swap `internal/sandbox` lifecycle to Sandbox CRDs if it passes.
- **Why:** it is the Kubernetes-standard sandbox lifecycle (Red Hat supported 2026-07, GKE Agent Sandbox, Lovable in production); multi-node + Helm-on-existing-cluster come with it.
- **Pros:** less lifecycle code to own; aligns with ecosystem; easier existing-cluster install.
- **Cons:** reopens R15/R16/R22 (workspace on PVC, CRD vs meta.json truth, quota re-verify); extra controller pod.
- **Context:** from /plan-ceo-review C1, 2026-09-25: kept pods direct for single-node v1 because verified probes depend on raw hostPath pods.
- **Depends on:** v1 shipped; multi-node milestone (R13) starting.

## Deferred from first release (/plan-ceo-review SCOPE REDUCTION, D3.0, 2026-09-25)
Rule used: keep what proves the pitch (one command → sandbox API on VPS/laptop, fork, durability), defer polish. Each item below was in approved scope before the cut; design context lives in the design doc DX/eng sections.

### Console v1
- **What:** embedded web UI: sandbox list, live logs, fork tree, web terminal, create/kill (design D8=C, stage v1).
- **Why:** visual fork tree is a demo magnet; non-CLI users.
- **Cons:** L effort UI; auth + websocket terminal surface.
- **Depends on:** `/v1` API frozen. Trigger: first release out, users ask for a UI.

### Python SDK (swapped with TypeScript, 2026-09-25)
- **What:** same `create/exec/files/fork/kill` as the TS SDK (`sdk/typescript`), sync first, then the async TODO above.
- **Why:** user flipped CEO S2: TypeScript SDK ships first (Vercel AI SDK / Mastra users); installer quickstart is TS.
- **Cons:** second SDK to keep in sync.
- **Depends on:** `/v1` frozen; ideally generated from the same spec as TS.

### SDK TLS pinning (`DAWNBX_FINGERPRINT`)
- **What:** pin the self-signed cert fingerprint in the SDK once the server serves HTTPS (R12). Node fetch needs an undici dispatcher for custom verification.
- **Depends on:** R12 TLS front.

### Docs site (T11)
- **What:** static, searchable, versioned site on GitHub Pages built from `docs/` markdown (DX D1: no second tree).
- **Why:** search + versions once there are multiple releases.
- **Depends on:** second release (versions only matter then).

### Extra examples (T15)
- **What:** code-interpreter, repo-fixer with `fork(3)`, data-analysis; CI-run.
- **Why:** show fork value concretely. v0.1 ships one LLM-loop example (T5).
- **Depends on:** fork stable.

### Streaming exec (T9)
- **What:** `exec(stream=True)` across server, SDK, CLI.
- **Why:** live output for long builds/tests. v0.1: `background=True` + poll.
- **Depends on:** exec contract stable.

### `dawnbx.testing.FakeSandbox` (T13)
- **What:** in-memory fake for users' unit tests.
- **Depends on:** an SDK stable. Trigger: user request.

### Rollback + Deprecation/Sunset headers (T12)
- **What:** `install.sh --version <old>`; API deprecation headers + SDK warning.
- **Depends on:** a second released version exists.

### Warm pool (R10)
- **What:** N pre-started idle pods per image (R10 approved default 2).
- **Why:** create < 1s instead of ~2–3s cold.
- **Cons:** RAM on 4GB VPS; pool reconcile code.
- **Depends on:** E2 lifecycle. Trigger: measured cold start hurts users.

### `doc_url` per error + `docs/errors` pages (T10 part)
- **What:** each error code links a docs page. `dawnbx doctor` already shipped in v0.1.
- **Depends on:** docs site.

### Multi-node `install.sh --join` (R13)
- **What:** join token, k3s agent + gVisor on new node, dawnbx-node DaemonSet, NetworkPolicy node IPs, console Nodes page (R14).
- **Why:** scale past one box.
- **Context:** seams kept in v0.1: `dawnbx/node` label, `copyWorkspace(node, src, dst)`, `internal/sandbox` boundary. Pair with the agent-sandbox evaluation TODO above.
- **Depends on:** single-node v1 shipped.
