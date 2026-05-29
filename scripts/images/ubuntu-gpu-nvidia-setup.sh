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

apt-get update

# The Ubuntu minimal cloud image already ships with a kernel. We detect it and
# build DKMS against it — do NOT install linux-image-generic, which pulls a
# newer kernel ABI that won't boot (grub-install can't run in a chroot, so the
# bootloader never learns about the new kernel and keeps booting the original).
apt-get install -y --no-install-recommends initramfs-tools

echo "=== /boot ==="
ls -la /boot/ || true
echo "=== /lib/modules ==="
ls /lib/modules/ || true
echo "=== installed kernel packages ==="
dpkg -l 'linux-image-*' | grep '^ii' || true

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

# Prevent kernel postinst hooks from attempting grub-install against the host
# disk — this fails silently in a chroot and can abort the postinst, leaving
# /boot incomplete.
cat > /etc/kernel-img.conf <<'EOF'
do_symlinks = yes
do_bootloader = no
do_initrd = yes
link_in_boot = yes
EOF

# Install headers for the exact pre-installed kernel so DKMS builds against it.
apt-get install -y --no-install-recommends \
    "linux-headers-${KVER}"

# Detect the latest available versioned NVIDIA server driver. Ubuntu 26.04+
# no longer ships unversioned meta-packages (nvidia-dkms-server); the packages
# are now versioned (e.g. nvidia-dkms-570-server).
NVIDIA_VER=$(apt-cache search '^nvidia-dkms-[0-9]+-server$' 2>/dev/null \
    | grep -oP '(?<=nvidia-dkms-)\d+(?=-server)' | sort -rn | head -1)
if [[ -z "${NVIDIA_VER}" ]]; then
    echo "ERROR: No versioned nvidia-dkms-*-server package found in apt cache"
    apt-cache search nvidia-dkms || true
    exit 1
fi
echo "Installing NVIDIA server driver version: ${NVIDIA_VER}"
apt-get install -y --no-install-recommends \
    "nvidia-dkms-${NVIDIA_VER}-server" \
    "nvidia-utils-${NVIDIA_VER}-server"

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

# ── Docker CE + nvidia-container-toolkit ──────────────────────────────────────
UBUNTU_CODENAME=$(. /etc/os-release && echo "${UBUNTU_CODENAME:-${VERSION_CODENAME}}")
apt-get install -y --no-install-recommends gnupg ca-certificates
mkdir -p /usr/share/keyrings /etc/apt/sources.list.d

curl -fsSL https://download.docker.com/linux/ubuntu/gpg \
    | gpg --dearmor -o /usr/share/keyrings/docker-archive-keyring.gpg
echo "deb [arch=amd64 signed-by=/usr/share/keyrings/docker-archive-keyring.gpg] https://download.docker.com/linux/ubuntu ${UBUNTU_CODENAME} stable" \
    > /etc/apt/sources.list.d/docker.list

curl -fsSL https://nvidia.github.io/libnvidia-container/gpgkey \
    | gpg --dearmor -o /usr/share/keyrings/nvidia-container-toolkit-keyring.gpg
curl -fsSL https://nvidia.github.io/libnvidia-container/stable/deb/nvidia-container-toolkit.list \
    | sed 's|deb https://|deb [signed-by=/usr/share/keyrings/nvidia-container-toolkit-keyring.gpg] https://|g' \
    > /etc/apt/sources.list.d/nvidia-container-toolkit.list

apt-get update -qq
apt-get install -y --no-install-recommends \
    docker-ce docker-ce-cli containerd.io docker-buildx-plugin docker-compose-plugin \
    nvidia-container-toolkit

# Wire nvidia runtime into Docker so --gpus all works on first boot.
nvidia-ctk runtime configure --runtime=docker
systemctl enable docker

# Add the cloud-init default user (UID 1000) to docker at first boot.
mkdir -p /etc/cloud/cloud.cfg.d
cat > /etc/cloud/cloud.cfg.d/99-gpu-groups.cfg <<'EOF'
runcmd:
  - usermod -aG docker $(awk -F: '$3==1000{print $1}' /etc/passwd) || true
EOF

mkdir -p /etc/apt/apt.conf.d
cat > /etc/apt/apt.conf.d/99-gpu-ami <<'EOF'
Unattended-Upgrade::Package-Blacklist {
    "linux-";
    "nvidia-";
    "libnvidia-";
    "docker-";
    "containerd";
    "nvidia-container-toolkit";
};
EOF

# Rebuild initramfs with nouveau blacklisted and NVIDIA module included.
update-initramfs -u -k "${KVER}"

echo "NVIDIA GPU image setup complete: kernel=${KVER}, NVIDIA driver + DKMS pre-built, Docker + nvidia-container-toolkit installed"
