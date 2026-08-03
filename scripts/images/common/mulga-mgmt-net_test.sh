#!/bin/sh
# Self-contained POSIX test for mulga-mgmt-net: the setup pass that leases and
# addresses each NIC from the fw_cfg blob, and the --enforce-routes pass that
# re-asserts the route policy after cloud-init has re-rendered the ENIs. No
# interfaces and no root: ip and both DHCP clients are stubbed on PATH and driven
# from a fake routing table.
#
# Run: sh scripts/images/common/mulga-mgmt-net_test.sh
set -eu

SCRIPT_DIR=$(cd "$(dirname "$0")" && pwd)
SCRIPT="${SCRIPT_DIR}/mulga-mgmt-net.sh"
WORK=$(mktemp -d)
trap 'rm -rf "${WORK}"' EXIT

STUBBIN="${WORK}/bin"
mkdir -p "${STUBBIN}"

# ip stub: records every invocation and answers the three queries the script
# makes. Default routes live one per line in DEFAULTS as "<iface> <gateway>", so
# a `route del` really removes one and a re-query sees the result. The interface
# is the last argument in every form the script uses.
cat > "${STUBBIN}/ip" <<'EOF'
#!/bin/sh
echo "ip $*" >> "${IP_CALLS}"
for a in "$@"; do iface="${a}"; done
case " $* " in
    *" -o link "*)
        awk '{ printf "%d: %s: <BROADCAST> mtu 1500 link/ether %s\n", NR, $1, $2 }' "${LINKS}" ;;
    *" route show default dev "*)
        awk -v d="${iface}" '$1 == d { print "default via " $2 " dev " $1 }' "${DEFAULTS}" ;;
    *" route del default dev "*)
        awk -v d="${iface}" '$1 != d' "${DEFAULTS}" > "${DEFAULTS}.tmp"
        mv "${DEFAULTS}.tmp" "${DEFAULTS}" ;;
    *" addr show dev "*)
        if grep -q "^${iface}\$" "${LEASED}" 2>/dev/null; then
            echo "    inet 10.0.0.5/20 scope global"
        fi ;;
esac
exit 0
EOF

# Both DHCP clients are stubbed, because which one the script reaches for turns
# on whether the test host happens to have /usr/share/udhcpc/default.script —
# and the assertions below are about the lease, not about the client.
cat > "${STUBBIN}/dhcp-stub" <<'EOF'
#!/bin/sh
echo "dhcp $*" >> "${IP_CALLS}"
# udhcpc takes -i <iface>; dhcpcd takes the interface last.
iface=""
prev=""
for a in "$@"; do
    [ "${prev}" = "-i" ] && iface="${a}"
    prev="${a}"
done
[ -n "${iface}" ] || { for a in "$@"; do iface="${a}"; done; }
echo "${iface}" >> "${LEASED}"
# What a real lease installs, and the reason the policy exists at all.
grep -q "^${iface} " "${DEFAULTS}" || echo "${iface} 172.31.0.1" >> "${DEFAULTS}"
exit 0
EOF
cp "${STUBBIN}/dhcp-stub" "${STUBBIN}/udhcpc"
cp "${STUBBIN}/dhcp-stub" "${STUBBIN}/dhcpcd"
rm "${STUBBIN}/dhcp-stub"

chmod +x "${STUBBIN}"/*
PATH="${STUBBIN}:${PATH}"
export PATH

FAILS=0
fail() { echo "FAIL: $*"; FAILS=$((FAILS + 1)); }
pass() { echo "ok: $*"; }
# The condition is evaluated in an `if`, which `set -e` exempts, so a false
# assertion reports rather than aborting the run.
want()    { if eval "$1"; then pass "$2"; else fail "$2"; fi; }
wantnot() { if eval "$1"; then fail "$2"; else pass "$2"; fi; }

# The RDS DB VM's three NICs: system ENI (default), customer ENI (DHCP but never
# default) and the static mgmt NIC. This is the shape that regressed — the
# customer ENI is the one whose default route must not survive.
NETCFG="${WORK}/netcfg"
cat > "${NETCFG}" <<'EOF'
NIC0_MAC=02:aa:00:00:00:01
NIC0_DHCP=1
NIC0_DEFAULT=1
NIC1_MAC=02:aa:00:00:00:02
NIC1_DHCP=1
NIC1_DEFAULT=0
NIC2_MAC=02:aa:00:00:00:03
NIC2_CIDR=10.15.8.10/24
NIC2_DEFAULT=0
EOF

LINKS="${WORK}/links"
cat > "${LINKS}" <<'EOF'
eth0 02:aa:00:00:00:01
eth1 02:aa:00:00:00:02
eth2 02:aa:00:00:00:03
EOF

export LINKS
export MULGA_NETCFG="${NETCFG}"
# The stub client is instant; a real budget would only slow a failing test.
export MULGA_DHCP_BUDGET=5

IP_CALLS="${WORK}/ip-calls"; export IP_CALLS
DEFAULTS="${WORK}/defaults";  export DEFAULTS
LEASED="${WORK}/leased";      export LEASED

reset_state() { : > "${IP_CALLS}"; : > "${DEFAULTS}"; : > "${LEASED}"; }

called()      { grep -qF "$1" "${IP_CALLS}"; }
has_default() { grep -q "^$1 " "${DEFAULTS}"; }
leased()      { grep -q "^$1\$" "${LEASED}"; }

# --- The setup pass -------------------------------------------------------

reset_state
if ! sh "${SCRIPT}" > "${WORK}/setup.out" 2>&1; then
    fail "setup pass exited non-zero"
    cat "${WORK}/setup.out"
fi

want 'leased eth0' "setup leases the primary ENI"
want 'leased eth1' "setup leases the customer ENI"
want 'called "ip addr replace 10.15.8.10/24 dev eth2"' "setup addresses the static mgmt NIC"

want    'has_default eth0' "setup leaves the primary ENI's leased default route in place"
wantnot 'has_default eth1' "setup deletes the default route the customer ENI leased"

want 'called "ip route replace 169.254.169.254/32 via 172.31.0.1 dev eth0"' \
    "setup pins IMDS to the primary ENI"

# Every NIC in the blob has to be reached. A `set -e` trip part way through the
# loop is otherwise silent: the NICs before it are configured and the ones after
# it are never touched, which is how the bounded route-delete loop's trailing
# test escaped review.
want 'grep -q "configured eth2" "${WORK}/setup.out"' "setup reaches every NIC in the blob"

# --- The enforce pass -----------------------------------------------------

# cloud-init's network stage re-renders the ENIs from IMDS and re-DHCPs them,
# putting back the default route on the customer ENI. That is the state the
# enforce pass exists to correct.
reset_state
printf 'eth0 10.251.185.1\neth1 172.31.0.1\n' > "${DEFAULTS}"
printf 'eth0\neth1\n' > "${LEASED}"

if ! sh "${SCRIPT}" --enforce-routes > "${WORK}/enforce.out" 2>&1; then
    fail "enforce pass exited non-zero"
    cat "${WORK}/enforce.out"
fi

wantnot 'has_default eth1' "enforce removes the default route cloud-init restored"
want    'has_default eth0' "enforce leaves the primary ENI's default route alone"

wantnot 'called "dhcp "'         "enforce does not re-lease an interface"
wantnot 'called "ip addr replace"' "enforce does not re-address an interface"

want 'called "ip route replace 169.254.169.254/32 via 10.251.185.1 dev eth0"' \
    "enforce re-pins IMDS after a re-DHCP"

# --- Multiple default routes on one interface -----------------------------

# A re-DHCP can leave more than one, and deleting only the first would keep the
# interface carrying a default route the policy says it must not have.
reset_state
printf 'eth1 172.31.0.1\neth1 172.31.0.2\neth0 10.251.185.1\n' > "${DEFAULTS}"
printf 'eth0\neth1\n' > "${LEASED}"

if ! sh "${SCRIPT}" --enforce-routes > "${WORK}/multi.out" 2>&1; then
    fail "enforce pass exited non-zero (multiple defaults)"
    cat "${WORK}/multi.out"
fi
wantnot 'has_default eth1' "enforce clears every default route on the customer ENI, not just the first"

# --- No blob --------------------------------------------------------------

reset_state
if MULGA_NETCFG="${WORK}/absent" sh "${SCRIPT}" --enforce-routes > "${WORK}/noblob.out" 2>&1; then
    want 'grep -q "no fw_cfg netcfg" "${WORK}/noblob.out"' "a missing blob is a clean no-op"
else
    fail "a missing blob must be a no-op, not a failure"
fi

echo
if [ "${FAILS}" -ne 0 ]; then
    echo "${FAILS} failure(s)"
    exit 1
fi
echo "all mulga-mgmt-net checks passed"
