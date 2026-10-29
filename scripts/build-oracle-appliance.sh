#!/bin/bash
# build-oracle-appliance.sh — turn an Oracle Linux 10.2 system reachable over
# SSH with sudo into a sealed Spinifex gold image: installed, not initialised,
# with a first-boot service that sets each cloned unit up as a single node.
# The system powers off at the end; capture its disk as the image.
#
# Usage: build-oracle-appliance.sh [options] <user>@<host>
#   --channel latest|dev   install channel (default: latest)
#   --version <tag>        install a specific release
#   --tarball <path>       install a local release tarball instead
#
# Ports, keys and jump hosts come from ~/.ssh/config. <user> needs
# passwordless sudo.
set -euo pipefail

HERE=$(cd "$(dirname "$0")" && pwd)
PAYLOAD_SRC="$HERE/appliance"
REMOTE_DIR=/tmp/spinifex-appliance

CHANNEL="" VERSION="" TARBALL="" TARGET=""

usage() { sed -n '2,13p' "$0" | sed 's/^# \{0,1\}//'; exit "${1:-0}"; }
log() { echo "==> $*"; }
die() { echo "ERROR: $*" >&2; exit 1; }

while [ $# -gt 0 ]; do
    case "$1" in
        --channel) CHANNEL="${2:-}"; shift ;;
        --version) VERSION="${2:-}"; shift ;;
        --tarball) TARBALL="${2:-}"; shift ;;
        -h | --help) usage ;;
        -*) die "unknown option: $1" ;;
        *) [ -z "$TARGET" ] || die "one target only"; TARGET=$1 ;;
    esac
    shift
done
[ -n "$TARGET" ] || usage 1
case $TARGET in *@*) ;; *) die "target must be <user>@<host>" ;; esac
BUILD_USER=${TARGET%@*}
[ -z "$TARBALL" ] || [ -f "$TARBALL" ] || die "no such tarball: $TARBALL"

# One multiplexed connection; host key checking is relaxed because the build
# system is usually a fresh install whose key nobody has recorded yet.
CTL_DIR=$(mktemp -d)
trap 'ssh -o ControlPath="$CTL_DIR/c" -O exit "$TARGET" >/dev/null 2>&1 || true; rm -rf "$CTL_DIR"' EXIT
SSH_OPTS=(-o ControlMaster=auto -o ControlPath="$CTL_DIR/c" -o ControlPersist=60
    -o StrictHostKeyChecking=accept-new -o ServerAliveInterval=15 -o ConnectTimeout=10)

rsh() { ssh "${SSH_OPTS[@]}" "$TARGET" "$@"; }

# prepare <stage> — run one prepare.sh stage as root on the build system.
prepare() {
    rsh "sudo env INSTALL_SPINIFEX_CHANNEL='$CHANNEL' INSTALL_SPINIFEX_VERSION='$VERSION' \
        INSTALL_SPINIFEX_TARBALL='${TARBALL:+$REMOTE_DIR/$(basename "$TARBALL")}' \
        BUILD_USER='$BUILD_USER' bash $REMOTE_DIR/prepare.sh $1"
}

push_payload() {
    rsh "rm -rf $REMOTE_DIR && mkdir -p $REMOTE_DIR"
    tar -C "$PAYLOAD_SRC" -cf - . | rsh "tar -C $REMOTE_DIR -xf -"
    if [ -n "$TARBALL" ]; then
        scp "${SSH_OPTS[@]}" -q "$TARBALL" "$TARGET:$REMOTE_DIR/"
    fi
}

wait_for_ssh() {
    local _i
    ssh "${SSH_OPTS[@]}" -O exit "$TARGET" >/dev/null 2>&1 || true
    sleep 10
    for _i in $(seq 1 60); do
        if rsh true 2>/dev/null; then return 0; fi
        sleep 5
    done
    die "$TARGET did not come back over SSH after the reboot"
}

rsh true || die "cannot SSH to $TARGET; set its key in ~/.ssh/config (IdentityFile) or ssh-add it"
rsh "sudo -n true" || die "$BUILD_USER needs passwordless sudo on the build system"

log "Copying the payload to $TARGET"
push_payload
prepare check

log "Updating Oracle Linux"
prepare update | tee "$CTL_DIR/update.log"
if grep -qx 'PREPARE_REBOOT=1' "$CTL_DIR/update.log"; then
    log "Rebooting into the updated kernel"
    rsh "sudo systemctl reboot" || true
    wait_for_ssh
    # /tmp is cleared on boot.
    push_payload
    prepare check
fi

log "Installing Spinifex (not initialising it)"
prepare install

log "Installing the spinifex login and the first-boot service"
prepare appliance

log "Sealing; the system powers off when done"
prepare seal || true

log "Done. Capture $TARGET's disk once it has powered off; each unit set up from it initialises itself on first boot."
