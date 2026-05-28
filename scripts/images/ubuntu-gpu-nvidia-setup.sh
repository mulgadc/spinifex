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

# The Ubuntu minimal cloud image ships without a kernel. Install linux-image-generic
# first so DKMS has a kernel to build against and the guest boots the same kernel
# the module was built for.
apt-get install -y --no-install-recommends \
    linux-image-generic \
    initramfs-tools

KVER=$(ls /boot/vmlinuz-* 2>/dev/null | sort -V | tail -1 | sed 's|/boot/vmlinuz-||')
if [[ -z "${KVER}" ]]; then
    echo "ERROR: No kernel found under /boot after kernel install"
    exit 1
fi
echo "Target kernel: ${KVER}"

# Install headers for the exact installed kernel version for the DKMS build.
apt-get install -y --no-install-recommends \
    "linux-headers-${KVER}"

# Install the NVIDIA server DKMS driver. Use the distro default version (no
# hardcoded suffix) so Ubuntu 26.04's 580 driver is used instead of 550.
# nvidia-dkms-server is a meta package that resolves to the current recommended
# version; nvidia-utils-server brings nvidia-smi and related tools.
apt-get install -y --no-install-recommends \
    nvidia-dkms-server \
    nvidia-utils-server

# Detect the installed NVIDIA DKMS module name + version for explicit build.
NVIDIA_DKMS_NAME=$(dkms status 2>/dev/null | awk -F'[,/ ]+' '/nvidia/{print $1; exit}')
NVIDIA_DKMS_VER=$(dkms status 2>/dev/null  | awk -F'[,/ ]+' '/nvidia/{print $2; exit}')

if [[ -n "${NVIDIA_DKMS_NAME}" && -n "${NVIDIA_DKMS_VER}" ]]; then
    echo "Building NVIDIA DKMS module ${NVIDIA_DKMS_NAME}/${NVIDIA_DKMS_VER} for kernel ${KVER}..."
    dkms install -m "${NVIDIA_DKMS_NAME}" -v "${NVIDIA_DKMS_VER}" --kernelver "${KVER}" --force 2>&1
else
    echo "WARNING: nvidia DKMS module not found in dkms status — driver install may have failed"
fi

# Verify the module was actually built.
NVIDIA_KO=$(find "/lib/modules/${KVER}/updates" -name "nvidia.ko*" 2>/dev/null | head -1)
if [[ -z "${NVIDIA_KO}" ]]; then
    echo "ERROR: nvidia.ko not found under /lib/modules/${KVER}/updates after DKMS build"
    exit 1
fi
echo "NVIDIA kernel module: ${NVIDIA_KO}"

# Pin the exact kernel + headers so unattended-upgrades cannot replace the
# kernel the DKMS module was built for.
apt-mark hold \
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


echo "NVIDIA GPU image setup complete: kernel=${KVER}, NVIDIA driver + DKMS pre-built"
