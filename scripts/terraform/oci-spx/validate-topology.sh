#!/bin/bash
# Builds one OCI topology from nothing, proves Spinifex works on it, and destroys
# it again. Three topologies, because each breaks differently: bare metal presents
# VNICs unlike a VM, a single node has no Geneve underlay to get wrong, and only a
# cluster exercises RAFT, the gateway chassis and cross-node allocation.
#
# Every run is build-prove-destroy. A topology that cannot be torn down is half
# proved, so the teardown decides the verdict and runs even on failure.
set -euo pipefail

HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "$HERE/../../.." && pwd)"

TOPOLOGY=""
SSH_PUBLIC_KEY="$HOME/.ssh/oci-spx.pub"
SSH_PRIVATE_KEY="$HOME/.ssh/oci-spx"
KEEP=0
SKIP_WORKLOAD=0
SKIP_POOL=0
DRY_RUN=0
INSTANCE_PRINCIPAL=0
WORKBOOK="nginx-alb"

# Shapes and counts per topology. Named here rather than passed in, because the
# point of a topology is that it is the same every run.
declare -A TOPO_SHAPE=(
    [bm]="BM.Standard.E2.64"
    [vm-single]="VM.Standard.E6.Flex"
    [vm-multi]="VM.Standard.E6.Flex"
)
declare -A TOPO_NODES=(
    [bm]=1
    [vm-single]=1
    [vm-multi]=3
)

log() { printf '[validate-%s] %s\n' "${TOPOLOGY:-?}" "$*"; }
die() {
    printf '[validate-%s] ERROR: %s\n' "${TOPOLOGY:-?}" "$*" >&2
    exit 1
}

usage() {
    cat >&2 <<EOF
usage: ${0##*/} --topology <bm|vm-single|vm-multi> [options]

Builds the topology, installs Spinifex, forms the cluster, runs a Terraform
workbook against it, then destroys everything.

  --topology <name>       Required. No default: a command aimed at the wrong
                          topology is the easiest expensive mistake here.
  --ssh-public-key PATH   Public half installed on each node. Default $SSH_PUBLIC_KEY
  --ssh-private-key PATH  Private half used to reach them. Default $SSH_PRIVATE_KEY
  --instance-principal    Configure the pool with oci_auth="instance_principal"
                          instead of a key file. Needs the dynamic group and
                          policy to exist already; see instance-principal.tf.
  --workbook NAME         Workbook under docs/terraform-workbooks. Default $WORKBOOK
  --no-external-pool      Form without an OCI credential: no external pool and no
                          allocator gate. Implies --skip-workload, because a guest
                          with no public address proves nothing a customer wants.
  --skip-workload         Form and verify only; launch no guests.
  --keep                  Leave the infrastructure up. Implies no verdict.
  --dry-run               Print the plan and the per-node steps, change nothing.
EOF
    exit 2
}

while [ $# -gt 0 ]; do
    case "$1" in
        --topology) TOPOLOGY="${2:?}"; shift 2 ;;
        --ssh-public-key) SSH_PUBLIC_KEY="${2:?}"; shift 2 ;;
        --ssh-private-key) SSH_PRIVATE_KEY="${2:?}"; shift 2 ;;
        --workbook) WORKBOOK="${2:?}"; shift 2 ;;
        --instance-principal) INSTANCE_PRINCIPAL=1; shift ;;
        --no-external-pool) SKIP_POOL=1; SKIP_WORKLOAD=1; shift ;;
        --skip-workload) SKIP_WORKLOAD=1; shift ;;
        --keep) KEEP=1; shift ;;
        --dry-run) DRY_RUN=1; shift ;;
        -h | --help) usage ;;
        *) die "unknown option $1" ;;
    esac
done

[ -n "$TOPOLOGY" ] || usage
[ -n "${TOPO_SHAPE[$TOPOLOGY]:-}" ] || die "unknown topology $TOPOLOGY; one of: ${!TOPO_SHAPE[*]}"
[ -r "$SSH_PUBLIC_KEY" ] || die "no readable SSH public key at $SSH_PUBLIC_KEY"
[ -r "$SSH_PRIVATE_KEY" ] || die "no readable SSH private key at $SSH_PRIVATE_KEY"

SHAPE="${TOPO_SHAPE[$TOPOLOGY]}"
NODES="${TOPO_NODES[$TOPOLOGY]}"
STATE_DIR="$HERE/.validate-$TOPOLOGY"
RESULTS="$STATE_DIR/results.txt"

# A separate state directory per topology, so two topologies can be built from one
# checkout without one destroying the other's instances.
tf() {
    python3 "$HERE/scripts/oci_env.py" --ssh-public-key-path "$SSH_PUBLIC_KEY" -- \
        terraform -chdir="$HERE" "$@"
}

tf_vars=(
    -var "compute_shape=$SHAPE"
    -var "node_count=$NODES"
    -var "enable_instance_principal=$([ "$INSTANCE_PRINCIPAL" = 1 ] && echo true || echo false)"
    -state "$STATE_DIR/terraform.tfstate"
)

ssh_node() {
    local host="$1"
    shift
    ssh -i "$SSH_PRIVATE_KEY" -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null \
        -o BatchMode=yes -o ConnectTimeout=15 "ubuntu@$host" "$@"
}

record() { printf '%s\n' "$*" >> "$RESULTS"; }

# Teardown is the last assertion, not cleanup: a workbook or a topology that
# cannot be destroyed is a defect, and it has been one before.
DESTROY_RC=""
cleanup() {
    local rc=$?
    if [ "$KEEP" = 1 ]; then
        log "--keep: leaving the infrastructure up, no verdict recorded"
        return
    fi
    [ "$DRY_RUN" = 1 ] && return
    log "destroying"
    if tf destroy -auto-approve -no-color "${tf_vars[@]}" > "$STATE_DIR/destroy.log" 2>&1; then
        DESTROY_RC=0
        record "destroy: PASS"
    else
        DESTROY_RC=1
        record "destroy: FAIL (see $STATE_DIR/destroy.log)"
        log "TEARDOWN FAILED — resources may still be billing. $STATE_DIR/destroy.log"
    fi
    verdict "$rc"
}

verdict() {
    local run_rc="$1"
    echo
    log "=== $TOPOLOGY ($SHAPE, $NODES node(s)) ==="
    [ -r "$RESULTS" ] && cat "$RESULTS"
    if [ "$run_rc" = 0 ] && [ "$DESTROY_RC" = 0 ]; then
        log "PASS"
    else
        log "FAIL"
    fi
}

mkdir -p "$STATE_DIR"
: > "$RESULTS"

log "$SHAPE, $NODES node(s), state in $STATE_DIR"

if [ "$DRY_RUN" = 1 ]; then
    tf plan -no-color "${tf_vars[@]}" | tail -30
    log "dry run: would install Spinifex on $NODES node(s), form the cluster,$([ "$SKIP_POOL" = 1 ] && echo " configure no external pool,") $([ "$SKIP_WORKLOAD" = 1 ] && echo "launch no guests" || echo "run the $WORKBOOK workbook"), then destroy"
    exit 0
fi

trap cleanup EXIT

tf init -input=false > "$STATE_DIR/init.log" 2>&1 || die "terraform init failed; see $STATE_DIR/init.log"

log "building"
tf apply -auto-approve -no-color "${tf_vars[@]}" > "$STATE_DIR/apply.log" 2>&1 \
    || die "terraform apply failed; see $STATE_DIR/apply.log"
record "build: PASS"

mapfile -t HOSTS < <(tf output -raw "${tf_vars[@]}" hosts_file 2>/dev/null || tf output -raw hosts_file)
[ "${#HOSTS[@]}" = "$NODES" ] || die "expected $NODES host(s) from the hosts_file output, got ${#HOSTS[@]}"
log "hosts: ${HOSTS[*]}"

# cloud-init owns the volume, the ports and br-wan, and all three are units now, so
# the question is whether they converged rather than whether a runcmd happened to
# run. This is the gate the bare-metal findings were about.
log "waiting for cloud-init on each node"
for host in "${HOSTS[@]}"; do
    for _ in $(seq 1 120); do
        if ssh_node "$host" 'cloud-init status --wait >/dev/null 2>&1 || true; test -f /var/lib/cloud/instance/boot-finished' 2>/dev/null; then
            break
        fi
        sleep 10
    done
    ssh_node "$host" '
        set -e
        for u in spinifex-data-volume spinifex-service-ports spinifex-wan-bridge; do
            printf "%s=%s " "$u" "$(systemctl is-active "$u" 2>&1)"
        done
        echo
        findmnt --noheadings --output SOURCE,TARGET /var/lib/spinifex
        ip -br addr show br-wan
    ' > "$STATE_DIR/cloudinit-$host.txt" 2>&1 \
        || die "cloud-init did not converge on $host; see $STATE_DIR/cloudinit-$host.txt"
    log "$host cloud-init: $(head -1 "$STATE_DIR/cloudinit-$host.txt")"
done
record "cloud-init units: PASS"

# The published installer rather than a build of this tree, so what is proved is
# what a customer following the guide gets. update-nodes.sh would deploy the
# working tree, which is a different question.
log "installing Spinifex on each node"
for host in "${HOSTS[@]}"; do
    ssh_node "$host" '
        set -e
        curl -sfL https://install.mulgadc.com | sudo bash
        sudo /usr/local/share/spinifex/setup-ovn.sh --management --nat-uplink
    ' > "$STATE_DIR/install-$host.log" 2>&1 \
        || die "install failed on $host; see $STATE_DIR/install-$host.log"
    log "$host installed: $(ssh_node "$host" 'spx version' 2>&1 | head -1)"
done
record "install: PASS"

# install-node.sh owns formation, including the join for a cluster, so a topology
# difference is a host count here rather than a second code path.
printf '%s\n' "${HOSTS[@]}" > "$STATE_DIR/hosts"
log "forming the cluster"
"$REPO_ROOT/scripts/install-node.sh" \
    --hosts-file "$STATE_DIR/hosts" \
    --user ubuntu \
    --identity "$SSH_PRIVATE_KEY" \
    --external-mode nat \
    --ipsec off \
    --yes > "$STATE_DIR/form.log" 2>&1 \
    || die "formation failed; see $STATE_DIR/form.log"
record "formation: PASS"

# The IMDS remap and the pool are both set-before-first-start, and the remap is the
# half that is invisible when missing: without it the cloud's metadata service is
# shadowed by Spinifex's own endpoints and the allocator cannot resolve its VNIC.
log "configuring the IMDS remap$([ "$SKIP_POOL" = 1 ] && echo " (no external pool)" || echo " and the external pool")"
for host in "${HOSTS[@]}"; do
    ssh_node "$host" "SKIP_POOL=$SKIP_POOL bash -s" > "$STATE_DIR/pool-$host.log" 2>&1 <<'REMOTE' \
        || die "pool configuration failed on $host; see $STATE_DIR/pool-$host.log"
set -e
sudo cp /etc/spinifex/spinifex.toml /etc/spinifex/spinifex.toml.bak-prepool
grep -q imds_host_meta_ip /etc/spinifex/spinifex.toml || sudo python3 - <<"PY"
import re
path = "/etc/spinifex/spinifex.toml"
text = open(path).read()
text = re.sub(r"(?m)^\[network\]$",
              "[network]\nimds_host_meta_ip = \"169.254.42.254\"\nimds_host_dns_ip  = \"169.254.42.253\"",
              text, count=1)
open(path, "w").write(text)
PY
if [ "${SKIP_POOL:-0}" != 1 ]; then
    grep -q 'name               = "oci-public"' /etc/spinifex/spinifex.toml \
        || sudo tee -a /etc/spinifex/spinifex.toml < /etc/spinifex/oci/external-pool.toml >/dev/null
fi
sudo spx config validate --config /etc/spinifex/spinifex.toml 2>/dev/null || true
sudo systemctl restart spinifex.target
REMOTE
done

# resolved the external VNIC is the line that proves the credential works, the
# compartment is right and br-wan's MAC matched a real VNIC. A node missing it
# accepts allocate-address and then fails it.
if [ "$SKIP_POOL" = 1 ]; then
    log "--no-external-pool: no allocator to check"
    record "oci allocator: SKIPPED"
else
    log "checking the allocator came up"
    for host in "${HOSTS[@]}"; do
        found=0
        for _ in $(seq 1 30); do
            if ssh_node "$host" "sudo journalctl -u spinifex-vpcd --since -10min --no-pager | grep -q 'resolved the external VNIC'" 2>/dev/null; then
                found=1
                break
            fi
            sleep 10
        done
        [ "$found" = 1 ] || die "$host never logged 'resolved the external VNIC'; the OCI allocator is not up, so no guest can get a public address"
        log "$host allocator ready"
    done
    record "oci allocator: PASS"
fi

log "cluster membership"
ssh_node "${HOSTS[0]}" 'sudo spx get nodes' | tee "$STATE_DIR/nodes.txt"
ready="$(grep -c 'Ready' "$STATE_DIR/nodes.txt" || true)"
[ "$ready" = "$NODES" ] || die "expected $NODES Ready node(s), got $ready"
record "membership: PASS ($ready Ready)"

if [ "$SKIP_WORKLOAD" = 1 ]; then
    log "--skip-workload: stopping before the workbook"
    record "workbook: SKIPPED"
    exit 0
fi

# Run from the node, against 127.0.0.1, because the node certificate carries no SAN
# for its public address -- a workbook driven from outside the VCN is still blocked.
log "running the $WORKBOOK workbook"
WB_SRC="$REPO_ROOT/docs/terraform-workbooks/$WORKBOOK"
[ -d "$WB_SRC" ] || die "no workbook at $WB_SRC"
scp -i "$SSH_PRIVATE_KEY" -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null -q \
    -r "$WB_SRC" "ubuntu@${HOSTS[0]}:~/workbook" || die "could not copy the workbook"

ssh_node "${HOSTS[0]}" '
    set -e
    command -v terraform >/dev/null || {
        wget -qO /tmp/tf.zip https://releases.hashicorp.com/terraform/1.13.3/terraform_1.13.3_linux_amd64.zip
        sudo apt-get install -y -qq unzip >/dev/null 2>&1 || true
        sudo unzip -o -q /tmp/tf.zip -d /usr/local/bin
    }
    sudo spx admin images import --name ubuntu-26.04-x86_64 --config /etc/spinifex/spinifex.toml >/dev/null
    cd ~/workbook
    export AWS_PROFILE=spinifex AWS_CA_BUNDLE=/etc/spinifex/ca.pem
    terraform init -no-color >/dev/null
    terraform apply -no-color -auto-approve
' > "$STATE_DIR/workbook.log" 2>&1 || die "the $WORKBOOK workbook failed; see $STATE_DIR/workbook.log"
record "workbook $WORKBOOK: PASS"

# The workbook creating cleanly says nothing about whether it serves traffic, which
# is the only thing a customer notices.
log "proving the workbook serves traffic"
ssh_node "${HOSTS[0]}" '
    set -e
    export AWS_PROFILE=spinifex AWS_CA_BUNDLE=/etc/spinifex/ca.pem
    E="--endpoint-url https://127.0.0.1:9999 --region ap-southeast-2"
    IP=$(aws elbv2 describe-load-balancers $E --names nginx-alb \
        --query "LoadBalancers[0].AvailabilityZones[].LoadBalancerAddresses[].IpAddress" --output text | head -1)
    [ -n "$IP" ] || { echo "the load balancer reported no address"; exit 1; }
    echo "load balancer address: $IP"
    for i in $(seq 1 10); do curl -sS -o /dev/null -w "%{http_code}\n" --max-time 10 "http://$IP/"; done | sort | uniq -c
' > "$STATE_DIR/traffic.log" 2>&1 || die "the workbook created but did not serve traffic; see $STATE_DIR/traffic.log"
grep -q ' 200$' "$STATE_DIR/traffic.log" || die "no HTTP 200 through the load balancer; see $STATE_DIR/traffic.log"
record "traffic: PASS"

log "destroying the workbook before the topology, so a leak is attributed to the right one"
ssh_node "${HOSTS[0]}" '
    cd ~/workbook
    export AWS_PROFILE=spinifex AWS_CA_BUNDLE=/etc/spinifex/ca.pem
    terraform destroy -no-color -auto-approve
' > "$STATE_DIR/workbook-destroy.log" 2>&1 || die "the workbook would not destroy; see $STATE_DIR/workbook-destroy.log"
record "workbook teardown: PASS"
