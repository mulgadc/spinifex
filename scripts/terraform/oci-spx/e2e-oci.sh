#!/bin/bash
# e2e-oci.sh — prove a ref works on OCI, across every topology, and leave nothing
# running. One entry point for a workstation, a Cloudflare-dispatched nightly and
# the GitHub workflow, because three ways of starting it would drift into three
# different answers about whether OCI works.
#
# Configured entirely by environment variables so a dispatch needs no arguments:
#
#   SPX_REF           Ref to prove. Default: the spinifex checkout's current branch.
#   OCI_TOPOLOGIES    Space-separated. Default: "vm-single vm-multi bm".
#   OCI_SOURCE        tree (build SPX_REF) or release (the published installer).
#                     Default tree: a nightly exists to judge a ref, and the
#                     published installer says nothing about one.
#   WORKBOOKS         Workbooks to run on each topology. Default: the driver's own
#                     list. Set to "" to skip the workbook phase entirely.
#   OCI_INSTANCE_PRINCIPAL  1 to authenticate the node's allocator as the instance
#                     principal. Needs the dynamic group; without it no external
#                     address can be allocated and the workbook phase is skipped.
#   OCI_SSH_PUBLIC_KEY / OCI_SSH_PRIVATE_KEY   Paths. Default ~/.ssh/oci-spx[.pub].
#   OCI_ARTIFACT_DIR  Where logs and the verdict land. Default ./.e2e-oci-<stamp>.
#   OCI_KEEP_ON_FAIL  1 to leave a failed topology up for inspection. Off by
#                     default: an OCI bare-metal host left overnight is expensive.
#
# The credential comes from OCI_TENANCY_OCID, OCI_USER_OCID, OCI_FINGERPRINT,
# OCI_PRIVATE_KEY and OCI_REGION when they are set, and from ~/.oci/config when
# they are not. scripts/oci_env.py owns that choice; nothing here reads a key.
set -uo pipefail

HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
SPINIFEX_ROOT="$(cd "$HERE/../../.." && pwd)"

TOPOLOGIES="${OCI_TOPOLOGIES:-vm-single vm-multi bm}"
SOURCE="${OCI_SOURCE:-tree}"
REF="${SPX_REF:-$(git -C "$SPINIFEX_ROOT" rev-parse --abbrev-ref HEAD)}"
SSH_PUBLIC_KEY="${OCI_SSH_PUBLIC_KEY:-$HOME/.ssh/oci-spx.pub}"
SSH_PRIVATE_KEY="${OCI_SSH_PRIVATE_KEY:-$HOME/.ssh/oci-spx}"
ARTIFACT_DIR="${OCI_ARTIFACT_DIR:-$HERE/.e2e-oci-$(date -u +%Y%m%dT%H%M%SZ)}"
SWEEP_ONLY=0
DRY_RUN=0

log() { printf '[e2e-oci] %s\n' "$*"; }
die() {
    printf '[e2e-oci] ERROR: %s\n' "$*" >&2
    exit 1
}

usage() {
    cat >&2 <<EOF
usage: ${0##*/} [--sweep-only] [--dry-run]

Everything else is an environment variable; see the header of this script.

  --sweep-only   Destroy anything an earlier run left behind, then stop.
  --dry-run      Print what each topology would do, build nothing.
EOF
    exit 2
}

while [ $# -gt 0 ]; do
    case "$1" in
        --sweep-only) SWEEP_ONLY=1; shift ;;
        --dry-run) DRY_RUN=1; shift ;;
        -h | --help) usage ;;
        *) die "unknown option $1" ;;
    esac
done

case "$SOURCE" in
    tree | release) ;;
    *) die "OCI_SOURCE must be tree or release, got '$SOURCE'" ;;
esac

mkdir -p "$ARTIFACT_DIR"
VERDICT="$ARTIFACT_DIR/verdict.txt"
: > "$VERDICT"

# A topology destroys itself from its own EXIT trap, so this only catches a run
# the trap never reached: a killed process, a runner reclaimed mid-job, a reboot.
# It works from the state files rather than by name filter, so it can only ever
# destroy what one of these runs created.
sweep() {
    local found=0 dir topology
    for dir in "$HERE"/.validate-*; do
        [ -s "$dir/terraform.tfstate" ] || continue
        python3 -c '
import json, sys
state = json.load(open(sys.argv[1]))
sys.exit(0 if any(r["instances"] for r in state.get("resources", [])) else 1)
' "$dir/terraform.tfstate" 2>/dev/null || continue
        topology="${dir##*/.validate-}"
        found=1
        log "sweep: $topology still holds resources, destroying"
        if "$HERE/validate-topology.sh" --topology "$topology" --destroy-only \
            >> "$ARTIFACT_DIR/sweep.log" 2>&1; then
            log "sweep: $topology destroyed"
        else
            log "sweep: $topology WOULD NOT DESTROY -- it is still billing. See $ARTIFACT_DIR/sweep.log"
            echo "sweep $topology: FAIL" >> "$VERDICT"
        fi
    done
    [ "$found" = 0 ] && log "sweep: nothing left behind"
}

# Run the sweep before the topologies as well as after: a leftover cluster from a
# killed run is both a bill and a second set of resources with our names on it.
log "artifacts in $ARTIFACT_DIR"
sweep

if [ "$SWEEP_ONLY" = 1 ]; then
    log "--sweep-only: done"
    exit 0
fi

# Built once for every topology. A per-topology build would prove three different
# sets of bytes and could not say which one failed.
DISTRO_TARBALL=""
if [ "$SOURCE" = tree ]; then
    log "building $REF"
    DISTRO_TARBALL="$ARTIFACT_DIR/spinifex-distro.tar.gz"
    if [ "$DRY_RUN" = 1 ]; then
        log "dry run: would build $REF into $DISTRO_TARBALL"
    else
        "$SPINIFEX_ROOT/../scripts/tofu-cluster/build-install-artifacts.sh" "$REF" \
            "$DISTRO_TARBALL" "$ARTIFACT_DIR/setup.sh" "$ARTIFACT_DIR/spinifex-tests.tar" \
            > "$ARTIFACT_DIR/build.log" 2>&1 \
            || die "building $REF failed; see $ARTIFACT_DIR/build.log"
        log "built $(du -h "$DISTRO_TARBALL" | cut -f1) from $REF"
    fi
fi

RUN_RC=0
for topology in $TOPOLOGIES; do
    args=(--topology "$topology" --ssh-public-key "$SSH_PUBLIC_KEY" --ssh-private-key "$SSH_PRIVATE_KEY")
    [ "$DRY_RUN" = 1 ] && args+=(--dry-run)
    [ "${OCI_INSTANCE_PRINCIPAL:-0}" = 1 ] && args+=(--instance-principal) || args+=(--no-external-pool)
    [ -n "$DISTRO_TARBALL" ] && args+=(--distro "$DISTRO_TARBALL" --setup-sh "$ARTIFACT_DIR/setup.sh")
    [ "${OCI_KEEP_ON_FAIL:-0}" = 1 ] && args+=(--keep-on-fail)
    [ -n "${WORKBOOKS+x}" ] && args+=(--workbooks "$WORKBOOKS")

    # go test -v markers, so the same log feeds go-junit-report and the RCA bundle
    # the Go suites produce. One testcase per topology.
    tname="TestOCITopology_${topology//-/_}"
    start=$SECONDS
    echo "=== RUN   $tname"
    if "$HERE/validate-topology.sh" "${args[@]}" 2>&1 | tee "$ARTIFACT_DIR/$topology.log"; then
        printf -- '--- PASS: %s (%d.00s)\n' "$tname" "$((SECONDS - start))"
        echo "$topology: PASS" >> "$VERDICT"
    else
        printf -- '--- FAIL: %s (%d.00s)\n' "$tname" "$((SECONDS - start))"
        echo "$topology: FAIL" >> "$VERDICT"
        RUN_RC=1
    fi
done

# Again at the end, because a topology that failed its own teardown is exactly the
# case that costs money, and the verdict should say so rather than the log.
sweep

echo
log "=== $REF on OCI ($SOURCE build) ==="
cat "$VERDICT"
if [ "$RUN_RC" = 0 ] && ! grep -q FAIL "$VERDICT"; then
    log "PASS"
    printf 'ok  \te2e-oci\t%d.000s\n' "$SECONDS"
else
    log "FAIL"
    printf 'FAIL\te2e-oci\t%d.000s\n' "$SECONDS"
fi
exit "$RUN_RC"
