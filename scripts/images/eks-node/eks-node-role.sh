#!/bin/sh
set -eu

# eks-node-role — first-boot role selector for the unified eks-node AMI.
#
# The same image boots either as a K3s control-plane server or a worker agent;
# the role is chosen per-instance at first boot from SPINIFEX_K3S_ROLE in the
# cloud-init env file. This script enables + starts the matching OpenRC services
# (none are in the default runlevel at bake time), records the resolved role to
# ROLE_FILE so later boots and the etcd-snapshot cron can branch without
# re-parsing, then removes itself from the runlevel so it runs exactly once.
#
#   server      → eks-token-webhook, k3s (server), k3s-first-boot (bootstrap
#                 publish), mulga-eks-state-report
#   server-join → eks-token-webhook, k3s (server, joins the first server's etcd
#                 quorum), mulga-eks-state-report. NO k3s-first-boot: the first
#                 server already publishes the cluster-identical bootstrap
#                 artifacts; a join re-publish only races the bootstrap bus.
#   agent       → k3s-agent
#
# Paths are overridable via env so the selection logic is unit-testable (bats)
# without root or a real /etc.

ROLE_FILE="${EKS_NODE_ROLE_FILE:-/etc/spinifex-eks/role}"
ENVFILE="${EKS_NODE_ENVFILE:-/etc/spinifex-eks/first-boot.env}"
AGENT_ENVFILE="${EKS_NODE_AGENT_ENVFILE:-/etc/spinifex-eks/agent.env}"
LOGTAG="eks-node-role"

log() { echo "[${LOGTAG}] $*"; }
die() { log "ERROR: $*"; exit 1; }

# Already resolved on a prior boot — the role services are in the runlevel and
# this service should have been pulled, but guard anyway so a stray re-run is a
# no-op rather than a double-enable.
if [ -f "${ROLE_FILE}" ]; then
    log "role already resolved ($(cat "${ROLE_FILE}")); nothing to do"
    exit 0
fi

# Resolve the role. Server seeds SPINIFEX_K3S_ROLE in first-boot.env; a worker
# seed that predates the explicit var is inferred from the presence of agent.env
# (K3S_URL/K3S_TOKEN live there).
ROLE=""
if [ -f "${ENVFILE}" ]; then
    ROLE=$(. "${ENVFILE}"; printf '%s' "${SPINIFEX_K3S_ROLE:-}")
fi
if [ -z "${ROLE}" ] && [ -f "${AGENT_ENVFILE}" ]; then
    ROLE="agent"
fi

case "${ROLE}" in
    server)
        log "configuring server role"
        rc-update add eks-token-webhook default
        rc-update add k3s default
        rc-update add k3s-first-boot default
        rc-update add mulga-eks-state-report default
        rc-service eks-token-webhook start
        rc-service k3s start
        rc-service k3s-first-boot start
        rc-service mulga-eks-state-report start
        ;;
    server-join)
        log "configuring server-join role"
        rc-update add eks-token-webhook default
        rc-update add k3s default
        rc-update add mulga-eks-state-report default
        rc-service eks-token-webhook start
        rc-service k3s start
        rc-service mulga-eks-state-report start
        ;;
    agent)
        log "configuring agent role"
        rc-update add k3s-agent default
        rc-service k3s-agent start
        ;;
    *)
        die "SPINIFEX_K3S_ROLE missing or invalid: '${ROLE}'"
        ;;
esac

mkdir -p "$(dirname "${ROLE_FILE}")"
printf '%s\n' "${ROLE}" > "${ROLE_FILE}"
rc-update del eks-node-role default 2>/dev/null || true
log "role '${ROLE}' configured"
