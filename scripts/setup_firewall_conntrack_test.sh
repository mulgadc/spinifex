#!/bin/sh
# setup_firewall_conntrack_test.sh — arms the real host firewall under a TCP
# flow that opened while it was disarmed, in two network namespaces.
#
# The policy, the tracking table and spinifex-firewall-apply are cut out of
# setup.sh with their paths moved under a temp dir, and the helper's verbs are
# run inside the client namespace. Needs root or passwordless sudo, ip, nft and
# python3; skips without them. A stub systemctl keeps the helper off the host.
set -eu

SCRIPT_DIR=$(CDPATH='' cd -- "$(dirname -- "$0")" && pwd)
SETUP="$SCRIPT_DIR/setup.sh"

echo "-- setup.sh firewall: flows that outlive a disarm"

if [ "$(id -u)" = 0 ]; then SUDO=""
elif sudo -n true 2>/dev/null; then SUDO="sudo -n"
else echo "  skip - needs root or passwordless sudo"; exit 0
fi
for bin in ip nft python3; do
    if ! PATH="$PATH:/usr/sbin:/sbin" command -v "$bin" > /dev/null 2>&1; then
        echo "  skip - $bin not installed"; exit 0
    fi
done

TMP=$(mktemp -d)
S="fwct-s-$$"
C="fwct-c-$$"
cleanup() {
    $SUDO ip netns del "$S" 2> /dev/null || true
    $SUDO ip netns del "$C" 2> /dev/null || true
    rm -rf "$TMP"
}
trap cleanup EXIT

fails=0
pass() { echo "  ok   - $1"; }
fail() { echo "  FAIL - $1"; fails=$((fails + 1)); }

# heredoc <tag> — the body of setup.sh's quoted heredoc named <tag>, with the
# firewall directory moved under $TMP.
heredoc() {
    sed -n "/<< '$1'\$/,/^$1\$/p" "$SETUP" | sed '1d;$d' | sed "s#/etc/spinifex/firewall#$TMP/fw#g"
}

mkdir -p "$TMP/fw" "$TMP/bin"
heredoc RULES > "$TMP/fw/spinifex.nft"
heredoc APPLY > "$TMP/apply"
heredoc TRACK > "$TMP/fw/conntrack.nft"
printf 'define spinifex_ssh_ports = { 22 }\n' > "$TMP/fw/local.nft"
printf 'define spinifex_open_ports = { 0 }\n' > "$TMP/fw/open-ports.nft"
printf 'define trusted_ssh_peers = { 0.0.0.0/0 }\n' > "$TMP/fw/custom.nft"
printf '#!/bin/sh\nexit 0\n' > "$TMP/bin/systemctl"
chmod +x "$TMP/bin/systemctl"
if [ ! -s "$TMP/fw/spinifex.nft" ] || [ ! -s "$TMP/apply" ]; then
    echo "  FAIL - could not cut the policy or the helper out of setup.sh"; exit 1
fi

# One process owns both ends, so the order packets are sent in is exact. The
# server is 10.99.0.1 (a peer) and also holds 10.99.0.9 (not a peer).
cat > "$TMP/flow.py" << 'PY'
import os, socket, subprocess, sys

srv_ns, cli_ns, script = sys.argv[1], sys.argv[2], sys.argv[3]

def enter(ns):
    fd = os.open("/run/netns/" + ns, os.O_RDONLY)
    os.setns(fd, os.CLONE_NEWNET)
    os.close(fd)

enter(srv_ns)
listener = socket.socket()
listener.setsockopt(socket.SOL_SOCKET, socket.SO_REUSEADDR, 1)
listener.bind(("10.99.0.1", 6644))
listener.listen(1)
enter(cli_ns)

cli = srv = None
for step in script.split("@@"):
    if step == "connect":
        cli = socket.create_connection(("10.99.0.1", 6644), timeout=3)
        srv, _ = listener.accept()
        cli.settimeout(2)
        srv.settimeout(2)
    elif step in ("s2c", "c2s"):
        tx, rx = (srv, cli) if step == "s2c" else (cli, srv)
        tx.sendall(b"x" * 300)
        got = b""
        try:
            while len(got) < 300:
                got += rx.recv(300 - len(got))
        except socket.timeout:
            print("lost " + step)
            sys.exit(1)
    else:
        subprocess.run(step, shell=True, check=True)
PY

# dial <client source> <server addr> <port> — a new connection from the server
# namespace to the client, which listens on the port. 0 when it connects.
cat > "$TMP/dial.py" << 'PY'
import os, socket, sys

srv_ns, cli_ns, src, port = sys.argv[1], sys.argv[2], sys.argv[3], int(sys.argv[4])
fd = os.open("/run/netns/" + cli_ns, os.O_RDONLY)
os.setns(fd, os.CLONE_NEWNET)
listener = socket.socket()
listener.bind(("10.99.0.3", port))
listener.listen(1)
fd = os.open("/run/netns/" + srv_ns, os.O_RDONLY)
os.setns(fd, os.CLONE_NEWNET)
dialer = socket.socket()
dialer.bind((src, 0))
dialer.settimeout(2)
try:
    dialer.connect(("10.99.0.3", port))
except OSError:
    sys.exit(1)
PY

# A new pair of namespaces, and no peer file: every case starts disarmed.
fresh() {
    $SUDO rm -f "$TMP/fw/peers.nft"
    $SUDO ip netns del "$S" 2> /dev/null || true
    $SUDO ip netns del "$C" 2> /dev/null || true
    $SUDO ip netns add "$S"
    $SUDO ip netns add "$C"
    $SUDO ip link add vs netns "$S" type veth peer name vc netns "$C"
    $SUDO ip -n "$S" addr add 10.99.0.1/24 dev vs
    $SUDO ip -n "$S" addr add 10.99.0.9/24 dev vs
    $SUDO ip -n "$C" addr add 10.99.0.3/24 dev vc
    for l in "$S vs" "$C vc"; do
        # shellcheck disable=SC2086  # "<netns> <link>"
        set -- $l
        $SUDO ip -n "$1" link set lo up
        $SUDO ip -n "$1" link set "$2" up
    done
}

# Helper verbs and raw nft, run as root inside the client namespace.
in_c="$SUDO env PATH=$TMP/bin:$PATH:/usr/sbin:/sbin ip netns exec $C"
verb() { echo "$in_c sh $TMP/apply $*"; }
arm="$(verb set-peers) < $TMP/peers"
printf '10.99.0.1\n10.99.0.1\n' > "$TMP/peers"

# flow <desc> <steps> — passes when every transfer is delivered.
flow() {
    if out=$($SUDO python3 "$TMP/flow.py" "$S" "$C" "$2" 2>&1); then pass "$1"; else fail "$1: $out"; fi
}

# Control first: with nothing tracking, arming under an open flow must drop the
# server's next segment, or this kernel cannot show the failure and the cases
# below prove nothing.
fresh
if $SUDO python3 "$TMP/flow.py" "$S" "$C" "connect@@s2c@@c2s@@$arm@@s2c" > /dev/null 2>&1; then
    fail "control: a flow opened with no conntrack user survived the arm, so this kernel cannot reproduce the drop"
else
    pass "control: a flow opened with no conntrack user loses the server's next segment once armed"
fi

fresh
flow "a flow opened after disable survives set-peers, server speaking first" \
    "$(verb disable)@@connect@@s2c@@c2s@@$arm@@s2c@@c2s"

fresh
flow "a flow opened after boot with no peer file survives set-peers, server speaking first" \
    "$(verb boot)@@connect@@s2c@@c2s@@$arm@@s2c@@c2s"

fresh
flow "a flow opened while armed survives disable and a re-arm, server speaking first" \
    "$arm@@connect@@s2c@@c2s@@$(verb disable)@@s2c@@c2s@@s2c@@$arm@@s2c@@c2s"

fresh
flow "a flow opened before track survives set-peers once it has carried traffic" \
    "connect@@s2c@@c2s@@$(verb track)@@s2c@@c2s@@$arm@@s2c@@c2s"

# The tracking table must not open anything the policy drops.
fresh
sh -c "$(verb boot) && $arm"
if $SUDO python3 "$TMP/dial.py" "$S" "$C" 10.99.0.1 6644; then
    pass "armed: a peer still reaches a cluster port"
else
    fail "armed: a peer cannot reach a cluster port"
fi
if $SUDO python3 "$TMP/dial.py" "$S" "$C" 10.99.0.9 6644; then
    fail "armed: a non-peer reaches a cluster port"
else
    pass "armed: a non-peer is dropped on a cluster port"
fi
if $SUDO python3 "$TMP/dial.py" "$S" "$C" 10.99.0.1 7777; then
    fail "armed: a peer reaches a port the policy does not open"
else
    pass "armed: a peer is dropped on a port the policy does not open"
fi

# disable leaves only the tracking table, which accepts everything.
sh -c "$(verb disable)"
tables=$($in_c nft list tables | tr '\n' ' ')
case "$tables" in
    *spinifex_filter*) fail "disable left inet spinifex_filter loaded: $tables" ;;
    *spinifex_conntrack*) pass "disable removes the policy and keeps the tracking table" ;;
    *) fail "disable removed the tracking table: '$tables'" ;;
esac
if $SUDO python3 "$TMP/dial.py" "$S" "$C" 10.99.0.9 7777; then
    pass "disarmed: the tracking table drops nothing"
else
    fail "disarmed: the tracking table dropped a connection"
fi

if [ "$fails" -gt 0 ]; then
    echo "  $fails check(s) failed"
    exit 1
fi
echo "  setup_firewall_conntrack ok"
