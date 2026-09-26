# Quickstart: Validating the SSH-Free AWS Control Plane

Phase 1 output. This is a validation and run guide, not an implementation guide: it says how to
exercise the feature end to end and what "working" looks like at each step. Code, migrations and
test bodies belong in `tasks.md` and the implementation phase.

Every step states the observable result. A step that cannot be checked from the outside is not a
validation step.

## Prerequisites

```bash
# one per worktree
npm ci --prefix web && npm ci --prefix sdk/typescript && npm ci --prefix docs
python3 -m venv .venv && .venv/bin/pip install coverage
```

- An AWS account, a region with the six instance types in
  `deploy/aws/dawnbx.yaml:27` available, and credentials in the standard chain
  (`aws sts get-caller-identity` must succeed for whoever runs this).
- A hosted release artifact. `ReleaseUrl` is a required CloudFormation parameter with no default
  (`deploy/aws/dawnbx.yaml:44-49`) and must be a clean `https://` base serving `install.sh`,
  `checksums.txt`, and the `dawnbx-server-linux-arm64` / `dawnbx-linux-arm64` binaries
  (`install.sh:431-443`). Without it the create step cannot run; that is a real prerequisite, not
  a test artifact.
- **A throwaway account or strict budget alarm.** Creating a cluster starts a billable EC2 instance.
  `--disable-termination-protection` is not involved; the cleanup step below is what stops the cost.

## Tier 1 — the gate, no AWS, no k3s

The whole feature is provable without spending money.

```bash
bash hack/check.sh
```

Expected: `total 90.8% (floor 90.8%)` or higher, `check.sh: all green`, exit 0. The floors in
`hack/coverage-floor.txt` may only have risen. A dirty `internal/api/ui` fails the build
(`hack/check.sh:101-107`), so a green run also proves the dashboard bundle was rebuilt.

For the installer and template changes specifically:

```bash
shellcheck -S error install.sh hack/*.sh
cfn-lint deploy/aws/dawnbx.yaml
```

Expected: no output, exit 0 for both.

## Tier 2 — control-plane startup without a cluster

This is the core of FR-001 and needs no AWS at all.

```bash
rm -rf /tmp/dawnbx-cp /tmp/dawnbx-cp-data
go build -o /tmp/dawnbx-cp ./cmd/dawnbx-server

# The binary is a file and the data dir is a directory under it; pointing both
# at /tmp/dawnbx-cp is a "not a directory" error, not a subtle one, but the
# quickstart is the first thing anyone runs, so it has to actually run.
/tmp/dawnbx-cp --control-plane \
  --data-dir /tmp/dawnbx-cp-data \
  --listen 127.0.0.1:8099 \
  --admin-password 'a-long-test-password' &
```

Check all four, because each one is a distinct thing the old startup path refused to do:

```bash
ls -a /tmp/dawnbx-cp-data            # no .dawnbx-volume marker, and startup did not create one
curl -s 127.0.0.1:8099/v1/version    # 200 — no k3s client was built
curl -s 127.0.0.1:8099/v1/sandboxes  # 503 {"code":"cluster_unavailable",...}, not 404 and not a panic
curl -s -o /dev/null -w '%{http_code}\n' 127.0.0.1:8099/ui/   # 200 — the dashboard is served
```

Then the negative control, which is the part that is easy to get wrong:

```bash
/tmp/dawnbx-cp --data-dir /tmp/dawnbx-cp-data   # without --control-plane
```

Expected: exits non-zero with the marker error from `internal/store/store.go:73`. The old behaviour
must still be the default; `--control-plane` is opt-in.

Log in and confirm the identity surface still works — the control plane reuses it unchanged
(`internal/api/manage.go:67-296`):

```bash
curl -s -X POST -H 'X-Dawnbx: 1' -H 'Content-Type: application/json' \
  -d '{"username":"admin","password":"a-long-test-password"}' \
  -c /tmp/dawnbx-cp/cookies 127.0.0.1:8099/v1/login -o /dev/null -w '%{http_code}\n'   # 200

curl -s -b /tmp/dawnbx-cp/cookies -H 'X-Dawnbx: 1' 127.0.0.1:8099/v1/providers
```

Expected: `{"providers":[{"id":"aws","available":true},{"id":"gcp","available":false},{"id":"azure","available":false}]}`.

Then confirm the authorisation boundary with an API key, not a session — the property is that keys
can never manage infrastructure:

```bash
KEY=$(curl -s -X POST -H 'X-Dawnbx: 1' -b /tmp/dawnbx-cp/cookies \
  -d '{"name":"probe"}' 127.0.0.1:8099/v1/keys | sed -E 's/.*"key":"([^"]+)".*/\1/')
curl -s -H "Authorization: Bearer $KEY" 127.0.0.1:8099/v1/clusters   # 403 forbidden
```

## Tier 3 — the price gate, no AWS resources created

`POST /v1/aws/estimate` is read-only. It is safe to run against a real account.

```bash
curl -s -b /tmp/dawnbx-cp/cookies -H 'X-Dawnbx: 1' -H 'Content-Type: application/json' \
  -d '{"region":"eu-west-1","instance_type":"t4g.medium","disk_gib":30,"domain":""}' \
  127.0.0.1:8099/v1/aws/estimate
```

Expected: a `quote_id`, `lines` with `compute`, `storage` and `public_ipv4`, and
`excluded` containing `data_transfer`, `taxes`, `provider_discounts` (FR-005, SC-009).

Then the stale-quote guard, which is the property that makes the price review real:

```bash
# same quote_id, different instance type
curl -s -b /tmp/dawnbx-cp/cookies -H 'X-Dawnbx: 1' -H 'Content-Type: application/json' \
  -d '{"name":"probe1","region":"eu-west-1","instance_type":"t4g.large","disk_gib":30,"domain":"","quote_id":"'"$QUOTE"'"}' \
  127.0.0.1:8099/v1/clusters
```

Expected: `409 {"code":"quote_stale",...}`. And before any of that, `POST /v1/clusters` with **no**
`quote_id` is a `400`, not a provisioned cluster.

## Tier 4 — a real cluster, end to end

Run this in a throwaway account. It creates a billable instance.

```bash
# 1. create — returns provisioning, no SSH anywhere in this flow
curl -s -b /tmp/dawnbx-cp/cookies -H 'X-Dawnbx: 1' -H 'Content-Type: application/json' \
  -d '{"name":"probe1","region":"eu-west-1","instance_type":"t4g.medium","disk_gib":30,"domain":"","quote_id":"'"$QUOTE"'"}' \
  127.0.0.1:8099/v1/clusters

# 2. watch phases, 5s apart
watch -n5 "curl -s -b /tmp/dawnbx-cp/cookies -H 'X-Dawnbx: 1' \
  127.0.0.1:8099/v1/clusters/probe1 | jq '{status,phase,detail,url,tls_pin}'"
```

Check each transition, in order. The list is the state machine in `data-model.md`:

| Phase | Expected | Fails if |
|---|---|---|
| `validating` → `requesting_host` | the provider's secret channel holds the admin password, and the host request is in flight | the secret would be in the stack |
| `requesting_host` | on AWS, `DescribeStacks` shows `CREATE_IN_PROGRESS` | the control plane needed a public route |
| `bootstrapping` | the `WaitCondition` is satisfied; `detail` carries the sanitised reason from `install.sh:56-63` | `detail` contains anything resembling a key or password |
| `verifying` | the control plane logs into the cluster's own API with the injected password | a cluster can reach `ready` without proving it works |
| `minting_key` | `POST /v1/keys` on the cluster succeeds; the key is stored encrypted | the key crossed a channel it never needed to cross |
| `ready` | `url` non-empty, `tls_pin` non-empty | FR-009 is unmet |

**The secret-leak check, which is the one to run by hand.** After the cluster is ready, confirm
the credential never appeared anywhere AWS would echo it:

```bash
# the intended channel: the password is here, in the provider's secret store
aws ssm get-parameter --name /dawnbx/cluster/probe1 --with-decryption

# the leak checks: neither surface may contain it
aws cloudformation describe-stacks --stack-name probe1 \
  | grep -iE 'admin_password' && echo "LEAK" || echo "clean: template and outputs"
aws ec2 describe-instance-attribute --attribute userData \
  --instance-id "$(aws ec2 describe-instances \
     -f "Name=tag:aws:cloudformation:stack-name,Values=probe1" \
     -f 'Reservations[].Instances[].InstanceId' --query 'Reservations[].Instances[].InstanceId' --output text)" \
  --query UserData.Value | base64 -d | grep -iE 'DAWNBX_ADMIN_PASSWORD=' && echo "LEAK" || echo "clean: user-data"
```

Expected: the SSM lookup returns the password (that is the intended channel), and the other two print
`clean`. This is the mechanical form of SC-007 and of the recorded decision that `NoEcho` does not
protect a value inside `UserData`.

**Only the password is injected.** The API key never appears in the SSM parameter, the stack, or the
user-data, because the cluster mints it itself during `minting_key`. So the greps above look for the
password, and a `dawnbx_<hex>` key shape appearing in *any* of the three surfaces is itself a bug.

Then the operator-facing properties:

```bash
curl -s -b /tmp/dawnbx-cp/cookies -H 'X-Dawnbx: 1' 127.0.0.1:8099/v1/clusters/probe1/credentials
# the API key and admin password, without SSH
# 409 credentials_not_ready if the cluster has not reached `ready` yet

curl -s -b /tmp/dawnbx-cp/cookies -H 'X-Dawnbx: 1' -H 'Content-Type: application/json' \
  -d '{"instance_type":"t4g.medium","disk_gib":30}' \
  127.0.0.1:8099/v1/clusters/probe1/nodes          # a worker, provisioning
```

The worker becomes `ready` when the cluster reports it Ready. A second worker is the FR-011 case:
create a sandbox on it, then attempt removal and expect
`409 {"code":"node_holds_sandboxes",...}` with the count in the message.

**Cleanup, so the tier stops costing money:**

```bash
curl -s -X DELETE -b /tmp/dawnbx-cp/cookies -H 'X-Dawnbx: 1' \
  127.0.0.1:8099/v1/clusters/probe1/nodes/<node>    # each worker first; 409 while it holds sandboxes
curl -s -X DELETE -b /tmp/dawnbx-cp/cookies -H 'X-Dawnbx: 1' 127.0.0.1:8099/v1/clusters/probe1
aws cloudformation wait stack-delete-complete --stack-name probe1
aws ssm get-parameter --name /dawnbx/cluster/probe1   # expect ParameterNotFound
```

## Tier 5 — failure and cleanup

The recorded clarification requires automatic removal of resources created exclusively for a failed
operation, and the price is paid for by SC-008.

Force a failure the honest way: point `ReleaseUrl` at a base that serves an `install.sh` which
exits non-zero after the stack exists. Then:

```bash
# after the failure lands
curl -s -b /tmp/dawnbx-cp/cookies -H 'X-Dawnbx: 1' 127.0.0.1:8099/v1/clusters/probe2 \
  | jq '{status,detail}'
```

Expected: `status` is `failed`, `detail` is the wait condition's reason with no secret, and then:

```bash
aws cloudformation describe-stacks --stack-name probe2     # eventually gone or DELETE_COMPLETE
aws ssm get-parameter --name /dawnbx/cluster/probe2          # ParameterNotFound
```

A cluster that was already `ready` and later fails must **not** be auto-deleted: preserve the
running cluster and report that new provider actions need renewed access. Check that by expiring the
control plane's AWS credentials with `probe1` still `ready`.

## Tier 6 — the dashboard

```bash
npm run dev --prefix web     # proxies /v1 to the control plane
```

Open `/ui/clusters` and walk the wizard: AWS is selectable, GCP and Azure are visible and
unselectable, the estimate appears before the confirm button is meaningful, and the status page
reaches `ready` on its own. Then `npm run build --prefix web` and commit `internal/api/ui`.

## Tier 7 — the optional live tier

Only needed if the installer or template changed, and only on a machine where a Lima VM is
acceptable:

```bash
CHECK_LIVE=1 bash hack/check.sh
```

Expected: `hack/dev-vm.sh` installs the checkout into the VM and `hack/verify.sh` passes inside it.
`verify.sh:13-14` asserts `$DATA/server` is `0700` with `0600` files and that `api-key.pending` is
gone — those are the postconditions a `--credentials-parameter` change must not break. Because the
Lima VM installs without `--credentials-parameter`, the flag-gated `awscli` install must leave this
path byte-identical.

## Definition of done for this feature

| Requirement | How it is shown above |
|---|---|
| FR-001 control plane without a cluster or marker | Tier 2 |
| FR-002, FR-015 AWS only, GCP/Azure disabled | Tier 2, `/v1/providers` |
| FR-003 standard credential chain, no pasted keys | no route accepts a key; `contracts/cluster-routes.md` |
| FR-004, FR-005 configuration and the price gate | Tier 3 |
| FR-006, FR-007 credentials not exposed | Tier 2 credentials route; Tier 4 leak check; the key never crosses a channel |
| FR-008, FR-009 phase, failure text, URL only when ready | Tier 4 phase table |
| FR-010, FR-011 node add and the removal guard | Tier 4 |
| FR-012 no SSH in the happy path | Tier 4: no step uses `SshCommand` |
| FR-013 provider-neutral lifecycle | `internal/provider`; AWS is the only implementation |
| FR-014 SDKs talk to the cluster directly | no proxy route exists |
| SC-001 … SC-010 | Tier 2, 3, 4, 5 |
