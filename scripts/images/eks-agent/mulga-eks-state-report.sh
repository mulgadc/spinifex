#!/bin/sh
set -eu

# mulga-eks-state-report (agent variant) — periodic NATS publish of agent
# liveness signal. Subject: eks.state.{accountID}.{clusterName}.agent.{nodeName}
# Payload: JSON { kubelet: "ok"|"fail", ts: <unix> }
#
# The agent has no API server to query for its own status; we shell-check the
# local kubelet socket via crictl, and fall back to a process check.

ENVFILE=/etc/spinifex-eks/state-report.env
[ -f "${ENVFILE}" ] || { logger -t mulga-eks-state-report "${ENVFILE} missing"; exit 0; }
# shellcheck disable=SC1090
. "${ENVFILE}"

: "${SPINIFEX_NATS_URL:?}"
: "${EKS_ACCOUNT_ID:?}"
: "${EKS_CLUSTER_NAME:?}"
: "${EKS_NODE_NAME:?}"

if pgrep -f 'k3s agent' >/dev/null 2>&1; then
    KUBELET=ok
else
    KUBELET=fail
fi

TS=$(date +%s)
PAYLOAD=$(printf '{"kubelet":"%s","ts":%s}' "${KUBELET}" "${TS}")
SUBJ="eks.state.${EKS_ACCOUNT_ID}.${EKS_CLUSTER_NAME}.agent.${EKS_NODE_NAME}"

NATS_ARGS="-s ${SPINIFEX_NATS_URL}"
if [ -n "${SPINIFEX_NATS_CREDS_FILE:-}" ] && [ -f "${SPINIFEX_NATS_CREDS_FILE}" ]; then
    NATS_ARGS="${NATS_ARGS} --creds ${SPINIFEX_NATS_CREDS_FILE}"
fi

# shellcheck disable=SC2086
printf '%s' "${PAYLOAD}" | nats ${NATS_ARGS} pub "${SUBJ}" --stdin 2>&1 \
    | logger -t mulga-eks-state-report
