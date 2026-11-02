#!/bin/sh
set -eu

# mulga-eks-node-gc — deletes the Node of a nodegroup worker whose instance the
# nodegroup has terminated (DeleteNodegroup, scale-down, reclaim), as the AWS
# cloud controller deletes a Node whose instance no longer exists. K3s runs no
# AWS cloud provider: its own reports every instance as existing, so without
# this a terminated worker stays a NotReady Node forever, its pods stay bound to
# it, and once most Nodes are NotReady the node lifecycle controller stops
# evicting at all.
#
# Pull (host→VM): GET /clusters/{cluster}/internal-workers/{accountId}, the
# instance ID of every worker the cluster's nodegroups still own.
#
# A Node is deleted only when all of these hold, so a live worker is never
# touched: it carries the eks.amazonaws.com/nodegroup label, its providerID is
# aws:///<az>/<instance-id>, it is not Ready, and its instance is not in the
# fetched set. A failed fetch deletes nothing.
#
# Runs on every server; deletes are idempotent. Loops every NODE_GC_INTERVAL
# seconds in service mode, one-shot otherwise.

ENVFILE=${ENVFILE:-/etc/spinifex-eks/first-boot.env}
[ -f "${ENVFILE}" ] || { logger -t mulga-eks-node-gc "${ENVFILE} missing — exiting"; exit 0; }
# set -a so the sourced creds are exported to the eks-gateway-fetch child.
set -a
# shellcheck disable=SC1090
. "${ENVFILE}"
set +a

: "${EKS_GATEWAY_URL:?}"
: "${EKS_ACCOUNT_ID:?}"
: "${EKS_CLUSTER_NAME:?}"

KUBECONFIG=${KUBECONFIG:-/etc/rancher/k3s/k3s.yaml}
export KUBECONFIG

# One "name|providerID|nodegroup|Ready" row per Node.
NODE_ROWS='jsonpath={range .items[*]}{.metadata.name}{"|"}{.spec.providerID}{"|"}{.metadata.labels.eks\.amazonaws\.com/nodegroup}{"|"}{.status.conditions[?(@.type=="Ready")].status}{"\n"}{end}'

log() {
    logger -t mulga-eks-node-gc "$*"
    echo "[mulga-eks-node-gc] $*" > "${CONSOLE:-/dev/console}" 2>/dev/null || true
}

# stale_nodes LIVE_FILE — reads Node rows on stdin and prints the name of each
# Node to delete: a nodegroup worker, not Ready, whose instance is not listed
# in LIVE_FILE.
stale_nodes() {
    awk -F'|' -v live="$1" '
        BEGIN { while ((getline id < live) > 0) keep[id] = 1 }
        $3 == "" || $4 == "True" { next }
        $2 !~ /^aws:\/\/\/[^\/]+\/i-[0-9a-f]+$/ { next }
        { n = split($2, p, "/"); if (!(p[n] in keep)) print $1 }
    '
}

sync_once() {
    # Nodes are listed before the workers are fetched. A worker is recorded on
    # its nodegroup before it boots, so every Node listed here is already in a
    # set fetched after.
    if ! rows=$(kubectl get nodes -o "${NODE_ROWS}" 2>/dev/null); then
        log "kubectl get nodes failed; skipping this pass"
        return 0
    fi
    live=$(mktemp)
    if ! eks-gateway-fetch -resource workers > "${live}" 2>/dev/null; then
        log "fetch of the cluster's nodegroup workers failed; deleting nothing"
        rm -f "${live}"
        return 0
    fi
    printf '%s\n' "${rows}" | stale_nodes "${live}" | while read -r node; do
        if kubectl delete node "${node}" --ignore-not-found --wait=false >/dev/null 2>&1; then
            log "deleted Node ${node}: its instance is no longer a nodegroup worker"
        else
            log "delete of Node ${node} failed; retrying next pass"
        fi
    done
    rm -f "${live}"
}

if [ -n "${NODE_GC_INTERVAL:-}" ] && [ "${NODE_GC_INTERVAL}" -gt 0 ] 2>/dev/null; then
    while true; do
        sync_once
        sleep "${NODE_GC_INTERVAL}"
    done
else
    sync_once
fi
