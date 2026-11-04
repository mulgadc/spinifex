#!/usr/bin/env bash
# host-data-disk.sh — format and mount the blank data disk at /var/lib/spinifex. Run as root ON the host.
#
#   host-data-disk.sh [/dev/disk/by-id/nvme-eui.<id>]
#
# The disk is chosen by content, never by its nvmeXn1 name, which swaps between boots. A candidate is a
# whole-disk by-id EUI link whose device is not in the root filesystem's ancestry, has no partitions,
# holders, swap or mounts, no filesystem signature and no partition table. With none the script stops
# and lists the disks. With several it takes the largest, and the lowest by-id path among equals, and
# prints what it chose and the blank disks it left alone; pass the by-id path to choose another.
# It never reformats: a rerun on a host already done prints the UUID and exits. The last line of stdout
# is DATA_UUID=<uuid>, which the later install phases need.

set -euo pipefail

MP=/var/lib/spinifex
LABEL=spinifex-data
FSTAB_BAK=/etc/fstab.pre-spinifex
MIN_BYTES="${MIN_DATA_BYTES:-100000000000}"
WANT="${1:-}"

die() { echo "STOP: $*" >&2; exit 1; }
[ "$(id -u)" = 0 ] || die "run as root"

# Resume: the mount is already there and is ours.
if mp_uuid="$(findmnt --mountpoint "$MP" -no UUID 2>/dev/null)" && [ -n "$mp_uuid" ]; then
    src="$(findmnt --mountpoint "$MP" -no SOURCE)"
    [ "$(blkid -s LABEL -o value "$src")" = "$LABEL" ] || die "$MP is mounted from $src, which is not labelled $LABEL; not touching it"
    grep -q "UUID=${mp_uuid}[[:space:]]" /etc/fstab || die "$MP is mounted but /etc/fstab has no UUID=$mp_uuid entry"
    echo "already done: $MP is mounted from $src (UUID $mp_uuid)"
    echo "DATA_UUID=$mp_uuid"
    exit 0
fi
if grep -q "$MP" /etc/fstab; then die "/etc/fstab mentions $MP but nothing is mounted there; fix that by hand first"; fi
[ ! -e "$MP" ] || [ -z "$(ls -A "$MP")" ] || die "$MP exists and is not empty"

root_src="$(findmnt -no SOURCE /)" || die "cannot find the root source"
root_anc="$(lsblk -s -nro NAME "$root_src")" || die "cannot read the root ancestry"
[ -n "$root_anc" ] || die "empty root ancestry"

# blank_reason ID prints why the disk is not a candidate and returns 1, or returns 0.
blank_reason() {
    local id="$1" dev name size kids hold sw mnt rc w
    dev="$(readlink -e "$id")" || { echo "link does not resolve"; return 1; }
    [ -b "$dev" ] || { echo "not a block device"; return 1; }
    name="$(basename "$dev")"
    if grep -qx "$name" <<<"$root_anc"; then echo "in the root ancestry"; return 1; fi
    size="$(blockdev --getsize64 "$id")" || { echo "size unreadable"; return 1; }
    [ "$size" -ge "$MIN_BYTES" ] || { echo "only $size bytes"; return 1; }
    kids="$(lsblk -nro NAME "$id")" || { echo "children unreadable"; return 1; }
    [ "$kids" = "$name" ] || { echo "has children"; return 1; }
    hold="$(ls -A "/sys/block/$name/holders/")" || { echo "holders unreadable"; return 1; }
    [ -z "$hold" ] || { echo "has holders"; return 1; }
    sw="$(swapon --show=NAME --noheadings)" || { echo "swapon failed"; return 1; }
    if grep -q "$name" <<<"$sw"; then echo "is swap"; return 1; fi
    mnt="$(lsblk -nro MOUNTPOINTS "$id")" || { echo "mounts unreadable"; return 1; }
    [ -z "${mnt//[[:space:]]/}" ] || { echo "is mounted"; return 1; }
    set +e; blkid -p "$id" >/dev/null 2>&1; rc=$?; set -e
    [ "$rc" -eq 2 ] || { echo "blkid -p says it has a signature (rc $rc)"; return 1; }
    w="$(wipefs -n "$id")" || { echo "wipefs failed"; return 1; }
    [ -z "$w" ] || { echo "wipefs finds signatures"; return 1; }
    return 0
}

# choose_data_disk ID... prints the largest disk's ID, the lowest path among equal sizes, and says on
# stderr which it left. The choice depends only on size and path, so every node of one plan agrees.
choose_data_disk() {
    local chosen rest
    chosen="$(for id in "$@"; do printf '%s %s\n' "$(blockdev --getsize64 "$id")" "$id"; done |
        sort -k1,1nr -k2,2 | head -n1)"
    chosen="${chosen#* }"
    if [ "$#" -gt 1 ]; then
        rest="$(printf '%s\n' "$@" | grep -vxF "$chosen" | sort | tr '\n' ' ')"
        echo "chose $chosen ($(blockdev --getsize64 "$chosen") bytes), the largest of $# blank disks; left untouched: ${rest% }" >&2
    fi
    printf '%s\n' "$chosen"
}

candidates=()
shopt -s nullglob
for id in /dev/disk/by-id/nvme-eui.*; do
    case "$id" in *-part*) continue ;; esac
    if [ -n "$WANT" ] && [ "$id" != "$WANT" ]; then continue; fi
    if why="$(blank_reason "$id")"; then
        candidates+=("$id")
    else
        echo "skip $id: $why"
    fi
done
shopt -u nullglob

if [ "${#candidates[@]}" -eq 0 ]; then
    lsblk -o NAME,SIZE,TYPE,MODEL,SERIAL,FSTYPE,MOUNTPOINTS >&2 || true
    die "no blank data disk among the by-id EUI disks"
fi
ID="$(choose_data_disk "${candidates[@]}")"
DEV="$(readlink -e "$ID")"
echo "CHECKS PASSED: $ID -> $DEV"

mkdir -p "$MP"
mkfs.ext4 -q -L "$LABEL" "$ID"
U="$(blkid -p -o value -s UUID "$ID")" || die "blkid UUID failed"
[ -n "$U" ] || die "no UUID after mkfs"
[ "$(blkid -p -o value -s TYPE "$ID")" = ext4 ] || die "not ext4 after mkfs"

NEW="$(mktemp /etc/fstab.spinifex.XXXXXX)"
cp /etc/fstab "$NEW"
echo "UUID=$U $MP ext4 defaults,nofail,x-systemd.device-timeout=30s 0 2" >> "$NEW"
findmnt --verify --tab-file "$NEW" || { rm -f "$NEW"; die "staged fstab failed verify, /etc/fstab untouched"; }
BAK="$FSTAB_BAK.$(date +%s)"
cp -a /etc/fstab "$BAK"
trap 'cp -a "$BAK" /etc/fstab; systemctl daemon-reload; echo "ROLLED BACK /etc/fstab from $BAK" >&2' EXIT
chmod 644 "$NEW"
mv "$NEW" /etc/fstab
systemctl daemon-reload
mount "$MP"
[ "$(findmnt --mountpoint "$MP" -no UUID)" = "$U" ] || die "wrong UUID at $MP"
[ "$(findmnt --mountpoint "$MP" -no FSTYPE)" = ext4 ] || die "wrong fstype at $MP"
[ "$(findmnt --mountpoint "$MP" -no MAJ:MIN | tr -d ' ')" = "$(lsblk -dno MAJ:MIN "$ID" | tr -d ' ')" ] || die "wrong device at $MP"
trap - EXIT
findmnt --mountpoint "$MP" -o TARGET,SOURCE,MAJ:MIN,UUID,FSTYPE,OPTIONS
echo "STORAGE OK"
echo "DATA_UUID=$U"
