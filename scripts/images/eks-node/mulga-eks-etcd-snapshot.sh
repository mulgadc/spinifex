#!/bin/sh
set -eu

# mulga-eks-etcd-snapshot — K3s embedded etcd snapshot to predastore, on two
# cron cadences. The same script is installed into two Alpine crond periodic
# dirs and picks its cadence tier from the dir it was invoked from:
#   /etc/periodic/15min/mulga-eks-etcd-snapshot -> tier "frequent" (RPO window)
#   /etc/periodic/daily/mulga-eks-etcd-snapshot -> tier "daily"    (nightly)
# Nightly alone is too coarse a recovery point for a young cluster; the frequent
# tier bounds data loss to the cron interval. K3s has a built-in
# `k3s etcd-snapshot` command that handles the snapshot semantics; this wrapper
# exposes it on cron and uploads to the system-bucket (see eks-v1.md Q6 + Q17).
#
# Bucket layout (Q17): the tier is a key-prefix segment so a predastore S3
# lifecycle rule can expire the frequent rolling window shallow while keeping
# the nightly copies deep:
#   eks-backups-system/{accountID}/{clusterName}/etcd-{tier}-{timestamp}.snap
# This script does not prune; retention is the bucket lifecycle policy's job.

# Cadence tier from the invoking crond dir; TIER overrides it (unit test).
if [ -z "${TIER:-}" ]; then
    case "$0" in
        */15min/*) TIER=frequent ;;
        *)         TIER=daily ;;
    esac
fi

# Worker nodes share the unified eks-node AMI but have no etcd/SQLite datastore
# to snapshot. The role file is written once by eks-node-role at first boot.
ROLE_FILE=${ROLE_FILE:-/etc/spinifex-eks/role}
if [ "$(cat "${ROLE_FILE}" 2>/dev/null)" = "agent" ]; then
    exit 0
fi

ENVFILE=${ENVFILE:-/etc/spinifex-eks/etcd-snapshot.env}
[ -f "${ENVFILE}" ] || { logger -t mulga-eks-etcd-snapshot "${ENVFILE} missing"; exit 0; }
# shellcheck disable=SC1090
. "${ENVFILE}"

: "${EKS_ACCOUNT_ID:?}"
: "${EKS_CLUSTER_NAME:?}"
: "${SPINIFEX_PREDASTORE_ENDPOINT:?}"
: "${SPINIFEX_PREDASTORE_AKID:?}"
: "${SPINIFEX_PREDASTORE_SECRET:?}"

K3S_BIN=${K3S_BIN:-/usr/local/bin/k3s}
SNAPSHOT_DIR=${SNAPSHOT_DIR:-/var/lib/rancher/k3s/server/db/snapshots}
TS=$(date -u +%Y%m%dT%H%M%SZ)
# Tier in the name keeps the two cadences from colliding on an identical second
# (both dirs can fire at 02:00) and prefix-filters cleanly for S3 lifecycle.
NAME="etcd-${TIER}-${TS}"
BUCKET="eks-backups-system"
KEY="${EKS_ACCOUNT_ID}/${EKS_CLUSTER_NAME}/${NAME}.snap"

"${K3S_BIN}" etcd-snapshot save --name "${NAME}" 2>&1 | logger -t mulga-eks-etcd-snapshot

SNAPSHOT_FILE=$(ls -t "${SNAPSHOT_DIR}/${NAME}"* 2>/dev/null | head -1 || true)
if [ -z "${SNAPSHOT_FILE}" ]; then
    logger -t mulga-eks-etcd-snapshot "snapshot file for ${NAME} not found in ${SNAPSHOT_DIR}"
    exit 1
fi

# Upload via curl SigV4. predastore implements the S3 v4 wire so the standard
# AWS PUT request signs cleanly. aws-cli is not in the image (kept small) — we
# hand-roll the signed request with curl --aws-sigv4.
#
# Note: --aws-sigv4 requires curl >= 7.75 (Alpine 3.21 ships 8.x).
URL="${SPINIFEX_PREDASTORE_ENDPOINT%/}/${BUCKET}/${KEY}"
curl -fsSL \
    --aws-sigv4 "aws:amz:${AWS_REGION:-au-mel-1}:s3" \
    --user "${SPINIFEX_PREDASTORE_AKID}:${SPINIFEX_PREDASTORE_SECRET}" \
    -H "x-amz-content-sha256: UNSIGNED-PAYLOAD" \
    --upload-file "${SNAPSHOT_FILE}" \
    "${URL}" 2>&1 | logger -t mulga-eks-etcd-snapshot

# Remove the local snapshot to bound disk usage; the canonical copy lives in
# predastore. Restore path (`spx admin eks restore-snapshot`) pulls from there.
rm -f "${SNAPSHOT_FILE}"
logger -t mulga-eks-etcd-snapshot "uploaded ${KEY}"
