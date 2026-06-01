#!/bin/sh
set -eu

# setup.sh — chroot customisation for the eks-agent AMI.
# Pulls the same K3s release as eks-server (lock-step versioning per Q12) and
# wires the k3s-agent OpenRC service.

K3S_VERSION="v1.32.5+k3s1"
K3S_URL_BASE="https://github.com/k3s-io/k3s/releases/download/${K3S_VERSION}"

echo "[eks-agent-setup] fetching k3s checksums ${K3S_VERSION}"
curl -fsSL -o /tmp/k3s-checksums.txt "${K3S_URL_BASE}/sha256sum-amd64.txt"
K3S_SHA256=$(awk '/[ \t]k3s$/{print $1; exit}' /tmp/k3s-checksums.txt)
if [ -z "${K3S_SHA256}" ]; then
    echo "[eks-agent-setup] could not parse k3s sha256 from upstream checksums"
    cat /tmp/k3s-checksums.txt
    exit 1
fi

echo "[eks-agent-setup] downloading k3s ${K3S_VERSION} (sha256=${K3S_SHA256})"
curl -fsSL -o /usr/local/bin/k3s "${K3S_URL_BASE}/k3s"
echo "${K3S_SHA256}  /usr/local/bin/k3s" > /tmp/k3s.sha256
if ! sha256sum -c /tmp/k3s.sha256; then
    echo "[eks-agent-setup] k3s SHA256 mismatch — refusing to bake AMI"
    exit 1
fi
chmod 0755 /usr/local/bin/k3s
ln -sf /usr/local/bin/k3s /usr/local/bin/kubectl
ln -sf /usr/local/bin/k3s /usr/local/bin/crictl
ln -sf /usr/local/bin/k3s /usr/local/bin/ctr

# nats-cli — see eks-server setup.sh for rationale.
NATS_CLI_VERSION="0.4.0"
NATS_CLI_URL="https://github.com/nats-io/natscli/releases/download/v${NATS_CLI_VERSION}/nats-${NATS_CLI_VERSION}-linux-amd64.zip"
echo "[eks-agent-setup] downloading nats-cli ${NATS_CLI_VERSION}"
curl -fsSL -o /tmp/nats-cli.zip "${NATS_CLI_URL}"
apk add --no-cache unzip
unzip -q -d /tmp/nats-cli /tmp/nats-cli.zip
install -m 0755 /tmp/nats-cli/nats-${NATS_CLI_VERSION}-linux-amd64/nats /usr/local/bin/nats
rm -rf /tmp/nats-cli /tmp/nats-cli.zip
apk del --no-cache unzip

chmod 0755 /etc/init.d/k3s-agent
chmod 0755 /etc/periodic/15min/mulga-eks-state-report

mkdir -p /etc/spinifex-eks
echo "[eks-agent-setup] done"
