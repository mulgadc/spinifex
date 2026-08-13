---
title: "Updating Spinifex"
description: "Upgrade an existing Spinifex installation to the latest release."
category: "Admin"
tags:
  - update
  - upgrade
  - migrate
resources:
  - title: "Spinifex Repository"
    url: "https://github.com/mulgadc/spinifex"
  - title: "Single-Node Install"
    url: "/docs/install"
---

# Updating Spinifex

> Upgrade an existing Spinifex installation to the latest release.

## Table of Contents

- [Overview](#overview)
- [Instructions](#instructions)
- [Manual Upgrade](#manual-upgrade)
- [Checking for Unit Drift](#checking-for-unit-drift)
- [Troubleshooting](#troubleshooting)

---

## Overview

Updating Spinifex is the same command used to install it. The installer detects an existing installation, downloads the latest binary and runs any pending configuration migrations before restarting services.

For operators who want to review migrations before they are applied, a manual upgrade path is also supported.

> [!WARNING]
> **Swapping the `spx` binary alone is not an upgrade.** Systemd unit files (`KillMode`, `TimeoutStopSec`, drain ordering, and similar) are written once at install time and never re-asserted just because a new binary is in place — a node "upgraded" by replacing `/usr/local/bin/spx` directly keeps running whatever units it was first installed with, which can silently disagree with the new binary's behaviour. Re-running the installer always reinstalls units unconditionally, so it is unaffected. `spx admin upgrade` now reconciles units too, so it closes this gap for operators who update the binary by hand. See [Checking for Unit Drift](#checking-for-unit-drift).

> [!WARNING]
> **Object storage is exempt when upgrading from v1.15.0 or earlier to v1.16.0.** Predastore's configuration schema and on-disk layout changed with the object storage cutover in v1.16.0, and no migration converts an installation from before it. `spx admin upgrade` will not report anything pending for `predastore.toml`, and updating the binary over such an installation leaves Predastore unable to start. Those clusters have to be re-initialised from scratch, which discards their stored objects — export anything you need first. Upgrades between v1.16.0 and later releases are unaffected.

> [!WARNING]
> **AMI metadata is exempt when upgrading from v1.16.0 or earlier to the release carrying the EBS-provider decoupling.** AMI metadata moved to `ebsmetadata` documents, and the legacy path that read it from `ami-<id>/config.json` was removed rather than migrated. There is no prefix scan, no fallback and no backfill at daemon start, so an AMI imported before the change becomes invisible to the control plane afterwards: `describe-images --image-ids` answers `InvalidAMIID.NotFound` and launches fail with `AMI has no snapshot ID, cannot perform zero-copy clone`. As with object storage above, `spx admin upgrade` reports nothing pending, because the gap is in stored data rather than in a config file.
>
> **Re-import affected AMIs after upgrading**, using [`spx admin images import`](/docs/admin/spinifex-admin-cli), which writes metadata in the new location. If the source images are no longer available, the installation has to be re-initialised. Verify before you rely on it — an AMI is only healthy if it resolves by ID, not merely if it appears in the list:
>
> ```bash
> aws ec2 describe-images --image-ids <ami-id>
> ```
>
> This warning covers AMI metadata specifically, which is what has been observed. Check any other imported state you depend on before upgrading a cluster you cannot rebuild.

## Instructions

## Step 1. Re-run the Installer

```bash
curl -fsSL https://install.mulgadc.com | bash
```

That's it. The installer will:

1. Download and install the latest Spinifex binary.
2. Reinstall systemd units so new services are picked up.
3. Run any pending configuration migrations automatically (equivalent to `spx admin upgrade --yes`).
4. Restart `spinifex.target` if the services were already running.

## Step 2. Verify

```bash
export AWS_PROFILE=spinifex
aws ec2 describe-instance-types
```

If this returns a list of instance types, your upgrade is complete.

## Manual Upgrade

If you prefer to review pending migrations before they are applied, Spinifex supports running `spx admin init` to allow you to verify config file migrations.

## Step 1. Install the New Binary Without Running Migrations

```bash
curl -fsSL https://install.mulgadc.com | INSTALL_SPINIFEX_SKIP_MIGRATE=1 bash
```

The installer will download the new binary and reinstall systemd units, but will **not** apply any configuration migrations.

## Step 2. Review Pending Changes

```bash
sudo spx admin upgrade
```

The command prints the current version of each config file and systemd unit, the migrations and unit replacements that would be applied, and a `from → to` description for each. It then prompts for confirmation before making any changes. Answer `n` to abort without touching config or units. Use `--dry-run` instead of the prompt to only report and never apply.

## Step 3. Apply Changes

When you are ready, answer `y` at the prompt, or re-run with `--yes` to apply non-interactively:

```bash
sudo spx admin upgrade --yes
```

This requires root: config files are typically owned by their service user, but writing `/etc/systemd/system` needs root. Run the whole command with `sudo`, not just parts of it.

## Step 4. Restart Services

Migrations modify config files on disk but do not restart running services, and unit reconciliation deliberately never restarts anything either — it writes the unit and runs `systemctl daemon-reload`, so the fix applies to the *next* stop of that service without disturbing a running guest. Apply a config change with:

```bash
sudo systemctl restart spinifex.target
```

A restart preserves any running guests — they are not rebooted, and storage returns within seconds. See [Host and Guest Lifecycle](/docs/admin/host-lifecycle) for the full contract.

## Checking for Unit Drift

`spx admin upgrade --dry-run` reports whether a node's installed systemd units match the ones shipped in the running `spx` binary, without prompting or changing anything — the fastest way to answer "are this node's units current?" in a support conversation:

```bash
spx admin upgrade --dry-run
```

Each unit is reported as one of:

- **up to date** — installed content matches the embedded copy.
- **stale, will replace** — the installed marker version is older than the embedded one (or has no marker at all, which is version 0 — every node installed before units were versioned).
- **missing → will install** — no unit installed under that name.
- **operator-modified, not touched** — the installed marker version matches, but the content differs. `spx admin upgrade` never overwrites this case; review it with `systemctl cat <unit>` and reconcile by hand with `systemctl edit`.

Run `sudo spx admin upgrade --units-only --yes` to reconcile units without touching config, or `--skip-units` to do the reverse. A replaced unit is backed up alongside the original as `<unit>.pre-reconcile-<from>to<to>.<unix-timestamp>` before being overwritten.

This covers the 16 core units installed by `install_systemd()` (`spinifex-*.service`, `spinifex.target`, `spinifex.slice` and friends). Firstboot, banner, bridge and getty units written by the installer, and the auxiliary units Ansible manages (`wattle-wan-veth-persist`, `wattle-mgmt-bridge-persist`, `obs-agent`), are separate lifecycles with no overlap in unit names and are out of scope for this reconciler.

## Troubleshooting

### No Pending Config Migrations

```
No pending config migrations.
```

Your config is already at the latest version. Nothing to do.

### No Spinifex Installation Found

```
No Spinifex installation found at /etc/spinifex
Run 'spx admin init' first.
```

`spx admin upgrade` requires an initialized installation. If this is a fresh host, follow the [Single-Node Install](/docs/install) guide instead.

### Migration Failure

If a migration fails, the installer and `spx admin upgrade` exit non-zero and leave the config in its prior state where possible. Review the error output, then re-run `sudo spx admin upgrade` once the underlying issue is resolved.

### Services Did Not Pick Up New Config

Migrations edit config files on disk but the running daemons continue to use the config they loaded at start-up. Restart with:

```bash
sudo systemctl restart spinifex.target
```

A restart preserves any running guests — they are not rebooted, and storage returns within seconds. See [Host and Guest Lifecycle](/docs/admin/host-lifecycle) for the full contract.

### A Node Answers Some Requests as if It Were Still on the Old Build

Replacing `/usr/local/bin/spx` while `spinifex.target` is running does **not** move the running services onto the new binary. They keep executing the replaced file's now-unlinked inode until each unit restarts, so a node can serve the old build indefinitely. Only a service that happens to restart for its own reasons picks the new one up, which leaves a node running a mixture.

This is easy to miss, because the request handlers are NATS queue-group workers spread across nodes: one skewed node in three answers roughly one request in three with the old behaviour, which reads as an intermittent fault rather than a broken node.

Check for it with:

```bash
sudo spx admin preflight
```

Any unit reported `Stale` with kind `service` is running a replaced binary. The check covers every `.service` unit this build ships, and exits non-zero when it finds one, so it also works as a gate in a script. To see the raw state instead:

```bash
for u in spinifex-daemon spinifex-awsgw spinifex-viperblock spinifex-vpcd spinifex-ui spinifex-predastore; do
  pid=$(systemctl show -p MainPID --value "$u")
  [ "$pid" != "0" ] && printf '%-22s %s\n' "$u" "$(sudo readlink /proc/$pid/exe)"
done
```

Any line ending `(deleted)` is running a replaced binary. Restart the target to clear it:

```bash
sudo systemctl restart spinifex.target
```

Re-running the installer avoids this entirely — it restarts services after installing. Prefer it over copying a binary onto a live node.

### Instances Fail to Launch After an Upgrade

```
AMI has no snapshot ID, cannot perform zero-copy clone
```

Or `describe-images --image-ids` reports `InvalidAMIID.NotFound` for an AMI that still appears in the unfiltered `describe-images` list. Two different causes produce this, so check them in order:

1. **A node running a replaced binary**, per the previous entry. Suspect this first if the failure is intermittent — the same command succeeding on some attempts and failing on others is characteristic.
2. **AMI metadata predating the EBS-provider decoupling**, per the warning at the top of this page. This is consistent rather than intermittent, and is resolved by re-importing the AMI.

### Root Privileges Required to Write Systemd Units

```
root privileges required to write systemd units (writing to /etc/systemd/system): ...
Re-run as root to apply the unit changes reported above: sudo spx admin upgrade --units-only --yes
```

`spx admin upgrade` computes and prints unit drift without needing root, but writing `/etc/systemd/system` does. Nothing is written when this happens — re-run the full command with `sudo`.

### Operator-Modified Unit Reported, Not Replaced

A unit whose installed marker version matches the embedded one but whose content differs is never overwritten — this is a deliberate safety property so a hand-tuned unit does not get silently reverted. Compare it against the shipped copy and decide whether to keep, discard or merge the local change:

```bash
systemctl cat <unit>
```

If you want the shipped version, remove the local override and re-run `spx admin upgrade`; systemd falls back to the packaged unit and it reconciles as up to date.
