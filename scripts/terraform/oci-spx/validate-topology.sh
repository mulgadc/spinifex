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
KEEP_ON_FAIL=0
SKIP_WORKLOAD=0
SKIP_POOL=0
DRY_RUN=0
DESTROY_ONLY=0
INSTANCE_PRINCIPAL=0
DISTRO=""
SETUP_SH=""
# Empty means the driver's own default list. Unset is distinguishable from empty,
# so --workbooks "" can deliberately mean "run none".
WORKBOOKS_SET=0
WORKBOOKS=""

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
  --workbooks LIST        Space-separated workbooks for the published driver to
                          run on the cluster. Empty means run none; omitted means
                          the driver's own default list.
  --distro PATH           Install this distro tarball instead of the published
                          release, so what is proved is the ref it was built from.
  --setup-sh PATH         setup.sh to pair with --distro. Both or neither.
  --no-external-pool      Form without an OCI credential: no external pool and no
                          allocator gate. Implies --skip-workload, because a guest
                          with no public address proves nothing a customer wants.
  --skip-workload         Form and verify only; launch no guests.
  --keep                  Leave the infrastructure up. Implies no verdict.
  --keep-on-fail          Destroy on success, leave a failure up to inspect.
  --destroy-only          Destroy whatever this topology's state holds, then stop.
  --dry-run               Print the plan and the per-node steps, change nothing.
EOF
    exit 2
}

while [ $# -gt 0 ]; do
    case "$1" in
        --topology) TOPOLOGY="${2:?}"; shift 2 ;;
        --ssh-public-key) SSH_PUBLIC_KEY="${2:?}"; shift 2 ;;
        --ssh-private-key) SSH_PRIVATE_KEY="${2:?}"; shift 2 ;;
        --workbooks) WORKBOOKS_SET=1; WORKBOOKS="${2-}"; shift 2 ;;
        --distro) DISTRO="${2:?}"; shift 2 ;;
        --setup-sh) SETUP_SH="${2:?}"; shift 2 ;;
        --instance-principal) INSTANCE_PRINCIPAL=1; shift ;;
        --no-external-pool) SKIP_POOL=1; SKIP_WORKLOAD=1; shift ;;
        --skip-workload) SKIP_WORKLOAD=1; shift ;;
        --keep) KEEP=1; shift ;;
        --keep-on-fail) KEEP_ON_FAIL=1; shift ;;
        --destroy-only) DESTROY_ONLY=1; shift ;;
        --dry-run) DRY_RUN=1; shift ;;
        -h | --help) usage ;;
        *) die "unknown option $1" ;;
    esac
done

[ -n "$TOPOLOGY" ] || usage
[ -n "${TOPO_SHAPE[$TOPOLOGY]:-}" ] || die "unknown topology $TOPOLOGY; one of: ${!TOPO_SHAPE[*]}"
[ -r "$SSH_PUBLIC_KEY" ] || die "no readable SSH public key at $SSH_PUBLIC_KEY"
[ -r "$SSH_PRIVATE_KEY" ] || die "no readable SSH private key at $SSH_PRIVATE_KEY"
# Both or neither: a distro with the published setup.sh installs one ref's bytes
# with another's layout, and that is a cluster nobody can reason about.
if [ -n "$DISTRO" ] || [ -n "$SETUP_SH" ]; then
    [ -r "$DISTRO" ] || die "--distro is not readable: '$DISTRO'"
    [ -r "$SETUP_SH" ] || die "--setup-sh is not readable: '$SETUP_SH'"
fi

SHAPE="${TOPO_SHAPE[$TOPOLOGY]}"
NODES="${TOPO_NODES[$TOPOLOGY]}"
# Outside the checkout on a persistent runner, because actions/checkout runs
# git clean -ffdx: it would delete the Terraform state of a killed run before the
# sweep could read it, leaving instances running that nothing can find.
STATE_ROOT="${OCI_STATE_ROOT:-$HERE}"
mkdir -p "$STATE_ROOT"
STATE_DIR="$STATE_ROOT/.validate-$TOPOLOGY"
RESULTS="$STATE_DIR/results.txt"

# The OCI SDK is not on a stock runner image and is not a system package here
# either, so the harness owns an interpreter that has it rather than asking every
# host to be prepared. Resolved once, on first use, so --dry-run needs nothing.
PYTHON=""
python_bin() {
    if [ -z "$PYTHON" ]; then
        if python3 -c 'import oci' 2>/dev/null; then
            PYTHON=python3
        else
            [ -x "$STATE_ROOT/.venv/bin/python3" ] || {
                log "creating the OCI SDK virtualenv"
                python3 -m venv "$STATE_ROOT/.venv" >&2
                "$STATE_ROOT/.venv/bin/pip" install --quiet --disable-pip-version-check \
                    -r "$HERE/requirements.txt" >&2 || die "could not install $HERE/requirements.txt"
            }
            PYTHON="$STATE_ROOT/.venv/bin/python3"
        fi
    fi
    printf '%s\n' "$PYTHON"
}

# A separate state directory per topology, so two topologies can be built from one
# checkout without one destroying the other's instances.
tf() {
    "$(python_bin)" "$HERE/scripts/oci_env.py" --ssh-public-key-path "$SSH_PUBLIC_KEY" -- \
        terraform -chdir="$HERE" "$@"
}

# terraform output takes -state but not -var, so the two sets are kept apart. They
# were one set once, and the -var made output fail into the default state file.
tf_state=(-state "$STATE_DIR/terraform.tfstate")
tf_vars=(
    -var "compute_shape=$SHAPE"
    -var "node_count=$NODES"
    -var "enable_instance_principal=$([ "$INSTANCE_PRINCIPAL" = 1 ] && echo true || echo false)"
    "${tf_state[@]}"
)

ssh_node() {
    local host="$1"
    shift
    # LogLevel=ERROR: the host-key warning is unavoidable with a throwaway known-hosts
    # file, and it lands in the middle of whatever the remote command reported.
    ssh -i "$SSH_PRIVATE_KEY" -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null \
        -o LogLevel=ERROR -o BatchMode=yes -o ConnectTimeout=15 "ubuntu@$host" "$@"
}

record() { printf '%s\n' "$*" >> "$RESULTS"; }

# Teardown is the last assertion, not cleanup: a workbook or a topology that
# cannot be destroyed is a defect, and it has been one before.
DESTROY_RC=""
destroy_topology() {
    log "destroying"
    if tf destroy -auto-approve -no-color "${tf_vars[@]}" > "$STATE_DIR/destroy.log" 2>&1; then
        DESTROY_RC=0
        record "destroy: PASS"
    else
        DESTROY_RC=1
        record "destroy: FAIL (see $STATE_DIR/destroy.log)"
        log "TEARDOWN FAILED — resources may still be billing. $STATE_DIR/destroy.log"
    fi
}

cleanup() {
    local rc=$?
    if [ "$KEEP" = 1 ]; then
        log "--keep: leaving the infrastructure up, no verdict recorded"
        return
    fi
    # Only on a failure, and said out loud: the cost of forgetting is an OCI bare
    # metal host running overnight.
    if [ "$KEEP_ON_FAIL" = 1 ] && [ "$rc" != 0 ]; then
        log "--keep-on-fail: $TOPOLOGY FAILED and is being left up. IT IS STILL BILLING."
        log "destroy it with: $0 --topology $TOPOLOGY --destroy-only"
        verdict "$rc"
        return
    fi
    [ "$DRY_RUN" = 1 ] && return
    destroy_topology
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

if [ "$DESTROY_ONLY" = 1 ]; then
    log "destroy only: whatever $STATE_DIR/terraform.tfstate holds"
    destroy_topology
    [ "$DESTROY_RC" = 0 ] || exit 1
    exit 0
fi

: > "$RESULTS"

log "$SHAPE, $NODES node(s), state in $STATE_DIR"

# The directory's own terraform.tfstate belongs to hand-driven runs, not to a
# topology, and an instance left in it is both a bill and a name that collides
# with ours in the console. Say so rather than letting it be a surprise.
if [ -s "$HERE/terraform.tfstate" ]; then
    stray="$(python3 -c '
import json, sys
state = json.load(open(sys.argv[1]))
for res in state.get("resources", []):
    if res["type"] != "oci_core_instance":
        continue
    for inst in res["instances"]:
        attrs = inst["attributes"]
        if attrs.get("state") not in ("TERMINATED", None):
            print(attrs.get("display_name"), attrs.get("shape"), attrs.get("state"), attrs.get("public_ip"))
' "$HERE/terraform.tfstate" 2>/dev/null || true)"
    [ -n "$stray" ] && log "NOTE: the hand-driven state in $HERE still holds: $stray"
fi

if [ "$DRY_RUN" = 1 ]; then
    tf plan -no-color "${tf_vars[@]}" | tail -30
    plan="install Spinifex on $NODES node(s), form the cluster"
    [ "$SKIP_POOL" = 1 ] && plan="$plan, configure no external pool"
    if [ "$SKIP_WORKLOAD" = 1 ]; then
        plan="$plan, launch no guests"
    elif [ "$WORKBOOKS_SET" = 1 ]; then
        plan="$plan, run the workbooks '$WORKBOOKS'"
    else
        plan="$plan, run the driver's default workbooks"
    fi
    log "dry run: would $plan, then destroy"
    exit 0
fi

trap cleanup EXIT

tf init -input=false > "$STATE_DIR/init.log" 2>&1 || die "terraform init failed; see $STATE_DIR/init.log"

log "building"
tf apply -auto-approve -no-color "${tf_vars[@]}" > "$STATE_DIR/apply.log" 2>&1 \
    || die "terraform apply failed; see $STATE_DIR/apply.log"
record "build: PASS"

mapfile -t HOSTS < <(tf output -raw "${tf_state[@]}" hosts_file) \
    || die "could not read the hosts_file output from $STATE_DIR/terraform.tfstate"
[ "${#HOSTS[@]}" = "$NODES" ] || die "expected $NODES host(s) from the hosts_file output, got ${#HOSTS[@]}: ${HOSTS[*]}"
for host in "${HOSTS[@]}"; do
    [[ "$host" =~ ^[0-9]+(\.[0-9]+){3}$ ]] \
        || die "the hosts_file output is not an address: '$host' -- something on the wrapper's stdout is in the capture"
done
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

# With --distro the bytes are the ref under test; without it they are the published
# release, which answers a different question -- what a customer following the
# guide gets today. A nightly judging a branch must pass --distro.
log "installing Spinifex on each node ($([ -n "$DISTRO" ] && echo "$(basename "$DISTRO")" || echo "published release"))"
for host in "${HOSTS[@]}"; do
    if [ -n "$DISTRO" ]; then
        scp -i "$SSH_PRIVATE_KEY" -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null \
            -o LogLevel=ERROR -q "$DISTRO" "$SETUP_SH" "ubuntu@$host:/tmp/" \
            || die "could not copy the distro to $host"
    fi
    # cloud-init being finished does not mean apt is: the apt-daily timers and
    # unattended-upgrades run on their own schedule and hold the dpkg lock, which
    # the installer then fails on. Wait for the lock rather than fight it.
    ssh_node "$host" "DISTRO_NAME='$(basename "${DISTRO:-}")' SETUP_NAME='$(basename "${SETUP_SH:-}")' bash -s" \
        > "$STATE_DIR/install-$host.log" 2>&1 <<'REMOTE' \
        || die "install failed on $host; see $STATE_DIR/install-$host.log"
set -e
for _ in $(seq 1 120); do
    sudo fuser /var/lib/dpkg/lock-frontend /var/lib/apt/lists/lock >/dev/null 2>&1 || break
    sleep 5
done
sudo fuser /var/lib/dpkg/lock-frontend >/dev/null 2>&1 \
    && { echo "the dpkg lock was still held after 10 minutes"; exit 1; }
if [ -n "${DISTRO_NAME:-}" ]; then
    # The production setup.sh against the ref's own tarball, which is what the
    # release and the ISO both run, so there is no separate dev install layout.
    # No INSTALL_SPINIFEX_SKIP_APT/SKIP_AWS here, unlike the CI hypervisors: an OCI
    # stock image is not pre-baked, so the dependency stages have to run.
    sudo env INSTALL_SPINIFEX_TARBALL="/tmp/$DISTRO_NAME" bash "/tmp/$SETUP_NAME"
else
    curl -sfL https://install.mulgadc.com | sudo bash
fi
sudo /usr/local/share/spinifex/setup-ovn.sh --management --nat-uplink
REMOTE
    log "$host installed: $(ssh_node "$host" 'spx version' 2>&1 | head -1)"
done
record "install: PASS"

# A single node is its own documented path: install-node.sh refuses fewer than two
# hosts, because there is nothing to join. Both branches use the flags the guide
# names, so what is validated is what the guide tells a customer to type.
if [ "$NODES" = 1 ]; then
    log "initializing the single node"
    ssh_node "${HOSTS[0]}" '
        set -e
        sudo spx admin init --node "$(hostname -s)" --nodes 1 \
            --region ap-southeast-2 --az ap-southeast-2a \
            --external-mode=nat --ipsec=false
        sudo systemctl start spinifex.target
    ' > "$STATE_DIR/form.log" 2>&1 \
        || die "single-node init failed; see $STATE_DIR/form.log"
else
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
fi
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

# Parse the STATUS column, never grep for Ready: NotReady contains it, and that
# read passed a single-node run whose only node was NotReady on 0.0.0.0. The table
# is colourised, so the escapes come off first.
count_ready() {
    sed 's/\x1b\[[0-9;]*m//g' "$STATE_DIR/nodes.txt" \
        | awk -F'|' 'NR > 1 { gsub(/^[ \t]+|[ \t]+$/, "", $2); if ($2 == "Ready") n++ } END { print n + 0 }'
}

log "cluster membership"
ready=0
for _ in $(seq 1 30); do
    ssh_node "${HOSTS[0]}" 'sudo spx get nodes' > "$STATE_DIR/nodes.txt" 2>&1 || true
    ready="$(count_ready)"
    [ "$ready" = "$NODES" ] && break
    sleep 10
done
cat "$STATE_DIR/nodes.txt"
[ "$ready" = "$NODES" ] || die "expected $NODES Ready node(s), got $ready; see $STATE_DIR/nodes.txt"
record "membership: PASS ($ready Ready)"

if [ "$SKIP_WORKLOAD" = 1 ]; then
    log "--skip-workload: stopping before the workbook"
    record "workbook: SKIPPED"
    exit 0
fi

# The same driver the GitHub nightly runs (cell 17), on the same published
# workbooks, so a workbook that passes on a hypervisor and fails on OCI is a
# difference in OCI and not in the test. It owns its own per-workbook assertions
# and destroys each one it builds.
log "running the published workbooks"
DRIVER="$REPO_ROOT/tests/e2e/run-tofu-examples-e2e.sh"
[ -r "$DRIVER" ] || die "no workbook driver at $DRIVER"
scp -i "$SSH_PRIVATE_KEY" -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null \
    -o LogLevel=ERROR -q "$DRIVER" "ubuntu@${HOSTS[0]}:~/run-tofu-examples-e2e.sh" \
    || die "could not copy the workbook driver"
scp -i "$SSH_PRIVATE_KEY" -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null \
    -o LogLevel=ERROR -qr "$REPO_ROOT/docs/terraform-workbooks" "ubuntu@${HOSTS[0]}:~/workbooks" \
    || die "could not copy the workbooks"

# Every AMI the workbooks need, imported one at a time so predastore is not asked
# to absorb parallel uploads. rds-quickstart is the one that needs an appliance.
log "importing the images the workbooks need"
ssh_node "${HOSTS[0]}" '
    set -e
    for img in ubuntu-26.04-x86_64 spinifex-rds-postgres; do
        sudo spx admin images import --name "$img" --config /etc/spinifex/spinifex.toml >/dev/null
    done
' > "$STATE_DIR/images.log" 2>&1 || die "image import failed; see $STATE_DIR/images.log"

# WORKBOOKS unset leaves the driver on its own default list, which is the list the
# nightly judges every other platform by.
workbook_env="WORKBOOK_DIR=\$HOME/workbooks"
[ "$WORKBOOKS_SET" = 1 ] && workbook_env="$workbook_env $(printf 'WORKBOOKS=%q' "$WORKBOOKS")"

# if !, not a $? read after the fact: under set -e a failing ssh never reaches the
# next line, which is the one that prints the driver's own diagnostics.
if ssh_node "${HOSTS[0]}" "chmod +x ~/run-tofu-examples-e2e.sh; $workbook_env ~/run-tofu-examples-e2e.sh" \
    > "$STATE_DIR/workbooks.log" 2>&1; then
    passed="$(grep -c '^--- PASS' "$STATE_DIR/workbooks.log" || true)"
    ran="$(grep -c '^=== RUN' "$STATE_DIR/workbooks.log" || true)"
    record "workbooks: PASS ($passed of $ran)"
else
    tail -60 "$STATE_DIR/workbooks.log"
    grep -E '^--- (PASS|FAIL)' "$STATE_DIR/workbooks.log" | sed 's/^/  /' || true
    die "the published workbooks failed on $TOPOLOGY; see $STATE_DIR/workbooks.log"
fi
