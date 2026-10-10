# EKS managed add-on: lifecycle correction design

**Status:** Draft for requirements review; no behaviour changes until approved.
Reviewer decisions recorded (see "Review decisions"); the body is consistent with them.

This is the Phase 2 design for correcting the EKS managed add-on lifecycle, written against the code at `18794e017`.
The present contract and its gaps are in `docs/package-boundary/eks-addon-lifecycle.md`, and that record stays authoritative for what the code does today.
Paths are relative to `spinifex/` unless they start with `tests/`, `scripts/` or `contracts/`, which are relative to the spinifex repo root.

## Standing

The design is resource-specific.
It refines proposed requirement Q-146 and the recorded conflict C-82 (`docs/specs/requirements/managed-services.md` in the mulga repo); both are open and pending review, and this document closes neither.
It does not create an architecture proposal, a universal lifecycle interface, a generic finalizer package or a shared repository; ADR-0003 S2 (accepted) rules those out without two real consumers.
It applies ADR-0003 S1 to S5 and ADR-0004 (both accepted) to one resource.
It is not a NATS or JetStream redesign: every transport stays as it is.
Edge IDs E1 to E9 refer to Section 7.1 of `docs/package-boundary/adr-0004-inventory-eks.md`.

Q-75 (contract evolution) and Q-76 (command, event and notification semantics) in `docs/specs/requirements/resource-lifecycle.md` are not accepted ADRs.
They are Decided requirements: a named person stated them, and they have normative effect on this design.
They are not open questions here; only the mechanics of implementing them are.
Q-75 requires a safe compatibility path for older consumers, so a weaker substitute for a guarantee (such as a timing heuristic standing in for a fence) does not satisfy it.

## Review decisions

The reviewer decided the following; the sections below apply them.

| # | Decision |
|---|---|
| R1 | Q-75 and Q-76 are Decided requirements with human provenance and normative effect, not accepted ADRs; only their implementation mechanics remain open. |
| R2 | No v1 timing heuristics: neither "version match after a settle window" nor "delete complete after two listings" is used, because neither is a fence or proof of removal and both produce false success. |
| R3 | `CREATE_FAILED` and `UPDATE_FAILED` are terminal for their generation. A late `ready` is recorded diagnostically and does not restore `ACTIVE`. A new `UpdateAddon` starts a new generation, and `DeleteAddon` stays available. `DEGRADED` recovers to `ACTIVE` on a valid report for the current generation. |
| R4 | The external ARN stays name-only for this correction; the internal immutable `incarnationId` (UUID) is added now. The AWS-style per-incarnation ARN is a separate, explicitly migrated future change, because it affects IAM resource policies and must not break them silently. The divergence stays recorded. |
| R5 | No v1 fallback. v1 stays byte-compatible and readable, but lifecycle mutations that need fencing or observed deletion require an authenticated, v2-capable active control-plane executor; otherwise they return `InvalidRequestException` telling the operator to upgrade the control-plane image. This is a deliberate compatibility gate, not a hard cutover. |
| R6 | Delivery belongs to the EKS cluster, not to whichever server was primary. The cluster control plane assigns the active delivery executor, identified by executor identity and epoch; only that executor may fetch and report, a replaced executor is fenced before its successor acts, and the successor resumes from durable desired state without advancing the generation. The add-on side declares a narrow capability that the cluster owner provides. |
| R7 | The v2 guest contract does not emit `InvalidParameterValue`; it uses structured internal reasons. The AWS adapter maps invalid customer input to `InvalidParameterException`, and a bad directive detected by the guest becomes a reported realisation failure. v1 is unchanged. |
| R8 | `namespaceConfig` is rejected at the gateway while the pinned SDK cannot represent or apply it, and is never silently dropped; this is its own early slice. |
| R9 | Non-empty `configurationValues` stay rejected until a bundle publishes a schema and actually renders the value. |
| R10 | The `iam:PassRole` check on `serviceAccountRoleArn` stays; it is already implemented (`gateway/eks/passrole.go`). |
| R11 | `InvalidRequestException` answers a mutation that conflicts with an in-progress lifecycle operation; `ResourceInUseException` is reserved for a conflicting resource on create. |
| R12 | Deadlines are profile-configured and the chosen deadline is persisted when the operation is accepted; no per-bundle timing values yet. |
| R13 | A delete that supersedes an in-flight update marks that update `Cancelled`. |
| R14 | `serviceAccountRoleArn` is rejected for bundles with no IRSA marker. |
| R15 | The GPU add-on is not re-created automatically after a user deliberately deleted it. |
| R16 | Finalisation is an explicit, operator-visible add-on migration action with preflight evidence that legacy consumers are gone, as Q-75 requires. |

## AWS behaviour relied on

Verified against the AWS EKS API reference on 2026-10-08:

- `Addon.status` is one of `CREATING`, `ACTIVE`, `CREATE_FAILED`, `UPDATING`, `DELETING`, `DELETE_FAILED`, `DEGRADED`, `UPDATE_FAILED` (API_Addon).
- `CreateAddon` and `DeleteAddon` examples show `addonArn` as `arn:aws:eks:{region}:{account}:addon/{cluster}/{addon}/{uuid}`, so each incarnation has its own suffix (API_CreateAddon, API_DeleteAddon examples; the type reference does not state the format).
- `UpdateAddon` returns an `Update` with its own `id`; the example returns `status: InProgress` and `type: AddonUpdate` with `params` such as `ServiceAccountRoleArn` and `ResolveConflicts` (API_UpdateAddon).
- `Update.id` is "a UUID that is used to track the update"; `Update.status` is `InProgress`, `Failed`, `Cancelled` or `Successful`; `errors` carries details of a `Failed` update (API_Update).
- `DescribeUpdate` takes `addonName` as a query parameter, "required if the update is an add-on update"; `Successful` means complete and `Failed` carries an error detail (API_DescribeUpdate).
- `DeleteAddon` returns the add-on with `status: DELETING` (example), and `preserve` "preserves the add-on software on your cluster but Amazon EKS stops managing any settings for the add-on. If an IAM account is associated with the add-on, it isn't removed" (API_DeleteAddon).
- `resolveConflicts` is `OVERWRITE`, `NONE` or `PRESERVE`; on create, with no self-managed version installed, "Amazon EKS sets all values to default values, regardless of the option that you specify"; on update, `NONE` leaves a changed value and "the update might fail", `OVERWRITE` resets it, `PRESERVE` keeps it (API_CreateAddon, API_UpdateAddon).
- `configurationValues` "are validated against the schema returned by `DescribeAddonConfiguration`" (API_CreateAddon, API_UpdateAddon).
- `podIdentityAssociations` on update: "If this value is left blank, no change. If an empty array is provided, existing associations owned by the add-on are deleted" (API_UpdateAddon).
- The documented errors for `CreateAddon` and `UpdateAddon` are `ClientException`, `InvalidParameterException`, `InvalidRequestException` ("invalid given the state of the cluster"), `ResourceInUseException` (409), `ResourceNotFoundException` (404) and `ServerException`; `DeleteAddon` lists the same set without `ResourceInUseException`.
  `IdempotentParameterMismatch` is not among them.
- `CreateAddon` accepts `namespaceConfig`, which the pinned `aws-sdk-go` v1.55.8 model does not carry.

Not verified, and marked where used: which error AWS returns for an update or delete issued while another operation is in progress (R11 decides ours); whether `ready` after `CREATE_FAILED` or `UPDATE_FAILED` returns the add-on to `ACTIVE` (R3 decides ours); how long AWS honours a `clientRequestToken`; what a token reused with different parameters returns; whether `DescribeUpdate` finds an update of a deleted predecessor; the `AddonIssue` code set; whether a role passed to an add-on that needs no IAM permissions is rejected (R14 decides ours); and the Terraform provider's waiters.

## 1. Identity and generation

| Item | Design |
|---|---|
| Incarnation | `incarnationId`: a random UUID minted when `CreateAddon` is accepted and never changed. Delete followed by create of the same name yields a new incarnation. |
| Desired generation | `desired.generation`: an unsigned integer, 1 on create, incremented by exactly one in the same CAS that accepts each `UpdateAddon` and each `DeleteAddon`. Tag changes do not advance it, because tags are not delivered. |
| Where stored | In the add-on record `clusters/{cluster}/addons/{addon}` in `eks-account-{account}`, next to the existing fields, so one revision CAS covers intent, generation and status. No new bucket or key family. |
| Lookup key | Still cluster plus add-on name; the incarnation is a fence, not an address. |

The external ARN stays `addon/{cluster}/{addon}`, minted by the owner and independently derived by the gateway authorizer from names (inventory edge E9), and `incarnationId` is internal (R4).
This keeps authorization and existing IAM policies unchanged.
The divergence from AWS remains recorded as contract gap 10: AWS's `CreateAddon` reference examples carry a per-incarnation suffix, and a recreated Spinifex add-on reuses its predecessor's ARN.
Adopting the suffix is a separate change with its own explicit migration, because policies naming today's ARN would stop matching new add-ons and the authorizer would have to obtain the ARN from the owner before its policy check.
Shaping `incarnationId` as a UUID keeps that change possible.

## 2. Directives, reports and the stale-report rule

A directive is what the controller wants the guest to realise for one add-on; a report is what the guest observed.
In the successor contract (Section 9) both carry `incarnationId` and `generation`, and both carry the delivery executor's epoch (Section 10).

Directive listing (v2): the response carries `executorEpoch` once, then one entry per add-on:

| Field | Meaning |
|---|---|
| `addonName`, `incarnationId`, `generation` | Identity and the desired generation this directive represents. |
| `mode` | `install`, `remove` or `relinquish`. |
| `addonVersion`, `serviceAccountRoleArn`, `configurationValues` | The desired spec; for `remove` and `relinquish`, the last installed spec, so a guest without local state can still render the object list it must remove. |

Report (v2): `addon`, `incarnationId`, `generation`, `executorEpoch`, `phase` (`applied`, `ready`, `failed`, `removing`, `removed`, `relinquished`), `reason` (a structured code, required when `phase` is `failed`), `message`, `ts`.
The gateway attaches the executor identity it derived from the authenticated caller; no executor identity is read from the payload.

Exact comparison rule, applied by the owner to record `R` and report `P`, where `X` is the cluster's current active executor (identity and epoch):

1. If `P`'s gateway-attached executor identity is not `X`'s identity, or `P.executorEpoch` is not `X`'s epoch, discard `P` as a stale executor.
2. If `R` is absent, discard `P`.
3. If `P.incarnationId != R.incarnationId`, discard `P` as a stale incarnation.
   This rejects every report from before a delete and recreate.
4. If `P.generation < R.desired.generation`, discard `P` as stale.
   This rejects reports produced for a superseded update and reports produced before a delete was accepted.
5. If `P.generation > R.desired.generation`, discard `P` and log a protocol error; a guest cannot learn a generation the controller has not issued.
6. If `P.phase` does not fit `R.desired.mode` (`ready` while removing, `removed` while installing), discard `P` and log a protocol error.
7. Otherwise apply the transition in Section 6 through a revision CAS on `R`; on a CAS conflict, re-read `R` and start again from step 1.

Within one generation, reports apply in arrival order.
A reordered pair from the same generation describes the same desired state, and the level-triggered agent corrects it on its next tick.
Discards never write the record; they are counted in a metric, labelled by rule, so a stuck or fenced guest is visible.

## 3. Desired intent separated from observed state

Record shape sketch (JSON field names; existing fields unchanged):

```json
{
  "schemaVersion": 2,
  "addonName": "aws-load-balancer-controller",
  "arn": "arn:aws:eks:...:addon/prod/aws-load-balancer-controller",
  "incarnationId": "6f1c...",
  "createdAt": "...", "modifiedAt": "...", "tags": {},
  "origin": "user",
  "createToken": {"token": "...", "paramHash": "..."},
  "desired": {
    "generation": 3, "acceptedAt": "...", "mode": "install",
    "addonVersion": "2.11.0", "serviceAccountRoleArn": "...", "configurationValues": "",
    "resolveConflicts": "OVERWRITE",
    "deletion": null
  },
  "observed": {"generation": 2, "phase": "ready", "reason": "", "message": "", "receivedAt": "...",
               "contract": "v2", "executorEpoch": 4},
  "status": "UPDATING",
  "health": [{"code": "...", "message": "..."}],
  "deadline": {"at": "...", "duration": "...", "profileKey": "..."},
  "updates": [{"id": "...", "type": "AddonUpdate", "status": "InProgress", "generation": 3,
               "params": [{"type": "AddonVersion", "value": "2.11.0"}], "createdAt": "...",
               "token": "...", "paramHash": "...", "errors": []}],
  "addonVersion": "2.11.0", "serviceAccountRoleArn": "...", "configurationValues": ""
}
```

`desired` is written only by API commands; `observed` only by accepted reports (and by diagnostic late reports, Section 6); `status`, `health` and `deadline` are owner-derived from both and stored so reads stay a single get.
`origin` is `user` or `auto-staged` (Section 13).
The trailing top-level `addonVersion`, `serviceAccountRoleArn` and `configurationValues` are the legacy fields, kept in step with `desired` during the transition so an older binary can still read the record.
Updates are kept inline, bounded to the most recent N per incarnation, so accepting an update and advancing the generation is one CAS rather than a two-key write.

The staged manifest sub-key becomes a projection of `desired`.
The internal listing is built from the records, and the manifest key is still written for rollback until finalisation.
That removes today's crash window between the record put and staging (contract row "Restart and interruption"), because there is no longer a second write that can be missed.

Migration of existing records, as an idempotent and resumable expand step:

| Today's status | `incarnationId` | `desired.generation` | `observed` | Status after |
|---|---|---|---|---|
| `ACTIVE`, `DEGRADED` | newly minted | 1 | generation 1, phase from status, contract `v1` | unchanged |
| `CREATING`, `UPDATING` | newly minted | 1 | none | unchanged; a deadline from the profile starts at migration |
| `CREATE_FAILED` | newly minted | 1 | none | unchanged, with the stored `health` as an issue |

Minting at migration is safe because no deployed report carries an incarnation.
Existing non-empty `configurationValues` are preserved, never silently dropped, and gain a health issue stating they are not applied (Section 7).
An older binary that rewrites a migrated record decodes it into today's `Record` and drops the new fields, so the new lifecycle semantics activate only once every daemon in the Region is known to run the new release (Section 9).
Activation does not end rollback; the separate finalisation action does (Section 9, R16).

## 4. Deletion

On a cluster whose active executor has proven v2, `DeleteAddon` creates a durable obligation and keeps ownership until guest-side removal is observed:

1. One CAS sets `status: DELETING`, advances `desired.generation` to `D`, sets `desired.mode` to `remove` (or `relinquish` when `preserve=true`), records `desired.deletion.requestedAt`, persists the delete deadline and, if an update is in flight, marks it `Cancelled` (R13).
   The response is the record in that state.
2. The listing now carries a `remove` or `relinquish` directive for generation `D` instead of omitting the add-on.
3. The record stays until a report for (`incarnationId`, `D`) with phase `removed` or `relinquished` arrives; the owner then deletes the record and the manifest key.
4. While the record exists, `CreateAddon` of the same name returns `ResourceInUseException`, and `DescribeAddon` and `ListAddons` show it as `DELETING` or `DELETE_FAILED`.
   Recreate is therefore serialised behind removal, so a new incarnation never overlaps its predecessor's objects in the guest.

On a cluster without a v2 executor, `DeleteAddon` returns `InvalidRequestException` (Section 9).

What "observed" means for a v2 guest:
today's GC loop deletes the rendered file, runs `kubectl delete --wait=false` on a temporary copy and forgets it.
The v2 agent keeps the removed manifest in a durable per-add-on location instead of `mktemp`, and on every tick runs `kubectl get -f <copy> --ignore-not-found`.
It reports `removing` while any listed object remains and `removed` only when every object in the rendered manifest returns NotFound, including namespaces held by finalizers.
If the apiserver is unreachable, it sends nothing for that add-on rather than `removed`.
If it has no local copy (state lost, never rendered, or a newly assigned executor), it renders the bundle from the directive's last spec into a scratch file and uses that object list.
It keeps reporting `removed` on every tick while the directive is listed, so a lost report is repeated.

For `relinquish`, the v2 agent deletes only the rendered file, leaving the objects in place (K3s does not delete applied objects when the auto-deploy file disappears, per the agent's own comment), and reports `relinquished` once the file is gone.
What K3s does with its `Addon` object in that case is unverified.

Unavailable guest: the record stays `DELETING` and the directive stays listed.
When the persisted delete deadline passes, the owner sets `DELETE_FAILED` with a health issue, keeps the record and keeps listing the directive.
`DELETE_FAILED` is recoverable: a later `removed` report for (`incarnationId`, `D`) completes the deletion, and a repeated `DeleteAddon` re-arms the deadline without advancing the generation, since the intent is unchanged.
A guest that recovers after hours therefore converges the retained directive with no operator action.

Cluster deletion destroys the control-plane VMs and with them every in-cluster object, so add-on removal is implied there.
That path should call an add-on-owned teardown capability that records removal-by-cluster-teardown instead of the blanket `DeleteClusterPrefix` sweep (inventory edge E1); it is not subject to the v2 gate.

## 5. Update as a durable asynchronous operation

`UpdateAddon` is accepted in one CAS that advances `desired.generation` to `G`, sets `status: UPDATING`, appends an update `{id: new UUID, type: AddonUpdate, status: InProgress, generation: G, params}` and persists the update deadline.
The response is that update, not the add-on ARN marked `Successful`.

| Event for generation `G` | Update status | Add-on status |
|---|---|---|
| `ready` report | `Successful` | `ACTIVE` |
| `failed` report | `Failed`, with `reason` and `message` in `errors` | `UPDATE_FAILED` |
| Deadline passes with no `ready` | `Failed` ("timed out") | `UPDATE_FAILED` |
| `DeleteAddon` accepted first | `Cancelled` | `DELETING` |

`DescribeUpdate` with `addonName` routes to the add-on owner and finds the update by `id` among the record's retained updates for the current incarnation; an unknown `id` is `ResourceNotFoundException`.
Whether AWS still resolves an update of a deleted predecessor is unverified.
`ListUpdates` with `addonName` lists the retained ids.
Both are gateway stubs today, so this work also implements them for add-ons only; cluster and node-group updates stay stubs.

`UpdateAddon` is accepted from `ACTIVE`, `DEGRADED`, `CREATE_FAILED` and `UPDATE_FAILED`.
From `CREATING`, `UPDATING`, `DELETING` and `DELETE_FAILED` it conflicts with an in-progress lifecycle operation and returns `InvalidRequestException` (R11).
On a cluster without a v2 executor it returns `InvalidRequestException` (Section 9).
An update whose parameters equal the current desired spec is still accepted as a new generation, so a client gets a trackable update.

## 6. Status set and transitions

The owner adds `UPDATE_FAILED` and `DELETE_FAILED` to its `Status`; `DELETING` becomes a stored status.
Every report row below assumes the report passed the rule in Section 2.

| From | Event | To | Notes |
|---|---|---|---|
| absent | `CreateAddon` accepted | `CREATING` | generation 1; create deadline persisted |
| `CREATING` | `ready` | `ACTIVE` | |
| `CREATING` | `failed`, or create deadline | `CREATE_FAILED` | terminal for generation 1 |
| `ACTIVE` | `failed` | `DEGRADED` | |
| `DEGRADED` | `ready` | `ACTIVE` | health clears |
| `ACTIVE`, `DEGRADED`, `CREATE_FAILED`, `UPDATE_FAILED` | `UpdateAddon` accepted | `UPDATING` | generation +1 |
| `UPDATING` | `ready` | `ACTIVE` | update `Successful` |
| `UPDATING` | `failed`, or update deadline | `UPDATE_FAILED` | update `Failed`; terminal for that generation |
| `CREATING`, `ACTIVE`, `DEGRADED`, `CREATE_FAILED`, `UPDATE_FAILED` | `DeleteAddon` accepted | `DELETING` | generation +1 |
| `UPDATING` | `DeleteAddon` accepted | `DELETING` | generation +1; in-flight update `Cancelled` |
| `DELETING` | `DeleteAddon` | `DELETING` | idempotent; same generation; record returned |
| `DELETE_FAILED` | `DeleteAddon` | `DELETING` | same generation; deadline re-armed |
| `DELETING` | delete deadline | `DELETE_FAILED` | record and directive retained |
| `DELETING`, `DELETE_FAILED` | `removed` or `relinquished` | absent | record and manifest deleted |
| `CREATE_FAILED`, `UPDATE_FAILED` | `ready` for the same generation | unchanged | recorded diagnostically in `observed`; does not restore `ACTIVE` |

Failure is terminal for its generation and recoverable only by a new generation (R3).
Today `ready` lifts `CREATE_FAILED` to `ACTIVE`; that changes, because a failed update must keep a stable `Failed` outcome in `DescribeUpdate` and Terraform should not see a failed resource silently turn healthy.
The cost is low: the agent reports `failed` only for deterministic causes (missing baked bundle, webhook certificate generation, a directive the guest cannot realise), and a slow rollout reports `applied`, not `failed`.
`DEGRADED` is different: it describes a realised generation whose health dipped, so a valid `ready` for the current generation returns it to `ACTIVE`.
Staging is no longer a failure source, because the listing is a projection of the record.

## 7. Per-field decisions

The catalogue holds five bundles, one version each: `aws-load-balancer-controller` 2.11.0 and `aws-ebs-csi-driver` 1.40.1 (both `RequiresIRSA`), `argocd` 3.0.23, and the hidden `nvidia-device-plugin` 0.17.4 and `spinifex-noop` 0.1.0.
No accepted field may be stored and ignored.
Every rejection of invalid customer input uses `InvalidParameterException` (`awserrors.ErrorEKSInvalidParameter`); today's add-on validation returns `InvalidParameterValue`, which is not an EKS error code, and correcting it is part of slice 2.
State conflicts use `InvalidRequestException` and an existing resource on create uses `ResourceInUseException` (R11).

| Field | Today | Decision |
|---|---|---|
| `addonVersion` | Validated against the catalogue; applied. | Keep. Each bundle supports its one version; an unknown version is rejected. A change advances the generation. |
| `serviceAccountRoleArn` | Stored, `iam:PassRole` checked (already implemented, `gateway/eks/passrole.go`), rendered as web-identity env when the bundle has IRSA markers. | Implement for `aws-load-balancer-controller` and `aws-ebs-csi-driver`; keep the `iam:PassRole` check (R10). Reject for `argocd`, `nvidia-device-plugin` and `spinifex-noop`, whose bundles have no IRSA markers, so the role would be inert (R14). An update that changes it advances the generation. |
| `configurationValues` | Stored, staged, transported, discarded by the agent. | Reject a non-empty value for every bundle until that bundle publishes a schema through `DescribeAddonConfiguration` and the agent renders it (R9). Implement per bundle afterwards, `aws-load-balancer-controller` first. Existing stored values are kept and flagged (Section 3). |
| `resolveConflicts` | Accepted and dropped. | Create: accept all three values; with no pre-existing install AWS treats them alike. The v2 agent with `NONE` or `PRESERVE` checks the bundle's objects before the first render and reports `failed` on a conflict. Update: accept `OVERWRITE` (what K3s auto-deploy does); reject `NONE` and `PRESERVE` until field-level drift detection exists, because K3s will overwrite regardless. Persist the value and return it as an update param. |
| `preserve` (delete) | Ignored. | Implement as `relinquish` (Section 4). Clusters without a v2 executor cannot delete at all (Section 9). |
| `podIdentityAssociations` | Accepted and dropped. | Reject a non-empty list; EKS Pod Identity is not implemented. Accept an absent field and, on update, an empty list, which is a correct no-op because no associations exist. Return the field empty. |
| `clientRequestToken` | Dropped. | Implement for create and update (Section 8). |
| `tags` | Stored on create and returned; tag actions on the add-on ARN return `NotImplemented`. | Keep create-time tags. Implement `TagResource`, `UntagResource` and `ListTagsForResource` for add-on ARNs as an owner-provided tag mutation that does not advance the generation and is not gated, because a Terraform tag change on `aws_eks_addon` otherwise fails. |
| `namespaceConfig` | Not in the pinned SDK model, so silently dropped when the body is decoded. | Reject at the gateway when present in the raw body, while the pinned SDK cannot represent or apply it; never drop it silently (R8, slice 1). |

## 8. Client token idempotency

The token is stored in the record it created, not in a TTL bucket, so create stays one atomic write.

- `CreateAddon` becomes a CAS-create (`kv.Create`), fixing contract gap 1.
  The record stores the token and a hash of the request without the token.
- A create that loses to an existing record compares tokens: same token and same hash returns the existing add-on as it now stands; same token and a different hash is rejected; a different or absent token is `ResourceInUseException`.
- `UpdateAddon` with a token already held by a retained update of the current incarnation returns that update if the hash matches and is rejected if it does not; the replayed update is not re-applied and does not advance the generation.
- `DeleteAddon` has no token; a repeat while `DELETING` returns the record again, and a repeat once the record is gone is `ResourceNotFoundException`, as today.
- The token lives as long as its record or retained update. A retry after the add-on was deleted creates a new incarnation.
- A token reused with different parameters: recommend `InvalidParameterException`, because `IdempotentParameterMismatch` is not in the EKS error lists, although `CreateCluster` returns it today (open question 1).

## 9. Guest contract evolution and the v1 compatibility gate

`contracts/eks/v1` is frozen; its README already says a generation-aware protocol needs a successor.

- `contracts/eks/v2` adds the directive and report fields of Section 2, the new phases and structured `reason` codes.
  It is served on a new internal route (for example `GET /clusters/{c}/internal-addons/v2/{account}`) and reports go on a new subject (for example `eks.addon.v2.{account}.{cluster}.status`), so neither side has to sniff payloads.
- Following Q-75, the producer never emits v2 to a guest that has not asked for it, and v1 routes, subject and bodies stay byte-identical, pinned by the existing contract tests.
- The v2 internal routes do not return AWS error codes such as `InvalidParameterValue` (R7).
  A malformed guest request gets a structured internal reason (for example `NotActiveExecutor`, `StaleExecutorEpoch`, `UnknownContractMajor`, `MalformedReport`).
  Invalid customer input never reaches the guest: the AWS adapter rejects it with `InvalidParameterException`.
  A directive the guest finds it cannot realise (for example a bundle missing from the image) is reported as `failed` with a `reason`, and becomes `CREATE_FAILED`, `UPDATE_FAILED` or `DEGRADED` through Section 6.
- The v2 agent ships in a new EKS node image, so a cluster stays on v1 until its control-plane VM is replaced; the mixed window can be long.

How v2 capability is established: the cluster owner records it per executor epoch, from the authenticated caller only.
The gateway already authenticates the internal routes as a control-plane VM session and binds the session name (the VM's instance ID) to the named cluster (`AuthorizeInternal`).
For the v2 route it additionally requires that instance ID to be the cluster's active executor for the current epoch (Section 10), and only then does the cluster owner mark that epoch v2-capable.
No field in a guest payload can assert capability or executor identity.
A new epoch starts without capability, so the gate re-closes on every executor change until the new executor fetches v2.

Gated actions: on a cluster whose current epoch is not v2-capable, `CreateAddon`, `UpdateAddon` and `DeleteAddon` (with or without `preserve`) return `InvalidRequestException` with a message instructing the operator to upgrade the cluster's control-plane image.
Each of these needs either a fenced report (create and update reach `ACTIVE` only on a report for the current incarnation and generation) or observed deletion, which v1 cannot provide.
Not gated: `DescribeAddon`, `ListAddons`, `DescribeUpdate`, `ListUpdates`, `DescribeAddonVersions`, `DescribeAddonConfiguration`, tag actions, and cluster-teardown removal (Section 4).

v1 clusters during the transition:

| Concern | v1 handling |
|---|---|
| Reads | Records are readable and listed; the v1 route serves a v1 projection of the records. |
| Reports | v1 reports carry no incarnation, generation or epoch, so they never complete a create, update or delete. They are recorded diagnostically in `observed` with contract `v1`. Whether they may still move an add-on with no operation in flight between `ACTIVE` and `DEGRADED` is open question 6. |
| Operations in flight at activation | A migrated `CREATING` or `UPDATING` record cannot complete on v1 and reaches `CREATE_FAILED` or `UPDATE_FAILED` at its deadline; it converges after the image upgrade through a new `UpdateAddon` or a `DeleteAddon`. |
| Conformance | Q-146 conformance is claimed only for clusters whose executor has proven v2. |

This is a compatibility gate, not a hard cutover: v1 guests keep working, nothing is migrated destructively, and the gate opens per cluster once its image is upgraded.

Activation and finalisation are separate steps:

- **Activation** turns on the new lifecycle semantics and the gate once every daemon in the Region is known to run the new release, so no older binary can complete a delete by erasing the record; the transitional record stays readable by the previous release, so rollback is still possible.
- **Finalisation** is an explicit, operator-visible add-on migration action (R16).
  Its preflight shows evidence that legacy consumers are gone: no daemon older than the activating release in the Region, and no remaining reader of the manifest key or legacy record fields.
  It then stops writing the manifest key and legacy fields, and ends rollback.
  It never runs because a new binary started.

Removing v1 requires a published retirement, detection that no cluster in the Region still fetches the v1 route, and evidence that every cluster has moved to a v2 image; until then both are served.

## 10. Delivery executor

Delivery belongs to the EKS cluster, not to whichever server VM was first booted as primary (R6).
Today `eks-node-role.sh` enables `mulga-eks-addon-sync` only on the primary server, and any control-plane member that passes `AuthorizeInternal` may fetch and report.

Ownership split:

| Owner | Owns |
|---|---|
| Add-on owner (`domains/eks/addon`) | Desired state, generation, observed lifecycle and the comparison rule. |
| Cluster owner | Assignment of the active delivery executor: `executor {instanceId, epoch}`, plus whether that epoch has proven v2. |

The add-on package declares a narrow capability, for example `ActiveDeliveryExecutor(account, cluster) -> {instanceId, epoch, v2Capable}`, and the cluster owner provides it.
The add-on package does not read `ClusterMeta` or other cluster internals.
The gateway's internal-route check uses the same capability.

Rules:

- Only the active executor for the current epoch may fetch the v2 listing or post a v2 report; the gateway derives the executor identity from the authenticated caller and rejects any other control-plane member with `NotActiveExecutor`.
- Every report carries the `executorEpoch` it was issued in its listing; the gateway rejects a mismatched epoch, and the owner re-checks it (Section 2 rule 1) because the epoch may advance between the gateway check and the CAS.
- A replaced executor is fenced before its successor is assigned: the cluster owner advances the epoch only after the old executor is confirmed unable to render (VM stopped or terminated, or its delivery service disabled), not merely because it is unreachable.
  There is never an autonomous second primary.
- The new executor reads durable desired state from the listing and resumes delivery and reconciliation, including retained `remove` directives; nothing advances `desired.generation`.
- The handover interval is guest unavailability: operations keep their persisted deadlines, and a deadline that passes during handover produces the same `CREATE_FAILED`, `UPDATE_FAILED` or `DELETE_FAILED` as any outage.
  A cluster with no assigned executor is an unavailable guest.

The cluster lease `spinifex-eks-leader/{account}/{cluster}` elects the host-side daemon that reconciles the cluster; it is unrelated to the executor epoch, which fences the guest side.

## 11. Restart reconciliation

Every obligation is in the record, so restart needs no request memory.
An add-on reconcile pass runs under the cluster's existing lease (`spinifex-eks-leader/{account}/{cluster}`), at start and on a periodic tick, owned by `domains/eks/addon` and given its own host instead of the cluster reconciler's report callback (inventory edge E3).
For each record it:

- re-projects the listing and, until finalisation, rewrites a missing or stale manifest key, which re-drives a create or update interrupted after its CAS;
- keeps the `remove` or `relinquish` directive listed for `DELETING` and `DELETE_FAILED` records, which re-drives an interrupted delete;
- applies the persisted deadlines (create, update, delete); and
- retries nothing that already completed, because each transition is a CAS keyed on generation.

The only window left is a crash before the accepting CAS commits, in which case nothing was accepted and the client's retry with the same token is a new attempt.
Which daemon re-drives: whichever holds the cluster lease after restart, through the existing `SpawnRegisteredReconcilers` respawn; no new process role is introduced.

## 12. Status publications are notifications

Under Q-76 a delivery report is a notification, a hint that observed state may have changed, and never proof of completion by itself: completion is the owner's CAS after the Section 2 rule accepts a report for the current executor epoch, incarnation and generation.
Core NATS is therefore acceptable as the transport:

- **Lost:** the agent re-sends the current phase for every listed directive on every tick, including `removed` for a retained deletion, and the directive stays listed until the owner records completion; a report lost during a restart or with no lease holder is replaced within one tick.
- **Duplicated:** applying the same (`incarnationId`, `generation`, `phase`) twice changes nothing.
- **Delayed or reordered:** a late report from an earlier generation, incarnation or executor epoch fails the comparison rule; within one generation the next tick corrects it.
- **Never arriving:** deadlines turn silence into `CREATE_FAILED`, `UPDATE_FAILED` or a retained `DELETE_FAILED`, so liveness does not depend on delivery.

Correctness rests on durable desired state, periodic guest reporting and restart reconciliation, not on the transport, so no JetStream stream, queue group or delivery change is proposed.
v1 reports never drive a lifecycle completion (Section 9), so no weaker rule exists for v1.

## 13. GPU node-group auto-staging

`stageGPUDeviceAddon` (inventory edge E2) keeps calling the owner and gains the new create path: incarnation, generation 1, no token, no IRSA, `origin: auto-staged`.
Interactions under this design:

- An existing record in any status, including `DELETING`, remains a no-op for the node group, so the plugin is not re-staged while it is being removed.
- A user's deliberate deletion is remembered (R15): when a user-initiated `DeleteAddon` of `nvidia-device-plugin` completes, the add-on owner keeps a cluster-scoped declined marker in its own key space, and auto-staging skips the add-on while the marker exists.
  An explicit user `CreateAddon` of the add-on clears it, and cluster teardown removes it with the cluster.
  The marker's key, and whether a deletion of an `origin: user` record also sets it, are open question 7.
- On a cluster whose executor has not proven v2, the create path returns `InvalidRequestException` (Section 9), so auto-staging is logged and skipped, as its errors are today.
- `CREATE_FAILED` now persists (Section 6), so a device plugin that failed because the baked bundle was missing stays failed until updated or deleted; node-group readiness does not wait on it, and the failure is visible in `DescribeAddon`.
- Errors stay logged and swallowed, and node-group delete never unstages, until the node-group owner is extracted and the dependency is designed; this design does not change that edge.

## 14. Required evidence

Unit and owner tests (fault injection at the owner's store and installer seams), E2E and Terraform:

1. Interruption before the accepting CAS (nothing accepted; retry with the same token creates once) and after it (reconcile re-projects the directive; no second incarnation).
2. Repeated create (token replay returns the same incarnation), repeated update (same update id, generation advanced once) and repeated delete (same `DELETING` record, no second generation).
3. Stale report after a newer update: a `ready` and a `failed` for generation `G-1` arriving after `G` is accepted leave the add-on `UPDATING`.
4. Stale report after delete and recreate: a report carrying the old `incarnationId` never touches the new record.
5. Terminal failure: a `ready` for the failed generation after `CREATE_FAILED` or `UPDATE_FAILED` is recorded in `observed` and leaves the status unchanged; `DEGRADED` returns to `ACTIVE` on a current-generation `ready`.
6. Delete during `UPDATING` marks the update `Cancelled`.
7. Controller restart while a create, an update and a delete are each outstanding: every one converges after respawn with no request memory.
8. Unavailable guest during delete: the record stays `DELETING`, becomes `DELETE_FAILED` at the persisted deadline, and the directive stays listed.
9. Eventual guest recovery: a `removed` report after `DELETE_FAILED` completes deletion; an `applied` then `ready` after a long outage completes a pending create or update if its deadline has not passed.
10. Executor fencing: a fetch or report from a control-plane member that is not the active executor, or from a superseded epoch, is rejected at the gateway and at the owner; a handover resumes a pending create, update and delete without advancing the generation; the epoch does not advance while the old executor is merely unreachable.
11. v1 compatibility gate: `CreateAddon`, `UpdateAddon` and `DeleteAddon` on a cluster without a v2-capable epoch return `InvalidRequestException`; reads and tag actions still work; v1 reports never complete an operation; v1 body and subject bytes are unchanged; capability cannot be asserted through a payload field.
12. Field decisions: invalid customer input, including `namespaceConfig`, non-empty `configurationValues` and a role on a bundle without IRSA markers, returns `InvalidParameterException`; the role reaches the rendered bundle for both IRSA bundles.
13. `DescribeUpdate` and `ListUpdates` for add-ons: `InProgress`, `Successful`, `Failed` with errors, and `Cancelled`.
14. GPU auto-staging does not re-create `nvidia-device-plugin` after a user deleted it, and does after an explicit user create.
15. Finalisation preflight refuses while a legacy consumer remains, and finalisation is visible to the operator.
16. E2E in `tests/e2e/eks`: create, update (version and role), failed update, delete with observed removal, delete with `preserve`, guest outage across a delete, and executor replacement during an update.
17. Terraform `aws_eks_addon`: repeated apply and destroy, an in-place update (version or role) and a tag change, alongside the unit and E2E evidence, as ADR-0003 S5 requires.
    The Terraform provider's create, update and delete waiters are unverified and must be read before the fixture is written.

## 15. Implementation slices

Each slice is small and ordered; behaviour-changing slices are named as such.

| # | Slice | Behaviour change |
|---|---|---|
| 0 | Expand the record: write `schemaVersion`, `incarnationId`, `origin`, `desired`, `observed` beside the legacy fields; migrate existing records; nothing reads the new fields yet. | None visible; persisted bytes grow. |
| 1 | Reject `namespaceConfig` at the gateway when present in the raw body. | Yes: a previously dropped input is rejected. |
| 2 | CAS-create, create token in the record, `InvalidParameterException` for add-on validation errors. | Yes: concurrent and retried creates; error code. |
| 3 | Add-on reconcile pass with its own host under the cluster lease; listing built from records; manifest key kept for rollback. | None visible; clears the create-staging crash window. |
| 4 | Remaining field decisions of Section 7, except `preserve` and `resolveConflicts` conflict detection. | Yes: previously accepted inputs are rejected. |
| 5 | Cluster-owned delivery executor: executor identity and epoch, the declared capability, and the gateway's active-executor check on v2 routes. | None for v1 routes. |
| 6 | `contracts/eks/v2`, v2 route and subject, structured reasons, v2 agent in a new node image, executor-epoch, incarnation and generation fencing, v2 capability recorded per epoch. | Yes, for v2 clusters only. |
| 7 | Activation and the v1 compatibility gate for `CreateAddon`, `UpdateAddon` and `DeleteAddon`. | Yes: lifecycle mutations on v1 clusters return `InvalidRequestException`. |
| 8 | Durable update operation, `UPDATE_FAILED`, persisted profile deadlines, `DescribeUpdate`/`ListUpdates` for add-ons, update token, terminal failure. | Yes: update response and status. |
| 9 | Durable `DELETING`, `DELETE_FAILED`, delete deadline, recreate blocked while deleting, update `Cancelled` on delete. | Yes: delete response persists and describe no longer returns not found at once. |
| 10 | `preserve` as `relinquish` and create-time conflict detection. | Yes. |
| 11 | Tag actions on add-on ARNs. | Yes: `NotImplemented` becomes supported. |
| 12 | GPU declined marker for deliberately deleted auto-staged add-ons. | Yes: a deleted device plugin stays deleted. |
| 13 | Finalisation action with preflight; stop writing the manifest key and legacy fields. | None for current binaries; ends rollback. |

The per-incarnation ARN suffix and v1 retirement are separate later changes with their own migrations, not slices of this correction.

## 16. Open questions for the reviewer

These are implementation mechanics; the decisions above are not reopened.

1. Token reused with different parameters: `InvalidParameterException` (recommended) or `IdempotentParameterMismatch` to match `CreateCluster`, and should `CreateCluster` be revisited?
2. Existing stored `configurationValues`: keep and flag (recommended), or reject further updates to such add-ons until cleared?
3. How many updates to retain inline per incarnation?
4. Is there an existing Region-wide release gate that can establish "every daemon runs the new release" for activation and supply finalisation's preflight evidence, or does slice 7 need one?
5. Executor fencing mechanics: what evidence the cluster owner accepts that a replaced executor cannot render (instance state from the EC2 owner, a disabled service, an operator assertion), and who triggers reassignment.
6. May a v1 report still move an add-on with no operation in flight between `ACTIVE` and `DEGRADED`, given it carries no generation or epoch?
7. GPU declined marker: its key in the add-on owner's space, and whether deleting a user-created `nvidia-device-plugin` also sets it.
8. Profile keys and default values for the create, update and delete deadlines.
9. Rollback after activation: how the previous release presents records in `DELETING`, `DELETE_FAILED` or `UPDATE_FAILED`, which it does not model today.
