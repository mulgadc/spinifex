#!/bin/sh
# lib_test.sh — tests for the appliance first-boot NIC and hostname helpers,
# run against a fixture sysfs tree (SYSFS) so the host's own NICs never count.
set -eu

DIR=$(CDPATH='' cd -- "$(dirname -- "$0")" && pwd)
TMP=$(mktemp -d)
trap 'rm -rf "$TMP"' EXIT

fails=0
pass() { echo "  ok   - $1"; }
fail() { echo "  FAIL - $1"; fails=$((fails + 1)); }

echo "-- appliance/lib.sh"

# nic <name> <pci-slot> <carrier> [type] — a physical NIC behind a PCI device.
nic() {
    mkdir -p "$SYS/devices/pci0000:00/$2" "$SYS/class/net/$1"
    ln -s "../../../devices/pci0000:00/$2" "$SYS/class/net/$1/device"
    echo "${4:-1}" > "$SYS/class/net/$1/type"
    echo "$3" > "$SYS/class/net/$1/carrier"
}
# virt <name> — a link with no backing device (bridge, veth, OVS port).
virt() {
    mkdir -p "$SYS/class/net/$1"
    echo 1 > "$SYS/class/net/$1/type"
    echo 1 > "$SYS/class/net/$1/carrier"
}
fixture() { SYS="$TMP/$1"; mkdir -p "$SYS/class/net"; }
run() { SYSFS="$SYS" bash -c ". '$DIR/lib.sh'; $1"; }

check() {
    if [ "$2" = "$3" ]; then pass "$1"; else fail "$1: want '$2', got '$3'"; fi
}

fixture order
nic enp3s0 0000:00:03.0 0
nic enp1s0 0000:00:01.0 0
virt br-wan
virt lo
check "physical NICs in PCI order, virtual links skipped" "enp1s0 enp3s0" "$(run appliance_physical_nics | tr '\n' ' ' | sed 's/ $//')"
check "no carrier anywhere picks the first by PCI order" enp1s0 "$(run appliance_pick_nic)"

fixture carrier
nic eno1 0000:00:01.0 0
nic eno2 0000:00:02.0 1
nic eno3 0000:00:03.0 1
check "the first NIC with carrier wins over PCI order" eno2 "$(run appliance_pick_nic)"

fixture wireless
nic wlp1s0 0000:00:01.0 1
mkdir -p "$SYS/class/net/wlp1s0/wireless"
nic ib0 0000:00:02.0 1 32
nic enp4s0 0000:00:04.0 0
check "wireless and non-Ethernet links are skipped" enp4s0 "$(run appliance_pick_nic)"

fixture none
virt br-wan
check "no physical NIC prints nothing" "" "$(run appliance_pick_nic)"

check "hostname from the MAC's last 6 hex digits" spinifex-c0ffee "$(run 'appliance_hostname 52:54:00:C0:FF:EE')"

[ "$fails" -eq 0 ] || { echo "  $fails failed"; exit 1; }
