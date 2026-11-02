#!/usr/bin/env bash
# deploy-vultr.sh — build a one- or three-node Spinifex deployment on Vultr, or tear it down.
#
#   deploy-vultr.sh up --channel latest                               one VX VM
#   deploy-vultr.sh up --channel latest --nodes 3 --plan vbm-8c-64gb-amd three bare-metal servers
#   deploy-vultr.sh status                 nodes, addresses and what the deployment holds
#   deploy-vultr.sh credentials            print the AWS access key and secret
#   deploy-vultr.sh ssh [nodeN] [command]  SSH to a node, node1 by default
#   deploy-vultr.sh stop nodeN             power a node off, as a failure would
#   deploy-vultr.sh start nodeN            power it back on
#   deploy-vultr.sh destroy
#
# VULTR_API_KEY must be set. OpenTofu, or Terraform when OpenTofu is not
# installed, creates the VPC, the nodes and any boot volumes; this script
# installs and forms Spinifex over SSH, and records each step under the state
# directory so a rerun resumes rather than re-initialising.
# Three nodes are formed by install-node.sh with cluster and Geneve traffic on
# the VPC, after each has come up as an armed single node.
#
# The API key is copied to the node over SSH, so it is in neither the Terraform
# state nor the instance metadata. Destroy also deletes what Terraform never
# created and Vultr keeps billing: the boot volume and every reserved IP the
# node allocated under the deployment's label prefix.
#
# Without --name the deployment is called "default" and its AWS profile is
# "spinifex"; a named deployment's profile is "spinifex-vultr-<name>".

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
SPINIFEX_SRC="$(cd "$SCRIPT_DIR/../../.." && pwd)"
VULTR_API="https://api.vultr.com/v2"

CMD="${1:-}"
[ $# -gt 0 ] && shift
NODE_ARG=""
NAME="default"
REGION="ord"
PLAN="vx1-g-16c-64g"
DISTRO=""
CHANNEL=""
SPX_REGION="us-east-1"
SPX_AZ="us-east-1a"
SSH_KEY="$HOME/.ssh/id_ed25519"
IMAGE=""
VPC_SUBNET="10.10.8.0"
STATE_ROOT="$HOME/.spinifex/vultr"
NODES=1
YES=0

usage() {
    sed -n '2,27p' "$0" | sed 's/^# \{0,1\}//'
    cat <<EOF

Options:
  --name NAME          deployment name (default $NAME); labels every resource
  --region ID          Vultr region (default $REGION)
  --plan ID            Vultr plan (default $PLAN); vbm-8c-64gb-amd for bare metal
  --distro TARBALL     install this local release tarball
  --channel CHANNEL    install from a release channel (latest, dev)
  --spx-region NAME    Spinifex region (default $SPX_REGION)
  --spx-az NAME        Spinifex availability zone (default $SPX_AZ)
  --ssh-key PATH       private key; PATH.pub is installed (default $SSH_KEY)
  --image NAME         also import this AMI, such as debian-13-x86_64 (default none)
  --vpc-subnet ADDR    the deployment VPC's /24 (default $VPC_SUBNET)
  --nodes N            1 or 3 (default $NODES)
  --yes                destroy without asking
EOF
}

while [ $# -gt 0 ]; do
    case "$1" in
        --name) NAME="$2"; shift ;;
        --region) REGION="$2"; shift ;;
        --plan) PLAN="$2"; shift ;;
        --distro) DISTRO="$2"; shift ;;
        --channel) CHANNEL="$2"; shift ;;
        --spx-region) SPX_REGION="$2"; shift ;;
        --spx-az) SPX_AZ="$2"; shift ;;
        --ssh-key) SSH_KEY="$2"; shift ;;
        --image) IMAGE="$2"; shift ;;
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

[[ "$NAME" =~ ^[a-z0-9][a-z0-9-]{0,23}$ ]] || die "--name must be 1-24 lowercase letters, digits or hyphens"
[ "$NODES" = 1 ] || [ "$NODES" = 3 ] || die "--nodes must be 1 or 3"
case "$CMD" in up|destroy|status|stop|start) [ -n "${VULTR_API_KEY:-}" ] || die "VULTR_API_KEY is not set" ;; esac
if command -v tofu >/dev/null; then TF_BIN=tofu
elif command -v terraform >/dev/null; then TF_BIN=terraform
else die "neither OpenTofu (tofu) nor Terraform is installed"; fi

STATE_DIR="$STATE_ROOT/$NAME"
TF_STATE="$STATE_DIR/terraform.tfstate"
KNOWN_HOSTS="$STATE_DIR/known_hosts"
LABEL_PREFIX="spinifex-vultr-$NAME"
PROFILE="spinifex-vultr-$NAME"
[ "$NAME" = default ] && PROFILE="spinifex"
CA_FILE="$HOME/.spinifex/vultr-$NAME-ca.pem"
mkdir -p "$STATE_DIR"
chmod 0700 "$STATE_DIR"

# vultr METHOD PATH [BODY] prints the response body and fails on a non-2xx.
vultr() {
    local out code
    out="$(curl -sS -w '\n%{http_code}' -X "$1" -H "Authorization: Bearer $VULTR_API_KEY" \
        -H 'Content-Type: application/json' ${3:+-d "$3"} "$VULTR_API$2")"
    code="${out##*$'\n'}"
    out="${out%$'\n'*}"
    case "$code" in
        2??) printf '%s' "$out" ;;
        *) log "Vultr $1 $2 returned $code: $out"; return 1 ;;
    esac
}

tf() {
    "$TF_BIN" -chdir="$SCRIPT_DIR" "$@" -state="$TF_STATE" \
        -var "deployment_name=$NAME" -var "region=$REGION" -var "plan=$PLAN" \
        -var "ssh_public_key_path=$SSH_KEY.pub" -var "vpc_subnet=$VPC_SUBNET" \
        -var "node_count=$NODES"
}

done_step() { [ -f "$STATE_DIR/step.$1" ]; }
mark_step() { date -u +%FT%TZ > "$STATE_DIR/step.$1"; }

# node_field FIELD prints FIELD of every node, in node order.
node_field() {
    "$TF_BIN" -chdir="$SCRIPT_DIR" output -state="$TF_STATE" -json nodes |
        python3 -c 'import json,sys; [print(n[sys.argv[1]]) for n in json.load(sys.stdin)]' "$1"
}

# for_nodes STEP FN runs FN once per node with IP and NODE_NAME set,
# recording STEP.<node> so a rerun skips the nodes already done.
for_nodes() {
    local step="$1" fn="$2" i
    for i in "${!IPS[@]}"; do
        IP="${IPS[$i]}" NODE_NAME="node$((i + 1))"
        done_step "$step.$NODE_NAME" && continue
        "$fn"
        mark_step "$step.$NODE_NAME"
    done
}

# vpc_addr prints the VPC NIC's name and address on the current node.
vpc_addr() { on 'ip -4 -o addr show' | awk -v p="${VPC_SUBNET%.*}." 'index($4, p) == 1 {sub(/\/.*/, "", $4); print $2, $4; exit}'; }

on() {
    ssh -i "$SSH_KEY" -o BatchMode=yes -o ConnectTimeout=10 -o StrictHostKeyChecking=accept-new \
        -o UserKnownHostsFile="$KNOWN_HOSTS" "root@$IP" "$@"
}

# ── up ───────────────────────────────────────────────────────────────────────

preflight() {
    [ -r "$SSH_KEY" ] && [ -r "$SSH_KEY.pub" ] || die "SSH key $SSH_KEY (and .pub) not readable"
    if [ -n "$DISTRO" ]; then
        [ -r "$DISTRO" ] || die "--distro $DISTRO not readable"
    elif [ -z "$CHANNEL" ]; then
        die "pass --distro TARBALL or --channel latest|dev"
    fi
    vultr GET /account >/dev/null || die "the API key was refused; check its IP allowlist"
    local type=vx1
    [[ "$PLAN" == vbm-* ]] && type=vbm
    vultr GET "/regions/$REGION/availability?type=$type" | python3 -c '
import json,sys
plan=sys.argv[1]
if plan not in json.load(sys.stdin).get("available_plans",[]):
    sys.exit("plan %s is not available in this region" % plan)' "$PLAN" || die "plan check failed"
}

provision() {
    log "terraform apply ($PLAN in $REGION)"
    "$TF_BIN" -chdir="$SCRIPT_DIR" init -input=false >/dev/null
    tf apply -input=false -auto-approve >"$STATE_DIR/apply.log" 2>&1 || {
        tail -20 "$STATE_DIR/apply.log" >&2
        die "terraform apply failed; log in $STATE_DIR/apply.log"
    }
}

# wait_host pins the host key only once the marker cloud-init writes exists:
# bare metal first answers SSH from Vultr's installer, with a key of its own.
wait_host() {
    log "waiting for SSH and cloud-init on $IP"
    local i
    for i in $(seq 1 120); do
        ssh -i "$SSH_KEY" -o BatchMode=yes -o ConnectTimeout=10 -o StrictHostKeyChecking=no \
            -o UserKnownHostsFile=/dev/null -o LogLevel=ERROR "root@$IP" \
            'test -f /var/lib/cloud/spinifex-host-ready' 2>/dev/null && break
        [ "$i" = 120 ] && die "host $IP was not ready after 20 minutes"
        sleep 10
    done
    ssh-keygen -R "$IP" -f "$KNOWN_HOSTS" >/dev/null 2>&1 || true
    on 'cloud-init status --wait >/dev/null; test -e /dev/kvm' || die "no /dev/kvm on $IP"
    # Vultr's image writes a static resolv.conf, so the node would bypass the
    # split DNS Spinifex sets in resolved for its LB and EKS names.
    on 'if [ ! -L /etc/resolv.conf ] && systemctl is-active -q systemd-resolved; then
            ln -sf ../run/systemd/resolve/stub-resolv.conf /etc/resolv.conf; fi' ||
        die "could not point /etc/resolv.conf at systemd-resolved on $IP"
}

install_spinifex() {
    log "installing Spinifex"
    if [ -n "$DISTRO" ]; then
        scp -q -i "$SSH_KEY" -o BatchMode=yes -o UserKnownHostsFile="$KNOWN_HOSTS" \
            "$DISTRO" "$SPINIFEX_SRC/scripts/setup.sh" "root@$IP:/tmp/" || die "copy to $IP failed"
        on "INSTALL_SPINIFEX_TARBALL=/tmp/$(basename "$DISTRO") bash /tmp/setup.sh >/root/setup.log 2>&1" ||
            die "setup.sh failed; see /root/setup.log on $IP"
        # A tarball built by build-install-artifacts.sh ships setup.sh beside it
        # rather than in it, and the later steps run the installed copy.
        on 'test -x /usr/local/share/spinifex/setup.sh || install -D -m 0755 /tmp/setup.sh /usr/local/share/spinifex/setup.sh'
    else
        on "curl -sfL https://install.mulgadc.com | bash -s -- --channel $CHANNEL >/root/setup.log 2>&1" ||
            die "install failed; see /root/setup.log on $IP"
    fi
}

# form brings a node up as its own one-node cluster, the state an ISO install
# leaves and the one install-node.sh forms a larger cluster from.
form() {
    local vpc_if vpc_ip
    read -r vpc_if vpc_ip <<<"$(vpc_addr)"
    [ -n "$vpc_ip" ] || die "no VPC address on $IP"
    log "initialising $NODE_NAME on $IP (VPC $vpc_if $vpc_ip, region $SPX_REGION)"
    on "/usr/local/share/spinifex/setup-ovn.sh --management --nat-uplink --encap-ip=$vpc_ip >/root/setup-ovn.log 2>&1" ||
        die "setup-ovn.sh failed; see /root/setup-ovn.log on $IP"
    on "spx admin init --node $NODE_NAME --nodes 1 --region $SPX_REGION --az $SPX_AZ --ipsec=false --external-mode=nat >/root/init.log 2>&1" ||
        die "spx admin init failed; see /root/init.log on $IP"
}

install_key() {
    log "installing the API key on $NODE_NAME"
    printf '%s' "$VULTR_API_KEY" | on 'install -d -m 0700 -o spinifex-daemon -g spinifex /etc/spinifex/vultr &&
        umask 077 && cat > /etc/spinifex/vultr/api-key &&
        chown spinifex-daemon:spinifex /etc/spinifex/vultr/api-key && chmod 0600 /etc/spinifex/vultr/api-key'
    on 'curl -sf -o /dev/null -H "Authorization: Bearer $(cat /etc/spinifex/vultr/api-key)" https://api.vultr.com/v2/reserved-ips' ||
        die "the node's API calls are refused; add $IP to the key's allowlist"
}

# configure_pool adds the pool and every node's instance ID: the config is
# validated against all [nodes.*] tables, and a takeover attaches to the
# taking node's own ID.
configure_pool() {
    log "adding the vultr pool on $NODE_NAME (label prefix $LABEL_PREFIX)"
    local ids="" i
    for i in "${!IDS[@]}"; do ids+="node$((i + 1))=${IDS[$i]},"; done
    on "python3 - '$REGION' '$LABEL_PREFIX' '${ids%,}'" <<'PY'
import re, sys
region, prefix, ids = sys.argv[1:]
p = "/etc/spinifex/spinifex.toml"
s = open(p).read()
if 'source       = "vultr"' not in s:
    pool = ('[[network.external_pools]]\nname         = "vultr"\nsource       = "vultr"\n'
            f'vultr_region = "{region}"\nlabel_prefix = "{prefix}"\n\n[bootstrap]')
    assert s.count("\n[bootstrap]") == 1, "expected one [bootstrap] table"
    s = s.replace("\n[bootstrap]", "\n" + pool, 1)
for pair in ids.split(","):
    node, host_id = pair.split("=")
    head = "[nodes.%s]\n" % node
    m = re.search(r"^" + re.escape(head) + r"(.*?)(?=^\[|\Z)", s, flags=re.M | re.S)
    assert m, "could not find [nodes.%s]" % node
    if "provider_host_id" not in m.group(1):
        s = s[:m.start()] + head + 'provider_host_id = "%s"\n' % host_id + s[m.start() + len(head):]
open(p, "w").write(s)
PY
}

start_spinifex() {
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

# form_cluster hands the armed single nodes to install-node.sh. It dials the
# public addresses over SSH and is told to put the cluster and Geneve planes on
# the VPC NIC, which must have the same name on every node.
form_cluster() {
    local hosts_file="$STATE_DIR/hosts" vpc_if="" if_here ip_here pinned
    printf '%s\n' "${IPS[@]}" > "$hosts_file"
    for IP in "${IPS[@]}"; do
        read -r if_here ip_here <<<"$(vpc_addr)"
        [ -n "$ip_here" ] || die "no VPC address on $IP"
        [ -z "$vpc_if" ] || [ "$vpc_if" = "$if_here" ] || die "VPC NIC is $vpc_if on one node and $if_here on $IP"
        vpc_if="$if_here"
        # install-node.sh reads ~/.ssh/known_hosts and Vultr reuses addresses, so
        # replace any older key for this one with the key this deployment pinned.
        # -F, not grep: ssh_config usually hashes the entries.
        pinned="$(ssh-keygen -F "$IP" -f "$KNOWN_HOSTS" | grep -v '^#')" || die "no pinned host key for $IP in $KNOWN_HOSTS"
        ssh-keygen -R "$IP" >/dev/null 2>&1 || true
        printf '%s\n' "$pinned" >> "$HOME/.ssh/known_hosts"
    done
    log "forming ${#IPS[@]} nodes with install-node.sh (cluster plane $vpc_if)"
    "$SPINIFEX_SRC/scripts/install-node.sh" --hosts-file "$hosts_file" --user root --identity "$SSH_KEY" \
        --external-mode nat --ipsec off --region "$SPX_REGION" --az "$SPX_AZ" \
        --lan-bridge "$vpc_if" --vpc-bridge "$vpc_if" --yes >"$STATE_DIR/form.log" 2>&1 ||
        die "formation failed; see $STATE_DIR/form.log"
}

restart_spinifex() {
    log "restarting $NODE_NAME with the pool"
    on 'systemctl restart spinifex.target'
    wait_api
    local i
    for i in $(seq 1 30); do
        on 'journalctl -u spinifex-daemon --since "-10min" --no-pager | grep -q "floating using Vultr reserved IPs"' && return 0
        sleep 10
    done
    die "$NODE_NAME never logged its Vultr allocator; see journalctl -u spinifex-daemon on $IP"
}

import_image() {
    log "importing $IMAGE"
    on "spx admin images import --name $IMAGE --config /etc/spinifex/spinifex.toml >/root/image-import.log 2>&1" ||
        die "image import failed; see /root/image-import.log on $IP"
}

write_profile() {
    log "writing the CA to $CA_FILE and AWS profile $PROFILE"
    mkdir -p "$(dirname "$CA_FILE")"
    on 'cat /etc/spinifex/ca.pem' > "$CA_FILE"
    local creds key secret
    creds="$(on 'awk "/^\[spinifex\]/{f=1;next} /^\[/{f=0} f" ~/.aws/credentials')"
    key="$(awk -F' *= *' '/aws_access_key_id/{print $2}' <<<"$creds")"
    secret="$(awk -F' *= *' '/aws_secret_access_key/{print $2}' <<<"$creds")"
    [ -n "$key" ] && [ -n "$secret" ] || die "no [spinifex] credentials on $IP"
    aws configure set --profile "$PROFILE" aws_access_key_id "$key"
    aws configure set --profile "$PROFILE" aws_secret_access_key "$secret"
    aws configure set --profile "$PROFILE" region "$SPX_REGION"
    aws configure set --profile "$PROFILE" endpoint_url "https://$IP:9999"
    aws configure set --profile "$PROFILE" ca_bundle "$CA_FILE"
}

cmd_up() {
    preflight
    done_step terraform || { provision; mark_step terraform; }
    mapfile -t IPS < <(node_field public_ip)
    mapfile -t IDS < <(node_field provider_host_id)
    [ "${#IPS[@]}" = "$NODES" ] || die "the state holds ${#IPS[@]} node(s), not $NODES; destroy it or pass --nodes ${#IPS[@]}"
    for_nodes host wait_host
    for_nodes install install_spinifex
    for_nodes form form
    for_nodes start start_spinifex
    if [ "$NODES" -gt 1 ]; then
        done_step cluster || { form_cluster; mark_step cluster; }
    fi
    for_nodes key install_key
    for_nodes pool configure_pool
    for_nodes restart restart_spinifex
    IP="${IPS[0]}"
    if [ -n "$IMAGE" ]; then done_step image || { import_image; mark_step image; }; fi
    write_profile
    log "ready: https://$IP:9999 (profile $PROFILE)"
    printf 'endpoint=https://%s:9999\nprofile=%s\nca=%s\nconsole=https://%s:3000/\n' "$IP" "$PROFILE" "$CA_FILE" "$IP"
    local i
    for i in "${!IPS[@]}"; do printf 'node%d=%s\n' "$((i + 1))" "${IPS[$i]}"; done
    printf 'api_key_allowlist=%s\n' "$(printf '%s/32,' "${IPS[@]}" | sed 's/,$//')"
}

# ── destroy ──────────────────────────────────────────────────────────────────

# leftovers prints every billable object this deployment can own, one per line.
leftovers() {
    {
        vultr GET "/instances?per_page=500" | python3 -c '
import json,sys
for i in json.load(sys.stdin)["instances"]:
    if "deployment:"+sys.argv[1] in i.get("tags",[]): print("instance", i["id"], i["label"])' "$NAME"
        vultr GET "/bare-metals?per_page=500" | python3 -c '
import json,sys
for i in json.load(sys.stdin)["bare_metals"]:
    if "deployment:"+sys.argv[1] in i.get("tags",[]): print("bare-metal", i["id"], i["label"])' "$NAME"
        vultr GET "/blocks?per_page=500" | python3 -c '
import json,sys
for b in json.load(sys.stdin)["blocks"]:
    if b["label"].startswith("spinifex-"+sys.argv[1]+"-"): print("block", b["id"], b["label"])' "$NAME"
        vultr GET "/reserved-ips?per_page=500" | python3 -c '
import json,sys
for r in json.load(sys.stdin)["reserved_ips"]:
    if r["label"].startswith(sys.argv[1]+":"): print("reserved-ip", r["id"], r["subnet"])' "$LABEL_PREFIX"
        vultr GET "/vpcs?per_page=500" | python3 -c '
import json,sys
for v in json.load(sys.stdin)["vpcs"]:
    if v["description"] == "spinifex-"+sys.argv[1]: print("vpc", v["id"], v["description"])' "$NAME"
    }
}

cmd_destroy() {
    if [ "$YES" != 1 ]; then
        log "about to destroy deployment '$NAME':"
        leftovers >&2
        read -r -p "Type the deployment name to confirm: " answer
        [ "$answer" = "$NAME" ] || die "not confirmed"
    fi
    if [ -f "$TF_STATE" ]; then
        log "terraform destroy"
        "$TF_BIN" -chdir="$SCRIPT_DIR" init -input=false >/dev/null
        tf destroy -input=false -auto-approve >"$STATE_DIR/destroy.log" 2>&1 || {
            tail -20 "$STATE_DIR/destroy.log" >&2
            die "terraform destroy failed; log in $STATE_DIR/destroy.log"
        }
    fi

    # Reserved IPs the node created, and boot volumes the instance left behind.
    local kind id label i
    for i in 1 2 3 4 5 6; do
        local left
        left="$(leftovers)"
        [ -z "$left" ] && break
        while read -r kind id label; do
            case "$kind" in
                reserved-ip)
                    log "deleting reserved IP $label"
                    vultr POST "/reserved-ips/$id/detach" >/dev/null || true
                    vultr DELETE "/reserved-ips/$id" >/dev/null || true ;;
                block)
                    log "deleting block volume $label"
                    vultr DELETE "/blocks/$id" >/dev/null || true ;;
                instance|bare-metal|vpc)
                    log "deleting $kind $label, still listed after terraform destroy"
                    vultr DELETE "/${kind}s/$id" >/dev/null || true ;;
            esac
        done <<<"$left"
        sleep 10
    done

    local left
    left="$(leftovers)"
    if [ -n "$left" ]; then
        log "these still exist and are billing:"
        printf '%s\n' "$left" >&2
        exit 1
    fi
    rm -f "$STATE_DIR"/step.*
    log "deployment '$NAME' destroyed; nothing of it is left in the account"
}

cmd_status() {
    local nodes
    if [ -f "$TF_STATE" ] && nodes="$("$TF_BIN" -chdir="$SCRIPT_DIR" output -state="$TF_STATE" -json nodes 2>/dev/null)"; then
        python3 -c 'import json,sys; [print(n["name"], n["public_ip"]) for n in json.load(sys.stdin)]' <<<"$nodes"
    fi
    leftovers
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
    on 'awk "/^\[spinifex\]/{f=1} /^\[/&&!/^\[spinifex\]/{f=0} f" ~/.aws/credentials'
}

# cmd_power ACTION halts or starts NODE_ARG through the Vultr API. A halt is a
# hard power-off, so the node gets no chance to hand anything over first.
cmd_power() {
    [ -n "$NODE_ARG" ] || die "$CMD needs a node, such as node2"
    local id path
    IP="$(node_ip "$NODE_ARG")"
    id="$(node_field provider_host_id | sed -n "${NODE_ARG#node}p")"
    path="/instances/$id"
    "$TF_BIN" -chdir="$SCRIPT_DIR" state list -state="$TF_STATE" | grep -q '^vultr_bare_metal_server\.' &&
        path="/bare-metals/$id"
    vultr POST "$path/$1" >/dev/null || die "Vultr refused to $1 $NODE_ARG"
    log "$NODE_ARG ($IP): $1 requested"
}

cmd_ssh() {
    IP="$(node_ip "${NODE_ARG:-node1}")"
    ssh -i "$SSH_KEY" -o UserKnownHostsFile="$KNOWN_HOSTS" "root@$IP" "$@"
}

case "$CMD" in
    up) cmd_up ;;
    destroy) cmd_destroy ;;
    status) cmd_status ;;
    credentials) cmd_credentials ;;
    ssh) cmd_ssh "$@" ;;
    stop) cmd_power halt ;;
    start) cmd_power start ;;
    *) usage >&2; exit 2 ;;
esac
