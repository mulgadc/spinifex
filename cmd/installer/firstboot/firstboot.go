/*
Copyright © 2026 Mulga Defense Corporation

This program is free software: you can redistribute it and/or modify
it under the terms of the GNU Affero General Public License as published by
the Free Software Foundation, either version 3 of the License, or
(at your option) any later version.

This program is distributed in the hope that it will be useful,
but WITHOUT ANY WARRANTY; without even the implied warranty of
MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE.  See the
GNU Affero General Public License for more details.

You should have received a copy of the GNU Affero General Public License
along with this program.  If not, see <https://www.gnu.org/licenses/>.
*/

// Package firstboot writes the oneshot systemd service and configuration that
// completes Spinifex provisioning on the first real boot after installation.
package firstboot

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/mulgadc/spinifex/cmd/installer/systemd"
)

// Config holds the values the firstboot service needs to configure the node.
type Config struct {
	Hostname string
	// EncapIP is the Geneve tunnel IP for OVN. Set to the LAN bridge IP when a
	// dedicated LAN NIC is present, otherwise the WAN bridge IP. Empty when DHCP
	// is used — setup-ovn.sh auto-detects the IP from the default route in that case.
	EncapIP string
	// ClusterRole is "init" or "join".
	ClusterRole string
	// JoinAddr is host:port of the primary node, only used when ClusterRole is "join".
	JoinAddr string
	// Email is the operator email collected by the TUI or SPINIFEX_EMAIL on
	// the headless path. Passed to `spx admin init --email=<value>` when set;
	// omitted entirely when empty.
	Email string
}

// Write drops the firstboot script and systemd unit into root, which should be
// the path of the installed system's root filesystem (e.g. /mnt/spinifex-install).
func Write(root string, cfg Config) error {
	if err := writeScript(root, cfg); err != nil {
		return fmt.Errorf("firstboot script: %w", err)
	}
	if err := systemd.WriteFirstbootUnit(root); err != nil {
		return fmt.Errorf("firstboot unit: %w", err)
	}
	if err := systemd.EnableUnit(root, "spinifex-firstboot.service"); err != nil {
		return err
	}
	// Banner unit is written but NOT enabled here. firstboot enables it
	// (along with spinifex.target) at the end of its run, so on the first
	// boot the only spinifex unit running is firstboot itself — no implicit
	// pull of spinifex.target via the banner's Wants= relationship, which
	// was the root cause of the boot-time race against firstboot.
	if err := systemd.WriteBannerUnit(root); err != nil {
		return fmt.Errorf("banner unit: %w", err)
	}
	return systemd.WriteGettyDropIn(root)
}

func writeScript(root string, cfg Config) error {
	clusterCmd := buildClusterCmd(cfg)

	// --encap-ip is optional: when DHCP is used the IP is unknown at install time
	// and setup-ovn.sh auto-detects it from the default route at boot.
	setupOVN := "/usr/local/bin/setup-ovn.sh --management"
	if cfg.EncapIP != "" {
		setupOVN += fmt.Sprintf(" --encap-ip=%s", cfg.EncapIP)
	}

	script := fmt.Sprintf(`#!/bin/bash
# Spinifex firstboot — runs once after ISO installation. On success, writes
# /var/lib/spinifex/.firstboot-done and the systemd unit's
# ConditionPathExists=! prevents re-execution on subsequent boots. A partial
# run leaves no marker, so the next reboot retries from the top — safe
# because every step below is idempotent (hostnamectl, setup-ovn.sh, spx
# admin init are all "set if not set"; systemctl enable on an already-enabled
# unit is a no-op).
set -euo pipefail

DONE_MARKER=/var/lib/spinifex/.firstboot-done

# Idempotency: bail early if a previous run completed successfully. The unit
# also has ConditionPathExists=!$DONE_MARKER so we shouldn't get here on
# subsequent boots, but defend in depth in case the unit is re-triggered
# manually before the operator notices the marker exists.
if [ -f "$DONE_MARKER" ]; then
    echo "[firstboot] already complete — skipping"
    exit 0
fi

# Set hostname
hostnamectl set-hostname %s

# Pre-start OVS and OVN central so their databases are initialised before
# setup-ovn.sh runs. On physical hardware, first-boot DB initialisation takes
# longer than setup-ovn.sh's internal 15-second timeout allows. Starting them
# here and waiting until the NB DB is ready means setup-ovn.sh sees a live DB
# the moment it starts — no races, no timeout failures.
systemctl start openvswitch-switch
systemctl start ovn-central
echo "Waiting for OVN NB DB to initialise..."
for _i in $(seq 1 120); do
    if ovn-nbctl --timeout=2 get-connection >/dev/null 2>&1; then
        echo "OVN NB DB ready (${_i}s)"
        break
    fi
    sleep 1
done

# Create default nameservers if DHCP server returns blank
printf "nameserver 1.1.1.1\nnameserver 8.8.8.8\n" > /etc/resolvconf/resolv.conf.d/base
resolvconf -u

# Configure OVN networking.
# br-wan (and br-lan if present) are Linux bridges created by the installer.
# setup-ovn.sh auto-detects br-wan as the default route device (Linux bridge)
# and wires it to OVS via a veth pair — non-destructive, SSH-safe.
%s

# Write the banner (first time, spinifex-banner service will do this on reboot)
/usr/local/bin/spx admin banner --boot-check

# Cluster formation — capture credentials to file for display on console.
%s 2>&1

# Fix ownership of files spx admin init wrote. spx runs as root under
# systemd, so /etc/spinifex/{spinifex.toml,master.key,ca.key,*.pem} and any
# per-service files written under /var/lib/spinifex/* land as root:root.
# Delegate to setup.sh's fix_file_ownership (single source of truth) — it
# knows the per-file modes (ca.key 0600, *.pem 0644, toml/key 0640) that a
# blunt recursive chmod would clobber.
SETUP_STAGES=fixown /usr/local/share/spinifex/setup.sh
# The /var/lib/spinifex/awsgw/config symlink was created at build time by
# setup.sh's create_directories — it resolves to /etc/spinifex, so
# {BaseDir}/config/master.key automatically points at /etc/spinifex/master.key.

# Copy AWS credentials to the spinifex user's home directory.
# spx admin init runs with HOME=/root (set by the systemd unit), so credentials
# land in /root/.aws/. Copy them to the spinifex user's home so the operator
# can use the AWS CLI without sudo.
if [ -f /root/.aws/credentials ]; then
    mkdir -p /home/spinifex/.aws
    cp /root/.aws/credentials /home/spinifex/.aws/credentials
    cp /root/.aws/config /home/spinifex/.aws/config 2>/dev/null || true
    chown -R spinifex:spinifex /home/spinifex/.aws
    chmod 700 /home/spinifex/.aws
    chmod 600 /home/spinifex/.aws/credentials
    [ -f /home/spinifex/.aws/config ] && chmod 600 /home/spinifex/.aws/config
fi

# Enable + activate spinifex.target and the banner now that all configs are
# in place. "enable --now" creates the multi-user.target.wants/ symlinks (so
# they start directly on every subsequent boot — no longer dependent on
# firstboot running each time, since firstboot is condition-skipped after
# this run via the marker) and activates them in this boot.
# NOTE: Moved to firstinstall to enable
#systemctl enable --now spinifex.target spinifex-banner.service

# Enable services to start, on reboot
systemctl enable spinifex.target spinifex-banner.service

# Mark complete only after every step above has succeeded. Until this point,
# any failure (set -e) leaves the marker absent and the next reboot retries
# firstboot from the top.
mkdir -p "$(dirname "$DONE_MARKER")"
touch "$DONE_MARKER"

systemctl start spinifex.target

# Wait for the daemon to bring up external networking. When external_mode=pool
# is configured the daemon creates br-ext (the OVN external bridge) during
# startup; launching instances before it is ready results in no public IP being
# assigned. ip link requires no root and avoids parsing daemon logs.
if grep -q 'external_mode.*pool' /etc/spinifex/spinifex.toml 2>/dev/null; then
    echo "[firstboot] waiting for external networking (br-ext)..."
    for _i in $(seq 1 30); do
        if ip link show br-ext >/dev/null 2>&1; then
            echo "[firstboot] br-ext ready (${_i}s)"
            break
        fi
        sleep 1
    done
    if ! ip link show br-ext >/dev/null 2>&1; then
        echo "[firstboot] warning: br-ext not up after 30s — external networking may be delayed"
    fi
fi
`, cfg.Hostname, setupOVN, clusterCmd)

	path := filepath.Join(root, "usr/local/bin/spinifex-firstboot.sh")
	return os.WriteFile(path, []byte(script), 0o755)
}

func buildClusterCmd(cfg Config) string {
	emailFlag := ""
	if cfg.Email != "" {
		// shellEscapeSingle keeps the email safe if it ever contains a
		// character shell treats specially — belt-and-braces; the regex
		// validator already rejects whitespace and @-chains.
		emailFlag = " --email=" + shellEscapeSingle(cfg.Email)
	}
	switch cfg.ClusterRole {
	case "join":
		return fmt.Sprintf("spx admin join --node %s --host %s%s", cfg.Hostname, cfg.JoinAddr, emailFlag)
	default:
		return fmt.Sprintf("spx admin init --node %s --nodes 1%s", cfg.Hostname, emailFlag)
	}
}

// shellEscapeSingle wraps s in single quotes with any embedded single
// quotes escaped. Minimal — we only need this because the email value is
// interpolated into a shell script written by Write().
func shellEscapeSingle(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}
