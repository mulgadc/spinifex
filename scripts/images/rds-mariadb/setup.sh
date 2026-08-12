#!/bin/sh
set -eu

# setup.sh — guest customisation for the spinifex-rds-mariadb AMI. Runs inside
# the libguestfs appliance under build-system-image.sh, after packages and
# INSTALL_FILES are placed.

# INSTALL_FILES land 0644; OpenRC requires 0755 on init scripts, and rds-init
# and rds-datadir are executed directly by their services.
chmod 0755 /etc/init.d/rds-datadir /etc/init.d/rds-init /etc/init.d/rds-agent \
    /usr/local/sbin/rds-datadir /usr/local/sbin/rds-init
chmod 0755 /etc/init.d/mulga-mgmt-net /etc/init.d/mulga-mgmt-net-routes \
    /usr/local/sbin/mulga-mgmt-net

# mulga-mgmt-net goes in the boot runlevel, not default (where ENABLE_SERVICES
# lands services). It applies mgmt0's static address, which rds-agent needs to
# reach the gateway at all, and DHCPs the data NIC so the init-local Ec2 crawl
# reaches IMDS; a default entry runs after cloud-init-local and is too late.
rc-update add mulga-mgmt-net boot

# Where cloud-init drops the agent's env file and the gateway CA. Created here
# so the delivery lands in a root-only directory.
install -d -m 0700 /etc/spinifex-rds

# The engine this image bakes. rds-agent builds its engine implementation from
# this file, so running the wrong implementation is structurally impossible
# rather than merely validated against; a VM launched as another engine is
# refused before anything touches the datadir.
printf 'mariadb\n' > /etc/spinifex-rds/engine
chmod 0444 /etc/spinifex-rds/engine

# Empty in the image: rds-datadir mounts over it at boot. The package already
# creates it 0750 mysql:mysql; restated so a packaging change cannot loosen it.
install -d -m 0750 -o mysql -g mysql /var/lib/mysql

# The include target, baked so it exists even when the data volume is not
# mounted. MariaDB treats an !includedir it cannot open as a fatal error while
# parsing defaults, which would take down the client alongside the server on
# exactly the boot an operator needs both to diagnose the missing volume.
install -d -m 0750 -o mysql -g mysql /var/lib/mysql/conf.d

# The generated configuration must be read last, or a packaged default would win
# over a platform-owned setting. Sorted in byte order, the way MariaDB itself
# walks an include directory, so a future package adding a later-sorting file
# fails the build rather than silently overriding the platform.
last_dropin=$(ls /etc/my.cnf.d | LC_ALL=C sort | tail -n 1)
if [ "${last_dropin}" != "zz-rds-include.cnf" ]; then
    echo "[rds-mariadb-setup] /etc/my.cnf.d/${last_dropin} is read after the RDS include"
    exit 1
fi

# Alpine's packaged server drop-in sets skip-networking, which would leave the
# instance reachable only over its unix socket — the endpoint would resolve, the
# health probe would pass, and nothing on the customer ENI could ever connect.
# Disabled at the source rather than countered from a later file, so the image
# does not depend on override ordering for the endpoint to work at all.
sed -i 's/^skip-networking$/# skip-networking — disabled: the customer reaches this instance over TCP/' \
    /etc/my.cnf.d/mariadb-server.cnf
if grep -rEq '^[[:space:]]*skip[-_]networking' /etc/my.cnf /etc/my.cnf.d; then
    echo "[rds-mariadb-setup] a packaged configuration file still disables TCP networking"
    grep -rEn '^[[:space:]]*skip[-_]networking' /etc/my.cnf /etc/my.cnf.d
    exit 1
fi

# The resolved defaults checked against this image's own mariadbd. A name the
# server refuses at startup is not a rejected setting: it aborts the boot with the
# generated file already on the data volume. cmd/rds-agent keeps this file in step.
#
# --verbose --help parses the option files and exits without touching a datadir,
# so it is the whole check. defaults-extra-file rather than the include directory,
# which stays empty in the image; it must be the first option mariadbd sees.
default_params=/usr/local/share/spinifex-rds/default-parameters.cnf
if [ ! -f "${default_params}" ]; then
    echo "[rds-mariadb-setup] the catalog's default parameter set was not delivered to ${default_params}"
    exit 1
fi
if ! mariadbd --defaults-extra-file="${default_params}" --user=mysql --verbose --help \
    >/dev/null 2>/tmp/rds-defaults.err; then
    echo "[rds-mariadb-setup] mariadbd refuses the catalog's default parameter set"
    cat /tmp/rds-defaults.err
    exit 1
fi
# Build-time only: nothing at runtime reads it, and an include the server could
# find twice is a way for a stale copy to shadow the customer's own set.
rm -f /tmp/rds-defaults.err "${default_params}"
rmdir /usr/local/share/spinifex-rds 2>/dev/null || true

# The packaged service starts on a warning when the datadir is empty, so the
# dependency is what keeps it off an unbootstrapped volume. Asserted here, since
# an image whose engine can come up beside a failed bootstrap is the one failure
# this preset must not have.
grep -q '^rc_need="rds-init"' /etc/conf.d/mariadb || {
    echo "[rds-mariadb-setup] /etc/conf.d/mariadb does not make the engine need rds-init"
    exit 1
}

# rds-agent's health probe reads this pid to tell a server replaying its redo log
# from one that is not running at all. Nothing generated can move the path: the
# packaged service passes --pid-file on mysqld_safe's command line, which beats
# any option file. A package that renamed it would leave every instance reporting
# an engine that is not there, and the rollback guard restarting a healthy one.
grep -q '^pidfile="/run/mysqld/\$RC_SVCNAME.pid"$' /etc/init.d/mariadb &&
    grep -q -- '--pid-file=\$pidfile' /etc/init.d/mariadb || {
    echo "[rds-mariadb-setup] /etc/init.d/mariadb no longer starts the engine with its pidfile at /run/mysqld/mariadb.pid"
    grep -nE 'pidfile|command_args' /etc/init.d/mariadb
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

echo "[rds-mariadb-setup] done"
