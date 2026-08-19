---
title: "Physical Security Operator Guide"
seoTitle: "Physical Security Guide for Operators — Spinifex Docs"
description: "Operator guide to physical access controls, visitor handling, access logging, and access-device management at sites hosting Spinifex nodes and network gear."
category: "Security"
sections:
  - overview
tags:
  - security
  - compliance
  - cmmc
  - physical
  - facilities
  - access control
resources:
  - title: "NIST SP 800-171 Rev 3"
    url: "https://csrc.nist.gov/pubs/sp/800/171/r3/final"
  - title: "CMMC Level 1 Self-Assessment Guide v2.0"
    url: "https://dodcio.defense.gov/CMMC/Documentation"
  - title: "NIST SP 800-53 Rev 5 — PE Family"
    url: "https://csrc.nist.gov/pubs/sp/800/53/r5/upd1/final"
  - title: "NIST SP 800-116 Rev 1 — PIV for Physical Access"
    url: "https://csrc.nist.gov/pubs/sp/800/116/r1/final"
---

# Physical Security Operator Guide

> Operator guide for physical access controls, visitor handling, access logging, and access-device management at sites hosting Spinifex nodes

## Table of Contents

- [Overview](#overview)
- [CMMC Practices Covered](#cmmc-practices-covered)
- [Approach](#approach)
- [1. Protected Assets](#1-protected-assets)
- [2. Limit Physical Access (PE.L1-3.10.1)](#2-limit-physical-access-pel1-3101)
- [3. Escort and Monitor Visitors (PE.L1-3.10.3)](#3-escort-and-monitor-visitors-pel1-3103)
- [4. Physical Access Logs (PE.L1-3.10.4)](#4-physical-access-logs-pel1-3104)
- [5. Manage Physical Access Devices (PE.L1-3.10.5)](#5-manage-physical-access-devices-pel1-3105)
- [6. Evidence and Record Keeping](#6-evidence-and-record-keeping)
- [7. Operator Checklist](#7-operator-checklist)

---

## Overview

**Audience:** Operators deploying Spinifex into environments subject to CMMC Level 1, or any site that requires documented physical protection of compute and storage infrastructure.

**Scope:** The physical environment housing Spinifex nodes — the Linux hosts running `spinifex-daemon`, `spinifex-awsgw`, `spinifex-nats`, `spinifex-predastore`, `spinifex-viperblock`, `spinifex-vpcd`, `spinifex-ui`, and the OVN control plane — together with the network equipment, cabling, console/KVM access paths, and backup media that support them. Tenant workloads and operator endpoints (laptops, jump hosts) are out of scope.

## CMMC Practices Covered

PE.L1 objectives are discharged by the operator's facility, not by Spinifex. This guide names the assets, cadences, and evidence required.

| Practice | Title | Objective |
|----------|-------|-----------|
| PE.L1-3.10.1 | Limit Physical Access | [a] Authorized individuals allowed physical access are identified. [b] Physical access to organizational systems, equipment, and operating environments is limited to authorized individuals. |
| PE.L1-3.10.3 | Escort Visitors | [a] Visitors are escorted. [b] Visitor activity is monitored. |
| PE.L1-3.10.4 | Physical Access Logs | [a] Audit logs of physical access are maintained. |
| PE.L1-3.10.5 | Manage Physical Access Devices | [a] Physical access devices are identified. [b] Physical access devices are controlled. [c] Physical access devices are managed. |

## Approach

Spinifex does not mandate a specific access-control product. Operators typically deploy into a facility that already has badge readers, CCTV, and a visitor-management system (Kisi, HID, Lenel, Genetec, Envoy, Traction Guest, paper logs, etc.). Manual procedures (locked cabinet, paper sign-in, tracked key list) are acceptable for small sites provided the evidence trail exists; multi-rack sites should feed an electronic access-control system into the same SIEM used for Spinifex service logs.

## 1. Protected Assets

The physical protection boundary must enclose every item in this table. An asset is "protected" when access to it requires passing through a controlled barrier (locked room, cage, or cabinet).

| Asset | Why it must be inside the boundary |
|-------|-----------------------------------|
| Spinifex node chassis | Host console, BMC, and disks hold `/etc/spinifex/master.key`, cluster CA key, per-node TLS keys, and all tenant volume data. |
| Network switches and routers serving the cluster subnet | Physical access permits traffic capture, port mirroring, and control-plane tampering. |
| Structured cabling (top-of-rack to host, host to storage) | Passive taps are trivial on unprotected cabling. |
| Console / KVM / serial aggregators | Bypass host authentication; reach GRUB, single-user mode, BMC. |
| Backup media (tapes, removable disks, off-site copies) | Carry the same data as primary storage; covered by MP.L1-3.8.3 for disposal. |
| Facility power and HVAC cutoffs serving the rack | Unauthorised de-energise is a denial-of-service and a risk to in-flight writes. |

Nodes deployed in an unstaffed remote or edge location must be installed in a locked enclosure with tamper-evident seals and, where feasible, a sensor (door contact, accelerometer) feeding the central monitoring system. Record the enclosure location, seal serial, and sensor channel in the asset register.

## 2. Limit Physical Access (PE.L1-3.10.1)

### 2.1 Authorized Individuals ([a])

Maintain a written access list naming every individual authorized to enter each protected space. For each entry record:

- Full name and employing organization.
- Role justifying access (e.g. "Spinifex operator", "facilities", "vendor: OEM field service").
- Scope — which protected spaces, and whether escorted or unescorted.
- Start date, scheduled review date, and end date when access is removed.
- Approver (named individual, not a role mailbox).

Review the list at least quarterly and immediately on personnel change (role change, departure, contractor end-of-engagement). Revocations must be effective in the access-control system promptly per operator policy.

### 2.2 Enforcement ([b])

Access to protected spaces must require authentication at the barrier — a badge, PIN, key, biometric, or combination. Unaccompanied access by individuals not on the list in [§2.1](#21-authorized-individuals-a) must not be possible. Specifically:

- Doors and cages: electronic access control (badge/PIN) with door-forced and door-held alarms wired to the monitoring system. Mechanical-only locks are acceptable for lab/edge deployments provided key distribution is tracked under [§5](#5-manage-physical-access-devices-pel1-3105).
- Racks and cabinets holding Spinifex nodes: locked at all times when unattended. Key or combination distribution tracked as an access device under [§5](#5-manage-physical-access-devices-pel1-3105).
- Remote/edge enclosures: locked with tamper-evident seal; seal integrity checked on every site visit and recorded in the visit log.

Shared credentials (one badge used by several people, a rack key left in the cage) are not acceptable.

## 3. Escort and Monitor Visitors (PE.L1-3.10.3)

A visitor is any individual entering a protected space who is not on the authorized-access list in [§2.1](#21-authorized-individuals-a). This includes vendor field engineers, auditors, janitorial staff, and employees of the operator who do not hold Spinifex access.

### 3.1 Escort ([a])

- Every visitor must be signed in by a named authorized escort before entering the protected space.
- The escort must remain physically present with the visitor for the duration of the visit. Passing a visitor between escorts is permitted; leaving a visitor unaccompanied is not.
- Work inside a rack or at a node console requires one escort per visitor.

### 3.2 Monitoring ([b])

Visitor activity must be monitored by at least one of:

- Continuous physical presence of the escort in line-of-sight of the visitor. This is the minimum.
- CCTV coverage of the protected space with recordings retained for at least 30 days. Cameras must cover rack fronts and rears, door entries, and any console/KVM positions.
- For vendor maintenance involving system access (BMC, console, disk swap): a second operator witness on-site, or a recorded screen share if the work is performed from the console. Record the witness or session recording ID in the visitor log.

CCTV is strongly recommended for any facility hosting more than a single rack. It also serves as corroborating evidence for [§4](#4-physical-access-logs-pel1-3104) logs and is useful for incident response.

## 4. Physical Access Logs (PE.L1-3.10.4)

### 4.1 What to Log

Every entry into a protected space must produce a log entry capturing:

| Field | Source |
|-------|--------|
| Identity (name, badge ID, or visitor record ID) | Access control system or sign-in sheet. |
| Timestamp in and timestamp out | Access control system; manual for paper logs. |
| Protected space entered | Door/cage ID. |
| Purpose | Free-text; required for visitors, recommended for authorized staff. |
| Escort (for visitors) | Named individual from the authorized list. |
| Associated change or ticket ID | When the visit is tied to a Spinifex change (upgrade, disk swap, node rebuild). |

### 4.2 Retention and Review

- **Retention:** at least 12 months. CCTV recordings associated with the same visit, where used to discharge [§3.2](#32-monitoring-b), should be retained for the same period.
- **Review cadence:** monthly spot-check of 10% of entries, quarterly full review against the authorized-access list. Discrepancies (unknown badge ID, visitor with no matching sign-in, escort named who was off-site) must be investigated and the finding recorded.
- **Alerting:** out-of-hours entries, repeated failed reads at a single door, and door-forced / door-held events must fire an alert to the on-call operator. Treat these as incidents until triaged.

### 4.3 Integration with Spinifex Logs

Forward physical-access events to the same SIEM or log collector used for Spinifex service logs (see [Malware Protection §3](/docs/security/malware-protection#3-scan-schedule-sil1-3145)) so a `master.key` access attempt can be correlated with a physical entry into the rack.

## 5. Manage Physical Access Devices (PE.L1-3.10.5)

Physical access devices are badges, key cards, PINs, mechanical keys, rack keys, safe combinations, and tamper seals. USB tokens or HSMs holding the cluster CA key or master encryption key also belong in the register.

### 5.1 Identification ([a])

Maintain a device register with one entry per issued device. Minimum fields:

| Field | Notes |
|-------|-------|
| Device ID | Badge serial, key stamp, seal serial, HSM serial. |
| Device type | Badge / mechanical key / PIN / combination / seal / hardware token. |
| Scope | Which barriers it opens, or which asset it protects. |
| Holder | Named individual. Shared holders are not acceptable for badges or PINs. |
| Issued date, issued by | Audit trail for [c]. |
| Returned or revoked date, reason | Populated on personnel change. |

### 5.2 Control ([b])

- Issue only to individuals named in the [§2.1](#21-authorized-individuals-a) access list. Scope of the device must not exceed that individual's authorized scope.
- Badges and PINs must be unique per holder. Mechanical keys that must be shared (e.g. a single rack key) are permitted only when distribution is tracked by check-out/check-in against the register.
- Lost or compromised devices trigger immediate revocation: badges deactivated in the access-control system, PINs changed, affected mechanical locks re-keyed within 30 days, tamper seals re-applied on the next site visit. Record the event and remediation in the register.
- Terminated or reassigned personnel surrender all devices on last day. Mark returned in the register.

### 5.3 Management ([c])

- **Inventory review:** at least quarterly, reconcile the device register against (a) the access-control system's badge database, (b) the physical key count, and (c) the authorized-access list in [§2.1](#21-authorized-individuals-a). Discrepancies must be resolved before the review is marked complete.
- **Rotation / re-keying:** mechanical locks re-keyed on loss of any key, and at least every five years. Default combinations (safe, cabinet, BMC default passwords at install) must be changed before the device enters service.
- **Spares and master keys:** held in a secured location (locked cabinet or safe inside a protected space), accessible only to named individuals, with any access itself logged under [§4](#4-physical-access-logs-pel1-3104).

## 6. Evidence and Record Keeping

For CMMC assessment, retain the following for at least 12 months (longer where facility or contract policy requires):

- **Authorized access list** ([§2.1](#21-authorized-individuals-a)) with review dates and approver names.
- **Access-control system configuration** showing which badges are permitted at which readers, exported or screenshotted at each quarterly review.
- **Physical access logs** ([§4](#4-physical-access-logs-pel1-3104)) and, where applicable, CCTV retention policy and sample recordings.
- **Visitor logs** with escort names for every visit.
- **Device register** ([§5.1](#51-identification-a)) with issue, return, and revocation history.
- **Incident records** for door-forced/door-held events, lost devices, failed seal checks, and any access-review discrepancies, with remediation outcomes.
- **Annual review attestation** from the facility or security owner confirming the controls above operated for the prior 12 months.

## 7. Operator Checklist

Use this list to confirm a site meets the four CMMC practices before admitting Spinifex nodes to a production cluster:

- Authorized-access list exists, names every protected space, is reviewed at least quarterly, and has an approver for every entry.
- Every protected space enforces authentication at the barrier; shared credentials are not in use for badged/PIN'd spaces.
- Remote/edge enclosures are locked, sealed, and listed in the asset register with seal serials recorded.
- Visitor procedure is documented: sign-in, escort (one-per-visitor at equipment), sign-out.
- CCTV or equivalent monitoring covers the rack fronts, rears, and console positions, with 30-day minimum retention.
- Physical access logs are produced for every entry, retained 12 months, and reviewed on the cadence in [§4.2](#42-retention-and-review).
- Door-forced, door-held, and out-of-hours events alert the on-call operator.
- Physical access events forwarded to the SIEM used for Spinifex service logs (see [Malware Protection §3](/docs/security/malware-protection#3-scan-schedule-sil1-3145)).
- Device register reconciles quarterly against the access-control system and physical key count; discrepancies are closed before sign-off.
- Lost-device and termination procedures trigger immediate revocation per [§5.2](#52-control-b).
- System security plan references this guide and records the facility, access-control product, CCTV retention, and log-forwarding destination.
