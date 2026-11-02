#!/bin/sh
mount -t proc  proc  /proc
mount -t sysfs sys   /sys
mount -t devtmpfs dev /dev

# modprobe needs a usable modules.dep; insmod by explicit path does not, so
# fall back to it. Handles .ko and .ko.gz.
load_mod() {
    modprobe "$1" 2>/dev/null && return 0
    for ko in $(find /lib/modules -name "$1.ko" -o -name "$1.ko.gz" 2>/dev/null); do
        insmod "$ko" 2>/dev/null && return 0
        case "$ko" in
            *.gz) gunzip -c "$ko" >/tmp/mod.ko 2>/dev/null \
                  && insmod /tmp/mod.ko 2>/dev/null && return 0 ;;
        esac
    done
    return 1
}
# virtio_console provides the host carrier port (/dev/vportNpM).
load_mod virtio_console || true

ip link set lo up

exec /sbin/init
