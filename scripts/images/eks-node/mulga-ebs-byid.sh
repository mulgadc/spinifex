#!/bin/sh
# mulga-ebs-byid — mint the EBS-style /dev/disk/by-id symlink the upstream
# aws-ebs-csi-driver node plugin resolves. Spinifex attaches Viperblock volumes
# as virtio-blk with serial = volume-id (dashes stripped); the kernel exposes it
# at /sys/block/<dev>/serial. Alpine uses busybox mdev (no eudev), so the
# nvme-Amazon_Elastic_Block_Store_<serial> link is created here from that serial.
#
# Invoked by mdev (env MDEV=<dev>, ACTION add|remove) and at boot for coldplug.
set -eu

BYID_DIR=/dev/disk/by-id
PREFIX=nvme-Amazon_Elastic_Block_Store_

dev="${MDEV:-${1:-}}"
[ -n "${dev}" ] || exit 0

link_dev() {
    _d=$1
    _sf="/sys/block/${_d}/serial"
    [ -r "${_sf}" ] || return 0
    _serial=$(cat "${_sf}" 2>/dev/null) || return 0
    case "${_serial}" in
        vol*) mkdir -p "${BYID_DIR}"; ln -sf "../../${_d}" "${BYID_DIR}/${PREFIX}${_serial}" ;;
    esac
}

unlink_dev() {
    _d=$1
    for _l in "${BYID_DIR}/${PREFIX}"*; do
        [ -L "${_l}" ] || continue
        [ "$(readlink "${_l}")" = "../../${_d}" ] && rm -f "${_l}"
    done
}

case "${ACTION:-add}" in
    remove) unlink_dev "${dev}" ;;
    coldplug)
        for _p in /sys/block/vd*; do
            [ -e "${_p}" ] || continue
            link_dev "$(basename "${_p}")"
        done
        ;;
    *) link_dev "${dev}" ;;
esac
exit 0
