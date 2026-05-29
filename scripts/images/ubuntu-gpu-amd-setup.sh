#!/bin/bash
set -euo pipefail

# ubuntu-gpu-amd-setup.sh — Chroot setup for an AMD GPU-capable guest image.
#
# Installs linux-firmware (AMD firmware blobs required by the amdgpu kernel
# module), ROCm CLI tools, Python toolchain, and common utilities. Rebuilds
# initramfs so AMD firmware is available at early boot inside the guest.
#
# Host-side VFIO setup (driver blacklisting, IOMMU, initramfs) is handled
# by `spx admin gpu setup` — this script is for the guest VM image only.
#
# Must run inside an Ubuntu 26.04 chroot with /proc /sys /dev bind-mounted.

export DEBIAN_FRONTEND=noninteractive

apt-get update -qq
apt-get install -y --no-install-recommends curl gnupg ca-certificates

UBUNTU_CODENAME=$(. /etc/os-release && echo "${UBUNTU_CODENAME:-${VERSION_CODENAME}}")

# AMD ROCm 7.2 repo — pinned to "noble" (24.04) since AMD hasn't published a
# "resolute" (26.04) dist yet; the noble packages are ABI-compatible.
mkdir -p /etc/apt/keyrings
curl -fsSL https://repo.radeon.com/rocm/rocm.gpg.key \
    | gpg --dearmor -o /etc/apt/keyrings/rocm.gpg
echo "deb [arch=amd64 signed-by=/etc/apt/keyrings/rocm.gpg] https://repo.radeon.com/rocm/apt/7.2.3 noble main" \
    > /etc/apt/sources.list.d/rocm.list

# The Ubuntu minimal cloud image already ships with a kernel. Detect it and
# install matching headers — do NOT install linux-image-generic, which pulls a
# newer ABI the bootloader won't know about (grub-install can't run in a chroot).
KVER=$(ls /boot/vmlinuz-* 2>/dev/null | sort -V | tail -1 | sed 's|/boot/vmlinuz-||') || true
if [[ -z "${KVER}" ]]; then
    # Ubuntu 26.04 minimal cloud image keeps the kernel on the ESP (not mounted
    # in the chroot), so /boot is empty — fall back to /lib/modules.
    KVER=$(ls /lib/modules/ 2>/dev/null | sort -V | tail -1)
fi
if [[ -z "${KVER}" ]]; then
    echo "ERROR: No kernel found in /boot or /lib/modules — base image may be missing a kernel"
    exit 1
fi
echo "Target kernel: ${KVER}"

# Prevent kernel postinst hooks from attempting grub-install against the host disk.
cat > /etc/kernel-img.conf <<'EOF'
do_symlinks = yes
do_bootloader = no
do_initrd = yes
link_in_boot = yes
EOF

# linux-firmware carries the AMD GPU firmware blobs loaded by the amdgpu
# kernel module at boot. Without them the GPU is invisible to guest userland.
apt-get install -y -o Acquire::Retries=3 --no-install-recommends \
    "linux-headers-${KVER}" \
    linux-firmware \
    initramfs-tools \
    pciutils \
    python3 python3-venv python3-pip \
    git curl wget htop tmux \
    ffmpeg libgl1 libglib2.0-0

apt-get update -qq
apt-get install -y --no-install-recommends rocminfo rocm-smi-lib amd-smi-lib

# Create /opt/rocm symlink (normally done by rocm-core) and add to PATH.
ROCM_DIR=$(ls -d /opt/rocm-* 2>/dev/null | sort -V | tail -1)
if [[ -n "${ROCM_DIR}" ]]; then
    ln -sfn "${ROCM_DIR}" /opt/rocm
    echo "export PATH=\"/opt/rocm/bin:\$PATH\"" > /etc/profile.d/rocm.sh
    chmod 644 /etc/profile.d/rocm.sh
fi

# ── Docker CE ─────────────────────────────────────────────────────────────────
# AMD GPU containers use device passthrough — no separate container runtime
# is needed. Users pass --device=/dev/kfd --device=/dev/dri at run time.
mkdir -p /usr/share/keyrings
curl -fsSL https://download.docker.com/linux/ubuntu/gpg \
    | gpg --dearmor -o /usr/share/keyrings/docker-archive-keyring.gpg
echo "deb [arch=amd64 signed-by=/usr/share/keyrings/docker-archive-keyring.gpg] https://download.docker.com/linux/ubuntu ${UBUNTU_CODENAME} stable" \
    > /etc/apt/sources.list.d/docker.list
apt-get update -qq
apt-get install -y --no-install-recommends \
    docker-ce docker-ce-cli containerd.io docker-buildx-plugin docker-compose-plugin
systemctl enable docker

# Ensure render + video groups exist for /dev/kfd and /dev/dri access.
groupadd -f render
groupadd -f video

# Add the cloud-init default user (UID 1000) to docker/render/video at first boot.
mkdir -p /etc/cloud/cloud.cfg.d
cat > /etc/cloud/cloud.cfg.d/99-gpu-groups.cfg <<'EOF'
runcmd:
  - usermod -aG docker,render,video $(id -un 1000) || true
EOF

echo "Rebuilding initramfs for kernel: ${KVER}"
update-initramfs -u -k "${KVER}"

# Prevent unattended-upgrades from replacing linux-firmware mid-lifecycle;
# a firmware change without a matching initramfs rebuild can make the GPU invisible.
mkdir -p /etc/apt/apt.conf.d
cat > /etc/apt/apt.conf.d/99-gpu-ami <<'EOF'
Unattended-Upgrade::Package-Blacklist {
    "linux-firmware";
    "linux-image-";
    "linux-headers-";
    "docker-";
    "containerd";
};
EOF

echo "AMD GPU image setup complete: kernel=${KVER}, linux-firmware and ROCm userland installed, Docker ready"
