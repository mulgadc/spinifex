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
CREDENTIAL_HOOK="${OCI_CREDENTIAL_HOOK:-}"
# Empty means oci_env.py chooses, which is $OCI_CLI_PROFILE, then the reference
# tenancy, then DEFAULT. Named here so a deployment can say which tenancy it is in.
OCI_PROFILE="${OCI_PROFILE:-}"
DISTRO=""
SETUP_SH=""
# Empty means the driver's own default list. Unset is distinguishable from empty,
# so --workbooks "" can deliberately mean "run none".
WORKBOOKS_SET=0
CHANNEL=latest
INSTALL_VERSION=""
WORKBOOKS=""

# What a topology is worth when nothing says otherwise, so a CI run of a named
# topology is the same every time. Supplied as TF_VAR_, which terraform.auto.tfvars
# outranks, so a deployment sets its shape and node count in that file and no
# flag has to carry them.
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
  --credential-hook PATH  Executable run after formation, before the pool, as
                          "hook <ssh-key> <host>...". Where an API-key deployment
                          installs its credential. Default \$OCI_CREDENTIAL_HOOK.
  --oci-profile NAME      Profile in ~/.oci/config that Terraform builds with.
                          Default \$OCI_PROFILE, else \$OCI_CLI_PROFILE, else the
                          reference tenancy, else DEFAULT. A name that is passed
                          and absent is an error, never a fallback.
  --workbooks LIST        Space-separated workbooks for the published driver to
                          run on the cluster. Empty means run none; omitted means
                          the driver's own default list.
  --version TAG           Install this exact release tag. Preferred over
                          --channel dev for anything repeatable: a tag resolves
                          by redirect, while the dev channel resolves through a
                          rate-limited GitHub API call that 404s when it trips.
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
        --channel) CHANNEL="${2-}"; shift 2 ;;
        --version) INSTALL_VERSION="${2:?}"; shift 2 ;;
        --distro) DISTRO="${2:?}"; shift 2 ;;
        --setup-sh) SETUP_SH="${2:?}"; shift 2 ;;
        --instance-principal) INSTANCE_PRINCIPAL=1; shift ;;
        --credential-hook) CREDENTIAL_HOOK="${2:?}"; shift 2 ;;
        --oci-profile) OCI_PROFILE="${2:?}"; shift 2 ;;
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

# adopt, never create: this script's state is destroyed at the end of every run, and
# the dynamic group and policy outlive every topology. setup-identity.sh owns them.
PRINCIPAL_MODE=$([ "$INSTANCE_PRINCIPAL" = 1 ] && echo adopt || echo off)

TOPO_DEFAULT_SHAPE="${TOPO_SHAPE[$TOPOLOGY]}"
TOPO_DEFAULT_NODES="${TOPO_NODES[$TOPOLOGY]}"
# What the apply actually built, filled in from the hosts_file output once it has.
# Until then these are what the topology asked for, which is all there is to say.
SHAPE="$TOPO_DEFAULT_SHAPE"
NODES="$TOPO_DEFAULT_NODES"
# Outside the checkout on a persistent runner, because actions/checkout runs
# git clean -ffdx: it would delete the Terraform state of a killed run before the
# sweep could read it, leaving instances running that nothing can find.
STATE_ROOT="${OCI_STATE_ROOT:-$HERE}"
mkdir -p "$STATE_ROOT"
STATE_DIR="$STATE_ROOT/.validate-$TOPOLOGY"
RESULTS="$STATE_DIR/results.txt"

# Tab-separated siblings of the human block, for the run-page tables. Written here
# rather than parsed out of the log later: the status of a gate is then a field
# this script sets, not a regex over prose that reads fine and matches wrong.
RESULTS_TSV="$STATE_DIR/results.tsv"
WORKBOOKS_TSV="$STATE_DIR/workbooks.tsv"

# A separate state directory per topology, so two topologies can be built from one
# checkout without one destroying the other's instances.
tf() {
    # The topology's shape and count go in as TF_VAR_, which is the weakest source
    # Terraform reads: a terraform.auto.tfvars in the checkout outranks it, so a
    # deployment sizes itself in that file and the topology only supplies the
    # default a CI run wants.
    TF_VAR_compute_shape="$TOPO_DEFAULT_SHAPE" TF_VAR_node_count="$TOPO_DEFAULT_NODES" \
        python3 "$HERE/scripts/oci_env.py" --ssh-public-key-path "$SSH_PUBLIC_KEY" \
        ${OCI_PROFILE:+--profile "$OCI_PROFILE"} -- \
        terraform -chdir="$HERE" "$@"
}

# terraform output takes -state but not -var, so the two sets are kept apart. They
# were one set once, and the -var made output fail into the default state file.
tf_state=(-state "$STATE_DIR/terraform.tfstate")
# instance_principal stays a -var, and outranks any tfvars, because --instance-principal
# is a choice about this run rather than about the infrastructure's size.
tf_vars=(
    -var "instance_principal=$PRINCIPAL_MODE"
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

# record <gate> <PASS|FAIL|SKIPPED|INFO> [detail]
# The node's own account of a failed run. A workbook's log says which API call
# failed; only the journal says why, and the nodes are destroyed minutes later.
# Warnings first because that is where a swallowed error surfaces, then a bounded
# tail for context -- unbounded, a multi-hour suite's journal dwarfs the artifact.
capture_journals() {
    local host
    for host in "${HOSTS[@]}"; do
        ssh_node "$host" '
            echo "=== spinifex, warning and above ==="
            sudo journalctl -u "spinifex-*" --since -4h --priority=warning --no-pager | tail -2000
            echo "=== spinifex, all priorities, last 2000 lines ==="
            sudo journalctl -u "spinifex-*" --since -4h --no-pager | tail -2000
        ' > "$STATE_DIR/journal-$host.log" 2>&1 || log "could not collect the journal from $host"
    done
}

record() {
    local gate="$1" status="$2" detail="${3-}"
    printf '%s: %s%s\n' "$gate" "$status" "${detail:+ $detail}" >> "$RESULTS"
    printf '%s\t%s\t%s\n' "$gate" "$status" "$detail" >> "$RESULTS_TSV"
}

# The driver's per-workbook lines, which are go test's own shape because
# go-junit-report consumes them downstream. A workbook the driver never reached
# is absent from its log and so absent here, which the summary renders as a row
# rather than dropping — a missing row reads as a pass.
record_workbooks() {
    local log="$1"
    [ -r "$log" ] || return 0
    sed -n 's/^--- \(PASS\|FAIL\): TestTofuWorkbook_\([A-Za-z0-9_]*\) (\([0-9]*\)\.[0-9]*s)$/\2\t\1\t\3/p' \
        "$log" | tr '_' '-' > "$WORKBOOKS_TSV"
}

# Teardown is the last assertion, not cleanup: a workbook or a topology that
# cannot be destroyed is a defect, and it has been one before.
DESTROY_RC=""
destroy_topology() {
    log "destroying"
    if tf destroy -auto-approve -no-color "${tf_vars[@]}" > "$STATE_DIR/destroy.log" 2>&1; then
        DESTROY_RC=0
        record destroy PASS
    else
        DESTROY_RC=1
        record destroy FAIL "see $STATE_DIR/destroy.log"
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
: > "$RESULTS_TSV"
: > "$WORKBOOKS_TSV"

# Last run's logs go before this one's first line, so what is left in here always
# describes the run that is starting. Without this a topology keeps logs that read
# as current, and anything collecting the directory publishes them as this run's.
# Named globs rather than a find: the tfstate is how the sweep finds hosts to
# destroy, so what is removed here has to be readable at a glance.
# :? so an unset STATE_DIR stops the shell rather than expanding to /*.log.
rm -f "${STATE_DIR:?}"/*.log "${STATE_DIR:?}"/nodes.txt "${STATE_DIR:?}"/cloudinit-*.txt

log "$SHAPE, $NODES node(s), state in $STATE_DIR"

# Checked here, not where the hook is run: nothing about it depends on the apply, and
# the run is otherwise forty minutes and a bare-metal bill from discovering that the
# path in a CI variable does not exist on this runner.
if [ "$SKIP_POOL" != 1 ] && [ "$PRINCIPAL_MODE" = off ]; then
    [ -n "$CREDENTIAL_HOOK" ] \
        || die "this deployment authenticates with an API key and no credential hook is set, so no node could reach the OCI API; set --credential-hook or OCI_CREDENTIAL_HOOK, or use an instance principal"
    [ -x "$CREDENTIAL_HOOK" ] || die "credential hook is not executable: $CREDENTIAL_HOOK"
fi

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
record build PASS

mapfile -t HOSTS < <(tf output -raw "${tf_state[@]}" hosts_file) \
    || die "could not read the hosts_file output from $STATE_DIR/terraform.tfstate"
[ "${#HOSTS[@]}" -ge 1 ] || die "the hosts_file output named no hosts, so the apply built nothing to install on"
for host in "${HOSTS[@]}"; do
    [[ "$host" =~ ^[0-9]+(\.[0-9]+){3}$ ]] \
        || die "the hosts_file output is not an address: '$host' -- something on the wrapper's stdout is in the capture"
done

# node_count can come from a tfvars file, so the hosts the apply produced are the
# authority on how many there are. Every later count check reads this.
if [ "${#HOSTS[@]}" != "$NODES" ]; then
    log "note: $NODES node(s) is the $TOPOLOGY default, and the apply built ${#HOSTS[@]}; taking the apply's"
    NODES="${#HOSTS[@]}"
fi
SHAPE=$(tf output -raw "${tf_state[@]}" compute_shape) \
    || die "could not read the compute_shape output from $STATE_DIR/terraform.tfstate"
log "hosts: ${HOSTS[*]}"
log "$SHAPE, $NODES node(s)"

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
record "cloud-init units" PASS

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
    ssh_node "$host" "DISTRO_NAME='$(basename "${DISTRO:-}")' SETUP_NAME='$(basename "${SETUP_SH:-}")' CHANNEL='$CHANNEL' INSTALL_VERSION='$INSTALL_VERSION' bash -s" \
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
    # The real customer path, including the checksum step a local tarball skips.
    # The environment variable rather than --channel: the installer served by
    # install.mulgadc.com is the latest release's, so it predates the flag.
    #
    # A tag goes in as VERSION, not CHANNEL. Only the dev channel resolves through
    # an unauthenticated GitHub API call, which is rate limited per source address
    # and returns 404 once it trips -- indistinguishable from a missing asset. A
    # tagged path is a plain redirect, so it does not have that failure mode.
    if [ -n "$INSTALL_VERSION" ]; then
        curl -sfL https://install.mulgadc.com | sudo env INSTALL_SPINIFEX_VERSION="$INSTALL_VERSION" bash
    else
        curl -sfL https://install.mulgadc.com | sudo env INSTALL_SPINIFEX_CHANNEL="$CHANNEL" bash
    fi
fi
sudo /usr/local/share/spinifex/setup-ovn.sh --management --nat-uplink
REMOTE
    INSTALLED_VERSION="$(ssh_node "$host" 'spx version' 2>&1 | head -1)"
    log "$host installed: $INSTALLED_VERSION"
done
record install PASS "$INSTALLED_VERSION"

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
record formation PASS

# Runs after formation and before the pool is configured, which is the only window
# where a node has /etc/spinifex but has not yet started the allocator. Nothing in
# this repository knows what it does: an API-key deployment needs a credential on
# each node, and a credential belongs to the operator, not to a checked-in script.
# Skipped under instance principal, which needs no handoff at all.
if [ "$SKIP_POOL" != 1 ] && [ "$PRINCIPAL_MODE" = off ]; then
    log "running the credential hook"
    # Arguments, not a file: the hook is told where the nodes are and how to reach
    # them, and decides for itself what to put there.
    "$CREDENTIAL_HOOK" "$SSH_PRIVATE_KEY" "${HOSTS[@]}" > "$STATE_DIR/credential-hook.log" 2>&1 \
        || die "the credential hook failed; see $STATE_DIR/credential-hook.log"
    log "credential hook ok"
fi

# The IMDS remap and the pool are both set-before-first-start, and the remap is the
# half that is invisible when missing: without it the cloud's metadata service is
# shadowed by Spinifex's own endpoints and the allocator cannot resolve its VNIC.
log "configuring the IMDS remap$([ "$SKIP_POOL" = 1 ] && echo " (no external pool)" || echo " and the external pool")"
for host in "${HOSTS[@]}"; do
    ssh_node "$host" "SKIP_POOL=$SKIP_POOL bash -s" > "$STATE_DIR/pool-$host.log" 2>&1 <<'REMOTE' \
        || die "pool configuration failed on $host; see $STATE_DIR/pool-$host.log"
set -e
sudo cp /etc/spinifex/spinifex.toml /etc/spinifex/spinifex.toml.bak-prepool
# subn, not sub: with no [network] section to match, sub rewrites the file unchanged
# and exits 0, leaving the remap silently unapplied. That is the half of this that is
# invisible when missing, so the substitution count is checked rather than assumed.
grep -q imds_host_meta_ip /etc/spinifex/spinifex.toml || sudo python3 - <<"PY"
import re, sys
path = "/etc/spinifex/spinifex.toml"
text, n = re.subn(r"(?m)^\[network\]$",
                  "[network]\nimds_host_meta_ip = \"169.254.42.254\"\nimds_host_dns_ip  = \"169.254.42.253\"",
                  open(path).read(), count=1)
if n != 1:
    sys.exit("no [network] section in %s, so the IMDS remap was not applied" % path)
open(path, "w").write(text)
PY
if [ "${SKIP_POOL:-0}" != 1 ]; then
    grep -q 'name               = "oci-public"' /etc/spinifex/spinifex.toml \
        || sudo tee -a /etc/spinifex/spinifex.toml < /etc/spinifex/oci/external-pool.toml >/dev/null
fi
# spx has no config subcommand, so the `spx config validate` that used to be here
# could only ever fail, and was swallowed. Parsing the file is the check that was
# wanted: both edits above are textual, and a broken result would otherwise surface
# as every service failing to start with nothing saying why.
sudo python3 -c 'import sys, tomllib; tomllib.load(open(sys.argv[1], "rb"))' \
    /etc/spinifex/spinifex.toml \
    || { echo "spinifex.toml is not valid TOML after the IMDS remap and the pool append"; exit 1; }
sudo systemctl restart spinifex.target
REMOTE
done

# Everything needed to tell the three allocator faults apart, and deliberately no
# credential: the config file and the PEM are reported as mode and size only, which
# distinguishes absent from unreadable without copying either into a log that is
# attached to a CI run.
allocator_diagnostics() {
    local host="$1"
    ssh_node "$host" '
        echo "=== spinifex-daemon: the allocator (ocinet) lives here ==="
        sudo journalctl -u spinifex-daemon --since -20min --no-pager | grep -i "ocinet\|external VNIC\|allocator\|oci" | tail -40
        echo "=== spinifex-vpcd: consumes the addresses, does not allocate them ==="
        sudo journalctl -u spinifex-vpcd --since -20min --no-pager | tail -40
        echo "=== external_pools as configured ==="
        sudo sed -n "/\[\[network.external_pools\]\]/,\$p" /etc/spinifex/spinifex.toml
        echo "=== credential files (mode and size, never content) ==="
        sudo stat -c "%n %A %s bytes owner=%U:%G" \
            /etc/spinifex/oci/config /etc/spinifex/oci/oci_api_key.pem 2>&1
        echo "=== the bridge MAC the allocator matches on ==="
        ip -br link show
        echo "=== MACs IMDS reports for this instance VNICs ==="
        curl -sf -H "Authorization: Bearer Oracle" \
            http://169.254.169.254/opc/v2/vnics/ | tr "," "\n" | grep -i "macAddr\|privateIp" || \
            echo "IMDS unreachable -- check the remap, Spinifex claims 169.254.169.254 itself"
    ' 2>&1
}

# resolved the external VNIC is the line that proves the credential works, the
# compartment is right and br-wan's MAC matched a real VNIC. A node missing it
# accepts allocate-address and then fails it.
if [ "$SKIP_POOL" = 1 ]; then
    log "--no-external-pool: no allocator to check"
    record "oci allocator" SKIPPED "no external pool configured"
else
    log "checking the allocator came up"
    for host in "${HOSTS[@]}"; do
        found=0
        for _ in $(seq 1 30); do
            # spinifex-daemon, not spinifex-vpcd: the allocator is built in the
            # daemon, and vpcd only consumes the addresses it hands out. Gating on
            # vpcd's journal failed a node whose allocator was working.
            if ssh_node "$host" "sudo journalctl -u spinifex-daemon --since -10min --no-pager | grep -q 'resolved the external VNIC'" 2>/dev/null; then
                found=1
                break
            fi
            sleep 10
        done
        if [ "$found" != 1 ]; then
            # Before the teardown, because the teardown is what destroys the only
            # copy. Three unrelated faults land here -- a credential the SDK would
            # not load, a bridge MAC matching no VNIC, and a pool vpcd never read --
            # and they are indistinguishable from the missing log line alone.
            allocator_diagnostics "$host" > "$STATE_DIR/allocator-$host.log" 2>&1 || true
            die "$host never logged 'resolved the external VNIC'; the OCI allocator is not up, so no guest can get a public address. Diagnostics: $STATE_DIR/allocator-$host.log"
        fi
        log "$host allocator ready"
    done
    record "oci allocator" PASS
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
record membership PASS "$ready Ready"

if [ "$SKIP_WORKLOAD" = 1 ]; then
    log "--skip-workload: stopping before the workbook"
    record workbooks SKIPPED "--skip-workload"
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
# to absorb parallel uploads. Three are appliances rather than distros: RDS, ECS
# and EKS each boot their own, found by a spinifex:managed-by tag and never by name.
log "importing the images the workbooks need"
ssh_node "${HOSTS[0]}" '
    set -e
    for img in ubuntu-26.04-x86_64 spinifex-rds-postgres spinifex-ecs-node spinifex-eks-node; do
        sudo spx admin images import --name "$img" --config /etc/spinifex/spinifex.toml >/dev/null
    done
' > "$STATE_DIR/images.log" 2>&1 || die "image import failed; see $STATE_DIR/images.log"

# Every workbook that is a workbook. The shared driver's own default is five, which
# leaves ECS and all three EKS variants untested on every platform -- their
# assertions exist and nothing was running them.
#
# demo-app is absent because it is not a workbook: it has no .tf at all, being the
# container image the EKS workbooks' nested workloads/ modules deploy. Listing it
# here would fail on a missing root module rather than test anything.
OCI_WORKBOOKS="nginx-alb bastion-private-subnet nginx-webserver s3-webapp rds-quickstart"
OCI_WORKBOOKS="$OCI_WORKBOOKS ecs-quickstart eks-quickstart eks-https-ingress eks-gitops-argocd"
[ "$WORKBOOKS_SET" = 1 ] || WORKBOOKS="$OCI_WORKBOOKS"
workbook_env="WORKBOOK_DIR=\$HOME/workbooks $(printf 'WORKBOOKS=%q' "$WORKBOOKS")"

# Whether a node can reach a public address inside its own VCN. Informational: the
# remedy below is right either way, because a customer reaches a guest from outside.
#
# Needs two hosts. With one, HOSTS[0] and HOSTS[-1] are the same address and the node
# connects to itself, which always succeeds and measures nothing — it reported
# "present" on vm-single and bm for exactly that reason before this guard.
#
# A negative result is only interpretable when the security list admits the source,
# so the record says which it was: narrowing node_client_cidr_allow_list makes a
# node's own public address a non-permitted source, and the refusal that follows
# looks identical to a VCN that does not hairpin.
if [ "${#HOSTS[@]}" -lt 2 ]; then
    record "vcn hairpin" SKIPPED "needs two nodes; one node can only probe itself"
elif ssh_node "${HOSTS[0]}" \
    "bash -c 'exec 3<>/dev/tcp/${HOSTS[${#HOSTS[@]}-1]}/22' 2>/dev/null"; then
    record "vcn hairpin" INFO "present: a node reached another node's public address"
else
    record "vcn hairpin" INFO "not reached, which is a VCN without hairpin or a security list that excludes the source"
fi

# Remote dynamic forward: the node gets a SOCKS5 proxy on this port whose egress
# is this host, outside the VCN. It gives the driver the customer's vantage point
# for public addresses without moving the driver off the node, where its ovn-nbctl,
# journalctl and spx diagnostics have to run. The subnet's own allow list does not
# need a new entry: ssh from here already works, and its rules are per-CIDR for
# every protocol.
PUBLIC_PROXY_PORT="${PUBLIC_PROXY_PORT:-1080}"
workbook_env="$workbook_env E2E_PUBLIC_PROXY=127.0.0.1:$PUBLIC_PROXY_PORT"

# ExitOnForwardFailure, so a port already in use stops the run here instead of
# handing the driver a proxy that is not there. Keepalives because this one
# connection carries the proxy for the whole suite, and a dropped forward would
# read as every remaining public address going dark at once.
#
# if !, not a $? read after the fact: under set -e a failing ssh never reaches the
# next line, which is the one that prints the driver's own diagnostics.
if ssh -i "$SSH_PRIVATE_KEY" -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null \
    -o LogLevel=ERROR -o BatchMode=yes -o ConnectTimeout=15 \
    -o ServerAliveInterval=30 -o ServerAliveCountMax=6 \
    -o ExitOnForwardFailure=yes -R "$PUBLIC_PROXY_PORT" "ubuntu@${HOSTS[0]}" \
    "chmod +x ~/run-tofu-examples-e2e.sh; $workbook_env ~/run-tofu-examples-e2e.sh" \
    > "$STATE_DIR/workbooks.log" 2>&1; then
    record_workbooks "$STATE_DIR/workbooks.log"
    passed="$(grep -c '^--- PASS' "$STATE_DIR/workbooks.log" || true)"
    ran="$(grep -c '^=== RUN' "$STATE_DIR/workbooks.log" || true)"
    record workbooks PASS "$passed of $ran"
else
    # Before die, so a failed suite still gets its per-workbook table: which four
    # passed is most of what the fifth failing means.
    record_workbooks "$STATE_DIR/workbooks.log"
    failed="$(grep -c '^--- FAIL' "$STATE_DIR/workbooks.log" || true)"
    record workbooks FAIL "$failed failed, see $STATE_DIR/workbooks.log"
    capture_journals
    tail -60 "$STATE_DIR/workbooks.log"
    grep -E '^--- (PASS|FAIL)' "$STATE_DIR/workbooks.log" | sed 's/^/  /' || true
    die "the published workbooks failed on $TOPOLOGY; see $STATE_DIR/workbooks.log"
fi
