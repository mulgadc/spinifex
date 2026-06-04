#!/bin/sh
set -eu

# mulga-eks-etcd-snapshot — nightly K3s embedded etcd snapshot to predastore.
# Run from /etc/periodic/daily (Alpine crond).
# K3s has a built-in `k3s etcd-snapshot` command that handles the snapshot
# semantics; this wrapper exposes it on cron and uploads to the system-bucket
# (see eks-v1.md Q6 + Q17).
#
# Bucket layout (Q17):
#   eks-backups-system/{accountID}/{clusterName}/etcd-{timestamp}.snap
# Retention: managed via S3 lifecycle policy on the bucket; this script does
# not prune.

# Worker nodes share the unified eks-node AMI but have no etcd/SQLite datastore
# to snapshot. The role file is written once by eks-node-role at first boot.
if [ "$(cat /etc/spinifex-eks/role 2>/dev/null)" = "agent" ]; then
    exit 0
fi

ENVFILE=/etc/spinifex-eks/etcd-snapshot.env
[ -f "${ENVFILE}" ] || { logger -t mulga-eks-etcd-snapshot "${ENVFILE} missing"; exit 0; }
# shellcheck disable=SC1090
. "${ENVFILE}"

: "${EKS_ACCOUNT_ID:?}"
: "${EKS_CLUSTER_NAME:?}"
: "${SPINIFEX_PREDASTORE_ENDPOINT:?}"
: "${SPINIFEX_PREDASTORE_AKID:?}"
: "${SPINIFEX_PREDASTORE_SECRET:?}"

SNAPSHOT_DIR=/var/lib/rancher/k3s/server/db/snapshots
TS=$(date -u +%Y%m%dT%H%M%SZ)
NAME="etcd-${TS}"
BUCKET="eks-backups-system"
KEY="${EKS_ACCOUNT_ID}/${EKS_CLUSTER_NAME}/${NAME}.snap"

/usr/local/bin/k3s etcd-snapshot save --name "${NAME}" 2>&1 | logger -t mulga-eks-etcd-snapshot

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
