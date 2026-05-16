#!/bin/sh
set -e
mount -t proc  proc  /proc
mount -t sysfs sys   /sys
mount -t devtmpfs dev /dev

modprobe qemu_fw_cfg 2>/dev/null || modprobe fw_cfg_sysfs || {
    echo "FATAL: qemu_fw_cfg module not available — cannot read fw_cfg blobs" >&2
    exit 1
}

. /sys/firmware/qemu_fw_cfg/by_name/opt/spinifex/netcfg/raw
for n in 0 1 2 3 4 5; do
    eval mac=\$NIC${n}_MAC
    [ -z "$mac" ] && continue
    iface=$(ip -o link | awk -v m="$mac" 'tolower($0) ~ tolower(m) {print $2}' | tr -d :)
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

cp /sys/firmware/qemu_fw_cfg/by_name/opt/spinifex/lb-agent-env/raw /etc/conf.d/lb-agent
mkdir -p /etc/ssl/certs
cp /sys/firmware/qemu_fw_cfg/by_name/opt/spinifex/ca-cert/raw /etc/ssl/certs/ca-certificates.crt

exec /sbin/init
