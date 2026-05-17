#!/bin/sh
mount -t proc  proc  /proc
mount -t sysfs sys   /sys
mount -t devtmpfs dev /dev

modprobe qemu_fw_cfg 2>/dev/null || modprobe fw_cfg_sysfs 2>/dev/null || true
modprobe virtio_mmio 2>/dev/null || true
modprobe virtio_net 2>/dev/null || true

echo "[init] waiting for fw_cfg sysfs..."
for i in $(seq 1 10); do
    [ -f /sys/firmware/qemu_fw_cfg/by_name/opt/spinifex/netcfg/raw ] && break
    sleep 0.2
done

if [ ! -f /sys/firmware/qemu_fw_cfg/by_name/opt/spinifex/netcfg/raw ]; then
    echo "[init] FATAL: fw_cfg netcfg not available" >&2
    exit 1
fi

. /sys/firmware/qemu_fw_cfg/by_name/opt/spinifex/netcfg/raw

echo "[init] network interfaces:"
ip -o link show

for n in 0 1 2 3 4 5; do
    eval mac=\$NIC${n}_MAC
    [ -z "$mac" ] && continue

    # Retry finding the interface by MAC — virtio-mmio may take a moment to appear.
    iface=""
    for attempt in $(seq 1 10); do
        iface=$(ip -o link | awk -v m="$mac" 'tolower($0) ~ tolower(m) {print $2}' | tr -d :)
        [ -n "$iface" ] && break
        sleep 0.2
    done

    if [ -z "$iface" ]; then
        echo "[init] WARNING: no interface found for MAC $mac (NIC$n), skipping" >&2
        continue
    fi

    eval cidr=\$NIC${n}_CIDR
    eval gw=\$NIC${n}_GW
    eval default=\$NIC${n}_DEFAULT
    eval rdst=\$NIC${n}_ROUTE_DST
    eval rvia=\$NIC${n}_ROUTE_VIA

    echo "[init] configuring NIC$n: iface=$iface cidr=$cidr gw=$gw default=$default"
    ip link set "$iface" up
    [ -n "$cidr" ] && ip addr add "$cidr" dev "$iface"
    [ "$default" = "1" ] && [ -n "$gw" ] && ip route add default via "$gw" dev "$iface"
    [ -n "$rdst" ] && [ -n "$rvia" ] && ip route add "$rdst" via "$rvia" dev "$iface"
done

cp /sys/firmware/qemu_fw_cfg/by_name/opt/spinifex/lb-agent-env/raw /etc/conf.d/lb-agent
mkdir -p /etc/ssl/certs
cp /sys/firmware/qemu_fw_cfg/by_name/opt/spinifex/ca-cert/raw /etc/ssl/certs/ca-certificates.crt

exec /sbin/init
