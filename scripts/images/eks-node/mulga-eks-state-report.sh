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

ENVFILE=${ENVFILE:-/etc/spinifex-eks/first-boot.env}
[ -f "${ENVFILE}" ] || { logger -t mulga-eks-state-report "${ENVFILE} missing — exiting"; exit 0; }
# set -a so the sourced KEY=value lines are exported to the eks-gateway-publish
# child (it reads EKS_ACCOUNT_ID etc. from its environment); a bare source
# leaves them unexported and the helper exits "--account-id is required".
set -a
# shellcheck disable=SC1090
. "${ENVFILE}"
set +a

# EKS_ACCESS_KEY/EKS_SECRET_KEY are optional: when absent, eks-gateway-publish
# signs with IMDS instance-role credentials via the AWS SDK default chain.
: "${EKS_GATEWAY_URL:?}"
: "${EKS_ACCOUNT_ID:?}"
: "${EKS_CLUSTER_NAME:?}"

KUBECONFIG=${KUBECONFIG:-/etc/rancher/k3s/k3s.yaml}
export KUBECONFIG

# Diagnostic knobs (overridable for the unit test). ETCD_DB_DIR is the embedded
# etcd data dir whose free space we check; DISK_MIN_KB is the low-space threshold.
ETCD_DB_DIR=${ETCD_DB_DIR:-/var/lib/rancher/k3s/server/db}
DISK_MIN_KB=${DISK_MIN_KB:-262144}

# diagnose emits a compact, JSON-safe reason for an unhealthy apiserver by
# reading the ground truth only reachable from inside the guest: the failing
# /readyz subchecks, isolated etcd reachability, and the etcd-disk free space.
diagnose() {
    # apiserver prints "[+]name ok" for passing subchecks and "[-]name failed"
    # for failing ones; collect the failing names (etcd, poststarthook/*, ...).
    failed=$(kubectl get --raw='/readyz?verbose' 2>/dev/null \
        | awk '/^\[-\]/{sub(/^\[-\]/,""); sub(/[[:space:]].*$/,""); printf "%s%s", sep, $0; sep=" "}')
    [ -n "${failed}" ] || failed=none

    if kubectl get --raw='/healthz/etcd' 2>/dev/null | grep -q '^ok$'; then
        etcd=ok
    else
        etcd=unreachable
    fi

    avail_kb=$(df -Pk "${ETCD_DB_DIR}" 2>/dev/null | awk 'NR==2{print $4}')
    if [ -n "${avail_kb}" ] && [ "${avail_kb}" -lt "${DISK_MIN_KB}" ] 2>/dev/null; then
        disk="low:${avail_kb}k"
    else
        disk=ok
    fi

    printf 'readyz:[%s]; etcd:%s; disk:%s' "${failed}" "${etcd}" "${disk}"
}

publish_report() {
    # Probe /healthz through the node's admin kubeconfig (apiserver runs
    # anonymous-auth=false, CIS): an unauthenticated curl would return 401.
    reason=
    nodegroup_ready='{}'
    if kubectl get --raw='/healthz' 2>/dev/null | grep -q '^ok$'; then
        health=ok
        # Count Ready nodes only — k3s retains terminated workers as NotReady
        # until manually pruned, so a raw node count never drops on scale-down.
        node_count=$(kubectl get nodes --no-headers 2>/dev/null | awk '$2=="Ready"' | wc -l | tr -d ' ')
        # Per-nodegroup Ready breakdown: -L appends the node's
        # eks.amazonaws.com/nodegroup label as the last column ("<none>" when
        # absent, e.g. the control-plane node itself). Grouping by that label —
        # rather than only totalling — lets each nodegroup's own Ready-gate be
        # scoped to its own workers instead of the cluster-wide count, so one
        # nodegroup's Ready nodes can never mask another's stuck launch.
        nodegroup_ready=$(kubectl get nodes --no-headers -L eks.amazonaws.com/nodegroup 2>/dev/null \
            | awk '$2=="Ready" && $NF!="<none>"{c[$NF]++} END{sep=""; printf "{"; for (k in c) {printf "%s\"%s\":%d", sep, k, c[k]; sep=","}; printf "}"}')
        [ -n "${nodegroup_ready}" ] || nodegroup_ready='{}'
    else
        health=fail
        node_count=0
        # Strip quotes/backslashes/newlines so the reason is safe to embed in the
        # JSON payload without a real encoder.
        reason=$(diagnose | tr -d '"\\' | tr '\n' ' ')
        # Verbose block to the host-captured serial console — the only channel an
        # operator can read when the wedged guest has no shell access.
        {
            echo "=== mulga-eks CP unhealthy $(date -u +%Y-%m-%dT%H:%M:%SZ) cluster=${EKS_CLUSTER_NAME} ==="
            echo "reason: ${reason}"
            kubectl get --raw='/readyz?verbose' 2>&1 | grep -E '^\[-\]' || true
            echo "=== end mulga-eks CP diag ==="
        } > "${CONSOLE:-/dev/console}" 2>&1 || true
    fi
    if [ -n "${reason}" ]; then
        payload=$(printf '{"healthz":"%s","node_count":%s,"nodegroup_ready":%s,"reason":"%s","ts":%s}' \
            "${health}" "${node_count}" "${nodegroup_ready}" "${reason}" "$(date +%s)")
    else
        payload=$(printf '{"healthz":"%s","node_count":%s,"nodegroup_ready":%s,"ts":%s}' \
            "${health}" "${node_count}" "${nodegroup_ready}" "$(date +%s)")
    fi
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
