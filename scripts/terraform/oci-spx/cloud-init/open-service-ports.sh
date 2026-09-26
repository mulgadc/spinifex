#!/bin/bash
# Opens what a Spinifex node serves: every protocol to its cluster peers, and the
# listed TCP ports to everyone else. The Ubuntu OCI image ships an INPUT chain
# that ends in REJECT with only 22, ICMP, lo and ESTABLISHED above it, so the
# console, the predastore gate and the AWS gateway are closed by default.
#
# Usage: spinifex-open-service-ports <peer-cidr> <port> [port...]
set -euo pipefail

log() { printf '[spinifex-service-ports] %s\n' "$*"; }
die() {
    log "ERROR: $*"
    exit 1
}

[ "$#" -gt 1 ] || die "usage: $(basename "$0") <peer-cidr> <port> [port...]"

PEER_CIDR="$1"
shift

# Never flush. The image's InstanceServices chain in OUTPUT is what permits iSCSI
# to 169.254.2.0/24:3260 and the metadata service; clearing it takes the data
# volume and IMDS with it. FORWARD is left alone too -- Spinifex manages it.
insert_position() {
    iptables -L INPUT --line-numbers -n \
        | awk '$2 == "REJECT" || $2 == "DROP" { print $1; exit }'
}

open_port() {
    local port="$1" pos
    if iptables -C INPUT -p tcp -m state --state NEW -m tcp --dport "$port" -j ACCEPT 2>/dev/null; then
        log "port $port is already open"
        return 0
    fi

    pos="$(insert_position)"
    if [ -n "$pos" ]; then
        # Appending would land after the REJECT, where the rule can never match.
        iptables -I INPUT "$pos" -p tcp -m state --state NEW -m tcp --dport "$port" -j ACCEPT
        log "opened port $port at INPUT position $pos"
    else
        iptables -A INPUT -p tcp -m state --state NEW -m tcp --dport "$port" -j ACCEPT
        log "opened port $port (appended; INPUT has no terminal REJECT)"
    fi
}

# A cluster talks on far more than the ports customers reach: the formation
# server, OVN's NB/SB and raft, NATS, predastore, viperblock and Geneve. Listing
# them would be a list to keep in step with the code, and the subnet holds only
# our own nodes, so the peer CIDR is the boundary -- exactly as the OCI security
# list treats it. Narrowing the internet rule can then never cut the cluster.
open_peers() {
    if iptables -C INPUT -s "$PEER_CIDR" -j ACCEPT 2>/dev/null; then
        log "peer CIDR $PEER_CIDR is already open"
        return 0
    fi
    iptables -I INPUT 1 -s "$PEER_CIDR" -m comment --comment spinifex-peers -j ACCEPT
    log "opened all protocols from $PEER_CIDR at INPUT position 1"
}

main() {
    local port
    open_peers
    for port in "$@"; do
        open_port "$port"
    done

    netfilter-persistent save >/dev/null || die "netfilter-persistent save failed; rules would not survive a reboot"
    log "saved to /etc/iptables/rules.v4"
}

main "$@"
