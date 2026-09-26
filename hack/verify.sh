#!/usr/bin/env bash
# Checks an installed node. Run inside the VM as root: sudo bash verify.sh
set -uo pipefail
D=$(sed -n 's/.*"data_dir": *"\([^"]*\)".*/\1/p' /etc/dawnbx/install.json)
K="k3s kubectl"
bad=0
check() { if eval "$2" >/dev/null 2>&1; then echo "ok   $1"; else echo "FAIL $1"; bad=1; fi; }

check "k3s active, NRestarts=0" '[ "$(systemctl show k3s -p NRestarts --value)" = 0 ] && systemctl is-active k3s'
check "dawnbx-firewall enabled" 'systemctl is-enabled dawnbx-firewall && nft list table inet dawnbx | grep 10250'
check "data dir mounted with prjquota" 'findmnt -no OPTIONS "$D" | grep prjquota'
check "marker present" '[ -s "$D/.dawnbx-volume" ]'
check "server/ is 0700, files 0600" '[ "$(stat -c %a "$D/server")" = 700 ] && [ -z "$(find "$D/server" -type f ! -perm 600)" ]'
check "no pending key after success" '[ ! -e "$D/server/api-key.pending" ]'
check "RuntimeClass gvisor" '$K get runtimeclass gvisor'
check "NetworkPolicy" '$K -n dawnbx-sandboxes get networkpolicy sandbox-egress'
check "dawnbx-server active, NRestarts=0" '[ "$(systemctl show dawnbx -p NRestarts --value)" = 0 ] && systemctl is-active dawnbx'
check "GET /v1/version" 'curl -fs http://127.0.0.1:8080/v1/version | grep v1'
check "API rejects missing key" '[ "$(curl -s -o /dev/null -w %{http_code} http://127.0.0.1:8080/v1/sandboxes)" = 401 ]'
check "python image pre-pulled" 'k3s crictl images | grep python.*3.12-slim'

# Sandbox-shaped pod: gVisor, with the workspace under the data volume.
#
# There is deliberately no project-quota setup here. The product's per-sandbox
# cap is 5 GB and it is enforced by the reconciler measuring usage every 30 s
# and stopping the sandbox (internal/sandbox/reconcile.go, DiskLimit in
# internal/sandbox/sandbox.go), so a sandbox can overshoot briefly. Where the
# data volume was formatted with ext4 project quotas the kernel refuses the
# write instead; the mount check above proves that filesystem is present. An
# earlier version of this script set up a 50 MB quota of its own and asserted an
# 80 MB write failed - a mechanism and a size the product has never had, which
# is why it had been failing. The stop path needs 5 GB of writes to exercise
# live and is covered by the unit tests, which run in the default tier.
W=$D/sb/sb-verify1/ws
mkdir -p "$W"
$K -n dawnbx-sandboxes delete pod sb-verify1 --ignore-not-found --wait >/dev/null
$K apply -f - >/dev/null <<EOF
apiVersion: v1
kind: Pod
metadata: {name: sb-verify1, namespace: dawnbx-sandboxes}
spec:
  runtimeClassName: gvisor
  containers:
    - name: main
      image: docker.io/library/python:3.12-slim
      command: [sleep, infinity]
      volumeMounts: [{name: ws, mountPath: /workspace}]
  volumes: [{name: ws, hostPath: {path: $W}}]
EOF
$K -n dawnbx-sandboxes wait --for=condition=Ready pod/sb-verify1 --timeout=120s >/dev/null
E="$K -n dawnbx-sandboxes exec sb-verify1 --"
check "pod runs under gVisor" '$E dmesg | grep -i gvisor'
check "DNS + internet egress" '$E python -c "import urllib.request; urllib.request.urlopen(\"https://pypi.org\", timeout=10)"'
check "apiserver blocked from sandbox" '! $E python -c "import socket; socket.create_connection((\"10.43.0.1\", 443), timeout=3)"'
$K -n dawnbx-sandboxes delete pod sb-verify1 --wait=false >/dev/null
rm -rf "$D/sb/sb-verify1"
exit $bad
