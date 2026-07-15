#!/bin/sh
set -eu

# k3s-prestart — reproduces the start_pre logic from scripts/images/eks-node/
# k3s.initd so the same steps can later be wrapped by a systemd unit's
# ExecStartPre=: refuses to start on a restore-block marker, derives
# config.yaml from its skeleton, stages the konnectivity-agent + CoreDNS
# manifests, and waits (bounded) for the token-webhook kubeconfig. This
# start_pre performs no OpenRC state mutation (no command_args to set), so it
# needs no /run env file for the caller to source afterwards.
#
# Run as ExecStartPre=/start_pre; a nonzero exit here must stop the caller
# from ever invoking k3s, same as the OpenRC start_pre returning 1.

LOGTAG="k3s-prestart"

BLOCK_MARKER=${BLOCK_MARKER:-/run/spinifex-eks/k3s-restore-block}
K3S_CONFIG=${K3S_CONFIG:-/etc/rancher/k3s/config.yaml}
K3S_CONFIG_SKEL=${K3S_CONFIG_SKEL:-/etc/rancher/k3s/config.yaml.skel}
FIRST_BOOT_ENVFILE=${FIRST_BOOT_ENVFILE:-/etc/spinifex-eks/first-boot.env}
KONN_AGENT_MANIFEST_SRC=${KONN_AGENT_MANIFEST_SRC:-/usr/share/spinifex-eks/konnectivity/konnectivity-agent.yaml}
KONN_AGENT_MANIFEST_DST=${KONN_AGENT_MANIFEST_DST:-/var/lib/rancher/k3s/server/manifests/konnectivity-agent.yaml}
COREDNS_SRC=${COREDNS_SRC:-/usr/share/spinifex-eks/coredns/coredns-mulga.yaml}
COREDNS_SKIP=${COREDNS_SKIP:-/var/lib/rancher/k3s/server/manifests/coredns.yaml.skip}
COREDNS_DST=${COREDNS_DST:-/var/lib/rancher/k3s/server/manifests/coredns-mulga.yaml}
TOKEN_WEBHOOK_KUBECONFIG=${TOKEN_WEBHOOK_KUBECONFIG:-/etc/spinifex-eks/token-webhook.kubeconfig}
WAIT_SECS=${WAIT_SECS:-30}

# Konnectivity agent image (apiserver-network-proxy). Pinned alongside the
# konnectivity-server version baked by setup.sh; staged into the worker
# DaemonSet below.
KONN_AGENT_IMAGE="registry.k8s.io/kas-network-proxy/proxy-agent:v0.30.3"

# mulga-eks-k3s-recovery drops this marker when a required-snapshot restore
# could not fetch its etcd snapshot. Refuse to start: a fresh DR seed with no
# snapshot would cluster-init an EMPTY datastore (silent total data loss). The
# marker is in /run tmpfs, cleared on reboot, so recovery retries the fetch
# next boot and k3s starts once the snapshot restores.
if [ -f "${BLOCK_MARKER}" ]; then
    echo "[${LOGTAG}] restore-snapshot could not fetch the required etcd snapshot; refusing to start k3s on an empty datastore (see mulga-eks-k3s-recovery log)" >&2
    exit 1
fi

# config.yaml is written by k3s-first-boot from the skeleton + cloud-init
# data. Refuse to start if neither exists.
if [ ! -f "${K3S_CONFIG}" ]; then
    if [ -f "${K3S_CONFIG_SKEL}" ]; then
        cp "${K3S_CONFIG_SKEL}" "${K3S_CONFIG}"
    else
        echo "[${LOGTAG}] no ${K3S_CONFIG} and no ${K3S_CONFIG_SKEL} to derive from" >&2
        exit 1
    fi
fi

# Konnectivity agent: stage the worker-side DaemonSet into the auto-deploy dir
# (the apiserver `cluster` egress selector tunnels through konnectivity-server,
# which the agents dial out to). __KONN_HOST__ is the NLB endpoint agents reach,
# seeded by the launcher in first-boot.env; __KONN_AGENT_IMAGE__ is pinned here.
# The agent learns the apiserver replica count from the server handshake
# (--sync-forever), so no server-count token is substituted here. Guarded on
# the baked manifest so an older image without it simply skips the agent.
if [ -f "${KONN_AGENT_MANIFEST_SRC}" ]; then
    # The guard above is on the manifest, not the env file, so awk may be handed
    # a path that does not exist and exit nonzero. Swallow that under `set -e`:
    # an absent env file must fall through to the empty-host warning below and
    # still let k3s start, not abort the control plane before it boots.
    konn_host=$(awk -F= '/^EKS_KONNECTIVITY_HOST=/{print $2}' \
        "${FIRST_BOOT_ENVFILE}" 2>/dev/null || true)
    if [ -n "${konn_host}" ]; then
        mkdir -p "$(dirname "${KONN_AGENT_MANIFEST_DST}")"
        sed -e "s|__KONN_HOST__|${konn_host}|g" \
            -e "s|__KONN_AGENT_IMAGE__|${KONN_AGENT_IMAGE}|g" \
            "${KONN_AGENT_MANIFEST_SRC}" \
            > "${KONN_AGENT_MANIFEST_DST}"
        echo "[${LOGTAG}] konnectivity-agent staged (host ${konn_host})"
    else
        echo "[${LOGTAG}] EKS_KONNECTIVITY_HOST empty; konnectivity-agent not staged, apiserver->pod egress will fail" >&2
    fi
fi

# Node-local CoreDNS: suppress k3s's bundled CoreDNS (a single Deployment that
# tolerates the CP taint and strands itself on the control-plane node) and
# deploy the Mulga DaemonSet variant so every node resolves via its own
# CoreDNS. Worker pods cannot reach a CP-resident CoreDNS — the worker<->CP
# overlay does not cross the VPC boundary. Staged pre-start so the deploy
# controller never applies the bundled manifest. Guarded on the baked bundle:
# an older image without it keeps k3s's CoreDNS rather than losing DNS.
if [ -f "${COREDNS_SRC}" ]; then
    mkdir -p "$(dirname "${COREDNS_DST}")"
    : > "${COREDNS_SKIP}"
    cp "${COREDNS_SRC}" "${COREDNS_DST}"
fi

# The apiserver is configured with --authentication-token-webhook-config-file
# pointing at the kubeconfig eks-token-webhook writes at startup. `before k3s`
# orders the webhook's start but it backgrounds, so wait (bounded) for the
# kubeconfig to land before letting the apiserver load a missing file.
i=0
while [ ! -f "${TOKEN_WEBHOOK_KUBECONFIG}" ] && [ "${i}" -lt "${WAIT_SECS}" ]; do
    sleep 1
    i=$((i + 1))
done
if [ ! -f "${TOKEN_WEBHOOK_KUBECONFIG}" ]; then
    echo "[${LOGTAG}] eks-token-webhook kubeconfig absent after ${i}s; IAM token auth will be unavailable until the webhook starts" >&2
fi
