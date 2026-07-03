---
title: "Updating Spinifex"
description: "Upgrade an existing Spinifex installation to the latest release."
category: "Administration"
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
