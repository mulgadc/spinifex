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
# Reaps the stub's simulated daemons too, so a failing run does not strand
# background processes on the test host.
cleanup() {
    if [ -s "${WORK}/daemons" ]; then
        while read -r pid _; do kill "${pid}" 2>/dev/null || true; done < "${WORK}/daemons"
    fi
    rm -rf "${WORK}"
}
trap cleanup EXIT

STUBBIN="${WORK}/bin"
mkdir -p "${STUBBIN}"

# ip stub: records every invocation and answers the queries the script makes.
# Default routes live one per line in DEFAULTS as "<iface> <gateway>", so a
# `route del` really removes one and a re-query sees the result; policy routes
# live in TABLES as "<table> <destination> <full command>" and rules in RULES as
# "<pref> <source> <table>", so idempotency is observable rather than inferred
# from the call log. The interface is the last argument in the dev-scoped forms.
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
    *" route replace "*" table "*)
        dst=""; tid=""; prev=""
        for a in "$@"; do
            [ "${prev}" = "replace" ] && dst="${a}"
            [ "${prev}" = "table" ] && tid="${a}"
            prev="${a}"
        done
        # `replace` overwrites the entry for that destination, as the real one does.
        grep -v "^${tid} ${dst} " "${TABLES}" > "${TABLES}.tmp" 2>/dev/null
        mv "${TABLES}.tmp" "${TABLES}"
        echo "${tid} ${dst} $*" >> "${TABLES}" ;;
    *" rule add "*)
        src=""; tid=""; pref=""; prev=""
        for a in "$@"; do
            [ "${prev}" = "from" ] && src="${a}"
            [ "${prev}" = "table" ] && tid="${a}"
            [ "${prev}" = "pref" ] && pref="${a}"
            prev="${a}"
        done
        # Appended unconditionally: a real `ip rule add` of a duplicate lists it twice.
        echo "${pref} ${src} ${tid}" >> "${RULES}" ;;
    *" rule del "*)
        pref=""; prev=""
        for a in "$@"; do
            [ "${prev}" = "pref" ] && pref="${a}"
            prev="${a}"
        done
        grep -q "^${pref} " "${RULES}" || exit 2
        grep -v "^${pref} " "${RULES}" > "${RULES}.tmp"
        mv "${RULES}.tmp" "${RULES}" ;;
    *" addr show dev "*)
        if grep -q "^${iface}\$" "${LEASED}" 2>/dev/null; then
            awk -v d="${iface}" '$1 == d { print "    inet " $2 " scope global" }' "${ADDRS}"
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

# Both clients keep running unless told to exit, and the flag that does it is
# per-client: dhcpcd needs -1 (-q is only quiet), udhcpc needs -q. Leaving a
# live process behind is what the real client does, so the stub does it too.
oneshot=no
case "$(basename "$0")" in
    dhcpcd) case " $* " in *" -1 "*) oneshot=yes ;; esac ;;
    udhcpc) case " $* " in *" -q "*) oneshot=yes ;; esac ;;
esac
if [ "${oneshot}" = no ]; then
    sleep 30 &
    echo "$! $(basename "$0")" >> "${DAEMONS}"
fi

# dhcpcd de-configures the interface on exit unless -p, so a one-shot without
# it hands back a link with no address at all.
if [ "$(basename "$0")" = dhcpcd ] && [ "${oneshot}" = yes ]; then
    case " $* " in *" -p "*) ;; *) exit 0 ;; esac
fi

echo "${iface}" >> "${LEASED}"
# What a real lease installs, and the reason the policy exists at all: OVN serves
# the subnet's router IP as the gateway, which is what the return path needs.
gw=$(awk -v d="${iface}" '$1 == d { print $2; exit }' "${GWS}")
grep -q "^${iface} " "${DEFAULTS}" || echo "${iface} ${gw}" >> "${DEFAULTS}"
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

# What each NIC leases: the system VPC on eth0, a customer subnet on eth1. The
# customer address has a non-zero host part, so an assertion on the connected
# route in the policy table exercises the script's mask arithmetic.
ADDRS="${WORK}/addrs"
cat > "${ADDRS}" <<'EOF'
eth0 10.251.185.42/24
eth1 10.60.2.15/24
EOF

GWS="${WORK}/gws"
cat > "${GWS}" <<'EOF'
eth0 10.251.185.1
eth1 10.60.2.1
EOF

export LINKS ADDRS GWS
export MULGA_NETCFG="${NETCFG}"
# The stub client is instant; a real budget would only slow a failing test.
export MULGA_DHCP_BUDGET=5

IP_CALLS="${WORK}/ip-calls"; export IP_CALLS
DEFAULTS="${WORK}/defaults";  export DEFAULTS
LEASED="${WORK}/leased";      export LEASED
TABLES="${WORK}/tables";      export TABLES
RULES="${WORK}/rules";        export RULES
DAEMONS="${WORK}/daemons";    export DAEMONS

reset_state() {
    : > "${IP_CALLS}"; : > "${DEFAULTS}"; : > "${LEASED}"
    : > "${TABLES}"; : > "${RULES}"; : > "${DAEMONS}"
}

called()      { grep -qF "$1" "${IP_CALLS}"; }
has_default() { grep -q "^$1 " "${DEFAULTS}"; }
leased()      { grep -q "^$1\$" "${LEASED}"; }
# A route in policy table $1 for destination $2, and its full command in $3.
has_route()   { grep -q "^$1 $2 .*$3" "${TABLES}"; }
rule_count()  { grep -c "^$1 " "${RULES}"; }
# Reports which client was left running, not just that one was: the two
# branches fail for different reasons and the flag that fixes them differs.
survivors()   { [ -s "${DAEMONS}" ] && { echo "surviving DHCP clients: $(cat "${DAEMONS}")"; return 0; }; return 1; }

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

want 'called "ip route replace 169.254.169.254/32 via 10.251.185.1 dev eth0"' \
    "setup pins IMDS to the primary ENI"

# Every NIC in the blob has to be reached. A `set -e` trip part way through the
# loop is otherwise silent: the NICs before it are configured and the ones after
# it are never touched, which is how the bounded route-delete loop's trailing
# test escaped review.
want 'grep -q "configured eth2" "${WORK}/setup.out"' "setup reaches every NIC in the blob"

wantnot 'survivors' "setup leaves no DHCP client running on any NIC"

# --- The customer ENI's return path ---------------------------------------

# Deleting the customer ENI's default route is only half the policy: without a
# table of its own, a reply to a client in another subnet of the customer VPC
# matches nothing but main's default and leaves by the primary ENI.
want 'has_route 101 default "default via 10.60.2.1 dev eth1"' \
    "setup gives the customer ENI a default via its lease's gateway in its own table"
want 'has_route 101 10.60.2.0/24 "dev eth1 scope link src 10.60.2.15"' \
    "setup keeps the customer subnet on-link in its table, so the same-subnet case is unchanged"
want '[ "$(grep -c . "${RULES}")" -eq 1 ]' "setup adds exactly one rule"
want 'grep -q "^101 10.60.2.15 101\$" "${RULES}"' \
    "setup steers replies sourced from the endpoint address at that table"

# The primary ENI keeps main's default and needs no policy of its own; the
# static mgmt NIC never leases one.
wantnot 'grep -q " 10.251.185.42 " "${RULES}"' "setup adds no rule for the primary ENI"
wantnot 'grep -qE "^(100|102) " "${TABLES}"' "setup adds no policy table for the other NICs"

# A second setup pass is a stop/start re-attach. `ip rule add` appends, so a rule
# not deleted by priority first would be listed twice and the table would grow.
if ! sh "${SCRIPT}" > "${WORK}/setup2.out" 2>&1; then
    fail "second setup pass exited non-zero"
    cat "${WORK}/setup2.out"
fi
want '[ "$(rule_count 101)" -eq 1 ]' "a repeated setup pass leaves exactly one rule"
want '[ "$(grep -c "^101 " "${TABLES}")" -eq 2 ]' "a repeated setup pass leaves exactly two routes in the table"

# --- The dhcpcd fallback --------------------------------------------------

# Forced by removing udhcpc from PATH rather than left to whether the host has
# /usr/share/udhcpc/default.script: every Ubuntu image takes this branch, and it
# is the one that stranded a daemon on the data NIC and cost cloud-init 300s.
FALLBACKBIN="${WORK}/fallback-bin"
mkdir -p "${FALLBACKBIN}"
cp "${STUBBIN}/ip" "${STUBBIN}/dhcpcd" "${FALLBACKBIN}/"
chmod +x "${FALLBACKBIN}"/*

reset_state
if ! PATH="${FALLBACKBIN}:/usr/bin:/bin" sh "${SCRIPT}" > "${WORK}/fallback.out" 2>&1; then
    fail "dhcpcd fallback pass exited non-zero"
    cat "${WORK}/fallback.out"
fi

wantnot 'survivors' "the dhcpcd fallback leaves no daemon on the data NIC"
want 'leased eth0' "the dhcpcd fallback still leaves the interface addressed after it exits"
want 'called "dhcp -q -1 -p -t 20 eth0"' "the dhcpcd fallback is invoked one-shot and persistent"

# --- The enforce pass -----------------------------------------------------

# cloud-init's network stage re-renders the ENIs from IMDS and re-DHCPs them,
# putting back the default route on the customer ENI. That is the state the
# enforce pass exists to correct.
reset_state
printf 'eth0 10.251.185.1\neth1 10.60.2.1\n' > "${DEFAULTS}"
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

# --- The policy table after an interface bounce ---------------------------

# cloud-init bouncing eth1 takes the table's routes down with the device, but
# not the rule — rules are not device-scoped. A rule whose table lookup finds
# nothing falls through to main, which is the bug, silently. So the enforce pass
# has to rewrite the routes, not just re-assert the rule.
: > "${TABLES}"
printf 'eth0 10.251.185.1\neth1 10.60.2.1\n' > "${DEFAULTS}"

if ! sh "${SCRIPT}" --enforce-routes > "${WORK}/bounce.out" 2>&1; then
    fail "enforce pass exited non-zero (after a bounce)"
    cat "${WORK}/bounce.out"
fi

want 'has_route 101 default "default via 10.60.2.1 dev eth1"' \
    "enforce reinstalls the table's default after the interface bounced"
want 'has_route 101 10.60.2.0/24 "dev eth1 scope link src 10.60.2.15"' \
    "enforce reinstalls the table's connected route after the interface bounced"
want '[ "$(rule_count 101)" -eq 1 ]' "enforce leaves exactly one rule, not a duplicate"

# --- Multiple default routes on one interface -----------------------------

# A re-DHCP can leave more than one, and deleting only the first would keep the
# interface carrying a default route the policy says it must not have.
reset_state
printf 'eth1 10.60.2.1\neth1 10.60.2.2\neth0 10.251.185.1\n' > "${DEFAULTS}"
printf 'eth0\neth1\n' > "${LEASED}"

if ! sh "${SCRIPT}" --enforce-routes > "${WORK}/multi.out" 2>&1; then
    fail "enforce pass exited non-zero (multiple defaults)"
    cat "${WORK}/multi.out"
fi
wantnot 'has_default eth1' "enforce clears every default route on the customer ENI, not just the first"

# --- A guest whose only NIC is the default one ----------------------------

# An eks-node carries no extra ENI. The policy is for a NIC that is barred from
# the default route, so a guest without one must come out of both passes with
# main untouched and no rules at all.
ONENIC="${WORK}/netcfg-one"
printf 'NIC0_MAC=02:aa:00:00:00:01\nNIC0_DHCP=1\nNIC0_DEFAULT=1\n' > "${ONENIC}"

reset_state
if ! MULGA_NETCFG="${ONENIC}" sh "${SCRIPT}" > "${WORK}/onenic.out" 2>&1; then
    fail "setup pass exited non-zero (single NIC)"
    cat "${WORK}/onenic.out"
fi
want    'has_default eth0' "a single-NIC guest keeps its default route"
wantnot 'grep -q . "${RULES}"'  "a single-NIC guest gets no rules"
wantnot 'grep -q . "${TABLES}"' "a single-NIC guest gets no policy tables"

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
