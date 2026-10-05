#!/usr/bin/env bash
# Installs the OCI credential the external-address allocator authenticates with
# on every node, as /etc/spinifex/oci/{config,oci_api_key.pem}.
#
# Usage: spx-oci-config.sh [--dry-run] <ssh-private-key> <host>...
#
# The argument shape is validate-topology.sh's credential-hook contract, so this
# drops straight into --credential-hook or $OCI_CREDENTIAL_HOOK.
set -euo pipefail

HERE=$(CDPATH='' cd -- "$(dirname -- "$0")" && pwd)
SELF="$HERE/$(basename -- "$0")"

# What the allocator reads, from the pool fragment Terraform stages on each node.
# The config path and the profile name are the contract; the key filename is only
# ever named by key_file inside that config.
REMOTE_DIR=/etc/spinifex/oci
REMOTE_CONFIG="$REMOTE_DIR/config"
REMOTE_PEM="$REMOTE_DIR/oci_api_key.pem"
REMOTE_PROFILE=spinifex

SPX_KEY="${OCI_SPX_KEY:-$HOME/.oci/oci_api_key_spx.pem}"
SPX_PROFILE="${OCI_SPX_PROFILE:-spinifex}"
SSH_OPTS=(-o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null
          -o LogLevel=ERROR -o BatchMode=yes -o ConnectTimeout=15)
DRY_RUN=0

die() { echo "$(basename -- "$0"): $*" >&2; exit 1; }
log() { echo "[spx-oci-config] $*"; }

usage() {
    sed -n '2,8p' "$SELF" | sed 's/^# \{0,1\}//'
    cat <<'EOF'

Options:
  --dry-run   Resolve and validate the credential, print what would be written,
              and touch no host.
  -h, --help  This text.

The credential is resolved in this order, and the first one that is complete wins:

  1. $OCI_SPX_PRIVATE_KEY with $OCI_SPX_USER_OCID, $OCI_SPX_FINGERPRINT,
     $OCI_SPX_TENANCY_OCID and $OCI_SPX_REGION. A dedicated narrow credential,
     which is the shape a CI secret store holds. A partial set is an error.
  2. The [spinifex] profile in ~/.oci/config, or $OCI_SPX_PROFILE.
  3. ~/.oci/oci_api_key_spx.pem, or $OCI_SPX_KEY, with the user, tenancy and
     region of the profile Terraform itself uses. The fingerprint is derived
     from the key rather than read from the config.
  4. The credential Terraform builds with. This works and is wider than the
     allocator needs, so it warns.

Rules 2 to 4 resolve the Terraform profile through scripts/oci_env.py, so the
precedence is the same one the apply used and the two cannot disagree.
EOF
}

while [ "$#" -gt 0 ]; do
    case "$1" in
        --dry-run) DRY_RUN=1; shift ;;
        -h|--help) usage; exit 0 ;;
        --) shift; break ;;
        -*) die "unknown option: $1" ;;
        *) break ;;
    esac
done

# oci_env.py owns credential resolution for this directory: the environment route
# a runner uses, ~/.oci/config otherwise, a partial set treated as an error, and
# the shape checks. Re-exec under it rather than reimplement any of that here.
if [ -z "${SPX_OCI_ENV_READY:-}" ]; then
    [ -x "$HERE/scripts/oci_env.py" ] || [ -r "$HERE/scripts/oci_env.py" ] \
        || die "scripts/oci_env.py was not found next to $SELF"
    if [ "$DRY_RUN" = 1 ]; then
        exec env SPX_OCI_ENV_READY=1 python3 "$HERE/scripts/oci_env.py" -- "$SELF" --dry-run "$@"
    fi
    exec env SPX_OCI_ENV_READY=1 python3 "$HERE/scripts/oci_env.py" -- "$SELF" "$@"
fi

# The same argument shape in both modes, so a --dry-run rehearsal and the real run
# differ by one flag. Only the checks that need a host are skipped.
ssh_key="${1:?usage: $(basename -- "$0") [--dry-run] <ssh-private-key> <host>...}"
shift
if [ "$DRY_RUN" != 1 ]; then
    [ "$#" -gt 0 ] || die "no hosts given"
    [ -r "$ssh_key" ] || die "SSH private key is not readable: $ssh_key"
fi

# A key's fingerprint is a property of the key. Deriving it beats trusting a
# config field, because a stale fingerprint beside a good key is a 401 that names
# neither.
fingerprint_of() {
    openssl rsa -pubout -outform DER -in "$1" 2>/dev/null \
        | openssl md5 -c 2>/dev/null | awk '{print $NF}'
}

require_complete() {
    local source="$1" missing="" name
    shift
    for name in "$@"; do
        [ -n "${!name:-}" ] || missing="$missing $name"
    done
    [ -z "$missing" ] || die "$source is incomplete, missing:$missing"
}

key_pem=""      # the PEM itself, never a path, so nothing is read twice
oci_user=""
oci_fingerprint=""
oci_tenancy=""
oci_region=""
source_name=""

if [ -n "${OCI_SPX_PRIVATE_KEY:-}${OCI_SPX_USER_OCID:-}${OCI_SPX_FINGERPRINT:-}${OCI_SPX_TENANCY_OCID:-}${OCI_SPX_REGION:-}" ]; then
    require_complete "the OCI_SPX_* credential in the environment" \
        OCI_SPX_PRIVATE_KEY OCI_SPX_USER_OCID OCI_SPX_FINGERPRINT \
        OCI_SPX_TENANCY_OCID OCI_SPX_REGION
    # Base64 accepted by shape, because a PEM's newlines do not survive every
    # secret store, and detected rather than selected by a second variable.
    case "$OCI_SPX_PRIVATE_KEY" in
        *-----BEGIN*) key_pem="$OCI_SPX_PRIVATE_KEY" ;;
        *) key_pem=$(printf '%s' "$OCI_SPX_PRIVATE_KEY" | tr -d ' \n' | base64 -d 2>/dev/null) || true ;;
    esac
    case "$key_pem" in
        *-----BEGIN*) ;;
        *) die "OCI_SPX_PRIVATE_KEY is neither a PEM nor a base64-encoded PEM" ;;
    esac
    oci_user="$OCI_SPX_USER_OCID"
    oci_fingerprint="$OCI_SPX_FINGERPRINT"
    oci_tenancy="$OCI_SPX_TENANCY_OCID"
    oci_region="$OCI_SPX_REGION"
    source_name="the OCI_SPX_* environment credential"

elif [ -r "$HOME/.oci/config" ] && python3 - "$SPX_PROFILE" <<'PY'
import configparser, pathlib, sys
cfg = configparser.RawConfigParser()
cfg.read(pathlib.Path.home() / ".oci" / "config")
sys.exit(0 if cfg.has_section(sys.argv[1]) else 1)
PY
then
    # One pipe-separated line read into named variables, rather than eval of
    # generated shell: none of these fields can contain a pipe.
    profile_fields=$(python3 - "$SPX_PROFILE" <<'PY'
import configparser, pathlib, sys
cfg = configparser.RawConfigParser()
cfg.read(pathlib.Path.home() / ".oci" / "config")
section = dict(cfg[sys.argv[1]])
fields = ("user", "fingerprint", "tenancy", "region", "key_file")
missing = [f for f in fields if not section.get(f)]
if missing:
    sys.exit(f"profile {sys.argv[1]} has no {', '.join(missing)}")
print("|".join(section[f] for f in fields))
PY
) || die "could not read the [$SPX_PROFILE] profile from ~/.oci/config"
    IFS='|' read -r profile_user profile_fingerprint profile_tenancy \
        profile_region profile_key_file <<EOF
$profile_fields
EOF
    profile_key=$(printf '%s' "$profile_key_file" | sed "s|^~|$HOME|")
    [ -r "$profile_key" ] || die "key_file in [$SPX_PROFILE] is not readable: $profile_key"
    key_pem=$(cat "$profile_key")
    oci_user="$profile_user"
    oci_fingerprint="$profile_fingerprint"
    oci_tenancy="$profile_tenancy"
    oci_region="$profile_region"
    source_name="the [$SPX_PROFILE] profile in ~/.oci/config"

elif [ -r "$SPX_KEY" ]; then
    require_complete "the credential oci_env.py resolved" \
        TF_VAR_user_ocid TF_VAR_tenancy_ocid TF_VAR_region
    key_pem=$(cat "$SPX_KEY")
    oci_user="${TF_VAR_user_ocid:-}"
    oci_fingerprint=$(fingerprint_of "$SPX_KEY")
    oci_tenancy="${TF_VAR_tenancy_ocid:-}"
    oci_region="${TF_VAR_region:-}"
    source_name="$SPX_KEY with the user and tenancy of the Terraform credential"
    [ -n "$oci_fingerprint" ] || die "could not read an RSA public key out of $SPX_KEY"

else
    require_complete "the credential oci_env.py resolved" \
        TF_VAR_user_ocid TF_VAR_fingerprint TF_VAR_tenancy_ocid TF_VAR_region
    if [ -n "${TF_VAR_private_key:-}" ]; then
        key_pem="$TF_VAR_private_key"
    elif [ -n "${TF_VAR_private_key_path:-}" ] && [ -r "$TF_VAR_private_key_path" ]; then
        key_pem=$(cat "$TF_VAR_private_key_path")
    else
        die "no node credential found: set OCI_SPX_PRIVATE_KEY, add a [$SPX_PROFILE] profile to ~/.oci/config, or create $SPX_KEY"
    fi
    oci_user="${TF_VAR_user_ocid:-}"
    oci_fingerprint="${TF_VAR_fingerprint:-}"
    oci_tenancy="${TF_VAR_tenancy_ocid:-}"
    oci_region="${TF_VAR_region:-}"
    source_name="the credential Terraform builds with"
    log "WARNING: falling back to $source_name, which is wider than the allocator needs."
    log "WARNING: see the OCI guide for a user scoped to vnics, private-ips and public-ips."
fi

case "$oci_user$oci_tenancy" in
    ocid1.*) ;;
    *) die "$source_name did not yield OCIDs for the user and tenancy" ;;
esac
echo "$oci_fingerprint" | grep -Eq '^[0-9a-f]{2}(:[0-9a-f]{2}){15}$' \
    || die "$source_name has no 16-byte hex fingerprint: '$oci_fingerprint'"

# The fingerprint and the key have to be the same key, and nothing upstream checks
# that they are. Unchecked it is an OCI 401 at the first allocation, long after the
# deploy reports success.
pem_tmp=$(mktemp); trap 'rm -f "$pem_tmp"' EXIT
( umask 077; printf '%s\n' "$key_pem" > "$pem_tmp" )
derived=$(fingerprint_of "$pem_tmp")
[ -n "$derived" ] || die "$source_name does not hold a usable RSA private key"
[ "$derived" = "$oci_fingerprint" ] \
    || die "the key from $source_name has fingerprint $derived, but the credential names $oci_fingerprint"

log "credential: $source_name"
log "user: $oci_user"
log "fingerprint: $oci_fingerprint"
log "region: $oci_region"

if [ "$DRY_RUN" = 1 ]; then
    log "would write $REMOTE_CONFIG and $REMOTE_PEM as 0640 root:spinifex on:${*:-" (no hosts given)"}"
    exit 0
fi

# base64 so the PEM survives two shells, and carried in the stdin stream rather
# than in the command: an argument is readable from ps on the node for the length
# of the call, on a host that runs other people's guests.
key_b64=$(printf '%s\n' "$key_pem" | base64 | tr -d '\n')

install_on() {
    # Unquoted heredoc on purpose: the key and the four credential fields are
    # expanded here, so nothing in it is expanded on the node.
    # shellcheck disable=SC2087
    ssh -i "$ssh_key" "${SSH_OPTS[@]}" "ubuntu@$1" bash -s <<REMOTE
set -eu
getent group spinifex >/dev/null \
    || { echo "no spinifex group yet, so this ran before the install" >&2; exit 3; }
sudo install -d -m 0750 -o root -g spinifex $REMOTE_DIR
# install /dev/null first, then write: the file never exists at a wider mode, and
# sudo's umask is not the caller's.
sudo install -m 0640 -o root -g spinifex /dev/null $REMOTE_PEM
base64 -d <<'PEM_B64' | sudo tee $REMOTE_PEM >/dev/null
$key_b64
PEM_B64
sudo install -m 0640 -o root -g spinifex /dev/null $REMOTE_CONFIG
sudo tee $REMOTE_CONFIG >/dev/null <<'CONFIG'
[$REMOTE_PROFILE]
user=$oci_user
fingerprint=$oci_fingerprint
tenancy=$oci_tenancy
region=$oci_region
key_file=$REMOTE_PEM
CONFIG
REMOTE
}

# Every host is attempted and the failures are named. Stopping at the first leaves
# a half-credentialled cluster and a log that does not say which half.
failed=""
for host in "$@"; do
    log "installing the OCI credential on $host"
    install_on "$host" || failed="$failed $host"
done

[ -z "$failed" ] || die "failed on:$failed"
log "credential installed on $# host(s)"
