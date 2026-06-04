#!/bin/sh
set -eu

# k3s-first-boot — runs once after the K3s server reaches a healthy state.
# Reads the bootstrap node-token and admin kubeconfig that K3s writes at
# server startup, rewrites the kubeconfig server address to the cluster's
# NLB endpoint (so workers and external kubectl can use it), and publishes
# both as one-shot NATS messages for the spinifex cluster reconciler to
# consume into KV.
#
# Required env (from cloud-init user-data /etc/spinifex-eks/first-boot.env):
#   SPINIFEX_NATS_URL          nats://...
#   SPINIFEX_NATS_TOKEN        shared NATS auth token
#   SPINIFEX_NATS_CA           /etc/spinifex-eks/nats-ca.pem (TLS CA bundle)
#   EKS_ACCOUNT_ID
#   EKS_CLUSTER_NAME
#   EKS_NLB_ENDPOINT           https://{cluster}.{accountID}.eks.{region}.{suffix}
#
# Idempotent: a sentinel file at /var/lib/spinifex-eks/first-boot.pending gates
# execution. On success the sentinel is removed and the OpenRC service is
# pulled from the default runlevel so it does not retry on subsequent boots.

SENTINEL=/var/lib/spinifex-eks/first-boot.pending
ENVFILE=/etc/spinifex-eks/first-boot.env
LOGTAG="k3s-first-boot"

log() { echo "[${LOGTAG}] $*"; }
die() { log "ERROR: $*"; exit 1; }

if [ ! -f "${SENTINEL}" ]; then
    log "sentinel missing — first boot already complete"
    exit 0
fi

if [ ! -f "${ENVFILE}" ]; then
    die "${ENVFILE} not found — cloud-init did not seed first-boot env"
fi

# shellcheck disable=SC1090
. "${ENVFILE}"

for v in SPINIFEX_NATS_URL EKS_ACCOUNT_ID EKS_CLUSTER_NAME EKS_NLB_ENDPOINT; do
    eval "val=\${$v:-}"
    [ -n "${val}" ] || die "env ${v} not set"
done

# Paths K3s writes during server startup: the bootstrap node-token and the
# admin kubeconfig (used both to gate readiness and to publish downstream).
TOKEN_FILE=/var/lib/rancher/k3s/server/node-token
KUBECONFIG_FILE=/etc/rancher/k3s/k3s.yaml

# 1. Wait for the apiserver to be ready to serve, gating on /readyz. K3s runs
#    the apiserver with anonymous-auth=false, so an anonymous probe returns 401,
#    never "ok" — the probe must be authenticated. Use the node's admin
#    kubeconfig (kubectl get --raw), which k3s writes early in startup. kubectl
#    exits 0 only when /readyz returns 200; on a failing sub-check it exits
#    non-zero, so the body "ok" check plus exit status gates correctly. Every
#    30s the failing /readyz checks are logged so a stuck boot is diagnosable
#    from the captured serial console.
log "waiting for K3s apiserver readiness (/readyz) on 127.0.0.1:6443"
i=0
ready=0
while [ "${i}" -lt 300 ]; do
    if [ -r "${KUBECONFIG_FILE}" ] && \
       [ "$(kubectl --kubeconfig "${KUBECONFIG_FILE}" get --raw='/readyz' 2>/dev/null)" = "ok" ]; then
        log "K3s apiserver ready after ${i}s"
        ready=1
        break
    fi
    if [ $((i % 30)) -eq 0 ]; then
        log "apiserver not ready after ${i}s:"
        if [ -r "${KUBECONFIG_FILE}" ]; then
            kubectl --kubeconfig "${KUBECONFIG_FILE}" get --raw='/readyz?verbose' 2>&1 \
                | grep -E '^\[-\]|failed$' | head -20 | while IFS= read -r l; do log "  ${l}"; done
        else
            log "  (admin kubeconfig ${KUBECONFIG_FILE} not written yet)"
        fi
    fi
    i=$((i + 5))
    sleep 5
done
if [ "${ready}" -ne 1 ]; then
    log "K3s apiserver not ready within 5 minutes; last /readyz body:"
    kubectl --kubeconfig "${KUBECONFIG_FILE}" get --raw='/readyz?verbose' 2>&1 \
        | head -40 | while IFS= read -r l; do log "  ${l}"; done
    log "last 40 lines of /var/log/k3s.log follow:"
    tail -n 40 /var/log/k3s.log 2>/dev/null || log "(no /var/log/k3s.log)"
    die "K3s apiserver not ready within 5 minutes"
fi

# 2. Read the four bootstrap artifacts K3s wrote at server startup.
[ -r "${TOKEN_FILE}" ] || die "${TOKEN_FILE} unreadable"
[ -r "${KUBECONFIG_FILE}" ] || die "${KUBECONFIG_FILE} unreadable"

NODE_TOKEN=$(cat "${TOKEN_FILE}")
# K3s ships kubeconfig with server: https://127.0.0.1:6443 — rewrite to the
# NLB endpoint so it works from outside the control plane VM.
KUBECONFIG_REWRITTEN=$(sed "s|server: https://127\.0\.0\.1:6443|server: ${EKS_NLB_ENDPOINT}|" "${KUBECONFIG_FILE}")
# The cluster CA the daemon records (base64 PEM) is exactly the
# certificate-authority-data the admin kubeconfig already embeds.
CA_B64=$(awk '/certificate-authority-data:/ {print $2; exit}' "${KUBECONFIG_FILE}")
[ -n "${CA_B64}" ] || die "no certificate-authority-data in ${KUBECONFIG_FILE}"
# The OIDC JWKS the apiserver serves from the signing key cloud-init seeded. The
# daemon cross-checks its kid/kty against the controller-generated keypair.
JWKS=$(kubectl --kubeconfig "${KUBECONFIG_FILE}" get --raw='/openid/v1/jwks' 2>/dev/null)
[ -n "${JWKS}" ] || die "apiserver returned empty /openid/v1/jwks"

# 3. Publish the four one-shot bootstrap messages. Each is a BootstrapEnvelope
# JSON document (handlers/eks/nats_bootstrap.go); jq encodes the values so
# embedded newlines/quotes in the kubeconfig and JWKS stay valid JSON.
NATS_ARGS="-s ${SPINIFEX_NATS_URL}"
if [ -n "${SPINIFEX_NATS_TOKEN:-}" ]; then
    NATS_ARGS="${NATS_ARGS} --token ${SPINIFEX_NATS_TOKEN}"
fi
if [ -n "${SPINIFEX_NATS_CA:-}" ] && [ -f "${SPINIFEX_NATS_CA}" ]; then
    NATS_ARGS="${NATS_ARGS} --tlsca ${SPINIFEX_NATS_CA}"
fi

# publish_envelope <kind-suffix>: reads the envelope JSON from stdin.
publish_envelope() {
    subj="eks.bus.${EKS_ACCOUNT_ID}.${EKS_CLUSTER_NAME}.$1"
    log "publishing $1 to ${subj}"
    # shellcheck disable=SC2086
    nats ${NATS_ARGS} pub "${subj}" --force-stdin
}

jq -n --arg t "${NODE_TOKEN}"           '{token: $t}'                | publish_envelope k3s-bootstrap-token
jq -n --arg k "${KUBECONFIG_REWRITTEN}" '{adminKubeconfig: $k}'      | publish_envelope k3s-admin-kubeconfig
jq -n --arg j "${JWKS}"                 '{jwks: $j}'                  | publish_envelope k3s-oidc-jwks
jq -n --arg c "${CA_B64}"               '{certificateAuthority: $c}' | publish_envelope k3s-ca

# 4. Self-disable. Remove sentinel, pull from runlevel.
rm -f "${SENTINEL}"
rc-update del k3s-first-boot default 2>/dev/null || true
log "first boot complete"
