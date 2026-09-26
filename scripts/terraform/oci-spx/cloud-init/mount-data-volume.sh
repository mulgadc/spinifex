#!/bin/bash
# Prepares the OCI block volume that backs Spinifex's data directory, and makes
# the mount survive a reboot. Safe to re-run: it never formats a device that
# already holds a filesystem.
set -euo pipefail

DEVICE="${1:?device path required}"
MOUNTPOINT="${2:?mount point required}"
FSLABEL=spinifex-data

# Overridable so the fstab logic can be exercised against a scratch file rather
# than the host's real one. Nothing in production sets it.
FSTAB="${FSTAB:-/etc/fstab}"

log() { printf '[spinifex-data-volume] %s\n' "$*"; }
die() {
    log "ERROR: $*"
    exit 1
}

# The Oracle Cloud Agent logs the iSCSI session in asynchronously, so the device
# is not present when cloud-init first runs. Wait for it instead of racing it.
wait_for_device() {
    local i
    for i in $(seq 1 120); do
        if [ -b "$DEVICE" ]; then
            log "$DEVICE appeared after $((i * 5))s"
            return 0
        fi
        sleep 5
    done
    die "$DEVICE did not appear within 600s; check the iSCSI session with 'iscsiadm -m session'"
}

# blkid reporting a TYPE is the only thing standing between a re-run and an erased
# data volume, so a failure to read the device is fatal rather than "assume blank".
format_if_blank() {
    local fstype rc=0
    fstype="$(blkid -o value -s TYPE "$DEVICE" 2>/dev/null)" || rc=$?

    # blkid exits 2 for an unrecognised device, which is the genuinely blank case.
    if [ "$rc" -ne 0 ] && [ "$rc" -ne 2 ]; then
        die "blkid failed on $DEVICE with status $rc; refusing to format"
    fi

    if [ -n "$fstype" ]; then
        log "$DEVICE already holds a $fstype filesystem; leaving it alone"
        return 0
    fi

    log "no filesystem on $DEVICE; creating ext4"
    mkfs.ext4 -q -L "$FSLABEL" "$DEVICE"
}

# _netdev is load bearing. Without it the mount is attempted before iscsid has a
# session and the boot either hangs or silently lands the stack on the boot disk.
write_fstab_entry() {
    local uuid entry
    uuid="$(blkid -o value -s UUID "$DEVICE")" || die "no UUID on $DEVICE after formatting"
    entry="UUID=$uuid $MOUNTPOINT ext4 defaults,_netdev,nofail 0 2"

    if grep -qF "$entry" "$FSTAB"; then
        log "fstab already carries the entry for $uuid"
        return 0
    fi

    # Drop any earlier entry for this mount point, so a re-created volume with a
    # new UUID does not leave a stale line behind it.
    sed -i.bak -E "\%^[^#][^[:space:]]*[[:space:]]+${MOUNTPOINT}[[:space:]]%d" "$FSTAB"
    printf '%s\n' "$entry" >> "$FSTAB"
    log "added $entry"
}

main() {
    wait_for_device
    format_if_blank
    mkdir -p "$MOUNTPOINT"
    write_fstab_entry

    systemctl daemon-reload

    # Mount the device by name rather than letting mount(8) look the mount point up
    # in fstab: the fstab entry exists for the next boot, not for this one.
    if mountpoint -q "$MOUNTPOINT"; then
        log "$MOUNTPOINT is already mounted"
    else
        mount -o defaults,_netdev,nofail "$DEVICE" "$MOUNTPOINT"
    fi

    findmnt --noheadings --output SOURCE,TARGET,FSTYPE,OPTIONS "$MOUNTPOINT" \
        || die "$MOUNTPOINT is not mounted after mount(8) reported success"
}

main "$@"
