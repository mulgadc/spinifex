#!/bin/sh
set -eu

# setup.sh — chroot customisation for the eks-server AMI.
#
# Runs inside an Alpine 3.21 chroot under build-system-image.sh after packages
# and binaries are installed. Downloads the pinned K3s binary, verifies its
# SHA256, drops it into /usr/local/bin, sets executable bits on init scripts +
# cron entries, and writes the K3s server config skeleton.
#
# Network access inside the chroot is provided by the host's /etc/resolv.conf
# (build-system-image.sh copies it in). curl is in APK_PACKAGES.

K3S_VERSION="v1.32.5+k3s1"
K3S_URL_BASE="https://github.com/k3s-io/k3s/releases/download/${K3S_VERSION}"

# Pull the upstream signed checksums file and pin the amd64 line. A tampered
# release replacing both files would still produce a self-consistent download,
# so the SHA file URL is the trust anchor — anyone forging a Mulga AMI build
# would need to compromise the k3s-io GitHub release artefacts.
echo "[eks-server-setup] fetching k3s checksums ${K3S_VERSION}"
curl -fsSL -o /tmp/k3s-checksums.txt "${K3S_URL_BASE}/sha256sum-amd64.txt"
K3S_SHA256=$(awk '/[ \t]k3s$/{print $1; exit}' /tmp/k3s-checksums.txt)
if [ -z "${K3S_SHA256}" ]; then
    echo "[eks-server-setup] could not parse k3s sha256 from upstream checksums"
    cat /tmp/k3s-checksums.txt
    exit 1
fi
echo "[eks-server-setup] downloading k3s ${K3S_VERSION} (sha256=${K3S_SHA256})"
curl -fsSL -o /usr/local/bin/k3s "${K3S_URL_BASE}/k3s"
echo "${K3S_SHA256}  /usr/local/bin/k3s" > /tmp/k3s.sha256
if ! sha256sum -c /tmp/k3s.sha256; then
    echo "[eks-server-setup] k3s SHA256 mismatch — refusing to bake AMI"
    exit 1
fi
chmod 0755 /usr/local/bin/k3s
ln -sf /usr/local/bin/k3s /usr/local/bin/kubectl
ln -sf /usr/local/bin/k3s /usr/local/bin/crictl
ln -sf /usr/local/bin/k3s /usr/local/bin/ctr

# nats-cli is not in Alpine main/community — pull the upstream release binary.
# Used by k3s-first-boot.sh (one-shot publishes) and mulga-eks-state-report.sh
# (periodic publishes).
NATS_CLI_VERSION="0.4.0"
NATS_CLI_URL="https://github.com/nats-io/natscli/releases/download/v${NATS_CLI_VERSION}/nats-${NATS_CLI_VERSION}-linux-amd64.zip"
echo "[eks-server-setup] downloading nats-cli ${NATS_CLI_VERSION}"
curl -fsSL -o /tmp/nats-cli.zip "${NATS_CLI_URL}"
apk add --no-cache unzip
unzip -q -d /tmp/nats-cli /tmp/nats-cli.zip
install -m 0755 /tmp/nats-cli/nats-${NATS_CLI_VERSION}-linux-amd64/nats /usr/local/bin/nats
rm -rf /tmp/nats-cli /tmp/nats-cli.zip
apk del --no-cache unzip

# Init scripts ship as 0644 from INSTALL_FILES; OpenRC requires 0755.
chmod 0755 /etc/init.d/k3s /etc/init.d/eks-token-webhook /etc/init.d/k3s-first-boot
chmod 0755 /usr/local/sbin/k3s-first-boot
chmod 0755 /etc/periodic/15min/mulga-eks-state-report
chmod 0755 /etc/periodic/daily/mulga-eks-etcd-snapshot

# K3s server config — empty skeleton; cloud-init / first-boot fills in the
# per-cluster fields (cluster-cidr, service-cidr, token-file, etc).
# Webhook-token-auth is NOT wired in the skeleton: enabling it before the
# eks-token-webhook binary is the real implementation (currently a 503 stub,
# replaced by cs-eks-6c) would block /healthz from anonymous callers. The
# Sprint 6c orchestrator drops a webhook-config.yaml + restarts k3s once
# the binary is the real one.
mkdir -p /etc/rancher/k3s
cat > /etc/rancher/k3s/config.yaml.skel <<'EOF'
# Populated at first boot by cloud-init user-data via k3s-first-boot.sh.
# Fields documented at https://docs.k3s.io/installation/configuration.
cluster-init: true
disable:
  - traefik
  - servicelb
  - local-storage
EOF

# Sentinel file marker — k3s-first-boot self-disables after first success by
# checking for this path. Initial state is "not yet run".
touch /var/lib/spinifex-eks/first-boot.pending 2>/dev/null || {
    mkdir -p /var/lib/spinifex-eks
    touch /var/lib/spinifex-eks/first-boot.pending
}

echo "[eks-server-setup] done"
