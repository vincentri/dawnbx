# dawnbx

Self-hosted sandboxes for AI agents on one Linux box. Each sandbox is a gVisor
container on k3s with its own `/workspace`, which you can run commands in, read
and write files in, and fork. You get an HTTP API, a CLI, Python and TypeScript
SDKs, and a web dashboard with a terminal and file upload.

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
- A worker's disk isn't watched for low space yet. Only the server's is.
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
import { Sandbox } from "dawnbx"; // sdk/typescript, Node 18+

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
  `/var/lib/dawnbx/server/dawnbx.db`. To run several API nodes against one
  database, pass `--database-url postgres://...` to `dawnbx-server`.

## API

All routes are under `/v1` and need `Authorization: Bearer <key>`. Key, user,
org and password routes need a dashboard session instead. Errors are
JSON: `{"code", "message", "hint"}`.

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
| `GET /v1/me` | who the key or session is: `{"org", "key_id", "user", "admin"}` |
| `GET` / `POST /v1/keys`, `DELETE /v1/keys/{id}` | list, create `{"name", "ttl": "720h", "org"}` (the key is returned once; `org` is admin-only), revoke |
| `GET /v1/audit` | last 200 events of your org (admins: all) |
| `POST /v1/me/password` | `{"old", "new"}`: 10 to 72 chars; ends your sessions |
| `GET` / `POST /v1/orgs` | admin: list, create `{"id", "name"}` |
| `GET` / `POST /v1/users`, `DELETE /v1/users/{name}` | admin: list, create `{"username", "password", "org", "role"}`, delete |
| `POST /v1/users/{name}/password` | admin: `{"password"}` |
| `POST /v1/login`, `/logout` | dashboard session cookie (needs header `X-Dawnbx: 1`) |
| `GET /v1/version` | no auth |

## Develop

```sh
go test -race ./...
python3 -m unittest discover -s sdk/python/tests
(cd sdk/typescript && npm test)
python3 sdk/python/tests/smoke.py    # end-to-end against a live server
DAWNBX_TEST_DATABASE_URL=postgres://... go test ./internal/auth   # auth on Postgres (drops its tables first)
```

## License

Apache-2.0
