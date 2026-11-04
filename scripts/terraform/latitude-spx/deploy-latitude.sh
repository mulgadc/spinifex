#!/usr/bin/env bash
# deploy-latitude.sh — build a Spinifex deployment on latitude.sh bare metal, or tear it down.
#
#   deploy-latitude.sh up                                             one m4.metal.medium
#   deploy-latitude.sh status              nodes, addresses and what the deployment holds
#   deploy-latitude.sh credentials         print the AWS access key and secret
#   deploy-latitude.sh ssh [nodeN] [command]  SSH to a node, node1 by default
#   deploy-latitude.sh stop nodeN          power a node off, as a failure would
#   deploy-latitude.sh start nodeN         power it back on
#   deploy-latitude.sh destroy
#
# LATITUDESH_AUTH_TOKEN must be set. OpenTofu, or Terraform when OpenTofu is not
# installed, creates the project (unless --project), SSH key, user data, servers
# and Elastic IPs; this script formats the data disk, installs and forms Spinifex
# over SSH, and records each step under the state directory so a rerun resumes
# rather than re-initialising.
#
# The account token is read by the Terraform provider and by this script's own API calls;
# it is never a Terraform variable, never on a command line, and never sent to
# a node, so it is in no state file, log or user data. With --nodes 3 a second key,
# LATITUDESH_NODE_TOKEN, is copied to each node so the daemon can move Elastic IPs.
#
# The AWS profile is "spinifex-latitude-<name>" (name defaults to "default").
# Destroy removes by exact name only: servers spx-<name>-nodeN, and the key, user
# data and project spinifex-<name>; Elastic IPs only by the IDs it recorded.

set -euo pipefail
shopt -s inherit_errexit
# A provider trace log would carry the Authorization header.
unset TF_LOG TF_LOG_PATH TF_LOG_CORE TF_LOG_PROVIDER

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
SPINIFEX_SRC="$(cd "$SCRIPT_DIR/../../.." && pwd)"
LAT_API="https://api.latitude.sh"

CMD="${1:-}"
if [ $# -gt 0 ]; then shift; fi
NODE_ARG=""
NAME="default"
SITE="DAL"
PLAN="m4-metal-medium"
OS="ubuntu_26_04_x64_lts"
PROJECT=""
PROJECT_ID=""
DISTRO=""
SPX_BIN=/usr/local/bin/spx
# spx admin init runs under sudo and writes the profile to the invoking user's home, not root's.
NODE_AWS_DIR=/home/ubuntu/.aws
CHANNEL="latest"
SPX_REGION="us-east-1"
SPX_AZ="us-east-1a"
SSH_KEY="$HOME/.ssh/id_ed25519"
IMAGE=""
DATA_DISK_ID=""
STATE_ROOT="$HOME/.spinifex/latitude"
NODES=1
VPC_SUBNET="10.10.8.0"
VLAN_MTU=1500
YES=0

usage() {
    awk 'NR > 1 && /^$/ { exit } NR > 1' "$0" | sed 's/^# \{0,1\}//'
    cat <<EOF

Options:
  --name NAME          deployment name (default $NAME), 1-16 characters; names every resource
  --site SLUG          latitude.sh site (default $SITE; --region is the same); needs stock, not SAO
  --plan SLUG          server plan (default $PLAN)
  --os SLUG            operating system slug (default $OS)
  --project ID|SLUG    deploy into this existing project; by default one is created and removed on destroy
  --distro TARBALL     install this local release tarball
  --channel CHANNEL    install from a release channel: latest (default) or dev
  --spx-region NAME    Spinifex region (default $SPX_REGION)
  --spx-az NAME        Spinifex availability zone (default $SPX_AZ)
  --ssh-key PATH       private key; PATH.pub is installed (default $SSH_KEY)
  --image NAME         also import this AMI, such as ubuntu-26.04-x86_64 (default none)
  --data-disk-id PATH  the /dev/disk/by-id/nvme-eui.* path of the data disk; by default the largest blank disk
  --vpc-subnet ADDR    the VLAN's /24 for three nodes (default $VPC_SUBNET)
  --nodes N            1 or 3 (default $NODES)
  --yes                destroy without asking
EOF
}

while [ $# -gt 0 ]; do
    case "$1" in
        --name) NAME="$2"; shift ;;
        --site|--region) SITE="$2"; shift ;;
        --plan) PLAN="$2"; shift ;;
        --os) OS="$2"; shift ;;
        --project) PROJECT="$2"; shift ;;
        --distro) DISTRO="$2"; shift ;;
        --channel) CHANNEL="$2"; shift ;;
        --spx-region) SPX_REGION="$2"; shift ;;
        --spx-az) SPX_AZ="$2"; shift ;;
        --ssh-key) SSH_KEY="$2"; shift ;;
        --image) IMAGE="$2"; shift ;;
        --data-disk-id) DATA_DISK_ID="$2"; shift ;;
        --vpc-subnet) VPC_SUBNET="$2"; shift ;;
        --nodes) NODES="$2"; shift ;;
        --yes) YES=1 ;;
        -h|--help) usage; exit 0 ;;
        node[1-3])
            case "$CMD" in ssh|stop|start) ;; *) usage >&2; exit 2 ;; esac
            [ -z "$NODE_ARG" ] || { usage >&2; exit 2; }
            NODE_ARG="$1" ;;
        *) [ "$CMD" = ssh ] && break
           echo "unknown option: $1" >&2; usage >&2; exit 2 ;;
    esac
    shift
done

log() { printf '[deploy %s] %s\n' "$(date -u +%H:%M:%S)" "$*" >&2; }
die() { log "ERROR: $*"; exit 1; }

[[ "$NAME" =~ ^[a-z0-9][a-z0-9-]{0,15}$ ]] || die "--name must be 1-16 lowercase letters, digits or hyphens"
[ "$NODES" = 1 ] || [ "$NODES" = 3 ] || die "--nodes must be 1 or 3"
[[ "$VPC_SUBNET" =~ ^([0-9]{1,3}\.){3}0$ ]] || die "--vpc-subnet must be a /24 network address such as 10.10.8.0"
[[ "$SITE" =~ ^[A-Za-z0-9]+$ ]] || die "--site must be a site slug such as DAL"
[[ "$SPX_REGION" =~ ^[a-z0-9-]+$ ]] || die "--spx-region must be lowercase letters, digits and hyphens"
[[ "$SPX_AZ" =~ ^[a-z0-9-]+$ ]] || die "--spx-az must be lowercase letters, digits and hyphens"
[[ "$CHANNEL" =~ ^[a-z0-9.-]+$ ]] || die "--channel must be lowercase letters, digits, dots and hyphens"
[ -z "$IMAGE" ] || [[ "$IMAGE" =~ ^[A-Za-z0-9._-]+$ ]] || die "--image has characters an image name does not"
case "$CMD" in up|destroy|status|stop|start) [ -n "${LATITUDESH_AUTH_TOKEN:-}" ] || die "LATITUDESH_AUTH_TOKEN is not set" ;; esac
if command -v tofu >/dev/null; then TF_BIN=tofu
elif command -v terraform >/dev/null; then TF_BIN=terraform
else die "neither OpenTofu (tofu) nor Terraform is installed"; fi

STATE_DIR="$STATE_ROOT/$NAME"
TF_STATE="$STATE_DIR/terraform.tfstate"
KNOWN_HOSTS="$STATE_DIR/known_hosts"
KNOWN_IDS="$STATE_DIR/ids"
PROFILE="spinifex-latitude-$NAME"
CA_FILE="$HOME/.spinifex/latitude-$NAME-ca.pem"
mkdir -p "$STATE_DIR"
chmod 0700 "$STATE_DIR"
if [ -z "$PROJECT" ] && [ -f "$STATE_DIR/project" ]; then PROJECT="$(cat "$STATE_DIR/project")"; fi

# latitude METHOD PATH [JSON-BODY] prints the response body and fails on a non-2xx. The token goes to
# curl on stdin as a config line, so it is on no command line; printf is a shell builtin.
latitude() {
    local out code body=()
    if [ -n "${3:-}" ]; then body=(-H 'Content-Type: application/json' -d "$3"); fi
    out="$(printf 'header = "Authorization: Bearer %s"\n' "$LATITUDESH_AUTH_TOKEN" |
        curl -sS -g -K - -w '\n%{http_code}' -X "$1" -H 'Accept: application/json' "${body[@]}" "$LAT_API$2")" || return 1
    code="${out##*$'\n'}"
    out="${out%$'\n'*}"
    case "$code" in
        2??) printf '%s' "$out" ;;
        *) log "latitude.sh $1 $2 returned $code$(lat_error_code "$out")"; return 1 ;;
    esac
}

# lat_error_code prints " (CODE)" for the first error code of a response when it is one the API
# documents, and nothing else: a response can echo the request's headers, so its text is not logged.
lat_error_code() {
    printf '%s' "$1" | python3 -I -c '
import json, sys
known = {"ELASTIC_IP_NOT_ACTIVE", "ELASTIC_IP_NOT_RELEASABLE", "ELASTIC_IP_LIMIT_REACHED", "ELASTIC_IP_HAS_BGP_SESSIONS",
         "IP_ALLOCATION_FAILED", "SERVER_NOT_IN_PROJECT", "SITE_MISMATCH", "SITE_NOT_SUPPORTED", "SERVER_NETWORK_INCOMPATIBLE",
         "BGP_ELASTIC_IP_NOT_UPDATABLE", "UNAUTHORIZED", "FORBIDDEN", "NOT_FOUND"}
try:
    code = json.load(sys.stdin)["errors"][0]["code"]
except Exception:
    sys.exit(0)
if code in known:
    print(" (%s)" % code)' 2>/dev/null || true
}

# lat_list PATH prints the JSON array of a list: it follows links.next, and asks for the next page
# number while a page is full. A page that repeats an ID means the endpoint ignores page[number]
# (the Elastic IP list does), which is a failure here, not a complete list.
lat_list() {
    local path="$1?page[size]=100&page[number]=1" out res n next page=1 all='[]'
    while :; do
        out="$(latitude GET "$path")" || return 1
        res="$(python3 -I -c '
import json,sys
a = json.loads(sys.argv[1]); d = json.load(sys.stdin)
data = d.get("data") if isinstance(d, dict) else None
if not isinstance(data, list) or not all(isinstance(i, dict) and isinstance(i.get("id"), str) and isinstance(i.get("attributes"), dict) for i in data):
    sys.exit("unexpected list schema")
ids = {i["id"] for i in a}
if any(i["id"] in ids for i in data):
    sys.exit("a page repeats an earlier one")
links = d.get("links")
nxt = links.get("next") if isinstance(links, dict) else None
print(len(data)); print(nxt if isinstance(nxt, str) else ""); print(json.dumps(a + data))' "$all" <<<"$out")" || { log "latitude.sh list $1 could not be read"; return 1; }
        n="$(sed -n 1p <<<"$res")" next="$(sed -n 2p <<<"$res")" all="$(sed -n 3p <<<"$res")"
        page=$((page + 1))
        [ "$page" -le 200 ] || { log "latitude.sh list $1 has more than 200 pages"; return 1; }
        case "$next" in
            "") if [ "$n" -ge 100 ]; then path="$1?page[size]=100&page[number]=$page"; else break; fi ;;
            "$LAT_API"/*) path="${next#"$LAT_API"}" ;;
            /*) path="$next" ;;
            *) log "latitude.sh list $1 gave a next link to another host"; return 1 ;;
        esac
    done
    printf '%s' "$all"
}

# lat_exists PATH: 0 when GET answers 2xx, 1 on 404, 2 on anything else (logged).
lat_exists() {
    local code
    code="$(printf 'header = "Authorization: Bearer %s"\n' "$LATITUDESH_AUTH_TOKEN" |
        curl -sS -g -K - -o /dev/null -w '%{http_code}' -H 'Accept: application/json' "$LAT_API$1")" || return 2
    case "$code" in
        2??) return 0 ;;
        404) return 1 ;;
        *) log "latitude.sh GET $1 returned $code"; return 2 ;;
    esac
}

kind_path() {
    case "$1" in
        server) echo /servers ;; elastic-ip) echo /elastic_ips ;; ssh-key) echo /ssh_keys ;;
        user-data) echo /user_data ;; virtual-network) echo /virtual_networks ;; project) echo /projects ;; *) return 1 ;;
    esac
}

tf() {
    "$TF_BIN" -chdir="$SCRIPT_DIR" "$@" -state="$TF_STATE" \
        -var "deployment_name=$NAME" -var "site=$SITE" -var "plan=$PLAN" -var "operating_system=$OS" \
        -var "project=$PROJECT" -var "project_id=$PROJECT_ID" \
        -var "ssh_public_key_path=$SSH_KEY.pub" -var "node_count=$NODES"
}

done_step() { [ -f "$STATE_DIR/step.$1" ]; }
mark_step() { date -u +%FT%TZ > "$STATE_DIR/step.$1"; }

tf_output() { "$TF_BIN" -chdir="$SCRIPT_DIR" output -state="$TF_STATE" "$@"; }

# node_field FIELD prints FIELD of every node, in node order.
node_field() {
    tf_output -json nodes | python3 -I -c 'import json,sys; [print(n[sys.argv[1]]) for n in json.load(sys.stdin)]' "$1"
}

# for_nodes STEP FN runs FN once per node with IP, SERVER_ID and NODE_NAME set,
# recording STEP.<node> so a rerun skips the nodes already done.
for_nodes() {
    local step="$1" fn="$2" i
    for i in "${!IPS[@]}"; do
        IP="${IPS[$i]}" SERVER_ID="${IDS[$i]}" NODE_NAME="node$((i + 1))"
        done_step "$step.$NODE_NAME" && continue
        "$fn"
        mark_step "$step.$NODE_NAME"
    done
}

# on runs its argument as root on the node. latitude.sh puts the deploy key on the image's login
# user, ubuntu, so every remote command goes through passwordless sudo here and nowhere else.
# stdin passes through, so a caller can pipe a script or a secret without it reaching an argument.
# sudo resets SSH_CONNECTION, so the address this very session comes from is read before it and
# handed over as SSH_PEER.
on() {
    ssh -i "$SSH_KEY" -o BatchMode=yes -o IdentitiesOnly=yes -o ConnectTimeout=10 -o StrictHostKeyChecking=accept-new \
        -o ControlMaster=no -o ControlPath=none -o UserKnownHostsFile="$KNOWN_HOSTS" "ubuntu@$IP" \
        "peer=\"\${SSH_CONNECTION%% *}\"; sudo -n -H env \"SSH_PEER=\$peer\" bash -c $(printf '%q' "$*")"
}

# ── up ───────────────────────────────────────────────────────────────────────

# node_token_writable refuses a read-only node token, which passes every GET but cannot move an
# Elastic IP. The account token lists the team's keys; the node token is found by its last five
# characters, which reach the checker on stdin with the key list. If the key cannot be found,
# write authority is first proven by the first move.
node_token_writable() {
    local keys state
    keys="$(latitude GET /auth/api_keys)" || { log "cannot list API keys; the node token's write authority is first proven by the first Elastic IP move"; return 0; }
    state="$({ printf '%s\n' "${LATITUDESH_NODE_TOKEN: -5}"; printf '%s' "$keys"; } | python3 -I -c '
import json, sys
tail = sys.stdin.readline().strip()
for k in json.load(sys.stdin).get("data", []):
    a = k.get("attributes", {})
    if a.get("token_last_slice") == tail:
        print("read_only" if a.get("read_only") else "ok")
        break
else:
    print("unknown")')" || return 0
    case "$state" in
        read_only) die "LATITUDESH_NODE_TOKEN is a read-only key; a node must be able to move Elastic IPs" ;;
        ok) ;;
        *) log "LATITUDESH_NODE_TOKEN is not among the account's API keys; its write authority is first proven by the first Elastic IP move" ;;
    esac
}

# check_node_token checks the node key is set, differs from the account token and is accepted.
check_node_token() {
    # Each node holds a token to move Elastic IPs. It is a separate key so that it can be revoked on
    # its own, but a latitude.sh key is team-wide, not scoped to Elastic IPs: see the README.
    [ -n "${LATITUDESH_NODE_TOKEN:-}" ] || die "LATITUDESH_NODE_TOKEN is not set; create a second, writable API key for the nodes (it is copied to each node)"
    [ "$LATITUDESH_NODE_TOKEN" != "$LATITUDESH_AUTH_TOKEN" ] || die "LATITUDESH_NODE_TOKEN must differ from LATITUDESH_AUTH_TOKEN"
    printf 'header = "Authorization: Bearer %s"\n' "$LATITUDESH_NODE_TOKEN" |
        curl -sf -o /dev/null -K - "https://api.latitude.sh/projects?page%5Bsize%5D=1" || die "the API refused LATITUDESH_NODE_TOKEN"
    node_token_writable
}

# require_branch_build stops a three-node run that would install a release: the latitude pool
# needs a build with the latitude provider, and a daemon that does not know source="latitude"
# refuses its config and restarts in a loop.
require_branch_build() {
    [ "$NODES" -gt 1 ] || return 0
    [ -n "$DISTRO" ] || die "--nodes 3 needs --distro: the latitude pool is not in a release yet, and a released spx rejects source=\"latitude\" and crash-loops; build one with 'make distro-amd64' and pass its tarball"
}

# require_one_address stops a one-node run that would buy more than one Elastic IP. The one-node
# pool publishes a single address, and a second pool is never tried when the first is full, so
# the extra addresses would be paid for and never used.
require_one_address() {
    [ "$NODES" = 1 ] || return 0
    case "${TF_VAR_elastic_ip_count:-1}" in
        1) ;;
        *) die "a one-node deployment publishes one Elastic IP; unset TF_VAR_elastic_ip_count (it is ${TF_VAR_elastic_ip_count}) or use --nodes 3 for the floating pool" ;;
    esac
}
preflight() {
    require_branch_build
    require_one_address
    [ -r "$SSH_KEY" ] && [ -r "$SSH_KEY.pub" ] || die "SSH key $SSH_KEY (and .pub) not readable"
    command -v aws >/dev/null || die "the AWS CLI is not installed"
    command -v python3 >/dev/null || die "python3 is not installed"
    if [ -n "$DISTRO" ]; then
        [ -r "$DISTRO" ] || die "--distro $DISTRO not readable"
    fi
    latitude GET '/projects?page[size]=1' >/dev/null || die "the API refused the token (LATITUDESH_AUTH_TOKEN)"
    if [ "$NODES" -gt 1 ]; then check_node_token; fi
    if [ -n "$PROJECT" ]; then
        # The Elastic IP create needs the project ID, so an ID or a slug is resolved to the ID here.
        PROJECT_ID="$(latitude GET "/projects/$PROJECT" | python3 -I -c 'import json,sys; print(json.load(sys.stdin)["data"]["id"])')" ||
            die "no project with ID or slug $PROJECT; leave --project out to create one"
        printf '%s\n' "$PROJECT" > "$STATE_DIR/project"
    fi
    done_step terraform || stock_check
}

# stock_check fails when the plan has no stock at the site, before anything is created: an
# out-of-stock error comes only after other objects exist. A response of another shape is warned about.
stock_check() {
    local res
    res="$(latitude GET "/plans?filter[slug]=$PLAN" | python3 -I -c '
import json,sys
site, plan = sys.argv[1].lower(), sys.argv[2]
try:
    items = [i for i in json.load(sys.stdin)["data"] if i["attributes"].get("slug") == plan]
    regions = items[0]["attributes"]["regions"]
    stocked = {s.lower() for r in regions for s in (r.get("locations", {}).get("in_stock") or [])}
    print("ok" if site in stocked else "none")
except Exception:
    print("unknown")' "$SITE" "$PLAN")" || die "could not read the plan list"
    case "$res" in
        ok) ;;
        none) die "plan $PLAN has no stock at $SITE; try another --site or --plan" ;;
        *) log "could not read stock for $PLAN at $SITE from the plan list; continuing" ;;
    esac
}

# record_ids merges "kind id" for every latitudesh_* object in the Terraform state into the ids file,
# so destroy still knows an object after the state is gone. It fails, and the caller stops, when the
# state cannot be read, holds a resource type it does not know, or holds a resource without an ID.
record_ids() {
    local state found
    [ -f "$TF_STATE" ] || return 0
    state="$("$TF_BIN" -chdir="$SCRIPT_DIR" show -json "$TF_STATE")" || return 1
    found="$(python3 -I -c '
import json,sys
kinds = {"latitudesh_server": "server", "latitudesh_elastic_ip": "elastic-ip", "latitudesh_ssh_key": "ssh-key",
         "latitudesh_user_data": "user-data", "latitudesh_virtual_network": "virtual-network", "latitudesh_project": "project"}
skip = {"latitudesh_vlan_assignment"}
def walk(m):
    for r in m.get("resources", []):
        t = r.get("type", "")
        if r.get("mode") != "managed" or not t.startswith("latitudesh_") or t in skip:
            continue
        if t not in kinds:
            sys.exit("the state holds a %s, which this script does not know how to remove" % t)
        i = (r.get("values") or {}).get("id")
        if not isinstance(i, str) or not i:
            sys.exit("%s %s has no ID in the state" % (t, r.get("address")))
        print(kinds[t], i)
    for c in m.get("child_modules", []):
        walk(c)
walk((json.load(sys.stdin).get("values") or {}).get("root_module") or {})' <<<"$state")" || return 1
    { [ ! -f "$KNOWN_IDS" ] || cat "$KNOWN_IDS"; [ -z "$found" ] || printf '%s\n' "$found"; } | sort -u > "$KNOWN_IDS.new" &&
        mv -f "$KNOWN_IDS.new" "$KNOWN_IDS"
}

provision() {
    log "terraform apply ($PLAN at $SITE)"
    "$TF_BIN" -chdir="$SCRIPT_DIR" init -input=false >/dev/null
    tf apply -input=false -auto-approve >"$STATE_DIR/apply.log" 2>&1 || {
        tail -20 "$STATE_DIR/apply.log" >&2
        record_ids || log "WARNING: could not record the IDs in the state; check the dashboard before anything else"
        die "terraform apply failed; log in $STATE_DIR/apply.log. Check the latitude.sh dashboard for a server left in error"
    }
    record_ids || die "could not record the created IDs from the Terraform state; run destroy to remove them"
}

# wait_host pins the host key only once the marker cloud-init writes exists: the
# server first answers SSH from the provisioning image, with a key of its own.
wait_host() {
    log "waiting for SSH and cloud-init on $IP"
    local i
    for i in $(seq 1 120); do
        ssh -i "$SSH_KEY" -o BatchMode=yes -o IdentitiesOnly=yes -o ConnectTimeout=10 -o StrictHostKeyChecking=no \
            -o ControlMaster=no -o ControlPath=none -o UserKnownHostsFile=/dev/null -o LogLevel=ERROR "ubuntu@$IP" \
            'test -f /var/lib/cloud/spinifex-host-ready' 2>/dev/null && break
        [ "$i" = 120 ] && die "host $IP was not ready after 20 minutes: no SSH login as ubuntu with $SSH_KEY, or cloud-init has not finished"
        sleep 10
    done
    ssh-keygen -R "$IP" -f "$KNOWN_HOSTS" >/dev/null 2>&1 || true
    on 'cloud-init status --wait >/dev/null; test -e /dev/kvm' || die "no /dev/kvm on $IP"
    # The image's resolv.conf is not a link to systemd-resolved, so the node would bypass the
    # split DNS Spinifex sets in resolved for its LB and EKS names.
    on 'if [ ! -L /etc/resolv.conf ] && systemctl is-active -q systemd-resolved; then
            ln -sf ../run/systemd/resolve/stub-resolv.conf /etc/resolv.conf; fi' ||
        die "could not point /etc/resolv.conf at systemd-resolved on $IP"
}

# vlan_addr prints the VLAN interface's name and address on the current node.
vlan_addr() { on 'ip -4 -o addr show' | awk -v p="${VPC_SUBNET%.*}." 'index($4, p) == 1 {sub(/\/.*/, "", $4); print $2, $4; exit}'; }

# vlan puts the node on the deployment VLAN as eno2.<vid>, the cluster and Geneve plane. It never
# runs netplan apply, which re-applies every interface including the public one: the link is made
# with ip, and persisted as systemd-networkd files for eno2.<vid> plus a drop-in that attaches it to
# eno2. It refuses a subnet that holds the SSH peer or overlaps a route, and removes the VLAN again if
# the public interface's address, default route or the route to the SSH peer differ afterwards.
vlan() {
    local vid addr
    vid="$(tf_output -raw vid)"
    [[ "$vid" =~ ^[0-9]{1,4}$ ]] || die "the Terraform state has no VLAN ID"
    addr="${VPC_SUBNET%.*}.$((10 + ${NODE_NAME#node}))/24"
    oob_command
    log "console for $NODE_NAME if SSH is lost: $OOB_CMD"
    log "putting $NODE_NAME on VLAN $vid ($addr, MTU $VLAN_MTU)"
    on "bash -s -- $vid $addr $VLAN_MTU" <<'REMOTE' || die "the VLAN step failed on $IP; if SSH is lost, use the console command printed above, or destroy and up again"
set -euo pipefail
vid=$1 addr=$2 mtu=$3
client="${SSH_PEER:-}"
[ -n "$client" ] || { echo "cannot tell the address this SSH session comes from" >&2; exit 1; }
ip link show eno2 >/dev/null
pub="$(ip -4 route show default | awk '{for (i = 1; i < NF; i++) if ($i == "dev") {print $(i + 1); exit}}')"
[ -n "$pub" ] && [ "$pub" != eno2 ] && [ "$pub" != "eno2.$vid" ] || { echo "the default route is on '$pub', not the public NIC" >&2; exit 1; }
# The VLAN address adds a connected route for its whole /24, so the subnet must not hold the
# SSH peer or overlap any route already there.
python3 - "$addr" "$client" "eno2.$vid" <<'PY' || exit 1
import ipaddress, subprocess, sys
net = ipaddress.ip_interface(sys.argv[1]).network
peer = ipaddress.ip_address(sys.argv[2])
bad = []
if peer in net:
    bad.append("the SSH peer %s" % peer)
for line in subprocess.check_output(["ip", "-4", "route", "show"], text=True).splitlines():
    f = line.split()
    if not f or f[0] == "default" or ("dev" in f and f[f.index("dev") + 1] == sys.argv[3]):
        continue
    try:
        r = ipaddress.ip_network(f[0], strict=False)
    except ValueError:
        continue
    if r.overlaps(net):
        bad.append("the route %s" % line)
if bad:
    sys.exit("the VLAN subnet %s overlaps %s; choose another --vpc-subnet" % (net, "; ".join(bad)))
PY
snap() { ip -4 -o addr show dev "$pub"; ip -4 route show default; ip -4 route get "$client" | head -n1; }
undo() {
    ip link delete "eno2.$vid" 2>/dev/null || true
    rm -f /etc/systemd/network/60-spinifex-vlan.netdev /etc/systemd/network/60-spinifex-vlan.network \
        /etc/systemd/network/59-spinifex-eno2.network /etc/systemd/network/*.network.d/60-spinifex-vlan.conf
}
before="$(snap)"
if ip link show "eno2.$vid" >/dev/null 2>&1; then
    ip -d link show "eno2.$vid" | grep -q "vlan protocol 802.1Q id $vid " || { echo "eno2.$vid exists but is not VLAN $vid" >&2; exit 1; }
else
    ip link add link eno2 name "eno2.$vid" type vlan id "$vid"
fi
ip link set eno2 up
ip link set "eno2.$vid" mtu "$mtu" up
ip addr replace "$addr" dev "eno2.$vid"
n=/etc/systemd/network
install -d "$n"
printf '[NetDev]\nName=eno2.%s\nKind=vlan\n\n[VLAN]\nId=%s\n' "$vid" "$vid" > "$n/60-spinifex-vlan.netdev"
printf '[Match]\nName=eno2.%s\n\n[Network]\nAddress=%s\n\n[Link]\nMTUBytes=%s\n' "$vid" "$addr" "$mtu" > "$n/60-spinifex-vlan.network"
units="$(grep -lx 'Name=eno2' /run/systemd/network/*.network "$n"/*.network 2>/dev/null | xargs -r -n1 basename | sort -u)" || units=""
case "$(wc -w <<<"$units")" in
    0) printf '[Match]\nName=eno2\n\n[Network]\nVLAN=eno2.%s\nLinkLocalAddressing=no\n' "$vid" > "$n/59-spinifex-eno2.network" ;;
    1) install -d "$n/$units.d"
       printf '[Network]\nVLAN=eno2.%s\n' "$vid" > "$n/$units.d/60-spinifex-vlan.conf" ;;
    *) echo "more than one networkd unit matches eno2: $units" >&2; exit 1 ;;
esac
ip -4 -o addr show "eno2.$vid" | grep -q "${addr%/*}/" || { echo "eno2.$vid has no $addr" >&2; exit 1; }
if [ "$(snap)" != "$before" ]; then
    echo "the route to this SSH session or the public interface $pub changed; removing the VLAN again. before: $before after: $(snap)" >&2
    undo
    exit 1
fi
REMOTE
    # A new login (no shared connection) proves the public path is intact.
    vlan_addr | grep -q . || die "no VLAN address on $IP after the change"
}

# data_disk formats and mounts the blank data disk at /var/lib/spinifex, which latitude.sh
# servers keep apart from the root disk, and records the filesystem UUID it made.
data_disk() {
    log "preparing the data disk on $NODE_NAME"
    local tmp rc=0 uuid
    scp -q -i "$SSH_KEY" -o BatchMode=yes -o IdentitiesOnly=yes -o ControlMaster=no -o ControlPath=none -o UserKnownHostsFile="$KNOWN_HOSTS" \
        "$SCRIPT_DIR/host-data-disk.sh" "ubuntu@$IP:/tmp/host-data-disk.sh" || die "copy to $IP failed"
    tmp="$(mktemp "$STATE_DIR/data-disk.XXXXXX")"
    on "bash /tmp/host-data-disk.sh $DATA_DISK_ID" >"$tmp" 2>&1 || rc=$?
    cat "$tmp" >&2
    cat "$tmp" >> "$STATE_DIR/data-disk.$NODE_NAME.log"
    uuid="$(sed -n 's/^DATA_UUID=//p' "$tmp" | tail -n1)"
    rm -f "$tmp"
    [ "$rc" = 0 ] || die "the data disk step failed (exit $rc); it may already have formatted the disk, so read the output above and $STATE_DIR/data-disk.$NODE_NAME.log before rerunning"
    [[ "$uuid" =~ ^[0-9a-fA-F-]{16,36}$ ]] || die "the data disk step gave no UUID"
    printf '%s\n' "$uuid" > "$STATE_DIR/data_uuid.$NODE_NAME"
}

# check_mount fails unless /var/lib/spinifex is mounted from the filesystem formatted at deploy
# time. It runs before anything writes there, on every run, so a node whose disks were renamed
# or swapped is stopped instead of filling the root disk.
check_mount() {
    local want have
    want="$(cat "$STATE_DIR/data_uuid.$NODE_NAME" 2>/dev/null)" || die "no recorded data disk UUID for $NODE_NAME"
    have="$(on 'findmnt --mountpoint /var/lib/spinifex -no UUID')" || have=""
    [ "$have" = "$want" ] || die "/var/lib/spinifex on $NODE_NAME is mounted from '${have:-nothing}', not the data disk $want"
}

install_spinifex() {
    log "installing Spinifex"
    if [ -n "$DISTRO" ]; then
        scp -q -i "$SSH_KEY" -o BatchMode=yes -o IdentitiesOnly=yes -o ControlMaster=no -o ControlPath=none -o UserKnownHostsFile="$KNOWN_HOSTS" \
            "$DISTRO" "$SPINIFEX_SRC/scripts/setup.sh" "ubuntu@$IP:/tmp/" || die "copy to $IP failed"
        on "INSTALL_SPINIFEX_TARBALL=/tmp/$(basename "$DISTRO") bash /tmp/setup.sh >/root/setup.log 2>&1" ||
            die "setup.sh failed; see /root/setup.log on $IP"
        # A tarball built by build-install-artifacts.sh ships setup.sh beside it
        # rather than in it, and the later steps run the installed copy.
        on 'test -x /usr/local/share/spinifex/setup.sh || install -D -m 0755 /tmp/setup.sh /usr/local/share/spinifex/setup.sh'
    else
        on "curl -fsSL -o /root/install.sh https://install.mulgadc.com && bash /root/install.sh --channel $CHANNEL >/root/setup.log 2>&1" ||
            die "install failed; see /root/setup.log on $IP"
    fi
    # Units that write under /var/lib/spinifex wait for its mount rather than start on the root disk.
    on 'for u in spinifex-daemon spinifex-nats spinifex-predastore spinifex-viperblock spinifex-vpcd spinifex-awsgw; do
            install -d /etc/systemd/system/$u.service.d
            printf "[Unit]\nRequiresMountsFor=/var/lib/spinifex\n" > /etc/systemd/system/$u.service.d/10-spinifex-data-mount.conf
        done
        systemctl daemon-reload' || die "could not add the data mount requirement on $IP"
}

# form brings a node up as its own one-node cluster, the state an ISO install
# leaves and the one install-node.sh forms a larger cluster from.
form() {
    local enc="$IP" vlan_if vlan_ip
    if [ "$NODES" -gt 1 ]; then
        read -r vlan_if vlan_ip <<<"$(vlan_addr)"
        [ -n "$vlan_ip" ] || die "no VLAN address on $IP"
        enc="$vlan_ip"
    fi
    log "initialising $NODE_NAME on $IP (encap $enc, region $SPX_REGION)"
    on "/usr/local/share/spinifex/setup-ovn.sh --management --nat-uplink --encap-ip=$enc >/root/setup-ovn.log 2>&1" ||
        die "setup-ovn.sh failed; see /root/setup-ovn.log on $IP"
    on "spx admin init --node $NODE_NAME --nodes 1 --region $SPX_REGION --az $SPX_AZ --ipsec=false --external-mode=nat >/root/init.log 2>&1" ||
        die "spx admin init failed; see /root/init.log on $IP"
}

# oob_command sets OOB_CMD to the serial console command. The server's out_of_band_connection
# (GET, or POST with an SSH key ID to enable it) carries username, access_ip, port and status; only
# those are read, because the response also holds credentials, which are never printed or logged.
oob_command() {
    OOB_CMD="the ssh command in the server's Recovery tab, Out-of-Band, in the latitude.sh dashboard"
    local key_id out info i u ip port
    key_id="$(tf_output -raw ssh_key_id 2>/dev/null)" || key_id=""
    for i in 1 2 3 4 5 6 7; do
        out="$(latitude GET "/servers/$SERVER_ID/out_of_band_connection" 2>/dev/null)" || out=""
        info="$(python3 -I -c '
import json,re,sys
try:
    a = json.load(sys.stdin)["data"]["attributes"]
    u, ip, port, st = a.get("username"), a.get("access_ip"), str(a.get("port")), a.get("status")
    ok = re.fullmatch(r"[A-Za-z0-9_.-]+", u or "") and re.fullmatch(r"(\d{1,3}\.){3}\d{1,3}", ip or "") and re.fullmatch(r"\d{1,5}", port)
    print("%s %s %s %s" % (st, u, ip, port) if ok else "bad")
except Exception:
    print("none")' <<<"$out")" || info=none
        case "$info" in
            "connected "*) read -r _ u ip port <<<"$info"; OOB_CMD="ssh -i $SSH_KEY $u@$ip -p $port"; return 0 ;;
            none) if [ "$i" = 1 ] && [ -n "$key_id" ]; then
                      latitude POST "/servers/$SERVER_ID/out_of_band_connection" "{\"data\":{\"type\":\"out_of_band\",\"attributes\":{\"ssh_key_id\":\"$key_id\"}}}" >/dev/null 2>&1 || return 0
                  fi ;;
            bad) return 0 ;;
        esac
        sleep 10
    done
    return 0
}

start_spinifex() {
    oob_command
    log "console for $NODE_NAME: $OOB_CMD"
    log "if SSH is lost once the firewall is armed, a fresh deployment is recovered by destroy then up"
    log "arming the firewall and starting $NODE_NAME"
    on 'env SETUP_STAGES=firewall /usr/local/share/spinifex/setup.sh --firewall on >/root/firewall.log 2>&1' ||
        die "arming the firewall failed; see /root/firewall.log on $IP"
    on 'systemctl start spinifex.target'
    wait_api
}

wait_api() {
    local i
    for i in $(seq 1 30); do
        on 'curl -sk -o /dev/null -w "%{http_code}" https://127.0.0.1:9999/' 2>/dev/null | grep -q '^[34]' && break
        [ "$i" = 30 ] && die "the API did not come up on $IP"
        sleep 10
    done
}

# form_cluster hands the armed single nodes to install-node.sh. It dials the public addresses
# over SSH and puts the cluster and Geneve planes on the VLAN interface, named alike on every node.
form_cluster() {
    local hosts_file="$STATE_DIR/hosts" vlan_if="" if_here ip_here pinned
    printf '%s\n' "${IPS[@]}" > "$hosts_file"
    for IP in "${IPS[@]}"; do
        read -r if_here ip_here <<<"$(vlan_addr)"
        [ -n "$ip_here" ] || die "no VLAN address on $IP"
        [ -z "$vlan_if" ] || [ "$vlan_if" = "$if_here" ] || die "VLAN interface is $vlan_if on one node and $if_here on $IP"
        vlan_if="$if_here"
        # install-node.sh reads ~/.ssh/known_hosts and addresses are reused, so replace any older
        # key for this one with the key this deployment pinned. -F, not grep: entries are usually hashed.
        pinned="$(ssh-keygen -F "$IP" -f "$KNOWN_HOSTS" | grep -v '^#')" || die "no pinned host key for $IP in $KNOWN_HOSTS"
        ssh-keygen -R "$IP" >/dev/null 2>&1 || true
        printf '%s\n' "$pinned" >> "$HOME/.ssh/known_hosts"
    done
    log "forming ${#IPS[@]} nodes with install-node.sh (cluster plane $vlan_if)"
    "$SPINIFEX_SRC/scripts/install-node.sh" --hosts-file "$hosts_file" --user ubuntu --identity "$SSH_KEY" \
        --external-mode nat --ipsec off --region "$SPX_REGION" --az "$SPX_AZ" \
        --lan-bridge "$vlan_if" --vpc-bridge "$vlan_if" --yes >/dev/null 2>&1 ||
        die "formation failed; its output is not kept, as it can hold the join token: rerun install-node.sh by hand (hosts in $hosts_file, --user ubuntu --identity $SSH_KEY) and keep its output to yourself"
}

# configure_pool gives a one-node deployment its Elastic IP as a static pool and moves the
# IMDS addresses off the ones latitude.sh routes. The gateway is the node's own next hop; the
# placeholder gateway_ip keeps NAT mode from taking the pool's only address.
configure_pool() {
    log "adding the Elastic IP pool on $NODE_NAME"
    local eip gw rp
    eip="$(tf_output -json elastic_ips | python3 -I -c '
import json, sys
ips = json.load(sys.stdin)
if len(ips) != 1:
    sys.exit("the one-node pool publishes one Elastic IP, and this deployment has %d" % len(ips))
print(ips[0]["address"])')" || die "a one-node deployment must have exactly one Elastic IP; set elastic_ip_count to 1 and run up again"
    gw="$(on "ip -4 route show default" | awk '{for (i = 1; i < NF; i++) if ($i == "via") {print $(i + 1); exit}}')"
    [ -n "$gw" ] || die "no default gateway on $IP"
    rp="$(on 'i=$(ip -4 route show default | awk "{for (i = 1; i < NF; i++) if (\$i == \"dev\") {print \$(i + 1); exit}}"); a=$(cat /proc/sys/net/ipv4/conf/all/rp_filter); d=$(cat /proc/sys/net/ipv4/conf/$i/rp_filter); echo $(( a > d ? a : d ))')" || rp=""
    [ "$rp" != 1 ] || die "rp_filter is strict on $IP: an Elastic IP arrives from the gateway but guest replies leave by another path; set net.ipv4.conf.all.rp_filter and the interface's to 2 and rerun"
    on "python3 - '$eip' '$gw'" <<'PY' || die "could not edit spinifex.toml on $IP"
import re, sys, tomllib
eip, gateway = sys.argv[1:]
path = "/etc/spinifex/spinifex.toml"
text = open(path).read()
if "imds_host_meta_ip" not in text:
    text, n = re.subn(r"(?m)^\[network\]$",
                      '[network]\nimds_host_meta_ip = "169.254.42.254"\nimds_host_dns_ip  = "169.254.42.253"', text, count=1)
    if n != 1:
        sys.exit("no [network] table in %s" % path)
if 'name               = "latitude-eip"' not in text:
    # prefix_len stays unset: a /32 pool with a gateway outside it is rejected otherwise.
    text = text.rstrip("\n") + (
        '\n\n[[network.external_pools]]\nname               = "latitude-eip"\nsource             = "static"\n'
        'range_start        = "%s"\nrange_end          = "%s"\ngateway            = "%s"\ngateway_ip         = "192.0.2.1"\n' % (eip, eip, gateway))
open(path, "w").write(text)
net = tomllib.load(open(path, "rb")).get("network", {})
pools = [p for p in net.get("external_pools", []) if p.get("name") == "latitude-eip"]
bad = []
if net.get("imds_host_meta_ip") != "169.254.42.254" or net.get("imds_host_dns_ip") != "169.254.42.253": bad.append("imds remap")
if net.get("external_mode") != "nat": bad.append("external_mode %r" % net.get("external_mode"))
if len(pools) != 1: bad.append("expected one latitude-eip pool, found %d" % len(pools))
else:
    p = pools[0]
    for key, want in (("source", "static"), ("range_start", eip), ("range_end", eip), ("gateway", gateway), ("gateway_ip", "192.0.2.1")):
        if p.get(key) != want: bad.append("pool %s is %r, expected %r" % (key, p.get(key), want))
    if "prefix_len" in p: bad.append("pool has prefix_len, which must be unset")
if bad:
    sys.exit("spinifex.toml does not match: " + "; ".join(bad))
PY
}

# install_key puts the node token where the daemon reads it, on every up: a rerun after a
# rotation replaces a revoked token, and one with the same token changes nothing. The token travels
# on stdin and the script is the command, so the token is in no argument on either side. The
# daemon rereads the file on each call.
install_key() {
    local did
    log "installing the node API token on $NODE_NAME"
    did="$(printf '%s' "$LATITUDESH_NODE_TOKEN" | on "$(cat "$SCRIPT_DIR/node-install-api-token.sh")")" ||
        die "could not install the node token on $IP"
    log "node API token on $NODE_NAME: $did"
    on 'printf "header = \"Authorization: Bearer %s\"\n" "$(cat /etc/spinifex/latitude/api-token)" |
        curl -sf -o /dev/null -K - "https://api.latitude.sh/projects?page%5Bsize%5D=1"' ||
        die "the node's API calls are refused from $IP"
}

# node_knows_latitude stops before a node is given a config its binary rejects. The pool's key
# names appear in the binary only if it knows the latitude source.
node_knows_latitude() {
    on "grep -aqF latitude_elastic_ip_ids '$SPX_BIN'" ||
        die "the spx on $IP does not know source=\"latitude\" (no latitude_elastic_ip_ids in $SPX_BIN); install a build from this branch with --distro"
}

# configure_latitude_pool adds the floating pool, naming the Elastic IPs, and every node's server ID:
# the config is validated against all [nodes.*] tables, and a takeover moves the address to the
# taking node's own server.
configure_latitude_pool() {
    log "adding the latitude pool on $NODE_NAME"
    node_knows_latitude
    local ids="" eips project i
    for i in "${!IDS[@]}"; do ids+="node$((i + 1))=${IDS[$i]},"; done
    eips="$(tf_output -json elastic_ips | python3 -I -c 'import json,sys; print(",".join(e["id"] for e in json.load(sys.stdin)))')"
    project="$(tf_output -raw project_id)"
    [ -n "$eips" ] && [ -n "$project" ] || die "the Terraform state has no Elastic IP IDs or project ID"
    on "python3 - '$project' '$eips' '${ids%,}'" <<'PY' || die "could not edit spinifex.toml on $IP"
import re, sys, tomllib
project, eips, ids = sys.argv[1:]
eips = eips.split(",")
path = "/etc/spinifex/spinifex.toml"
s = open(path).read()
if "imds_host_meta_ip" not in s:
    s, n = re.subn(r"(?m)^\[network\]$",
                   '[network]\nimds_host_meta_ip = "169.254.42.254"\nimds_host_dns_ip  = "169.254.42.253"', s, count=1)
    if n != 1:
        sys.exit("no [network] table in %s" % path)
if 'source                  = "latitude"' not in s:
    pool = ('[[network.external_pools]]\nname                    = "latitude"\nsource                  = "latitude"\n'
            'latitude_project_id     = "%s"\nlatitude_key_file       = "/etc/spinifex/latitude/api-token"\n'
            'latitude_elastic_ip_ids = [%s]\n\n[bootstrap]' % (project, ", ".join('"%s"' % e for e in eips)))
    if s.count("\n[bootstrap]") != 1:
        sys.exit("expected one [bootstrap] table")
    s = s.replace("\n[bootstrap]", "\n" + pool, 1)
for pair in ids.split(","):
    node, host_id = pair.split("=")
    head = "[nodes.%s]\n" % node
    m = re.search(r"^" + re.escape(head) + r"(.*?)(?=^\[|\Z)", s, flags=re.M | re.S)
    if not m:
        sys.exit("could not find [nodes.%s]" % node)
    if "provider_host_id" not in m.group(1):
        s = s[:m.start()] + head + 'provider_host_id = "%s"\n' % host_id + s[m.start() + len(head):]
open(path, "w").write(s)
cfg = tomllib.loads(s)
pools = [q for q in cfg.get("network", {}).get("external_pools", []) if q.get("name") == "latitude"]
bad = []
if len(pools) != 1: bad.append("expected one latitude pool, found %d" % len(pools))
else:
    q = pools[0]
    if q.get("source") != "latitude" or q.get("latitude_project_id") != project or q.get("latitude_elastic_ip_ids") != eips:
        bad.append("pool fields differ from what was written")
for pair in ids.split(","):
    node, host_id = pair.split("=")
    if cfg.get("nodes", {}).get(node, {}).get("provider_host_id") != host_id:
        bad.append("%s provider_host_id" % node)
if cfg.get("network", {}).get("external_mode") != "nat": bad.append("external_mode is not nat")
net = cfg.get("network", {})
if net.get("imds_host_meta_ip") != "169.254.42.254" or net.get("imds_host_dns_ip") != "169.254.42.253": bad.append("imds remap")
if bad:
    sys.exit("spinifex.toml does not match: " + "; ".join(bad))
PY
}

restart_spinifex() {
    log "restarting $NODE_NAME with the pool"
    on 'systemctl restart spinifex.target'
    wait_api
    # A node with a floating pool must log its allocator, or public addresses fail only at first launch.
    [ "$NODES" -gt 1 ] || return 0
    local i state
    for i in $(seq 1 30); do
        state="$(on "set -- latitude; $(cat "$SCRIPT_DIR/node-pool-state.sh")")" || state=waiting
        case "$state" in
            ready) return 0 ;;
            refused) die "$NODE_NAME's daemon was refused by latitude.sh on the pool's first call; see journalctl -u spinifex-daemon on $IP" ;;
        esac
        sleep 10
    done
    die "$NODE_NAME's current daemon never logged its latitude pool as ready; see journalctl -u spinifex-daemon on $IP"
}

# public_default_subnets turns on auto-assign for the default subnets formation
# created before the pool existed, as AWS default subnets have it. Each change is read back.
public_default_subnets() {
    log "auto-assigning public addresses in the default subnets"
    on "bash -s -- '$NODE_AWS_DIR'" <<'REMOTE' || die "could not turn on public addresses in the default subnets on $IP"
set -euo pipefail
export AWS_PROFILE=spinifex AWS_SHARED_CREDENTIALS_FILE=$1/credentials AWS_CONFIG_FILE=$1/config
subnets="$(aws ec2 describe-subnets --filters Name=default-for-az,Values=true --query "Subnets[].SubnetId" --output text)"
[ -n "$subnets" ] || { echo "no default subnets found" >&2; exit 1; }
for s in $subnets; do
    aws ec2 modify-subnet-attribute --subnet-id "$s" --map-public-ip-on-launch
    v="$(aws ec2 describe-subnets --subnet-ids "$s" --query "Subnets[0].MapPublicIpOnLaunch" --output text)"
    [ "$v" = True ] || { echo "subnet $s MapPublicIpOnLaunch is '$v'" >&2; exit 1; }
done
REMOTE
}

import_image() {
    log "importing $IMAGE"
    on "spx admin images import --name $IMAGE --config /etc/spinifex/spinifex.toml >/root/image-import.log 2>&1" ||
        die "image import failed; see /root/image-import.log on $IP"
}

# write_credentials KEY SECRET sets the profile's keys in the shared credentials file without putting
# the secret in any process argument: it reaches python on stdin. Other profiles are kept, the old
# file is saved beside it, and the new one is 0600.
write_credentials() {
    local code
    code='
import configparser, os, shutil, sys, time
profile = sys.argv[1]
key, secret = sys.stdin.read().split("\n")[:2]
path = os.environ.get("AWS_SHARED_CREDENTIALS_FILE") or os.path.expanduser("~/.aws/credentials")
os.umask(0o077)
os.makedirs(os.path.dirname(path), mode=0o700, exist_ok=True)
cp = configparser.RawConfigParser()
cp.optionxform = str
if os.path.exists(path):
    cp.read(path)
    shutil.copy2(path, "%s.pre-latitude.%d" % (path, int(time.time())))
if not cp.has_section(profile):
    cp.add_section(profile)
cp.set(profile, "aws_access_key_id", key)
cp.set(profile, "aws_secret_access_key", secret)
cp.remove_option(profile, "aws_session_token")
tmp = path + ".tmp"
with open(tmp, "w") as f:
    cp.write(f)
os.chmod(tmp, 0o600)
os.replace(tmp, path)
'
    printf '%s\n%s\n' "$1" "$2" | python3 -I -c "$code" "$PROFILE" || die "could not write the credentials file"
}

write_profile() {
    log "writing the CA to $CA_FILE and AWS profile $PROFILE"
    mkdir -p "$(dirname "$CA_FILE")"
    local creds key secret ca_new
    # The CA is fetched to a temporary file and checked before it replaces the existing one.
    ca_new="$(mktemp "$CA_FILE.new.XXXXXX")"
    on 'cat /etc/spinifex/ca.pem' > "$ca_new" || { rm -f "$ca_new"; die "could not read the node's CA"; }
    grep -q -- '-----BEGIN CERTIFICATE-----' "$ca_new" || { rm -f "$ca_new"; die "the node's CA is empty or not a PEM certificate"; }
    creds="$(on "awk '/^\\[spinifex\\]/{f=1;next} /^\\[/{f=0} f' '$NODE_AWS_DIR/credentials'")"
    key="$(awk -F' *= *' '/aws_access_key_id/{print $2}' <<<"$creds")"
    secret="$(awk -F' *= *' '/aws_secret_access_key/{print $2}' <<<"$creds")"
    [ -n "$key" ] && [ -n "$secret" ] || { rm -f "$ca_new"; die "no [spinifex] credentials on $IP"; }
    chmod 0644 "$ca_new"
    mv -f "$ca_new" "$CA_FILE"
    write_credentials "$key" "$secret"
    aws configure set --profile "$PROFILE" region "$SPX_REGION"
    aws configure set --profile "$PROFILE" endpoint_url "https://$IP:9999"
    aws configure set --profile "$PROFILE" ca_bundle "$CA_FILE"
    aws --profile "$PROFILE" sts get-caller-identity --query Account --output text >/dev/null ||
        die "profile $PROFILE does not work against https://$IP:9999; check the API certificate names $IP"
}

cmd_up() {
    preflight
    done_step terraform || { provision; mark_step terraform; }
    mapfile -t IPS < <(node_field public_ip)
    mapfile -t IDS < <(node_field provider_host_id)
    [ "${#IPS[@]}" = "$NODES" ] || die "the state holds ${#IPS[@]} node(s), not $NODES; destroy it or pass --nodes ${#IPS[@]}"
    for_nodes host wait_host
    for_nodes disk data_disk
    local i
    for i in "${!IPS[@]}"; do IP="${IPS[$i]}" NODE_NAME="node$((i + 1))"; check_mount; done
    if [ "$NODES" -gt 1 ]; then for_nodes vlan vlan; fi
    for_nodes install install_spinifex
    for_nodes form form
    for_nodes start start_spinifex
    if [ "$NODES" -gt 1 ]; then
        done_step cluster || { form_cluster; mark_step cluster; }
        for i in "${!IPS[@]}"; do IP="${IPS[$i]}" NODE_NAME="node$((i + 1))"; install_key; done
        for_nodes pool configure_latitude_pool
    else
        for_nodes pool configure_pool
    fi
    for_nodes restart restart_spinifex
    IP="${IPS[0]}"
    done_step public || { public_default_subnets; mark_step public; }
    if [ -n "$IMAGE" ]; then done_step image || { import_image; mark_step image; }; fi
    write_profile
    log "ready: https://$IP:9999 (profile $PROFILE)"
    printf 'endpoint=https://%s:9999\nprofile=%s\nca=%s\nconsole=https://%s:3000/\n' "$IP" "$PROFILE" "$CA_FILE" "$IP"
    for i in "${!IPS[@]}"; do printf 'node%d=%s\n' "$((i + 1))" "${IPS[$i]}"; done
    printf 'elastic_ip=%s\n' "$(tf_output -json elastic_ips | python3 -I -c 'import json,sys; print(",".join(e["address"] for e in json.load(sys.stdin)))')"
}

# ── destroy ──────────────────────────────────────────────────────────────────

# lat_match KIND LIST-PATH FIELD REGEX prints "KIND id name" for each item whose FIELD fully matches.
lat_match() {
    lat_list "$2" | python3 -I -c '
import json,re,sys
kind, field, rx = sys.argv[1:]
for i in json.load(sys.stdin):
    v = i["attributes"].get(field) or ""
    if re.fullmatch(rx, v): print(kind, i["id"], v)' "$1" "$3" "$4"
}

# recorded_identity KIND ID answers whether a recorded object still carries this deployment's name:
# 0 yes, 1 it belongs to something else (an ID can be reused after a delete), 2 it cannot be read.
# Elastic IPs have no name, so they are not checked here.
recorded_identity() {
    local kind="$1" id="$2" field rx out
    case "$kind" in
        server) field=hostname rx="spx-$NAME-node[1-3]" ;;
        ssh-key|project) field=name rx="spinifex-$NAME" ;;
        user-data|virtual-network) field=description rx="spinifex-$NAME" ;;
        *) return 0 ;;
    esac
    out="$(latitude GET "$(kind_path "$kind")/$id")" || return 2
    printf '%s' "$out" | python3 -I -c '
import json, re, sys
field, rx = sys.argv[1:]
try:
    v = json.load(sys.stdin)["data"]["attributes"].get(field) or ""
except Exception:
    sys.exit(2)
sys.exit(0 if re.fullmatch(rx, v) else 1)' "$field" "$rx"
}

# check_recorded_identity stops a destroy before Terraform runs when a recorded object that still
# exists no longer carries this deployment's name, since Terraform would delete whatever has the ID.
check_recorded_identity() {
    local kind id rc
    [ -f "$KNOWN_IDS" ] || return 0
    while read -r kind id; do
        [ -n "$kind" ] || continue
        lat_exists "$(kind_path "$kind")/$id" && rc=0 || rc=$?
        case "$rc" in 0) ;; 1) continue ;; *) die "could not read $kind $id; nothing was destroyed" ;; esac
        recorded_identity "$kind" "$id" && rc=0 || rc=$?
        case "$rc" in
            0) ;;
            1) die "$kind $id is recorded for '$NAME' but now belongs to something else (the ID was reused); nothing was destroyed. Remove its line from $KNOWN_IDS and its entry from the Terraform state, then run destroy again" ;;
            *) die "could not read $kind $id; nothing was destroyed" ;;
        esac
    done < "$KNOWN_IDS"
}

# leftovers prints every billable object this deployment can own, one "kind id name" per line, and
# fails when anything could not be read. Every recorded ID is fetched by itself (2xx present, 404
# gone, else unknown); the name lists only add objects that are not recorded.
leftovers() {
    local kind id rc=0 named present same
    if [ -f "$KNOWN_IDS" ]; then
        while read -r kind id; do
            [ -n "$kind" ] || continue
            lat_exists "$(kind_path "$kind")/$id" && present=0 || present=$?
            case "$present" in
                0) recorded_identity "$kind" "$id" && same=0 || same=$?
                   case "$same" in
                       0) echo "$kind $id $id" ;;
                       1) log "recorded $kind $id now belongs to something else; it is not touched" ;;
                       *) echo "$kind $id $id (unknown)"; rc=1 ;;
                   esac ;;
                1) ;;
                *) echo "$kind $id $id (unknown)"; rc=1 ;;
            esac
        done < "$KNOWN_IDS"
    fi
    named="$({
        lat_match server /servers hostname "spx-$NAME-node[1-3]" || echo FAIL
        lat_match ssh-key /ssh_keys name "spinifex-$NAME" || echo FAIL
        lat_match user-data /user_data description "spinifex-$NAME" || echo FAIL
        lat_match virtual-network /virtual_networks description "spinifex-$NAME" || echo FAIL
        if [ -z "$PROJECT" ]; then lat_match project /projects name "spinifex-$NAME" || echo FAIL; fi
    })"
    if grep -qx FAIL <<<"$named"; then rc=1; named="$(grep -vx FAIL <<<"$named")"; fi
    [ -z "$named" ] || untracked "$named"
    return "$rc"
}

# untracked prints the lines of $1 whose kind and ID are not in the recorded objects.
untracked() {
    local kind id name
    while read -r kind id name; do
        [ -n "$kind" ] || continue
        grep -qx "$kind $id" "$KNOWN_IDS" 2>/dev/null || echo "$kind $id $name"
    done <<<"$1"
}

# ask PROMPT prints the prompt and echoes one line read from the terminal: empty without one.
ask() {
    local a=""
    printf '%s' "$1" >&2
    read -r a 2>/dev/null < /dev/tty || true
    printf '%s' "$a"
}

# delete_left LEFT APPROVED deletes the recorded or approved objects in a leftovers listing, and
# never one whose ownership could not be read: a failed lookup on a reused ID proves nothing.
delete_left() {
    local kind id name
    while read -r kind id name; do
        [ -n "$kind" ] || continue
        case "$name" in
            *"(unknown)") log "not deleting $kind $id: its ownership could not be verified"; continue ;;
        esac
        if ! grep -qx "$kind $id" "$KNOWN_IDS" 2>/dev/null && ! printf '%s' "$2" | grep -qxF "$kind $id"; then continue; fi
        log "deleting $kind $name"
        latitude DELETE "$(kind_path "$kind")/$id" >/dev/null || true
    done <<<"$1"
}

cmd_destroy() {
    local left kind id name i answer="" other fresh approved="" declined=""
    if [ "$YES" != 1 ]; then
        log "about to destroy deployment '$NAME':"
        leftovers >&2 || true
        read -r -p "Type the deployment name to confirm: " answer
        [ "$answer" = "$NAME" ] || die "not confirmed"
    fi
    if [ -f "$TF_STATE" ]; then
        "$TF_BIN" -chdir="$SCRIPT_DIR" init -input=false >/dev/null
        # Every ID is recorded before teardown, and a failure stops here: an Elastic IP cannot be found
        # again by name once its state entry is gone.
        record_ids || die "could not record the IDs in the Terraform state; nothing was destroyed"
        check_recorded_identity
        log "terraform destroy"
        tf destroy -input=false -auto-approve >"$STATE_DIR/destroy.log" 2>&1 || {
            tail -20 "$STATE_DIR/destroy.log" >&2
            die "terraform destroy failed; log in $STATE_DIR/destroy.log"
        }
    fi

    # What the state did not delete. Recorded objects go at once; objects that only match by name are
    # deleted only after their own typed confirmation, which --yes does not give.
    for i in 1 2 3 4 5 6; do
        left="$(leftovers)" || true
        [ -z "$left" ] && break
        other="$(untracked "$left")"
        # Only the IDs shown at a prompt are approved by it; an ID that turns up later needs its own.
        fresh=""
        while read -r kind id name; do
            [ -n "$kind" ] || continue
            if ! printf '%s\n%s\n' "$approved" "$declined" | grep -qxF "$kind $id"; then fresh+="$kind $id $name"$'\n'; fi
        done <<<"$other"
        if [ -n "$fresh" ]; then
            log "these match this deployment's names but are not recorded as made by it (they may belong to someone else):"
            printf '%s' "$fresh" >&2
            answer="$(ask 'Type "delete-untracked" to delete exactly these IDs: ')"
            while read -r kind id name; do
                [ -n "$kind" ] || continue
                if [ "$answer" = delete-untracked ]; then approved+="$kind $id"$'\n'; else declined+="$kind $id"$'\n'; fi
            done <<<"$fresh"
        fi
        delete_left "$left" "$approved"
        sleep 10
    done

    left="$(leftovers)" || { log "could not read everything that can remain; the records are kept"; printf '%s\n' "$left" >&2; exit 1; }
    if [ -n "$left" ]; then
        log "these still exist and may be billing (untracked ones are never deleted without their own confirmation):"
        printf '%s\n' "$left" >&2
        exit 1
    fi
    log "every recorded ID is gone, but listings cannot rule out Elastic IPs that were never recorded (their list ignores pagination)"
    answer="$(ask 'Check the latitude.sh dashboard for leftover servers, Elastic IPs, keys, user data and projects of this deployment, then type "checked": ')"
    [ "$answer" = checked ] || { log "no all-clear: the records are kept. Run destroy again after checking the dashboard"; exit 1; }
    rm -f "$STATE_DIR"/step.* "$KNOWN_IDS" "$STATE_DIR/project"
    log "deployment '$NAME' destroyed; the dashboard check was yours"
}

cmd_status() {
    local nodes
    if [ -f "$TF_STATE" ] && nodes="$(tf_output -json nodes 2>/dev/null)"; then
        python3 -I -c 'import json,sys; [print(n["name"], n["public_ip"]) for n in json.load(sys.stdin)]' <<<"$nodes"
    fi
    leftovers || true
    find "$STATE_DIR" -maxdepth 1 -name 'step.*' | sort | sed 's/.*step\./done: /'
}

# node_ip NODE prints the public address of nodeN from the Terraform state.
node_ip() {
    [ -f "$TF_STATE" ] || die "no deployment '$NAME'; run up first"
    local ip
    ip="$(node_field public_ip | sed -n "${1#node}p")"
    [ -n "$ip" ] || die "deployment '$NAME' has no $1"
    printf '%s' "$ip"
}

cmd_credentials() {
    IP="$(node_ip node1)"
    on "awk '/^\\[spinifex\\]/{f=1} /^\\[/&&!/^\\[spinifex\\]/{f=0} f' '$NODE_AWS_DIR/credentials'"
}

# cmd_power ACTION powers NODE_ARG off or on through the latitude.sh API. Power off is not a
# shutdown, so the node gets no chance to hand anything over first.
cmd_power() {
    [ -n "$NODE_ARG" ] || die "$CMD needs a node, such as node2"
    local id
    IP="$(node_ip "$NODE_ARG")"
    id="$(node_field provider_host_id | sed -n "${NODE_ARG#node}p")"
    latitude POST "/servers/$id/actions" "{\"data\":{\"type\":\"actions\",\"attributes\":{\"action\":\"$1\"}}}" >/dev/null ||
        die "latitude.sh refused to $1 $NODE_ARG"
    log "$NODE_ARG ($IP): $1 requested"
}

cmd_ssh() {
    IP="$(node_ip "${NODE_ARG:-node1}")"
    ssh -i "$SSH_KEY" -o IdentitiesOnly=yes -o ControlMaster=no -o ControlPath=none -o UserKnownHostsFile="$KNOWN_HOSTS" "ubuntu@$IP" "$@"
}

# A test sources the functions above without running a command.
[ "${BASH_SOURCE[0]}" = "$0" ] || return 0

case "$CMD" in
    up) cmd_up ;;
    destroy) cmd_destroy ;;
    status) cmd_status ;;
    credentials) cmd_credentials ;;
    ssh) cmd_ssh "$@" ;;
    stop) cmd_power power_off ;;
    start) cmd_power power_on ;;
    *) usage >&2; exit 2 ;;
esac
