# EKS add-on v2: release membership and activation (case study)

Code read at `25b3efdec`; not verified live.

**Status:** Draft for review; design dependency only, nothing implemented.

This is the design dependency for prerequisites P1 (release membership), P2 (activation record) and P5 (controlled rollback) of `docs/package-boundary/eks-addon-v2-contract-design.md` Section 3, plus the finalisation action of R16 (`docs/package-boundary/eks-addon-lifecycle-design.md`).
It is written as an EKS case study: every finding is classed as **(a)** a generic compatibility or activation concept that may be reusable later, **(b)** an EKS add-on implementation need, or **(c)** out-of-scope platform-wide release orchestration.
Paths are relative to `spinifex/` unless they start with `docs/`; `mulga/` paths are relative to the umbrella repo.

## Standing

ADR-0006 (`mulga/docs/adr/0006-contract-guest-and-persisted-state-evolution.md`) is accepted and cited by clause: S1 (boundary declaration), S2 (N/N-1 for a coordinated Regional release), S3 (no timing heuristic as capability proof; capability invalidated on replacement or rollback), S4 (an older writer that discards fields is not compatible), S5 (prepare, activate, migrate, finalise; scope limited to components that read, write or enforce the contract; controlled rollback), S6 (guest legacy mode) and S8 (evidence).
ADR-0006's non-goals leave the membership data source and the `spx admin` command shape to implementation, and its Consequences say the add-on work must not decide the platform-wide release mechanism by itself.
ADR-0003:S2 rules out a shared abstraction without two real consumers.
Q-75 and Q-76 (`mulga/docs/specs/requirements/resource-lifecycle.md:261,275`) are Decided requirements; Q-183 and Q-184 are Open and used as context only.
R1 to R16 and V1 to V10 are applied, not reopened; V9 keeps activation EKS-add-on-specific.

## 1. Facts this design depends on

| Fact | Evidence | Class |
|---|---|---|
| No cluster-visible record of the release each process runs; no activation gate | `docs/package-boundary/release-and-version-inventory.md` Section 7 (G1, G2) | a |
| Only awsgw receives the ldflags build version; the daemon never does | `operator/cli/version.go:10-15`, `operator/cli/service.go:613`, `runtime/roles/awsgw/awsgw.go:59-71` | b |
| Daemons rewrite `heartbeat.<node>` every 10 s in `spinifex-cluster-state` (1 h TTL), with no build or capability field | `daemon/heartbeat.go:9,41-72`, `daemon/jetstream.go:180-191,224-248` | b |
| The only prefix lister of that bucket reads `heartbeat.`; other keys are read by exact name | `runtime/compute/cache/liveness.go:25,44-49,103-123`, `daemon/jetstream.go:327,362,408` | b |
| Liveness already treats a missing or unreadable heartbeat as Unknown, never Stale | `runtime/compute/cache/liveness.go:32-41,74-95` | a |
| Every daemon, N-1 included, answers a per-node health subject without any build field | `daemon/daemon.go:1030`, `daemon/daemon_handlers.go:337-360`, `contracts/cluster/v1/health.go:15-24` | b |
| awsgw answers admin-only `GetVersion` for itself only; it writes nothing to cluster state | `gateway/spinifex.go:16-17,53-56,61-62`, `runtime/roles/awsgw/awsgw.go:584-586` | b |
| Expected membership is the node config map with per-node services (`daemon`, `awsgw`, ...) | `bootstrap/config/config.go:20-28,186-197,544-562` | a |
| A Region is one formed Spinifex cluster | Q-72 (`mulga/docs/specs/requirements/resource-lifecycle.md:215-225`) | a |
| Every daemon serves every `eks.*` add-on subject in queue group `spinifex-workers` | `daemon/daemon.go:1089-1122` | b |
| Reconciler lease holder and GC sweep holder are daemons identified by node name | `daemon/eks_deps.go:80`, `handlers/eks/cluster_reconciler.go:395,448`, `daemon/daemon.go:2336-2348,2411-2416` | b |
| awsgw reads cluster meta for internal-route authz and relays fetch and report | `gateway/eks/internal_authz.go:48-80,143-170`, `gateway/eks.go:40-42,57` | b |
| The v1 listing reads manifest keys, and a v1 guest removes any add-on absent from it | `handlers/eks/addons.go:132-156` | b |
| Bumping a bucket stamp makes N-1 refuse the whole bucket | `foundation/state/migrate/kv.go:55-67,81-88`, `handlers/eks/store.go:28-29,197-208` | a |
| No durable operator audit log; the account-deletion job is the one durable operator record (client token, `kv.Create` claim, replay) | `gateway/requestaudit.go:17-38`, `gateway/admin_deleteaccount.go:80-97,359-386` | a |
| `spx admin eks` exists with one subcommand reaching the daemon over NATS | `operator/cli/admin_eks.go:14-77` | b |

## 2. (a) Generic concepts this case study relies on

These are concepts and invariants, not a protocol specification or a package.

| Concept | Meaning |
|---|---|
| Contract scope | The roles that read, write or enforce one contract (ADR-0006:S5). Other roles are never consulted, so no global "everything upgraded" dependency arises. |
| Slot | (Region, node, role), derived from configured membership, never from who reports. |
| Incarnation | Random per process start; a restart is a new incarnation. |
| Capability token | Compiled into the binary; never configurable; a build version string is diagnostic only. |
| Self-report | Written only by the process it describes; additive to existing storage. |
| Probe | One per-slot request at evaluation time proving the current occupant; a pre-protocol binary answers without the new fields, which is positive legacy evidence. |
| Legacy signal | A record the pre-protocol binary already rewrites wholesale; a fresh rewrite without the protocol marker means a legacy occupant. |
| Activation record | One per contract and scope, no TTL, single-CAS transitions, audit inside the record. |
| Activation generation | Advanced on each activation; long-lived consumers' capability proofs are tagged with it, so rollback invalidates them (ADR-0006:S3). |

| Invariant | Why |
|---|---|
| Unknown is never compatible: `stale`, `absent`, `unreachable` and `unexpected` slots block exactly as `legacy` does | Q-75; ADR-0006:S3 |
| Time can only make the answer more conservative: staleness moves a slot from capable to stale, never the reverse | Ageing out removes evidence, never manufactures it; the staleness threshold is the only time constant and is never a fence |
| Ageing out never removes an expected slot; a dead member blocks until a capable occupant reports or the node leaves configured membership | Slots come from configuration |
| Prepare snapshots every slot's incarnation; activate requires the same incarnations | Replaces a settle window with identity (no timing heuristic) |
| No automatic transition; only an operator action moves the state | Q-75: finalisation never follows a binary start |
| Unreadable activation state selects neither old nor new semantics | Fail closed |
| No bucket stamp bump as the fence | Stamp refusal is all-or-nothing (Section 1) |
| Rollback is freeze, drain, deactivate, then downgrade; deactivation needs no binary evidence because it moves toward shared semantics | ADR-0006:S5 |
| Long-lived consumers never gate activation; they gate per resource, and may gate finalisation | ADR-0006:S5 step 2, S6 |

State machine (generic):

| From | To | Evidence |
|---|---|---|
| absent or `PREPARED` | `PREPARED` | All in-scope slots capable; one config epoch; no unexpected slot; no unresolved legacy observation; contract readiness; snapshot stored |
| `PREPARED` | `ACTIVE` | Same, plus incarnations equal the snapshot |
| `ACTIVE` | `DEACTIVATING` | Authorisation only; freezes N-only mutations |
| `DEACTIVATING` | `PREPARED` | Zero outstanding N-only obligations; every record in the N-1-readable form |
| `DEACTIVATING` | `ACTIVE` | Activation evidence again (abort rollback) |
| `ACTIVE` | `FINALISED` | Slots capable; no legacy observation since activation; migration complete; affected long-lived consumers proven; explicit confirmation |

Concurrent operators: every transition is one CAS on the record revision with a client token; the loser gets the current state and changes nothing, and a repeat with the same token returns the recorded outcome.
A crash before the CAS changes nothing; after it, the new state already carries its audit entry.

## 3. (b) EKS add-on implementation needs

### 3.1 Relevant participants

| Participant | Why relevant | Must report |
|---|---|---|
| Every configured `daemon` slot | Any daemon may serve any add-on mutation; N-1 DROPs, OVERWRITEs and ERASEs (`docs/package-boundary/eks-addon-v1-compatibility.md`) | Preserves v2 record fields on every write; delete is an obligation, not an erase; honours the executor record; lease holder subscribes the v2 subject |
| Lease holder and GC sweep holder | Daemons, covered by the daemon slot | as daemon |
| Every configured `awsgw` slot | Enforcer and relay; an N-1 gateway answers v2 `InvalidAction`, letting a v2 guest fall back (V1) to an unbound v1 fetch (V5 lost) | v2 routes; v1 fetch bound to executor; v1 listing projected from records |
| Control-plane VM agent | Long-lived consumer; capability per executor epoch (V8) | Not a slot; `addonContract: 2` tagged with the activation generation |
| Not participants | predastore, viperblock, vpcd, nats, northstar, ui, non-EKS services; account teardown and OIDC readers write no add-on keys (`accountteardown/reapers_eks.go:102-107`, `handlers/sts/oidc_jwks.go:101`) | nothing |

### 3.2 What the v2 design may depend on

| Need | Choice | Reason |
|---|---|---|
| Daemon report | `release.daemon.<node>` in `spinifex-cluster-state` | TTL ages it out; invisible to N-1 readers (Section 1) |
| Daemon legacy signal | Additive marker in `heartbeat.<node>` | N-1 rewrites the heartbeat without it every 10 s |
| Daemon probe | Additive fields in the per-node health reply | N-1 answers without them |
| awsgw report and probe | `release.awsgw.<node>`; additive fields in `GetVersion` to the node's configured gateway | N-1 answers `GetVersion` without them |
| Activation record | EKS-owned, no-TTL storage keyed `eks-addon-lifecycle/2` | Not cluster-state (TTL), not per-account (scope is the Region), not the stamp |
| Operator surface | `spx admin eks addon-lifecycle {status,prepare,activate,deactivate,finalise}`, with client token and reason; evaluated by a daemon on a subject only N daemons subscribe | Extends the existing group; an N-1 evaluator cannot exist |
| Audit | Operator principal (or `local:<node>`), token, from, to, reason and evidence digest, in the same CAS, plus a log line | No durable audit facility exists |

### 3.3 Activation preflight (EKS)

| Check | Refusal (summary) |
|---|---|
| Every configured daemon and awsgw slot capable | `{role} on node {node} is {class}` |
| One config epoch; no unexpected slot | `node {node} reports epoch {e}` or `is not in the Region configuration` |
| No unresolved legacy observation | `an older {role} was observed on node {node}` |
| Record expand complete (P3) over every `eks-account-*` bucket; enumeration fails closed (`handlers/eks/store.go:149-171`) | `{n} add-on records not yet expanded` or `buckets could not be enumerated` |
| Executor record present per non-deleting cluster (P4); empty executor allowed and gated per cluster | `cluster {c} has no delivery-executor record` |
| Incarnations unchanged since prepare | `{role} on node {node} restarted after prepare; run prepare again` |

Clusters whose executor is not yet proven are reported, not refused: the per-cluster gate (v2 design Section 8.1) handles them.

### 3.4 Rollback before finalisation (P5)

| N-1 behaviour on activated state (compat doc) | Deactivation rule |
|---|---|
| DROPs `incarnationId`, `desired`, `observed` on update, report, failure | Allowed only with no obligation in flight; re-activation re-expands |
| ERASEs on delete and cluster-prefix delete | No record in `DELETING` or `DELETE_FAILED`; each completes on an observed `removed` or `relinquished` report first |
| Does not model `UPDATING`, `UPDATE_FAILED`, `DELETING`, `DELETE_FAILED` | None in those statuses; `UPDATE_FAILED` is resolved by `UpdateAddon` or `DeleteAddon` first |
| OVERWRITEs the manifest from v1 fields | Harmless: manifest and legacy fields are still written until finalisation (R16) |
| Readers IGNORE added fields | Safe |

While `DEACTIVATING`, create, update and delete are refused with a rollback-in-progress message; reports, deadlines and reconcile continue so obligations drain under v2 semantics.
After deactivation, v2 routes answer `NotActivated` and v2 guests fall back to v1 (V1).
On re-activation, no earlier `addonContract: 2` marking counts until a fresh fenced v2 report under the new activation generation, because an N-1 reconciler may have swapped control-plane members without advancing any epoch.

### 3.5 Finalisation (R16)

| Preflight | Why |
|---|---|
| Every daemon and awsgw slot capable; no legacy observation since activation | No N-1 host remains |
| Every record migrated; every awsgw and daemon projects the v1 listing from records | Manifest keys stop being written |
| Every non-deleting cluster's current executor epoch proven v2 under the current activation generation; no executor, or a `FAILED` cluster with running VMs, refuses | A v1 guest served from manifest keys would remove every add-on (`handlers/eks/addons.go:132-134`) |
| Explicit `--confirm eks-addon-lifecycle/2` | Q-75, ADR-0006:S5 step 4 |

Finalisation permits stopping, then removing, the manifest key and legacy top-level fields; it makes deactivation and any N-1 rejoin impossible; it does not retire the v1 routes or v1 guest mode, which stay an ADR-0006:S6 retirement.

### 3.6 Coverage

| Prerequisite | Covered by | Not covered |
|---|---|---|
| P1 | 3.1, 3.2 | Post-activation awsgw legacy signal (gap 3) |
| P2 | 3.2, 3.3 | none |
| P5 | 3.4 | Repair after an N-1 writer runs post-activation (gap 2) |
| P3, P4 | Consumed as evidence only | Expand pass; executor record and V10 fencing |

### 3.7 Evidence required later

| Test | Pins |
|---|---|
| Rolling upgrade in both orders (daemons first, gateways first); activation refused until the last slot is capable; v1 unchanged | ADR-0006:S2, S8 item 1 |
| Activation refused with an N-1 daemon, an N-1 awsgw, a stopped node, a never-reported slot, an unexpected node, an unexpanded record, an unenumerable bucket | Unknown is never compatible |
| Activation refused after a restart between prepare and activate | No settle window |
| Rollback: freeze; pending delete completes on `removed`; deactivation refused while an obligation or unmodelled status exists; N-1 then serves every record with today's semantics | ADR-0006:S5, S8 items 5 and 6 |
| Re-activation re-closes every per-cluster gate until a fresh v2 report | ADR-0006:S3 invalidation |
| Finalisation refused while one v1 guest, one `FAILED` cluster with VMs, or one legacy observation remains, and without confirmation | R16, Q-75 |
| Crash before and after each CAS; same-token retry returns the recorded outcome | Crash safety |
| Two concurrent operators: one transition, one audit entry | CAS |
| N-1 daemon started after activation is observed and freezes mutations; resolves only on a capable new incarnation | Detection |
| Unreadable activation state: no semantics chosen | Fail closed |
| N-1 binaries unaffected by the added keys and fields | ADR-0006:S2 |

## 4. (c) Out of scope

Global release orchestration, release managers and rollout drivers (ADR-0006:S5; Q-183 and Q-184 open); any "all Spinifex components upgraded" gate; Predastore, Viperblock, Bluebottle and non-EKS services; shared packages (ADR-0003:S2); NATS or JetStream redesign and stamp bumps; node enrolment, credential revocation and installer downgrade refusal (Q-71, Q-184).

## 5. Other likely consumers (not enrolled)

- EBS provider protocol: exact-match wire version today; would need provider slots and a newer-major activation.
- RDS and ECS guest agents: versions recorded, never compared; would need per-instance capability under long-lived-consumer rules.
- Predastore: independently released; would need a declared version matrix evaluated by capability, not N/N-1.
- Viperblock formats: exact-match headers; would need expand and finalise for retained artefacts.
- Bluebottle: a linked library, so the linking binary is the participant.
- Spinifex KV crossings such as the instance-record migration: first-opener activation could become an operator action.

Future service work can reuse or challenge the generic concepts in Section 2; this design does not decide that.

## 6. Gaps and conflicts (flagged, not resolved)

1. V9 limits activation to EKS; Section 2 states generic concepts, which V9 did not anticipate, though no framework or package is built.
2. No fence stops an N-1 daemon started after activation; it can DROP or ERASE before detection, and the heartbeat starts after subscriptions (`daemon/daemon.go:2257,2309`). ADR-0006:S5's rule on automatic downgrade is met only on the controlled path.
3. awsgw has no post-activation legacy signal; an N-1 gateway re-opens the V1 fallback and an unbound v1 fetch until its report goes stale.
4. The daemon has no runtime build version (`operator/cli/service.go:613`).
5. Finalisation requiring every cluster to prove v2 keeps the rollback window open as long as the slowest image upgrade; lifecycle design Section 9 frames finalisation around host consumers and v1 retirement separately.
6. Section 3.3 and a fail-closed unreadable state may need a new v2 response reason, while v2 design Section 6.3 lists response reasons as fixed.
7. Slots key on the configured node name, while Q-71 requires an enrolled identity (authority open under O-1).
8. A node removed from configuration is not revoked (`operator/cli/get.go:80` uses a config-held token), so its record ageing out is paired with configuration evidence only.
9. Not every reader of `spinifex-cluster-state` was confirmed (`operator/cli/admin_jsprobe.go:82` opens it).
10. The restore path's successor-first ordering still blocks automatic epoch advance after re-activation (v2 design Section 4.4).

## 7. Open questions for the reviewer

1. Finalisation: require every cluster to prove v2 (as written), or allow it once the v1 listing is projected from records and leave v1 guests wholly to ADR-0006:S6 retirement?
2. Should a post-activation legacy observation freeze add-on mutations Region-wide (as written) or only alarm?
3. For an unreadable activation record, add a new v2 response reason or reuse `NotActivated` with HTTP 503?
