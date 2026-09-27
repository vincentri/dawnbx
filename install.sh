#!/usr/bin/env bash
# dawnbx installer: k3s + gVisor + dawnbx on one Linux box.
#
#   curl -sfL https://.../install.sh | sudo bash -s -- [flags]
#
# Flags:
#   --local              laptop/VM mode: HTTP on 127.0.0.1 only, no HTTPS
#   --yes                non-interactive (also formats a blank --data-device)
#   --data-dir PATH      where sandboxes live (default /var/lib/dawnbx)
#   --data-device DEV    block device for the data dir; formatted only if blank
#   --adopt-data         allow a non-empty data dir without a dawnbx marker
#   --domain NAME        Let's Encrypt cert for NAME (DNS must point here, ports 80+443 open);
#                        without it HTTPS uses a self-signed cert (env DAWNBX_DOMAIN)
#   --release-url URL    download dawnbx binaries + checksums.txt from URL (env DAWNBX_RELEASE_URL)
#   --server-bin PATH    dawnbx-server binary to install instead (dev builds)
#   --cli-bin PATH       dawnbx CLI binary to install alongside (dev builds)
#   --new-key            replace the API key (the old one stops working)
#   --allow-join CIDR    let machines in CIDR join as workers (opens 6443 to them only)
#   --join URL TOKEN     join a server as a worker (copy the command from Settings > Nodes,
#                        or TOKEN is /var/lib/rancher/k3s/server/node-token on the server)
#   --report URL         PUT the result as CloudFormation WaitCondition JSON (EC2 user-data)
#   --bootstrap-parameter NAME
#                        read the admin password from SSM SecureString NAME; server only,
#                        and the password never reaches a log or a command line
# Env: DAWNBX_VERSION, DAWNBX_DOMAIN, DAWNBX_RELEASE_URL, GVISOR_RELEASE (default the pinned release),
#      DAWNBX_ADMIN_PASSWORD (dashboard login for user "admin"; generated if unset),
#      DAWNBX_BOOTSTRAP_PARAMETER (same as --bootstrap-parameter, for image
#      builders that cannot pass a flag; the flag wins when both are set)
set -euo pipefail

DAWNBX_VERSION=${DAWNBX_VERSION:-dev}
K3S_VERSION=v1.35.5+k3s1
# Pinned, like k3s. `latest` made a cluster's sandbox runtime change under a
# machine that was already running, so two clusters installed from the same
# release could not be compared and an upgrade could not be reasoned about.
# Overridable on purpose: an operator pinning a security fix should not have to
# edit this file.
GVISOR_RELEASE=${GVISOR_RELEASE:-20260921.0}
DEFAULT_IMAGE=docker.io/library/python:3.12-slim
MIN_FREE_GB=10

LOCAL=0 YES=0 ADOPT=0 NEWKEY=0 REPORT="" ALLOW_JOIN="" JOIN_URL="" JOIN_TOKEN="" DATA="" DEV="" DOMAIN=${DAWNBX_DOMAIN:-} SERVER_BIN="" CLI_BIN="" RELEASE_URL=${DAWNBX_RELEASE_URL:-} BOOTSTRAP_PARAM=${DAWNBX_BOOTSTRAP_PARAMETER:-}
while [ $# -gt 0 ]; do
  case $1 in
    --local) LOCAL=1 ;;
    --yes|-y) YES=1 ;;
    --adopt-data) ADOPT=1 ;;
    --new-key) NEWKEY=1 ;;
    --data-dir) DATA=$2; shift ;;
    --data-device) DEV=$2; shift ;;
    --domain) DOMAIN=$2; shift ;;
    --server-bin) SERVER_BIN=$2; shift ;;
    --release-url) RELEASE_URL=${2%/}; shift ;;
    --cli-bin) CLI_BIN=$2; shift ;;
    --report) REPORT=$2; shift ;;
    --allow-join) ALLOW_JOIN=$2; shift ;;
    --join) JOIN_URL=$2 JOIN_TOKEN=${3:-}; shift 2 ;;
    --bootstrap-parameter) BOOTSTRAP_PARAM=$2; shift ;;
    # The whole comment header, anchored to the `set -` that ends it rather than
    # to a line number. The number was 28 and set -euo pipefail was on 29, and
    # adding one flag description silently truncated --help - while README.md and
    # the installation guide both promise it lists every flag.
    -h|--help) sed -n '2,/^set -/p' "$0" | sed '$d'; exit 0 ;;
    *) echo "unknown flag: $1 (see --help)" >&2; exit 2 ;;
  esac
  shift
done

log() { printf '==> %s\n' "$*"; }
warn() { printf 'warning: %s\n' "$*" >&2; }
# CloudFormation WaitCondition format. Reason is the fail() text, which never holds secrets.
report() { # status reason
  [ -n "$REPORT" ] || return 0
  reason=$(printf %s "$2" | tr '\n\t' '  ' | sed 's/\\/\\\\/g; s/"/\\"/g' | cut -c1-900)
  printf '{"Status":"%s","Reason":"%s","UniqueId":"install","Data":"%s"}' "$1" "$reason" "$1" |
    curl -fsS --max-time 20 --retry 3 -X PUT -H 'Content-Type:' --data-binary @- "$REPORT" >/dev/null ||
    warn "could not send result to --report URL"
}
fail() { # problem cause fix
  printf '\nerror: %s\n  cause: %s\n  fix:   %s\n' "$1" "$2" "$3" >&2
  report FAILURE "$1: $2. Fix: $3"
  exit 1
}
# set -e exits skip fail(), so a failure in a command carrying no `|| fail`
# guard used to report only a line number: the CloudFormation WaitCondition
# carried "stopped at line 445" where fail() would have carried what went wrong
# and what to do about it. The trap goes through fail() now, so every failure
# path - guarded or not - reaches the stack with a cause and a fix.
trap 'fail "install failed at line $LINENO" "the command above did not succeed, so the install is incomplete" "read that line, fix it, and re-run - or read /var/log/cloud-init-output.log for the full log"' ERR

# ---------------------------------------------------------------- preflight
# Upgrades without --server-bin or --release-url reuse the installed binary.
if [ -n "$JOIN_URL" ]; then : # workers run only k3s-agent + gVisor
elif [ -n "$SERVER_BIN" ]; then
  [ -x "$SERVER_BIN" ] || fail "--server-bin $SERVER_BIN is not an executable file" "wrong path" "build it: GOOS=linux CGO_ENABLED=0 go build -o dawnbx-server ./cmd/dawnbx-server"
elif [ -n "$RELEASE_URL" ]; then
  case $RELEASE_URL in https://*) ;; *) fail "bad --release-url $RELEASE_URL" "binaries must come over https" "pass the https URL of a release" ;; esac
elif [ ! -x /usr/local/bin/dawnbx-server ]; then
  fail "no dawnbx-server binary" "no --release-url or --server-bin given" "pass --release-url (a dawnbx release) or --server-bin PATH (hack/dev-vm.sh does this)"
fi
# Everything here only reads; the box is untouched until preflight passes.

[ "$(id -u)" = 0 ] || fail "must run as root" "installer writes /etc, /usr/local/bin and systemd units" "re-run with sudo"

case $(uname -m) in
  x86_64) GV_ARCH=x86_64 GO_ARCH=amd64 ;;
  aarch64|arm64) GV_ARCH=aarch64 GO_ARCH=arm64 ;;
  *) fail "unsupported CPU $(uname -m)" "gVisor ships amd64 and arm64 only" "use an amd64 or arm64 machine" ;;
esac

[ -f /sys/fs/cgroup/cgroup.controllers ] ||
  fail "cgroup v2 not enabled" "k3s + gVisor need the unified cgroup hierarchy" "use Ubuntu 22.04+ / Debian 12+, or boot with systemd.unified_cgroup_hierarchy=1"

# Node-to-node traffic is WireGuard-encrypted (flannel wireguard-native), so
# workers can join over the public internet.
[ -d /sys/module/wireguard ] || modprobe wireguard 2>/dev/null ||
  fail "kernel has no WireGuard" "node traffic is encrypted with WireGuard" "use Linux 5.6+ (Ubuntu 22.04+ / Debian 12+)"

UPGRADE=0
if [ -f /etc/dawnbx/install.json ]; then
  UPGRADE=1
  prev=$(sed -n 's/.*"data_dir": *"\([^"]*\)".*/\1/p' /etc/dawnbx/install.json)
  if [ -n "$DATA" ] && [ -n "$prev" ] && [ "$DATA" != "$prev" ]; then
    fail "--data-dir $DATA differs from installed $prev" "moving the data dir in place is not supported" "re-run without --data-dir, or reinstall on a fresh machine"
  fi
  DATA=${DATA:-$prev}
  # Re-runs keep the mode and domain they were installed with.
  grep -q '"local": *true' /etc/dawnbx/install.json && LOCAL=1
  DOMAIN=${DOMAIN:-$(sed -n 's/.*"domain": *"\([^"]*\)".*/\1/p' /etc/dawnbx/install.json)}
  ALLOW_JOIN=${ALLOW_JOIN:-$(sed -n 's/.*"allow_join": *"\([^"]*\)".*/\1/p' /etc/dawnbx/install.json)}
  prevjoin=$(sed -n 's/.*"join": *"\([^"]*\)".*/\1/p' /etc/dawnbx/install.json)
  if [ -n "$prevjoin" ] && [ -z "$JOIN_URL" ]; then
    fail "this machine is a worker of $prevjoin" "re-running as a server would start a second cluster" "re-run with --join $prevjoin TOKEN"
  fi
elif command -v k3s >/dev/null || [ -d /etc/rancher/k3s ]; then
  fail "k3s already installed and not by dawnbx" "dawnbx never adopts a foreign cluster" "use a fresh machine (existing clusters = Helm chart, phase 2)"
fi
DATA=${DATA:-/var/lib/dawnbx}
if [ "$LOCAL" = 1 ] && [ -n "$DOMAIN" ]; then
  fail "--domain needs a public install" "--local serves plain HTTP on 127.0.0.1 only" "drop --local (or --domain)"
fi
case $DOMAIN in *[!a-zA-Z0-9.-]*) fail "bad --domain $DOMAIN" "only letters, digits, dots and dashes" "pass a DNS name like sandbox.example.com" ;; esac
if [ -n "$JOIN_URL" ]; then
  case $JOIN_URL in https://*:6443) ;; *) fail "bad --join URL $JOIN_URL" "it is the server's k3s address" "pass https://SERVER_IP:6443" ;; esac
  case $JOIN_TOKEN in K10*::server:*) ;; *) fail "--join needs the server's node token" "got something that is not a k3s token" "on the server: sudo cat /var/lib/rancher/k3s/server/node-token" ;; esac
  [ -z "$DOMAIN$ALLOW_JOIN" ] || fail "--join takes no --domain or --allow-join" "those configure a server" "drop them"
  SERVER_IP=${JOIN_URL#https://}; SERVER_IP=${SERVER_IP%:6443}
  case $SERVER_IP in *[!0-9.]*) fail "--join URL must use the server's IPv4 address" "got $SERVER_IP" "pass https://SERVER_IP:6443" ;; esac
fi
case $ALLOW_JOIN in ""|*.*.*.*/*) ;; *) fail "bad --allow-join $ALLOW_JOIN" "expected an IPv4 CIDR" "pass something like 10.0.0.0/16" ;; esac
case $ALLOW_JOIN in *[!0-9./]*) fail "bad --allow-join $ALLOW_JOIN" "expected an IPv4 CIDR" "pass something like 10.0.0.0/16" ;; esac

if [ -n "$BOOTSTRAP_PARAM" ]; then
  # A worker has no dashboard, so it can use neither the password nor the role
  # that reads it: taking the flag here would only put a CLI and a credential
  # path on a box that needs neither.
  [ -z "$JOIN_URL" ] || fail "--bootstrap-parameter is a server flag" "a worker has no dashboard to set the admin password on" "drop it, or drop --join"
  case $BOOTSTRAP_PARAM in
    *[!a-zA-Z0-9_./-]*) fail "bad --bootstrap-parameter $BOOTSTRAP_PARAM" "an SSM parameter name is letters, digits, dot, dash, underscore and slash" "pass the name, e.g. /dawnbx/admin" ;;
  esac
fi

case $DATA in /*) ;; *) fail "--data-dir must be absolute" "got $DATA" "pass a full path like /var/lib/dawnbx" ;; esac

if [ "$UPGRADE" = 0 ]; then
  for p in 80 443 6443; do
    if ss -Htln "sport = :$p" | grep -q .; then
      fail "port $p already in use" "$(ss -Htlnp "sport = :$p" | awk '{print $NF}' | head -1)" "stop that service or use a fresh machine"
    fi
  done
fi

free_gb=$(df -Pk /var/lib | awk 'NR==2 {print int($4/1048576)}')
[ "$free_gb" -ge "$MIN_FREE_GB" ] ||
  fail "only ${free_gb} GB free on /var/lib" "images and k3s state need at least ${MIN_FREE_GB} GB" "grow the disk or free space"

if [ -n "$DEV" ]; then
  [ -b "$DEV" ] || fail "$DEV is not a block device" "--data-device must name an attached disk" "check lsblk"
  if ! blkid "$DEV" >/dev/null 2>&1 && [ "$YES" = 0 ]; then
    [ -t 0 ] || exec </dev/tty
    read -r -p "$DEV is blank. Format it as ext4 for dawnbx data? [y/N] " ans
    [ "$ans" = y ] || [ "$ans" = Y ] || fail "format declined" "blank $DEV needs a filesystem" "re-run with --yes, or drop --data-device"
  fi
fi

command -v apt-get >/dev/null || command -v nft >/dev/null ||
  fail "nftables missing" "k3s ports are firewalled with nft" "install nftables with your package manager"

# Check the server and token now, not after installing everything: the token
# is K10<sha256 of the server CA>::server:<password>.
if [ -n "$JOIN_URL" ]; then
  ca=$(mktemp)
  curl -fsk --max-time 10 "$JOIN_URL/cacerts" -o "$ca" ||
    fail "can't reach $JOIN_URL" "the server is down, or its firewall drops this machine" "check the server is up and installed with --allow-join covering this machine's IP"
  h=${JOIN_TOKEN#K10}; h=${h%%::*}
  [ "$(sha256sum <"$ca" | cut -d' ' -f1)" = "$h" ] ||
    fail "$JOIN_URL is not the server this token belongs to" "its certificate doesn't match the token" "copy the join command again from Settings > Nodes"
  code=$(curl -s -o /dev/null -w '%{http_code}' --max-time 10 --cacert "$ca" -u "node:${JOIN_TOKEN#*::server:}" "$JOIN_URL/v1-k3s/readyz")
  rm -f "$ca"
  [ "$code" = 200 ] || fail "the server rejected the join token (HTTP $code)" "the token is wrong or was rotated" "copy the join command again from Settings > Nodes"
fi

log "preflight ok ($(uname -m), data dir $DATA, $([ "$UPGRADE" = 1 ] && echo upgrade || echo fresh install))"

# ---------------------------------------------------------------- packages
if command -v apt-get >/dev/null; then
  log "installing packages"
  export DEBIAN_FRONTEND=noninteractive NEEDRESTART_SUSPEND=1
  # The bootstrap parameter is read with a stdlib SigV4 signer below, so nothing
  # here is needed for it. Ubuntu 24.04 has no awscli package at all - not in
  # main, not in universe - so asking for it made every install that carried a
  # bootstrap parameter fail at exactly this line.
  PKGS=(nftables quota curl openssl bzip2)
  apt-get -qq update
  apt-get -qq install -y "${PKGS[@]}" >/dev/null
  # Stock cloud kernels ship quota_v2 in linux-modules-extra; without it an
  # ext4 volume with the quota feature fails to mount (ESRCH).
  if ! modinfo quota_v2 >/dev/null 2>&1; then
    apt-get -qq install -y "linux-modules-extra-$(uname -r)" >/dev/null 2>&1 || true
  fi
fi
QUOTA_MOD=0
if modprobe quota_v2 2>/dev/null; then
  QUOTA_MOD=1
  echo quota_v2 >/etc/modules-load.d/dawnbx.conf
fi

# ---------------------------------------------------------------- data volume
mkdir -p "$DATA"
if [ -n "$DEV" ]; then
  if ! blkid "$DEV" >/dev/null 2>&1; then
    [ "$QUOTA_MOD" = 1 ] || warn "quota_v2 module unavailable; formatting without project quota"
    log "formatting $DEV (ext4$([ "$QUOTA_MOD" = 1 ] && echo ', project quota'))"
    if [ "$QUOTA_MOD" = 1 ]; then mkfs.ext4 -q -O quota,project "$DEV"; else mkfs.ext4 -q "$DEV"; fi
  fi
  opts=defaults,nofail
  if [ "$QUOTA_MOD" = 1 ] && tune2fs -l "$DEV" 2>/dev/null | grep -q 'features:.*project'; then opts=$opts,prjquota; fi
  uuid=$(blkid -s UUID -o value "$DEV")
  grep -q "UUID=$uuid" /etc/fstab || echo "UUID=$uuid $DATA ext4 $opts 0 2" >>/etc/fstab
  systemctl daemon-reload
  mountpoint -q "$DATA" || mount "$DATA"
fi

if mountpoint -q "$DATA"; then :
elif [ "$LOCAL" = 0 ]; then
  warn "$DATA is on the root disk; sandboxes won't survive losing this machine. Use --data-device with a separate volume."
fi

if [ -f "$DATA/.dawnbx-volume" ]; then
  log "data volume marker found, reusing $DATA"
elif [ -n "$(ls -A "$DATA" | grep -vx lost+found || true)" ] && [ "$ADOPT" = 0 ]; then
  fail "$DATA is not empty and has no dawnbx marker" "it may be another app's data, or the real volume is not mounted over it" "mount the right volume, or pass --adopt-data to use this dir as is"
else
  openssl rand -hex 16 >"$DATA/.dawnbx-volume"
fi
mkdir -p "$DATA/sb"

if ! findmnt -no OPTIONS --target "$DATA" | grep -q prjquota; then
  warn "no ext4 project quota on $DATA; per-sandbox disk cap falls back to a 30 s du check (dawnbx doctor shows how to fix)"
fi

NODE_IP=$(ip -4 route get 1.1.1.1 2>/dev/null | awk '{for(i=1;i<NF;i++) if($i=="src") print $(i+1)}')
# ssm_get_parameter prints the decrypted value of one SecureString.
#
# It prefers the aws CLI where a distro ships one, and otherwise signs the
# GetParameter request itself with SigV4 using the instance role. The fallback is
# not a convenience: Ubuntu 24.04 - the image install.sh's own default AMI comes
# from - has no awscli package, so a CLI-only reader cannot complete an install
# there. Everything the signer needs (hmac, hashlib, json, urllib) is in the
# Python standard library that cloud-init already requires, so this adds no
# package, no repository and no key to the box.
ssm_get_parameter() {
  [ -n "$1" ] || return 1
  if command -v aws >/dev/null 2>&1; then
    aws ssm get-parameter --name "$1" --with-decryption --query Parameter.Value --output text 2>/dev/null && return 0
  fi
  command -v python3 >/dev/null 2>&1 || return 1
  python3 - "$1" <<'SSM_SIGNER'
import hashlib, hmac, json, sys, urllib.parse, urllib.request
from datetime import datetime, timezone

META = "http://169.254.169.254/latest"


def _req(url, headers=None, method="GET", data=None):
    return urllib.request.urlopen(
        urllib.request.Request(url, headers=headers or {}, method=method, data=data), timeout=10
    ).read().decode()


try:
    name = sys.argv[1]
    # IMDSv2: a token first, then everything else carries it.
    token = _req(META + "/api/token", {"X-aws-ec2-metadata-token-ttl-seconds": "300"}, "PUT")
    hdr = {"X-aws-ec2-metadata-token": token}
    # The credentials live under meta-data, not off /latest directly: the
    # shorter path is a 404 that looks exactly like "no role attached".
    role = _req(META + "/meta-data/iam/security-credentials/", hdr).strip().splitlines()[0]
    cred = json.loads(_req(META + "/meta-data/iam/security-credentials/" + role, hdr))

    # The credentials carry no region: it is instance metadata, not IAM data.
    region, service = _req(META + "/meta-data/placement/region", hdr), "ssm"
    host = "ssm." + region + ".amazonaws.com"
    now = datetime.now(timezone.utc)
    amz, day = now.strftime("%Y%m%dT%H%M%SZ"), now.strftime("%Y%m%d")
    # SSM answers x-amz-target with a JSON body. The older Query protocol (a
    # form-encoded Action=GetParameter) is what the shape of the AWS Query API
    # suggests, and SSM rejects it with a bare ValidationError.
    ctype, target = "application/x-amz-json-1.1", "AmazonSSM.GetParameter"
    body = json.dumps({"Name": name, "WithDecryption": True})
    payload = hashlib.sha256(body.encode()).hexdigest()
    # Header names are lowercased and sorted, and the set signed must match the
    # set sent. The session token is in it because instance-role credentials
    # always carry one; without it in the signature the call is 403.
    signed = "content-type;host;x-amz-date;x-amz-security-token;x-amz-target"
    canon = "\n".join([
        "POST", "/", "",
        "content-type:%s\nhost:%s\nx-amz-date:%s\nx-amz-security-token:%s\nx-amz-target:%s\n"
        % (ctype, host, amz, cred["Token"], target),
        signed, payload,
    ])
    scope = "%s/%s/%s/aws4_request" % (day, region, service)
    to_sign = "\n".join([
        "AWS4-HMAC-SHA256", amz, scope, hashlib.sha256(canon.encode()).hexdigest(),
    ])
    k = ("AWS4" + cred["SecretAccessKey"]).encode()
    for part in (day, region, service, "aws4_request"):
        k = hmac.new(k, part.encode(), hashlib.sha256).digest()
    sig = hmac.new(k, to_sign.encode(), hashlib.sha256).hexdigest()
    auth = ("AWS4-HMAC-SHA256 Credential=%s/%s, SignedHeaders=%s, Signature=%s"
            % (cred["AccessKeyId"], scope, signed, sig))
    out = _req("https://%s/" % host, {
        "Content-Type": ctype, "X-Amz-Target": target,
        "X-Amz-Date": amz, "X-Amz-Security-Token": cred["Token"], "Authorization": auth,
    }, "POST", body.encode())
    sys.stdout.write(json.loads(out)["Parameter"]["Value"])
except Exception:
    # The caller's fail() turns this into an operator-readable reason; an SSM
    # error body can echo request context, so nothing from it is printed here.
    sys.exit(1)
SSM_SIGNER
}

if [ -n "$BOOTSTRAP_PARAM" ]; then
  # The password is assigned to a variable, never passed to another command, so
  # it appears in no argument list (ps) and in no log line; and the CLI's stderr
  # is dropped because an SSM error echoes back request context. The branch below
  # writes it to $SRV/admin.env, which is where the server reads it from.
  DAWNBX_ADMIN_PASSWORD=$(ssm_get_parameter "$BOOTSTRAP_PARAM") || DAWNBX_ADMIN_PASSWORD=""
  [ -n "$DAWNBX_ADMIN_PASSWORD" ] ||
    fail "could not read SSM parameter $BOOTSTRAP_PARAM" "the instance role may not carry the parameter, the name may be wrong, or the value is empty" "check the parameter and the stack's IAM role, then re-run"
  export DAWNBX_ADMIN_PASSWORD
fi

# wait_probe runs a command until it succeeds, then runs it once more so the
# caller can trust a zero return. The four waits this installer had were the same
# shape with the timeout and the re-assertion written out in each, and they could
# drift apart; the reason text stays at the call site, where the operator reads it.
wait_probe() { # probe_cmd max_tries gap_seconds
  local i
  for i in $(seq 1 "$2"); do
    if eval "$1" >/dev/null 2>&1; then
      eval "$1" >/dev/null 2>&1
      return 0
    fi
    sleep "$3"
  done
  return 1
}

# The two steps a freshly installed node takes before it can run a sandbox, and
# that both the worker and the server branch need. They sat twenty lines apart and
# were kept in step by hand, differing only in one word of the warning.
drop_legacy_flannel() {
  # left over from the VXLAN backend before wireguard-native
  ip link del flannel.1 2>/dev/null || true
}

prepull_image() {
  # $1 is where the image will be pulled: here, or the next sandbox create.
  log "pre-pulling $DEFAULT_IMAGE"
  k3s crictl pull "$DEFAULT_IMAGE" >/dev/null || warn "pre-pull failed; first sandbox create $1 will be slower"
}

# Everything that mints or writes a credential lives in server_identity, so the
# strict umask is scoped to it. The previous version set umask 077 and then
# restored it to a hardcoded 022, which silently widened a stricter caller's
# umask for the rest of the install - the opposite of what a hardened image or a
# CI runner asked for. local keeps API_KEY, ADMIN_PASSWORD and the rest out of
# the global namespace as well.
server_identity() {
  local old_umask
  old_umask=$(umask)
  umask 077
  local API_KEY=""
  # The plaintext key waits in api-key.pending until it has been printed, so a
  # run that fails halfway still shows it on the next run.
  if [ -f "$SRV/api-key.pending" ]; then
    API_KEY=$(cat "$SRV/api-key.pending")
  elif [ -f "$SRV/api-keys.json" ] && [ "$NEWKEY" = 0 ]; then
    log "restored server identity from $SRV"
  else
    API_KEY="dawnbx_$(openssl rand -hex 24)"
    printf '{"v":1,"keys":[{"sha256":"%s","created":"%s"}]}\n' \
      "$(printf %s "$API_KEY" | sha256sum | cut -d' ' -f1)" "$(date -u +%FT%TZ)" >"$SRV/api-keys.json"
    printf %s "$API_KEY" >"$SRV/api-key.pending"
  fi

  # Dashboard admin login. The server reads admin.env verbatim (any characters are
  # fine) and keeps only a bcrypt hash in its DB. Changing the value here and
  # restarting dawnbx resets the password; a change made in the dashboard sticks otherwise.
  local ADMIN_PASSWORD=""
  if [ -n "${DAWNBX_ADMIN_PASSWORD:-}" ]; then
    ADMIN_PASSWORD=$DAWNBX_ADMIN_PASSWORD
    printf 'DAWNBX_ADMIN_PASSWORD=%s\n' "$ADMIN_PASSWORD" >"$SRV/admin.env"
  elif [ -f "$SRV/admin.pending" ]; then
    ADMIN_PASSWORD=$(cat "$SRV/admin.pending")
  elif [ ! -f "$SRV/admin.env" ]; then
    ADMIN_PASSWORD=$(openssl rand -base64 18 | tr -d '/+=')
    printf 'DAWNBX_ADMIN_PASSWORD=%s\n' "$ADMIN_PASSWORD" >"$SRV/admin.env"
    printf %s "$ADMIN_PASSWORD" >"$SRV/admin.pending"
  fi

  if [ "$LOCAL" = 0 ]; then
    mkdir -p "$SRV/tls"
    if [ ! -f "$SRV/tls/cert.pem" ]; then
      san="IP:$NODE_IP,DNS:$NODE_IP.sslip.io"
      [ -n "$DOMAIN" ] && san="$san,DNS:$DOMAIN"
      openssl req -x509 -newkey ec -pkeyopt ec_paramgen_curve:P-256 -nodes -days 3650 \
        -subj /CN=dawnbx -addext "subjectAltName=$san" \
        -keyout "$SRV/tls/key.pem" -out "$SRV/tls/cert.pem" 2>/dev/null
    fi
  fi
  umask "$old_umask"
}

# A worker that joins an existing server must not mint an identity of its own,
# which is what the JOIN_URL guard below is for; the function is defined outside
# it so it always exists, and called inside it.

if [ -z "$JOIN_URL" ]; then
# ---------------------------------------------------------------- server identity
# Lives on the data volume so a rebuilt machine keeps the same API key and TLS
# cert. Never mounted into sandboxes (they only get sb/<id>/ws).
SRV=$DATA/server
install -d -m 700 "$SRV"
server_identity
fi

mkdir -p /etc/dawnbx
printf '{"version":"%s","installed":"%s","data_dir":"%s","local":%s,"domain":"%s","allow_join":"%s","join":"%s"}\n' \
  "$DAWNBX_VERSION" "$(date -u +%FT%TZ)" "$DATA" "$([ "$LOCAL" = 1 ] && echo true || echo false)" "$DOMAIN" "$ALLOW_JOIN" "$JOIN_URL" >/etc/dawnbx/install.json

# ---------------------------------------------------------------- firewall
# apiserver (6443) and kubelet (10250) answer only on loopback, the pod network
# and peers: machines in --allow-join on a server, the server on a worker.
PEERS=${ALLOW_JOIN:-${SERVER_IP:+$SERVER_IP/32}}
PEER_RULE=""
[ -n "$PEERS" ] && PEER_RULE="ip saddr $PEERS tcp dport { 6443, 10250 } accept"
cat >/etc/dawnbx/firewall.nft <<NFT
table inet dawnbx
delete table inet dawnbx
table inet dawnbx {
  chain input {
    type filter hook input priority -10; policy accept;
    $PEER_RULE
    iifname != { "lo", "cni0", "flannel.1", "flannel-wg" } tcp dport { 6443, 10250 } drop
  }
}
NFT
cat >/etc/systemd/system/dawnbx-firewall.service <<'UNIT'
[Unit]
Description=dawnbx: block k3s apiserver/kubelet on non-local interfaces
Before=k3s.service k3s-agent.service
[Service]
Type=oneshot
RemainAfterExit=yes
ExecStart=/usr/sbin/nft -f /etc/dawnbx/firewall.nft
[Install]
WantedBy=multi-user.target
UNIT
systemctl daemon-reload
systemctl enable dawnbx-firewall.service >/dev/null 2>&1
systemctl restart dawnbx-firewall.service ||
  fail "firewall rules failed to load" "$(journalctl -u dawnbx-firewall -n 3 --no-pager | tail -1)" "check nft is installed and the kernel has nf_tables"
log "firewall: 6443/10250 closed on non-local interfaces${PEERS:+ except $PEERS}"

# ---------------------------------------------------------------- gVisor
log "installing gVisor ($GVISOR_RELEASE)"
tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT
url=https://storage.googleapis.com/gvisor/releases/release/$GVISOR_RELEASE/$GV_ARCH/gvisor.tar.bz2
curl -fsSL -o "$tmp/gvisor.tar.bz2" "$url" && curl -fsSL -o "$tmp/gvisor.tar.bz2.sha512" "$url.sha512" ||
  fail "gVisor download failed" "$url unreachable" "check outbound HTTPS, or set GVISOR_RELEASE to a release date like 20260901"
(cd "$tmp" && sha512sum -c --quiet gvisor.tar.bz2.sha512) ||
  fail "gVisor checksum mismatch" "download corrupted or tampered" "re-run the installer"
tar -xjf "$tmp/gvisor.tar.bz2" -C /usr/local/bin runsc containerd-shim-runsc-v1 gvisor-bin
chmod 755 /usr/local/bin/runsc /usr/local/bin/containerd-shim-runsc-v1

# ---------------------------------------------------------------- k3s
# Config, containerd template and manifests are all written before k3s starts,
# so it comes up once with gVisor instead of needing a restart (the restart
# is what tripped the cloud-controller crash loop in the C2 probe).
mkdir -p /etc/rancher/k3s /var/lib/rancher/k3s/agent/etc/containerd
K3S_CONF_OLD=$(cat /etc/rancher/k3s/config.yaml 2>/dev/null || true)
if [ -z "$JOIN_URL" ]; then
  mkdir -p /var/lib/rancher/k3s/server/manifests
  cat >/etc/rancher/k3s/config.yaml <<'EOF'
disable-cloud-controller: true
write-kubeconfig-mode: "0600"
flannel-backend: wireguard-native
# k3s deploys traefik as a packaged ingress controller on a LoadBalancer
# service, and its ServiceLB claims host ports 80 and 443 with iptables rather
# than a listening socket. dawnbx creates no Ingress at all and serves its own
# HTTPS on 443, so that controller takes the port dawnbx needs and answers
# every request with traefik's default certificate and a 404 - while `ss` still
# shows dawnbx-server listening, because the socket is there and the traffic
# simply never reaches it. dawnbx owns 80/443; k3s does not need them.
disable:
  - traefik
EOF
fi
cat >/var/lib/rancher/k3s/agent/etc/containerd/config-v3.toml.tmpl <<'EOF'
{{ template "base" . }}

[plugins.'io.containerd.cri.v1.runtime'.containerd.runtimes.runsc]
  runtime_type = "io.containerd.runsc.v1"
EOF
if [ -z "$JOIN_URL" ]; then
cat >/var/lib/rancher/k3s/server/manifests/dawnbx.yaml <<EOF
apiVersion: node.k8s.io/v1
kind: RuntimeClass
metadata: {name: gvisor}
handler: runsc
---
apiVersion: v1
kind: Namespace
metadata: {name: dawnbx-sandboxes}
---
# Policies add up, so: deny everything for every sandbox, then allow egress
# for all except network=none.
apiVersion: networking.k8s.io/v1
kind: NetworkPolicy
metadata: {name: sandbox-deny-all, namespace: dawnbx-sandboxes}
spec:
  podSelector: {}
  policyTypes: [Egress, Ingress]
---
# DNS + internet, nothing inside the cluster, the node or cloud metadata.
apiVersion: networking.k8s.io/v1
kind: NetworkPolicy
metadata: {name: sandbox-egress, namespace: dawnbx-sandboxes}
spec:
  podSelector:
    matchExpressions: [{key: dawnbx/network, operator: NotIn, values: ["none"]}]
  policyTypes: [Egress]
  egress:
    - to:
        - namespaceSelector: {matchLabels: {kubernetes.io/metadata.name: kube-system}}
          podSelector: {matchLabels: {k8s-app: kube-dns}}
      ports: [{protocol: UDP, port: 53}, {protocol: TCP, port: 53}]
    - to:
        - ipBlock:
            cidr: 0.0.0.0/0
            except: [10.42.0.0/16, 10.43.0.0/16, 169.254.169.254/32${NODE_IP:+, $NODE_IP/32}${ALLOW_JOIN:+, $ALLOW_JOIN}]
EOF
fi

if [ -n "$JOIN_URL" ]; then
  log "joining $JOIN_URL as a worker (k3s $K3S_VERSION)"
  curl -sfL https://get.k3s.io | INSTALL_K3S_VERSION=$K3S_VERSION K3S_URL=$JOIN_URL K3S_TOKEN=$JOIN_TOKEN sh -s - >"$tmp/k3s.log" 2>&1 ||
    fail "k3s agent install failed" "$(tail -1 "$tmp/k3s.log")" "see journalctl -u k3s-agent"
  # The kubelet's local health port answers once the node registered with the server.
  # A node name already on the server is reported immediately rather than after
  # the full wait, so the loop is bounded but the loop itself is the helper.
  if journalctl -u k3s-agent --no-pager -n 50 2>/dev/null | grep -q 'password rejected'; then
    fail "the server already has a node named $(hostname)" "k3s refuses a second machine with the same name" "on the server: sudo k3s kubectl delete node $(hostname), then re-run"
  fi
  wait_probe "curl -fs http://127.0.0.1:10248/healthz" 90 2 ||
    fail "worker did not join within 3 min" "$(journalctl -u k3s-agent -n 1 --no-pager -o cat)" "check the token, and that the server was installed with --allow-join covering $NODE_IP"
  drop_legacy_flannel
  prepull_image here
  echo
  echo "Joined $JOIN_URL as node $(hostname). See it on the server's Nodes page."
  report SUCCESS "node $(hostname) joined $JOIN_URL"
  exit 0
fi

log "installing k3s $K3S_VERSION"
curl -sfL https://get.k3s.io | INSTALL_K3S_VERSION=$K3S_VERSION sh -s - >"$tmp/k3s.log" 2>&1 ||
  fail "k3s install failed" "$(tail -1 "$tmp/k3s.log")" "see journalctl -u k3s"
# The k3s installer restarts only when its unit changed, not on a new config.yaml.
if [ -n "$K3S_CONF_OLD" ] && [ "$K3S_CONF_OLD" != "$(cat /etc/rancher/k3s/config.yaml)" ]; then
  log "k3s config changed, restarting k3s (sandboxes keep running)"
  systemctl restart k3s
fi
K="k3s kubectl"
wait_probe "$K get node | grep -q ' Ready' && $K get runtimeclass gvisor" 90 2 ||
  fail "k3s node not Ready after 3 min" "$(journalctl -u k3s -n 1 --no-pager)" "journalctl -u k3s"
drop_legacy_flannel
prepull_image "will be slower"

# ---------------------------------------------------------------- dawnbx-server
if [ -z "$SERVER_BIN" ] && [ -n "$RELEASE_URL" ]; then
  log "downloading dawnbx from $RELEASE_URL"
  curl -fsSL --retry 3 -o "$tmp/checksums.txt" "$RELEASE_URL/checksums.txt" ||
    fail "could not download $RELEASE_URL/checksums.txt" "wrong URL or no network" "check the release URL opens in a browser"
  for b in dawnbx-server dawnbx; do
    f=$b-linux-$GO_ARCH
    curl -fsSL --retry 3 -o "$tmp/$f" "$RELEASE_URL/$f" ||
      fail "could not download $RELEASE_URL/$f" "the release has no $GO_ARCH build, or no network" "check the release assets"
    (cd "$tmp" && grep "  $f\$" checksums.txt | sha256sum -c --status) ||
      fail "$f does not match checksums.txt" "corrupted or tampered download" "re-run; if it keeps failing, don't use this release"
    chmod 755 "$tmp/$f"
  done
  SERVER_BIN=$tmp/dawnbx-server-linux-$GO_ARCH CLI_BIN=$tmp/dawnbx-linux-$GO_ARCH
fi
if [ -n "$SERVER_BIN" ]; then
  install -m 755 "$SERVER_BIN" /usr/local/bin/dawnbx-server.new
  mv -f /usr/local/bin/dawnbx-server.new /usr/local/bin/dawnbx-server
fi
if [ -n "$CLI_BIN" ]; then
  install -m 755 "$CLI_BIN" /usr/local/bin/dawnbx.new
  mv -f /usr/local/bin/dawnbx.new /usr/local/bin/dawnbx
fi
cat >/etc/systemd/system/dawnbx.service <<UNIT
[Unit]
Description=dawnbx sandbox API
After=k3s.service
Wants=k3s.service
RequiresMountsFor=$DATA
[Service]
# The server writes its SQLite database into the 0700 server/ directory, and
# systemd's default umask is 022, so the database files came out 0644 - readable
# by any account on the host, and it holds password and API-key hashes. 0077
# keeps everything the process creates inside a directory that is already 0700.
UMask=0077
ExecStart=/usr/local/bin/dawnbx-server --data-dir $DATA --listen 127.0.0.1:8080$([ "$LOCAL" = 0 ] && echo " --https-listen :443")${DOMAIN:+ --domain $DOMAIN}
Restart=always
RestartSec=2
[Install]
WantedBy=multi-user.target
UNIT
systemctl daemon-reload
systemctl enable dawnbx.service >/dev/null 2>&1
systemctl restart dawnbx.service
wait_probe "curl -fs http://127.0.0.1:8080/v1/version" 30 1 ||
  fail "dawnbx-server did not start" "$(journalctl -u dawnbx -n 1 --no-pager -o cat)" "journalctl -u dawnbx"
log "dawnbx-server up on 127.0.0.1:8080"

# ---------------------------------------------------------------- done
# On-box clients use loopback HTTP (no cert to trust); remote ones use HTTPS.
# Lima forwards guest loopback ports, so the loopback URL works from the Mac too.
URL=http://127.0.0.1:8080
if [ "$LOCAL" = 1 ]; then PUBLIC=$URL
elif [ -n "$DOMAIN" ]; then
  PUBLIC=https://$DOMAIN
  # Two claims, not one. curl succeeding proves something on 443 answered with
  # a certificate curl was willing to trust; it does not prove that answer came
  # from dawnbx. A different proxy can answer the same way and still be the
  # wrong service, so the certificate is read back and has to name this domain.
  if curl -fsS --max-time 60 "$PUBLIC/v1/version" >/dev/null 2>&1 &&
    echo | openssl s_client -connect "$DOMAIN:443" -servername "$DOMAIN" 2>/dev/null |
      openssl x509 -noout -ext subjectAltName 2>/dev/null | grep -q "DNS:$DOMAIN"; then
    log "Let's Encrypt cert ready for $DOMAIN"
  else
    warn "could not reach $PUBLIC with a valid cert yet. Check: DNS A record for $DOMAIN points at this server's public IP; ports 80 and 443 are open in the cloud firewall / security group. The server retries on the next HTTPS request; errors: journalctl -u dawnbx"
    warn "if the check above passed but this one did not, something other than dawnbx-server is answering on 443 for $DOMAIN"
  fi
  PUBLIC=https://${NODE_IP:-<this-server-ip>}
fi

home=$(getent passwd "${SUDO_USER:-root}" | cut -d: -f6)
# Plaintext copy for the installing user only (like ~/.kube/config), so a
# re-run can print it again. The data volume keeps only the hash.
KEYFILE=$home/.dawnbx/env
if [ -n "$API_KEY" ]; then
  install -d -m 700 "$home/.dawnbx"
  printf 'export DAWNBX_URL=%s\nexport DAWNBX_API_KEY=%s\n' "$URL" "$API_KEY" >"$KEYFILE"
  chown -R "${SUDO_USER:-root}" "$home/.dawnbx" 2>/dev/null || true
elif [ -f "$KEYFILE" ]; then
  API_KEY=$(sed -n 's/^export DAWNBX_API_KEY=//p' "$KEYFILE")
fi
rm -f "$home/dawnbx-quickstart.mjs" # older installs wrote an npm quickstart; the SDKs are not published yet
echo
# Secrets go to the screen only. Without a terminal (EC2 user-data, pipes) stdout
# lands in logs like cloud-init-output.log, which the EC2 console serves to anyone
# with ec2:GetConsoleOutput, so print where they live instead.
if [ ! -t 1 ]; then
  API_KEY_SHOWN="(in $KEYFILE)" ADMIN_SHOWN=""
else
  API_KEY_SHOWN=$API_KEY ADMIN_SHOWN=$ADMIN_PASSWORD
fi
echo "dawnbx $DAWNBX_VERSION installed."
echo
echo "  export DAWNBX_URL=$PUBLIC"
if [ -n "$API_KEY" ]; then echo "  export DAWNBX_API_KEY=$API_KEY_SHOWN"
else echo "  # API key not on this machine; re-run with --new-key to make one"; fi
echo
if [ -n "$API_KEY" ]; then
  echo "Saved to $KEYFILE for use on this machine (source it in new shells)."
fi
if [ -n "$ADMIN_SHOWN" ]; then
  echo "Dashboard: $PUBLIC  user: admin  password: $ADMIN_SHOWN"
  echo "  (make more API keys there; the password lives in $SRV/admin.env)"
else
  echo "Dashboard: $PUBLIC  user: admin  (password in $SRV/admin.env)"
fi
if [ "$LOCAL" = 0 ] && [ -z "$DOMAIN" ]; then
  echo "HTTPS uses a self-signed cert: browsers warn and the SDKs refuse it. For a real cert, point a"
  echo "DNS name here and re-run with --domain NAME (ports 80 and 443 must be reachable)."
fi
if command -v dawnbx >/dev/null; then
  echo
  echo "Try it (the CLI reads $KEYFILE):"
  echo "  id=\$(dawnbx create) && dawnbx exec \$id \"python -c 'print(6*7)'\" && dawnbx kill \$id"
fi
rm -f "$SRV/api-key.pending" "$SRV/admin.pending"
report SUCCESS "dawnbx $DAWNBX_VERSION up at $PUBLIC"
