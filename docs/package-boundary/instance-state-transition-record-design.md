# EC2 instance state: transition record and activation gate (slice 1a)

Code read at `830f70489`; not verified live.

**Status:** Draft for review; design only, nothing implemented.

This is the design part of slice 1 ("Transition-record design and expand stage") of `mulga/docs/development/feature/instance-state-authority-migration.md`.
It changes no production code, schema, writer or test.
Paths are relative to `spinifex/` unless they start with `docs/` (this repository) or `mulga/` (the umbrella repository).

## Standing

ADR-0007 (`mulga/docs/adr/0007-instance-state-authority-and-partition-recovery.md`) is accepted and is applied by clause: S1 (owner-controlled desired intent, assignment generation and fence), S2 (local file is an execution journal), S3 (intent before realization, evidence after), S4 and S5 (partition and reconnection), S6 (narrow capabilities), S7 (ADR-0006 evolution; no compatibility from a bucket version alone; no best-effort poll as a fence) and S9 (reuse `kvutil`, `kvstore`, `kvlease`, `migrate`, `reconciler`, `resource.Object`).
ADR-0006 (`mulga/docs/adr/0006-contract-guest-and-persisted-state-evolution.md`) is accepted and is applied by clause: S2 (N/N-1 in both rollout orders), S3 (no heuristic as capability proof), S4 (an older writer that discards fields is not compatible; expand, migrate, contract), S5 (prepare, activate, migrate, finalise; controlled rollback) and S8 (evidence).
No new ADR is needed: these two decisions authorise the design, and the clauses below are implementation design under them.
The generic activation concepts are taken from `docs/package-boundary/eks-release-membership-and-activation-design.md` Section 2 and review decisions M1 to M4; this document does not widen the EKS scope or build a shared package.

Release names used below:

| Name | Meaning |
|---|---|
| R0 | Every binary deployed today: decodes into Go structs and re-encodes, drops unknown fields, has no per-record version and no capability report. |
| R1 | The expand release delivered by slice 1b. |
| R2+ | Any later release that changes the record format again. |

## 1. Evidence this design depends on

All of it is from the slice-0 inventory in `docs/PACKAGE_BOUNDARY_MIGRATION.md` ("ADR-0007 instance-state inventory (slice 0)") or the code at `830f70489`.

| # | Fact | Evidence |
|---|---|---|
| E1 | Every KV writer decodes into `vm.InstanceRecord` and re-encodes; unknown fields are lost at every nesting level. | Generation matrix, every row; `TestCurrentBehaviour_TypedRecordRoundTripDropsUnknownFields` |
| E2 | `WriteRunningSet`, `WriteStoppedInstance`, `UpdateStoppedInstance`, `WriteTerminatedInstance`, `UpdateTerminatedInstance` and the `LoadState` path also lose `Generation` and `ObservedGeneration`. | Matrix rows marked "Lost"; `daemon/instance_running_set.go:74`, `daemon/jetstream.go:534`, `daemon/jetstream.go:778` (`mutateInstance`), `daemon/instance_records.go:164` (`writeRecord`) |
| E3 | `VMFromRecord` then `Record()` also loses `UID`, `Region`, `OwnerRefs`, `Finalizers` and `Metadata.Tags`. | `TestCurrentBehaviour_RecordThroughVMLosesGenerationAndBookkeeping` |
| E4 | Records carry no per-record version; the only version is the bucket stamp `_version` (live 5, terminated 3). | Persisted-state table; `daemon/jetstream.go:53`; `foundation/state/kvutil/state.go:4` |
| E5 | The stamp is checked only when a handle is opened; `SchemaAheadError` refuses a bucket stamped ahead, and a handle already open keeps writing. | `TestCurrentBehaviour_BucketVersionGate`; `foundation/state/migrate/kv.go:57-89`; `foundation/state/kvstore/bucket.go:89-107` |
| E6 | `kvutil.WriteVersion` only raises a stamp; nothing lowers one. | `foundation/state/kvutil/kvutil.go:211-228` |
| E7 | Migrations run on open in whichever process opens through the daemon's config, including `vpcd`. | INV-02, INV-13; `vpcd/imds_instance_state.go:88-95` |
| E8 | The gateway (INV-16, INV-17, INV-18) and network reconcile (INV-15) open the bucket with no hook and decode records; neither writes instance records. | `runtime/roles/awsgw/awsgw.go:573-633`, `domains/network/reconcile/intent.go:444-474`; no `Put`, `Update`, `Create`, `Replace`, `Set`, `Mutate` or `Delete` call on the instance store in `runtime/compute/cache/cache.go`, `domains/admission/quota/records.go` or `intent.go` |
| E9 | Assignment today is `Status.LastNode`, moved by CAS on the entry revision (`ClaimRecoverableInstance`, `ClaimStoppedInstance`, `ReleaseRecoveredInstance`). | `daemon/jetstream.go:596-705`; INV-10 |
| E10 | The local file is `{"schema_version":1,"vms":{...}}`; any other version fails daemon boot, and `vpcd` reads the same file through `daemon.ReadLocalState`. | `TestCurrentBehaviour_LocalStateSchemaVersionFailures`, `TestDaemonLoadState_CorruptFile`; INV-13 |
| E11 | Unknown fields in the local file at the same version are accepted and dropped on rewrite. | `TestCurrentBehaviour_LocalStateUnknownFieldsAreDroppedOnRewrite` |
| E12 | A restore with KV unavailable leaves local instances unlaunched with their in-memory status untouched. | `TestCurrentBehaviour_RestoreWithKVUnavailableLeavesLocalUnlaunched`; owner disposition in the slice-0 record |
| E13 | The daemon has no runtime build version; heartbeats in `spinifex-cluster-state` carry node, epoch and services but no capability. | `docs/package-boundary/release-and-version-inventory.md` G1; `daemon/heartbeat.go` |
| E14 | `kvlease` requires a TTL bucket whose TTL equals the lease TTL; the instance bucket has no TTL. | `foundation/state/kvlease/lease.go:28-75`; `TestCurrentBehaviour_InstanceBucketConfiguration` |

## 2. Transition record (T-series)

**T1. Key and owner.**
The record stays at `i.<instance-id>` in `spinifex-instance-state` and, once terminal, in `spinifex-terminated-instances`.
Its codec, keys and format marker belong to the EC2 instance owner (ADR-0007 S1); slice 2 moves them behind `domains/ec2/instance`, and this design does not require that move first.
Rationale: the key migration already landed (live 3→4); re-keying again would add a second crossing to every consumer INV-03 to INV-18.

**T2. Format marker.**
Each record gains a top-level integer `format`.
An absent marker means format 1, the shape R0 writes today.
The transitional shape is format 2.
A reader treats the marker as follows:

| Marker | R1 reader | R1 writer |
|---|---|---|
| Absent or 1 | Decode the legacy fields. | May rewrite as format 1; may raise to 2 only under G8. |
| 2 | Decode legacy and new sections. | Writes format 2, preserving every section. |
| Greater than 2 | Decode the legacy fields for reading only and report `unknown_format`. | Refuses the write with an explicit error and leaves the record untouched. |
| Not an integer | Refuses to decode the record; it is listed as unreadable, as a corrupt record is today. | Refuses the write. |

Rationale: E4 shows nothing can tell an old record from a new one, so neither lazy migration (ADR-0006 S7) nor a write-time refusal is possible without a per-record field.
The marker states the format a record is in; it is not a fence and grants nothing (ADR-0006 S3).

**T3. Desired section (reused).**
Desired lifecycle intent stays where it is: `spec` (including `spec.desired_state`) and the AWS-visible identity in `metadata` (`name`, `uid`, `account_id`, `region`, `owner_refs`, `finalizers`, `tags`, `deletion_timestamp`).
`metadata.generation` becomes the desired generation and advances by one on each accepted desired mutation, through `resource.Object.MutateSpec`.
Rationale: the envelope already has these fields (S9); the defect is that E2 and E3 writers erase them, not that they are missing.

**T4. Assignment section (new).**
A new top-level `assignment` holds exactly one execution assignment:

| Field | Meaning |
|---|---|
| `node` | Node identity the assignment grants realization to. Empty means unassigned (stopped, or awaiting placement). |
| `generation` | Integer advanced by one on every assignment change: launch placement, start on another node, recovery claim, release, reassignment, unassignment. |
| `epoch` | The activation generation (G5) under which the assignment was made. |
| `token` | Random value minted with each assignment change. |
| `for_desired_generation` | The desired generation the assignment was made to realize. |
| `reason` | `launch`, `start`, `recovery`, `release`, `operator` or `migrated`. |
| `storage_fence` | Optional reference to fencing evidence for the previous assignee's storage, required for a cross-node `recovery` assignment (T6). |

The fence a node holds and presents is the tuple (`instance`, `epoch`, `generation`, `token`).
Comparison is lexicographic on (`epoch`, `generation`); `token` must match exactly.
`status.last_node` and `status.az` remain as the legacy mirror of `assignment.node` and its zone while R0 readers exist (T9).

**T5. Observation section (new).**
A new top-level `observation` holds the assigned node's report:

| Field | Meaning |
|---|---|
| `node` | Reporting node. |
| `assignment_epoch`, `assignment_generation`, `assignment_token` | The assignment the observation was made under. |
| `desired_generation` | The desired generation the node had acted on; mirrored into `metadata.observed_generation`. |
| `state` | Canonical observed state, including `unconfirmed` and `degraded` (Section 7). |
| `evidence` | Kind (`qmp`, `process`, `journal`, `teardown`), node daemon incarnation and the time the evidence was taken. |
| `reported_at` | Time of publication. |

The legacy `status` object remains the AWS-visible projection and R0-readable mirror until contract (T9).

**T6. Fence kinds.**

| Candidate | Viable as | Not viable as |
|---|---|---|
| KV entry revision (`kvstore.CompareAndSet`, `Mutate`) | The write precondition for every record change; it orders concurrent writers. | An assignment identity: it changes on every write, so a node cannot hold it across a partition or a restart. |
| Assignment `generation` plus `token` in the record and the journal | The fence a node holds and presents; checked by the observation reporter inside the CAS and by the node on reconnection. | Proof that a partitioned former assignee has stopped; it cannot see the record. |
| `kvlease` | Nothing in this slice. | The assignment fence: E14 requires a separate TTL bucket, and a lease lost by expiry during a partition conflicts with S4, which lets the node keep running an already assigned guest. |
| Storage fence held by the EBS provider (`ClaimRecoverableInstance` comment, `daemon/jetstream.go:636-638`) | The only mechanism that stops a partitioned former assignee damaging data; referenced by `assignment.storage_fence` for a cross-node recovery assignment. | Something this design implements; volume fencing is an ADR-0007 non-goal. |

Forbidden as a fence, by ADR-0007 S4 and S7, ADR-0006 S3 and plan delivery rule 5: the local file or any local checkpoint, the bucket `_version` stamp, the record `format` marker, elapsed time or a polling interval, a best-effort poll, and heartbeat absence alone.
Heartbeat staleness may still trigger recovery as it does today; it is the trigger, not the fence.

**T7. Field map.**

| Group | Current field | Slice-0 loss | Transition |
|---|---|---|---|
| Identity | `metadata.name`, `account_id` | Survive | Reused |
| Identity | `metadata.uid`, `region`, `owner_refs`, `finalizers`, `tags` | Lost through `VMFromRecord` then `Record()` (E3) | Reused; every writer must preserve |
| Desired | `spec.*`, `spec.desired_state`, `metadata.deletion_timestamp` | Survive the VM round trip | Reused |
| Desired | `metadata.generation` | Lost by E2 writers; never written from `vm.VM` | Reused as desired generation |
| Assignment | `status.last_node`, `status.az` | Overwritten by `WriteRunningSet` with the writer's node | Mirror of `assignment.node` |
| Assignment | none | n/a | New: `assignment.*` |
| Observation | `status.status`, `status.health`, `status.instance`, network and device fields | Survive | Legacy mirror and AWS projection |
| Observation | `metadata.observed_generation` | Lost by E2 writers | Mirror of `observation.desired_generation` |
| Observation | none | n/a | New: `observation.*` |
| Format | none | n/a | New: `format` |
| Exception | `spec.config.net_devs`, `spec.config.devices` | Node appends at launch (`runtime/compute/vm/record.go` comment) | Recorded divergence: a node-written spec field; slice 3 moves the appended part to observation |

**T8. Terminated bucket.**
Records in `spinifex-terminated-instances` follow T2 to T7 unchanged; the owner moves a record there with its sections and members intact.
Rationale: R0 writes there too (`WriteTerminatedInstance`, `UpdateTerminatedInstance`; INV-04, INV-06, INV-12), and the matrix shows the same losses.

**T9. Legacy mirror.**
While any R0-shaped reader remains, format-2 writers keep `status.last_node`, `status.az`, `status.status` and `metadata.observed_generation` consistent with `assignment` and `observation` in the same CAS.
R1 readers take assignment and observation from the new sections and use the mirror only for format-1 records.
The mirror is removed only at finalisation, after every reader slot reports the contract-stage token (G5).

## 3. Writer authority (W-series)

**W1. Authority table.**

| Actor | Process | May write | Must not write |
|---|---|---|---|
| EC2 API and lifecycle controller (INV-07, INV-08, launch and stop paths) | daemon, any node | `metadata` identity, `spec`, `metadata.generation`, `deletion_timestamp`, `finalizers`; `assignment` for launch, start and operator reassignment | `observation`; `status` other than the AWS-visible transition it is accepting |
| Assigned node realization (INV-03 running set, INV-06) | daemon, assignee only | `observation`, the legacy `status` mirror, `metadata.observed_generation`, under a matching fence | `spec`, `metadata.generation`, `assignment` |
| Recovery (INV-10) | daemon acting as controller | `assignment` via conditional claim, release and abandon, with `reason` `recovery` or `release` | `observation` for a node other than itself |
| Recovered-evidence reporter (ADR-0007 S5) | daemon, any node | A separate evidence key, never `i.<id>` (W4) | The canonical record |
| Owner migrations (`daemon/instance_records_migrate.go`) | daemon only | Records and the stamp, through `migrate.Registry` | n/a |
| `vpcd` (INV-13, INV-14, INV-15) | vpcd | Nothing | Records, the stamp, migrations |
| Gateway instance cache and quota (INV-16, INV-17, INV-18) | awsgw | Nothing (E8) | Records, the stamp |
| Operator tooling (INV-19 touches cluster state only) | `spx` | The activation record through the owner (G6) | Records directly |

**W2. Partial writes replace whole-record writes.**
Each writer mutates only its field group inside a CAS on the revision it read, and copies every other section and every unknown top-level member from that read.
`WriteRunningSet` becomes a per-instance observation update that checks the fence, rather than `Replace` of a record built from `vm.VM` and stamped with the writer's node.
`WriteStoppedInstance` and `WriteTerminatedInstance` become mutations of the existing record (create only when absent), so identity and generations survive.
`mutateInstance` (used by `UpdateStoppedInstance` and `UpdateTerminatedInstance`) stops doing `*record = *instance.Record()` and instead applies the caller's change to the record's own fields, or confines the `vm.VM` conversion to the fields the caller's group owns.
`VMFromRecord` stays a read-side projection; no write path may round-trip a record through `vm.VM`.
This is described for slice 1b and slice 3; nothing changes here.

**W3. Read-only consumers.**
`vpcd`, the gateway and network reconcile are read-only today and stay so.
They become narrow-capability consumers in slice 6 (S6); until then the expand release changes only how they open the bucket (G9).

**W4. Recovered evidence is not a record write.**
A node that holds a journal entry for an instance whose canonical record is absent, foreign or terminal writes recovered-execution evidence under its own key (for example `recovered.<instance-id>.<node>`), never `i.<id>`.
The owner decides adoption, orphan handling or termination (ADR-0007 S5).
Slice 1b must confirm that no R0 reader enumerates the live bucket without a prefix filter before choosing the key.

## 4. Unknown-field preservation (P-series)

**P1. R1 preserves unknown top-level members on every decode–re-encode path.**
The record codec keeps a retained set of top-level members it did not decode (`map[string]json.RawMessage`) and writes them back unchanged, and writers follow W2.
Raw JSON merge at every level and `json.RawMessage` for the whole record were considered; both make every typed mutation a merge problem across about 40 status fields and the embedded AWS SDK structs.

**P2. Future format changes add top-level members.**
Because P1 preserves only top-level members, a later format adds sections at the top level and never adds a field inside `metadata`, `spec`, `status`, `assignment` or `observation` that an older writer must keep.
A change that cannot follow P2 raises `format` and relies on T2's refusal.

**P3. Preservation alone is insufficient.**
R0 binaries are deployed and cannot be patched: they drop every unknown member (E1), erase generations (E2, E3), keep writing through open handles (E5) and are ordinary writers on every node (INV-03, INV-05 to INV-08, INV-10).
An R0 daemon that rewrites a format-2 record leaves a format-1 record with the legacy mirror and no assignment fence, which T2 would read as a valid format-1 record.
Format 2 is therefore activated only once no R0 writer can run (Section 5).

## 5. Activation gate (G-series)

**G1. Contract scope.**
The scope is every process that writes, migrates or decodes instance records or the local file (ADR-0006 S5: only roles that read, write or enforce the contract).

| Slot role | Why in scope | Required capability token |
|---|---|---|
| `daemon`, one per configured node | Writes records (W1), runs migrations, writes the local file | `ec2-instance-record/2-preserve` and `ec2-instance-journal/2` |
| `vpcd`, one per configured node running it | Reads the local file (a v2 journal fails an R0 `vpcd`, E10), runs migrations today (E7) | `ec2-instance-journal/2-read` and `ec2-instance-record/2-read` |
| `awsgw`, one per configured node running it | Decodes records into the instance cache and quota counts | `ec2-instance-record/2-read` |
| Operator CLI | Does not touch instance records today (INV-19) | Not a slot; becomes one only if a later command writes records |
| Migration runner | The daemon slot after G9 removes migration from `vpcd` | Covered by `daemon` |

A slot is (Region, node, role), derived from configured membership (`bootstrap/config` node map and its per-node services), never from who reports.

**G2. Capability report.**
Each R1 process writes a self-report for its slot on start and on an interval: role, node, random incarnation, capability tokens compiled into the binary, configuration epoch and a diagnostic build version.
The report lives in `spinifex-cluster-state` beside the heartbeat, under a key R0 never reads (`release.<role>.<node>`), as the EKS design chose (E13).
The R1 daemon heartbeat gains an additive capability marker; an R0 daemon rewrites its heartbeat every 10 s without it, which is positive legacy evidence.
Each slot also answers a per-slot probe at evaluation time: the daemon's existing per-node health reply gains incarnation and tokens; `vpcd` and `awsgw` need a probe route (awsgw's `GetVersion` may carry it).

**G3. Proof of compatibility.**
A slot counts as compatible only when all of these hold at evaluation:

1. Its report is present, names every required token, and is newer than the staleness threshold.
2. Its probe answers with the same incarnation and tokens.
3. No legacy heartbeat or probe answer for that node has been observed since the report.
4. Its configuration epoch equals the evaluator's.

A slot that is absent, stale, unreachable, partitioned, crashed, reporting an unexpected node, or answering without tokens is not compatible, and activation is refused.
Time only makes the answer more conservative: staleness moves a slot from compatible to stale, never the reverse.
A dead or removed node blocks until it reports again or leaves configured membership.
`kvlease` is not used: a report is per slot, not mutual exclusion, and a lease's expiry would add a time-based proof ADR-0006 S3 forbids.

**G4. Activation record.**
One record for this contract, stored owner-private in `spinifex-instance-state` under a key outside `i.` (for example `activation.instance-record`), no TTL, history 1.
It holds the state, activation generation, prepared incarnation snapshot, legacy observations since activation, and an audit list.
Every transition is one `kvstore.CompareAndSet` on the record revision with an operator client token; the loser receives the current state and changes nothing, and a repeat with the same token returns the recorded outcome.
Each audit entry holds principal, token, from, to, reason and an evidence digest, written in the same CAS, plus a log line.

**G5. State machine.**

| From | To | Condition |
|---|---|---|
| absent (Inactive) or `PREPARED` | `PREPARED` | G3 holds for every slot; every record decodes; snapshot of incarnations stored |
| `PREPARED` | `ACTIVE` | G3 still holds and every incarnation equals the snapshot; both bucket stamps raised (G7); activation generation advanced |
| `ACTIVE` | `DEACTIVATING` | Operator authorisation only; freezes format-raising writes and new assignments |
| `DEACTIVATING` | `PREPARED` | Every record and journal back in format 1 (G12, J4); stamps lowered |
| `DEACTIVATING` | `ACTIVE` | G3 again (abort rollback) |
| `ACTIVE` | `FINALISED` | Every record format 2; every slot reports the contract-stage tokens; no legacy observation since activation; explicit confirmation |

No transition is automatic; only the owner, driven by an `spx admin` operator action evaluated by an R1 daemon on a subject R0 does not subscribe, moves the state.
A binary start never activates anything, so first-opener activation (E7) is not used.

**G6. Who flips.**
The operator command reaches an R1 daemon, which evaluates G3 and performs the CAS.
On a single-node deployment the same command applies with three slots (daemon, vpcd, awsgw); the upgrade procedure may invoke it, but it is still an explicit, audited step.

**G7. Bucket stamp as the guard against R0 restarts.**
At `ACTIVE`, the owner raises the live bucket from 5 to 6 and the terminated bucket from 3 to 4.
An R0 daemon or R0 `vpcd` that starts afterwards then fails its open with `SchemaAheadError` (E5) and never writes.
Limits, stated as measured:
- An already open R0 handle keeps writing (E5); G3's incarnation snapshot is what excludes one at activation.
- The gateway and network reconcile open with no hook (E8) and ignore the stamp until G9 lands.
- `WriteVersion` cannot lower a stamp (E6); deactivation needs an owner-run lowering write, which `kvutil` does not provide today.
- R1 must accept 6 without migrating, so its open hook runs registered migrations up to 5 and treats 6 as understood; `migrate.Registry.RunKV` has a single target today and needs an "understood ceiling" or an owner-side pre-check.
This departs from the EKS design's "no stamp bump" invariant deliberately: there the all-or-nothing refusal was the reason to avoid it, while here, after every slot is proven R1, refusing every R0 opener is the wanted outcome.

**G8. R1 write-time rules.**
These apply in every state, so they also protect against an R2 activation later.
1. Inside each CAS, the writer reads the record's `format` and refuses a format it cannot preserve (T2); the check and the write share one revision, so no read-then-write gap exists.
2. A writer never changes a record's format except under a readable `ACTIVE` state (raise) or `DEACTIVATING` state (lower).
3. The writer watches the activation record with `kvstore.Bucket.Watch` and re-reads it on a periodic backstop (the `reconciler` pattern); the cached state decides only whether to emit format 2 and the journal v2, never whether to preserve.
4. Unknown top-level members are always preserved (P1).

**G9. INV-15 and INV-16 gaps, and `vpcd` migrations.**
Before activation, R1 must:
- open the bucket in `vpcd` (INV-13) attach-only with a check-only hook that refuses a stamp above its understood ceiling and runs no migration, so migrations have one runner;
- give the gateway (INV-16) and network reconcile (INV-15) the same check-only hook, so a later R2 stamp makes them fail visibly rather than misread;
- keep their decodes tolerant: `json.Unmarshal` into `vm.InstanceRecord` already ignores the new sections (`foundation/state/kvstore/store.go:338`).
These consumers write nothing (E8), so the gap they open is misreading, not corruption.
The legacy mirror (T9) keeps their reads correct during `ACTIVE`; they gate activation as slots (G1) so that the journal v2 and `unconfirmed` semantics are understood where they are read.

**G10. Unreadable activation state.**
When the activation record cannot be read or decoded, an R1 writer emits no format 2 and no journal v2, writes an existing record only in the format it found (G8.2), and refuses creates and assignment changes that would require choosing a format with a retryable unavailable result.
For the EC2 API that result is a retryable 503 through `awserrors`; slice 1b names the exact code against AWS behaviour.
An absent record means Inactive; only that case selects format-1 semantics.
This mirrors EKS M3 (`ActivationStateUnavailable`): unreadable selects neither old nor new semantics.

**G11. Legacy writer after activation.**
An R0 daemon heartbeat or probe answer observed while `ACTIVE` is recorded in the activation record and freezes instance desired mutations and assignment changes Region-wide; reads and fenced observations continue (EKS M2 analogue).
The freeze clears only on a compatible new incarnation for that slot or an operator rollback.
Damage already done by such a writer appears as format-1 records with no assignment; the owner reports them and does not re-derive a fence from `status.last_node` without an operator action.

**G12. Rollback.**
- Before activation: R1 has written only format 1 and journal v1, so R0 reads and writes everything; P1's retained members are empty.
- After activation: freeze, drain or cancel outstanding format-2 obligations (in-flight recovery assignments, unpublished observations), down-convert every record to format 1 (drop `format`, `assignment`, `observation`; the legacy mirror is already current), down-convert each node's journal to v1 (Section 6), lower the stamps, then move to `PREPARED`; only then may an R0 binary start.
- Re-activation advances the activation generation, so every assignment minted afterwards outranks any lost before (T4); no fence from the previous epoch is honoured without re-issue.
- After `FINALISED`, rollback to R0 is unsupported.

**G13. Mixed-version rolling upgrade.**
Both rollout orders are safe while Inactive: R1 and R0 both write format 1 and journal v1, R1 preserves what R0 drops, and R0 refuses nothing new.
Activation is impossible until the last slot is R1, which is the gate's purpose and not a requirement that every node stop at once (ADR-0006 S2).

**G14. Reusable versus instance-specific.**

| Mechanism | Class |
|---|---|
| Slot derivation from configured membership; incarnation; capability token compiled into the binary; self-report and probe; legacy signal from the heartbeat | Reusable concept, shared with the EKS design; a common report record per slot is Question 1 |
| Activation record shape, CAS transitions with client token and in-record audit; unreadable means fail closed; activation generation | Reusable concept; each contract keeps its own record |
| `migrate.Registry` understood ceiling; owner-run stamp lowering | Reusable extension of `foundation/state/migrate` and `kvutil` |
| Per-record `format` marker and top-level member preservation | Reusable pattern; codec is instance-specific |
| Scope (daemon, vpcd, awsgw), tokens, stamp values, assignment fence, legacy mirror, journal v2, freeze scope | Instance-specific |
| Rollout driver, global "all upgraded" gate, release manager | Out of scope |

## 6. Local execution journal (J-series)

**J1. Content.**
Journal v2 keeps the v1 body (`vms`) and adds, per instance: the assignment fence tuple (T4), the desired generation being realized, the realization handle needed to reattach or stop (process identity, QMP socket, tap and volumes), the last confirmed observation, the queue of unpublished observations, and local safety actions taken during a partition (ADR-0007 S2, S4).
Keeping `vms` makes down-conversion to v1 a removal of the added members.

**J2. Version.**
Journal v2 is `schema_version: 2`.
An R0 daemon then refuses to boot and an R0 `vpcd` fails its local lookup (E10); that refusal is the intended behaviour for an unplanned downgrade, because keeping version 1 with extra members would be silently dropped on the next R0 rewrite (E11).

**J3. Tolerant reader.**
R1 daemon and R1 `vpcd` read v1 and v2.
A node writes v2 only after it has observed `ACTIVE` and writes v1 otherwise, so the file version follows the activation state and never precedes it.

**J4. Down-conversion.**
During `DEACTIVATING`, each R1 daemon rewrites its journal as v1 after its unpublished observations are published or explicitly abandoned, and reports completion; deactivation waits for every node (G5).

## 7. KV-unavailable disposition (K-series)

**K1.** Observation `state` includes `unconfirmed` (journal says assigned and was running; no live process confirmed yet) and `degraded` (process confirmed, publication or a dependency failing).
No R1 path sets `running` without `evidence` of a confirmed live process.

**K2.** With KV unavailable at boot, the daemon launches, recreates and migrates nothing (current behaviour, E12, kept), and may reattach only to a live process that matches a journal v2 entry carrying a fence tuple.
A journal v1 entry has no fence, so reattachment needs format 2 to be active; until then E12 stands unchanged.

**K3.** On reconnection, the node publishes an observation only if the canonical fence tuple equals the journal's; otherwise it quarantines or tears down per ADR-0007 S5 and writes recovered evidence (W4).

**K4.** AWS keeps a host-impaired instance in state `running` and reports it through status checks; the AWS-visible projection of `unconfirmed` is therefore a status-check result, not a new instance state (Question 4 covers the legacy mirror).

## 8. Slice 1b sequence

| Step | Delivers | Acceptance tests | ADR-0007 evidence / plan item | Slice-0 tests it flips |
|---|---|---|---|---|
| 1 | Record codec: `format` marker, `assignment` and `observation` sections, top-level member preservation; no writer emits format 2 | Unknown top-level member survives every writer path; format 3 refused without change; absent reads as 1 | 8 / 7 | `TestCurrentBehaviour_TypedRecordRoundTripDropsUnknownFields` (top level only; nested stays lost by P2) |
| 2 | W2 for existing writers: `WriteRunningSet`, stopped and terminated writers and `mutateInstance` preserve identity, generations and members | Every matrix row reads Preserved; a node write does not move `metadata.generation` | 8 / 2, 7 | `TestCurrentBehaviour_GenerationAcrossKVWriters` "Lost" rows; `TestCurrentBehaviour_RecordKeyAndWireForm` if the wire bytes gain members |
| 3 | G8 write-time format check inside CAS | Concurrent raise between read and write loses the CAS; format above ceiling refused | 8 / 7 | none |
| 4 | Open hooks: understood ceiling, owner-only migrations, check-only hooks in `vpcd`, gateway and network reconcile (G7, G9) | Each process refuses a stamp above its ceiling; `vpcd` runs no migration | 8, 10 / 7 | `TestCurrentBehaviour_BucketVersionGate` (reader without a hook) |
| 5 | Capability report, probe fields and heartbeat marker for daemon, `vpcd`, `awsgw` (G2) | R0 heartbeat recognised as legacy; restart changes incarnation | 8 / 7 | none |
| 6 | Activation record and `spx admin` status, prepare, activate, deactivate (G4 to G6, G10) | Refusal for an R0 slot, stale slot, absent slot, unexpected node, restart after prepare; CAS race; same-token retry; unreadable state fails closed | 8 / 7 | none |
| 7 | Dual write under `ACTIVE`: format 2 with assignment generation, epoch and token minted on every assignment change, legacy mirror kept | R0 reader sees unchanged legacy fields; assignment generation monotonic across claim, release, start | 4, 8 / 2, 7 | none |
| 8 | Journal v2 reader in daemon and `vpcd`, v2 writer under `ACTIVE`, down-conversion (J1 to J4) | v1 and v2 both read; v2 only after `ACTIVE`; deactivation returns v1 | 2, 8 / 7 | `TestCurrentBehaviour_LocalStateSchemaVersionFailures` (version 2 becomes valid), `TestCurrentBehaviour_LocalStateFileExactBytes` under `ACTIVE` only |
| 9 | Upgrade and rollback evidence: both rollout orders; activation refused until the last slot; rollback after activation; R0 restart refused after activation | ADR-0006 S8 items 1, 5, 6 | 7, 8 / 7 | none |

Not in slice 1b: fence enforcement in realization and observation (slice 3), `unconfirmed` and reattachment (slice 5, flips `TestCurrentBehaviour_RestoreWithKVUnavailableLeavesLocalUnlaunched`), missing-key and terminated-bucket recovery (slice 5, flips `TestCurrentBehaviour_RestoreMissingCanonicalKeyRepublishesFromLocal` and `TestCurrentBehaviour_RestoreIgnoresTerminatedBucket`), and the `vpcd` projection (slice 6).
ADR-0007 evidence items 1, 3, 5, 6, 9 and plan items 1, 3 to 6 and 8 belong to those slices.

## 9. Open questions

| # | Question | Recommendation | Alternative |
|---|---|---|---|
| 1 | Should the per-slot capability report be one record shared with the EKS add-on design? | Yes: one report per slot listing every capability token, with each contract keeping its own activation record; this avoids two heartbeat-adjacent writers per process without creating a release manager. | Instance-specific report keys, accepting duplication until a second consumer is real (ADR-0003 S2). |
| 2 | Raise the bucket stamps at activation (G7)? | Yes: after every slot is proven R1, a refused R0 open is the wanted result, and it closes the "R0 restarted after activation" gap the EKS design leaves open. | No stamp change; rely on legacy detection and the G11 freeze, accepting that an R0 restart can strip fields before it is detected. |
| 3 | Freeze scope when a legacy writer appears after activation (G11)? | Region-wide for desired mutations and assignment changes, because any daemon may accept an instance mutation. | Freeze only the affected node's instances, which leaves records it can reach unprotected. |
| 4 | What does the legacy `status.status` mirror show for `unconfirmed`? | Keep the last confirmed value for R0 readers and expose `unconfirmed` through the observation and the AWS status-check projection (K4). | Map it to an existing state such as `pending` or `error`, which changes what R0 readers and `DescribeInstances` show. |
