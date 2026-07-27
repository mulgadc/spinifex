#!/bin/sh
# Self-contained POSIX test for mulga-vpc-mtu.sh route re-stamping. No root or
# real interfaces needed: `ip` is stubbed on PATH, serving fixture route output
# and recording every mutating call.
#
# Covers the fault this script exists to prevent: dhcpcd stamps the DHCP MTU
# (option 26) onto its routes, Linux prefers a route's MTU metric over the device
# MTU, so pinning only the link leaves the node advertising an MSS too large for
# the path and large inbound segments blackhole in the host netns.
#
# Run: sh scripts/images/eks-node/mulga-vpc-mtu_test.sh
set -eu

SCRIPT_DIR=$(cd "$(dirname "$0")" && pwd)
PINNER="${SCRIPT_DIR}/mulga-vpc-mtu.sh"
WORK=$(mktemp -d)
trap 'rm -rf "${WORK}"' EXIT

STUBBIN="${WORK}/bin"
mkdir -p "${STUBBIN}"

# Stub `ip`: serves ${ROUTES_DEFAULT} / ${ROUTES_DEV} for show, records mutations
# ("link set …" / "route replace …") to ${CALLS}. Anything else is a no-op.
cat > "${STUBBIN}/ip" <<'STUB'
#!/bin/sh
args="$*"
case "$args" in
    *"route show default"*) cat "${ROUTES_DEFAULT}"; exit 0 ;;
    *"route show dev "*)    cat "${ROUTES_DEV}"; exit 0 ;;
    "link show dev "*)      exit 0 ;;
    "link set dev "*)       echo "$args" >> "${CALLS}"; exit 0 ;;
    "route replace "*)      echo "$args" >> "${CALLS}"; exit 0 ;;
esac
exit 0
STUB
chmod +x "${STUBBIN}/ip"

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

# run_case <name> <default-routes> <dev-routes>: runs the pinner against fixture
# route tables and leaves the recorded mutations in ${CALLS}.
run_case() {
    CASE="${WORK}/$1"
    mkdir -p "${CASE}"
    CALLS="${CASE}/calls"
    ROUTES_DEFAULT="${CASE}/default"
    ROUTES_DEV="${CASE}/dev"
    : > "${CALLS}"
    printf '%s\n' "$2" > "${ROUTES_DEFAULT}"
    printf '%s\n' "$3" > "${ROUTES_DEV}"
    export CALLS ROUTES_DEFAULT ROUTES_DEV
    PATH="${STUBBIN}:${PATH}" sh "${PINNER}" >/dev/null 2>&1
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
run_case custom-mtu \
    'default via 10.32.101.1 dev eth0 proto dhcp metric 1002 mtu 1442' \
    'default via 10.32.101.1 proto dhcp metric 1002 mtu 1442'
CASE="${WORK}/custom-mtu-run"
mkdir -p "${CASE}"
CALLS="${CASE}/calls"
: > "${CALLS}"
export CALLS
PATH="${STUBBIN}:${PATH}" MULGA_VPC_NIC_MTU=1408 sh "${PINNER}" >/dev/null 2>&1
check "MULGA_VPC_NIC_MTU overrides both link and route" \
    "route replace default via 10.32.101.1 proto dhcp metric 1002 mtu 1408 dev eth0" \
    "$(grep '^route replace' "${CALLS}" | head -1)"

echo "pass=${PASS} fail=${FAIL}"
[ "${FAIL}" -eq 0 ]
