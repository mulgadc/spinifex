#!/bin/sh
set -eu

# setup.sh — guest customisation for the spinifex-ecs-node AMI.
#
# Runs inside the libguestfs appliance (via virt-customize --run) under
# build-system-image.sh, after packages, binaries, and INSTALL_FILES are placed.
# Sets exec bits on the OpenRC services, wires the CNI bin path containerd's CRI
# plugin expects, and applies the standard Alpine-cloud serial-console + fast
# boot-menu tweaks so orchestrator-captured ttyS0 logs work.

# INSTALL_FILES land 0644; OpenRC requires 0755 on init scripts.
chmod 0755 /etc/init.d/ecs-agent /etc/init.d/ecs-ca-install

# cni-plugins (alpine) installs to /usr/libexec/cni; containerd's CRI plugin
# defaults its bin dir to /opt/cni/bin. Symlink so the baked bridge conflist
# resolves its plugins when Sprint 4d wires task networking.
mkdir -p /opt/cni
ln -sf /usr/libexec/cni /opt/cni/bin

# Content-store + agent config dirs.
mkdir -p /var/lib/containerd /etc/spinifex-ecs

# Bind /dev/console to the serial port so userspace boot output (OpenRC service
# starts, cloud-init, ecs-agent registration) reaches ttyS0, which the
# orchestrator captures host-side. Stock Alpine lists console=tty0 last and Linux
# makes the last console= the controlling /dev/console — reorder so ttyS0 wins.
sed -i \
    's|console=ttyS0,115200n8 console=ttyAMA0,115200n8 console=tty0|console=tty0 console=ttyAMA0,115200n8 console=ttyS0,115200n8|' \
    /etc/update-extlinux.conf /boot/extlinux.conf

# Cut the boot-menu countdown from 10s to ~1s (a fixed tax on every VM start).
# Patch the generator config (seconds) and the rendered output (1/10s) so a
# regenerate keeps the short value; a small nonzero keeps the menu interruptible.
sed -i 's/^timeout=.*/timeout=1/' /etc/update-extlinux.conf
sed -i 's/^TIMEOUT[[:space:]].*/TIMEOUT 10/' /boot/extlinux.conf

echo "[ecs-node-setup] done"
