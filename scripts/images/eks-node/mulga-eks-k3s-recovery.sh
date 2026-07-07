#!/bin/sh
set -eu

# mulga-eks-k3s-recovery — applies a control-plane etcd-quorum recovery directive
# before k3s starts. Runs every boot, server role only; a no-op unless the
# spinifex reconciler escalated a wedged control plane (every member VM-running
# but embedded etcd never reformed quorum after a simultaneous restart).
#
# The reconciler cannot push into the guest, so recovery is pull-based: this
# fetches a per-member directive epoch<TAB>action<TAB>snapshot through the AWS
# gateway (eks-gateway-fetch -resource recovery), epoch-guarded so a directive
# applies exactly once. Ordered `before k3s` so the etcd datastore is mutated
# while the apiserver is stopped. Actions:
#   none          steady state — nothing to do
#   cluster-reset seed member: `k3s server --cluster-reset` reforms a
#                 single-member etcd from this node's intact data (optionally
#                 --cluster-reset-restore-path=<snap> from a predastore snapshot)
#   wipe-rejoin   follower: delete the local etcd datastore so k3s re-joins the
#                 reset seed's quorum fresh on the next k3s start
#
# Env knobs default to production paths; overridable so the logic is unit-testable
# without root, IMDS, a gateway, or a real k3s.

ROLE_FILE=${ROLE_FILE:-/etc/spinifex-eks/role}
ENVFILE=${ENVFILE:-/etc/spinifex-eks/first-boot.env}
SNAPSHOT_ENVFILE=${SNAPSHOT_ENVFILE:-/etc/spinifex-eks/etcd-snapshot.env}
EPOCH_FILE=${EPOCH_FILE:-/var/lib/spinifex-eks/recovery.epoch}
K3S_BIN=${K3S_BIN:-/usr/local/bin/k3s}
K3S_CONFIG=${K3S_CONFIG:-/etc/rancher/k3s/config.yaml}
ETCD_DIR=${ETCD_DIR:-/var/lib/rancher/k3s/server/db/etcd}
SNAPSHOT_DIR=${SNAPSHOT_DIR:-/var/lib/rancher/k3s/server/db/snapshots}
FETCH_BIN=${FETCH_BIN:-eks-gateway-fetch}
IMDS=${IMDS:-http://169.254.169.254/latest}
LOGTAG="mulga-eks-k3s-recovery"

log() { echo "[${LOGTAG}] $*"; }

# Server role only: workers have no etcd datastore. The role file is written by
# eks-node-role at first boot, so it is present on every boot recovery runs.
role=$(cat "${ROLE_FILE}" 2>/dev/null || echo "")
case "${role}" in
    server | server-join) ;;
    *)
        log "role '${role}' is not a server; nothing to recover"
        exit 0
        ;;
esac

if [ ! -f "${ENVFILE}" ]; then
    log "${ENVFILE} missing — no gateway creds, skipping recovery"
    exit 0
fi
set -a
# shellcheck disable=SC1090
. "${ENVFILE}"
set +a

# fetch_snapshot <name>: download a predastore etcd snapshot into SNAPSHOT_DIR via
# SigV4 curl (predastore speaks the S3 v4 wire), from the same system-bucket
# layout the snapshot cron uploads to. Non-zero on any failure so the caller
# falls back to a local-data cluster-reset.
fetch_snapshot() {
    _name=$1
    [ -f "${SNAPSHOT_ENVFILE}" ] || {
        log "snapshot env ${SNAPSHOT_ENVFILE} missing"
        return 1
    }
    # shellcheck disable=SC1090
    . "${SNAPSHOT_ENVFILE}"
    [ -n "${SPINIFEX_PREDASTORE_ENDPOINT:-}" ] || return 1
    [ -n "${SPINIFEX_PREDASTORE_AKID:-}" ] || return 1
    [ -n "${SPINIFEX_PREDASTORE_SECRET:-}" ] || return 1
    _url="${SPINIFEX_PREDASTORE_ENDPOINT%/}/eks-backups-system/${EKS_ACCOUNT_ID}/${EKS_CLUSTER_NAME}/${_name}"
    mkdir -p "${SNAPSHOT_DIR}"
    curl -fsSL \
        --aws-sigv4 "aws:amz:${AWS_REGION:-au-mel-1}:s3" \
        --user "${SPINIFEX_PREDASTORE_AKID}:${SPINIFEX_PREDASTORE_SECRET}" \
        -H "x-amz-content-sha256: UNSIGNED-PAYLOAD" \
        -o "${SNAPSHOT_DIR}/${_name}" \
        "${_url}" 2>&1 | logger -t "${LOGTAG}"
}

# Instance ID from IMDS keys the per-member directive. IMDSv2 token if enforced,
# tokenless (v1) fallback. Without it the directive cannot be addressed — skip
# rather than guess, so we never apply another member's directive.
imds_tok=$(curl -fsS --max-time 3 -X PUT "${IMDS}/api/token" \
    -H "X-aws-ec2-metadata-token-ttl-seconds: 300" 2>/dev/null || true)
imds_get() {
    if [ -n "${imds_tok}" ]; then
        curl -fsS --max-time 3 -H "X-aws-ec2-metadata-token: ${imds_tok}" "${IMDS}/meta-data/$1" 2>/dev/null
    else
        curl -fsS --max-time 3 "${IMDS}/meta-data/$1" 2>/dev/null
    fi
}
IID=${EKS_INSTANCE_ID:-$(imds_get instance-id || true)}
if [ -z "${IID}" ]; then
    log "IMDS instance-id unavailable; cannot address recovery directive, skipping"
    exit 0
fi

# Fetch the directive. eks-gateway-fetch retries the gateway with backoff; a
# sustained failure exits non-zero — do not block boot on it, leave k3s to start
# on the existing (possibly still-wedged) datastore.
line=$("${FETCH_BIN}" -resource recovery -instance-id "${IID}" 2>/dev/null | head -1 || true)
if [ -z "${line}" ]; then
    log "no recovery directive returned for ${IID}; nothing to do"
    exit 0
fi
epoch=$(printf '%s' "${line}" | cut -f1)
action=$(printf '%s' "${line}" | cut -f2)
snapshot=$(printf '%s' "${line}" | cut -f3)
[ -n "${action}" ] || action=none
case "${epoch}" in '' | *[!0-9]*) epoch=0 ;; esac

if [ "${action}" = "none" ]; then
    log "directive action=none (epoch ${epoch}); nothing to do"
    exit 0
fi

# Epoch guard: apply a directive exactly once. The reconciler bumps the epoch each
# time it issues a new directive; the guest records the last-applied epoch so a
# reboot after a successful reset does not re-run it.
applied=$(cat "${EPOCH_FILE}" 2>/dev/null || echo 0)
case "${applied}" in '' | *[!0-9]*) applied=0 ;; esac
if [ "${epoch}" -le "${applied}" ]; then
    log "directive epoch ${epoch} <= applied ${applied}; already handled"
    exit 0
fi

mkdir -p "$(dirname "${EPOCH_FILE}")"

case "${action}" in
    cluster-reset)
        restore_arg=""
        if [ -n "${snapshot}" ]; then
            if fetch_snapshot "${snapshot}"; then
                restore_arg="--cluster-reset-restore-path=${SNAPSHOT_DIR}/${snapshot}"
                log "cluster-reset restoring snapshot ${snapshot}"
            else
                log "WARN: snapshot ${snapshot} fetch failed; cluster-reset from local etcd only"
            fi
        fi
        log "running k3s server --cluster-reset ${restore_arg}"
        # shellcheck disable=SC2086
        if "${K3S_BIN}" server --config "${K3S_CONFIG}" --cluster-reset ${restore_arg} 2>&1 | logger -t "${LOGTAG}"; then
            log "cluster-reset complete"
        else
            log "ERROR: cluster-reset failed; leaving datastore, k3s will start on it"
        fi
        ;;
    wipe-rejoin)
        log "wiping local etcd datastore ${ETCD_DIR} to rejoin the reset quorum"
        rm -rf "${ETCD_DIR}"
        ;;
    *)
        log "unknown directive action '${action}'; ignoring"
        exit 0
        ;;
esac

# Record the applied epoch last: a crash before this re-runs the directive next
# boot, which is safe — cluster-reset and wipe-rejoin are idempotent on a stopped
# k3s (reset re-derives the same single-member etcd; wipe re-removes an empty dir).
printf '%s\n' "${epoch}" > "${EPOCH_FILE}"
log "recorded applied epoch ${epoch} for action ${action}"
