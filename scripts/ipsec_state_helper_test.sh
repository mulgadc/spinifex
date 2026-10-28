#!/bin/sh
# ipsec_state_helper_test.sh — tests for the plaintext Geneve guard in the
# IPsec state helper that setup.sh installs.
#
# The helper is cut out of setup.sh's heredoc and run against stub ip,
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
cat > "$TMP/bin/ip" <<EOF
#!/bin/sh
case "\$*" in
    "xfrm policy list "*) cat "$TMP/policies" ;;
    *) echo "\$*" >> "$TMP/ip.log" ;;
esac
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
    *) exit 1 ;;
esac
EOF
printf '#!/bin/sh\nexit 0\n' > "$TMP/bin/systemctl"
chmod +x "$TMP/bin/ip" "$TMP/bin/ovs-vsctl" "$TMP/bin/systemctl"

# reset — no tunnels, no policies, OVS up, no pluto.
reset() {
    rm -f "$TMP"/opt.* "$TMP/ovs-down" "$TMP/ip.log" "$TMP/pluto"
    : > "$TMP/ifaces"
    : > "$TMP/policies"
    touch "$TMP/ip.log"
}
# tunnel <name> <local_ip|-> [remote_name] — a geneve interface as ovn-controller writes it.
tunnel() {
    echo "$1" >> "$TMP/ifaces"
    [ "$2" = - ] || printf '"%s"\n' "$2" > "$TMP/opt.$1.local_ip"
    [ -z "${3:-}" ] || echo "$3" > "$TMP/opt.$1.remote_name"
}
# held <src> — a guard block already in the kernel, as `ip xfrm policy list` prints it.
held() {
    printf 'src %s dst 0.0.0.0/0 proto udp dport 6081 \n\tdir out action block priority 4000000 ptype main \n' "$1" >> "$TMP/policies"
}
run() {
    RC=0
    OUT=$(PATH="$TMP/bin:$PATH" "$HELPER" "$1" 2>&1) || RC=$?
    IPLOG=$(cat "$TMP/ip.log")
}
UPDATE="xfrm policy update src %s dst 0.0.0.0/0 proto udp dport 6081 dir out priority 4000000 action block"
DELETE="xfrm policy delete src %s dst 0.0.0.0/0 proto udp dport 6081 dir out"
# expect_log <desc> <expected ip calls, one per line>
expect_log() {
    if [ "$RC" = 0 ] && [ "$IPLOG" = "$2" ]; then pass "$1"; else fail "$1 (rc=$RC): got [$IPLOG] want [$2] $OUT"; fi
}

reset
tunnel ovn-node2-0 10.26.7.1 node2
tunnel ovn-node3-0 10.26.7.1 node3
run guard
# shellcheck disable=SC2059
expect_log "blocks plaintext Geneve from the encap address once OVN wants the tunnels encrypted" "$(printf "$UPDATE" 10.26.7.1/32)"

reset
tunnel ovn-node2-0 10.26.7.1
tunnel ovn-node3-0 10.26.7.1
run guard
expect_log "adds no block while OVN runs the tunnels in plaintext" ""

reset
tunnel ovn-node2-0 10.26.7.1
held 10.26.7.1/32
run guard
# shellcheck disable=SC2059
expect_log "lifts the block once OVN stops asking for encryption" "$(printf "$DELETE" 10.26.7.1/32)"

reset
tunnel ovn-node2-0 10.26.7.1 node2
held 10.26.7.9/32
run guard
# shellcheck disable=SC2059
expect_log "replaces a block for an encap address the host no longer uses" \
    "$(printf "$UPDATE\n$DELETE" 10.26.7.1/32 10.26.7.9/32)"

reset
tunnel ovn-node2-0 - node2
run guard
# shellcheck disable=SC2059
expect_log "guards a tunnel without a local_ip from every source" "$(printf "$UPDATE" 0.0.0.0/0)"

reset
tunnel ovn-node2-0 10.26.7.1 node2
held 10.26.7.1/32
touch "$TMP/ovs-down"
run guard
if [ "$RC" != 0 ] && [ -z "$IPLOG" ]; then
    pass "keeps the block and fails when OVS cannot be read"
else
    fail "with OVS down: rc=$RC ip calls [$IPLOG]"
fi

reset
touch "$TMP/pluto"
chmod +x "$TMP/pluto"
held 10.26.7.1/32
run off
# shellcheck disable=SC2059
expect_log "off removes the block on a libreswan host" "$(printf "$DELETE" 10.26.7.1/32)"

reset
held 10.26.7.1/32
run unguard
# shellcheck disable=SC2059
expect_log "unguard removes the block" "$(printf "$DELETE" 10.26.7.1/32)"

reset
run bogus
if [ "$RC" = 2 ] && [ -z "$IPLOG" ]; then pass "refuses an unknown verb"; else fail "unknown verb: rc=$RC [$IPLOG]"; fi

if [ "$fails" -gt 0 ]; then
    echo "$fails failure(s)"
    exit 1
fi
echo "all passed"
