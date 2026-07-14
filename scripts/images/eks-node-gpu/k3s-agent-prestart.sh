#!/bin/sh
set -eu

# k3s-agent-prestart — reproduces the start_pre logic from
# scripts/images/eks-node/k3s-agent.initd for the systemd unit: requires
# K3S_URL + K3S_TOKEN from /etc/spinifex-eks/agent.env (sourced by the unit's
# own EnvironmentFile=), then resolves the kubelet provider-id from IMDSv2
# (aws:///<az>/<instance-id>, EKS parity so the AWS Load Balancer Controller
# can resolve nodes for ip-mode target registration) and writes it as an
# extra k3s agent argument for the unit's ExecStart to source.
#
# Run as ExecStartPre=; a nonzero exit here fails k3s-agent.service before k3s
# is ever invoked, same as the OpenRC start_pre returning 1.

ENV_FILE=/etc/spinifex-eks/agent.env
DROPIN=/run/k3s-agent-extra-args.env
LOGTAG="k3s-agent-prestart"

# EnvironmentFile= already injects agent.env into this process' environment,
# but source it directly too so this script also works if invoked standalone.
if [ -f "${ENV_FILE}" ]; then
    # shellcheck disable=SC1090
    . "${ENV_FILE}"
fi

if [ -z "${K3S_URL:-}" ] || [ -z "${K3S_TOKEN:-}" ]; then
    echo "[${LOGTAG}] K3S_URL and K3S_TOKEN must be set in ${ENV_FILE}" >&2
    exit 1
fi

IMDS=http://169.254.169.254/latest
CURL="curl -fsS --connect-timeout 3 --max-time 5"

EXTRA_ARGS=""
TOK=$(${CURL} -X PUT -H 'X-aws-ec2-metadata-token-ttl-seconds: 120' "${IMDS}/api/token" 2>/dev/null || true)
if [ -n "${TOK}" ]; then
    IID=$(${CURL} -H "X-aws-ec2-metadata-token: ${TOK}" "${IMDS}/meta-data/instance-id" 2>/dev/null || true)
    AZ=$(${CURL} -H "X-aws-ec2-metadata-token: ${TOK}" "${IMDS}/meta-data/placement/availability-zone" 2>/dev/null || true)
    if [ -n "${IID}" ] && [ -n "${AZ}" ]; then
        EXTRA_ARGS="--kubelet-arg=provider-id=aws:///${AZ}/${IID}"
    else
        echo "[${LOGTAG}] IMDS instance-id/az empty; keeping default providerID" >&2
    fi
else
    echo "[${LOGTAG}] IMDS token request failed; keeping default providerID" >&2
fi

echo "K3S_AGENT_EXTRA_ARGS=${EXTRA_ARGS}" > "${DROPIN}"
