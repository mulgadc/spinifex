#!/bin/bash
# Spinifex appliance first boot, for Oracle Linux units cloned from the gold
# image that build-oracle-appliance.sh seals. The OL counterpart of the ISO's
# spinifex-firstboot.sh: it also does the per-unit work the ISO installer does
# before first boot (hostname, br-wan on the NIC that is present), then sets
# up a single node. On success it writes $DONE_MARKER and the unit's
# ConditionPathExists=! stops it running again. A failed run leaves no marker,
# so the next boot retries from the top; every step below is safe to repeat.
set -euo pipefail

DONE_MARKER=/var/lib/spinifex/.appliance-firstboot-done
SHARE=/usr/local/share/spinifex

# shellcheck source=lib.sh
. "$SHARE/appliance/lib.sh"
# shellcheck source=../lib/distro.sh
. "$SHARE/lib/distro.sh"

log() { echo "[appliance-firstboot] $*"; }

if [ -f "$DONE_MARKER" ]; then
    log "already complete, skipping"
    exit 0
fi

# ── Primary NIC ─────────────────────────────────────────────────────────────
# Links start down when NetworkManager has no profile for them, so bring every
# physical NIC up to read its carrier, and give a cable up to 60s to come up.
nics=$(appliance_physical_nics)
if [ -z "$nics" ]; then
    log "no physical Ethernet device found"
    exit 1
fi
for nic in $nics; do
    ip link set "$nic" up || true
done
for _i in $(seq 1 60); do
    for nic in $nics; do
        [ "$(cat "/sys/class/net/$nic/carrier" 2>/dev/null)" = 1 ] && break 2
    done
    sleep 1
done
NIC=$(appliance_pick_nic)
MAC=$(cat "/sys/class/net/$NIC/address")
log "primary NIC $NIC ($MAC), carrier $(cat "/sys/class/net/$NIC/carrier" 2>/dev/null || echo 0)"

HOSTNAME_NEW=$(appliance_hostname "$MAC")
hostnamectl set-hostname "$HOSTNAME_NEW"
log "hostname $HOSTNAME_NEW"

# ── br-wan on the primary NIC, DHCP ─────────────────────────────────────────
# The bridge takes the NIC's MAC so a DHCP reservation made for the unit's
# port keeps working. bridge.stp no: with STP on, the port sits in its
# forwarding delay long enough for DHCP on the bridge to time out.
for con in $(nmcli -g UUID,DEVICE connection show | awk -F: -v d="$NIC" '$2 == d {print $1}'); do
    nmcli connection delete "$con" || true
done
nmcli connection delete br-wan br-wan-port >/dev/null 2>&1 || true
nmcli connection add type bridge ifname br-wan con-name br-wan \
    bridge.stp no bridge.mac-address "$MAC" \
    ipv4.method auto ipv6.method disabled connection.autoconnect yes
nmcli connection add type bridge-slave ifname "$NIC" master br-wan con-name br-wan-port \
    connection.autoconnect yes
nmcli connection up br-wan || true

log "waiting for a DHCP address on br-wan"
WAN_IP=""
for _i in $(seq 1 120); do
    WAN_IP=$(ip -4 -o addr show br-wan scope global 2>/dev/null | awk '{print $4}' | cut -d/ -f1 | head -1)
    if [ -n "$WAN_IP" ] && ip -4 route show default dev br-wan | grep -q .; then
        break
    fi
    WAN_IP=""
    sleep 1
done
if [ -z "$WAN_IP" ]; then
    log "br-wan got no DHCP address and default route in 120s; check the cable on $NIC and the site's DHCP"
    exit 1
fi
log "br-wan $WAN_IP"

# ── Clock ───────────────────────────────────────────────────────────────────
# Credentials and TLS certificates are minted below, and a certificate issued
# against a wrong RTC carries a NotBefore that outlives the correction.
# Bounded and non-fatal, as on the ISO.
chronyc -a makestep >/dev/null 2>&1 || true
if chronyc waitsync 12 0.1 0 5 >/dev/null 2>&1; then
    log "clock synced: $(date -uIs)"
else
    log "WARNING: clock not synced, continuing with $(date -uIs)"
fi

# ── Connection tracking ─────────────────────────────────────────────────────
# Before OVN opens any link, so arming the firewall later cannot drop one the
# kernel first saw mid-stream. Non-fatal; the unit reloads it on later boots.
/usr/local/lib/spinifex/spinifex-firewall-apply track \
    || log "WARNING: connection tracking not loaded; arming the firewall may drop OVN links"

# ── OVS and OVN ─────────────────────────────────────────────────────────────
# Start them before setup-ovn.sh so their databases exist when it runs: on
# physical hardware first-boot DB initialisation outlasts its own timeout.
OVS_UNIT=$(distro_ovs_unit)
OVN_LAYOUT=$(distro_ovn_central_layout)
case $OVN_LAYOUT in
    split) OVN_CENTRAL_UNIT=ovn-central.service ;;
    lone-northd) OVN_CENTRAL_UNIT=ovn-northd.service ;;
esac
systemctl start "$OVS_UNIT" "$OVN_CENTRAL_UNIT"
for _i in $(seq 1 120); do
    if ovn-nbctl --timeout=2 get-connection >/dev/null 2>&1; then
        log "OVN NB DB ready (${_i}s)"
        break
    fi
    sleep 1
done

"$SHARE/setup-ovn.sh" --management

# ── Single-node cluster ─────────────────────────────────────────────────────
# Same arguments as the ISO's single-NIC DHCP path. spx writes credentials to
# disk and never prints them.
spx admin init --node "$HOSTNAME_NEW" --nodes 1 2>&1

# spx ran as root, so the files it wrote are root:root. setup.sh's fixown
# stage knows each file's owner and mode.
SETUP_STAGES=fixown "$SHARE/setup.sh"

# ── spinifex login ──────────────────────────────────────────────────────────
if [ ! -f /home/spinifex/.ssh/id_ed25519 ]; then
    install -d -m 700 -o spinifex -g spinifex /home/spinifex/.ssh
    ssh-keygen -q -t ed25519 -f /home/spinifex/.ssh/id_ed25519 -N "" -C "spinifex@$HOSTNAME_NEW"
    chown spinifex:spinifex /home/spinifex/.ssh/id_ed25519 /home/spinifex/.ssh/id_ed25519.pub
fi
# init wrote the AWS profile under HOME=/root; the operator logs in as spinifex.
if [ -f /root/.aws/credentials ]; then
    install -d -m 700 -o spinifex -g spinifex /home/spinifex/.aws
    install -m 600 -o spinifex -g spinifex /root/.aws/credentials /home/spinifex/.aws/credentials
    [ -f /root/.aws/config ] && install -m 600 -o spinifex -g spinifex /root/.aws/config /home/spinifex/.aws/config
fi

# ── Services ────────────────────────────────────────────────────────────────
# Enabled here, not in the image, so a clone runs nothing of Spinifex before
# init has written its config.
systemctl enable spinifex.target spinifex-banner.service spinifex-firewall.service
systemctl enable --now spinifex-nats-watchdog.timer

mkdir -p "$(dirname "$DONE_MARKER")"
touch "$DONE_MARKER"

systemctl start spinifex.target
/usr/local/bin/spx admin banner --boot-check
log "complete: https://$WAN_IP:3000, login spinifex"
