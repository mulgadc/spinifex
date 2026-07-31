#!/bin/sh
set -eu

# konnectivity-server-prestart — reproduces the start_pre logic (plus the
# top-level server-count derivation) from scripts/images/eks-node/
# konnectivity-server.initd so the same steps can later be wrapped by a
# systemd unit's ExecStartPre=: waits (bounded) for the k3s server CA + admin
# kubeconfig, mints the konnectivity serving cert, and writes the apiserver
# replica count for the caller's command line to source.
#
# Run as ExecStartPre=/start_pre; a nonzero exit here must stop the caller
# from ever invoking konnectivity-server, same as the OpenRC start_pre
# returning 1.

LOGTAG="konnectivity-server-prestart"

KONN_DIR=${KONN_DIR:-/var/lib/spinifex-eks/konnectivity}
KONN_RUNDIR=${KONN_RUNDIR:-/run/konnectivity}
ENVFILE=${ENVFILE:-/etc/spinifex-eks/first-boot.env}
K3S_CA=${K3S_CA:-/var/lib/rancher/k3s/server/tls/server-ca.crt}
K3S_CA_KEY=${K3S_CA_KEY:-/var/lib/rancher/k3s/server/tls/server-ca.key}
KUBECONFIG_FILE=${KUBECONFIG_FILE:-/etc/rancher/k3s/k3s.yaml}
DROPIN=${DROPIN:-/run/konnectivity-server-args.env}
WAIT_SECS=${WAIT_SECS:-120}
CERT_BIN=${CERT_BIN:-eks-konnectivity-cert}

# server-count is the apiserver replica count: each agent holds a tunnel to every
# replica, so egress from any apiserver always resolves (the HA-correct fan-out).
# Seeded by the launcher in first-boot.env; default 1 for a single control plane.
server_count=1
if [ -f "${ENVFILE}" ]; then
    _sc=$(awk -F= '/^EKS_KONNECTIVITY_SERVER_COUNT=/{print $2}' "${ENVFILE}")
    [ -n "${_sc}" ] && server_count="${_sc}"
fi

mkdir -p "${KONN_RUNDIR}" "${KONN_DIR}"

# Wait for k3s to write the server CA (+ key) and admin kubeconfig: the CA
# signs the serving cert agents validate, the kubeconfig is the TokenReview
# client for agent SA-token auth. Bounded; without them konnectivity is inert.
i=0
while [ "${i}" -lt "${WAIT_SECS}" ]; do
    if [ -r "${K3S_CA}" ] && [ -r "${K3S_CA_KEY}" ] && [ -r "${KUBECONFIG_FILE}" ]; then
        break
    fi
    i=$((i + 2))
    sleep 2
done
if [ ! -r "${K3S_CA}" ] || [ ! -r "${K3S_CA_KEY}" ] || [ ! -r "${KUBECONFIG_FILE}" ]; then
    echo "[${LOGTAG}] k3s server CA / admin kubeconfig not present after ${i}s" >&2
    exit 1
fi

# Mint (or reuse) the serving cert from the k3s server CA, SANed to the NLB
# endpoints agents dial. That CA is the in-pod ca.crt the agent already trusts,
# so no separate CA distribution is needed.
sans=""
if [ -f "${ENVFILE}" ]; then
    sans=$(awk -F= '/^EKS_KONNECTIVITY_SANS=/{print $2}' "${ENVFILE}")
fi
if [ -z "${sans}" ]; then
    echo "[${LOGTAG}] EKS_KONNECTIVITY_SANS empty; agents would fail server cert validation" >&2
    exit 1
fi
if ! "${CERT_BIN}" -dir "${KONN_DIR}" -ca-cert "${K3S_CA}" \
        -ca-key "${K3S_CA_KEY}" -cn konnectivity-server -sans "${sans}" >/dev/null; then
    echo "[${LOGTAG}] failed to mint konnectivity serving cert" >&2
    exit 1
fi

echo "KONN_SERVER_COUNT=${server_count}" > "${DROPIN}"
