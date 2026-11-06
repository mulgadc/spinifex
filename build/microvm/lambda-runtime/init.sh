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
# lambda-agent reads its identity from fw_cfg, or the configuration disk, and
# seals it before anything else runs. virtio_net needs net_failover and failover, and the
# insmod fallback resolves no dependencies, so load the chain bottom-up.
load_mod qemu_fw_cfg || echo "[init] qemu_fw_cfg did not load" >/dev/console
load_mod failover || true
load_mod net_failover || true
load_mod virtio_net || echo "[init] virtio_net did not load" >/dev/console

ip link set lo up

# Without fw_cfg the executor attaches the same entries as a tar archive on
# the second disk; lambda-agent reads them here and erases the disk before the
# function starts.
CFG_ROOT=/spinifex-cfg
FWCFG_ROOT=/sys/firmware/qemu_fw_cfg
if [ ! -d "$FWCFG_ROOT" ]; then
    load_mod virtio_blk || echo "[init] virtio_blk did not load" >/dev/console
    i=0
    while [ "$i" -lt 25 ] && [ ! -b /dev/vdb ]; do
        sleep 0.04
        i=$((i + 1))
    done
    if [ -b /dev/vdb ]; then
        mkdir -m 0700 "$CFG_ROOT"
        if ! mount -t tmpfs -o mode=0700,size=1m tmpfs "$CFG_ROOT" ||
            ! tar -xf /dev/vdb -C "$CFG_ROOT"; then
            echo "[init] configuration disk not readable" >/dev/console
        fi
        FWCFG_ROOT=$CFG_ROOT
    fi
fi

# The guest's addresses come from the same netcfg entry as other system VMs.
NETCFG=$FWCFG_ROOT/by_name/opt/spinifex/netcfg/raw
i=0
while [ "$i" -lt 25 ] && [ ! -f "$NETCFG" ]; do
    sleep 0.04
    i=$((i + 1))
done
if [ -f "$NETCFG" ]; then
    # shellcheck source=/dev/null
    . "$NETCFG"
    for n in 0 1 2 3 4 5; do
        eval mac=\$NIC${n}_MAC
        [ -z "$mac" ] && continue
        iface=""
        for _ in 1 2 3 4 5; do
            iface=$(ip -o link | awk -v m="$mac" 'tolower($0) ~ tolower(m) {print $2}' | tr -d :)
            [ -n "$iface" ] && break
            sleep 0.02
        done
        if [ -z "$iface" ]; then
            echo "[init] no interface for MAC $mac (NIC$n)" >/dev/console
            continue
        fi
        cidr="" gw="" default="" rdst="" rvia=""
        eval cidr=\$NIC${n}_CIDR
        eval gw=\$NIC${n}_GW
        eval default=\$NIC${n}_DEFAULT
        eval rdst=\$NIC${n}_ROUTE_DST
        eval rvia=\$NIC${n}_ROUTE_VIA
        ip link set "$iface" up
        [ -n "$cidr" ] && ip addr add "$cidr" dev "$iface"
        [ "$default" = "1" ] && [ -n "$gw" ] && ip route add default via "$gw" dev "$iface"
        [ -n "$rdst" ] && [ -n "$rvia" ] && ip route add "$rdst" via "$rvia" dev "$iface"
    done
else
    # lambda-agent then fails to reach the host and the guest powers off.
    echo "[init] fw_cfg netcfg not available" >/dev/console
fi

exec /sbin/init
