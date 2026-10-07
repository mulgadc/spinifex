#!/bin/bash
# setup-identity.sh -- create the dynamic group and policy that let Spinifex nodes
# authenticate to the OCI API as themselves, so no API key is installed on any node.
#
# Run once per tenancy, with a tenancy-admin principal. Every deployment afterwards
# passes instance_principal = "adopt", which references these by nothing at all: the
# matching rule covers the whole compartment, so it names no instance and needs no
# OCID from here.
#
# Its own state file, deliberately. validate-topology.sh destroys its state at the
# end of every run, and these two resources outlive every topology -- holding them
# in a per-topology state would let a nightly teardown delete the tenancy's policy.
#
# MUST MAINTAIN BASH 3.X COMPATABILITY. Operators run this on macOS, which ships
# bash 3.2: no declare -A, no mapfile, and under set -u an empty "${arr[@]}" is an
# unbound variable, so guard every such expansion with ${#arr[@]}.
set -uo pipefail

HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
STATE_DIR="${OCI_STATE_ROOT:-$HERE}/.identity"
SSH_PUBLIC_KEY="${OCI_SSH_PUBLIC_KEY:-$HOME/.ssh/oci-spx.pub}"
DESTROY=0
DRY_RUN=0

log() { printf '[setup-identity] %s\n' "$*"; }
die() {
    printf '[setup-identity] ERROR: %s\n' "$*" >&2
    exit 1
}

usage() {
    cat >&2 <<EOF
usage: ${0##*/} [--dry-run] [--destroy]

Creates the OCI dynamic group and policy for instance-principal authentication.
Needs a tenancy-admin OCI credential; the compartment-scoped user the rest of this
configuration runs as cannot create either resource.

  --dry-run   Show the plan, change nothing.
  --destroy   Remove them again. Every node using "adopt" loses its credential.
EOF
    exit 2
}

while [ $# -gt 0 ]; do
    case "$1" in
        --dry-run) DRY_RUN=1; shift ;;
        --destroy) DESTROY=1; shift ;;
        -h | --help) usage ;;
        *) die "unknown option $1" ;;
    esac
done

[ -r "$SSH_PUBLIC_KEY" ] || die "no readable SSH public key at $SSH_PUBLIC_KEY (set OCI_SSH_PUBLIC_KEY)"
mkdir -p "$STATE_DIR"

# The instance and network resources are not targeted, so nothing else in this
# configuration is created or read. A tenancy admin running this does not need to
# hold the compartment rights the rest of it uses.
targets=(-target oci_identity_dynamic_group.nodes -target oci_identity_policy.nodes)

tf() {
    python3 "$HERE/scripts/oci_env.py" --ssh-public-key-path "$SSH_PUBLIC_KEY" -- \
        terraform -chdir="$HERE" "$@"
}

tf init -input=false -upgrade >/dev/null || die "terraform init failed"

if [ "$DESTROY" = 1 ]; then
    log "destroying the dynamic group and policy"
    tf destroy -auto-approve -var 'instance_principal=create' \
        -state "$STATE_DIR/terraform.tfstate" "${targets[@]}" \
        || die "destroy failed"
    log "removed. Any node configured for instance principal can no longer allocate an address."
    exit 0
fi

args=(-var 'instance_principal=create' -state "$STATE_DIR/terraform.tfstate" "${targets[@]}")

if [ "$DRY_RUN" = 1 ]; then
    log "dry run: planning the dynamic group and policy only"
    tf plan "${args[@]}" || die "plan failed"
    exit 0
fi

log "creating the dynamic group and policy (tenancy-root resources)"
tf apply -auto-approve "${args[@]}" || die "apply failed -- a tenancy-admin principal is required"

log "done. State in $STATE_DIR/terraform.tfstate -- keep it, it is the only record."
log "Deployments can now use instance_principal = \"adopt\" with a compartment-scoped user."
