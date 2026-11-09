---
title: "UEFI Variable Store"
seoTitle: "UEFI Variable Store Durability and Recovery — Spinifex Docs"
description: "Where a UEFI instance's variable store lives, how much a node loss can lose, how owed write-backs recover, and which firmware differences refuse a launch."
category: "Admin"
tags:
  - uefi
  - firmware
  - instances
  - storage
resources:
  - title: "Spinifex Repository"
    url: "https://github.com/mulgadc/spinifex"
  - title: "Host and Guest Lifecycle"
    url: "/docs/host-lifecycle"
---

# UEFI Variable Store

> Where a UEFI instance keeps its firmware variables, how durable they are, and what an operator sees when something goes wrong.

## Table of Contents

- [Overview](#overview)
- [Where the Store Lives](#where-the-store-lives)
- [Durability](#durability)
- [Owed Write-Backs](#owed-write-backs)
- [Starting on Another Node](#starting-on-another-node)
- [Firmware Compatibility](#firmware-compatibility)
- [Instances Refused at Launch](#instances-refused-at-launch)
- [Terminate](#terminate)
- [Images](#images)
- [Inspecting Stores](#inspecting-stores)
- [Metrics](#metrics)

---

## Overview

An instance whose boot mode is `uefi` or `uefi-preferred` boots through UEFI firmware, which keeps its variables (`BootOrder`, `BootNext`, secure-boot state) in a VARS region. Spinifex gives each such instance its own variable store, so those variables survive a reboot, a stop and start, and a start on a different node. A direct-boot microvm boots without UEFI and has no store.

RunInstances seeds the store from the launching image's store when the image carries one (see [Images](#images)), and from the launching node's VARS template otherwise. From then on the node running the instance writes the guest's changes back to the platform object store, and every later start boots from the last change that landed there.

## Where the Store Lives

A store has three parts.

| Part | Where | What it holds |
|---|---|---|
| Store objects | The platform object store, in the bucket set as the node's Predastore bucket, under `uefi/<instance-id>/` | The VARS region, gzip-compressed, one object per write. Each write goes to a fresh key, so no write replaces an object a record points at. Each object's metadata names the instance, the writer node, the source (`template`, `write-back` or `image:<ami-id>`) and the firmware it was written under. |
| Store record | KV bucket `spinifex-uefi-stores`, keyed by instance ID | The instance and account, the state (`live` or `deleting`), the claim generation that fences writes, and the pointer: the one object the instance boots from, with the firmware it was written under, the writer node and the write time. |
| Local copy | `<data-dir>/uefi/<instance-id>/` on the node running the instance (`/var/lib/spinifex/uefi/<instance-id>/` by default) | `vars.fd`, the file QEMU opens as pflash unit 1, and `seeded.json`, which records the key the copy came from or last landed at, the claim generation, the firmware, and whether a write-back is owed. |

The record outlives the instance record, so the store's deletion obligation is not lost when a terminated instance record expires. The bucket is never recreated empty if it goes missing, because an empty bucket would report every store as having no obligations.

Only one node may write a store at a time. That node holds the store's claim, `uefi.<instance-id>` in the cluster's sync bucket, and a write moves the pointer only while the record still carries that claim's generation. A node that takes the claim stamps the record with its own generation before it boots, so a write from the previous holder is refused rather than landing over newer variables.

## Durability

The running node watches `vars.fd` for writes. Once the guest changes a variable, the change is written back 2 seconds after the last write, or 30 seconds after the first write in the window, whichever is sooner. A guest that writes continuously therefore still lands a write-back every 30 seconds. A node that cannot watch the file polls it every 10 seconds instead, and logs `Cannot watch the UEFI variable store; polling it for changes instead`.

A write-back puts the region at a fresh key (bounded at 10 seconds) and then moves the store record's pointer to it (bounded at 10 seconds). A write-back that fails is retried 30 seconds later, and the changes stay owed until one lands.

When an instance stops, the node writes the store back once QEMU has exited and before the instance's volumes unmount, then releases the claim.

**What a node loss can lose:** the variable changes made since the last write-back that landed. While the object store is reachable that is at most the 30-second window plus the write in flight. While write-backs are failing, it is every change since the last one that landed, however long the failures last. The node's local copy still holds those changes, and they are recovered if the node comes back before another node takes the store over (see below).

## Owed Write-Backs

A write-back is owed whenever `vars.fd` may hold changes no object has. The node records that in `seeded.json` as `write_back_owed` when a window opens and clears it when the write lands. A stop whose write-back fails keeps the claim, marks the copy owed and logs `The stop could not write the UEFI variable store back; this node keeps its claim and retries`.

Each node runs a `uefi-write-back` reaper on the garbage-collection sweep, every 2 minutes while KV is healthy. For each local copy whose instance is not running on the node, it:

- writes an owed copy back and releases the claim once it lands (`trigger=reaper` on the write-back metric);
- releases the claim over a clean copy once the stream leader confirms the record still points at the object the copy came from;
- drops an owed copy whose claim another node now holds, or whose record a later claim has stamped, logging `vm/gc: dropping an owed UEFI variable store write-back; its changes are lost` and counting `mulga.vm.uefi_store.write_backs_lost`;
- removes the copy of a store that is being deleted.

The same reaper starts a writer for a running guest that has none, such as one whose reconnect after a daemon restart could not read the claim. A guest that outlives a daemon restart has its writer resumed with the window open, so any change the restart missed is written back.

The reaper also deletes objects no pointer names under each prefix its write-backs touched, keeping the pointer's object and anything younger than 10 minutes.

An operator does nothing to recover an owed write-back on a node that is up: the reaper retries until it lands. If it keeps failing, the node's log carries `Failed to write the UEFI variable store back` with the error, and the fix is whatever stops the node writing to the object store.

## Starting on Another Node

A start on a node that does not hold the claim takes it if it is free. If another node holds it, the start is refused while that node's heartbeat is fresh (under 60 seconds old) or cannot be read, because that node may still land newer variables. StartInstances returns `InsufficientInstanceCapacity` for that refusal, and the node logs `Refusing to start: another live node holds the UEFI variable store claim`. This is what an operator sees when a stopped instance's owed write-back has not landed yet: start it on the node that holds the claim, or let the reaper land the write-back and release the claim.

Once the holder's heartbeat is 60 seconds old or absent, the starting node takes the claim over and logs `Took over the UEFI variable store claim from a node whose heartbeat has stopped; variable changes it never wrote back are lost`. If the old node comes back, its reaper finds the claim held elsewhere and drops its owed copy.

A start on the node that holds the claim under its copy's generation boots that copy, because it may carry changes no object has.

## Firmware Compatibility

A store records the firmware it was written under: architecture, VARS size and the SHA-256 of the CODE blob. A node's firmware is the first installed pair from its firmware candidates (`/usr/share/OVMF/OVMF_CODE_4M.fd` and `OVMF_VARS_4M.fd` first on x86_64, `/usr/share/AAVMF/AAVMF_CODE.fd` and `AAVMF_VARS.fd` first on arm64).

| Difference | Result |
|---|---|
| Architecture | The launch fails before the claim is taken, so the store is left untouched. |
| VARS size | The launch fails before the claim is taken, so the store is left untouched. pflash needs a byte-exact region. |
| CODE hash only | The instance boots. The node logs `UEFI variable store was written under a different CODE build; booting it` and counts `mulga.vm.uefi_store.firmware_mismatches` with `kind=code`. |

A refused launch logs `UEFI variable store firmware does not match this node; refusing the launch` naming both firmwares, counts the mismatch with `kind=arch` or `kind=vars_size`, and records a `Server.InternalError` state reason naming both firmwares.

Every node of an architecture should carry the same firmware package. A node with a different VARS size cannot boot any store written elsewhere, and a node with a different CODE build boots stores with a counted warning on every launch.

## Instances Refused at Launch

A UEFI instance is refused before anything about it changes when:

- it still carries an EFI volume request (`<root>-efi`), which no node boots;
- it has no store record, its record is `deleting`, or its record names no pointer;
- its record cannot be read as valid.

These starts fail with `Server.InternalError`, and the node logs `refusing to launch a UEFI instance without exactly one variable store` or `Refusing to launch a UEFI instance without a variable store` with the reason. A start whose store object is missing or does not decode to the node's VARS size also fails, and retrying does not help.

## Terminate

Terminate moves the store record to `deleting`, releases a claim the node holds, and removes the node's local copy. It does not delete objects. A cluster-wide `uefi-store` reaper deletes every object under the instance's prefix, then the claim, then the record, and only for an instance whose terminate is decided. It completes a terminate whose mark failed from the terminated instance record. A `deleting` record whose instance is not terminating is left for an operator and logged as `uefi-store: refusing to delete the store of an instance that is not terminating`.

## Images

An image made from a UEFI instance carries a copy of the instance's variable store, so instances launched from it start with the same variables.

- **CreateImage** reads the instance's store record from the stream leader and copies the object its pointer names to a fresh key under `uefi/<ami-id>/`, before the root snapshot is taken. The AMI document records that key and its firmware. The image therefore carries the last write-back that landed, not changes still waiting in the write-back window. An instance with no store record, or a record with no pointer, gives an image with no store. A store being deleted refuses the request with `IncorrectInstanceState`.
- **CopyImage** copies the source image's store to a fresh key under the new image's prefix, so deregistering either image leaves the other's store intact.
- **RunInstances** from an image that carries a store seeds the new instance from it, with the object source `image:<ami-id>`. A store whose CODE hash differs from the node's seeds with a warning and counts `kind=code`. A store whose architecture or VARS size differs cannot boot on the node, so the instance is seeded from the node's VARS template instead, logged as `The image's UEFI variable store cannot boot under this node's firmware; seeding from the VARS template` and counted as `kind=image`.
- **DeregisterImage** deletes the image's store objects before the AMI document. If that delete fails, the request fails with `Server.InternalError` and the image stays, so a retry finishes it. `spx admin images remove` deletes the store first in the same way.

A CreateImage or CopyImage that fails after copying the store can leave an object under `uefi/<ami-id>/` with no AMI document, which `spx admin volumes orphans` reports.

## Inspecting Stores

`spx admin volumes orphans` reports UEFI variable stores nothing owns before it reports orphaned volumes:

- `uefi/i-*/` prefixes with neither an instance record nor a store record;
- `uefi/ami-*/` prefixes with no AMI document;
- prefixes named for neither an instance nor an image;
- store records that are not `deleting` and have no instance record.

It only reads, and attaches to the buckets rather than creating them. It never deletes: an absent instance record is not proof of a terminate, so settling each one is an operator decision.

To read one instance's store record:

```bash
nats kv get spinifex-uefi-stores i-0123456789abcdef0
```

## Metrics

| Metric | Meaning |
|---|---|
| `mulga.vm.uefi_store.seeds` | UEFI launches by where the store came from: `source=local` (the node's copy) or `source=object`. |
| `mulga.vm.uefi_store.write_backs` | Write-backs by `outcome` (`landed`, `failed`, `fenced`, `deleting`) and `trigger` (`change`, `stop`, `reaper`). |
| `mulga.vm.uefi_store.write_back_skips` | Writers stopped without writing back, by the path that stopped them. |
| `mulga.vm.uefi_store.write_backs_lost` | Owed write-backs dropped because another node took the claim over. |
| `mulga.vm.uefi_store.pruned_objects` | Superseded store objects a prune deleted. |
| `mulga.vm.uefi_store.firmware_mismatches` | Launches whose store was written under different firmware, by `kind`. `kind=code` boots the store, and `kind=image` seeds a new instance from the VARS template instead of the image's store. |
| `mulga.vm.uefi_store.claim_refusals` | Starts refused because another node holds the claim and may be live, by `reason`. |
| `mulga.vm.uefi_store.claim_takeovers` | Claims taken over from a node whose heartbeat had stopped. |
