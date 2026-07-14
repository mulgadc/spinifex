---
title: "Media Sanitization and Disposal"
description: "Operator guide for sanitizing and disposing of storage media used by Spinifex nodes"
category: "Security"
sections:
  - overview
tags:
  - security
  - compliance
  - cmmc
  - media
  - sanitization
  - disposal
  - decommissioning
resources:
  - title: "NIST SP 800-88 Rev 1 — Guidelines for Media Sanitization"
    url: "https://csrc.nist.gov/pubs/sp/800/88/r1/final"
  - title: "NIST SP 800-171 Rev 3"
    url: "https://csrc.nist.gov/pubs/sp/800/171/r3/final"
  - title: "CMMC Level 1 Self-Assessment Guide v2.0"
    url: "https://dodcio.defense.gov/CMMC/Documentation"
  - title: "ATA/ATAPI Command Set — Secure Erase"
    url: "https://www.t13.org"
  - title: "NVM Express — Sanitize Command"
    url: "https://nvmexpress.org/specifications"
---

# Media Sanitization and Disposal

> Operator guide for sanitizing and disposing of storage media used by Spinifex nodes

## Table of Contents

- [Overview](#overview)
- [CMMC Practices Covered](#cmmc-practices-covered)
- [Approach](#approach)
- [1. Media in Scope](#1-media-in-scope)
- [2. Sanitization Method Selection](#2-sanitization-method-selection)
- [3. Volume-Level Sanitization — Before Reuse (MP.L1-3.8.3 [b])](#3-volume-level-sanitization--before-reuse-mpl1-383-b)
- [4. Whole-Drive and Node Decommissioning — Before Disposal (MP.L1-3.8.3 [a])](#4-whole-drive-and-node-decommissioning--before-disposal-mpl1-383-a)
- [5. Key Destruction](#5-key-destruction)
- [6. Removable and Backup Media](#6-removable-and-backup-media)
- [7. Evidence and Record Keeping](#7-evidence-and-record-keeping)
- [8. Operator Checklist](#8-operator-checklist)

---

## Overview

**Audience:** Operators decommissioning, reassigning, returning under warranty, or disposing of hardware that has been used to run Spinifex nodes in environments subject to CMMC Level 1, or any site that requires documented media sanitization.

**Scope:** Every storage medium that has, or may have, held Federal Contract Information (FCI) as part of a Spinifex deployment — node system disks, Viperblock WAL/chunk disks, Predastore object stores, NATS JetStream disks, removable master-key tokens, backup media, and any loose drives that previously occupied a Spinifex role. Guest VM internal media (disks as seen from inside a tenant VM) is the workload owner's responsibility and out of scope.

## CMMC Practices Covered

| Practice | Title | Objective |
|----------|-------|-----------|
| MP.L1-3.8.3 | Media Disposal | [a] System media containing FCI is sanitized or destroyed before disposal. [b] System media containing FCI is sanitized before it is released for reuse. |

## Approach

NIST SP 800-88 Rev 1 defines three sanitization categories — **Clear**, **Purge**, and **Destroy** — chosen by the confidentiality of the data and whether the medium will leave the operator's control. Spinifex discharges MP.L1-3.8.3 along two tracks:

1. **Volume-level sanitization** for EBS volumes and VM disks returning to the free pool. Handled in-platform by **cryptographic erase** — the per-volume data encryption key (DEK) is deleted, rendering the ciphertext in Predastore S3 irrecoverable. This is the NIST SP 800-88 **Purge**-level method for encrypted media.
2. **Whole-drive sanitization** for physical media leaving a Spinifex node — drive replacement, node retirement, warranty return, resale, or destruction. The operator runs the sanitization; Spinifex does not and cannot reach the firmware commands required. This guide prescribes the method per media type and the records to keep.

The key principle: every piece of media that ever held plaintext FCI (or keys that wrapped FCI ciphertext) must be sanitized to at least **Purge** before release for reuse, and to **Purge** or **Destroy** before leaving operator control. When in doubt, Destroy.

## 1. Media in Scope

Any medium in any of these roles on a Spinifex node has held, or may have held, FCI:

| Medium | Typical hardware | Contents |
|--------|------------------|----------|
| Node system disk | NVMe/SATA SSD | `/etc/spinifex/master.key`, cluster CA key, per-node TLS keys, service configs, logs, journal, swap. |
| Viperblock WAL device | NVMe/SATA SSD | Plaintext of in-flight block writes before chunking. High-churn, short residency, but FCI lands here. |
| Viperblock chunk cache / local backing | NVMe/SATA SSD or HDD | Encrypted volume chunks. |
| Predastore object store | NVMe/SATA SSD or HDD | All S3 object data — AMIs, snapshots, user-uploaded artifacts, IAM state files, tenant data. |
| NATS JetStream disk | NVMe/SATA SSD | IAM NATS token, cluster metadata, pending job state, NATS KV entries (including wrapped DEKs). |
| Removable master-key media | USB flash token, HSM, smart card | Master encryption key (USB-mount path). Destroying this sanitizes every DEK-encrypted volume cluster-wide. |
| Backup media | LTO tape, removable HDD/SSD, off-site cloud backup | Any of the above, point-in-time. |
| BMC / iDRAC / iLO storage | Embedded flash | Console recordings, SEL logs, BMC credentials, cached operator certificates. Sanitize via the BMC "Reset to defaults" / "Erase user data" command before disposal. |
| Switch / router config storage | Embedded flash | Cluster VLAN, management IPs, ACLs. Operator network kit, out of Spinifex scope but noted for completeness. |
| Optical / write-once media | DVD/BD-R | If used to transport keys or images. Always Destroy. |

Any drive whose history cannot be traced — pulled from a spares bin, recovered from a failed host, found unlabelled — must be treated as though it held FCI.

## 2. Sanitization Method Selection

The method depends on (a) media type and (b) whether the media remains inside the protected boundary after sanitization.

| Scenario | Minimum method | Reference |
|----------|----------------|-----------|
| Encrypted volume returning to free pool, DEK deletable | **Cryptographic Erase** (Purge) via DEK deletion | [§3](#3-volume-level-sanitization--before-reuse-mpl1-383-b) |
| SSD / NVMe leaving the node | **Cryptographic Erase** + **Purge** (SANITIZE block-erase or ATA Secure Erase) | [§4.1](#41-nvme-ssd) |
| Magnetic HDD leaving the node, operational | **Purge** (single-pass overwrite with verify, or ATA Secure Erase on drives that support it) | [§4.3](#43-hdd) |
| Magnetic HDD leaving the node, faulty (not writable) | **Destroy** (degauss followed by shred/incinerate) | [§4.3](#43-hdd) |
| Any media leaving operator control (resale, warranty, disposal) | **Destroy** if residual confidentiality concern remains after Purge; otherwise Purge + third-party attestation | [§4.4](#44-destruction) |
| Tape | **Destroy** (degauss + shred) — overwrite is not reliable on LTO with compression | [§6](#6-removable-and-backup-media) |
| Optical write-once | **Destroy** (shred/incinerate) | [§6](#6-removable-and-backup-media) |
| BMC / switch flash | Vendor "factory reset + erase user data"; Destroy chip if procedure is not available | [§1](#1-media-in-scope) |

**Cryptographic erase is only valid when** the key was generated and handled under modern cryptographic hygiene (AES-256 or equivalent, key never exfiltrated, key store itself sanitized) and the encrypted data was not also written anywhere in plaintext. For Spinifex volumes these conditions hold.

**Self-encrypting drives (SEDs)** support a single-command cryptographic erase via the TCG Opal "RevertSP" / `sedutil-cli --revertNoErase` or the ATA `SECURITY ERASE UNIT ENHANCED` that triggers the on-drive MEK rotation. When using an SED as a node system or data disk, record the SED model and method in the decommissioning procedure.

## 3. Volume-Level Sanitization — Before Reuse (MP.L1-3.8.3 [b])

Applies to EBS volumes, AMIs, and snapshots whose underlying storage will be reused for a different tenant or workload.

### 3.1 Cryptographic Erase via DEK Deletion

Deletion is sanitization:

- `DeleteVolume` removes the wrapped DEK from NATS KV. The chunks in Predastore S3 remain but are unrecoverable ciphertext. No further operator action is required.
- `TerminateInstances` with the default `DeleteOnTermination=true` on root volumes performs the same cryptographic erase for the terminated instance's root.
- `DeleteSnapshot` removes the snapshot's DEK wrapper; the underlying chunks become irrecoverable ciphertext.

This is the sanitization-before-reuse path for every in-platform object. Operators do not need to take separate action — the API call is the sanitization.

### 3.2 What Happens to the Physical Storage

Cryptographic erase leaves ciphertext chunks in Predastore. These chunks are eventually overwritten as new volumes reuse the space. For CMMC purposes, the ciphertext without the DEK satisfies Purge. The underlying drive still requires whole-drive sanitization when it eventually leaves the cluster — see [§4](#4-whole-drive-and-node-decommissioning--before-disposal-mpl1-383-a).

## 4. Whole-Drive and Node Decommissioning — Before Disposal (MP.L1-3.8.3 [a])

Applies when a physical drive — or a whole node — leaves the Spinifex cluster: drive swap, node retirement, warranty return, resale, recycling, destruction.

### 4.1 NVMe SSD

Preferred: the NVMe **SANITIZE** command with the **Block Erase** action. Supported on most enterprise NVMe drives.

```bash
# Confirm support
nvme id-ctrl /dev/nvme0 | grep -i sanicap

# Purge — block erase
nvme sanitize /dev/nvme0 --sanact=2

# Poll until complete
nvme sanitize-log /dev/nvme0
```

Fallback for drives without SANITIZE support: NVMe **Format** with Secure Erase set to 1 (`--ses=1`), or vendor tool. Cryptographic Erase (`--sanact=4`) is acceptable for SEDs whose encryption posture is documented; otherwise Block Erase is the safer default.

Boot media: NVMe targets must be the non-boot drive when running from the OS. For the node system disk, boot a sanitization-purpose live USB (e.g. [PartedMagic](https://partedmagic.com/), the vendor's diagnostic ISO) or pull the drive and sanitize in a dedicated sanitization workstation.

### 4.2 SATA/SAS SSD

Preferred: **ATA Secure Erase (Enhanced)** via `hdparm`:

```bash
# Confirm support and not frozen
hdparm -I /dev/sdX | grep -A1 "Security"

# If frozen, power-cycle (suspend/resume or hot-swap) without unplugging the OS disk

# Set password and issue enhanced erase
hdparm --user-master u --security-set-pass p /dev/sdX
hdparm --user-master u --security-erase-enhanced p /dev/sdX
```

Fallback: vendor Secure Erase utility, or `blkdiscard --secure /dev/sdX` on drives that report `TRIM deterministic + RZAT`. A single-pass overwrite with `shred -v -n 1 /dev/sdX` is **not** sufficient for SSDs because of wear-levelling reserve blocks — use Secure Erase or Destroy.

### 4.3 HDD

Preferred: **ATA Secure Erase** (same `hdparm` sequence as [§4.2](#42-satasas-ssd)). Most modern HDDs support it.

Fallback: single-pass overwrite with verify:

```bash
shred -v -n 1 -z /dev/sdX
# or
dd if=/dev/zero of=/dev/sdX bs=1M status=progress && \
  cmp /dev/zero /dev/sdX          # expect "EOF on /dev/zero" only
```

For HDDs that fail or refuse sanitization, or that held high-sensitivity data, **degauss then Destroy** — degaussing alone renders a modern HDD non-functional, so there is no reuse path after degauss. Degaussing requires an NSA/CSS-listed degausser appropriate to the drive's coercivity.

### 4.4 Destruction

When a drive cannot be sanitized (failed, SED with no working password, sanitize aborts) or when leaving operator control with any residual confidentiality concern, Destroy:

- **SSD/NVMe:** shred to ≤2 mm particle size (NSA/CSS EPL-listed shredder) or incinerate. Crushing alone is not sufficient for modern flash — chips can survive.
- **HDD:** degauss (on drives with magnetic platters) followed by shredding, drilling multiple holes through the platters and the head assembly, or incineration.
- **Tape, optical:** shredding.
- **Chain of custody:** transport destroyed media inside tamper-evident containers. The destruction record ([§7](#7-evidence-and-record-keeping)) must name the destruction method, destruction vendor (if third-party), and operator witness.

Third-party destruction vendors must provide a certificate of destruction listing every drive serial. Reconcile against the asset register before the destruction record is closed.

### 4.5 Node Decommissioning Runbook

When retiring a whole node:

1. **Drain** — migrate or terminate all instances hosted on the node; confirm Predastore and Viperblock roles are reassigned if the node held one. Volume DEKs of terminated instances are already cryptographically erased per [§3.1](#31-cryptographic-erase-via-dek-deletion).
2. **Deregister** — raise a change ticket; the operator removes the node from the cluster (`spx admin node remove`) so NATS routes, OVN chassis, and predastore distributed membership drop it.
3. **Stop services and unmount** — `systemctl stop 'spinifex-*'`, `nbdkit` sessions, OVN agent. Confirm `/run/spinifex/nbd/` is empty.
4. **Destroy on-disk keys** — see [§5](#5-key-destruction). This is the single most important step: sanitization of volume data reduces to sanitization of keys once data is encrypted.
5. **Sanitize each drive** by media type per [§4.1](#41-nvme-ssd) – [§4.3](#43-hdd). Every drive bay is sanitized, including any unpopulated cache or WAL devices.
6. **Verify** — capture the output of the sanitize/erase command (exit code + sanitize-log for NVMe, `hdparm -I` "not enabled, not locked" for ATA, overwrite verify for HDD overwrite) into the decommissioning record.
7. **Label** drives "Sanitized — <method> — <date> — <operator>" before they leave the rack or enter a transit container. Unlabelled drives are treated as unsanitized if reintroduced.
8. **Record** per [§7](#7-evidence-and-record-keeping).

## 5. Key Destruction

Cryptographic erase relies on the key being unrecoverable. A drive that previously held `/etc/spinifex/master.key` must have that key destroyed — otherwise an attacker with both the drive image and the key recovers the cleartext even after "erase".

| Key | Location | Destruction |
|-----|----------|-------------|
| `/etc/spinifex/master.key` | Node system disk (every node) | Overwrite the file (`shred -u /etc/spinifex/master.key`) before the drive is sanitized, in addition to drive sanitization. Once the master key is gone cluster-wide, every DEK it wrapped is effectively destroyed. |
| Cluster CA private key (`/etc/spinifex/ca.key`) | Leader node system disk | Same treatment. Loss of the CA key does not sanitize data, but prevents impersonation of the cluster identity post-disposal. |
| Per-node TLS private key (`/etc/spinifex/server.key`) | Each node system disk | `shred -u` before drive sanitization. |
| Wrapped DEKs in NATS KV | `/var/lib/spinifex/nats/` on NATS-carrying nodes | Deleted via the `DeleteVolume` API; the KV record is compacted. Sanitization of the NATS JetStream disk per [§4](#4-whole-drive-and-node-decommissioning--before-disposal-mpl1-383-a) ensures no recoverable copies. |
| Removable master-key media (USB) | USB flash token | Physically destroy the token when the cluster is decommissioned; alternatively retain inside a secured location as evidence of cluster-wide cryptographic erase. Do not reuse for another cluster without full sanitization (see [§6](#6-removable-and-backup-media)). |
| Backup-copy keys | Any off-site or escrow copy | Destroy simultaneously with the primary. A surviving backup defeats the cryptographic erase. |

If even one copy of the master key survives and the encrypted drives are recoverable, cryptographic erase has **not** been achieved. Track every key copy in the device register ([Physical Security Guide §5](/docs/security/physical-security-guide#5-manage-physical-access-devices-pel1-3105)).

## 6. Removable and Backup Media

| Media | Sanitization |
|-------|--------------|
| USB flash (master-key token, key-transport USB, image-import USB) | Destroy. The price of a replacement does not justify the risk of partial wear-levelled sanitization. Record the serial in the device register and the destruction in the decommissioning log. |
| External HDD/SSD (backup) | Treat as [§4.2](#42-satasas-ssd) / [§4.3](#43-hdd). |
| LTO tape | Degauss with a tape-rated degausser, then shred. Overwrite is not reliable under LTO hardware compression and is not accepted by 800-88 for tape Purge. |
| Optical (CD/DVD/BD-R) | Shred or incinerate. |
| Off-site cloud backup | Issue the cloud provider's delete-and-purge API; retain the provider's deletion-confirmation receipt. For cryptographically wrapped backups, destroy the wrapping key cluster-side and retain that deletion as evidence. |
| Printed material (console photos, recovery codes, admin handover sheets) | Cross-cut shred. |

Any removable medium that has entered the protected boundary and been written to must be tracked in the device register (see [Physical Security Guide §5](/docs/security/physical-security-guide#5-manage-physical-access-devices-pel1-3105)) and accounted for at decommissioning.

## 7. Evidence and Record Keeping

For CMMC assessment, retain the following for at least three years (or longer where contract policy requires):

- **Asset register** listing every drive and removable medium by serial number, media type, role in the cluster (system / WAL / chunk / predastore / NATS / backup), and in-service / decommissioned status.
- **Decommissioning record** per drive or node with: date, operator, sanitization method, command output or tool report, verification result, final disposition (reuse / return / destroy), destination (e.g. "warranty RMA #12345", "shredder vendor X, CoD #6789").
- **Certificates of destruction** from third-party vendors, reconciled against the asset register.
- **Key-destruction record** confirming `master.key`, CA key, and per-node keys were shredded before drive sanitization.
- **Exceptions log** — faulty drives that could not be sanitized and were destroyed instead; any sanitize failures and their resolution.
- **Annual attestation** from the operator confirming the procedures in this guide operated for the prior 12 months, signed by the named security owner.

Cross-reference each decommissioning record to the change ticket that drove the retirement, so drive disposal can be audited against cluster state change.

## 8. Operator Checklist

Use this list to confirm a Spinifex deployment meets MP.L1-3.8.3:

- Asset register enumerates every drive and removable medium by serial, media type, and role.
- Every volume-producing path uses encryption so that `DeleteVolume` is sanitization.
- Node-decommissioning runbook exists and matches [§4.5](#45-node-decommissioning-runbook), including the drain → deregister → key-destroy → drive-sanitize sequence.
- Sanitization method is pre-selected per media type per [§2](#2-sanitization-method-selection); tool availability (hdparm, nvme-cli, vendor ISOs, degausser, shredder or destruction vendor) is verified before a decommissioning begins.
- `master.key`, CA key, and per-node TLS keys are shredded before the drive holding them is sanitized.
- Every backup copy of any key is destroyed at the same time as the primary; surviving backup copies are tracked and accounted for.
- Removable media (USB, tape, optical) is destroyed rather than erased unless the medium is an SED with documented cryptographic erase.
- Decommissioning records capture method, tool output, verification, final disposition, and operator; certificates of destruction are reconciled against the asset register.
- Drives awaiting sanitization or awaiting pickup by a destruction vendor are held inside the physical protection boundary ([Physical Security Guide §1](/docs/security/physical-security-guide#1-protected-assets)) with chain of custody logged.
- System security plan references this guide and names the sanitization tooling, destruction vendor (if any), retention period for records, and the security owner attesting annually.
