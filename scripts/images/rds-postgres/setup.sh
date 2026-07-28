#!/bin/sh
set -eu

# setup.sh — guest customisation for the spinifex-rds-postgres AMI.
#
# Runs inside the libguestfs appliance (via virt-customize --run) under
# build-system-image.sh, after packages and INSTALL_FILES are placed. Sets exec
# bits on the init scripts and the bootstrap oneshot, creates the agent config
# directory, the datadir mount point and the log directory, and applies the
# standard Alpine-cloud serial-console + fast boot-menu tweaks so
# orchestrator-captured ttyS0 logs work.

# INSTALL_FILES land 0644; OpenRC requires 0755 on init scripts, and rds-init is
# executed directly by its service.
chmod 0755 /etc/init.d/rds-init /etc/init.d/rds-agent /usr/local/sbin/rds-init

# Where cloud-init drops the agent's env file and the per-instance gateway CA.
# Created here so the delivery lands in a root-only directory rather than
# whatever a first write would create it as.
install -d -m 0700 /etc/spinifex-rds

# Mount point for the instance's Viperblock data volume. It stays empty in the
# image: the datadir is created one level inside it by rds-init, once the volume
# is attached. Owned by postgres so the engine can traverse it if the volume's
# own root is stricter than the default.
install -d -m 0750 -o postgres -g postgres /var/lib/postgresql

# Postmaster log plus the bootstrap server's log, on the boot volume — they are
# per-boot diagnostics, not state, and keeping them off the data volume keeps
# the snapshot content to the customer's data.
install -d -m 0755 -o postgres -g postgres /var/log/postgresql

# The stock conf.d shipped by postgresql-common-openrc is replaced wholesale by
# INSTALL_FILES; assert the auto-setup-on-empty-datadir behaviour is actually
# off, since baking an image that can silently initdb over a missing data volume
# is the one failure this image must not have.
grep -q '^auto_setup="no"' /etc/conf.d/postgresql || {
    echo "[rds-postgres-setup] /etc/conf.d/postgresql does not disable auto_setup"
    exit 1
}

# Bind /dev/console to the serial port so userspace boot output (OpenRC service
# starts, cloud-init, the rds-init bootstrap) reaches ttyS0, which the
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

echo "[rds-postgres-setup] done"
