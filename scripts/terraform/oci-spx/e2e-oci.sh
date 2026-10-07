#!/bin/bash
# e2e-oci.sh — prove a ref works on OCI, across every topology, and leave nothing
# running. One entry point for a workstation, a Cloudflare-dispatched nightly and
# the GitHub workflow, because three ways of starting it would drift into three
# different answers about whether OCI works.
#
# Configured entirely by environment variables so a dispatch needs no arguments:
#
#   SPX_REF           Ref to prove. Default: the spinifex checkout's current branch.
#   OCI_TOPOLOGIES    Space-separated. Default: "vm-single vm-multi bm
#                     vm-single-principal". Set one name to test one thing: a
#                     single VM topology is roughly a quarter of the full run.
#   OCI_SOURCE        tree (build SPX_REF) or release (the published installer).
#   OCI_VERSION       With OCI_SOURCE=release, install this exact release tag and
#                     ignore OCI_CHANNEL. Preferred for anything repeatable: a tag
#                     resolves by redirect, while the dev channel resolves through a
#                     rate-limited GitHub API call that 404s when it trips.
#   OCI_CHANNEL       With OCI_SOURCE=release, which published channel to install:
#                     latest (default) or dev, the newest prerelease. dev is the
#                     path our cloud contacts are given, so it is worth testing.
#                     Default tree: a nightly exists to judge a ref, and the
#                     published installer says nothing about one.
#   WORKBOOKS         Workbooks to run on each topology. Default: the driver's own
#                     list. Set to "" to skip the workbook phase entirely.
#   OCI_INSTANCE_PRINCIPAL  Authenticate the node's allocator as the instance
#                     principal. Default 1, and it needs the tenancy's dynamic
#                     group to exist. The vm-single-principal topology ignores it
#                     and always authenticates that way, which is its purpose.
#
#                     A tenancy where setup-identity.sh cannot run has no such
#                     group, and then no principal run can pass: the node forms,
#                     authenticates, and is refused every allocation. That now
#                     fails at the allocator gate within a minute of the daemon
#                     starting rather than an hour later inside a workbook, but
#                     it still costs a full provision, so drop the topology from
#                     OCI_TOPOLOGIES until the group exists.
#   OCI_NO_EXTERNAL_POOL    1 to form with no allocator at all, for a tenancy whose
#                     dynamic group does not exist yet. The allocator and every
#                     workbook are then recorded SKIPPED, never PASS, so a green
#                     run with this set is not a claim about guest networking.
#                     Not applied to vm-single-principal, which the allocator is
#                     the entire test of; that pairing is refused rather than run.
#   OCI_SSH_PUBLIC_KEY / OCI_SSH_PRIVATE_KEY   Paths. Default ~/.ssh/oci-spx[.pub].
#   OCI_CREDENTIAL_HOOK     Executable run on each topology after formation and
#                     before the pool, as "hook <ssh-key> <host>...". An API-key
#                     deployment installs its credential here; instance principal
#                     needs none, so it is skipped unless the pool is key-based.
#   OCI_PRINCIPAL_TENANCY_OCID, OCI_PRINCIPAL_USER_OCID,
#   OCI_PRINCIPAL_FINGERPRINT, OCI_PRINCIPAL_PRIVATE_KEY, OCI_PRINCIPAL_REGION,
#   OCI_PRINCIPAL_COMPARTMENT_OCID
#                     The credential vm-single-principal builds with, replacing the
#                     run's own for that one topology. The dynamic group is a
#                     tenancy-root resource, so a tenancy that will not let
#                     setup-identity.sh create one refuses every allocation however
#                     correct the code is, and the principal route has to be proved
#                     in a tenancy we administer while the rest of the run stays
#                     where it is.
#
#                     All six or none. A partial set builds one tenancy's instances
#                     against another's compartment, which is refused rather than
#                     reported; with none set the topology is recorded SKIPPED
#                     rather than costing a provision to fail. Running it in the
#                     run's own tenancy is still available by pointing all six
#                     there, which says so explicitly instead of by default.
#   OCI_PRINCIPAL_SHAPE     Compute shape for vm-single-principal. Its tenancy
#                     carries its own service limits, so the shape the other
#                     topologies use can have zero quota there and fail the apply
#                     before anything about the principal is exercised. Default
#                     VM.Standard.E6.Flex, as the others.
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
# On a workstation spinifex is a submodule of mulga; in CI the two are siblings
# under the workspace. Deriving one from the other is only right in the first case.
MULGA_ROOT="${MULGA_ROOT:-$SPINIFEX_ROOT/..}"

TOPOLOGIES="${OCI_TOPOLOGIES:-vm-single vm-multi bm vm-single-principal}"
SOURCE="${OCI_SOURCE:-tree}"
CHANNEL="${OCI_CHANNEL:-latest}"
INSTALL_VERSION="${OCI_VERSION:-}"
REF="${SPX_REF:-$(git -C "$SPINIFEX_ROOT" rev-parse --abbrev-ref HEAD)}"
SSH_PUBLIC_KEY="${OCI_SSH_PUBLIC_KEY:-$HOME/.ssh/oci-spx.pub}"
SSH_PRIVATE_KEY="${OCI_SSH_PRIVATE_KEY:-$HOME/.ssh/oci-spx}"
ARTIFACT_DIR="${OCI_ARTIFACT_DIR:-$HERE/.e2e-oci-$(date -u +%Y%m%dT%H%M%SZ)}"
# Exported so validate-topology.sh agrees with the sweep about where state lives.
# On a persistent runner this belongs outside the checkout: actions/checkout runs
# git clean -ffdx, which would delete the state of a killed run before the sweep.
export OCI_STATE_ROOT="${OCI_STATE_ROOT:-$HERE}"
STATE_ROOT="$OCI_STATE_ROOT"
SWEEP_ONLY=0
SUMMARY_ONLY=0
DRY_RUN=0

log() { printf '[e2e-oci] %s\n' "$*"; }
die() {
    printf '[e2e-oci] ERROR: %s\n' "$*" >&2
    exit 1
}

usage() {
    cat >&2 <<EOF
usage: ${0##*/} [--sweep-only] [--summary-only] [--dry-run]

Everything else is an environment variable; see the header of this script.

  --sweep-only   Destroy anything an earlier run left behind, then stop.
  --summary-only Write the result tables from the state files, then stop. For
                 a job that was cancelled or killed before it got there.
  --dry-run      Print what each topology would do, build nothing.
EOF
    exit 2
}

while [ $# -gt 0 ]; do
    case "$1" in
        --sweep-only) SWEEP_ONLY=1; shift ;;
        --summary-only) SUMMARY_ONLY=1; shift ;;
        --dry-run) DRY_RUN=1; shift ;;
        -h | --help) usage ;;
        *) die "unknown option $1" ;;
    esac
done

case "$SOURCE" in
    tree | release) ;;
    *) die "OCI_SOURCE must be tree or release, got '$SOURCE'" ;;
esac

case "$CHANNEL" in
    latest | dev) ;;
    *) die "OCI_CHANNEL must be latest or dev, got '$CHANNEL'" ;;
esac
# Saying release+dev and tree at once is two different artifacts in one verdict.
[ "$SOURCE" = tree ] && [ -n "$INSTALL_VERSION" ] \
    && die "OCI_VERSION needs OCI_SOURCE=release: a tree build installs the ref, not a published tag"
[ "$SOURCE" = tree ] && [ "$CHANNEL" != latest ] \
    && die "OCI_CHANNEL=$CHANNEL needs OCI_SOURCE=release: a tree build installs the ref, not a channel"

mkdir -p "$ARTIFACT_DIR"
VERDICT="$ARTIFACT_DIR/verdict.txt"
: > "$VERDICT"

# vm-single-principal is the one topology that cannot run wherever the others do.
# It needs the tenancy's dynamic group, and creating that is a tenancy-root write,
# so the tenancy a shared test account gives us can never pass it. These let it
# build somewhere else without moving the rest of the run.
PRINCIPAL_CRED_VARS="TENANCY_OCID USER_OCID FINGERPRINT PRIVATE_KEY REGION COMPARTMENT_OCID"

# principal_cred_state is none, all or partial. Partial is the one worth a stop:
# one tenancy's OCIDs with another's compartment builds in a compartment nobody
# chose, and the apply reports success doing it.
principal_cred_state() {
    local name var have=0 miss=0
    for name in $PRINCIPAL_CRED_VARS; do
        var="OCI_PRINCIPAL_$name"
        if [ -n "${!var:-}" ]; then
            have=$((have + 1))
        else
            miss=$((miss + 1))
        fi
    done
    if [ "$have" = 0 ]; then
        printf 'none\n'
    elif [ "$miss" = 0 ]; then
        printf 'all\n'
    else
        printf 'partial\n'
    fi
}

# Into an array rather than a rendered string, because the private key is a
# multi-line PEM and any line-based plumbing silently truncates it to its header.
PRINCIPAL_ENV=()
build_principal_env() {
    local name var
    PRINCIPAL_ENV=()
    for name in $PRINCIPAL_CRED_VARS; do
        var="OCI_PRINCIPAL_$name"
        case "$name" in
            COMPARTMENT_OCID) PRINCIPAL_ENV+=("TF_VAR_compartment_ocid=${!var}") ;;
            *) PRINCIPAL_ENV+=("OCI_$name=${!var}") ;;
        esac
    done
}

# Here rather than at the topology, because the build runs first and takes minutes,
# and a credential that cannot work is knowable before any of it. The loop checks
# again, since it is what reads the variables.
case " $TOPOLOGIES " in
    *" vm-single-principal "*)
        [ "$(principal_cred_state)" != partial ] \
            || die "the OCI_PRINCIPAL_* credential is incomplete: set all of $PRINCIPAL_CRED_VARS or none of them"
        ;;
esac

# The run page is the evidence, and its readers are not us: someone assessing
# whether Spinifex works on their cloud should get the answer without opening a
# log or knowing what this harness is. Markdown on stdout as well as to the step
# summary, so a workstation run shows the same thing CI publishes.
mark() {
    case "$1" in
        PASS) printf '✅ PASS' ;;
        FAIL) printf '❌ FAIL' ;;
        SKIPPED) printf '⏭️ SKIPPED' ;;
        *) printf 'ℹ️ %s' "$1" ;;
    esac
}

summary() {
    printf '## Spinifex on OCI — %s\n\n' "$REF"
    # Said, not implied. A tree build and a published release answer different
    # questions, and a table headed "published release" over a build of somebody's
    # branch is the one error here that would mislead a reader who trusted it.
    if [ "$SOURCE" = tree ]; then
        printf 'Built from this ref, region `%s`.\n\n' "${OCI_REGION:-ap-sydney-1}"
    else
        printf 'Published release `%s`, region `%s`.\n\n' \
            "${INSTALL_VERSION:-$CHANNEL channel}" "${OCI_REGION:-ap-sydney-1}"
    fi

    # In the header, not left to a SKIPPED row further down. A run with no allocator
    # is not evidence about public addressing, and that is precisely the claim a
    # reader of a green table would otherwise take from it.
    # Said per topology rather than once for the run: vm-single-principal pins its
    # own credential, so a single claim over a mixed run is wrong for one of them.
    if [ "${OCI_NO_EXTERNAL_POOL:-0}" = 1 ]; then
        printf '> **No external address pool.** Public-address allocation and every workbook needing a public address were skipped, so nothing here speaks to guest ingress. `vm-single-principal`, if it ran, is the exception and did build one.\n\n'
    elif [ "${OCI_INSTANCE_PRINCIPAL:-1}" = 1 ]; then
        printf 'Allocator authenticated as the instance principal.\n\n'
    else
        printf 'Allocator authenticated with an API key, except `vm-single-principal` which authenticates as the instance.\n\n'
    fi
    # Said in the summary because the row is otherwise read as a result about the
    # same tenancy as every row above it, which is the one thing it is not.
    if [ "$(principal_cred_state)" = all ]; then
        printf '`vm-single-principal` built in its own tenancy in `%s`, from the `OCI_PRINCIPAL_*` credential.\n\n' \
            "$OCI_PRINCIPAL_REGION"
    fi

    local topology results workbooks gate status detail name secs
    for topology in $TOPOLOGIES; do
        results="$STATE_ROOT/.validate-$topology/results.tsv"
        workbooks="$STATE_ROOT/.validate-$topology/workbooks.tsv"
        printf '### `%s`\n\n' "$topology"
        # A topology the loop never reached is said so rather than omitted: a
        # reader takes an absent row for a passing one.
        if [ ! -s "$results" ]; then
            printf 'Did not run.\n\n'
            continue
        fi
        printf '| Gate | Result | Detail |\n| --- | --- | --- |\n'
        while IFS=$'\t' read -r gate status detail; do
            [ -n "$gate" ] || continue
            printf '| %s | %s | %s |\n' "$gate" "$(mark "$status")" "$detail"
        done < "$results"
        printf '\n'
        if [ ! -s "$workbooks" ]; then
            printf 'No published workbooks ran.\n\n'
            continue
        fi
        printf '| Terraform workbook | Result | Duration |\n| --- | --- | --- |\n'
        while IFS=$'\t' read -r name status secs; do
            [ -n "$name" ] || continue
            printf '| `%s` | %s | %ss |\n' "$name" "$(mark "$status")" "$secs"
        done < "$workbooks"
        printf '\n'
    done
}

# GITHUB_STEP_SUMMARY is per step, so appending from both the run and the dedicated
# publish step would put two copies of the tables on the run page. One owner: the run
# writes the file and prints it, and only --summary-only publishes it.
emit_summary() {
    summary | tee "$ARTIFACT_DIR/summary.md"
}

publish_summary() {
    emit_summary
    if [ -n "${GITHUB_STEP_SUMMARY:-}" ]; then
        cat "$ARTIFACT_DIR/summary.md" >> "$GITHUB_STEP_SUMMARY"
    fi
}

# A topology destroys itself from its own EXIT trap, so this only catches a run
# the trap never reached: a killed process, a runner reclaimed mid-job, a reboot.
# It works from the state files rather than by name filter, so it can only ever
# destroy what one of these runs created.
sweep() {
    local found=0 dir topology
    local sweep_env=(env)
    for dir in "$STATE_ROOT"/.validate-*; do
        [ -s "$dir/terraform.tfstate" ] || continue
        python3 -c '
import json, sys
state = json.load(open(sys.argv[1]))
sys.exit(0 if any(r["instances"] for r in state.get("resources", [])) else 1)
' "$dir/terraform.tfstate" 2>/dev/null || continue
        topology="${dir##*/.validate-}"
        # The child reads OCI_STATE_ROOT from the environment, so it looks in the
        # same place this loop found the state rather than beside its own script.
        found=1
        # The same credential the topology built with, or the destroy authenticates
        # against a tenancy that never held these resources and reports a clean
        # sweep over a deployment that is still billing.
        sweep_env=(env)
        if [ "$topology" = vm-single-principal ] && [ "$(principal_cred_state)" = all ]; then
            build_principal_env
            sweep_env=(env "${PRINCIPAL_ENV[@]}")
        fi
        log "sweep: $topology still holds resources, destroying"
        if "${sweep_env[@]}" "$HERE/validate-topology.sh" --topology "$topology" --destroy-only \
            >> "$ARTIFACT_DIR/sweep.log" 2>&1; then
            log "sweep: $topology destroyed"
        else
            log "sweep: $topology WOULD NOT DESTROY -- it is still billing. See $ARTIFACT_DIR/sweep.log"
            echo "sweep $topology: FAIL" >> "$VERDICT"
        fi
    done
    [ "$found" = 0 ] && log "sweep: nothing left behind"
}

# Before the sweep, because the sweep is what destroys the state the tables are
# read from. This is the path a cancelled job takes, so it has to work on whatever
# the run got through rather than assuming a complete set.
if [ "$SUMMARY_ONLY" = 1 ]; then
    publish_summary
    exit 0
fi

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
    BUILDER="$MULGA_ROOT/scripts/tofu-cluster/build-install-artifacts.sh"
    [ -x "$BUILDER" ] || die "no artifact builder at $BUILDER (set MULGA_ROOT)"
    # It clones each repo from GitHub rather than using the checkout beside it, so a
    # ref that exists only on this workstation silently falls back to dev and the
    # run would report a verdict about the wrong code. Fail on that instead.
    if ! git -C "$SPINIFEX_ROOT" rev-parse --verify --quiet "refs/remotes/origin/$REF" >/dev/null \
        && ! git -C "$SPINIFEX_ROOT" ls-remote --exit-code --tags origin "$REF" >/dev/null 2>&1; then
        die "$REF is not on origin: the builder clones from GitHub, so an unpushed ref would fall back to dev"
    fi
    if [ "$DRY_RUN" = 1 ]; then
        log "dry run: would build $REF into $DISTRO_TARBALL"
    else
        # --skip-e2e-build: the Go suites are not run here. The workbook driver is a
        # shell script, so building their binaries would add minutes and prove nothing.
        "$BUILDER" "$REF" "$DISTRO_TARBALL" "$ARTIFACT_DIR/setup.sh" \
            "$ARTIFACT_DIR/spinifex-tests.tar" --skip-e2e-build \
            > "$ARTIFACT_DIR/build.log" 2>&1 \
            || die "building $REF failed; see $ARTIFACT_DIR/build.log"
        log "built $(du -h "$DISTRO_TARBALL" | cut -f1) from $REF"
    fi
fi

RUN_RC=0
for topology in $TOPOLOGIES; do
    # The credential swap happens here and nowhere else, so everything downstream
    # of the one validate call is unaware the topology built somewhere different.
    # Always a bare `env` otherwise, because an empty array is an unbound variable
    # under set -u on the bash 3.2 operators run this with.
    run_env=(env)
    if [ "$topology" = vm-single-principal ]; then
        case "$(principal_cred_state)" in
            none)
                log "$topology: SKIPPED -- no OCI_PRINCIPAL_* credential, and the run's own tenancy cannot hold the dynamic group this topology needs"
                echo "$topology: SKIPPED (no principal credential)" >> "$VERDICT"
                continue
                ;;
            partial)
                die "the OCI_PRINCIPAL_* credential is incomplete: set all of $PRINCIPAL_CRED_VARS or none of them"
                ;;
        esac
        build_principal_env
        run_env=(env "${PRINCIPAL_ENV[@]}")
    fi
    args=(--topology "$topology" --ssh-public-key "$SSH_PUBLIC_KEY" --ssh-private-key "$SSH_PRIVATE_KEY")
    [ "$DRY_RUN" = 1 ] && args+=(--dry-run)
    # Two separate choices. --no-external-pool is the one that forms without any
    # allocator, for a tenancy whose dynamic group does not exist yet; it records
    # the allocator and the workbooks as SKIPPED rather than passing them.
    #
    # vm-single-principal opts out of both: it sets its own credential, and a run
    # of it without an allocator would exercise nothing. validate-topology.sh
    # refuses that pairing, so skipping it here keeps a mixed run from aborting.
    if [ "$topology" = vm-single-principal ]; then
        :
    elif [ "${OCI_NO_EXTERNAL_POOL:-0}" = 1 ]; then
        args+=(--no-external-pool)
    elif [ "${OCI_INSTANCE_PRINCIPAL:-1}" = 1 ]; then
        args+=(--instance-principal)
    fi
    # Not under --dry-run, where the build was skipped and naming the tarball it
    # would have produced fails the readable check, so every topology reported FAIL
    # and the flag operators are told to run first could never pass. Still
    # unconditional in a real run, so a build that produced nothing stays loud
    # rather than quietly installing a release instead.
    if [ -n "$DISTRO_TARBALL" ] && [ "$DRY_RUN" != 1 ]; then
        args+=(--distro "$DISTRO_TARBALL" --setup-sh "$ARTIFACT_DIR/setup.sh")
    fi
    if [ "$SOURCE" = release ]; then
        if [ -n "$INSTALL_VERSION" ]; then
            args+=(--version "$INSTALL_VERSION")
        else
            args+=(--channel "$CHANNEL")
        fi
    fi
    [ -n "${OCI_CREDENTIAL_HOOK:-}" ] && args+=(--credential-hook "$OCI_CREDENTIAL_HOOK")
    [ "${OCI_KEEP_ON_FAIL:-0}" = 1 ] && args+=(--keep-on-fail)
    [ -n "${WORKBOOKS+x}" ] && args+=(--workbooks "$WORKBOOKS")

    # go test -v markers, so the same log feeds go-junit-report and the RCA bundle
    # the Go suites produce. One testcase per topology.
    tname="TestOCITopology_${topology//-/_}"
    start=$SECONDS
    echo "=== RUN   $tname"
    if "${run_env[@]}" "$HERE/validate-topology.sh" "${args[@]}" 2>&1 | tee "$ARTIFACT_DIR/$topology.log"; then
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

emit_summary


echo
log "=== $REF on OCI ($SOURCE build$([ "$SOURCE" = release ] && { [ -n "$INSTALL_VERSION" ] && echo ", $INSTALL_VERSION" || echo ", $CHANNEL channel"; })) ==="
cat "$VERDICT"
if [ "$RUN_RC" = 0 ] && ! grep -q FAIL "$VERDICT"; then
    log "PASS"
    printf 'ok  \te2e-oci\t%d.000s\n' "$SECONDS"
else
    log "FAIL"
    printf 'FAIL\te2e-oci\t%d.000s\n' "$SECONDS"
fi
exit "$RUN_RC"
