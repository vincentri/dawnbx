# dawnbx — agent rules

Self-hosted gVisor sandboxes for AI agents. Go control plane + CLI (`cmd/`, `internal/`), React dashboard embedded in the server binary (`web/` → `internal/api/ui`), Python and TypeScript SDKs (`sdk/`), static docs site (`docs/`), single-box installer (`install.sh`, `deploy/aws/`, `hack/`).

This file is the only agent rules file. Do not add a per-folder `AGENTS.md` until a section runs 15+ lines and is irrelevant to most tasks, or this file passes ~150 lines. A nested file is additive — never a copy of this one — and the more specific file wins where they conflict.

**Governance:** `.specify/memory/constitution.md` is the constitution (v1.2.0, ratified 2026-09-26). It supersedes practice and habit, and this file is its operational expression: where the two disagree the constitution wins, and both are corrected in the same change. New features go through Spec Kit before any code — `/speckit.specify`, then plan, tasks, implement. Do not start a feature from a chat request alone.

**State (2026-09-27).** `001-aws-control-plane` is merged into `main`: the SSH-free
AWS control plane, 164 tasks, 0 open, converged four times. The whole lifecycle is
proven against a real account — create, reach `ready`, add a worker, remove it,
rotate credentials, delete — and `install.sh` is exercised by that path, not just
by `hack/verify.sh`. Every push to `main` cuts a release.

What a green gate does **not** cover, because it ran nothing: the ten bugs found
in one session were all found by executing the product. A fake written by the
same mind that wrote the code agrees with the code, and agrees with its bugs — a
fake EC2 client accepts a request with no `ImageId`, a node list that answers with
the provider's own id. Before believing a cluster feature works, run it against an
account; `CHECK_LIVE=1 bash hack/check.sh` covers the box, and nothing but a real
provisioning run covers the control plane's own paths.

**Cluster management is provider-neutral.** The control plane (`dawnbx-server
--control-plane`) manages clusters; sandboxes still run only inside them. The
provider boundary in `internal/provider` carries values and intent — `ClusterSpec`,
`Bootstrap`, and an opaque `Handle` — never a provider's resource names. A cloud's
SDK, orchestration model and secret store live in its adapter and nowhere else, and
`internal/cluster` must not import an adapter. In the contract, a provider is a
path *value* (`/v1/providers/{provider}/…`), never a path family of its own, and no
response body may carry a stack name, security group, launch template or secret
parameter. Adding a second provider means a new adapter, not a migration.

A worker is the one exception, and it is deliberate: its id is whatever the
adapter returned, returned to the operator because `DELETE
/v1/clusters/{name}/nodes/{node}` needs a nameable id. It is a handle, never
parsed — the contract says so in the same words.

What enforces this: `internal/provider`'s import-purity test reads that package's
own non-test files and fails on an AWS import or an adapter import. It covers
`internal/provider` only, and claiming otherwise is the mistake this paragraph
exists to prevent. `internal/cluster` is kept clean by the orchestration tests
running against a fake provider, which proves testability, not the import rule.

`cluster_ops` records provisioning phases and the stall check is its only
reader: no route returns those rows and no page renders them, so a doc claiming
the dashboard shows the history is wrong.

## 1. All development runs in a git worktree

Never edit, build, test, or commit in the primary checkout, and never on `main`. One worktree per task.

**Never push, and never open a PR, without an explicit instruction in the current session.** Branches, merges, and anything else stay in the local repo. The remote is the user's call, every time.

```bash
# from the primary checkout, ~/work/private/sandbox
git fetch origin
git worktree add .worktree/<task> -b task/<slug> origin/main
cd .worktree/<task>          # every command below runs here
```

Worktrees live in `.worktree/` **inside** the primary checkout, one directory per
task, and `.worktree/` is gitignored — so a checkout of this repository can be
opened anywhere and the rule reads the same, and there is no sibling directory
outside the project to keep straight.

**A task's directory is deleted when the task is merged.** Not left behind, not
left "for reference":

```bash
git worktree remove .worktree/<task>
git branch -d task/<slug>
```

Do the `AGENTS.md` update *before* that, and merge it: a change made on a branch
that is then deleted never reached `main`.

**The tell is the path, not the intention.** Writing a file into the primary
checkout instead of the worktree happened twice in one session while doing exactly
the right thing — a CI pin fix, a release workflow — and both had to be reverted
and redone. Check the path in the `write` and `edit` call, not the plan in your
head, and `git status` in the worktree before you commit. The primary checkout is
a place you merge *into*; a task that starts there has already broken the rule
before the first line is written.

- `node_modules/` is gitignored: a new worktree has none — run `npm ci --prefix web` (and `npm ci --prefix sdk/typescript`, `npm ci --prefix docs`) once per worktree.
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
- `python3 hack/check-api-docs.py` — the two `/v1` route tables and `openapi.yaml` are all hand-maintained and this is what keeps them equal. It checks the *set* of routes, not the prose: a route in one table and not the other, a route documented that the contract never declared, and a contract route in neither table all fail. It found the terminal route being served and documented but never declared — so it never reached the generated web types.
- `python3 hack/check-harness-cites.py` — runs on every commit via `.githooks/pre-commit`; fails when a line in this file cites a file that is gone. Enable it once per clone with `git config core.hooksPath .githooks`. If it blocks you, the cite is stale or the sentence should go — do not widen the check to make it pass.

**Two gaps in that gate, each of which has already hidden a real bug.** It runs
no `go test -race`, so a concurrency invariant needs a test that fails *without*
the detector — a registry read that took no lock passed every test in the suite
while racing every write to it. And a test double is not evidence about a
query: `memStore.Ops` read a limit of 0 as unlimited where the SQL behind it
read it as no rows, so the stall check was green against the fake and dead in
production. What mattered was not that a test read through the double — the
phase history has no production reader, so reading it through the store is the
only way to observe it — but that nothing compared the double's edge cases with
the query they stand in for.

Live tier, only for sandbox / k8s / quota work: `hack/dev-vm.sh` builds and installs into the Lima VM, `hack/verify.sh` runs inside it as root against k3s, gVisor, and a real sandbox pod. Neither is a lint gate.

## 3. The one cross-area contract

`internal/api/openapi.yaml` is hand-written and is the only shared contract. `web/` types are generated from it (`npm run gen --prefix web` → committed `web/src/lib/schema.d.ts`); both SDKs and the docs site mirror it by hand. `hack/check-api-docs.py` checks the *routes* in `README.md` and `docs/content/docs/guide/api.mdx` against it — the SDKs are still mirrored by hand and nothing checks those.

Adding or changing a route therefore means, in one change: edit `openapi.yaml`, regenerate the web types, update `sdk/python/src/dawnbx/__init__.py` **and** `sdk/typescript/src/index.ts`, update `docs/content/docs/guide/api.mdx` and the table in `README.md`. The gate will name whichever one you forgot.

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
- Control-plane mode adds eight flags, and all eight are on `dawnbx-server --control-plane`: `-control-plane` itself, `-control-plane-key` (32 bytes that encrypt cluster credentials at rest; generated into `<data-dir>/server/control-plane.key` when unset), `-admin-password`, `-region`, `-release-url`, `-template`, `-key-pair`, `-ssh-cidr`. The last two are **rescue access only** — they open a provisioned host to SSH, and the product's normal path is the injected password and the minted key, with no SSH at all. A control plane started without them starts fine and provisions hosts nothing can log into if the normal path fails.
- **A provisioned cluster host's VPC must be the default VPC, or `VpcCidr` must be set on the stack the adapter creates.** The AWS adapter never sends `VpcCidr`, so `deploy/aws/dawnbx.yaml` falls back to 172.31.0.0/16 for the 6443 rule. A host landing in any other CIDR has a security group that does not describe its own network: a worker added to that cluster cannot reach the apiserver on 6443, and nothing in the product reports it. The failure is silent by construction — check the CIDR before adding a worker.
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

- `deploy/aws/dawnbx.yaml` wraps `install.sh` on one arm64 EC2 with an EIP, and is the intended phase-1 path. It has now been launched repeatedly against a real account, and the control plane creates clusters from it unattended. Its `ReleaseUrl` points at a GitHub release, cut by `.github/workflows/release.yml` on every push to `main`. The base is one release tag's asset prefix, `https://github.com/OWNER/dawnbx/releases/download/vX.Y.Z`; GitHub also serves a `latest` alias for the newest release, and that one moves, so pin the tag for anything you care about. Its `SshCommand` output is the *command* that reads the API key and admin password off the instance, not the secrets themselves — but running it puts them in your terminal, so keep that output out of tickets and CI logs.
- k3s deploys traefik as a packaged ingress controller whose ServiceLB claims host ports 80 and 443 with **iptables rather than a listening socket**. dawnbx creates no Ingress and serves its own HTTPS on 443, so k3s's config disables traefik. Without that, every request is answered by traefik's default certificate and a 404 while `ss` still shows `dawnbx-server` listening — the socket is there and the traffic never reaches it.
- Ubuntu 24.04, the image the template defaults to, has **no `awscli` package at all**. `install.sh` reads its bootstrap parameter with a stdlib SigV4 signer using the instance role, and uses the CLI only on a distro that ships one. Do not reintroduce an `apt-get install` of it; there is nothing to install.
- A worker has two names and the two sides never agree: a cluster calls it `ip-172-31-22-242`, the provider calls it `i-0ad9fe…`. The address is the only bridge, so `Provider.NodeAddrs` exists and `NodeRef` carries both. A cluster that reports itself ready and cannot be reached is a `503 cluster_unreachable` carrying the reason — never an empty list, which reads as "no workers" and is the answer that leaves an operator waiting on a repair that is already broken.
- `install.sh` provisions one box: k3s `v1.35.5+k3s1`, gVisor, and the server on loopback. It refuses to install where a node with the same name already exists — never point it at a machine with an existing cluster.
