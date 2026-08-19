---
title: "Spinifex Flaw Remediation Policy (CVSS)"
description: "CVSS-tiered SLAs for identifying, reporting, and correcting software flaws in Spinifex and its direct dependencies, for maintainers and CMMC Level 1 operators."
category: "Security"
sections:
  - overview
tags:
  - security
  - compliance
  - cmmc
  - vulnerabilities
  - cvss
  - patching
resources:
  - title: "NIST SP 800-171 Rev 3"
    url: "https://csrc.nist.gov/pubs/sp/800/171/r3/final"
  - title: "CMMC Level 1 Self-Assessment Guide v2.0"
    url: "https://dodcio.defense.gov/CMMC/Documentation"
  - title: "FIRST CVSS v3.1 Specification"
    url: "https://www.first.org/cvss/v3-1/specification-document"
  - title: "Go govulncheck"
    url: "https://pkg.go.dev/golang.org/x/vuln/cmd/govulncheck"
  - title: "GitHub Dependabot"
    url: "https://docs.github.com/en/code-security/dependabot"
---

# Flaw Remediation Policy

> CVSS-tiered SLAs for identifying, reporting, and correcting software flaws in Spinifex

## Table of Contents

- [Overview](#overview)
- [CMMC Practices Covered](#cmmc-practices-covered)
- [Approach](#approach)
- [1. Identification](#1-identification)
- [2. Severity](#2-severity)
- [3. Remediation SLAs](#3-remediation-slas)
- [4. Reporting Workflow](#4-reporting-workflow)
- [5. Operator Responsibilities](#5-operator-responsibilities)
- [6. Evidence](#6-evidence)
- [7. Operator Checklist](#7-operator-checklist)

---

## Overview

**Audience:** Spinifex maintainers who publish releases, and operators deploying Spinifex into CMMC Level 1 environments.

**Scope:** Flaws in Spinifex's own code and its direct dependencies (Go modules, UI npm packages, GitHub Actions). Operating system, kernel, OVN/OVS, and hypervisor CVEs are the operator's responsibility — see [§5](#5-operator-responsibilities).

## CMMC Practices Covered

| Practice | Title | Objective |
|----------|-------|-----------|
| SI.L1-3.14.1 | Flaw Remediation | [a] Time to identify flaws is specified. [b] Flaws identified in time. [c] Time to report flaws is specified. [d] Flaws reported in time. [e] Time to correct flaws is specified. [f] Flaws corrected in time. |

## Approach

Spinifex ships a short dependency chain and identification is already automated: Dependabot raises security PRs the moment GitHub links a CVE to a shipped dependency, `govulncheck` runs on every PR, and GitHub Security Advisories watch all three `mulgadc/*` repositories.

Sections map to the three scored objectives: [§1](#1-identification) names the identification sources (objectives [a]/[b]); [§3](#3-remediation-slas) and [§4](#4-reporting-workflow) set the report and correct timeframes (objectives [c]–[f]). [§5](#5-operator-responsibilities) delineates layers the operator owns, and [§6](#6-evidence) covers the records needed to demonstrate compliance at assessment.

## 1. Identification

| Source | Frequency | Coverage |
|--------|-----------|----------|
| Dependabot security updates | Immediate — raised as soon as GitHub links an advisory to a dependency | Go modules, GitHub Actions, UI npm packages |
| Dependabot version updates | Weekly | Routine version bumps (non-security) |
| `govulncheck` (CI `lint_and_security` job) | Every PR and push to `main` | Go stdlib + module CVEs reachable in the binary |
| GitHub Security Advisories | Continuous | `mulgadc/spinifex`, `mulgadc/predastore`, `mulgadc/viperblock` |
| Third-party reports (sensitive) | Ad hoc | [GitHub private vulnerability reporting](https://github.com/mulgadc/spinifex/security/advisories/new) |
| Third-party reports (non-sensitive) | Ad hoc | Public GitHub issue, labelled `security` |
| Internal discovery | Ad hoc | Review, audit, red team, incident response |

Dependabot security PRs are merged immediately once CI passes — for dependency CVEs this collapses identify, report, and correct into a single action. The separate weekly cadence applies only to non-security version bumps.

Reporters: use private vulnerability reporting for anything plausibly Critical or High; a public issue is fine for hardening suggestions and non-exploitable bugs. A flaw is **identified** once it appears in any channel above with enough detail to score CVSS.

## 2. Severity

CVSS v3.1 base score from the upstream advisory; otherwise scored by maintainers and the vector recorded on the tracking issue.

| Severity | CVSS v3.1 | Examples |
|----------|-----------|----------|
| Critical | 9.0 – 10.0 | Unauthenticated RCE reachable from tenant networks; `awsgw` auth bypass; master-key disclosure |
| High | 7.0 – 8.9 | Authenticated RCE; intra-service privilege escalation; cluster-internal auth bypass; memory disclosure exposing credentials. |
| Medium | 4.0 – 6.9 | Authenticated DoS; non-secret information disclosure; limited-blast-radius logic errors |
| Low | 0.1 – 3.9 | Heavily mitigated issues — local-only, non-default config, negligible impact |

**Severity modifiers.** Bump one tier **up** if any of the following apply: the flaw is under active exploitation, reaches the master encryption key or cluster CA private key, or allows cross-tenant data access. Bump one tier **down** if the flaw is only reachable by a cluster-internal process already holding credentials equivalent to the attack's outcome.

## 3. Remediation SLAs

Clocks start at the moment of identification ([§1](#1-identification-sources)). All three objectives — identify [a], report [c], correct [e] — are time-bound.

| Severity | Identify | Report | Correct |
|----------|----------|--------|---------|
| Critical | 24 hours | 48 hours, with patch | 48 hours |
| High     | 48 hours | 7 days, with patch | 7 days |
| Medium   | 7 days   | Next release changelog | 30 days |
| Low      | 30 days  | Next release changelog | 90 days |

**Identify** - triaged into a tracking issue, CVSS scored, affected components and versions determined, severity label applied.

**Report** - GitHub Security Advisory is published (for Critical/High) or the fix is described in the release notes (for Medium/Low). Operators are the audience; the report must let them determine whether their deployment is affected.

**Correct** - tagged Spinifex release containing the fix is available for operators to pull. For dependency CVEs, this is a release that bumps the vulnerable module to a fixed version. For first-party code, it is a release containing the patch.

If an upstream embargo applies, the correct-SLA pauses until disclosure; the identify-SLA does not. Record the embargo on the tracking issue.

## 4. Reporting Workflow

1. **Intake** — External findings arrive via GitHub (private for sensitive, public issue otherwise). Maintainers mirror each finding into the internal tracker with CVSS score, affected components, and advisory link. Private disclosures stay private until the advisory is drafted.
2. **Triage** — Validate severity within the identify-SLA, confirm reachability in Spinifex, and either close as not-applicable (with justification) or proceed.
3. **Fix** — Patch lands on `main` via the normal PR + E2E workflow. Commit message references the CVE / GHSA ID.
4. **Release** — Tagged release cut; release notes state affected versions, CVSS, and upgrade guidance. Critical/High get a published GHSA.
5. **Notify** — GitHub release/advisory notifications reach subscribed operators automatically. Out-of-band email notification is triggered only for Critical.

Not-applicable findings (unreachable, already patched, scanner false positive) are still tracked to closure so every identification has an audit trail.

## 5. Operator Responsibilities

Operators own remediation for layers Spinifex does not ship:

| Layer | Responsibility |
|-------|----------------|
| Spinifex releases | Apply released patches within the correct-SLA above. |
| Host OS + kernel | Track distribution advisories (Debian DSA, Ubuntu USN) at matching cadence. |
| OVN / OVS | Patched via the operator's package channel alongside the OS. |
| QEMU / KVM / libvirt | As above — hypervisor CVEs affect tenant isolation. |
| Host AV / EDR agent | See [Malware Protection §2](/docs/security/malware-protection#2-update-requirements-sil1-3144). |

Operators should subscribe to the `mulgadc/spinifex` release feed (Watch → Custom → Releases + Security Advisories) so Critical/High advisories arrive via GitHub's notification channel rather than polling.

## 6. Evidence and Record Keeping

For CMMC assessment, retain the following for at least 12 months:

- Tracking record for every identified flaw (GitHub issue, GHSA, or internal tracker ID) with identify / triage / close timestamps.
- Dependabot history and `govulncheck` CI logs — export before GitHub Actions retention expires if shorter.
- Release notes and GHSAs for each corrected flaw.
- Operator patch-apply records (change tickets, config-management runs) showing releases deployed within SLA.

These demonstrate objectives [b], [d], [f] — "within the specified time" — in practice.

## 7. Operator Checklist

- System security plan references this policy as the identify / report / correct timeframes for Spinifex.
- Change-management records patch-apply timestamps for every Spinifex release.
- Operator subscribes to `mulgadc/spinifex`, `mulgadc/predastore`, `mulgadc/viperblock` releases and advisories.
- OS / kernel / hypervisor patching cadence documented and at least as strict as [§3](#3-remediation-slas).
- Annual review confirms at least one patch cycle completed within SLA in the prior 12 months, with evidence per [§6](#6-evidence).
