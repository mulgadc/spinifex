#!/usr/bin/env bash
# install-node.sh — install Spinifex on one latitude.sh bare-metal host. Run as root ON the host.
#
#   install-node.sh --phase install --eip <eip> --data-uuid <uuid>   host prep, mount guard, packages
#   install-node.sh --phase ovn     --eip <eip> --data-uuid <uuid>   setup-ovn.sh alone, routes checked
#   install-node.sh --phase start   --eip <eip> --data-uuid <uuid>   init, config, start, public subnets
#   install-node.sh --phase firewall --eip <eip> --data-uuid <uuid> arm the host firewall, on a typed yes
#   install-node.sh --phase verify  --eip <eip> --data-uuid <uuid>   read-only checks, for after a reboot
#   (--phase firewall-on and firewall-off are retired: use firewall; no disarm phase is reviewed)
#   add --dry-run to any phase to print the plan and read-only checks without changing anything
#
# This is the on-host half of vultr-spx/deploy-vultr.sh for a single node: no Terraform
# and no SSH, because the person runs it by hand on the server. Each step logs to
# /root/<step>.log and prints one OK or FAIL line. A step is recorded in /root/.latitude-install
# and a rerun skips it only while a live check of its result still holds. The inputs are
# recorded too, and a rerun with different inputs is refused.
#
# Nothing is installed unless /var/lib/spinifex is a mount whose filesystem UUID is --data-uuid,
# and every Spinifex unit that writes there gets a drop-in that requires the mount and
# re-checks the UUID before the service starts. The firewall is never armed by the
# install, ovn or start phases; only --phase firewall arms it, after a typed yes.

set -euo pipefail

PHASE=""
EIP=""
DATA_UUID=""
CHANNEL="latest"
VERSION=""
DISTRO=""
SETUP_SCRIPT=""
IFACE=""
NODE_NAME="node1"
SPX_REGION="us-east-1"
SPX_AZ="us-east-1a"
IMAGE=""
POOL_NAME="latitude-eip"
ARMED=0
DRY=0
BASE_URL="${INSTALL_BASE_URL:-https://install.mulgadc.com}"
DATA_MP="/var/lib/spinifex"
STATE_DIR="/root/.latitude-install"
PIN_FILE="/root/latitude-install-pin.txt"
SHARE="/usr/local/share/spinifex"
CHECK_BIN="/usr/local/lib/spinifex/latitude-check-data-mount"
TOML="/etc/spinifex/spinifex.toml"
FW_DIR="/etc/spinifex/firewall"
FW_MODE="$FW_DIR/mode"
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
HOST_IP=""
GATEWAY=""

# The units that write under /var/lib/spinifex, read from build/systemd: each names the
# directory in ReadWritePaths or Environment, or runs a script kept there.
# Not guarded, because their unit files never name it: spinifex-hostnet,
# spinifex-firewall and spinifex-shutdown.
GUARD_UNITS=(
    spinifex-awsgw spinifex-daemon spinifex-nats spinifex-nats-watchdog spinifex-northstar
    spinifex-predastore spinifex-qmp-collector spinifex-ui spinifex-viperblock spinifex-vpcd
)

usage() {
    sed -n '2,10p' "$0" | sed 's/^# \{0,1\}//'
    cat <<EOF

Options:
  --phase NAME         install, ovn, start, firewall or verify (required)
  --eip IP             the Elastic IP routed to this host
  --data-uuid UUID     filesystem UUID that must be mounted at $DATA_MP
  --channel CHANNEL    release channel: latest (default) or dev
  --version TAG        install this release instead of a channel
  --distro TARBALL     install this local release tarball instead of downloading
  --setup-script PATH  setup.sh to run with --distro (default: scripts/setup.sh of this checkout)
  --iface NAME         public NIC (default: the NIC of the default route)
  --node-name NAME     Spinifex node name (default $NODE_NAME)
  --spx-region NAME    Spinifex region (default $SPX_REGION)
  --spx-az NAME        Spinifex availability zone (default $SPX_AZ)
  --image NAME         start phase: also import this AMI, such as debian-13-x86_64
  --armed              start phase: the firewall was armed before this start (mode on); the daemon loads the policy
  --pool-name NAME     firewall, verify: the pool name to look for (default $POOL_NAME)
  --dry-run           print the plan and the read-only checks, change nothing
EOF
}

while [ $# -gt 0 ]; do
    case "$1" in
        --phase) PHASE="${2:?--phase needs a value}"; shift ;;
        --eip) EIP="${2:?--eip needs a value}"; shift ;;
        --data-uuid) DATA_UUID="${2:?--data-uuid needs a value}"; shift ;;
        --channel) CHANNEL="${2:?--channel needs a value}"; shift ;;
        --version) VERSION="${2:?--version needs a value}"; shift ;;
        --distro) DISTRO="${2:?--distro needs a value}"; shift ;;
        --setup-script) SETUP_SCRIPT="${2:?--setup-script needs a value}"; shift ;;
        --iface) IFACE="${2:?--iface needs a value}"; shift ;;
        --node-name) NODE_NAME="${2:?--node-name needs a value}"; shift ;;
        --spx-region) SPX_REGION="${2:?--spx-region needs a value}"; shift ;;
        --spx-az) SPX_AZ="${2:?--spx-az needs a value}"; shift ;;
        --image) IMAGE="${2:?--image needs a value}"; shift ;;
        --armed) ARMED=1 ;;
        --pool-name) POOL_NAME="${2:?--pool-name needs a value}"; shift ;;
        --dry-run) DRY=1 ;;
        -h|--help) usage; exit 0 ;;
        *) echo "unknown option: $1" >&2; usage >&2; exit 2 ;;
    esac
    shift
done

# Step output goes to its log; fd 3 is the console for lines the person must see.
exec 3>&2
# redact rewrites this host's public addresses, which the person pastes back, as placeholders.
# The step logs under /root keep the real values.
redact() {
    local pair v e=(-E)
    for pair in "$HOST_IP=<node-ip>" "$GATEWAY=<gw>" "$EIP=<eip>"; do
        v="${pair%%=*}"
        if [ -n "$v" ]; then e+=(-e "s/${v//./\\.}/${pair#*=}/g"); fi
    done
    e+=(-e 's/\b[23][0-9a-f]{3}:[0-9a-f:]+/<ipv6>/g')
    sed "${e[@]}"
}
log() { printf '[install %s] %s\n' "$(date -u +%H:%M:%S)" "$*" | redact >&3; }
die() { log "ERROR: $*"; exit 1; }
say() { printf '%s\n' "$*" | redact >&3; }
# check_fail stops a real run and only reports in --dry-run, so a dry run lists every problem.
PREFLIGHT_FAILS=0
check_fail() {
    PREFLIGHT_FAILS=$((PREFLIGHT_FAILS + 1))
    if [ "$DRY" = 1 ]; then log "WOULD FAIL: $*"; else die "$*"; fi
}

case "$PHASE" in
    install|ovn|start|firewall|verify) ;;
    firewall-on|firewall-off) echo "--phase $PHASE is retired: use --phase firewall to arm; no disarm phase is reviewed yet" >&2; exit 2 ;;
    *) echo "--phase must be install, ovn, start, firewall or verify" >&2; usage >&2; exit 2 ;;
esac
case "$PHASE" in
    install|ovn|start|firewall|verify)
        [ -n "$EIP" ] || { echo "--eip is required for --phase $PHASE" >&2; exit 2; }
        [ -n "$DATA_UUID" ] || { echo "--data-uuid is required for --phase $PHASE" >&2; exit 2; }
        [[ "$EIP" =~ ^([0-9]{1,3}\.){3}[0-9]{1,3}$ ]] || die "--eip $EIP is not an IPv4 address"
        [[ "$DATA_UUID" =~ ^[0-9a-fA-F-]{16,36}$ ]] || die "--data-uuid $DATA_UUID does not look like a filesystem UUID"
        [[ "$NODE_NAME" =~ ^[a-z0-9][a-z0-9-]{0,23}$ ]] || die "--node-name must be 1-24 lowercase letters, digits or hyphens"
        [ -z "$DISTRO" ] || [ "$DRY" = 1 ] || [ -r "$DISTRO" ] || die "--distro $DISTRO not readable" ;;
esac

done_step() { [ -f "$STATE_DIR/step.$1" ]; }
mark_step() { date -u +%FT%TZ > "$STATE_DIR/step.$1"; }

# run_step NAME DESCRIPTION FN [LIVE] runs FN in a subshell under set -e with its output in
# /root/NAME.log. A recorded step is skipped only while LIVE, a check of its actual result,
# still passes. The subshell is not a condition of an if or ||, which would switch errexit
# off inside it.
run_step() {
    local name="$1" desc="$2" fn="$3" live="${4:-}" rc=0
    if [ "$DRY" = 1 ]; then
        local note=""
        if done_step "$name"; then note=" (marked done $(cat "$STATE_DIR/step.$name"))"; fi
        printf 'PLAN  %-9s %s%s\n' "$name" "$desc" "$note" | redact
        return 0
    fi
    if done_step "$name"; then
        if [ -z "$live" ] || "$live" >/dev/null 2>&1; then
            printf 'SKIP  %-9s done %s, live check holds\n' "$name" "$(cat "$STATE_DIR/step.$name")"
            return 0
        fi
        printf 'STALE %-9s marked done but the live check failed; running again\n' "$name"
    fi
    printf 'RUN   %-9s %s\n' "$name" "$desc" | redact
    set +e
    ( set -euo pipefail; "$fn" ) >"/root/$name.log" 2>&1
    rc=$?
    set -e
    if [ "$rc" -ne 0 ]; then
        printf 'FAIL  %-9s exit %s, full log /root/%s.log\n' "$name" "$rc" "$name"
        tail -n 20 "/root/$name.log" | sed 's/^/      | /' | redact
        exit 1
    fi
    mark_step "$name"
    printf 'OK    %-9s\n' "$name"
}

# run_always is run_step for work that is safe to repeat and has no marker.
run_always() {
    local name="$1" desc="$2" fn="$3" rc=0
    if [ "$DRY" = 1 ]; then printf 'PLAN  %-9s %s\n' "$name" "$desc" | redact; return 0; fi
    printf 'RUN   %-9s %s\n' "$name" "$desc" | redact
    set +e
    ( set -euo pipefail; "$fn" ) >"/root/$name.log" 2>&1
    rc=$?
    set -e
    if [ "$rc" -ne 0 ]; then
        printf 'FAIL  %-9s exit %s, full log /root/%s.log\n' "$name" "$rc" "$name"
        tail -n 20 "/root/$name.log" | sed 's/^/      | /' | redact
        exit 1
    fi
    printf 'OK    %-9s\n' "$name"
}

# require_done STEP LIVE refuses a later phase unless an earlier step is marked and still true.
require_done() {
    local step="$1" live="$2"
    if ! done_step "$step"; then check_fail "step '$step' is not done; run the earlier phase first"; return 0; fi
    if ! "$live" >/dev/null 2>&1; then check_fail "step '$step' is marked done but its live check fails; rerun the earlier phase"; fi
}

# ── common checks ────────────────────────────────────────────────────────────

# rp_filter_effective prints what the kernel applies to IFACE: the larger of the
# "all" and per-interface values, where 1 is strict reverse-path filtering.
rp_filter_effective() {
    local a i
    a="$(cat /proc/sys/net/ipv4/conf/all/rp_filter)" || return 1
    i="$(cat "/proc/sys/net/ipv4/conf/$IFACE/rp_filter")" || return 1
    echo $(( a > i ? a : i ))
}

check_rp_filter() {
    local rp
    rp="$(rp_filter_effective)" || { check_fail "cannot read rp_filter for $IFACE under /proc/sys/net/ipv4/conf"; return 0; }
    if [ "$rp" = 1 ]; then
        check_fail "rp_filter is strict on $IFACE (all=$(cat /proc/sys/net/ipv4/conf/all/rp_filter) $IFACE=$(cat "/proc/sys/net/ipv4/conf/$IFACE/rp_filter")); an Elastic IP arrives from the gateway but guest replies leave by another path. Nothing was changed; decide how to relax it (net.ipv4.conf.all.rp_filter and net.ipv4.conf.$IFACE.rp_filter to 2) and rerun"
    else
        log "rp_filter on $IFACE is $rp (not strict)"
    fi
}

# detect_iface sets IFACE, HOST_IP and GATEWAY from the default route, for the messages
# of every phase, and returns non-zero when any is missing.
detect_iface() {
    if [ -z "$IFACE" ]; then
        IFACE="$(ip -4 route show default | awk '{for (i = 1; i < NF; i++) if ($i == "dev") {print $(i + 1); exit}}')"
    fi
    [ -n "$IFACE" ] || return 1
    HOST_IP="$(ip -4 -o addr show dev "$IFACE" scope global 2>/dev/null | awk '{sub(/\/.*/, "", $4); print $4}')" || true
    GATEWAY="$(ip -4 route show default dev "$IFACE" 2>/dev/null | awk '{for (i = 1; i < NF; i++) if ($i == "via") {print $(i + 1); exit}}')" || true
    [ -n "$HOST_IP" ] && [ -n "$GATEWAY" ]
}

# inputs_line is every input a rerun must repeat; the image is not one, because
# importing another image later is legitimate.
inputs_line() {
    local dsha=""
    if [ -n "$DISTRO" ] && [ -r "$DISTRO" ]; then dsha="$(sha256sum "$DISTRO" | awk '{print $1}')"; fi
    printf 'eip=%s uuid=%s channel=%s version=%s distro_sha256=%s iface=%s node=%s region=%s az=%s' \
        "$EIP" "${DATA_UUID,,}" "$CHANNEL" "$VERSION" "$dsha" "$IFACE" "$NODE_NAME" "$SPX_REGION" "$SPX_AZ"
}

check_inputs() {
    local now rec
    now="$(inputs_line)"
    if [ -f "$STATE_DIR/inputs" ]; then
        rec="$(cat "$STATE_DIR/inputs")"
        if [ "$rec" != "$now" ]; then
            check_fail "inputs differ from the first run. recorded: $rec | now: $now | use the same inputs, or remove $STATE_DIR on a host you are sure is clean"
        else
            log "inputs match the first run"
        fi
    elif [ "$PHASE" = verify ] || [ "$PHASE" = firewall ]; then
        check_fail "no recorded inputs in $STATE_DIR: the install phases have not run on this host"
    elif [ "$DRY" != 1 ]; then
        install -d -m 0700 "$STATE_DIR"
        printf '%s\n' "$now" > "$STATE_DIR/inputs"
        log "recorded inputs: $now"
    fi
}

preflight() {
    log "preflight (reads only)"
    if [ "$(id -u)" != 0 ]; then check_fail "run as root"; fi
    if [ -r /etc/os-release ]; then
        # shellcheck disable=SC1091
        log "OS: $(. /etc/os-release && echo "$PRETTY_NAME")"
    else
        check_fail "no /etc/os-release"
    fi
    [ -e /dev/kvm ] || check_fail "no /dev/kvm: virtualisation is off or the plan is not bare metal"

    # The data mount is proven first: nothing below may write to the root disk by accident.
    local got
    if got="$(findmnt --mountpoint "$DATA_MP" -no UUID 2>/dev/null)" && [ -n "$got" ]; then
        if [ "${got,,}" = "${DATA_UUID,,}" ]; then
            log "$DATA_MP is mounted from UUID $got"
        else
            check_fail "$DATA_MP is mounted from UUID $got, expected $DATA_UUID"
        fi
    else
        check_fail "$DATA_MP is not a mount point; mount the data disk first (refusing to install onto the root disk)"
    fi

    if ! detect_iface; then
        check_fail "need a default route via a gateway on one NIC with a global IPv4 address (iface='${IFACE:-}' address='${HOST_IP:-}' gateway='${GATEWAY:-}')"
    elif [ "$(printf '%s\n' "$HOST_IP" | grep -c .)" != 1 ]; then
        check_fail "expected exactly one global IPv4 address on $IFACE, found: $HOST_IP"
    fi
    [ -n "$IFACE" ] || IFACE=eno1
    log "public NIC $IFACE address ${HOST_IP:-?} gateway ${GATEWAY:-?} elastic ip $EIP"

    if ip -4 -o addr show | awk '{sub(/\/.*/, "", $4); print $4}' | grep -qx "$EIP"; then
        check_fail "--eip $EIP is configured on a host interface; an Elastic IP is routed to the host and must not be an address on it (remove it from lo or $IFACE)"
    fi
    [ "$EIP" != "$HOST_IP" ] || check_fail "--eip is the host's own address"

    check_rp_filter
    log "ip_forward is $(cat /proc/sys/net/ipv4/ip_forward) now (must be 1 when the start phase finishes)"

    if [ ! -L /etc/resolv.conf ] && systemctl is-active -q systemd-resolved; then
        log "WARN: /etc/resolv.conf is not a symlink to systemd-resolved; Spinifex split DNS for load balancers and EKS names will be bypassed (not changed)"
    fi
    if [ "$PHASE" = install ]; then
        if [ -n "$DISTRO" ] && [ -z "$SETUP_SCRIPT" ]; then SETUP_SCRIPT="$SCRIPT_DIR/../../setup.sh"; fi
        if [ -n "$DISTRO" ] && [ ! -r "$SETUP_SCRIPT" ]; then check_fail "--distro needs a readable setup.sh (--setup-script), none at $SETUP_SCRIPT"; fi
        if [ -z "$DISTRO" ]; then command -v curl >/dev/null || check_fail "curl is not installed"; fi
        command -v python3 >/dev/null || check_fail "python3 is not installed"
    fi
    check_inputs
    if [ "$DRY" = 1 ]; then log "preflight: $PREFLIGHT_FAILS problem(s)"; else log "preflight passed"; fi
}

# step_fwcheck fails unless the mode file says off and a successful nft inventory does not list the
# policy table. A failing nft is never read as "absent", whatever its message or locale.
step_fwcheck() {
    local mode tables
    mode="$(cat "$FW_MODE" 2>/dev/null)" || mode=missing
    [ "$mode" = off ] || { echo "firewall mode file is '$mode', expected off" >&2; exit 1; }
    command -v nft >/dev/null || { echo "nft is not installed, so the policy table cannot be checked" >&2; exit 1; }
    tables="$(nft list tables)" || { echo "nft list tables failed, so absence of the policy table is not proven" >&2; exit 1; }
    if grep -Fxq 'table inet spinifex_filter' <<<"$tables"; then
        echo "table inet spinifex_filter is loaded" >&2
        exit 1
    fi
    echo "mode off; nft list tables succeeded and does not list table inet spinifex_filter"
}

# ── live checks: what each recorded step must still be true of ────────────────

guard_dropin() { echo "/etc/systemd/system/$1.service.d/10-latitude-data-mount.conf"; }

live_prep() {
    local unit
    for unit in apt-daily.timer apt-daily-upgrade.timer; do
        if systemctl is-enabled -q "$unit" 2>/dev/null; then return 1; fi
    done
    command -v git >/dev/null
}
live_guard() {
    local u
    [ -x "$CHECK_BIN" ] || return 1
    for u in "${GUARD_UNITS[@]}"; do [ -f "$(guard_dropin "$u")" ] || return 1; done
    "$CHECK_BIN"
}
live_install() { [ -x /usr/local/bin/spx ] && [ -x "$SHARE/setup.sh" ] && [ -x "$SHARE/setup-ovn.sh" ] && live_guard; }
live_init() { [ -r "$TOML" ]; }
# validate_config parses spinifex.toml and exits non-zero naming every difference from what this
# script writes: both IMDS values, exactly one latitude-eip pool (static, no prefix_len, range and
# gateway as wanted, gateway_ip the reserved placeholder) and external_mode nat.
validate_config() {
    python3 - "$TOML" "$EIP" "$GATEWAY" "$POOL_NAME" <<'PY'
import sys, tomllib
path, eip, gateway, pool_name = sys.argv[1:]
net = tomllib.load(open(path, "rb")).get("network", {})
bad = []
if net.get("imds_host_meta_ip") != "169.254.42.254": bad.append("imds_host_meta_ip is %r" % net.get("imds_host_meta_ip"))
if net.get("imds_host_dns_ip") != "169.254.42.253": bad.append("imds_host_dns_ip is %r" % net.get("imds_host_dns_ip"))
if net.get("external_mode") != "nat": bad.append("external_mode is %r, not 'nat'" % net.get("external_mode"))
pools = [p for p in net.get("external_pools", []) if p.get("name") == pool_name]
if len(pools) != 1:
    bad.append("expected one %s pool, found %d" % (pool_name, len(pools)))
else:
    p = pools[0]
    # gateway_ip: nat mode reserves it for a router address that comes from nat-transit, and unset it takes the pool's only address.
    for key, want in (("source", "static"), ("range_start", eip), ("range_end", eip), ("gateway", gateway), ("gateway_ip", "192.0.2.1")):
        if p.get(key) != want: bad.append("pool %s is %r, expected %r" % (key, p.get(key), want))
    if "prefix_len" in p: bad.append("pool has prefix_len %r, which must be unset" % p["prefix_len"])
if bad:
    sys.exit("spinifex.toml does not match: " + "; ".join(bad))
print("spinifex.toml matches: imds remap, %s pool with gateway_ip, external_mode nat" % pool_name)
PY
}
live_config() { validate_config; }
live_ovn_state() { ovn_verify; }

# ── phase: install ───────────────────────────────────────────────────────────

step_prep() {
    # apt-daily would hold the dpkg lock and unattended upgrades could restart spinifex-daemon.
    local unit
    for unit in apt-daily.timer apt-daily-upgrade.timer unattended-upgrades.service; do
        if systemctl list-unit-files "$unit" --no-legend | grep -q .; then
            systemctl disable --now "$unit"
        else
            echo "$unit is not installed"
        fi
    done
    if systemctl is-enabled apt-daily.timer apt-daily-upgrade.timer; then
        echo "an apt timer is still enabled" >&2
        exit 1
    fi
    echo "apt timers disabled"
    # The latitude.sh Ubuntu image ships no git, which the published Terraform workbooks need.
    if command -v git >/dev/null; then
        echo "git already installed"
    else
        # A fresh image has no apt package lists, so git has no installation candidate until an update.
        DEBIAN_FRONTEND=noninteractive apt-get update
        DEBIAN_FRONTEND=noninteractive apt-get install -y git
        command -v git >/dev/null
    fi
}

# step_guard runs before setup.sh: a drop-in for a unit that does not exist yet is
# read when the unit appears, so no service can start without it.
step_guard() {
    local u
    install -d -m 0755 "$(dirname "$CHECK_BIN")"
    cat > "$CHECK_BIN" <<EOF
#!/bin/sh
# Written by install-node.sh: refuse to start while $DATA_MP is not the expected filesystem.
want="$DATA_UUID"
got=\$(findmnt --mountpoint "$DATA_MP" -no UUID) || { echo "FATAL: $DATA_MP is not a mount point" >&2; exit 1; }
[ "\$got" = "\$want" ] || { echo "FATAL: $DATA_MP is UUID \$got, expected \$want" >&2; exit 1; }
EOF
    chmod 0755 "$CHECK_BIN"
    "$CHECK_BIN"
    for u in "${GUARD_UNITS[@]}"; do
        install -d -m 0755 "/etc/systemd/system/$u.service.d"
        # ExecStartPre, not ExecCondition: a wrong disk must show as a failed start, not a quiet skip.
        cat > "$(guard_dropin "$u")" <<EOF
[Unit]
RequiresMountsFor=$DATA_MP

[Service]
ExecStartPre=$CHECK_BIN
EOF
        echo "guarded $u"
    done
    systemctl daemon-reload
    echo "${#GUARD_UNITS[@]} units guarded"
}

# unit_audit fails when an installed Spinifex unit names the data directory and has no
# drop-in, which means the unit layout changed after this script was written.
unit_audit() {
    local unit name frag bad=0
    for unit in $(systemctl list-unit-files 'spinifex*.service' --no-legend | awk '{print $1}'); do
        name="${unit%.service}"
        frag="$(systemctl show -p FragmentPath --value "$unit")"
        [ -n "$frag" ] || continue
        if grep -q "$DATA_MP" "$frag"; then
            if [ -f "$(guard_dropin "$name")" ]; then
                echo "audit: $unit names $DATA_MP and is guarded"
            else
                echo "audit: $unit names $DATA_MP and has NO guard" >&2
                bad=1
            fi
        else
            echo "audit: $unit does not name $DATA_MP (guard is harmless there)"
        fi
    done
    [ "$bad" = 0 ]
}

step_install() {
    export INSTALL_SPINIFEX_SKIP_NEWGRP=1
    local installer sha line
    : > "$PIN_FILE"
    {
        echo "date=$(date -u +%FT%TZ)"
        echo "inputs=$(inputs_line | redact)"
    } >> "$PIN_FILE"
    if [ -n "$DISTRO" ]; then
        sha="$(sha256sum "$DISTRO" | awk '{print $1}')"
        echo "tarball=$DISTRO sha256=$sha" | tee -a "$PIN_FILE"
        INSTALL_SPINIFEX_TARBALL="$DISTRO" bash "$SETUP_SCRIPT" --firewall off
        # A tarball built by build-install-artifacts.sh ships setup.sh beside it,
        # and the later steps run the installed copy.
        test -x "$SHARE/setup.sh" || install -D -m 0755 "$SETUP_SCRIPT" "$SHARE/setup.sh"
    else
        installer="$STATE_DIR/installer-setup.sh"
        curl -fsSL -o "$installer" "$BASE_URL"
        echo "installer=$BASE_URL sha256=$(sha256sum "$installer" | awk '{print $1}')" | tee -a "$PIN_FILE"
        if [ -n "$VERSION" ]; then
            bash "$installer" --version "$VERSION" --firewall off
        else
            bash "$installer" --channel "$CHANNEL" --firewall off
        fi
        # The tarball is downloaded and removed inside setup.sh, so its bytes are not hashed here;
        # the pin records only the checksum verification setup.sh reports, and fails without it.
        line="$(grep -a 'Checksum verified' /root/install.log | head -n1 | sed 's/\x1b\[[0-9;]*m//g')" || line=""
        [ -n "$line" ] || { echo "setup.sh did not report 'Checksum verified'; the download was not verified" >&2; exit 1; }
        echo "tarball=$BASE_URL/download/${VERSION:-$CHANNEL}/amd64 setup.sh logged: $line (tarball bytes not independently hashed)" | tee -a "$PIN_FILE"
    fi
    test -x "$SHARE/setup.sh"
    test -x "$SHARE/setup-ovn.sh"
    test -x /usr/local/bin/spx
    echo "spx_version=$(/usr/local/bin/spx version 2>&1 | head -n1)" | tee -a "$PIN_FILE"
    echo "spx_binary_sha256=$(sha256sum /usr/local/bin/spx | awk '{print $1}')" | tee -a "$PIN_FILE"
    echo "firewall_mode=$(cat "$FW_MODE" 2>/dev/null || echo missing)" | tee -a "$PIN_FILE"
    live_guard
    unit_audit
}

step_init() {
    spx admin init --node "$NODE_NAME" --nodes 1 --region "$SPX_REGION" --az "$SPX_AZ" --ipsec=false --external-mode=nat
}

step_config() {
    cp -n "$TOML" "$TOML.bak-prelatitude"
    python3 - "$TOML" "$EIP" "$GATEWAY" <<'PY'
import re, sys, tomllib
path, eip, gateway = sys.argv[1:]
text = open(path).read()
if "imds_host_meta_ip" not in text:
    # subn, not sub: with no [network] table sub would exit 0 and leave the remap unapplied.
    text, n = re.subn(r"(?m)^\[network\]$",
                      '[network]\nimds_host_meta_ip = "169.254.42.254"\nimds_host_dns_ip  = "169.254.42.253"',
                      text, count=1)
    if n != 1:
        sys.exit("no [network] table in %s, so the IMDS remap was not applied" % path)
if 'name               = "latitude-eip"' not in text:
    # prefix_len stays unset: a /32 pool with a gateway outside it is rejected otherwise.
    text = text.rstrip("\n") + (
        '\n\n[[network.external_pools]]\n'
        'name               = "latitude-eip"\n'
        'source             = "static"\n'
        'range_start        = "%s"\n'
        'range_end          = "%s"\n'
        'gateway            = "%s"\n'
        'gateway_ip         = "192.0.2.1"\n' % (eip, eip, gateway))
open(path, "w").write(text)
tomllib.load(open(path, "rb"))  # the edited file must still parse
PY
    # An existing wrong pool or IMDS value is reported, never silently kept.
    validate_config
}

PASTE_NOTE="Redact public IPs in anything you paste: this script prints <node-ip>, <gw> and <eip>, but your own ip, nft and journal output does not, and the /root/*.log files hold real addresses."

paste_install() {
    say ""
    say "$PASTE_NOTE"
    say "Paste back: the lines above (RUN/OK/FAIL), then the output of:"
    say "  cat $PIN_FILE"
    say "  cat $FW_MODE   (must print: off)"
    say "  systemctl cat spinifex-daemon.service | grep -n -A3 latitude   (shows the guard drop-in)"
    say "Next: ./install-node.sh --phase ovn with the same --eip and --data-uuid. Keep the out-of-band serial console open for it."
}

phase_install() {
    run_step prep "disable apt-daily, apt-daily-upgrade and unattended-upgrades; apt-get update and install git if missing" step_prep live_prep
    run_step guard "drop-ins for ${#GUARD_UNITS[@]} spinifex units: RequiresMountsFor + ExecStartPre UUID check ($DATA_UUID)" step_guard live_guard
    local src="channel $CHANNEL"
    if [ -n "$VERSION" ]; then src="version $VERSION"; fi
    if [ -n "$DISTRO" ]; then src="tarball $DISTRO"; fi
    run_step install "setup.sh --firewall off from $src; pin the verification setup.sh logged; audit units" step_install live_install
    run_always fwcheck "firewall mode is off and nft list tables (rc 0) does not list table inet spinifex_filter" step_fwcheck
    say "NOTE  guard: RequiresMountsFor only proves that something is mounted at $DATA_MP, and systemd checks it before any ExecStartPre. The UUID check proves it is the right disk, and runs after a unit's own ExecStartPre lines."
    if [ "$DRY" != 1 ]; then paste_install; fi
}

# ── phase: ovn ───────────────────────────────────────────────────────────────

net_snapshot() {
    echo "== ip -4 route ($1)"
    ip -4 route
    echo "== ip -br addr ($1)"
    ip -br addr
}

OVN_BASE_DEF="$STATE_DIR/ovn-baseline.default"
OVN_BASE_CONN="$STATE_DIR/ovn-baseline.connected"
OVN_LOG="/root/ovn.log"

# ovn_verify prints one FAIL line per broken fact and returns non-zero if any is. It compares the
# routes with the exact lines recorded before setup-ovn ran (the connected route may gain a
# second, lower-metric copy from setup-ovn.sh step 6b, so each original line must be present,
# not the whole set equal), pings the gateway, and checks the OVN state setup-ovn leaves.
ovn_verify() {
    local bad=0 line now_def now_conn ovs_br
    if [ ! -r "$OVN_BASE_DEF" ] || [ ! -r "$OVN_BASE_CONN" ]; then
        echo "FAIL: no route baseline in $STATE_DIR, so the routes cannot be compared"
        return 1
    fi
    now_def="$(ip -4 route show default)"
    if [ "$now_def" = "$(cat "$OVN_BASE_DEF")" ]; then
        echo "ok: default route unchanged: $now_def"
    else
        echo "FAIL: default route changed. before: $(cat "$OVN_BASE_DEF") | now: ${now_def:-none}"; bad=1
    fi
    now_conn="$(ip -4 route show dev "$IFACE" proto kernel scope link)"
    while IFS= read -r line; do
        if grep -Fxq -- "$line" <<<"$now_conn"; then
            echo "ok: connected route present: $line"
        else
            echo "FAIL: connected route missing: $line | now: ${now_conn:-none}"; bad=1
        fi
    done < "$OVN_BASE_CONN"
    if ip -4 -o addr show dev "$IFACE" | grep -q "inet $HOST_IP/"; then echo "ok: $HOST_IP on $IFACE"; else echo "FAIL: $HOST_IP is no longer on $IFACE"; bad=1; fi
    if ip -4 route get "$GATEWAY" | grep -q "dev $IFACE"; then echo "ok: gateway routed via $IFACE"; else echo "FAIL: gateway $GATEWAY is not routed via $IFACE"; bad=1; fi
    if ip -4 route get 1.1.1.1 | grep -q "dev $IFACE"; then echo "ok: internet egress via $IFACE"; else echo "FAIL: internet egress does not leave through $IFACE"; bad=1; fi
    if ping -c2 -W2 "$GATEWAY" >/dev/null 2>&1; then echo "ok: gateway $GATEWAY answers ping"; else echo "FAIL: gateway $GATEWAY does not answer ping"; bad=1; fi
    if ovs-vsctl br-exists br-ext 2>/dev/null; then echo "ok: br-ext exists"; else echo "FAIL: OVS bridge br-ext is missing"; bad=1; fi
    if ip -4 -o addr show dev spx-nat-host 2>/dev/null | grep -q 'inet 100.127.0.1/24'; then echo "ok: spx-nat-host has 100.127.0.1/24"; else echo "FAIL: spx-nat-host does not have 100.127.0.1/24"; bad=1; fi
    if [ "$(cat /proc/sys/net/ipv4/ip_forward)" = 1 ]; then echo "ok: ip_forward is 1"; else echo "FAIL: ip_forward is $(cat /proc/sys/net/ipv4/ip_forward), expected 1"; bad=1; fi
    # setup-ovn.sh:1443-1445 attaches spx-nat-ovs to br-ext; :1509-1513 install the transit NAT and FORWARD rules.
    ovs_br="$(ovs-vsctl port-to-br spx-nat-ovs 2>/dev/null)" || ovs_br=""
    if [ "$ovs_br" = br-ext ]; then echo "ok: spx-nat-ovs is attached to br-ext"; else echo "FAIL: spx-nat-ovs is attached to '${ovs_br:-nothing}', expected br-ext"; bad=1; fi
    if command -v iptables >/dev/null 2>&1; then
        if iptables -t nat -C POSTROUTING -s 100.127.0.0/24 ! -d 100.127.0.0/24 -m comment --comment spinifex-nat-egress -j MASQUERADE 2>/dev/null; then
            echo "ok: transit MASQUERADE rule present"
        else
            echo "FAIL: transit MASQUERADE rule (nat POSTROUTING, comment spinifex-nat-egress) is missing"; bad=1
        fi
        if iptables -t filter -C FORWARD -i spx-nat-host -s 100.127.0.0/24 -m comment --comment spinifex-nat-egress -j ACCEPT 2>/dev/null; then
            echo "ok: transit FORWARD accept (in) present"
        else
            echo "FAIL: transit FORWARD rule for -i spx-nat-host is missing"; bad=1
        fi
        if iptables -t filter -C FORWARD -o spx-nat-host -m conntrack --ctstate RELATED,ESTABLISHED -m comment --comment spinifex-nat-egress -j ACCEPT 2>/dev/null; then
            echo "ok: transit FORWARD accept (return) present"
        else
            echo "FAIL: transit FORWARD rule for -o spx-nat-host RELATED,ESTABLISHED is missing"; bad=1
        fi
    else
        echo "FAIL: iptables is not installed, so the transit NAT rules cannot be checked"; bad=1
    fi
    return "$bad"
}

step_ovn() {
    local rc=0
    ip -4 route show default > "$OVN_BASE_DEF"
    ip -4 route show dev "$IFACE" proto kernel scope link > "$OVN_BASE_CONN"
    if [ ! -s "$OVN_BASE_DEF" ] || [ ! -s "$OVN_BASE_CONN" ]; then
        rm -f "$OVN_BASE_DEF" "$OVN_BASE_CONN"
        echo "precondition: no default route or no connected route on $IFACE before setup-ovn" >&2
        exit 1
    fi
    # The baseline test is the ping itself: a gateway that is silent now cannot be checked after.
    ping -c2 -W2 "$GATEWAY" >/dev/null 2>&1 || { rm -f "$OVN_BASE_DEF" "$OVN_BASE_CONN"; echo "precondition: the gateway does not answer ping before setup-ovn, so it cannot be a check after" >&2; exit 1; }
    net_snapshot before | redact | tee /dev/fd/3
    # setup-ovn.sh --nat-uplink bridges no NIC, so it should leave these routes alone.
    # It is not rolled back automatically: if the checks below fail, recover from the serial console.
    "$SHARE/setup-ovn.sh" --management --nat-uplink "--encap-ip=$HOST_IP"
    net_snapshot after | redact | tee /dev/fd/3
    ovn_verify 2>&1 | redact | tee /dev/fd/3 || rc=$?
    return "$rc"
}

paste_ovn() {
    say ""
    say "$PASTE_NOTE"
    say "Paste back: the RUN/OK/FAIL lines and the before/after ip -4 route and ip -br addr blocks above, then:"
    say "  ovs-vsctl show"
    say "  cat /root/ovn.log | tail -n 30"
    say "Next: ./install-node.sh --phase start with the same --eip and --data-uuid."
}

# ovn_resume_check runs the same checks on a rerun and never runs setup-ovn.sh again: whatever it
# changed has to be understood first.
ovn_resume_check() {
    local rc=0
    set +e
    ovn_verify 2>&1 | redact
    rc="${PIPESTATUS[0]}"
    set -e
    if [ "$rc" -ne 0 ]; then
        printf 'FAIL  ovn       setup-ovn.sh ran earlier but the network or OVN state no longer matches what it left\n'
        say "Not running setup-ovn.sh again. Paste back: ip -4 route; ip -br addr; ovs-vsctl show; tail -n 40 $OVN_LOG"
        say "If the host is unreachable, recover from the out-of-band serial console."
        exit 1
    fi
    printf 'OK    ovn       already done; routes, gateway and OVN state re-checked\n'
}

phase_ovn() {
    require_done install live_install
    run_always fwcheck "firewall mode is off and nft list tables (rc 0) does not list table inet spinifex_filter" step_fwcheck
    if [ "$DRY" = 1 ]; then
        printf 'PLAN  ovn       setup-ovn.sh --management --nat-uplink --encap-ip=<node-ip>; compare the exact default and connected route lines, ping <gw>, check br-ext, spx-nat-host 100.127.0.1/24 and ip_forward 1\n'
        printf 'PLAN  ovn       a rerun repeats those checks and does not run setup-ovn.sh again\n'
        if done_step ovn; then printf 'PLAN  ovn       (marked done %s)\n' "$(cat "$STATE_DIR/step.ovn")"; fi
        return 0
    fi
    if done_step ovn; then
        ovn_resume_check
    elif [ -e "$OVN_LOG" ] || ovs-vsctl br-exists br-ext 2>/dev/null; then
        printf 'FAIL  ovn       setup-ovn.sh appears to have run before (%s or br-ext exists) but did not finish cleanly\n' "$OVN_LOG"
        say "Not running it again. Paste back: tail -n 40 $OVN_LOG; ip -4 route; ip -br addr; ovs-vsctl show"
        say "If you are sure it changed nothing, remove $OVN_LOG and rerun."
        exit 1
    else
        run_step ovn "setup-ovn.sh --management --nat-uplink --encap-ip=<node-ip>, then check routes, gateway ping and OVN state" step_ovn
    fi
    paste_ovn
}

# ── phase: start ─────────────────────────────────────────────────────────────

# step_prestart refuses to continue when Spinifex was already running: its firewall and config
# state are then unknown, and this phase does not infer them.
step_prestart() {
    local u running=""
    for u in spinifex-daemon.service spinifex.target; do
        if systemctl is-active -q "$u"; then running="$running $u"; fi
    done
    if [ -n "$running" ]; then
        echo "Spinifex was already running before this phase (active:$running), so its firewall and config state are not known." >&2
        echo "Stop it with 'sudo systemctl stop spinifex.target', check 'nft list tables' and the config, then rerun." >&2
        exit 1
    fi
    echo "spinifex-daemon and spinifex.target are not active"
}

# step_fwcfg accepts only an absent network.firewall_enabled or the exact boolean false. The daemon
# reads config through a weakly typed decoder, so "true", 1 and similar all arm the policy at start
# (firewall.go:216-228), and keys match case-insensitively, so every spelling under [network] counts.
step_fwcfg() {
    python3 - "$TOML" <<'PY'
import sys, tomllib
net = tomllib.load(open(sys.argv[1], "rb")).get("network", {})
keys = {k: v for k, v in net.items() if k.lower() == "firewall_enabled"}
bad = {k: v for k, v in keys.items() if v is not False}
if bad:
    sys.exit("network.%s in spinifex.toml is %r; only an absent key or the boolean false is accepted, because anything else may arm the firewall at start" % (next(iter(bad)), next(iter(bad.values()))))
print("network.firewall_enabled: %s" % (keys if keys else "absent"))
PY
}

step_start() {
    local i
    systemctl start spinifex.target
    for i in $(seq 1 30); do
        if curl -sk -o /dev/null -w '%{http_code}' https://127.0.0.1:9999/ 2>/dev/null | grep -q '^[34]'; then
            echo "API answered on attempt $i"
            return 0
        fi
        sleep 10
    done
    echo "the API did not answer on 127.0.0.1:9999 after 5 minutes" >&2
    systemctl --no-pager --failed >&2 || true
    exit 1
}

# step_units requires the core units active after the start, waiting up to a minute for each, and
# prints the last journal lines of any that is not. The rest are reported, not required.
REQUIRED_UNITS=(spinifex-daemon spinifex-vpcd spinifex-awsgw spinifex-nats spinifex-predastore spinifex-viperblock ovsdb-server ovs-vswitchd ovn-controller)
OTHER_UNITS=(spinifex-northstar spinifex-ui spinifex-qmp-collector)

step_units() {
    local u i bad=0
    for u in "${REQUIRED_UNITS[@]}"; do
        for i in $(seq 1 12); do
            if systemctl is-active -q "$u.service"; then break; fi
            sleep 5
        done
        if systemctl is-active -q "$u.service"; then
            echo "ok: $u active"
        else
            echo "FAIL: $u is $(systemctl is-active "$u.service" 2>&1 || true)" | tee /dev/fd/3
            journalctl -u "$u.service" -b -n 20 --no-pager 2>&1 | redact | tee /dev/fd/3 || true
            bad=1
        fi
    done
    for u in "${OTHER_UNITS[@]}"; do
        echo "info: $u is $(systemctl is-active "$u.service" 2>&1 || true)"
    done
    return "$bad"
}

# aws_home prints the home directory spx admin init wrote the aws profile into: SUDO_USER's
# when run through sudo, root's otherwise (spinifex/operator/admin/admin.go:921-935).
aws_home() {
    local u="${SUDO_USER:-}" h=""
    if [ "$(id -u)" = 0 ] && [ -n "$u" ]; then h="$(getent passwd "$u" | cut -d: -f6)"; fi
    printf '%s' "${h:-$HOME}"
}

SPX_ENDPOINT="https://localhost:9999"

# aws_cli drops every ambient AWS setting that could point the call at another account or endpoint.
aws_cli() {
    env -u AWS_ACCESS_KEY_ID -u AWS_SECRET_ACCESS_KEY -u AWS_SESSION_TOKEN -u AWS_ENDPOINT_URL \
        -u AWS_ENDPOINT_URL_EC2 -u AWS_DEFAULT_PROFILE -u AWS_CA_BUNDLE \
        aws --profile spinifex --region "$SPX_REGION" --endpoint-url "$SPX_ENDPOINT" "$@"
}

step_public() {
    local home subnets s v n=0 acct
    home="$(aws_home)"
    export AWS_SHARED_CREDENTIALS_FILE="$home/.aws/credentials" AWS_CONFIG_FILE="$home/.aws/config"
    [ -r "$AWS_SHARED_CREDENTIALS_FILE" ] || { echo "no credentials file at $AWS_SHARED_CREDENTIALS_FILE" >&2; exit 1; }
    grep -q '^\[spinifex\]' "$AWS_SHARED_CREDENTIALS_FILE" || { echo "no [spinifex] profile in $AWS_SHARED_CREDENTIALS_FILE" >&2; exit 1; }
    echo "using the spinifex profile in $home/.aws (keys not shown)"
    acct="$(aws_cli sts get-caller-identity --query Account --output text)"
    [ -n "$acct" ] || { echo "sts get-caller-identity returned no account" >&2; exit 1; }
    echo "endpoint=$SPX_ENDPOINT region=$SPX_REGION profile=spinifex account=$acct"
    subnets="$(aws_cli ec2 describe-subnets --filters Name=default-for-az,Values=true --query 'Subnets[].SubnetId' --output text)"
    [ -n "$subnets" ] || { echo "no default subnets found" >&2; exit 1; }
    for s in $subnets; do
        aws_cli ec2 modify-subnet-attribute --subnet-id "$s" --map-public-ip-on-launch
        v="$(aws_cli ec2 describe-subnets --subnet-ids "$s" --query 'Subnets[0].MapPublicIpOnLaunch' --output text)"
        [ "$v" = True ] || { echo "subnet $s MapPublicIpOnLaunch is '$v', expected True" >&2; exit 1; }
        echo "subnet $s MapPublicIpOnLaunch=True"
        n=$((n + 1))
    done
    echo "$n default subnet(s) assign public addresses"
}

step_image() {
    spx admin images import --name "$IMAGE" --config "$TOML"
}

verify_start() {
    local fwd rp bad=0
    fwd="$(cat /proc/sys/net/ipv4/ip_forward)"
    if [ "$fwd" = 1 ]; then log "ip_forward is 1"; else log "FAIL: ip_forward is $fwd, must be 1"; bad=1; fi
    rp="$(rp_filter_effective)" || rp=unreadable
    if [ "$rp" = 0 ] || [ "$rp" = 2 ]; then log "rp_filter on $IFACE is $rp (not strict)"; else log "FAIL: rp_filter on $IFACE is $rp"; bad=1; fi
    if findmnt --mountpoint "$DATA_MP" -no UUID | grep -qix "$DATA_UUID"; then log "$DATA_MP still on $DATA_UUID"; else log "FAIL: $DATA_MP changed"; bad=1; fi
    if ip -4 -o addr show | awk '{sub(/\/.*/, "", $4); print $4}' | grep -qx "$EIP"; then log "FAIL: $EIP is on a host interface"; bad=1; fi
    if systemctl is-active -q spinifex.target; then log "spinifex.target active"; else log "FAIL: spinifex.target not active"; bad=1; fi
    if [ "$bad" != 0 ]; then printf 'FAIL  verify\n'; exit 1; fi
    printf 'OK    verify\n'
    printf 'endpoint=https://%s:9999\nconsole=https://%s:3000/\neip=%s\n' "$HOST_IP" "$HOST_IP" "$EIP" | redact
}

say_disarm() {
    say "TEMPORARY emergency disarm, only if you lose ssh; run from the out-of-band serial console, in this order:"
    say "  sudo systemctl stop spinifex-daemon"
    say "  systemctl is-active spinifex-daemon      (expect: inactive)"
    say "  sudo nft delete table inet spinifex_filter"
    say "  sudo mv /etc/spinifex/firewall/peers.nft /root/peers.nft.disarmed"
    say "  sudo nft list tables      (must succeed and not list spinifex_filter)"
    say "  then test a new ssh login."
    say "Do not reboot or restart Spinifex afterwards until a durable disarm is reviewed: the boot unit reloads the policy from peers.nft (setup.sh:880-895, spinifex-firewall.service:18) and the daemon re-creates it."
}

paste_start() {
    say ""
    say "$PASTE_NOTE"
    if [ "$ARMED" = 1 ]; then
        say "The host firewall is armed (mode on, policy table loaded with the ssh 22 rule, checked after the start). Access is NOT verified by this script: open a NEW ssh login."
    else
        say "The host firewall is NOT armed (mode off, policy table absent, checked after the start). Arm it before guests run: ./install-node.sh --phase firewall with the same --eip and --data-uuid."
    fi
    say_disarm
    say "Paste back: the RUN/OK/FAIL lines above, then:"
    say "  grep -n -B1 -A8 '$POOL_NAME' $TOML"
    say "  systemctl list-units 'spinifex*' --no-pager"
    say "  journalctl -u spinifex-daemon -b --no-pager | tail -n 40"
}

phase_start() {
    require_done ovn live_ovn_state
    run_always prestart "refuse if spinifex-daemon or spinifex.target is already active" step_prestart
    if [ "$ARMED" = 1 ]; then
        run_always fwcheck "armed start: mode on, active ssh accept rule, ssh ports cover sshd -T, 0.0.0.0/0 trusted, nft readable" step_fwarmed
    else
        run_always fwcheck "firewall mode is off and nft list tables (rc 0) does not list table inet spinifex_filter" step_fwcheck
    fi
    run_step init "spx admin init --node $NODE_NAME --nodes 1 --region $SPX_REGION --az $SPX_AZ --ipsec=false --external-mode=nat" step_init live_init
    run_step config "[network] imds remap 169.254.42.254/.253; static pool $EIP via ${GATEWAY:-?} with gateway_ip 192.0.2.1, no prefix_len" step_config live_config
    if [ "$ARMED" = 1 ]; then
        run_always fwcfg "armed start: network.firewall_enabled must be absent" step_fwabsent
    else
        run_always fwcfg "fail on an explicit network.firewall_enabled = true in spinifex.toml" step_fwcfg
    fi
    run_always start "systemctl start spinifex.target, wait up to 5 minutes for the API" step_start
    if [ "$ARMED" = 1 ]; then
        run_always fwpost "after the start: wait up to 6 minutes for table inet spinifex_filter with policy drop and the ssh 22 accept rule" step_fwloaded
    else
        run_always fwpost "after the start: firewall mode off and nft list tables (rc 0) does not list table inet spinifex_filter" step_fwcheck
    fi
    run_always units "after the start: ${REQUIRED_UNITS[*]} must be active; journal of any that is not" step_units
    run_always public "mapPublicIpOnLaunch on every default subnet, each aws exit status checked, then read back" step_public
    if [ -n "$IMAGE" ]; then
        run_always image "import image $IMAGE" step_image
    fi
    if [ "$DRY" = 1 ]; then return 0; fi
    verify_start
    paste_start
}

# ── phase: firewall ──────────────────────────────────────────────────────────

# redact_ips hides every IPv4 address except 0.0.0.0, for the loaded policy and the peers file.
redact_ips() {
    sed -E -e 's/\b0\.0\.0\.0\b/ANYV4/g' -e 's/([0-9]{1,3}\.){3}[0-9]{1,3}/<ip>/g' -e 's/ANYV4/0.0.0.0/g' | redact
}

# table_state prints present, absent or unknown from a successful nft inventory; a failing nft
# is unknown, never absent.
table_state() {
    local out
    out="$(nft list tables 2>&1)" || { echo unknown; return 0; }
    if grep -Fxq 'table inet spinifex_filter' <<<"$out"; then echo present; else echo absent; fi
}

# fw_stop_verified stops the daemon, which re-creates the policy table, and proves it is not running.
fw_stop_verified() {
    local s
    systemctl stop spinifex-daemon || log "WARN: daemon stop returned an error"
    s="$(systemctl is-active spinifex-daemon || true)"
    log "daemon state after stop: $s"
    [ "$s" = inactive ] || [ "$s" = failed ]
}

FW_ARMED=0
FW_STARTED=0
FW_FINISHED=0

# fw_on_exit runs on every exit of the firewall phase. After the mode may have changed it stops the
# daemon if it was started, shows what the table looks like and says what to do; it never claims access works.
fw_on_exit() {
    local t
    if [ "$FW_FINISHED" = 1 ] || [ "$FW_ARMED" != 1 ]; then return 0; fi
    if [ "$FW_STARTED" = 1 ]; then
        if fw_stop_verified; then log "daemon stopped; keep it stopped"; else log "daemon NOT confirmed stopped"; fi
    fi
    t="$(table_state)"
    log "current spinifex_filter table state: $t (a snapshot; a helper may still change it)"
    stat -c "%A %U %s %n" "$FW_DIR/peers.nft" 2>&1 | redact_ips >&3 || true
    log "Firewall mode may now be ON persistently (not checked). Do NOT reboot or restart Spinifex until reviewed. Paste this output."
    if [ "$FW_STARTED" = 1 ] || [ "$t" != absent ]; then
        log "If SSH is lost, use the out-of-band serial console."
        say_disarm
    fi
}

# step_fwabsent accepts only an absent network.firewall_enabled, in any key spelling. An explicit
# value overrides the mode file: true arms the policy before this phase wants it, false removes it.
step_fwabsent() {
    python3 - "$TOML" <<'PY'
import sys, tomllib
net = tomllib.load(open(sys.argv[1], "rb")).get("network", {})
keys = {k: v for k, v in net.items() if k.lower() == "firewall_enabled"}
if keys:
    k = next(iter(keys))
    sys.exit("network.%s in spinifex.toml is %r; remove it so the mode file decides" % (k, keys[k]))
print("network.firewall_enabled is absent, so the mode file decides")
PY
}

# step_fwarmed is the baseline of an armed start. The daemon writes peers.nft and loads the policy during
# the start (setup.sh set-peers), so that file is not required here. What must hold before the start is
# the effective ssh allowance, read from the non-comment lines of the generated files: the input rule,
# the defined ssh ports covering every port sshd listens on, and trust of 0.0.0.0/0.
step_fwarmed() {
    local mode state ports sshd_ports p
    mode="$(cat "$FW_MODE" 2>/dev/null)" || mode=missing
    [ "$mode" = on ] || { echo "firewall mode file is '$mode', an armed start needs on" >&2; exit 1; }
    for f in spinifex.nft local.nft custom.nft; do
        [ -s "$FW_DIR/$f" ] || { echo "$FW_DIR/$f is missing or empty" >&2; exit 1; }
    done
    # shellcheck disable=SC2016  # literal nft variable names
    grep -v '^[[:space:]]*#' "$FW_DIR/spinifex.nft" | grep -q 'ip saddr \$trusted_ssh_peers tcp dport \$spinifex_ssh_ports accept' ||
        { echo "spinifex.nft has no active ssh accept rule; starting would lock out ssh" >&2; exit 1; }
    for f in local custom; do
        grep -v '^[[:space:]]*#' "$FW_DIR/spinifex.nft" | grep -q "include \"$FW_DIR/$f.nft\"" ||
            { echo "spinifex.nft does not include $f.nft" >&2; exit 1; }
    done
    ports="$(grep -v '^[[:space:]]*#' "$FW_DIR/local.nft" | sed -n 's/^define spinifex_ssh_ports *= *{\(.*\)}.*/\1/p' | tr -d ' ')"
    [ -n "$ports" ] || { echo "local.nft defines no spinifex_ssh_ports" >&2; exit 1; }
    sshd_ports="$(sshd -T 2>/dev/null | sed -n 's/^port //p')"
    [ -n "$sshd_ports" ] || { echo "sshd -T gave no listening ports" >&2; exit 1; }
    for p in $sshd_ports; do
        case ",$ports," in *",$p,"*) ;; *) echo "sshd listens on $p but the policy allows only $ports" >&2; exit 1 ;; esac
    done
    grep -v '^[[:space:]]*#' "$FW_DIR/custom.nft" | grep -q '^define trusted_ssh_peers *= *{[^}]*0\.0\.0\.0/0[^}]*}' ||
        { echo "custom.nft does not trust 0.0.0.0/0 for ssh; refusing an unattended start" >&2; exit 1; }
    state="$(table_state)"
    [ "$state" != unknown ] || { echo "nft list tables failed, so the table state is unknown" >&2; exit 1; }
    echo "mode on, ssh rule active, ports {$ports} cover sshd {${sshd_ports//$'\n'/,}}, 0.0.0.0/0 trusted, table is $state"
}

# step_vfwcfg is the verify form: absent or the exact boolean true; anything else overrides mode on.
step_vfwcfg() {
    python3 - "$TOML" <<'PY'
import sys, tomllib
net = tomllib.load(open(sys.argv[1], "rb")).get("network", {})
keys = {k: v for k, v in net.items() if k.lower() == "firewall_enabled"}
bad = {k: v for k, v in keys.items() if v is not True}
if bad:
    k = next(iter(bad))
    sys.exit("network.%s in spinifex.toml is %r; only an absent key or the boolean true leaves the firewall on" % (k, bad[k]))
print("network.firewall_enabled in the file: %s" % (keys if keys else "absent"))
PY
}

phase_firewall() {
    require_done ovn live_ovn_state
    require_done config live_config
    # Everything before the console prompt reads only: run_ro writes no file.
    run_ro fwbase "baseline: firewall mode off and policy table absent" step_fwcheck
    run_ro fwabsent "network.firewall_enabled is absent (an explicit value overrides the mode file)" step_fwabsent
    if [ "$DRY" = 1 ]; then
        printf 'PLAN  firewall  print the console disarm block, then wait for a typed console-open\n'
        printf 'PLAN  firewall  stop spinifex-daemon and prove it is not running, then recheck mode off and table absent\n'
        printf 'PLAN  firewall  SETUP_STAGES=firewall setup.sh --firewall on (log /root/firewall.log)\n'
        printf 'PLAN  firewall  show the ssh rules unredacted on the local terminal only, the sshd listener, then wait for a typed yes\n'
        printf 'PLAN  firewall  start spinifex-daemon, wait up to 6 minutes for table inet spinifex_filter, check %s, print the table\n' "${REQUIRED_UNITS[*]}"
        return 0
    fi
    local ans l i ok=0
    if [ "$VERIFY_BAD" != 0 ]; then die "baseline refused (above); nothing was changed"; fi
    if [ ! -r /dev/tty ] || [ ! -w /dev/tty ]; then
        die "no terminal for the confirmations (run from an interactive ssh session, not a pipe); nothing was changed"
    fi
    say "Nothing has been changed yet. Open the out-of-band serial console in another window now and log in."
    say_disarm
    say "Type console-open and Enter once the serial console is open and logged in; anything else stops here with nothing changed:"
    read -r ans </dev/tty || die "could not read your answer; nothing was changed"
    [ "$ans" = console-open ] || die "console not confirmed; nothing was changed"
    trap fw_on_exit EXIT
    trap 'log "STOP: interrupted"; exit 130' INT TERM HUP
    fw_stop_verified || die "daemon did not stop"
    # With the daemon stopped nothing can load the table, so this absence is the baseline the later wait relies on.
    step_fwcheck >/dev/null 2>&1 || die "after stopping the daemon, firewall mode is not off or the policy table is loaded; daemon left stopped, nothing else changed. Check: cat $FW_MODE; nft list tables"
    FW_ARMED=1
    env SETUP_STAGES=firewall "$SHARE/setup.sh" --firewall on >/root/firewall.log 2>&1 || {
        tail -n 20 /root/firewall.log | sed 's/^/      | /' | redact_ips >&3
        die "setup.sh --firewall on failed (full log /root/firewall.log); daemon left stopped"
    }
    # The real ssh sources go to the terminal only, so the person can check their laptop is covered; nothing here is redacted.
    grep -r -n -e trusted_ssh_peers -e spinifex_ssh_ports "$FW_DIR/" >/dev/null || die "no ssh rule found; daemon left stopped"
    {
        echo "== LOCAL ONLY, do not paste: ssh rules that will be applied, unredacted"
        grep -r -n -e trusted_ssh_peers -e spinifex_ssh_ports "$FW_DIR/"
        echo "== end of the local-only block"
    } >/dev/tty
    say "== sshd listening"
    l="$(ss -tlnH sport = :22)" || die "ss failed; daemon left stopped"
    [ -n "$l" ] || die "nothing listening on tcp 22; daemon left stopped"
    echo "$l" | redact_ips >&3
    say "Check the local-only block above: the trusted ssh source must cover your laptop's address (0.0.0.0/0 does), and the ssh port must be 22."
    say "Type yes and Enter to start the daemon and arm the firewall; anything else stops here:"
    read -r ans </dev/tty || die "could not read your answer; daemon left stopped"
    [ "$ans" = yes ] || die "not confirmed; daemon left stopped"
    FW_STARTED=1
    systemctl start spinifex-daemon || die "daemon start failed"
    for i in $(seq 1 36); do
        if [ "$(table_state)" = present ]; then ok=1; break; fi
        sleep 10
    done
    [ "$ok" = 1 ] || die "table inet spinifex_filter not loaded within 6 minutes"
    step_units >&3 || die "a required unit is not active"
    say "== loaded policy (IPv4 redacted except 0.0.0.0)"
    nft list table inet spinifex_filter | redact_ips >&3 || die "could not list the loaded table"
    say "== peers file (redacted)"
    redact_ips <"$FW_DIR/peers.nft" >&3 || die "peers file unreadable"
    FW_FINISHED=1
    say "POLICY LOADED from an absent baseline. Check the input chain above: ssh 22 accepted, internal ports only from the peer set, default drop."
    say "Access is NOT verified yet. Keep this terminal open, open a NEW terminal and ssh in."
    say "Next: reboot only after that works and you have the out-of-band console open, then ./install-node.sh --phase verify with the same --eip and --data-uuid."
}

# ── phase: verify ────────────────────────────────────────────────────────────

# The verify phase only reads: run_ro keeps each check's output in memory and prints it, and
# writes no file under /root. It reports every failing check, then exits non-zero if any failed.
VERIFY_BAD=0
run_ro() {
    local name="$1" desc="$2" fn="$3" out rc=0
    if [ "$DRY" = 1 ]; then printf 'PLAN  %-9s %s\n' "$name" "$desc" | redact; return 0; fi
    set +e
    out="$( ( set -euo pipefail; "$fn" ) 2>&1 )"
    rc=$?
    set -e
    if [ "$rc" -eq 0 ]; then printf 'OK    %-9s\n' "$name"; else printf 'FAIL  %-9s exit %s\n' "$name" "$rc"; VERIFY_BAD=1; fi
    if [ -n "$out" ]; then printf '%s\n' "$out" | sed 's/^/      | /' | redact_ips; fi
}

# step_vdaemon reports the daemon's state and InvocationID. The daemon reads its config once, and nothing
# here ties the file to the bytes it loaded, so the effective config is reported as unproven.
step_vdaemon() {
    local state inv since
    state="$(systemctl is-active spinifex-daemon || true)"
    inv="$(systemctl show -p InvocationID --value spinifex-daemon)"
    since="$(systemctl show -p ActiveEnterTimestamp --value spinifex-daemon)"
    echo "spinifex-daemon is $state, InvocationID ${inv:-none}, active since ${since:-unknown}"
    [ "$state" = active ] || { echo "spinifex-daemon is not active" >&2; exit 1; }
    echo "daemon effective config: unproven (file checked, not the loaded bytes)"
}

step_vmode() {
    local mode
    mode="$(cat "$FW_MODE" 2>/dev/null)" || mode=missing
    [ "$mode" = on ] || { echo "firewall mode file is '$mode', expected on" >&2; exit 1; }
    echo "firewall mode is on"
}

# step_vtable waits (a minute by default) for the daemon or the boot unit to load the policy.
step_vtable() {
    local i chain
    for i in $(seq 1 "${VTABLE_TRIES:-12}"); do
        if [ "$(table_state)" = present ]; then break; fi
        sleep "${VTABLE_GAP:-5}"
    done
    [ "$(table_state)" = present ] || { echo "table inet spinifex_filter is not loaded (nft state: $(table_state))" >&2; exit 1; }
    chain="$(nft list chain inet spinifex_filter input)" || { echo "could not list chain input of spinifex_filter" >&2; exit 1; }
    grep -Fq 'ip saddr 0.0.0.0/0 tcp dport 22 accept' <<<"$chain" || { echo "input chain has no 'ip saddr 0.0.0.0/0 tcp dport 22 accept' rule" >&2; exit 1; }
    grep -Fq 'policy drop' <<<"$chain" || { echo "input chain does not have policy drop" >&2; exit 1; }
    echo "table loaded; input chain has policy drop and the ssh 22 accept rule"
}

# step_fwloaded is the armed start's proof that the daemon loaded the policy: up to 6 minutes for the table.
step_fwloaded() {
    VTABLE_TRIES=36 VTABLE_GAP=10 step_vtable
}

step_vpeers() {
    [ -r "$FW_DIR/peers.nft" ] || { echo "$FW_DIR/peers.nft is not readable" >&2; exit 1; }
    [ -s "$FW_DIR/peers.nft" ] || { echo "$FW_DIR/peers.nft is empty" >&2; exit 1; }
    echo "peers file present: $(wc -l < "$FW_DIR/peers.nft") lines"
}

step_vnet() {
    local bad=0 r
    ovn_verify || bad=1
    if ip -4 -o addr show | awk '{sub(/\/.*/, "", $4); print $4}' | grep -qx "$EIP"; then
        echo "FAIL: $EIP is an address on a host interface"; bad=1
    else
        echo "ok: $EIP is not an address on any host interface"
    fi
    r="$(ip -4 route get "$EIP" 2>&1)" || r="no route: $r"
    echo "info: route to the EIP: $r"
    return "$bad"
}

# step_vfinal repeats the mode and table checks last, because the earlier checks can take minutes.
step_vfinal() {
    step_vmode
    step_vtable
}

phase_verify() {
    local t0
    t0="$(date -u +%FT%TZ)"
    run_ro vmode "firewall mode file says on" step_vmode
    run_ro vfwcfg "spinifex.toml network.firewall_enabled is absent or true, not false" step_vfwcfg
    run_ro vdaemon "spinifex-daemon active, InvocationID reported; the loaded config is not proven" step_vdaemon
    run_ro vtable "table inet spinifex_filter is loaded (waits up to 1 minute), input chain policy drop with the ssh 22 accept rule" step_vtable
    run_ro vpeers "$FW_DIR/peers.nft exists and is not empty" step_vpeers
    run_ro vconfig "spinifex.toml has the $POOL_NAME pool with gateway_ip, the imds remap and external_mode nat" validate_config
    run_ro vunits "${REQUIRED_UNITS[*]} are active (waits up to 1 minute each)" step_units
    run_ro vnet "default route, connected routes, gateway, br-ext and transit NAT as recorded before setup-ovn; $EIP not on a host interface" step_vnet
    run_ro vfinal "recheck firewall mode and loaded table after all the other checks" step_vfinal
    if [ "$DRY" = 1 ]; then return 0; fi
    say ""
    if [ "$VERIFY_BAD" != 0 ]; then
        printf 'FAIL  verify    at least one check failed; see above\n'
        exit 1
    fi
    say "VERIFIED: checks ran one after another from $t0 to $(date -u +%FT%TZ), not as one atomic snapshot; mode and table were rechecked last. They cover firewall mode, the config file, daemon state, loaded policy, peers file, config pool, required units, routes and transit NAT."
    say "Not shown: that the running daemon loaded this config file, what the daemon does at a later reconcile or restart, a guest launch, public reachability of the EIP, and a new ssh login through the firewall. Test those yourself."
}

# ── main ─────────────────────────────────────────────────────────────────────

case "$PHASE" in
    install|ovn|start|firewall|verify)
        if [ "$DRY" != 1 ] && [ "$PHASE" != verify ] && [ "$PHASE" != firewall ]; then install -d -m 0700 "$STATE_DIR"; fi
        preflight
        echo "--- plan: phase $PHASE ---" ;;
esac
case "$PHASE" in
    install) phase_install ;;
    ovn) phase_ovn ;;
    start) phase_start ;;
    firewall) phase_firewall ;;
    verify) phase_verify ;;
esac
if [ "$DRY" = 1 ]; then
    echo "--- dry run: nothing was changed; problems found: $PREFLIGHT_FAILS ---"
    [ "$PREFLIGHT_FAILS" = 0 ]
fi
