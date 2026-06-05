#!/bin/sh
set -eu

# mulga-eks-state-report — NATS publish of K3s server health for the spinifex
# cluster reconciler. The apiserver binds the VPC node-ip, unreachable from the
# host-side daemon, so the daemon cannot probe /healthz directly — this report
# carries {healthz, node_count} over the management NATS bus the daemon shares.
#
# Subject: eks.state.{accountID}.{clusterName}.server
# Payload: JSON { healthz: "ok"|"fail", node_count: N, ts: <unix-seconds> }
#
# Reads NATS creds + cluster identity from the cloud-init-seeded first-boot env
# (same vars k3s-first-boot.sh publishes its bootstrap artifacts with). Run as a
# loop by the mulga-eks-state-report OpenRC service when STATE_REPORT_INTERVAL is
# set; one-shot otherwise (e.g. a manual invocation).

ENVFILE=/etc/spinifex-eks/first-boot.env
[ -f "${ENVFILE}" ] || { logger -t mulga-eks-state-report "${ENVFILE} missing — exiting"; exit 0; }
# shellcheck disable=SC1090
. "${ENVFILE}"

: "${SPINIFEX_NATS_URL:?}"
: "${EKS_ACCOUNT_ID:?}"
: "${EKS_CLUSTER_NAME:?}"

KUBECONFIG=${KUBECONFIG:-/etc/rancher/k3s/k3s.yaml}
export KUBECONFIG

SUBJ="eks.state.${EKS_ACCOUNT_ID}.${EKS_CLUSTER_NAME}.server"

# NATS args mirror k3s-first-boot.sh (token + TLS CA) — the proven publish path.
NATS_ARGS="-s ${SPINIFEX_NATS_URL}"
if [ -n "${SPINIFEX_NATS_TOKEN:-}" ]; then
    NATS_ARGS="${NATS_ARGS} --token ${SPINIFEX_NATS_TOKEN}"
fi
if [ -n "${SPINIFEX_NATS_CA:-}" ] && [ -f "${SPINIFEX_NATS_CA}" ]; then
    NATS_ARGS="${NATS_ARGS} --tlsca ${SPINIFEX_NATS_CA}"
fi

publish_report() {
    if curl -sk --max-time 3 https://127.0.0.1:6443/healthz | grep -q '^ok$'; then
        health=ok
        # Count Ready nodes only — k3s retains terminated workers as NotReady
        # until manually pruned, so a raw node count never drops on scale-down.
        node_count=$(kubectl get nodes --no-headers 2>/dev/null | awk '$2=="Ready"' | wc -l | tr -d ' ')
    else
        health=fail
        node_count=0
    fi
    payload=$(printf '{"healthz":"%s","node_count":%s,"ts":%s}' "${health}" "${node_count}" "$(date +%s)")
    # shellcheck disable=SC2086
    printf '%s' "${payload}" | nats ${NATS_ARGS} pub "${SUBJ}" --force-stdin 2>&1 \
        | logger -t mulga-eks-state-report
}

# STATE_REPORT_INTERVAL set → run forever (service mode); unset → one-shot.
if [ -n "${STATE_REPORT_INTERVAL:-}" ] && [ "${STATE_REPORT_INTERVAL}" -gt 0 ] 2>/dev/null; then
    while true; do
        publish_report
        sleep "${STATE_REPORT_INTERVAL}"
    done
else
    publish_report
fi
