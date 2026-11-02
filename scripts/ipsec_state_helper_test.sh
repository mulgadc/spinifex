#!/bin/sh
# ipsec_state_helper_test.sh — tests for the plaintext Geneve guard in the
# IPsec state helper that setup.sh installs.
#
# The helper is cut out of setup.sh's heredoc and run against stub nft,
# ovs-vsctl and systemctl commands, with pluto's path pointed at a fixture.
set -eu

SCRIPT_DIR=$(CDPATH='' cd -- "$(dirname -- "$0")" && pwd)
SETUP="$SCRIPT_DIR/setup.sh"
TMP=$(mktemp -d)
trap 'rm -rf "$TMP"' EXIT

fails=0
pass() { echo "  ok   - $1"; }
fail() { echo "  FAIL - $1"; fails=$((fails + 1)); }

echo "-- IPsec state helper: plaintext Geneve guard"

HELPER="$TMP/helper"
awk '/tee "\$\{IPSEC_STATE_HELPER\}"/ { on = 1; next } on && /^HELPER$/ { exit } on { print }' "$SETUP" |
    sed "s#/usr/libexec/ipsec/pluto#$TMP/pluto#g" > "$HELPER"
chmod +x "$HELPER"
grep -q '^    guard)' "$HELPER" || { echo "could not extract the helper from setup.sh" >&2; exit 1; }

mkdir -p "$TMP/bin"
cat > "$TMP/bin/nft" <<EOF
#!/bin/sh
[ "\$*" = "-f -" ] || { echo "nft \$*" >> "$TMP/nft.log"; exit 0; }
cat >> "$TMP/nft.log"
echo "--" >> "$TMP/nft.log"
EOF
cat > "$TMP/bin/ovs-vsctl" <<EOF
#!/bin/sh
[ -e "$TMP/ovs-down" ] && { echo "ovs-vsctl: database connection failed" >&2; exit 1; }
case "\$*" in
    *" find Interface type=geneve") cat "$TMP/ifaces" ;;
    *" get Interface "*)
        for last; do :; done
        f="$TMP/opt.\$5.\${last#options:}"
        if [ -e "\$f" ]; then cat "\$f"; else echo; fi
        ;;
    *" get Open_vSwitch . external_ids:ovn-encap-ip")
        if [ -e "$TMP/encap" ]; then cat "$TMP/encap"; else echo; fi
        ;;
    *) exit 1 ;;
esac
EOF
printf '#!/bin/sh\nexit 0\n' > "$TMP/bin/systemctl"
chmod +x "$TMP/bin/nft" "$TMP/bin/ovs-vsctl" "$TMP/bin/systemctl"

# reset — no tunnels, OVS up, no pluto.
reset() {
    rm -f "$TMP"/opt.* "$TMP/ovs-down" "$TMP/nft.log" "$TMP/pluto" "$TMP/encap"
    : > "$TMP/ifaces"
    touch "$TMP/nft.log"
}
# tunnel <name> <local_ip|-> [remote_name] — a geneve interface as ovn-controller writes it.
tunnel() {
    echo "$1" >> "$TMP/ifaces"
    [ "$2" = - ] || printf '"%s"\n' "$2" > "$TMP/opt.$1.local_ip"
    [ -z "${3:-}" ] || echo "$3" > "$TMP/opt.$1.remote_name"
}
run() {
    RC=0
    OUT=$(PATH="$TMP/bin:$PATH" "$HELPER" "$1" 2>&1) || RC=$?
    NFTLOG=$(cat "$TMP/nft.log")
}
LIFTED="table inet spinifex_ipsec_guard
delete table inet spinifex_ipsec_guard
--"
# expect_guarded <desc> <sources> — one transaction that swaps in a table
# dropping Geneve without an SA from exactly those sources.
expect_guarded() {
    if [ "$RC" = 0 ] &&
        [ "$(grep -c '^--$' "$TMP/nft.log")" = 1 ] &&
        [ "$(sed -n 2p "$TMP/nft.log")" = "delete table inet spinifex_ipsec_guard" ] &&
        grep -q "^        elements = { $2 }$" "$TMP/nft.log" &&
        grep -q '^        ip saddr @srcs udp dport 6081 rt ipsec missing drop$' "$TMP/nft.log"; then
        pass "$1"
    else
        fail "$1 (rc=$RC): got [$NFTLOG] want sources [$2] $OUT"
    fi
}
expect_lifted() {
    if [ "$RC" = 0 ] && [ "$NFTLOG" = "$LIFTED" ]; then pass "$1"; else fail "$1 (rc=$RC): got [$NFTLOG] $OUT"; fi
}

reset
tunnel ovn-node2-0 10.26.7.1 node2
tunnel ovn-node3-0 10.26.7.1 node3
run guard
expect_guarded "guards the encap address once OVN wants the tunnels encrypted" "10.26.7.1/32"

reset
tunnel ovn-node2-0 10.26.7.1
tunnel ovn-node3-0 10.26.7.1
run guard
expect_lifted "lifts the guard while OVN runs the tunnels in plaintext"

reset
tunnel ovn-node2-0 10.26.7.1 node2
tunnel ovn-node3-0 10.26.8.1
run guard
expect_guarded "guards only the addresses of tunnels OVN encrypts" "10.26.7.1/32"

reset
tunnel ovn-node2-0 - node2
run guard
expect_guarded "guards a tunnel without a local_ip from every source" "0.0.0.0/0"

reset
echo '"10.26.7.1"' > "$TMP/encap"
run guard
expect_guarded "guards the encap address before ovn-controller has built the tunnels" "10.26.7.1/32"

reset
echo '"10.26.7.1,10.26.8.1"' > "$TMP/encap"
run guard
expect_guarded "guards each encap address of a multi-address host before the tunnels exist" "10.26.7.1/32,10.26.8.1/32"

reset
run guard
expect_guarded "guards every source before the tunnels exist when no encap address is set" "0.0.0.0/0"

reset
tunnel ovn-node2-0 10.26.7.1 node2
touch "$TMP/ovs-down"
run guard
if [ "$RC" != 0 ] && [ -z "$NFTLOG" ]; then
    pass "keeps the guard and fails when OVS cannot be read"
else
    fail "with OVS down: rc=$RC nft calls [$NFTLOG]"
fi

reset
touch "$TMP/pluto"
chmod +x "$TMP/pluto"
run off
expect_lifted "off lifts the guard on a libreswan host"

reset
run off
if [ "$RC" = 0 ] && [ -z "$NFTLOG" ]; then pass "off leaves nft alone without libreswan"; else fail "off without pluto: rc=$RC [$NFTLOG]"; fi

reset
run unguard
expect_lifted "unguard lifts the guard"

reset
run bogus
if [ "$RC" = 2 ] && [ -z "$NFTLOG" ]; then pass "refuses an unknown verb"; else fail "unknown verb: rc=$RC [$NFTLOG]"; fi

if [ "$fails" -gt 0 ]; then
    echo "$fails failure(s)"
    exit 1
fi
echo "all passed"
