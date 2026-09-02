#!/bin/bash
# Prepare a runner to build a guest image with build-system-image.sh.
#
# Installs libguestfs + qemu-utils if they are missing and makes a host kernel
# readable, so the libguestfs appliance can boot. Nothing here touches a host
# block device: virt-customize works inside an isolated userspace appliance, so
# this is safe on a shared runner.
#
# Callers must still export LIBGUESTFS_BACKEND=direct themselves — it has to be
# set in the environment the build runs in, which is not this subshell.
set -euo pipefail

# Three nightly service cells share one ci-single runner and start together, so
# concurrent apt-get runs are the normal case rather than the exception. Two of
# them lost the lock outright before this guard existed. Skipping the install
# when the tools are already present avoids most of the contention; the lock
# timeout covers the rest, including apt runs started by anything else on the
# box.
if command -v virt-customize >/dev/null && command -v qemu-img >/dev/null; then
    echo "libguestfs-tools and qemu-utils already present"
else
    APT_OPTS=(-o DPkg::Lock::Timeout=600)
    sudo apt-get "${APT_OPTS[@]}" update -qq
    sudo apt-get "${APT_OPTS[@]}" install -y --no-install-recommends \
        libguestfs-tools qemu-utils
fi

# Debian/Ubuntu ship /boot/vmlinuz-* mode 0600 and the appliance needs to read a
# host kernel. Tolerated on failure: a runner whose kernel is already readable
# has no vmlinuz to chmod under some images.
sudo chmod 0644 /boot/vmlinuz-* || true
