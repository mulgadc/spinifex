#!/bin/bash
set -euo pipefail

# ubuntu-gpu-nvidia-setup.sh — Chroot setup for an NVIDIA GPU-capable guest image.
#
# Pre-builds the NVIDIA headless server driver via DKMS against a pinned kernel
# so guests can use the GPU immediately after launch. Also installs Python
# toolchain and common utilities.
#
# nouveau is blacklisted inside the guest image so the NVIDIA kernel module can
# bind — this is distinct from the host-side blacklist that `spx admin gpu setup`
# configures for VFIO passthrough.
#
# Host-side VFIO setup (IOMMU, vfio-pci binding, host initramfs) is handled
# by `spx admin gpu setup` — this script is for the guest VM image only.
#
# Must run inside an Ubuntu 26.04 chroot with /proc /sys /dev bind-mounted
# (required by DKMS during the nvidia package post-install hook).

export DEBIAN_FRONTEND=noninteractive

apt-get update -qq

# Pinned kernel and headers so DKMS has a stable build target.
apt-get install -y --no-install-recommends \
    linux-image-generic \
    linux-headers-generic \
    initramfs-tools

KVER=$(ls /boot/vmlinuz-* 2>/dev/null | sort -V | tail -1 | sed 's|/boot/vmlinuz-||')
if [[ -z "${KVER}" ]]; then
    echo "ERROR: No kernel found under /boot"
    exit 1
fi
echo "Target kernel: ${KVER}"

# nvidia-dkms-550-server post-install runs `dkms autoinstall` against ${KVER}.
apt-get install -y --no-install-recommends \
    nvidia-headless-550-server \
    nvidia-utils-550-server

if dkms status 2>/dev/null | grep -q "nvidia"; then
    echo "NVIDIA DKMS module: $(dkms status 2>/dev/null | grep nvidia | head -1)"
else
    echo "WARNING: nvidia DKMS module not found in dkms status"
fi

# Pin kernel + NVIDIA packages so unattended-upgrades cannot invalidate the
# pre-built DKMS module inside the guest.
apt-mark hold \
    linux-image-generic \
    linux-headers-generic \
    "linux-image-${KVER}" \
    "linux-headers-${KVER}"

NVIDIA_PKGS=$(dpkg --get-selections 'nvidia-*' 2>/dev/null | awk '/install$/{print $1}' || true)
LIBNVIDIA_PKGS=$(dpkg --get-selections 'libnvidia-*' 2>/dev/null | awk '/install$/{print $1}' || true)
# shellcheck disable=SC2086
[[ -n "${NVIDIA_PKGS}" ]]    && apt-mark hold ${NVIDIA_PKGS}
# shellcheck disable=SC2086
[[ -n "${LIBNVIDIA_PKGS}" ]] && apt-mark hold ${LIBNVIDIA_PKGS}

# Blacklist nouveau inside the guest — nvidia.ko and nouveau conflict, and
# without this blacklist the NVIDIA driver will not bind after boot.
cat > /etc/modprobe.d/blacklist-nouveau.conf <<'EOF'
blacklist nouveau
options nouveau modeset=0
EOF

# Common tooling matching the AMD image.
apt-get install -y -o Acquire::Retries=3 \
    pciutils \
    python3 python3-venv python3-pip \
    git curl wget htop tmux \
    ffmpeg libgl1 libglib2.0-0

mkdir -p /etc/apt/apt.conf.d
cat > /etc/apt/apt.conf.d/99-gpu-ami <<'EOF'
Unattended-Upgrade::Package-Blacklist {
    "linux-";
    "nvidia-";
    "libnvidia-";
};
EOF

# Rebuild initramfs with nouveau blacklisted and NVIDIA module included.
update-initramfs -u -k "${KVER}"

# The DKMS build tree and kernel headers are only needed to compile nvidia.ko.
# The built module lives in /lib/modules/${KVER}/ — remove headers and build
# artefacts to recover ~500MB from the image.
apt-get remove --purge -y linux-headers-generic "linux-headers-${KVER}" 2>/dev/null || true
apt-get autoremove -y 2>/dev/null || true
rm -rf /var/lib/dkms/nvidia/*/build

echo "NVIDIA GPU image setup complete: kernel=${KVER}, NVIDIA 550 driver + DKMS pre-built"
