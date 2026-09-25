#!/usr/bin/env bash
# Fresh Lima VM running this checkout's install.sh --local.
#   hack/dev-vm.sh [name]     create (or reuse) VM and (re)run the installer
#   hack/dev-vm.sh rm [name]  delete VM and its data disk
set -euo pipefail
cd "$(dirname "$0")/.."

if [ "${1:-}" = rm ]; then
  n=${2:-dawnbx}
  limactl delete -f "$n" || true
  limactl disk delete "$n-data" 2>/dev/null || true
  exit 0
fi
n=${1:-dawnbx}

if ! limactl list -q | grep -qx "$n"; then
  limactl disk list -q 2>/dev/null | grep -qx "$n-data" || limactl disk create "$n-data" --size 20GiB --format raw
  sed "s/name: dawnbxdata/name: $n-data/" hack/lima-dawnbx.yaml >"${TMPDIR:-/tmp}/lima-$n.yaml"
  limactl create -y --name "$n" "${TMPDIR:-/tmp}/lima-$n.yaml"
fi
limactl list -f '{{.Status}}' "$n" | grep -q Running || limactl start "$n"

(cd web && npm ci --silent && npm run build >/dev/null) # dashboard, embedded by the server
# vz runs the host's arch, so the host GOARCH is the VM's.
for b in dawnbx-server dawnbx; do
  CGO_ENABLED=0 GOOS=linux GOARCH=$(go env GOARCH) go build -o "${TMPDIR:-/tmp}/$b" ./cmd/$b
  limactl copy "${TMPDIR:-/tmp}/$b" "$n:/tmp/$b"
done
limactl copy install.sh "$n:/tmp/install.sh"
# First run: pick the unpartitioned, unmounted disk (the raw data disk).
# Re-runs find nothing and take the upgrade path, which reads the data dir from /etc/dawnbx/install.json.
dev=$(limactl shell "$n" -- sh -c 'for d in $(lsblk -dnpo NAME,TYPE | awk "\$2==\"disk\" {print \$1}"); do
  [ "$(lsblk -no NAME,MOUNTPOINTS "$d" | wc -l)" = 1 ] && [ -z "$(lsblk -dno MOUNTPOINTS,FSTYPE "$d" | tr -d " ")" ] && echo "$d"
done; true' | head -1)
limactl shell "$n" -- sudo bash /tmp/install.sh --local --yes --server-bin /tmp/dawnbx-server --cli-bin /tmp/dawnbx ${dev:+--data-device "$dev"} "${@:2}"

# Lima forwards 127.0.0.1:8080 to the Mac, so the same env file works for the host CLI and SDKs.
env=$(limactl shell "$n" -- sh -c 'cat ~/.dawnbx/env 2>/dev/null' || true)
if [ -n "$env" ]; then
  (umask 077 && mkdir -p ~/.dawnbx && printf '%s\n' "$env" >~/.dawnbx/env)
  echo "Copied to ~/.dawnbx/env on this Mac too."
fi
