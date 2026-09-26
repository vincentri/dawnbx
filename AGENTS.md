# dawnbx — agent rules

Self-hosted gVisor sandboxes for AI agents. Go control plane + CLI (`cmd/`, `internal/`), React dashboard embedded in the server binary (`web/` → `internal/api/ui`), Python and TypeScript SDKs (`sdk/`), static docs site (`docs/`), single-box installer (`install.sh`, `deploy/aws/`, `hack/`).

This file is the only agent rules file. Do not add a per-folder `AGENTS.md` until a section runs 15+ lines and is irrelevant to most tasks, or this file passes ~150 lines. A nested file is additive — never a copy of this one — and the more specific file wins where they conflict.

**Governance:** `.specify/memory/constitution.md` is the constitution (v1.0.0, ratified 2026-09-26). It supersedes practice and habit, and this file is its operational expression: where the two disagree the constitution wins, and both are corrected in the same change. New features go through Spec Kit before any code — `/speckit.specify`, then plan, tasks, implement. Do not start a feature from a chat request alone.

## 1. All development runs in a git worktree

Never edit, build, test, or commit in the primary checkout, and never on `main`. One worktree per task.

**Never push, and never open a PR, without an explicit instruction in the current session.** Branches, merges, and anything else stay in the local repo. The remote is the user's call, every time.

```bash
# from the primary checkout
git fetch origin
git worktree add ../dawnbx-<task> -b task/<slug> origin/main
cd ../dawnbx-<task>        # every command below runs here
```

- `node_modules/` is gitignored: a new worktree has none — run `npm ci --prefix web` (and `npm ci --prefix sdk/typescript`, `npm ci --prefix docs`) once per worktree.
- After the branch lands, drop the worktree: `git worktree remove ../dawnbx-<task>`.
- Only exception is a direct hotfix on `main`; say so in the commit body.

## 2. Checks

**Nothing gets pushed until the whole gate is green.** `bash hack/check.sh` is that gate, one command, every step reported so one run shows every failure. `.githooks/pre-push` runs it and refuses the push.

- **Lint** — `gofmt`, `staticcheck`, `golangci-lint`, `biome` (web), `eslint` + `prettier` (TS SDK), `ruff check` + `ruff format` (Python SDK)
- **Build** — `go build ./...`, `npm run build --prefix web`
- **Test** — `go test ./...`, the web vitest suite, both SDK suites
- **Coverage** — a 95% floor in all four areas

The floor is the target, not today's state: the gate currently fails on it, and `hack/check.sh` prints the real number per area. Raise coverage or argue the floor down in the configs; never delete the step to get a green push.

A fresh worktree needs `npm ci --prefix web`, `npm ci --prefix sdk/typescript`, `npm ci --prefix docs`, and `python3 -m venv .venv && .venv/bin/pip install coverage` before the gate will run at all.

- `go build ./... && go test ./...` — server, CLI, core
- `npm run typecheck --prefix web` — dashboard
- `npm test --prefix sdk/typescript` — TS SDK (type-checks, then tests `dist/`)
- `cd sdk/python && python3 -m unittest discover -s tests` — Python SDK
- `npm run types:check --prefix docs` — docs site
- `npm run build --prefix web` — **required for any `web/` change**, see §3
- `python3 hack/check-harness-cites.py` — runs on every commit via `.githooks/pre-commit`; fails when a line in this file cites a file that is gone. Enable it once per clone with `git config core.hooksPath .githooks`. If it blocks you, the cite is stale or the sentence should go — do not widen the check to make it pass.

Live tier, only for sandbox / k8s / quota work: `hack/dev-vm.sh` builds and installs into the Lima VM, `hack/verify.sh` runs inside it as root against k3s, gVisor, and a real sandbox pod. Neither is a lint gate.

## 3. The one cross-area contract

`internal/api/openapi.yaml` is hand-written and is the only shared contract. `web/` types are generated from it (`npm run gen --prefix web` → committed `web/src/lib/schema.d.ts`); both SDKs and the docs site mirror it by hand. Nothing in CI checks that they agree.

Adding or changing a route therefore means, in one change: edit `openapi.yaml`, regenerate the web types, update `sdk/python/src/dawnbx/__init__.py` **and** `sdk/typescript/src/index.ts`, update `docs/content/docs/guide/api.mdx`.

Same for the UI: Vite writes its build straight into `internal/api/ui` with `emptyOutDir`, that directory is committed, and `internal/api/api.go` embeds it with `//go:embed ui`. Skip `npm run build --prefix web` and the binary serves the old dashboard. Releases do the same via a `.goreleaser.yaml` pre-hook, so a release built without Node installed is not a thing.

## 4. `internal/` + `cmd/` — server and CLI

- State lives in `<data-dir>/sb/<id>/meta.json` (`store.Version`); pods are rebuilt from it every 30s. A `meta.json` this build does not understand is skipped, never deleted. Change the on-disk shape and you bump the version — otherwise existing sandboxes go dark.
- Reconcile order per tick is deliberate: TTL → per-sandbox disk cap → node volume headroom (under 15% free blocks create; under 10% deletes expiring sandboxes largest-first, then stops a keep-forever one) → pod/label drift. Keep the order; reordering silently changes eviction.
- Auth: tokens are `dbx_<id>_<secret>`, stored as sha256 only. Key checks are cached 30s per node, so a revoke elsewhere lands within 30s; login lockout is per-node in-memory only.
- Three credential surfaces: `Authorization: Bearer` (the scheme is case-insensitive per RFC 7235), WebSocket subprotocol `bearer.<key>`, or the `dawnbx_session` cookie. A cookie request that is not GET must carry `X-Dawnbx: 1` or it 403s.
- Fields that distinguish "unset" from "explicit" (`ttl`, `timeout`) are decided by JSON key *presence* (`decode` → `HasTTL` in `internal/api/api.go`), never by value. Any new optional field follows the same rule, and the SDKs mirror it with a sentinel.
- Server-side caps the SDKs do not enforce: fork ≤ 10 (children pinned to the parent node; the parent is `kill -STOP -1`'d and resumed in a defer), 10 MB reads, 100 MB writes.
- Host assumptions baked into the code: gVisor `RuntimeClass`, namespace `dawnbx-sandboxes`, node name == hostname, and the sandbox NetworkPolicies exist only because `install.sh` created them.
- Flags: `-data-dir` (`/var/lib/dawnbx`), `-listen` (`127.0.0.1:8080`), `-https-listen`, `-domain`, `-kubeconfig`, `-database-url`, `-pool`; env `DAWNBX_ADMIN_USER`, `DAWNBX_ADMIN_PASSWORD`. Reconcile interval and shutdown timeout are hardcoded in `cmd/dawnbx-server/main.go` — they are not flags, change them there.
- The CLI resolves `DAWNBX_URL`, then `DAWNBX_API_KEY`, then `~/.dawnbx/env`.

## 5. `web/` — dashboard

- Router basepath and Vite `base` are `/ui`; the dev proxy sends `/v1` to `http://127.0.0.1:8080`, which the server reaches because `hack/dev-vm.sh` forwards its loopback port.
- `web/src/lib/schema.d.ts` is generated and committed — regenerate it, never hand-edit it.
- `web/index.html` hardcodes `class="dark"`, so the light tokens in `web/src/index.css` are unreachable. Light-mode work starts by removing that.
- `web/components.json` points the `@/hooks` alias at a directory that does not exist. Do not add imports against it.
- Status and node views poll every 5s and `useMe` has `staleTime: Infinity`, so a logout in another tab does not refresh this one.

## 6. `sdk/` — Python and TypeScript

- Both SDKs are hand-written clients over the same `/v1/sandboxes*` routes. There is no codegen: any route or semantic change lands in both files in the same change, including the retry rule — 3 tries for GET, 1 for mutations, retry only on body code `cluster_unavailable` or a transport error.
- `ttl: null` means keep-forever, an absent key means default. The two sentinels (`_UNSET`, `undefined`) implement that and must stay in sync.
- Neither SDK wraps the terminal WebSocket, `/v1/version` or `/v1/status`; `list()` is unbounded and org-filtered server-side for non-admin keys.
- `npm test --prefix sdk/typescript` compiles first and then exercises `dist/`, so it proves the build output, not `src/`.
- Live smokes need a running server (`DAWNBX_URL`, `DAWNBX_API_KEY`): `sdk/python/tests/smoke.py`, `sdk/typescript/test/smoke.mjs`. The TS smoke covers strictly more (background exec and log readback, symlink containment, egress and `network:"none"`, `image_pull_failed`) — port a new behavior there first. `sdk/typescript/test/bench.mjs` is a latency probe wired to no script.
- Keep the Python SDK on the standard library; it ships zero dependencies.

## 7. `docs/` — docs site

- Fumadocs on Next, `output: 'export'` in `docs/next.config.mjs`, so `npm run build --prefix docs` produces a static site in `docs/out/`. Content is MDX in `docs/content/docs/` with `meta.json` sidebars, and a page missing from `meta.json` still renders.
- `SITE_URL` unset bakes `http://localhost:3000` into the OG and Twitter tags. Set it when publishing.
- Dependencies deliberately differ from `web/` (typescript `^7`, `cn` `^0.3`) because Next 16 needs them. Do not "align" them.
- The server does not serve `openapi.yaml` over HTTP. It is a repo file; the docs page says so, and clients read it from a checkout.
- The `/v1` route table is duplicated between `README.md` and `guide/api.mdx`; there is no generator keeping them equal.

## 8. Ops surface

- `deploy/aws/dawnbx.yaml` wraps `install.sh` on one arm64 EC2 with an EIP, and is the intended phase-1 path — but it has never been launched: cfn-lint clean, no stack ever created, and its `ReleaseUrl` must be hand-hosted until there is a public release. `install.sh` is the exercised path. Its `SshCommand` output is the *command* that reads the API key and admin password off the instance, not the secrets themselves — but running it puts them in your terminal, so keep that output out of tickets and CI logs.
- `install.sh` provisions one box: k3s `v1.35.5+k3s1`, gVisor, and the server on loopback. It refuses to install where a node with the same name already exists — never point it at a machine with an existing cluster.
