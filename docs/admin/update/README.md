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
- [Troubleshooting](#troubleshooting)

---

## Overview

Updating Spinifex is the same command used to install it. The installer detects an existing installation, downloads the latest binary and runs any pending configuration migrations before restarting services.

For operators who want to review migrations before they are applied, a manual upgrade path is also supported.

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

## Step 2. Review Pending Migrations

```bash
sudo spx admin upgrade
```

The command prints the current version of each config file, the migrations that would be applied, and a `from → to` description for each. It then prompts for confirmation before making any changes. Answer `n` to abort without touching your config.

## Step 3. Apply Migrations

When you are ready, answer `y` at the prompt, or re-run with `--yes` to apply non-interactively:

```bash
sudo spx admin upgrade --yes
```

## Step 4. Restart Services

Migrations modify config files on disk but do not restart running services. Apply the new config with:

```bash
sudo systemctl restart spinifex.target
```

A restart preserves any running guests — they are not rebooted, and storage returns within seconds. See [Host and Guest Lifecycle](/docs/admin/host-lifecycle) for the full contract.

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
