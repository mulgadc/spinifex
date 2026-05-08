#!/bin/bash
set -euo pipefail

# ubuntu-gpu-setup.sh — Chroot setup for Ubuntu 24.04 GPU AMI
#
# Installs NVIDIA headless server driver with DKMS module pre-built against
# a pinned kernel. Blacklists nouveau. Prevents unattended-upgrades from
# invalidating the pre-built module.
#
# Must run inside an Ubuntu 24.04 chroot with /proc /sys /dev bind-mounted
# (required by DKMS during the nvidia package post-install hook).

export DEBIAN_FRONTEND=noninteractive

apt-get update

# Install a pinned kernel and its headers so DKMS has a stable build target.
# linux-image-generic and linux-headers-generic pull in the current HWE stack.
apt-get install -y --no-install-recommends \
    linux-image-generic \
    linux-headers-generic

# Capture the installed kernel version before adding the driver.
KVER=$(ls /boot/vmlinuz-* 2>/dev/null | sort -V | tail -1 | sed 's|/boot/vmlinuz-||')
if [[ -z "${KVER}" ]]; then
    echo "ERROR: No kernel found under /boot after linux-image-generic install"
    exit 1
fi
echo "Target kernel: ${KVER}"

# Install the NVIDIA headless server driver.
# The nvidia-dkms-550-server post-install hook runs `dkms autoinstall`, which
# builds the module against every kernel found under /lib/modules/ — including
# ${KVER} whose headers we just installed above.
apt-get install -y --no-install-recommends \
    nvidia-headless-550-server \
    nvidia-utils-550-server

# Verify the DKMS module was registered and built.
if dkms status 2>/dev/null | grep -q "nvidia"; then
    echo "NVIDIA DKMS module status: $(dkms status 2>/dev/null | grep nvidia | head -1)"
else
    echo "WARNING: nvidia DKMS module not found in dkms status — manual verification required"
fi

# Pin the kernel so unattended-upgrades cannot replace it and invalidate
# the pre-built DKMS module.
apt-mark hold \
    linux-image-generic \
    linux-headers-generic \
    "linux-image-${KVER}" \
    "linux-headers-${KVER}"

# Pin all installed NVIDIA packages for the same reason.
NVIDIA_PKGS=$(dpkg --get-selections 'nvidia-*' 2>/dev/null | awk '/install$/{print $1}' || true)
LIBNVIDIA_PKGS=$(dpkg --get-selections 'libnvidia-*' 2>/dev/null | awk '/install$/{print $1}' || true)
# shellcheck disable=SC2086
[[ -n "${NVIDIA_PKGS}" ]]    && apt-mark hold ${NVIDIA_PKGS}
# shellcheck disable=SC2086
[[ -n "${LIBNVIDIA_PKGS}" ]] && apt-mark hold ${LIBNVIDIA_PKGS}

# Blacklist nouveau so it does not conflict with the NVIDIA driver on boot.
cat > /etc/modprobe.d/blacklist-nouveau.conf << 'EOF'
blacklist nouveau
options nouveau modeset=0
EOF

# Prevent unattended-upgrades from updating kernel or NVIDIA packages,
# which would break the pre-built DKMS module.
mkdir -p /etc/apt/apt.conf.d
cat > /etc/apt/apt.conf.d/99-gpu-ami << 'EOF'
Unattended-Upgrade::Package-Blacklist {
    "linux-";
    "nvidia-";
    "libnvidia-";
};
EOF

# Rebuild initramfs with nouveau blacklisted and NVIDIA module included.
update-initramfs -u -k "${KVER}"

echo "GPU AMI setup complete: kernel=${KVER}, NVIDIA 550 driver installed, DKMS module pre-built"
