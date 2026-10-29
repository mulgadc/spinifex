#!/bin/bash
# Helpers shared by the appliance first-boot script and its test. Sourced, not run.

# appliance_physical_nics — physical Ethernet devices, one per line, in PCI
# order. Skips anything without a backing device (bridges, veths, OVS ports,
# loopback), non-Ethernet link types and wireless. SYSFS overrides /sys.
appliance_physical_nics() {
    local sys="${SYSFS:-/sys}/class/net" dev
    for dev in "$sys"/*; do
        [ -e "$dev/device" ] || continue
        [ "$(cat "$dev/type" 2>/dev/null)" = 1 ] || continue
        [ -d "$dev/wireless" ] && continue
        printf '%s %s\n' "$(readlink -f "$dev/device")" "$(basename "$dev")"
    done | sort | awk '{print $2}'
}

# appliance_pick_nic — the first physical NIC with carrier, else the first
# physical NIC. Prints nothing when the host has none.
appliance_pick_nic() {
    local sys="${SYSFS:-/sys}/class/net" nic first=""
    for nic in $(appliance_physical_nics); do
        [ -n "$first" ] || first=$nic
        if [ "$(cat "$sys/$nic/carrier" 2>/dev/null)" = 1 ]; then
            echo "$nic"
            return 0
        fi
    done
    [ -n "$first" ] && echo "$first"
    return 0
}

# appliance_hostname <mac> — spinifex-<last 6 hex digits of the MAC>.
appliance_hostname() {
    local hex
    hex=$(echo "$1" | tr -d ':' | tr '[:upper:]' '[:lower:]')
    echo "spinifex-${hex: -6}"
}
