#!/bin/sh
mount -t proc  proc  /proc
mount -t sysfs sys   /sys
mount -t devtmpfs dev /dev

# virtio_mmio is built-in (CONFIG_VIRTIO_MMIO=y) so virtio-net devices are
# already enumerated by the time PID 1 runs — no modprobe needed.
# qemu_fw_cfg and virtio_net are =m in stock Alpine linux-virt, so load them.
modprobe qemu_fw_cfg 2>/dev/null || modprobe fw_cfg_sysfs 2>/dev/null || true
modprobe virtio_net 2>/dev/null || true

NETCFG=/sys/firmware/qemu_fw_cfg/by_name/opt/spinifex/netcfg/raw
i=0
while [ "$i" -lt 25 ]; do
    [ -f "$NETCFG" ] && break
    sleep 0.04
    i=$((i + 1))
done

if [ ! -f "$NETCFG" ]; then
    # Dump host/guest fw_cfg state to the serial console before init exits so a
    # "netcfg not available" panic is diagnosable from the captured console log
    # (module-not-loaded vs qemu-not-exposing-by_name vs wrong machine type).
    echo "[init] FATAL: fw_cfg netcfg not available" >&2
    echo "[init] dmi: vendor=$(cat /sys/class/dmi/id/sys_vendor 2>/dev/null) product=$(cat /sys/class/dmi/id/product_name 2>/dev/null) bios=$(cat /sys/class/dmi/id/bios_version 2>/dev/null)" >&2
    echo "[init] qemu_fw_cfg dir present: $([ -d /sys/firmware/qemu_fw_cfg ] && echo yes || echo NO)" >&2
    echo "[init] modules: $(lsmod 2>/dev/null | awk 'NR>1{print $1}' | tr '\n' ',')" >&2
    echo "[init] by_name listing:" >&2
    ls -la /sys/firmware/qemu_fw_cfg/by_name/ 2>&1 | sed 's/^/[init]   /' >&2
    ls -la /sys/firmware/qemu_fw_cfg/by_name/opt/ 2>&1 | sed 's/^/[init]   /' >&2
    echo "[init] dmesg fw_cfg:" >&2
    dmesg 2>/dev/null | grep -i fw_cfg | sed 's/^/[init]   /' >&2
    exit 1
fi

. "$NETCFG"

for n in 0 1 2 3 4 5; do
    eval mac=\$NIC${n}_MAC
    [ -z "$mac" ] && continue

    iface=""
    for attempt in 1 2 3 4 5; do
        iface=$(ip -o link | awk -v m="$mac" 'tolower($0) ~ tolower(m) {print $2}' | tr -d :)
        [ -n "$iface" ] && break
        sleep 0.02
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

    ip link set "$iface" up
    [ -n "$cidr" ] && ip addr add "$cidr" dev "$iface"
    [ "$default" = "1" ] && [ -n "$gw" ] && ip route add default via "$gw" dev "$iface"
    [ -n "$rdst" ] && [ -n "$rvia" ] && ip route add "$rdst" via "$rvia" dev "$iface"
done

cp /sys/firmware/qemu_fw_cfg/by_name/opt/spinifex/lb-agent-env/raw /etc/conf.d/lb-agent
mkdir -p /etc/ssl/certs
cp /sys/firmware/qemu_fw_cfg/by_name/opt/spinifex/ca-cert/raw /etc/ssl/certs/ca-certificates.crt

exec /sbin/init
