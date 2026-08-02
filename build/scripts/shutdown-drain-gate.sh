#!/bin/sh
# Gate the guest drain (spinifex-shutdown.service ExecStop) to a genuine host
# shutdown/reboot, not a `systemctl restart spinifex.target` rolling deploy.
#
# PartOf=spinifex.target means ExecStop fires on every target stop, including
# the stop half of a restart, even though the host stays up. `systemctl
# is-system-running` reports "stopping" only once the manager itself starts
# unwinding into shutdown.target (reboot/poweroff/halt/kexec) -- it is still
# "running" for a plain target restart or `systemctl stop spinifex.target`.
# Draining only on "stopping" keeps QEMU/nbdkit alive (KillMode=process) for
# every case that has a "back up" to reconnect to.
STATE=$(systemctl is-system-running 2>/dev/null)
if [ "$STATE" != "stopping" ]; then
    exit 0
fi

exec /usr/local/bin/spx admin node drain --local --timeout=120s
