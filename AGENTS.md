# dawnbx — agent rules

Self-hosted gVisor sandboxes for AI agents. Go control plane + CLI (`cmd/`, `internal/`), React dashboard embedded in the server binary (`web/` → `internal/api/ui`), Python and TypeScript SDKs (`sdk/`), static docs site (`docs/`), single-box installer (`install.sh`, `deploy/aws/`, `hack/`).

## 1. All development runs in a git worktree

Never edit, build, test, or commit in the primary checkout, and never on `main`. One worktree per task.

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

- `go build ./... && go test ./...` — server, CLI, core
- `npm run typecheck --prefix web` — dashboard
- `npm test --prefix sdk/typescript` — TS SDK (type-checks, then tests `dist/`)
- `cd sdk/python && python3 -m unittest discover -s tests` — Python SDK
- `npm run types:check --prefix docs` — docs site
- `npm run build --prefix web` — **required for any `web/` change**, see §3

Live tier, only for sandbox / k8s / quota work: `hack/dev-vm.sh` builds and installs into the Lima VM, `hack/verify.sh` runs inside it as root against k3s, gVisor, and a real sandbox pod. Neither is a lint gate.

## 3. The one cross-area contract

`internal/api/openapi.yaml` is hand-written and is the only shared contract. `web/` types are generated from it (`npm run gen --prefix web` → committed `web/src/lib/schema.d.ts`); both SDKs and the docs site mirror it by hand. Nothing in CI checks that they agree.

Adding or changing a route therefore means, in one change: edit `openapi.yaml`, regenerate the web types, update `sdk/python/src/dawnbx/__init__.py` **and** `sdk/typescript/src/index.ts`, update `docs/content/docs/guide/api.mdx`.

Same for the UI: Vite writes its build straight into `internal/api/ui` with `emptyOutDir`, that directory is committed, and `internal/api/api.go` embeds it with `//go:embed ui`. Skip `npm run build --prefix web` and the binary serves the old dashboard. Releases do the same via a `.goreleaser.yaml` pre-hook, so a release built without Node installed is not a thing.

## 4. `internal/` + `cmd/` — server and CLI

- State lives in `<data-dir>/sb/<id>/meta.json` (`store.Version`); pods are rebuilt from it every 30s. A `meta.json` this build does not understand is skipped, never deleted. Change the on-disk shape and you bump the version — otherwise existing sandboxes go dark.
- Reconcile order per tick is deliberate: TTL → per-sandbox disk cap → node volume headroom (under 15% free blocks create; under 10% deletes expiring sandboxes largest-first, then stops a keep-forever one) → pod/label drift. Keep the order; reordering silently changes eviction.
- Auth: tokens are `dbx_<id>_<secret>`, stored as sha256 only. Key checks are cached 30s per node, so a revoke elsewhere lands within 30s; login lockout is per-node in-memory only.
- Three credential surfaces: `Authorization: Bearer`, WebSocket subprotocol `bearer.<key>`, or the `dawnbx_session` cookie. A cookie request that is not GET must carry `X-Dawnbx: 1` or it 403s.
- Fields that distinguish "unset" from "explicit" (`ttl`, `timeout`) are decided by JSON key *presence* (`decode` → `HasTTL` in `internal/api/api.go`), never by value. Any new optional field follows the same rule, and the SDKs mirror it with a sentinel.
- Server-side caps the SDKs do not enforce: fork ≤ 10 (children pinned to the parent node; the parent is `kill -STOP -1`'d and resumed in a defer), 10 MB reads, 100 MB writes.
- Host assumptions baked into the code: gVisor `RuntimeClass`, namespace `dawnbx-sandboxes`, node name == hostname, and the sandbox NetworkPolicies exist only because `install.sh` created them.
- Flags: `-data-dir` (`/var/lib/dawnbx`), `-listen` (`127.0.0.1:8080`), `-https-listen`, `-domain`, `-kubeconfig`, `-database-url`, `-pool`; env `DAWNBX_ADMIN_USER`, `DAWNBX_ADMIN_PASSWORD`. Reconcile interval and shutdown timeout are hardcoded in `cmd/dawnbx-server/main.go` — they are not flags, change them there.
- The CLI resolves `DAWNBX_URL`, then `DAWNBX_API_KEY`, then `~/.dawnbx/env`.

## 5. `web/` — dashboard

- Router basepath and Vite `base` are `/ui`; the dev proxy sends `/v1` to `http://127.0.0.1:8080`, which the server reaches because `hack/dev-vm.sh` forwards its loopback port.
- `web/src/lib/schema.d.ts` is generated and committed — regenerate it, never hand-edit it.
- `web/index.html` hardcodes `class="dark"`, so the light tokens in `web/src/index.css` are unreachable. Light-mode work starts by removing that.
- `web/components.json` points the `@/hooks` alias at a directory that does not exist; `textarea.tsx` and `Tooltip*` are unused. Do not import against them.
- Status and node views poll every 5s and `useMe` has `staleTime: Infinity`, so a logout in another tab does not refresh this one.

## 6. `sdk/` — Python and TypeScript

- Both SDKs are hand-written clients over the same six `/v1/sandboxes*` routes. There is no codegen: any route or semantic change lands in both files in the same change, including the retry rule — 3 tries for GET, 1 for mutations, retry only on body code `cluster_unavailable` or a transport error.
- `ttl: null` means keep-forever, an absent key means default. The two sentinels (`_UNSET`, `undefined`) implement that and must stay in sync.
- Neither SDK wraps the terminal WebSocket, `/v1/version` or `/v1/status`; `list()` is unbounded and org-filtered server-side for non-admin keys.
- `npm test --prefix sdk/typescript` compiles first and then exercises `dist/`, so it proves the build output, not `src/`.
- Live smokes need a running server (`DAWNBX_URL`, `DAWNBX_API_KEY`): `sdk/python/tests/smoke.py`, `sdk/typescript/test/smoke.mjs`. The TS smoke covers strictly more (background exec and log readback, symlink containment, egress and `network:"none"`, `image_pull_failed`) — port a new behavior there first. `sdk/typescript/test/bench.mjs` is a latency probe wired to no script.
- Keep the Python SDK on the standard library; it ships zero dependencies.

## 7. `docs/` — docs site

- Fumadocs on Next with `output: 'export'`: `npm run build --prefix docs` writes static HTML to `docs/out/`. Content is MDX in `docs/content/docs/` with `meta.json` sidebars, and a page missing from `meta.json` still renders.
- `SITE_URL` unset bakes `http://localhost:3000` into the OG and Twitter tags. Set it when publishing.
- Dependencies deliberately differ from `web/` (typescript `^7`, `cn` `^0.3`) because Next 16 needs them. Do not "align" them.
- Known wrong today: `docs/content/docs/guide/api.mdx` says the server serves `/openapi.yaml`. No such route exists. Fix it when you touch that page.
- The `/v1` route table is duplicated between `README.md` and `guide/api.mdx`; there is no generator keeping them equal.

## 8. Ops surface

- `install.sh` provisions one box: k3s `v1.35.5+k3s1`, gVisor, and the server on loopback. It refuses to install where a node with the same name already exists — never point it at a machine with an existing cluster.
- `deploy/aws/dawnbx.yaml` is one arm64 EC2 with an EIP, and its `SshCommand` output prints the API key and the admin password. Treat CloudFormation stack output as a secret.
