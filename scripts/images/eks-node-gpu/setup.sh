#!/bin/sh
set -eu

# setup.sh — guest customisation for the eks-node-gpu AMI (Ubuntu, K3s agent
# worker only — GPU nodes never run the EKS control plane, so there is no
# server/role-selector path here, unlike scripts/images/eks-node/setup.sh).
#
# Runs inside the libguestfs appliance (via virt-customize --run) under
# build-system-image.sh, after packages, binaries and INSTALL_FILES are
# placed. Downloads + verifies the pinned K3s binary (same recipe as
# scripts/images/eks-node/setup.sh), sets exec bits on the uploaded shell
# helpers, and bakes the headless NVIDIA driver + nvidia-container-toolkit
# (steps reused from scripts/images/ubuntu-gpu-nvidia-setup.sh — see
# install_nvidia_gpu_stack() below, duplicated verbatim in
# scripts/images/ecs-agent-gpu/setup.sh so both images stay consistent).
#
# Network access inside the appliance is provided by virt-customize --network.

K3S_VERSION="v1.32.5+k3s1"
K3S_URL_BASE="https://github.com/k3s-io/k3s/releases/download/${K3S_VERSION}"

echo "[eks-node-gpu-setup] fetching k3s checksums ${K3S_VERSION}"
curl -fsSL -o /tmp/k3s-checksums.txt "${K3S_URL_BASE}/sha256sum-amd64.txt"
K3S_SHA256=$(awk '/[ \t]k3s$/{print $1; exit}' /tmp/k3s-checksums.txt)
if [ -z "${K3S_SHA256}" ]; then
    echo "[eks-node-gpu-setup] could not parse k3s sha256 from upstream checksums"
    cat /tmp/k3s-checksums.txt
    exit 1
fi
echo "[eks-node-gpu-setup] downloading k3s ${K3S_VERSION} (sha256=${K3S_SHA256})"
curl -fsSL -o /usr/local/bin/k3s "${K3S_URL_BASE}/k3s"
echo "${K3S_SHA256}  /usr/local/bin/k3s" > /tmp/k3s.sha256
if ! sha256sum -c /tmp/k3s.sha256; then
    echo "[eks-node-gpu-setup] k3s SHA256 mismatch — refusing to bake AMI"
    exit 1
fi
chmod 0755 /usr/local/bin/k3s
ln -sf /usr/local/bin/k3s /usr/local/bin/kubectl
ln -sf /usr/local/bin/k3s /usr/local/bin/crictl
ln -sf /usr/local/bin/k3s /usr/local/bin/ctr

# Reused shell helpers + the new prestart wrapper land 0644 via INSTALL_FILES;
# make them executable. The systemd unit files and the udev rules file do not
# need an exec bit (unlike OpenRC's /etc/init.d/*).
chmod 0755 /usr/local/sbin/mulga-ebs-byid /usr/local/sbin/mulga-eks-provider-id \
    /usr/local/sbin/mulga-vpc-mtu /usr/local/sbin/k3s-agent-prestart

# K3s config dir (mulga-eks-provider-id's drop-in + cloud-init/first-boot
# agent config) and the shared spinifex-eks state dir (agent.env lands here
# from cloud-init at launch).
mkdir -p /etc/rancher/k3s/config.yaml.d /etc/spinifex-eks

# ── NVIDIA headless driver + container toolkit (node-image subset) ──────────
# Reused from scripts/images/ubuntu-gpu-nvidia-setup.sh: kernel detection,
# linux-headers-<kver>, the latest versioned nvidia-dkms-<ver>-server +
# nvidia-utils-<ver>-server, `dkms install`, nvidia.ko verification,
# `apt-mark hold` on the kernel/nvidia/libnvidia packages, the nouveau
# modprobe blacklist, and `update-initramfs -u -k <kver>`.
#
# Dropped for a node image: pciutils/python3/git/curl-extras/htop/tmux/ffmpeg/
# libgl1/libglib dev payload, and Docker CE entirely — GPU nodes run k3s
# (this image) or containerd (scripts/images/ecs-agent-gpu), never Docker.
# `nvidia-ctk runtime configure --runtime=docker` and the docker-group
# cloud-init drop-in are dropped for the same reason; the runtime is instead
# wired via CDI at first boot by mulga-cdi-generate.service, gated on an
# NVIDIA GPU actually being present.
install_nvidia_gpu_stack() {
    KVER=$(ls /boot/vmlinuz-* 2>/dev/null | sort -V | tail -1 | sed 's|/boot/vmlinuz-||') || true
    if [ -z "${KVER}" ]; then
        # Ubuntu 26.04 minimal cloud image keeps the kernel on the ESP (not
        # mounted in the appliance chroot), so /boot is empty — fall back to
        # /lib/modules.
        KVER=$(ls /lib/modules/ 2>/dev/null | sort -V | tail -1)
    fi
    if [ -z "${KVER}" ]; then
        echo "ERROR: No kernel found in /boot or /lib/modules — base image may be missing a kernel"
        exit 1
    fi
    echo "[nvidia-gpu-stack] target kernel: ${KVER}"

    # Prevent kernel postinst hooks from attempting grub-install against the
    # host disk — this fails silently inside the appliance and can abort the
    # postinst, leaving /boot incomplete.
    cat > /etc/kernel-img.conf <<'EOF'
do_symlinks = yes
do_bootloader = no
do_initrd = yes
link_in_boot = yes
EOF

    apt-get install -y --no-install-recommends "linux-headers-${KVER}"

    # Ubuntu 26.04+ no longer ships unversioned meta-packages, so the branch is
    # named explicitly. Pinned rather than resolved at build time: the number in
    # the package name is a branch alias, not a driver version (nvidia-dkms-535-server
    # installs 580.126.20), so picking the highest number sorts on a value that
    # need not match what gets installed. Resolving also let identical source
    # produce different drivers on different days. Bumping is a deliberate edit.
    NVIDIA_VER=595
    echo "[nvidia-gpu-stack] installing NVIDIA server driver branch: ${NVIDIA_VER}"
    # The -open (GSP/open-kernel-module) DKMS variant, not the closed/proprietary
    # one: Blackwell-generation GPUs (e.g. RTX Pro 6000 Blackwell) refuse to
    # initialize under the closed kernel module — RmInitAdapter fails with
    # "requires use of the NVIDIA open kernel modules" (dmesg NVRM error 0x22).
    # nvidia-utils has no separate open variant (userspace tools only).
    apt-get install -y --no-install-recommends \
        "nvidia-dkms-${NVIDIA_VER}-server-open" \
        "nvidia-utils-${NVIDIA_VER}-server"

    NVIDIA_DKMS_NAME=$(dkms status 2>/dev/null | awk -F'[,/ ]+' '/nvidia/{print $1; exit}')
    NVIDIA_DKMS_VER=$(dkms status 2>/dev/null  | awk -F'[,/ ]+' '/nvidia/{print $2; exit}')
    if [ -n "${NVIDIA_DKMS_NAME}" ] && [ -n "${NVIDIA_DKMS_VER}" ]; then
        echo "[nvidia-gpu-stack] building NVIDIA DKMS module ${NVIDIA_DKMS_NAME}/${NVIDIA_DKMS_VER} for kernel ${KVER}..."
        dkms install -m "${NVIDIA_DKMS_NAME}" -v "${NVIDIA_DKMS_VER}" --kernelver "${KVER}" --force
    else
        echo "WARNING: nvidia DKMS module not found in dkms status — driver install may have failed"
    fi

    NVIDIA_KO=$(find "/lib/modules/${KVER}/updates" -name "nvidia.ko*" 2>/dev/null | head -1)
    if [ -z "${NVIDIA_KO}" ]; then
        echo "ERROR: nvidia.ko not found under /lib/modules/${KVER}/updates after DKMS build"
        exit 1
    fi
    echo "[nvidia-gpu-stack] NVIDIA kernel module: ${NVIDIA_KO}"

    # Pin the exact kernel + headers + nvidia/libnvidia packages so
    # unattended-upgrades cannot replace what the DKMS module was built for.
    apt-mark hold "linux-image-${KVER}" "linux-headers-${KVER}"

    NVIDIA_PKGS=$(dpkg --get-selections 'nvidia-*' 2>/dev/null | awk '/install$/{print $1}' || true)
    LIBNVIDIA_PKGS=$(dpkg --get-selections 'libnvidia-*' 2>/dev/null | awk '/install$/{print $1}' || true)
    # shellcheck disable=SC2086
    [ -n "${NVIDIA_PKGS}" ]    && apt-mark hold ${NVIDIA_PKGS}
    # shellcheck disable=SC2086
    [ -n "${LIBNVIDIA_PKGS}" ] && apt-mark hold ${LIBNVIDIA_PKGS}

    # nvidia.ko and nouveau conflict; without this the NVIDIA driver will not
    # bind after boot.
    cat > /etc/modprobe.d/blacklist-nouveau.conf <<'EOF'
blacklist nouveau
options nouveau modeset=0
EOF

    # Load the NVIDIA modules at boot so /dev/nvidia* exist before CDI
    # generation and the container runtime start — a headless node never runs
    # an X server or CUDA app to trigger the on-demand load.
    cat > /etc/modules-load.d/nvidia.conf <<'EOF'
nvidia
nvidia_uvm
nvidia_modeset
EOF

    # nvidia-container-toolkit lives in its own NVIDIA-hosted apt repo, which
    # is not enabled on the base Ubuntu cloud image — add it here (same
    # gpg key + list as scripts/images/ubuntu-gpu-nvidia-setup.sh) and
    # install only the headless toolkit (no Docker CE).
    apt-get install -y --no-install-recommends gnupg
    mkdir -p /usr/share/keyrings /etc/apt/sources.list.d
    curl -fsSL https://nvidia.github.io/libnvidia-container/gpgkey \
        | gpg --dearmor -o /usr/share/keyrings/nvidia-container-toolkit-keyring.gpg
    curl -fsSL https://nvidia.github.io/libnvidia-container/stable/deb/nvidia-container-toolkit.list \
        | sed 's|deb https://|deb [signed-by=/usr/share/keyrings/nvidia-container-toolkit-keyring.gpg] https://|g' \
        > /etc/apt/sources.list.d/nvidia-container-toolkit.list
    apt-get update -qq
    apt-get install -y --no-install-recommends nvidia-container-toolkit
    apt-mark hold nvidia-container-toolkit nvidia-container-toolkit-base \
        libnvidia-container1 libnvidia-container-tools 2>/dev/null || true

    # Rebuild initramfs with nouveau blacklisted and the NVIDIA module included.
    update-initramfs -u -k "${KVER}"

    echo "[nvidia-gpu-stack] done: kernel=${KVER}, NVIDIA headless driver + nvidia-container-toolkit installed"
}

install_nvidia_gpu_stack

echo "[eks-node-gpu-setup] done"
