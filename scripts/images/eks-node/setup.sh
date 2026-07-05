#!/bin/sh
set -eu

# setup.sh — guest customisation for the unified eks-node AMI (server + agent).
#
# Runs inside the libguestfs appliance (via virt-customize --run) under
# build-system-image.sh after packages and binaries are installed. Downloads the
# pinned K3s binary, verifies its SHA256, drops it into /usr/local/bin, sets
# executable bits on all role init scripts + cron entries, and writes the K3s
# server config skeleton. The role (server vs agent) is selected per-instance at
# first boot by eks-node-role.
#
# Network access inside the appliance is provided by virt-customize --network.
# curl is in APK_PACKAGES.

K3S_VERSION="v1.32.5+k3s1"
K3S_URL_BASE="https://github.com/k3s-io/k3s/releases/download/${K3S_VERSION}"

# Pull the upstream signed checksums file and pin the amd64 line. A tampered
# release replacing both files would still produce a self-consistent download,
# so the SHA file URL is the trust anchor — anyone forging a Mulga AMI build
# would need to compromise the k3s-io GitHub release artefacts.
echo "[eks-node-setup] fetching k3s checksums ${K3S_VERSION}"
curl -fsSL -o /tmp/k3s-checksums.txt "${K3S_URL_BASE}/sha256sum-amd64.txt"
K3S_SHA256=$(awk '/[ \t]k3s$/{print $1; exit}' /tmp/k3s-checksums.txt)
if [ -z "${K3S_SHA256}" ]; then
    echo "[eks-node-setup] could not parse k3s sha256 from upstream checksums"
    cat /tmp/k3s-checksums.txt
    exit 1
fi
echo "[eks-node-setup] downloading k3s ${K3S_VERSION} (sha256=${K3S_SHA256})"
curl -fsSL -o /usr/local/bin/k3s "${K3S_URL_BASE}/k3s"
echo "${K3S_SHA256}  /usr/local/bin/k3s" > /tmp/k3s.sha256
if ! sha256sum -c /tmp/k3s.sha256; then
    echo "[eks-node-setup] k3s SHA256 mismatch — refusing to bake AMI"
    exit 1
fi
chmod 0755 /usr/local/bin/k3s
ln -sf /usr/local/bin/k3s /usr/local/bin/kubectl
ln -sf /usr/local/bin/k3s /usr/local/bin/crictl
ln -sf /usr/local/bin/k3s /usr/local/bin/ctr

# Init scripts ship as 0644 from INSTALL_FILES; OpenRC requires 0755. Every
# role's services are baked; the selector enables the right ones at first boot.
chmod 0755 /etc/init.d/eks-node-role /etc/init.d/k3s /etc/init.d/k3s-agent \
    /etc/init.d/eks-token-webhook /etc/init.d/k3s-first-boot /etc/init.d/mulga-eks-state-report \
    /etc/init.d/mulga-eks-addon-sync /etc/init.d/konnectivity-server
chmod 0755 /usr/local/sbin/eks-node-role /usr/local/sbin/k3s-first-boot \
    /usr/local/sbin/mulga-eks-state-report /usr/local/sbin/mulga-eks-addon-sync
chmod 0755 /etc/init.d/mulga-ebs-byid /usr/local/sbin/mulga-ebs-byid
chmod 0755 /etc/init.d/mulga-eks-provider-id /usr/local/sbin/mulga-eks-provider-id
chmod 0755 /etc/init.d/mulga-mgmt-net /usr/local/sbin/mulga-mgmt-net
chmod 0755 /etc/init.d/mulga-vpc-mtu /usr/local/sbin/mulga-vpc-mtu
# etcd snapshot runs on two crond cadences (nightly + a 15-min RPO window); the
# same script picks its tier from the dir it is invoked from. Both must be 0755.
mkdir -p /etc/periodic/15min
chmod 0755 /etc/periodic/daily/mulga-eks-etcd-snapshot /etc/periodic/15min/mulga-eks-etcd-snapshot

# mulga-mgmt-net goes in the boot runlevel, not default (where ENABLE_SERVICES
# lands services). It DHCPs the data NIC so the init-local Ec2 crawl reaches
# IMDS; a default entry runs after cloud-init-local and is too late.
rc-update add mulga-mgmt-net boot

# EBS by-id bridge: route every virtio-blk event through mulga-ebs-byid, which
# delegates to the stock persistent-storage helper and then mints the
# nvme-Amazon_Elastic_Block_Store_<serial> link the EBS CSI node plugin resolves.
# busybox mdev stops at the first matching rule, so the stock vd* persistent-
# storage line is replaced in place — appending a second vd* rule would be
# shadowed and never fire. The leading '*' runs the command on add and remove.
sed -i \
    's#^vd\[a-z\]\.\*[[:space:]].*persistent-storage#vd[a-z].*\troot:disk 0660 */usr/local/sbin/mulga-ebs-byid#' \
    /etc/mdev.conf
grep -q 'mulga-ebs-byid' /etc/mdev.conf || {
    echo "[eks-node-setup] failed to wire mulga-ebs-byid into /etc/mdev.conf"
    exit 1
}

# K3s server config — skeleton; cloud-init / first-boot fills in the
# per-cluster fields (cluster-cidr, service-cidr, token-file, etc). Agents
# ignore this file (they run `k3s agent`).
# IAM token-auth is wired here via kube-apiserver-arg: the eks-token-webhook
# service (ordered `before k3s`) writes its kubeconfig to
# /etc/spinifex-eks/token-webhook.kubeconfig before the apiserver reads it.
# This only affects bearer-token requests; anonymous and client-cert paths
# (the first-boot /readyz probe uses the admin kubeconfig) are unaffected.
mkdir -p /etc/rancher/k3s
cat > /etc/rancher/k3s/config.yaml.skel <<'EOF'
# Populated at first boot by cloud-init user-data via k3s-first-boot.sh.
# Fields documented at https://docs.k3s.io/installation/configuration.
# cluster-init selects the embedded etcd datastore. cloud-init write_files
# normally overrides this skeleton; kept consistent as the boot fallback.
cluster-init: true
disable:
  - traefik
  - servicelb
  - local-storage
kube-apiserver-arg:
  - "authentication-token-webhook-config-file=/etc/spinifex-eks/token-webhook.kubeconfig"
  - "authentication-token-webhook-cache-ttl=5m"
EOF

# Shared state dir for both roles (agent.env / role marker live here).
mkdir -p /etc/spinifex-eks

# Sentinel file marker — k3s-first-boot (server role) self-disables after first
# success by checking for this path. Initial state is "not yet run".
touch /var/lib/spinifex-eks/first-boot.pending 2>/dev/null || {
    mkdir -p /var/lib/spinifex-eks
    touch /var/lib/spinifex-eks/first-boot.pending
}

# Bind /dev/console to the serial port so userspace boot output — OpenRC
# service starts, cloud-init, role selection, and k3s-first-boot diagnostics —
# reaches ttyS0, which the orchestrator captures to a host-side log. The stock
# Alpine cloud image lists `console=tty0` LAST in default_kernel_opts; Linux
# makes the last console= the controlling /dev/console, so userspace logs to
# the framebuffer and the serial capture sees only kernel dmesg. Reorder so
# ttyS0 is last in both the generator config and the rendered extlinux.conf.
sed -i \
    's|console=ttyS0,115200n8 console=ttyAMA0,115200n8 console=tty0|console=tty0 console=ttyAMA0,115200n8 console=ttyS0,115200n8|' \
    /etc/update-extlinux.conf /boot/extlinux.conf

# Cut the boot-menu countdown from 10s to ~1s. The stock cloud image waits 10s at
# the SYSLINUX menu before auto-booting — a fixed, network-independent tax on every
# VM start. Patch the generator config (timeout in seconds) and the rendered output
# (TIMEOUT in 1/10s) so a regenerate keeps the short value. A small nonzero value is
# kept so the menu stays interruptible over serial; TIMEOUT 0 would wait forever.
sed -i 's/^timeout=.*/timeout=1/' /etc/update-extlinux.conf
sed -i 's/^TIMEOUT[[:space:]].*/TIMEOUT 10/' /boot/extlinux.conf

# Disable dhcpcd IPv4LL so it never hijacks IMDS on a multi-NIC system VM. The
# EKS control plane is dual-NIC; the mgmt NIC sits on br-mgmt (no DHCP server),
# so dhcpcd's no-lease fallback would assign a 169.254.x.x address and a
# 169.254.0.0/16 route on it. That /16 captures 169.254.169.254 and steers IMDS
# off the data NIC (away from the per-tap br-imds datapath) and onto the mgmt
# NIC, where the host RSTs it. noipv4ll turns off only the no-lease fallback:
# real DHCP leases (workers, the data ENI) are unaffected, and IMDS reaches .254
# through the data NIC's default route and the br-imds demux, as single-NIC
# instances already do.
if ! grep -qxF 'noipv4ll' /etc/dhcpcd.conf 2>/dev/null; then
    echo 'noipv4ll' >> /etc/dhcpcd.conf
fi

echo "[eks-node-setup] done"
