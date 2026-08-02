#!/bin/sh
set -eu

# setup.sh — guest customisation for the spinifex-rds-postgres AMI. Runs inside
# the libguestfs appliance under build-system-image.sh, after packages and
# INSTALL_FILES are placed.

# INSTALL_FILES land 0644; OpenRC requires 0755 on init scripts, and rds-init
# and rds-datadir are executed directly by their services.
chmod 0755 /etc/init.d/rds-datadir /etc/init.d/rds-init /etc/init.d/rds-agent \
    /usr/local/sbin/rds-datadir /usr/local/sbin/rds-init
chmod 0755 /etc/init.d/mulga-mgmt-net /usr/local/sbin/mulga-mgmt-net

# mulga-mgmt-net goes in the boot runlevel, not default (where ENABLE_SERVICES
# lands services). It applies mgmt0's static address, which rds-agent needs to
# reach the gateway at all, and DHCPs the data NIC so the init-local Ec2 crawl
# reaches IMDS; a default entry runs after cloud-init-local and is too late.
rc-update add mulga-mgmt-net boot

# Where cloud-init drops the agent's env file and the gateway CA. Created here
# so the delivery lands in a root-only directory.
install -d -m 0700 /etc/spinifex-rds

# Empty in the image: rds-datadir mounts over it at boot. Postgres-owned so the
# engine can traverse it if the volume's own root is stricter.
install -d -m 0750 -o postgres -g postgres /var/lib/postgresql

# Kept on the boot volume: per-boot diagnostics, so a snapshot stays data-only.
install -d -m 0755 -o postgres -g postgres /var/log/postgresql

# INSTALL_FILES replaces the stock conf.d wholesale; assert auto-setup is really
# off, since an image that can silently initdb over a missing data volume is the
# one failure this image must not have.
grep -q '^auto_setup="no"' /etc/conf.d/postgresql || {
    echo "[rds-postgres-setup] /etc/conf.d/postgresql does not disable auto_setup"
    exit 1
}

# Bind /dev/console to the serial port so userspace boot output reaches ttyS0,
# which the orchestrator captures host-side. Linux makes the last console= the
# controlling one, and stock Alpine lists tty0 last — reorder so ttyS0 wins.
sed -i \
    's|console=ttyS0,115200n8 console=ttyAMA0,115200n8 console=tty0|console=tty0 console=ttyAMA0,115200n8 console=ttyS0,115200n8|' \
    /etc/update-extlinux.conf /boot/extlinux.conf

# Cut the boot-menu countdown from 10s to ~1s, a fixed tax on every VM start.
# Both the generator config (seconds) and the rendered output (1/10s) are patched
# so a regenerate keeps it; a small nonzero keeps the menu interruptible.
sed -i 's/^timeout=.*/timeout=1/' /etc/update-extlinux.conf
sed -i 's/^TIMEOUT[[:space:]].*/TIMEOUT 10/' /boot/extlinux.conf

echo "[rds-postgres-setup] done"
