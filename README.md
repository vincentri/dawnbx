# dawnbx

Self-hosted sandboxes for AI agents on one Linux box. Each sandbox is a gVisor
container on k3s with its own `/workspace`, which you can run commands in, read
and write files in, and fork. You get an HTTP API, a CLI, Python and TypeScript
SDKs, and a web dashboard with a terminal and file upload.

Full documentation: [docs/](./docs) builds the docs site with Fumadocs
(`cd docs && npm install && npm run dev`).

## Install

On a Linux server (amd64 or arm64):

```sh
sudo ./install.sh --domain sb.example.com   # Let's Encrypt; DNS must point here, ports 80+443 open
sudo ./install.sh                           # no domain: HTTPS with a self-signed cert
sudo ./install.sh --local                   # laptop/VM: HTTP on 127.0.0.1:8080 only
```

When it finishes, the installer prints `DAWNBX_URL`, `DAWNBX_API_KEY`, and the
dashboard login (user `admin` plus a generated password). It also saves the URL
and key to `~/.dawnbx/env`. `./install.sh --help` lists all flags.

- To choose the admin password, run `sudo DAWNBX_ADMIN_PASSWORD=... ./install.sh`.
- Lost the password? Put a new one in `/var/lib/dawnbx/server/admin.env`
  (`DAWNBX_ADMIN_PASSWORD=...`, read as-is, no quoting), then run
  `systemctl restart dawnbx`. A password changed in the dashboard survives
  restarts until that line changes.
- Lost the key? Make a new one in the dashboard under **Settings**, or re-run
  with `--new-key`.

Run it without a terminal (cloud-init, CI) and it prints the paths of the key
and password files, never the secrets themselves.

### AWS

`deploy/aws/dawnbx.yaml` is a CloudFormation stack: one arm64 EC2 instance
(t4g.medium by default) with an Elastic IP and HTTPS on `<ip>.sslip.io`, or on
your domain. Create it in the console (**Create stack > Upload a template**)
and fill in the key pair, your IP for SSH, and `ReleaseUrl`: the base URL of a
release (see [Release](#release)). Releases are cut automatically on every push to
`main`, so `ReleaseUrl` is
`https://github.com/vincentri/dawnbx/releases/download/v0.1.12` (pin a version;
`latest/download` moves). The stack finishes when the installer does, about
8 min. Then the `SshCommand` output prints the API key and admin password.

The template has been launched repeatedly against a real account, including a
full lifecycle on `v0.1.12`: a cluster reached ready behind a genuine Let's
Encrypt certificate whose SPKI matched the pin the control plane stored, a worker
joined and was removed, and the account swept clean afterwards. Prefer
`install.sh` on a box you already have when you only need one server.

- `t4g.medium` (4 GB) is the smallest size that installs: the host runs k3s, gVisor
  and dawnbx-server, and the apiserver is still starting when memory is tightest.
  A run is about 20 minutes, so roughly $0.04.
- The Elastic IP is released with the stack. On a failed run, check for a leftover
  one: an unattached address is billed until it goes.

- `CpuCredits` defaults to `standard`: the price stays fixed, but CPU slows down
  once burst credits run out. `unlimited` stays fast and bills the extra.
- If creation fails, re-create with rollback disabled ("Preserve successfully
  provisioned resources"). Then SSH in and read `/var/log/cloud-init-output.log`.
- Workers: launch them into the stack's `SecurityGroup` output, then run the
  join command on them (see below).

### More machines

Sandboxes can run on extra Linux boxes (workers). Node traffic goes over
WireGuard, so the kernel needs the `wireguard` module (any recent Ubuntu or
Debian has it).

```sh
sudo ./install.sh --allow-join 10.0.0.0/16   # on the server: which network may reach k3s (port 6443)
sudo ./install.sh --join https://10.0.0.5:6443 K10...   # on the worker: the command from Settings > Nodes
```

The join command is under **Settings > Nodes** (admins only; viewing it is
logged). The token in it lets a machine join the cluster, so treat it like a
password. Boxes also need UDP 51820 open to each other.

- A sandbox's files live on the node that runs it. Its forks go on the same
  node. If the node dies, its sandboxes stay unavailable until it comes back.
- Use the same `--data-dir` on workers as on the server (the default is fine).
- Each node's disk is watched. Under 15% free, new sandboxes skip that
  worker and forks of its sandboxes fail with `disk_low`. Under 10%, sandboxes on it that have a TTL are deleted,
  biggest first, then keep-forever ones are stopped. When no node has room,
  create fails with `no_room`.
- Remove a worker from **Settings > Nodes** once it holds no sandboxes, then
  run `k3s-agent-uninstall.sh` on it. Re-joining a box that has the same
  hostname needs that removal first.

On a Mac, [Lima](https://lima-vm.io) runs it in a VM:

```sh
brew install lima
hack/dev-vm.sh                 # about 10 min the first time; re-run to upgrade
hack/dev-vm.sh dawnbx --new-key   # print a fresh key (old one stops working)
```

The script copies the VM's `~/.dawnbx/env` to the Mac, so the CLI and SDKs work
from the host against `http://127.0.0.1:8080`.

## Hello world

```sh
go install ./cmd/dawnbx     # CLI into $(go env GOPATH)/bin; reads ~/.dawnbx/env
id=$(dawnbx create) && dawnbx exec $id "python -c 'print(6*7)'" && dawnbx kill $id
```

`dawnbx doctor` checks the connection and key and says how to fix what fails.

```python
from dawnbx import Sandbox  # sdk/python

with Sandbox.create() as sb:
    print(sb.exec("python -c 'print(6*7)'").stdout)
```

```ts
import { Sandbox } from "dawnbx"; // sdk/typescript, Node 24+

const sb = await Sandbox.create();
try {
  console.log((await sb.exec("python -c 'print(6*7)'")).stdout);
} finally {
  await sb.kill();
}
```

The SDKs are not on PyPI or npm yet, so install them from the checkout.
Python: `pip install ./sdk/python`. TypeScript: run `npm install && npm run build`
in `sdk/typescript`, then `npm install <path-to>/sdk/typescript` in your project.
Both read `DAWNBX_URL` and `DAWNBX_API_KEY` from the environment
(`source ~/.dawnbx/env`), or take `url` / `api_key` (TS: `apiKey`).

```sh
source ~/.dawnbx/env
curl -s -H "Authorization: Bearer $DAWNBX_API_KEY" -d '{}' $DAWNBX_URL/v1/sandboxes
```

## Auth

- **API keys** are for the SDKs, CLI and curl. Make and revoke them in the
  dashboard under **Settings**. A revoked key stops working within 30 s. Keys
  can't manage keys or users, so a leaked key can't mint more.
- **Dashboard** sign-in is a username and password. The session is an HttpOnly
  cookie. After 5 wrong passwords, that username is locked out from that IP for
  15 minutes.
- **Orgs and users:** each key and user belongs to an org and sees only that
  org's sandboxes, keys and audit log. Admins see every org, and add orgs and
  users (role `member` or `admin`) under **Settings**. Changing a password
  signs that user out everywhere.
- **Audit log:** creates, kills, forks, logins, key, user and org changes are
  recorded there.
- **Storage:** everything above lives in SQLite at
  `/var/lib/dawnbx/server/dawnbx.db` by default, and in PostgreSQL when
  `--database-url postgres://...` is passed. The control plane runs on
  PostgreSQL, because it is designed for more than one API node and SQLite is
  single-writer; the auth suite is tested on both engines.

## API

All routes are under `/v1` and need `Authorization: Bearer <key>`. Key, user,
org and password routes need a dashboard session instead. The cluster routes
need a dashboard session too — an API key can never manage infrastructure —
and all but the capability probe and the provider list need an administrator's.
Errors are JSON: `{"code", "message", "hint"}`.

| Route | |
|---|---|
| `POST /v1/sandboxes` | create: `{"image", "ttl", "network": "internet"\|"none", "cpu", "memory"}`; `"ttl": null` keeps it until killed |
| `GET /v1/sandboxes`, `GET /v1/sandboxes/{id}` | list, get |
| `DELETE /v1/sandboxes/{id}` | kill |
| `POST /v1/sandboxes/{id}/exec` | `{"cmd", "timeout", "background", "env"}` → `{"exit_code", "stdout", "stderr"}` |
| `GET` / `PUT /v1/sandboxes/{id}/files?path=` | read / write a file under `/workspace` |
| `POST /v1/sandboxes/{id}/fork` | copy `/workspace` into new sandboxes |
| `POST /v1/sandboxes/{id}/extend`, `/start` | change TTL, restart a stopped sandbox |
| `GET /v1/sandboxes/{id}/terminal` | WebSocket TTY (used by the dashboard) |
| `GET /v1/status` | this server's health: `{"version", "free_pct", "warm", "pool_size"}`. A control plane answers `503 cluster_unavailable`, because it has no runtime of its own |
| `GET /v1/me` | who the key or session is: `{"org", "key_id", "user", "admin"}` |
| `GET` / `POST /v1/keys`, `DELETE /v1/keys/{id}` | list, create `{"name", "ttl": "720h", "org"}` (the key is returned once; `org` is admin-only), revoke |
| `GET /v1/audit` | last 200 events of your org (admins: all) |
| `POST /v1/me/password` | `{"old", "new"}`: 10 to 72 chars; ends your sessions |
| `GET` / `POST /v1/orgs` | admin: list, create `{"id", "name"}` |
| `GET` / `POST /v1/users`, `DELETE /v1/users/{name}` | admin: list, create `{"username", "password", "org", "role"}`, delete |
| `POST /v1/users/{name}/password` | admin: `{"password"}` |
| `POST /v1/login`, `/logout` | dashboard session cookie (needs header `X-Dawnbx: 1`) |
| `GET /v1/version` | no auth |
| `GET /v1/nodes` | admin: the boxes in this cluster, their role and sandbox counts |
| `GET /v1/nodes/join` | admin: the command that adds a worker (audited) |
| `DELETE /v1/nodes/{name}` | admin: remove a worker; `409` while it holds sandboxes |
| `GET /v1/control-plane` | session: control plane or cluster, and which providers this mode can use |
| `GET /v1/providers` | session: which providers this control plane can use, and which are listed but not yet available |
| `GET /v1/providers/{p}/regions`, `/instance-types?region=` | admin: what a provider offers here, and at what price. With no `region` the catalogue is priced for the provider's first advertised region, so it is indicative; `POST /v1/providers/{p}/estimate` is priced for the region you name, and its `quote_id` is what the create demands |
| `POST /v1/providers/{p}/estimate` | admin: price a configuration; creates nothing, and its `quote_id` is what the create demands |
| `GET` / `POST /v1/clusters` | admin: list, and start provisioning given a `quote_id` from the estimate |
| `GET` / `DELETE /v1/clusters/{name}` | admin: one cluster, and delete it; `409` while workers are attached |
| `GET /v1/clusters/{name}/credentials` | admin, audited: the cluster's key and password, no SSH needed; `409` until it is ready |
| `POST /v1/clusters/{name}/rotate` | admin, audited: mint a new pair and retire the old key |
| `GET` / `POST /v1/clusters/{name}/nodes` | admin: list workers, add one |
| `DELETE /v1/clusters/{name}/nodes/{node}` | admin: remove a worker; `409` while it holds sandboxes |

`docs/content/docs/guide/api.mdx` is the long form; this table is a copy of it
and nothing keeps the two equal, so change both.

## Develop

### Test it

One command runs everything: lint, builds, unit tests, coverage floors, and the
browser suite. It needs Go, Node 24 and Python; the browser suite also needs
Docker.

```sh
bash hack/check.sh                  # the gate, with the browser suite (~3 min)
SKIP_E2E=1 bash hack/check.sh       # no Docker: skips the suite, and says so
CHECK_LIVE=1 bash hack/check.sh     # adds a Lima VM: real k3s, gVisor, installer
```

The live tier is the only thing that runs `install.sh` and the CloudFormation
user-data for real. It needs [Lima](https://lima-vm.io) (`brew install lima`) and
takes about five minutes. It has earned its place: it caught a `local` in
`install.sh` that silently broke the installer, which the gate above could not
see.

### The browser suite on its own

18 tests drive the control-plane dashboard in a real browser and record a video
of every run. It runs entirely in Docker Compose — PostgreSQL, the server and the
browser — so nothing is installed on your machine except the recordings.

```sh
bash e2e/run.sh                     # the whole suite, ~1.5 min
bash e2e/run.sh -- -g "sign in"     # one test
docker compose -f e2e/docker-compose.yml up -d server   # browse it at :18080
docker compose -f e2e/docker-compose.yml down -v        # remove everything
```

Recordings land in `.e2e/`, which is git-ignored. Locally every run is kept, so a
*passing* run can be watched to confirm the journey really happened; in CI only
failures are kept, so a green push uploads nothing.

Use `e2e/run.sh` rather than a raw `docker compose run`: the script starts from a
fresh stack, and a raw run inherits the last one's state and then fails on a
cluster name that is already taken.

The suite runs against a **test provider**, not a cloud. It proves the
operator-facing interface and the orchestration above the provider boundary, and
proves nothing about whether a real cloud accepts what is sent — that is what a
real-account lifecycle is for. The [e2e guide](docs/content/docs/guide/e2e.mdx)
covers running it and watching a recording.

### Against a real database

The gate runs the auth suite on SQLite, and again on PostgreSQL in a container.
To use your own:

```sh
DAWNBX_TEST_DATABASE_URL='postgres://user:pass@host:5432/db?sslmode=disable' \
  go test ./internal/auth/ -count=1
```

It drops the tables first, so point it at a scratch database.

### The dashboard

React + Vite in `web/`. `npm run build` writes to `internal/api/ui`, which the
server embeds; that output is committed so `go build` needs no Node. API types
come from `internal/api/openapi.yaml` (`npm run gen` after changing it).

```sh
cd web && npm install && npm run dev   # on :5173, proxies /v1 to 127.0.0.1:8080
```

### Release

Every push to `main` cuts a release once the gate is green, named
`v0.1.<run-number>`, and its assets are fetchable without an account. Locally:

```sh
go run github.com/goreleaser/goreleaser/v2@latest release --snapshot --clean   # into dist/
```

A release is `install.sh`, `checksums.txt` and binaries named
`<binary>-<os>-<arch>` (e.g. `dawnbx-server-linux-arm64`). Wherever those are
served over HTTPS, `sudo ./install.sh --release-url <base-url>` downloads the
binaries and checks them against `checksums.txt`.

Pin a version. `latest/download` moves, and a cluster that installs one is not
reproducible.

### Work on this in a worktree

Not on `main`, and not in the primary checkout:

```sh
git worktree add .worktree/<task> -b task/<slug> main
cd .worktree/<task>
npm ci --prefix web && npm ci --prefix sdk/typescript && npm ci --prefix docs
```

`.worktree/` is git-ignored and excluded from the Docker build context. When the
work merges, remove both:

```sh
git worktree remove .worktree/<task> && git branch -d task/<slug>
```

`AGENTS.md` has the per-area contracts; read it before changing the installer,
the provider boundary, or the gate.

## License

Apache-2.0
