#!/bin/sh
set -eu

# mulga-eks-state-report — periodic NATS publish of K3s server health.
# Invoked every 15 minutes via /etc/periodic/15min (Alpine crond). One-shot
# best-effort; failures log but do not retry — the spinifex reconciler also
# does its own NLB healthcheck so this is supplementary signal.
#
# Subject: eks.state.{accountID}.{clusterName}.server
# Payload: JSON { healthz: "ok"|"fail", node_count: N, ts: <unix-seconds> }

ENVFILE=/etc/spinifex-eks/state-report.env
[ -f "${ENVFILE}" ] || { logger -t mulga-eks-state-report "${ENVFILE} missing — exiting"; exit 0; }
# shellcheck disable=SC1090
. "${ENVFILE}"

: "${SPINIFEX_NATS_URL:?}"
: "${EKS_ACCOUNT_ID:?}"
: "${EKS_CLUSTER_NAME:?}"

KUBECONFIG=${KUBECONFIG:-/etc/rancher/k3s/k3s.yaml}
export KUBECONFIG

if curl -sk --max-time 3 https://127.0.0.1:6443/healthz | grep -q '^ok$'; then
    HEALTH=ok
else
    HEALTH=fail
fi

if [ "${HEALTH}" = "ok" ]; then
    NODE_COUNT=$(kubectl get nodes --no-headers 2>/dev/null | wc -l | tr -d ' ')
else
    NODE_COUNT=0
fi

TS=$(date +%s)
PAYLOAD=$(printf '{"healthz":"%s","node_count":%s,"ts":%s}' "${HEALTH}" "${NODE_COUNT}" "${TS}")
SUBJ="eks.state.${EKS_ACCOUNT_ID}.${EKS_CLUSTER_NAME}.server"

NATS_ARGS="-s ${SPINIFEX_NATS_URL}"
if [ -n "${SPINIFEX_NATS_CREDS_FILE:-}" ] && [ -f "${SPINIFEX_NATS_CREDS_FILE}" ]; then
    NATS_ARGS="${NATS_ARGS} --creds ${SPINIFEX_NATS_CREDS_FILE}"
fi

# shellcheck disable=SC2086
printf '%s' "${PAYLOAD}" | nats ${NATS_ARGS} pub "${SUBJ}" --stdin 2>&1 \
    | logger -t mulga-eks-state-report
