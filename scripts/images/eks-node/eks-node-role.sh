#!/bin/sh
set -eu

# eks-node-role — first-boot role selector for the unified eks-node AMI.
#
# The same image boots either as a K3s control-plane server or a worker agent;
# the role is chosen per-instance at first boot from SPINIFEX_K3S_ROLE in the
# cloud-init env file. This script enables + starts the matching services
# under whichever init the image ships (none are enabled at bake time),
# records the resolved role to ROLE_FILE so later boots and the etcd-snapshot
# cron can branch without re-parsing, then disables itself so it runs exactly
# once.
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

# This script is delivered unmodified to both the legacy Alpine/OpenRC image
# and the mkosi Ubuntu/systemd image, so the enable/start/disable calls below
# must dispatch on whichever init actually owns the box. EKS_NODE_INIT lets
# the env knob win outright (same override idiom as the paths above, and the
# only way to exercise both branches from one test run); detection then
# prefers rc-update so an Alpine host with a stray systemctl shim still
# resolves to openrc.
INIT_SYSTEM="${EKS_NODE_INIT:-}"
if [ -z "${INIT_SYSTEM}" ]; then
    if command -v rc-update >/dev/null 2>&1; then
        INIT_SYSTEM=openrc
    elif command -v systemctl >/dev/null 2>&1; then
        INIT_SYSTEM=systemd
    else
        die "no supported init system found (need rc-update or systemctl)"
    fi
fi

# svc_enable/svc_start/svc_disable: thin wrappers so every call site below
# reads the same regardless of init. k3s-first-boot.sh duplicates this same
# dispatch rather than sourcing it from here — both are single-file
# deliverables installed straight to /usr/local/sbin with no shared
# library-path convention, and scripts/images/ is deleted at image-build
# cutover, so a shared file would outlive its usefulness.
svc_enable() {
    case "${INIT_SYSTEM}" in
        openrc) rc-update add "$1" default ;;
        systemd) systemctl enable "$1.service" ;;
    esac
}

# --no-block is load-bearing on systemd, not a tuning knob. This unit declares
# Before=k3s.service, and mulga-eks-k3s-recovery declares After=eks-node-role,
# so a blocking `systemctl start` deadlocks: systemd will not run the queued job
# until this unit finishes, and this unit cannot finish until the job it is
# waiting on runs. Type=oneshot defaults TimeoutStartSec=infinity, so nothing
# ever breaks the tie and the node wedges mid-boot with the role chosen but no
# control plane. Enqueue instead and let systemd sequence the units by the
# After=/Before= they already declare between themselves.
#
# OpenRC has no such constraint — rc-service runs the script directly rather
# than asking a supervisor to schedule it — so that branch still blocks and
# still surfaces a start failure via set -e.
svc_start() {
    case "${INIT_SYSTEM}" in
        openrc) rc-service "$1" start ;;
        systemd) systemctl start --no-block "$1.service" ;;
    esac
}

svc_disable() {
    case "${INIT_SYSTEM}" in
        openrc) rc-update del "$1" default 2>/dev/null || true ;;
        systemd) systemctl disable "$1.service" 2>/dev/null || true ;;
    esac
}

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
    server | server-join | agent) ;;
    *)
        die "SPINIFEX_K3S_ROLE missing or invalid: '${ROLE}'"
        ;;
esac

# Written before the rc-service calls below: mulga-eks-k3s-recovery, started
# inline for server/server-join, reads ROLE_FILE at the top of its own logic
# and must find it already in place, not empty.
mkdir -p "$(dirname "${ROLE_FILE}")"
printf '%s\n' "${ROLE}" > "${ROLE_FILE}"

case "${ROLE}" in
    server)
        log "configuring server role"
        svc_enable eks-token-webhook
        # Pre-k3s etcd recovery: enabled for later boots and started inline
        # now too, so a restore-snapshot DR seed's directive applies before
        # k3s starts on this first boot. A plain create has no directive, so it's a no-op.
        svc_enable mulga-eks-k3s-recovery
        svc_enable k3s
        # konnectivity-server fronts apiserver egress; every server replica runs
        # one so the agent's per-replica tunnel always lands a live server.
        svc_enable konnectivity-server
        svc_enable k3s-first-boot
        svc_enable mulga-eks-state-report
        # Managed-addon delivery runs only on the primary server: it renders
        # staged addon bundles into the K3s auto-deploy dir, which a single
        # writer owns. HA multi-server addon delivery is tracked separately
        # (mulga-siv-231.7); server-join nodes do not run it.
        svc_enable mulga-eks-addon-sync
        svc_start eks-token-webhook
        svc_start mulga-eks-k3s-recovery
        svc_start k3s
        svc_start konnectivity-server
        svc_start k3s-first-boot
        svc_start mulga-eks-state-report
        svc_start mulga-eks-addon-sync
        ;;
    server-join)
        log "configuring server-join role"
        svc_enable eks-token-webhook
        # Pre-k3s etcd recovery (see server role): server-join members are the
        # wipe-rejoin followers of a cluster-reset, so they need it too, and for
        # the same first-boot reason must be started here, not deferred.
        svc_enable mulga-eks-k3s-recovery
        svc_enable k3s
        svc_enable konnectivity-server
        svc_enable mulga-eks-state-report
        svc_start eks-token-webhook
        svc_start mulga-eks-k3s-recovery
        svc_start k3s
        svc_start konnectivity-server
        svc_start mulga-eks-state-report
        ;;
    agent)
        log "configuring agent role"
        svc_enable k3s-agent
        svc_start k3s-agent
        ;;
    *)
        die "SPINIFEX_K3S_ROLE missing or invalid: '${ROLE}'"
        ;;
esac

svc_disable eks-node-role
log "role '${ROLE}' configured"
