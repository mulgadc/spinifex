---
title: "Host and Guest Lifecycle"
description: "The contract between Spinifex host service lifecycle events and running guest EC2 instances."
category: "Admin"
tags:
  - lifecycle
  - shutdown
  - systemd
  - guests
resources:
  - title: "Spinifex Repository"
    url: "https://github.com/mulgadc/spinifex"
  - title: "Updating Spinifex"
    url: "/docs/admin/update"
---

# Host and Guest Lifecycle

> The contract between Spinifex host service lifecycle events and running guest EC2 instances.

## Table of Contents

- [Overview](#overview)
- [The Contract](#the-contract)
- [Older Releases: `systemctl stop spinifex.target`](#older-releases-systemctl-stop-spinifextarget)
- [Older Releases: A Local Drain Drained the Whole Cluster](#older-releases-a-local-drain-drained-the-whole-cluster)
- [Why Guests Outlive the Daemon](#why-guests-outlive-the-daemon)
- [Checking State](#checking-state)
- [Maintenance Runbooks](#maintenance-runbooks)

---

## Overview

`spinifex.target` groups every Spinifex service on a node: `spinifex-nats`,
`spinifex-predastore`, `spinifex-northstar`, `spinifex-viperblock`,
`spinifex-daemon`, `spinifex-awsgw`, `spinifex-vpcd`, `spinifex-ui`,
`spinifex-qmp-collector`, `spinifex-shutdown`, and the
`spinifex-nats-watchdog` timer. A guest EC2 instance is a `qemu-system-x86_64`
process launched by `spinifex-daemon` — it is not itself a systemd unit, and
it is deliberately allowed to keep running after the service that launched it
stops.

The one idea to hold onto: guests outlive the daemon by design, so what
happens to a running guest depends entirely on which lifecycle event
triggered the stop, not on whether "Spinifex" as a whole looks like it is
still running.

## The Contract

| Event | What happens to running guests | Is storage available afterwards | What the operator must do |
|---|---|---|---|
| Host `reboot` / `poweroff` / `halt` | Drained gracefully — QMP `system_powerdown`, volumes unmounted, WAL flushed — by `spinifex-shutdown.service`'s `ExecStop` | n/a, the host is going down | Nothing extra |
| `systemctl restart spinifex.target` | Survive. Not rebooted. Reattached over QMP by `Manager.Restore` when the daemon comes back | Yes, within seconds | Nothing extra — this is the supported way to pick up a config change |
| `systemctl stop spinifex.target` | Drained the same as a host shutdown, because nothing guarantees the stack comes back on its own | No, not until the stack is started again | Nothing extra — but see [Older Releases](#older-releases-systemctl-stop-spinifextarget) if the node predates this behaviour |
| `systemctl restart spinifex-daemon.service` (daemon only) | Survive via `KillMode=process`. Reattached by `Manager.Restore` | Yes, throughout | Nothing extra |
| Daemon crash / SIGKILL | Survive | Yes, throughout — the crash only takes down the daemon | Nothing — crash recovery reattaches automatically on the next daemon start |
| Host power loss | Die with the host. No drain is possible | No, until the host boots and services start again | Expect `Manager.Restore` to run crash recovery on boot. Guest filesystems may need an fsck the same as after any unclean power cut |
| `spx admin cluster shutdown` | Stopped in the DRAIN phase, before storage (STORAGE, PERSIST) and control-plane (INFRA) services stop | n/a, the cluster is being taken down | Nothing extra — this is the correct command for taking a whole cluster down |

## Older Releases: `systemctl stop spinifex.target`

> [!WARNING]
> **Releases from v1.15.0 up to the release carrying this page do NOT drain on a plain `systemctl stop spinifex.target`.** Running guests get no signal at all — no QMP `system_powerdown`, no SIGTERM. Predastore and viperblock stop underneath the guest, leaving it with no data path, while QEMU keeps running. Drives are launched with `cache=none,werror=report,rerror=report`, so I/O errors are reported into the guest instead of pausing it — the guest keeps running and keeps believing it has a disk. Anything written after storage goes away is lost, and a guest filesystem that takes I/O errors on its journal can be left inconsistent.

**Cause:** on those releases `spinifex-shutdown.service`'s `ExecStop` ran
`spx admin node drain --local --timeout=120s --only-if-host-stopping`, which
drains only when `systemctl is-system-running` reports the literal state
`stopping`. That is true for a real reboot/poweroff but not for a plain
`systemctl stop spinifex.target` — systemd reports `running` for both a plain
stop and the stop half of a restart, so the gate could not tell them apart. It
skipped the drain and logged `Host is not shutting down; skipping guest
drain.` The gate now also reads systemd's pending job list, which does
distinguish the two.

The fastest way to check is `spx admin upgrade --dry-run`, which reports every
unit's status against the one shipped in the running binary — including
`spinifex-shutdown.service` — without changing anything. See
[Checking for Unit Drift](/docs/admin/update#checking-for-unit-drift). A node
reported stale or missing for that unit predates this fix; reconcile it with
`sudo spx admin upgrade --units-only --yes` (this only rewrites the unit and
runs `daemon-reload` — it does not restart anything or drain guests itself).

To check by hand instead, look at the unit directly:

```bash
systemctl cat spinifex-shutdown.service | grep ExecStop
```

`--unless-restarting` drains on a plain stop; `--only-if-host-stopping` does
not. **On a node still showing `--only-if-host-stopping`, drain guests
explicitly before stopping the target.**

1. Stop the instances first, via the AWS-compatible API:

   ```bash
   aws ec2 stop-instances --instance-ids i-0123456789abcdef0
   ```

   Or run the drain command by hand on the node — invoked without a gate flag
   it drains unconditionally:

   ```bash
   sudo spx admin node drain --local
   ```

2. Confirm no guests are left running:

   ```bash
   ps auxw | grep qemu-system
   ```

3. Only then stop the target:

   ```bash
   sudo systemctl stop spinifex.target
   ```

This does not apply to `systemctl restart spinifex.target` — the restart row
in the contract table above is unaffected, since the same gate is what makes
guests survive a restart intact.

## Older Releases: A Local Drain Drained the Whole Cluster

> [!WARNING]
> **Releases up to the one carrying this section drain every node in the
> cluster when any single node stops its target.** Stopping or rebooting one
> node powers down every guest on every other node, stops their API gateway,
> UI and VPC daemon, and leaves each daemon refusing new work until it is
> restarted. On a multi-node cluster this reads as an unexplained cluster-wide
> outage triggered by routine single-node maintenance.

**Cause:** the local drain published its GATE and DRAIN requests to the shared
`spinifex.cluster.shutdown.*` subjects, which every daemon subscribes to as a
fan-out. The node filter applied only to the replies the stopping node waited
for, so the other nodes had already drained by the time it was applied.
Requests now name the node they apply to, and a daemon ignores one addressed
to another node.

Both ends have to be current for this to hold: the node that issues the drain
sends the target, and the nodes that receive it must be new enough to honour
it. A cluster mid-upgrade still drains cluster-wide from any node that has not
yet been updated. **Upgrade every node before relying on single-node
maintenance**, and until then stop guests deliberately rather than as a side
effect of stopping a target.

To check whether a cluster is exposed, confirm each node's `spx` is current:

```bash
spx version
```

## Why Guests Outlive the Daemon

`KillMode=process` is set on `spinifex-daemon.service` and
`spinifex-viperblock.service`. It tells systemd to signal only the unit's
main process and leave everything else in its cgroup alone, so guest QEMU
processes and any nbdkit backend still serving a guest survive a restart of
the service that spawned them. This is deliberate: a control-plane restart or
upgrade must never interrupt a customer workload just because the daemon that
launched it needed to bounce.

On the way back up, `Manager.Restore` reconnects to each surviving guest over
QMP rather than relaunching it, so the guest is never rebooted by a daemon or
target restart.

This is what makes the restart row in the contract table safe, and it holds
only as long as storage comes back with the daemon — a daemon-only restart
never takes predastore or viperblock down, and a target restart brings them
back within seconds. It does not hold for an event that leaves storage down
indefinitely, which is exactly the deviation described above.

## Checking State

```bash
# Service status for the whole stack
systemctl status spinifex.target

# Is systemd mid-shutdown, or just running?
systemctl is-system-running

# Any guest QEMU processes still alive
ps auxw | grep qemu-system

# Guest state as Spinifex sees it (node, status, health)
spx get vms

# EC2-API view of instance state
aws ec2 describe-instances

# Confirm whether the last target stop actually drained guests
journalctl -u spinifex-shutdown -b
# Look for: "Host is not shutting down; skipping guest drain."
```

## Maintenance Runbooks

### Apply a Config Change

1. Edit `/etc/spinifex/spinifex.toml`.
2. Apply it:

   ```bash
   sudo systemctl restart spinifex.target
   ```

Guests survive this restart — they are not rebooted, and storage returns
within seconds. This is the supported way to pick up a config change; see
the restart row in [The Contract](#the-contract).

### Take a Node Down for Hardware Maintenance

1. Stop or reboot the host — both drain running guests first:

   ```bash
   sudo systemctl stop spinifex.target   # taking the node fully out of service
   sudo reboot                           # or, for a reboot
   ```

2. Confirm nothing is left running:

   ```bash
   ps auxw | grep qemu-system
   ```

The drain is automatic on both paths. Stop the instances yourself first only
if you want them to stay stopped when the node returns, or if the node still
runs an older build — see [Older Releases](#older-releases-systemctl-stop-spinifextarget)
above.
