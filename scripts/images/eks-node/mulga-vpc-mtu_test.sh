#!/bin/sh
# Self-contained POSIX test for mulga-vpc-mtu.sh. No root or real interfaces
# needed: `ip` and `networkctl` are stubbed on PATH. The `ip` stub is stateful
# — "link set ... mtu" and "route replace ..." write back into a fake sysfs
# tree and the route fixture, the way a real kernel would — so the assertion
# pass added below sees the effect of the pin/restamp it is checking, not a
# frozen fixture.
#
# Covers two faults this script exists to prevent:
#  - dhcpcd stamps the DHCP MTU (option 26) onto its routes, and Linux prefers
#    a route's MTU metric over the device MTU, so pinning only the link leaves
#    the node advertising an MSS too large for the path.
#  - netplan renders UseMTU=true for systemd-networkd, so an unpinned lease
#    renewal re-inflates the link 30+ minutes after boot, long after the
#    boot-time pin has already run.
#
# Run: sh scripts/images/eks-node/mulga-vpc-mtu_test.sh
set -eu

SCRIPT_DIR=$(cd "$(dirname "$0")" && pwd)
PINNER="${SCRIPT_DIR}/mulga-vpc-mtu.sh"
WORK=$(mktemp -d)
trap 'rm -rf "${WORK}"' EXIT

STUBBIN="${WORK}/bin"
mkdir -p "${STUBBIN}"

# Stub `ip`: serves ${ROUTES_DEFAULT} / ${ROUTES_DEV} for show, records
# mutations ("link set …" / "route replace …") to ${CALLS}, and reflects them
# back into ${MULGA_NET_SYSFS}/<iface>/mtu and ${ROUTES_DEV} respectively so a
# later read in the same run observes the mutation. LINKSET_FAIL / ROUTE_FAIL
# make the corresponding mutation fail without touching state, to simulate a
# pin or restamp that did not take.
cat > "${STUBBIN}/ip" <<'STUB'
#!/bin/sh
args="$*"
case "$args" in
    *"route show default"*) cat "${ROUTES_DEFAULT}"; exit 0 ;;
    *"route show dev "*)    cat "${ROUTES_DEV}"; exit 0 ;;
    "link show dev "*)      exit 0 ;;
    "link set dev "*)
        echo "$args" >> "${CALLS}"
        [ "${LINKSET_FAIL:-0}" = "1" ] && exit 1
        iface=$(echo "$args" | awk '{print $4}')
        mtu=$(echo "$args" | awk '{print $6}')
        if [ -n "${MULGA_NET_SYSFS:-}" ] && [ -n "$iface" ]; then
            mkdir -p "${MULGA_NET_SYSFS}/${iface}"
            printf '%s\n' "$mtu" > "${MULGA_NET_SYSFS}/${iface}/mtu"
        fi
        exit 0
        ;;
    "route replace "*)
        echo "$args" >> "${CALLS}"
        [ "${ROUTE_FAIL:-0}" = "1" ] && exit 1
        # `route show dev` omits the trailing "dev X" token, so strip it back
        # off before writing the corrected line back to the route fixture.
        newline=$(echo "$args" | sed -e 's/^route replace //' -e 's/ dev [^ ]*$//')
        printf '%s\n' "$newline" > "${ROUTES_DEV}"
        exit 0
        ;;
esac
exit 0
STUB
chmod +x "${STUBBIN}/ip"

# Stub `networkctl`: records every call, and fails if NETWORKCTL_FAIL=1.
cat > "${STUBBIN}/networkctl" <<'STUB'
#!/bin/sh
echo "networkctl $*" >> "${CALLS}"
[ "${NETWORKCTL_FAIL:-0}" = "1" ] && exit 1
exit 0
STUB
chmod +x "${STUBBIN}/networkctl"

PASS=0
FAIL=0
check() { # check <desc> <expected> <actual>
    if [ "$2" = "$3" ]; then
        PASS=$((PASS + 1))
    else
        FAIL=$((FAIL + 1))
        echo "FAIL: $1"
        echo "  expected: [$2]"
        echo "  actual:   [$3]"
    fi
}

# new_case <name>: sets up a fresh, isolated fixture directory — including a
# sysfs stand-in and netplan-unit directory that do not exist by default, so
# assert_pinned and usemtu_off are no-ops unless a test populates them. This
# also keeps every run off the real /sys/class/net and /run/systemd/network on
# the box running the test.
new_case() {
    CASE="${WORK}/$1"
    mkdir -p "${CASE}"
    CALLS="${CASE}/calls"
    ROUTES_DEFAULT="${CASE}/default"
    ROUTES_DEV="${CASE}/dev"
    SYSFS="${CASE}/sysfs"
    NETPLAN="${CASE}/netplan"
    STDERR="${CASE}/stderr"
    : > "${CALLS}"
    export CALLS ROUTES_DEFAULT ROUTES_DEV
}

# run_case <name> <default-routes> <dev-routes>: runs the pinner against
# fixture route tables and leaves the recorded mutations in ${CALLS}.
run_case() {
    new_case "$1"
    printf '%s\n' "$2" > "${ROUTES_DEFAULT}"
    printf '%s\n' "$3" > "${ROUTES_DEV}"
    PATH="${STUBBIN}:${PATH}" MULGA_NET_SYSFS="${SYSFS}" MULGA_NETPLAN_DIR="${NETPLAN}" \
        sh "${PINNER}" >/dev/null 2>"${STDERR}"
}

replaces() { grep -c '^route replace' "${CALLS}" 2>/dev/null | head -1; }

# 1. The real-world fault: dhcpcd's 1442 on the default route is rewritten to the
#    pinned link MTU, and the rest of the route spec is preserved verbatim.
run_case dhcpcd-1442 \
    'default via 10.32.101.1 dev eth0 proto dhcp src 10.32.101.7 metric 1002 mtu 1442' \
    'default via 10.32.101.1 proto dhcp src 10.32.101.7 metric 1002 mtu 1442'
check "link is pinned to 1320" \
    "link set dev eth0 mtu 1320" \
    "$(grep '^link set' "${CALLS}" | head -1)"
check "route MTU rewritten to the pinned link MTU" \
    "route replace default via 10.32.101.1 proto dhcp src 10.32.101.7 metric 1002 mtu 1320 dev eth0" \
    "$(grep '^route replace' "${CALLS}" | head -1)"

# 2. `ip route show dev X` omits the dev token; it must be added back or the
#    replace would be rejected (and the fault would silently persist).
run_case dev-token-restored \
    'default via 10.32.101.1 dev eth0 proto dhcp metric 1002 mtu 1442' \
    '10.32.101.0/24 proto kernel scope link src 10.32.101.7 metric 1002 mtu 1442'
check "dev token re-added to a dev-scoped route" \
    "route replace 10.32.101.0/24 proto kernel scope link src 10.32.101.7 metric 1002 mtu 1320 dev eth0" \
    "$(grep '^route replace' "${CALLS}" | head -1)"

# 3. Idempotent: a route already at the pinned MTU is left alone, so a re-run (or
#    a boot after the dhcpcd.conf fix) issues no route changes.
run_case already-pinned \
    'default via 10.32.101.1 dev eth0 proto dhcp metric 1002 mtu 1320' \
    'default via 10.32.101.1 proto dhcp metric 1002 mtu 1320'
check "no replace when the route MTU already matches" "0" "$(replaces)"

# 4. A route with no MTU metric inherits the device MTU — leave it alone rather
#    than pinning a value the link may later change.
run_case no-mtu-metric \
    'default via 10.32.101.1 dev eth0 proto dhcp metric 1002' \
    'default via 10.32.101.1 proto dhcp metric 1002'
check "no replace when the route carries no MTU metric" "0" "$(replaces)"

# 5. The pinned value is overridable, and the override reaches the routes too.
new_case custom-mtu
printf '%s\n' 'default via 10.32.101.1 dev eth0 proto dhcp metric 1002 mtu 1442' > "${ROUTES_DEFAULT}"
printf '%s\n' 'default via 10.32.101.1 proto dhcp metric 1002 mtu 1442' > "${ROUTES_DEV}"
PATH="${STUBBIN}:${PATH}" MULGA_NET_SYSFS="${SYSFS}" MULGA_NETPLAN_DIR="${NETPLAN}" MULGA_VPC_NIC_MTU=1408 \
    sh "${PINNER}" >/dev/null 2>"${STDERR}"
check "MULGA_VPC_NIC_MTU overrides both link and route" \
    "route replace default via 10.32.101.1 proto dhcp metric 1002 mtu 1408 dev eth0" \
    "$(grep '^route replace' "${CALLS}" | head -1)"

# 6. UseMTU is forced off under both the documented [DHCPv4] section and the
#    legacy [DHCP] alias netplan renders, so the drop-in cannot parse cleanly and
#    do nothing, and networkd is reloaded rather than waiting for a reboot.
new_case usemtu-off
printf '%s\n' 'default via 10.32.101.1 dev eth0 proto dhcp metric 1002 mtu 1320' > "${ROUTES_DEFAULT}"
printf '%s\n' 'default via 10.32.101.1 proto dhcp metric 1002 mtu 1320' > "${ROUTES_DEV}"
mkdir -p "${NETPLAN}"
printf '[Network]\nDHCP=ipv4\n\n[DHCP]\nRouteMetric=100\nUseMTU=true\n' > "${NETPLAN}/10-netplan-eth0.network"
PATH="${STUBBIN}:${PATH}" MULGA_NET_SYSFS="${SYSFS}" MULGA_NETPLAN_DIR="${NETPLAN}" \
    sh "${PINNER}" >/dev/null 2>"${STDERR}"
DROPIN="${NETPLAN}/10-netplan-eth0.network.d/10-mulga-usemtu.conf"
check "drop-in targets the [DHCPv4] section" "[DHCPv4]" "$(sed -n '1p' "${DROPIN}" 2>/dev/null)"
check "drop-in disables UseMTU" "UseMTU=false" "$(sed -n '2p' "${DROPIN}" 2>/dev/null)"
check "drop-in also states the legacy [DHCP] alias" "[DHCP]" "$(sed -n '4p' "${DROPIN}" 2>/dev/null)"
check "both sections disable UseMTU" "2" "$(grep -c '^UseMTU=false' "${DROPIN}" 2>/dev/null)"
check "networkd is reloaded exactly once" "1" "$(grep -c '^networkctl reload' "${CALLS}")"

# 7. Every rendered unit gets its own drop-in, keyed off its own filename, and
#    one reload covers all of them.
new_case usemtu-off-multi
printf '%s\n' 'default via 10.32.101.1 dev eth0 proto dhcp metric 1002 mtu 1320' > "${ROUTES_DEFAULT}"
printf '%s\n' 'default via 10.32.101.1 proto dhcp metric 1002 mtu 1320' > "${ROUTES_DEV}"
mkdir -p "${NETPLAN}"
printf '[Network]\nDHCP=ipv4\n\n[DHCP]\nUseMTU=true\n' > "${NETPLAN}/10-netplan-eth0.network"
printf '[Network]\nDHCP=ipv4\n\n[DHCP]\nUseMTU=true\n' > "${NETPLAN}/10-netplan-mgmt0.network"
PATH="${STUBBIN}:${PATH}" MULGA_NET_SYSFS="${SYSFS}" MULGA_NETPLAN_DIR="${NETPLAN}" \
    sh "${PINNER}" >/dev/null 2>"${STDERR}"
check "eth0 unit got a drop-in" \
    "UseMTU=false" \
    "$(sed -n '2p' "${NETPLAN}/10-netplan-eth0.network.d/10-mulga-usemtu.conf" 2>/dev/null)"
check "mgmt0 unit got a drop-in" \
    "UseMTU=false" \
    "$(sed -n '2p' "${NETPLAN}/10-netplan-mgmt0.network.d/10-mulga-usemtu.conf" 2>/dev/null)"
check "one reload covers both units" "1" "$(grep -c '^networkctl reload' "${CALLS}")"

# 8. Alpine parity guard: no /run/systemd/network (dhcpcd, no networkd) means
#    no drop-in logic runs at all, not a silent no-op glob.
run_case usemtu-off-absent \
    'default via 10.32.101.1 dev eth0 proto dhcp metric 1002 mtu 1320' \
    'default via 10.32.101.1 proto dhcp metric 1002 mtu 1320'
check "no networkctl call when the netplan dir is absent" "0" "$(grep -c '^networkctl' "${CALLS}")"

# 9. Happy path: after pin + restamp the link and every route match, so the
#    assertion is silent and the script exits 0 (run_case already required
#    this for every case above; this one checks it explicitly).
run_case assert-happy-path \
    'default via 10.32.101.1 dev eth0 proto dhcp metric 1002 mtu 1442' \
    'default via 10.32.101.1 proto dhcp metric 1002 mtu 1442'
check "no assertion errors on the happy path" "0" "$(grep -c 'ERROR' "${STDERR}")"

# 10. A link that did not take the pin (driver rejected it, or something reset
#     it) is a loud, non-zero-exit failure, not a silently wrong node.
new_case assert-catches-link-drift
printf '%s\n' 'default via 10.32.101.1 dev eth0 proto dhcp metric 1002 mtu 1320' > "${ROUTES_DEFAULT}"
printf '%s\n' 'default via 10.32.101.1 proto dhcp metric 1002 mtu 1320' > "${ROUTES_DEV}"
mkdir -p "${SYSFS}/eth0"
printf '1500\n' > "${SYSFS}/eth0/mtu"
rc=0
PATH="${STUBBIN}:${PATH}" MULGA_NET_SYSFS="${SYSFS}" MULGA_NETPLAN_DIR="${NETPLAN}" LINKSET_FAIL=1 \
    sh "${PINNER}" >/dev/null 2>"${STDERR}" || rc=$?
check "script fails loudly when the link MTU cannot be verified" "1" "$rc"
check "the drift is reported on stderr" "1" "$(grep -c 'ERROR:.*link MTU' "${STDERR}")"

# 11. A route the restamp pass could not fix is likewise a loud failure — the
#     scenario this whole script exists for, now caught instead of assumed.
new_case assert-catches-route-drift
printf '%s\n' 'default via 10.32.101.1 dev eth0 proto dhcp metric 1002 mtu 1442' > "${ROUTES_DEFAULT}"
printf '%s\n' 'default via 10.32.101.1 proto dhcp metric 1002 mtu 1442' > "${ROUTES_DEV}"
rc=0
PATH="${STUBBIN}:${PATH}" MULGA_NET_SYSFS="${SYSFS}" MULGA_NETPLAN_DIR="${NETPLAN}" ROUTE_FAIL=1 \
    sh "${PINNER}" >/dev/null 2>"${STDERR}" || rc=$?
check "script fails loudly when a route mtu cannot be restamped" "1" "$rc"
check "the drift is reported on stderr" "1" "$(grep -c 'ERROR:.*route carries conflicting mtu' "${STDERR}")"

echo "pass=${PASS} fail=${FAIL}"
[ "${FAIL}" -eq 0 ]
