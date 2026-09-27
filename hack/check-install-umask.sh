#!/usr/bin/env bash
# The umask in install.sh must be scoped, not reset.
#
# The installer used to set `umask 077` for the block that mints the server's
# credentials and then restore it to a hardcoded `umask 022`. An install run
# under a stricter umask — a hardened image, a CI runner, a wrapper script —
# silently had it widened for the rest of the install, which is the opposite of
# what the caller asked for.
#
# This sources the function out of install.sh and checks it leaves the umask as it
# found it. It does not run the installer: that needs root and real system work,
# so the live tier is what exercises the whole thing. This covers the property,
# cheaply, on every change.
set -uo pipefail
cd "$(dirname "$0")/.."

fail=0
note() { printf '  %s\n' "$*"; }

# server_identity has to be a function before this can source it; if the shape
# ever changes back to inline `umask 077` / `umask 022`, that is the bug and this
# says so rather than quietly passing.
if ! grep -q '^server_identity() {' install.sh; then
  note "install.sh has no server_identity function; the umask is unscoped again"
  exit 1
fi
if grep -qE '^[[:space:]]*umask 022' install.sh; then
  note "install.sh still resets the umask to a hardcoded 022"
  fail=1
fi

# shellcheck source=/dev/null
eval "$(sed -n '/^server_identity() {/,/^}/p' install.sh)"

probe=$(mktemp -d)
trap 'rm -rf "$probe"' EXIT
mkdir -p "$probe/srv" "$probe/data"

# The function only writes into $SRV and reads $DATA, $LOCAL and $NEWKEY.
SRV="$probe/srv"
DATA="$probe/data"
LOCAL=1
NEWKEY=0
# shellcheck disable=SC2034
NODE_IP=127.0.0.1
# shellcheck disable=SC2034
DOMAIN=""
# shellcheck disable=SC2034
DAWNBX_ADMIN_PASSWORD=""
# shellcheck disable=SC2034
log() { :; }
# shellcheck disable=SC2034
warn() { :; }

for want in 077 027 022 077; do
  umask "$want"
  before=$(umask)
  server_identity
  after=$(umask)
  if [ "$before" = "$after" ]; then
    note "umask $before preserved across server_identity"
  else
    note "umask $before became $after inside server_identity"
    fail=1
  fi
done

# The credentials it writes must be private, which is what the umask is for.
umask 077
server_identity
for f in "$SRV/api-keys.json" "$SRV/admin.env"; do
  [ -f "$f" ] || continue
  mode=$(stat -c '%a' "$f" 2>/dev/null || stat -f '%Lp' "$f")
  case "$mode" in
    600|400) note "$f is $mode, private as intended" ;;
    *) note "$f is $mode; a credential file should be 600" ; fail=1 ;;
  esac
done

[ "$fail" -eq 0 ] && note "umask is scoped and the caller's is restored"
exit "$fail"
