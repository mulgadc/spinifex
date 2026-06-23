#!/bin/sh
set -eu

# mulga-eks-state-report — publishes K3s server health to the spinifex cluster
# reconciler through the AWS gateway. The apiserver binds the VPC node-ip,
# unreachable from the host-side daemon, so the daemon cannot probe /healthz
# directly — this report carries {healthz, node_count}. eks-gateway-publish
# SigV4-signs the POST to the gateway, which relays it onto the management NATS
# bus the daemon shares; the VM never speaks NATS directly.
#
# Subject (gateway-side): eks.state.{accountID}.{clusterName}.server
# Payload: JSON { healthz: "ok"|"fail", node_count: N, ts: <unix-seconds> }
#
# Reads gateway creds + cluster identity from the cloud-init-seeded first-boot
# env (same vars k3s-first-boot.sh publishes its bootstrap artifacts with). Run
# as a loop by the mulga-eks-state-report OpenRC service when
# STATE_REPORT_INTERVAL is set; one-shot otherwise (e.g. a manual invocation).

ENVFILE=/etc/spinifex-eks/first-boot.env
[ -f "${ENVFILE}" ] || { logger -t mulga-eks-state-report "${ENVFILE} missing — exiting"; exit 0; }
# set -a so the sourced KEY=value lines are exported to the eks-gateway-publish
# child (it reads EKS_ACCOUNT_ID etc. from its environment); a bare source
# leaves them unexported and the helper exits "--account-id is required".
set -a
# shellcheck disable=SC1090
. "${ENVFILE}"
set +a

: "${EKS_GATEWAY_URL:?}"
: "${EKS_ACCESS_KEY:?}"
: "${EKS_SECRET_KEY:?}"
: "${EKS_ACCOUNT_ID:?}"
: "${EKS_CLUSTER_NAME:?}"

KUBECONFIG=${KUBECONFIG:-/etc/rancher/k3s/k3s.yaml}
export KUBECONFIG

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
    printf '%s' "${payload}" | eks-gateway-publish -channel state 2>&1 \
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
