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

# --- Deferred built-in ingress enable -------------------------------------
# traefik is deferred at boot (its helm-install Job overloads the embedded etcd
# during bootstrap and can crash the control plane). k3s writes traefik.yaml but
# k3s.initd pre-stages a <name>.yaml.skip marker the deploy controller honours,
# so traefik is not applied. Once the apiserver has been healthy for
# TRAEFIK_ENABLE_AFTER consecutive reports — a crash resets the streak, so this
# only fires post-stabilisation — remove the skip markers so the deploy
# controller installs traefik with etcd headroom. Gated on EKS_DEFER_TRAEFIK=1
# (set only for clusters that want built-in ingress) and run once via a sentinel.
TRAEFIK_ENABLE_AFTER=${TRAEFIK_ENABLE_AFTER:-4}
TRAEFIK_ENABLED_SENTINEL=/var/lib/spinifex-eks/traefik-enabled
K3S_MANIFESTS=/var/lib/rancher/k3s/server/manifests
healthy_streak=0

maybe_enable_traefik() {
    [ "${EKS_DEFER_TRAEFIK:-0}" = "1" ] || return 0
    [ -f "${TRAEFIK_ENABLED_SENTINEL}" ] && return 0
    if [ "${1}" = "ok" ]; then
        healthy_streak=$((healthy_streak + 1))
    else
        healthy_streak=0
        return 0
    fi
    [ "${healthy_streak}" -ge "${TRAEFIK_ENABLE_AFTER}" ] || return 0
    rm -f "${K3S_MANIFESTS}/traefik.yaml.skip" "${K3S_MANIFESTS}/traefik-crd.yaml.skip"
    : > "${TRAEFIK_ENABLED_SENTINEL}"
    logger -t mulga-eks-state-report \
        "enabled deferred built-in ingress (traefik) after ${healthy_streak} healthy reports"
}

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
        maybe_enable_traefik "${health}"
        sleep "${STATE_REPORT_INTERVAL}"
    done
else
    publish_report
fi
