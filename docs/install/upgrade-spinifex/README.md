---
title: "Upgrading Spinifex"
seoTitle: "Upgrading a Spinifex Cluster — Spinifex Docs"
description: "Move a running Spinifex cluster to a new release: what an upgrade changes, why the whole cluster stops together, and how to verify the result."
category: "Install"
tags:
  - install
  - upgrade
  - cluster
  - migration
resources:
  - title: "Multi-Node Install"
    url: "/docs/install-multi-node"
  - title: "Spinifex Repository"
    url: "https://github.com/mulgadc/spinifex"
---

# Upgrading Spinifex

> Move a running cluster to a new release without leaving it half-upgraded.

## Table of Contents

- [What an upgrade changes](#what-an-upgrade-changes)
- [Why there is no rolling upgrade](#why-there-is-no-rolling-upgrade)
- [Before you start](#before-you-start)
- [The procedure](#the-procedure)
- [Verifying the upgrade](#verifying-the-upgrade)
- [Rollback](#rollback)
- [Troubleshooting](#troubleshooting)

## What an upgrade changes

Four things change, and each one changes by a different mechanism. Knowing which is which is most of what makes an upgrade predictable.

| What | How it is updated | When it takes effect |
| ---- | ----------------- | -------------------- |
| The `spx` binary | The installer replaces `/usr/local/bin/spx` | Only when a service restarts |
| systemd units | `spx admin upgrade` | On the next `daemon-reload` and restart |
| Config files in `/etc/spinifex` | `spx admin upgrade` | On the next service restart |
| JetStream KV schema | The service that owns each bucket, as it opens it | At service start |

The last row is the one that surprises people. `spx admin upgrade` does **not** migrate the cluster's state. Each service migrates the buckets it owns while it starts, so the state schema advances as a side effect of the services coming back up — which means a service that cannot complete its migration does not start at all, and says so in its journal.

**Every Spinifex service is the same binary.** The units all run `spx service <name> start`, so replacing `spx` leaves every running service executing the old, now-unlinked image. An upgrade is not real until every `spx` process has exited and been replaced.

## Why there is no rolling upgrade

Spinifex stops the whole cluster, upgrades every node, then starts the whole cluster. It does not drain one node at a time.

The reason is that the cluster's state is shared. When a newer node migrates a KV bucket, an older node that opens the same bucket finds a schema it has no code for. Rather than read the keys it recognises and write back a half-understood view of the bucket, it refuses:

```
spinifex-instance-state is at schema version 6 but this build understands 5:
another node is running a newer release of Spinifex; upgrade this node to match
```

Refusing to open is recoverable. Writing back a partial view of a bucket is not. So a mixed-version cluster is not a degraded state to be minimised — it is a state to be avoided entirely, and the procedure below never creates one.

The cost is a real outage. Guests are drained before the stop and started again afterwards, so plan the window accordingly.

## Before you start

- **Read the release notes** for every version you are crossing, not just the target. Migrations are a chain, and each one is applied in order.
- **Know the outgoing version.** It is the rollback target, and after the upgrade nothing on the cluster records what it used to be.

  ```bash
  for n in node1 node2 node3; do echo -n "$n: "; ssh spinifex@$n 'spx version'; done
  ```

- **Clear the guests, or accept the drain.** Stopping a cluster drains every running guest. A drain that takes minutes is telling you something is wrong with a volume seal; investigate it rather than raising the timeout.
- **Take a backup.** There are no down-migrations. Once a KV migration has run, rollback means restoring state, not reinstalling the old binary.

## The procedure

### 1. Stop the cluster

```bash
sudo spx admin cluster shutdown
```

This gates the API cluster-wide, drains every guest, then takes storage and predastore down in phase order, waiting for every node to acknowledge each phase. Run it once, from any node.

### 2. Stop the services on every node, and confirm

```bash
for n in node1 node2 node3; do
    ssh spinifex@$n "sudo systemctl stop spinifex.target
        for i in \$(seq 1 45); do [ -z \"\$(pgrep -x spx || true)\" ] && break; sleep 2; done
        echo \"\$(hostname) spx remaining: \$(pgrep -x spx | wc -l)\""
done
```

**Every node must report `spx remaining: 0` before you go on.** `systemctl stop` returns once the target is inactive, which is not the same as every service having exited — the shutdown unit's drain runs while storage is still up, so those stop jobs can still be queued behind it. A node that still has an `spx` process will keep running the old binary after the install, and a later start cancels its pending stop.

### 3. Install the new release on every node

Install however you normally do — the release tarball's `setup.sh`, or your deployment tooling. Do not start anything yet.

**All nodes must be installed before any node is started.** This is the same rule as the stop: the installs happen together so that no node can come back up against a cluster that is still on the old release.

### 4. Apply config and unit migrations on every node

```bash
sudo spx admin upgrade --dry-run     # review
sudo spx admin upgrade
```

This reconciles the systemd units against the new binary and applies any pending config-file migrations, taking a timestamped backup of each file before it touches it. It is per-node and does not talk to the cluster, so run it on every node.

`Nothing to do.` is a normal and common result — most releases register no config migrations.

### 5. Start the cluster

```bash
for n in node1 node2 node3; do ssh spinifex@$n 'sudo systemctl start spinifex.target'; done
```

State migrations run here, as each service opens its buckets. On a multi-node cluster the services start on every node at once and each will attempt the same migration, so expect the same migration line in more than one journal.

## Verifying the upgrade

**Check the version on every node, and check that the services actually restarted.**

```bash
for n in node1 node2 node3; do echo -n "$n: "; ssh spinifex@$n 'spx version'; done
for n in node1 node2 node3; do
    echo -n "$n predastore up since: "
    ssh spinifex@$n 'systemctl show spinifex-predastore -p ActiveEnterTimestamp --value'
done
```

An `ActiveEnterTimestamp` older than the upgrade means that service never restarted and is still running the previous binary.

**Check that the nodes agree about the state.** `spx admin kv digest` snapshots the store on one node; `spx admin kv compare` reads several snapshots and reports any stream the nodes disagree about.

```bash
for n in node1 node2 node3; do
    ssh spinifex@$n 'sudo spx admin kv digest --config /etc/spinifex/spinifex.toml' > "digest-$n.json" &
done
wait
spx admin kv compare digest-node1.json digest-node2.json digest-node3.json
```

**Take the digests concurrently, as above, never in a loop.** The leader-lease buckets carry a 60-second message age, so digests taken even a couple of seconds apart legitimately differ and will be reported as diverged.

**Then prove it with a workload.** Launch an instance, attach a volume, reach it over the network. Nothing in the steps above touches the path a customer uses, so until a guest runs the upgrade is unverified where it matters most.

## Rollback

There are no down-migrations. Rolling back is safe only while no state migration has run, in which case it is the same procedure with the previous release: stop, install, `spx admin upgrade`, start.

Once a migration has run, the store is at a schema the old binary refuses to open, and the only way back is to restore the state from a backup taken before the upgrade. That is why the backup in [Before you start](#before-you-start) is not optional.

## Troubleshooting

**A service will not start and the journal says the schema is ahead.** That node is running an older release than the cluster's state. Finish upgrading it — this is the guard working, not a fault.

```
<bucket> is at schema version N but this build understands M
```

**A service will not start and the journal says no migrations are registered.** The store is at a version this build has no path from, usually because a release was skipped. Upgrade through the intermediate release rather than around it.

```
no migrations registered for <bucket> from version N to M
```

**Nothing seems to have changed after the install.** Check for surviving `spx` processes (`pgrep -ax spx`). This is the single most common upgrade failure, and it reports success either way — see step 2.

**Do not restart one service on its own to fix an upgrade.** Every service is the same binary, so a selective restart is not reliably the thing you changed. Take the target down and bring it back up.
