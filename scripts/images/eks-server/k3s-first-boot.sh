#!/bin/sh
set -eu

# k3s-first-boot — runs once after the K3s server reaches a healthy state.
# Reads the bootstrap node-token and admin kubeconfig that K3s writes during
# `--cluster-init`, rewrites the kubeconfig server address to the cluster's
# NLB endpoint (so workers and external kubectl can use it), and publishes
# both as one-shot NATS messages for the spinifex cluster reconciler to
# consume into KV.
#
# Required env (from cloud-init user-data /etc/spinifex-eks/first-boot.env):
#   SPINIFEX_NATS_URL          nats://...
#   SPINIFEX_NATS_CREDS_FILE   /etc/spinifex-eks/nats.creds
#   EKS_ACCOUNT_ID
#   EKS_CLUSTER_NAME
#   EKS_NLB_ENDPOINT           https://{cluster}.{accountID}.eks.{region}.{suffix}
#
# Idempotent: a sentinel file at /var/lib/spinifex-eks/first-boot.pending gates
# execution. On success the sentinel is removed and the OpenRC service is
# pulled from the default runlevel so it does not retry on subsequent boots.

SENTINEL=/var/lib/spinifex-eks/first-boot.pending
ENVFILE=/etc/spinifex-eks/first-boot.env
LOGTAG="k3s-first-boot"

log() { echo "[${LOGTAG}] $*"; }
die() { log "ERROR: $*"; exit 1; }

if [ ! -f "${SENTINEL}" ]; then
    log "sentinel missing — first boot already complete"
    exit 0
fi

if [ ! -f "${ENVFILE}" ]; then
    die "${ENVFILE} not found — cloud-init did not seed first-boot env"
fi

# shellcheck disable=SC1090
. "${ENVFILE}"

for v in SPINIFEX_NATS_URL EKS_ACCOUNT_ID EKS_CLUSTER_NAME EKS_NLB_ENDPOINT; do
    eval "val=\${$v:-}"
    [ -n "${val}" ] || die "env ${v} not set"
done

# 1. Wait for K3s /healthz. K3s self-signs initially; --insecure-skip is fine
#    here, we are on loopback.
log "waiting for K3s /healthz on 127.0.0.1:6443"
i=0
while [ "${i}" -lt 300 ]; do
    if curl -sk --max-time 2 https://127.0.0.1:6443/healthz | grep -q '^ok$'; then
        log "K3s healthz ok after ${i}s"
        break
    fi
    i=$((i + 2))
    sleep 2
done
[ "${i}" -lt 300 ] || die "K3s did not become healthy within 5 minutes"

# 2. Read node-token + admin kubeconfig.
TOKEN_FILE=/var/lib/rancher/k3s/server/node-token
KUBECONFIG_FILE=/etc/rancher/k3s/k3s.yaml
[ -r "${TOKEN_FILE}" ] || die "${TOKEN_FILE} unreadable"
[ -r "${KUBECONFIG_FILE}" ] || die "${KUBECONFIG_FILE} unreadable"

NODE_TOKEN=$(cat "${TOKEN_FILE}")
# K3s ships kubeconfig with server: https://127.0.0.1:6443 — rewrite to the
# NLB endpoint so it works from outside the control plane VM.
KUBECONFIG_REWRITTEN=$(sed "s|server: https://127\.0\.0\.1:6443|server: ${EKS_NLB_ENDPOINT}|" "${KUBECONFIG_FILE}")

# 3. Publish one-shot NATS messages. Subjects per eks-v1.md Q14 + Q15.
TOKEN_SUBJ="eks.bus.${EKS_ACCOUNT_ID}.${EKS_CLUSTER_NAME}.k3s-bootstrap-token"
KUBE_SUBJ="eks.bus.${EKS_ACCOUNT_ID}.${EKS_CLUSTER_NAME}.k3s-admin-kubeconfig"

NATS_ARGS="-s ${SPINIFEX_NATS_URL}"
if [ -n "${SPINIFEX_NATS_CREDS_FILE:-}" ] && [ -f "${SPINIFEX_NATS_CREDS_FILE}" ]; then
    NATS_ARGS="${NATS_ARGS} --creds ${SPINIFEX_NATS_CREDS_FILE}"
fi

log "publishing node-token to ${TOKEN_SUBJ}"
# shellcheck disable=SC2086
printf '%s' "${NODE_TOKEN}" | nats ${NATS_ARGS} pub "${TOKEN_SUBJ}" --stdin

log "publishing admin kubeconfig to ${KUBE_SUBJ}"
# shellcheck disable=SC2086
printf '%s' "${KUBECONFIG_REWRITTEN}" | nats ${NATS_ARGS} pub "${KUBE_SUBJ}" --stdin

# 4. Self-disable. Remove sentinel, pull from runlevel.
rm -f "${SENTINEL}"
rc-update del k3s-first-boot default 2>/dev/null || true
log "first boot complete"
