#!/usr/bin/env bash
# e2e-vultr.sh — prove a ref works on Vultr: deploy it with deploy-vultr.sh, run
# the published Terraform workbooks against it from node1, then destroy it and
# fail if anything is left billing. One entry point for a workstation and CI.
#
# Configured by environment variables, so a dispatch needs no arguments:
#
#   VULTR_API_KEY        Required. Its access control must admit this host and
#                        every node, so a CI key needs 0.0.0.0/0 or a fixed runner.
#   SPX_REF              Ref to prove. Default: the checkout's current branch.
#   VULTR_SOURCE         tree (build SPX_REF, the default) or release.
#   VULTR_CHANNEL        With VULTR_SOURCE=release: latest (default) or dev.
#   VULTR_NODES          1 (default) or 3.
#   VULTR_PLAN           Default vx1-g-16c-64g; vbm-8c-64gb-amd for bare metal.
#   VULTR_REGION         Default ord.
#   VULTR_NAME           Deployment name. Default e2e-<UTC stamp>.
#   WORKBOOKS            Workbooks to run. Unset: the list below. "": none.
#   VULTR_KEEP_ON_FAIL   1 leaves a failed deployment up. It keeps billing.
#   VULTR_ARTIFACT_DIR   Logs and the verdict. Default ./.e2e-vultr-<stamp>.
#   MULGA_ROOT           mulga checkout, for the artifact builder.
#
#   e2e-vultr.sh [--sweep-only]   --sweep-only destroys VULTR_NAME and stops.
set -uo pipefail

HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
SPINIFEX_ROOT="$(cd "$HERE/../../.." && pwd)"
MULGA_ROOT="${MULGA_ROOT:-$SPINIFEX_ROOT/..}"
STAMP="$(date -u +%Y%m%dT%H%M%SZ)"

REF="${SPX_REF:-$(git -C "$SPINIFEX_ROOT" rev-parse --abbrev-ref HEAD)}"
SOURCE="${VULTR_SOURCE:-tree}"
CHANNEL="${VULTR_CHANNEL:-latest}"
NODES="${VULTR_NODES:-1}"
PLAN="${VULTR_PLAN:-vx1-g-16c-64g}"
REGION="${VULTR_REGION:-ord}"
NAME="${VULTR_NAME:-e2e-$(date -u +%m%d%H%M%S)}"
ARTIFACT_DIR="${VULTR_ARTIFACT_DIR:-$HERE/.e2e-vultr-$STAMP}"
DEPLOY="$HERE/deploy-vultr.sh"
DEFAULT_WORKBOOKS="nginx-alb bastion-private-subnet nginx-webserver s3-webapp rds-quickstart ecs-quickstart eks-quickstart eks-https-ingress eks-gitops-argocd"

log() { printf '[e2e-vultr] %s\n' "$*"; }
die() { log "ERROR: $*"; echo "run: FAIL ($*)" >> "$VERDICT"; finish 1; }

mkdir -p "$ARTIFACT_DIR"
VERDICT="$ARTIFACT_DIR/verdict.txt"
[ "${1:-}" = --sweep-only ] && VERDICT="$ARTIFACT_DIR/sweep-verdict.txt"
: > "$VERDICT"
[ -n "${VULTR_API_KEY:-}" ] || { log "VULTR_API_KEY is not set"; exit 2; }
export VULTR_API_KEY

# A throwaway key per run unless one is given, so CI needs no SSH secret and a
# key never outlives the deployment it was made for.
SSH_KEY="${VULTR_SSH_KEY:-$ARTIFACT_DIR/ssh/id_ed25519}"
if [ ! -f "$SSH_KEY" ]; then
    mkdir -p "$(dirname "$SSH_KEY")" && chmod 700 "$(dirname "$SSH_KEY")"
    ssh-keygen -q -t ed25519 -N "" -C "spinifex-e2e-$NAME" -f "$SSH_KEY"
fi
DEPLOY_ARGS=(--name "$NAME" --nodes "$NODES" --plan "$PLAN" --region "$REGION" --ssh-key "$SSH_KEY")

# on_node1 [ssh options --] command: the options go before the destination,
# where ssh reads them, and everything after -- is the remote command.
on_node1() {
    local ip opts=()
    if [[ " $* " == *" -- "* ]]; then
        while [ "$1" != -- ]; do opts+=("$1"); shift; done
        shift
    fi
    ip="$("$DEPLOY" status "${DEPLOY_ARGS[@]}" 2>/dev/null | awk '$1 == "node1" {print $2}')"
    [ -n "$ip" ] || { log "no node1 address in deploy-vultr.sh status"; return 1; }
    ssh -i "$SSH_KEY" -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null -o LogLevel=ERROR \
        -o BatchMode=yes -o ServerAliveInterval=30 -o ServerAliveCountMax=6 "${opts[@]}" "root@$ip" "$@"
}

# destroy is also the last assertion: it fails, listing what remains, if anything
# of the deployment is still in the account afterwards.
sweep() {
    log "destroying deployment $NAME"
    if "$DEPLOY" destroy "${DEPLOY_ARGS[@]}" --yes > "$ARTIFACT_DIR/destroy.log" 2>&1; then
        echo "teardown: PASS" >> "$VERDICT"
    else
        tail -20 "$ARTIFACT_DIR/destroy.log"
        echo "teardown: FAIL (see destroy.log)" >> "$VERDICT"
        return 1
    fi
}

# collect_node_logs copies each node's install and deploy logs into the
# artifacts before a teardown takes them, so a failed run can be diagnosed.
collect_node_logs() {
    local name ip
    "$DEPLOY" status "${DEPLOY_ARGS[@]}" 2>/dev/null | awk '/^node[0-9] / {print $1, $2}' |
        while read -r name ip; do
            mkdir -p "$ARTIFACT_DIR/$name"
            scp -i "$SSH_KEY" -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null -o LogLevel=ERROR \
                -o BatchMode=yes -o ConnectTimeout=10 -q "root@$ip:/root/*.log" "$ARTIFACT_DIR/$name/" ||
                log "could not copy logs from $name ($ip)"
        done
}

finish() {
    local rc="$1"
    [ "$rc" = 0 ] || collect_node_logs
    if [ "$rc" != 0 ] && [ "${VULTR_KEEP_ON_FAIL:-0}" = 1 ]; then
        log "keeping $NAME up for inspection; it keeps billing until destroyed"
    else
        sweep || rc=1
    fi
    grep -q FAIL "$VERDICT" && rc=1
    log "=== $REF on Vultr ($NODES x $PLAN in $REGION, $SOURCE build) ==="
    cat "$VERDICT"
    [ "$rc" = 0 ] && log PASS || log FAIL
    exit "$rc"
}

if [ "${1:-}" = --sweep-only ]; then
    sweep
    exit $?
fi

src_args=()
if [ "$SOURCE" = tree ]; then
    git -C "$SPINIFEX_ROOT" rev-parse --verify --quiet "refs/remotes/origin/$REF" >/dev/null ||
        die "$REF is not on origin: the builder clones from GitHub and would build dev instead"
    BUILDER="$MULGA_ROOT/scripts/tofu-cluster/build-install-artifacts.sh"
    [ -x "$BUILDER" ] || die "no artifact builder at $BUILDER (set MULGA_ROOT)"
    log "building $REF"
    "$BUILDER" "$REF" "$ARTIFACT_DIR/spinifex-distro.tar.gz" "$ARTIFACT_DIR/setup.sh" \
        "$ARTIFACT_DIR/spinifex-tests.tar" --skip-e2e-build > "$ARTIFACT_DIR/build.log" 2>&1 ||
        die "building $REF failed; see build.log"
    src_args=(--distro "$ARTIFACT_DIR/spinifex-distro.tar.gz")
else
    src_args=(--channel "$CHANNEL")
fi

log "deploying $NAME: $NODES x $PLAN in $REGION"
start=$SECONDS
if "$DEPLOY" up "${DEPLOY_ARGS[@]}" "${src_args[@]}" > "$ARTIFACT_DIR/deploy.log" 2>&1; then
    echo "deploy: PASS ($((SECONDS - start))s)" >> "$VERDICT"
else
    tail -30 "$ARTIFACT_DIR/deploy.log"
    die "deploy-vultr.sh up failed; see deploy.log"
fi

if [ -n "${WORKBOOKS+x}" ] && [ -z "$WORKBOOKS" ]; then
    echo "workbooks: SKIPPED (none requested)" >> "$VERDICT"
    finish 0
fi
WORKBOOKS="${WORKBOOKS-$DEFAULT_WORKBOOKS}"

log "importing the images the workbooks need"
on_node1 'set -e; for img in ubuntu-26.04-x86_64 spinifex-rds-postgres spinifex-ecs-node spinifex-eks-node; do
    spx admin images import --name "$img" >/dev/null; done' > "$ARTIFACT_DIR/images.log" 2>&1 ||
    die "image import failed; see images.log"

ip1="$("$DEPLOY" status "${DEPLOY_ARGS[@]}" 2>/dev/null | awk '$1 == "node1" {print $2}')"
scp -i "$SSH_KEY" -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null -o LogLevel=ERROR -q \
    "$SPINIFEX_ROOT/tests/e2e/run-tofu-examples-e2e.sh" "root@$ip1:/root/run-tofu-examples-e2e.sh" &&
    scp -i "$SSH_KEY" -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null -o LogLevel=ERROR -qr \
        "$SPINIFEX_ROOT/docs/terraform-workbooks" "root@$ip1:/root/workbooks" ||
    die "could not copy the workbook driver and workbooks to node1"

# The same driver and workbooks as the hypervisor nightly and OCI. The reverse
# SOCKS forward gives it this host's vantage, outside Vultr, for public addresses.
log "running the workbooks: $WORKBOOKS"
if on_node1 -o ExitOnForwardFailure=yes -R 1080 -- \
    "chmod +x /root/run-tofu-examples-e2e.sh; WORKBOOK_DIR=/root/workbooks $(printf 'WORKBOOKS=%q' "$WORKBOOKS") E2E_PUBLIC_PROXY=127.0.0.1:1080 /root/run-tofu-examples-e2e.sh" \
    2>&1 | tee "$ARTIFACT_DIR/workbooks.log"; then
    rc=0
else
    rc=1
fi
grep -E '^--- (PASS|FAIL|SKIP)' "$ARTIFACT_DIR/workbooks.log" | sed 's/^--- /workbook /' >> "$VERDICT"
passed="$(grep -c '^--- PASS' "$ARTIFACT_DIR/workbooks.log" || true)"
ran="$(grep -c '^=== RUN' "$ARTIFACT_DIR/workbooks.log" || true)"
if [ "$rc" = 0 ]; then
    echo "workbooks: PASS ($passed of $ran)" >> "$VERDICT"
else
    echo "workbooks: FAIL ($passed of $ran passed)" >> "$VERDICT"
fi
finish "$rc"
