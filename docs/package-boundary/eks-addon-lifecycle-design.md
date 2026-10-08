# EKS managed add-on: lifecycle correction design

**Status:** Draft for requirements review; no behaviour changes until approved.

This is the Phase 2 design for correcting the EKS managed add-on lifecycle, written against the code at `18794e017`.
The present contract and its gaps are in `docs/package-boundary/eks-addon-lifecycle.md`, and that record stays authoritative for what the code does today.
Paths are relative to `spinifex/` unless they start with `tests/`, `scripts/` or `contracts/`, which are relative to the spinifex repo root.

## Standing

The design is resource-specific.
It refines proposed requirement Q-146 and the recorded conflict C-82 (`docs/specs/requirements/managed-services.md` in the mulga repo); both are open and pending review, and this document closes neither.
It does not create an architecture proposal, a universal lifecycle interface, a generic finalizer package or a shared repository; ADR-0003 S2 (accepted) rules those out without two real consumers.
It applies ADR-0003 S1 to S5 and ADR-0004 (both accepted) to one resource.
Q-75 (contract evolution) and Q-76 (command, event and notification semantics) are register entries marked Decided, which records who stated them, not team acceptance; this design follows them but treats them as open and does not cite them as accepted decisions.
It is not a NATS or JetStream redesign: every transport stays as it is.
Edge IDs E1 to E9 refer to Section 7.1 of `docs/package-boundary/adr-0004-inventory-eks.md`.

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

Not verified, and marked where used: which error AWS returns for an update or delete issued while another operation is in progress; whether `ready` after `CREATE_FAILED` or `UPDATE_FAILED` returns the add-on to `ACTIVE`; how long AWS honours a `clientRequestToken`; what a token reused with different parameters returns; whether `DescribeUpdate` finds an update of a deleted predecessor; the `AddonIssue` code set; whether a role passed to an add-on that needs no IAM permissions is rejected; and the Terraform provider's waiters.

## 1. Identity and generation

| Item | Design |
|---|---|
| Incarnation | `incarnationId`: a random UUID minted when `CreateAddon` is accepted and never changed. Delete followed by create of the same name yields a new incarnation. |
| Desired generation | `desired.generation`: an unsigned integer, 1 on create, incremented by exactly one in the same CAS that accepts each `UpdateAddon` and each `DeleteAddon`. Tag changes do not advance it, because tags are not delivered. |
| Where stored | In the add-on record `clusters/{cluster}/addons/{addon}` in `eks-account-{account}`, next to the existing fields, so one revision CAS covers intent, generation and status. No new bucket or key family. |
| Lookup key | Still cluster plus add-on name; the incarnation is a fence, not an address. |

ARN compatibility (contract gap 10) is related but separable.
Today the ARN is `addon/{cluster}/{addon}`, minted by the owner and independently derived by the gateway authorizer from names (inventory edge E9).

| Option | Effect | Cost |
|---|---|---|
| A. Keep the name-only ARN; incarnation stays internal | No change to authorization or existing IAM policies. | The compatibility gap remains: a recreated add-on reuses its predecessor's ARN. |
| B. New incarnations get `addon/{cluster}/{addon}/{incarnationId}`; migrated records keep their legacy ARN | Matches the AWS shape for new add-ons. | The authorizer must obtain the ARN from the owner before the policy check (one extra lookup per add-on action), IAM policies naming the exact legacy ARN stop matching new add-ons, and `CreateAddon`'s authorization resource must be settled. |
| C. As B with a suffix derived from (account, cluster, name, incarnation) | Same as B. | Same as B; no advantage over storing the UUID. |

Recommendation: option A for this correction, with `incarnationId` shaped as a UUID so that B stays possible as its own reviewed change.
The ARN decision touches authorization and customer policies, which this design should not couple to a lifecycle fix.

## 2. Directives, reports and the stale-report rule

A directive is what the controller wants the guest to realize for one add-on; a report is what the guest observed.
In the successor contract (Section 9) both carry `incarnationId` and `generation`, not only name and version.

Directive (one entry of the staged listing):

| Field | Meaning |
|---|---|
| `addonName`, `incarnationId`, `generation` | Identity and the desired generation this directive represents. |
| `mode` | `install`, `remove` or `relinquish`. |
| `addonVersion`, `serviceAccountRoleArn`, `configurationValues` | The desired spec; for `remove` and `relinquish`, the last installed spec, so a guest without local state can still render the object list it must remove. |

Report: `addon`, `incarnationId`, `generation`, `phase` (`applied`, `ready`, `failed`, `removing`, `removed`, `relinquished`), `message`, `ts`.

Exact comparison rule, applied by the owner to record `R` and report `P`:

1. If `R` is absent, discard `P`.
2. If `P.incarnationId != R.incarnationId`, discard `P` as a stale incarnation.
   This rejects every report from before a delete and recreate.
3. If `P.generation < R.desired.generation`, discard `P` as stale.
   This rejects reports produced for a superseded update and reports produced before a delete was accepted.
4. If `P.generation > R.desired.generation`, discard `P` and log a protocol error; a guest cannot learn a generation the controller has not issued.
5. If `P.phase` does not fit `R.desired.mode` (`ready` while removing, `removed` while installing), discard `P` and log a protocol error.
6. Otherwise apply the transition in Section 6 through a revision CAS on `R`; on a CAS conflict, re-read `R` and start again from step 1.

Within one generation, reports apply in arrival order.
A reordered pair from the same generation describes the same desired state, and the level-triggered agent corrects it on its next tick.
Discards never write the record; they are counted in a metric so a stuck guest is visible.

## 3. Desired intent separated from observed state

Record shape sketch (JSON field names; existing fields unchanged):

```json
{
  "schemaVersion": 2,
  "addonName": "aws-load-balancer-controller",
  "arn": "arn:aws:eks:...:addon/prod/aws-load-balancer-controller",
  "incarnationId": "6f1c...",
  "createdAt": "...", "modifiedAt": "...", "tags": {},
  "createToken": {"token": "...", "paramHash": "..."},
  "desired": {
    "generation": 3, "acceptedAt": "...", "mode": "install",
    "addonVersion": "2.11.0", "serviceAccountRoleArn": "...", "configurationValues": "",
    "resolveConflicts": "OVERWRITE",
    "deletion": null
  },
  "observed": {"generation": 2, "phase": "ready", "message": "", "receivedAt": "...", "contract": "v2"},
  "status": "UPDATING",
  "health": [{"code": "...", "message": "..."}],
  "deadline": "...",
  "updates": [{"id": "...", "type": "AddonUpdate", "status": "InProgress", "generation": 3,
               "params": [{"type": "AddonVersion", "value": "2.11.0"}], "createdAt": "...",
               "token": "...", "paramHash": "...", "errors": []}],
  "addonVersion": "2.11.0", "serviceAccountRoleArn": "...", "configurationValues": ""
}
```

`desired` is written only by API commands; `observed` only by accepted reports; `status`, `health` and `deadline` are owner-derived from both and stored so reads stay a single get.
The trailing top-level `addonVersion`, `serviceAccountRoleArn` and `configurationValues` are the legacy fields, kept in step with `desired` during the transition so an older binary can still read the record.
Updates are kept inline, bounded to the most recent N per incarnation, so accepting an update and advancing the generation is one CAS rather than a two-key write.

The staged manifest sub-key becomes a projection of `desired`.
The internal listing is built from the records, and the manifest key is still written for rollback until finalisation.
That removes today's crash window between the record put and staging (contract row "Restart and interruption"), because there is no longer a second write that can be missed.

Migration of existing records, as an idempotent and resumable expand step:

| Today's status | `incarnationId` | `desired.generation` | `observed` | Status after |
|---|---|---|---|---|
| `ACTIVE`, `DEGRADED` | newly minted | 1 | generation 1, phase from status, contract `v1` | unchanged |
| `CREATING`, `UPDATING` | newly minted | 1 | none | unchanged; a deadline starts at migration |
| `CREATE_FAILED` | newly minted | 1 | none | unchanged, with the stored `health` as an issue |

Minting at migration is safe because no deployed report carries an incarnation.
Existing non-empty `configurationValues` are preserved, never silently dropped, and gain a health issue stating they are not applied (Section 7).
An older binary that rewrites a migrated record decodes it into today's `Record` and drops the new fields, so the new fields are only acted on after an operator-visible finalisation confirms every daemon runs the new release, as Q-75 describes; until then the new binary behaves as today.

## 4. Deletion

`DeleteAddon` creates a durable obligation and keeps ownership until guest-side removal is observed:

1. One CAS sets `status: DELETING`, advances `desired.generation` to `D`, sets `desired.mode` to `remove` (or `relinquish` when `preserve=true`), records `desired.deletion.requestedAt` and starts the delete deadline.
   The response is the record in that state.
2. The listing now carries a `remove` or `relinquish` directive for generation `D` instead of omitting the add-on.
3. The record stays until a report for (`incarnationId`, `D`) with phase `removed` or `relinquished` arrives; the owner then deletes the record and the manifest key.
4. While the record exists, `CreateAddon` of the same name returns `ResourceInUseException`, and `DescribeAddon` and `ListAddons` show it as `DELETING` or `DELETE_FAILED`.
   Recreate is therefore serialized behind removal, so a new incarnation never overlaps its predecessor's objects in the guest.

What "observed" means for a v2 guest:
today's GC loop deletes the rendered file, runs `kubectl delete --wait=false` on a temporary copy and forgets it.
The v2 agent keeps the removed manifest in a durable per-add-on location instead of `mktemp`, and on every tick runs `kubectl get -f <copy> --ignore-not-found`.
It reports `removing` while any listed object remains and `removed` only when every object in the rendered manifest returns NotFound, including namespaces held by finalizers.
If the apiserver is unreachable, it sends nothing for that add-on rather than `removed`.
If it has no local copy (state lost, or never rendered), it renders the bundle from the directive's last spec into a scratch file and uses that object list.
It keeps reporting `removed` on every tick while the directive is listed, so a lost report is repeated.

For `relinquish`, the v2 agent deletes only the rendered file, leaving the objects in place (K3s does not delete applied objects when the auto-deploy file disappears, per the agent's own comment), and reports `relinquished` once the file is gone.
What K3s does with its `Addon` object in that case is unverified.

v1 guests cannot report removal, and Section 9 sets out what the controller does for them.

Unavailable guest: the record stays `DELETING` and the directive stays listed.
When the delete deadline passes, the owner sets `DELETE_FAILED` with a health issue, keeps the record and keeps listing the directive.
`DELETE_FAILED` is recoverable: a later `removed` report for (`incarnationId`, `D`) completes the deletion, and a repeated `DeleteAddon` re-arms the deadline without advancing the generation, since the intent is unchanged.
A guest that recovers after hours therefore converges the retained directive with no operator action.

Cluster deletion destroys the control-plane VMs and with them every in-cluster object, so add-on removal is implied there.
That path should call an add-on-owned teardown capability that records removal-by-cluster-teardown instead of the blanket `DeleteClusterPrefix` sweep (inventory edge E1).

## 5. Update as a durable asynchronous operation

`UpdateAddon` is accepted in one CAS that advances `desired.generation` to `G`, sets `status: UPDATING`, appends an update `{id: new UUID, type: AddonUpdate, status: InProgress, generation: G, params}` and starts the update deadline.
The response is that update, not the add-on ARN marked `Successful`.

| Event for generation `G` | Update status | Add-on status |
|---|---|---|
| `ready` report | `Successful` | `ACTIVE` |
| `failed` report | `Failed`, with the message in `errors` | `UPDATE_FAILED` |
| Deadline passes with no `ready` | `Failed` ("timed out") | `UPDATE_FAILED` |
| `DeleteAddon` accepted first | `Failed` or `Cancelled` (open question 9) | `DELETING` |

`DescribeUpdate` with `addonName` routes to the add-on owner and finds the update by `id` among the record's retained updates for the current incarnation; an unknown `id` is `ResourceNotFoundException`.
Whether AWS still resolves an update of a deleted predecessor is unverified.
`ListUpdates` with `addonName` lists the retained ids.
Both are gateway stubs today, so this slice also implements them for add-ons only; cluster and node-group updates stay stubs.

`UpdateAddon` is accepted from `ACTIVE`, `DEGRADED`, `CREATE_FAILED` and `UPDATE_FAILED`.
From `CREATING`, `UPDATING`, `DELETING` and `DELETE_FAILED` it is rejected; `ResourceInUseException` is recommended, and the AWS choice between that and `InvalidRequestException` is unverified.
An update whose parameters equal the current desired spec is still accepted as a new generation, so a client gets a trackable update.

## 6. Status set and transitions

The owner adds `UPDATE_FAILED` and `DELETE_FAILED` to its `Status`; `DELETING` becomes a stored status.
Every report row below assumes the report passed the rule in Section 2.

| From | Event | To | Notes |
|---|---|---|---|
| absent | `CreateAddon` accepted | `CREATING` | generation 1; create deadline starts |
| `CREATING` | `ready` | `ACTIVE` | |
| `CREATING` | `failed`, or create deadline | `CREATE_FAILED` | |
| `ACTIVE` | `failed` | `DEGRADED` | |
| `DEGRADED` | `ready` | `ACTIVE` | health clears |
| `ACTIVE`, `DEGRADED`, `CREATE_FAILED`, `UPDATE_FAILED` | `UpdateAddon` accepted | `UPDATING` | generation +1 |
| `UPDATING` | `ready` | `ACTIVE` | update `Successful` |
| `UPDATING` | `failed`, or update deadline | `UPDATE_FAILED` | update `Failed` |
| any except `DELETING` | `DeleteAddon` accepted | `DELETING` | generation +1 |
| `DELETE_FAILED` | `DeleteAddon` | `DELETING` | same generation; deadline re-armed |
| `DELETING` | delete deadline | `DELETE_FAILED` | record and directive retained |
| `DELETING`, `DELETE_FAILED` | `removed` or `relinquished` | absent | record and manifest deleted |
| `CREATE_FAILED`, `UPDATE_FAILED` | `ready` for the same generation | unchanged (recommended) | observation recorded in `observed` and `health` |

Failure is terminal for the generation but recoverable by a new generation.
Today `ready` lifts `CREATE_FAILED` to `ACTIVE`.
The recommendation is that `CREATE_FAILED` and `UPDATE_FAILED` stay until an `UpdateAddon` or `DeleteAddon`, because a failed update must keep a stable `Failed` outcome in `DescribeUpdate` and Terraform should not see a failed resource silently turn healthy.
The cost is low: the agent reports `failed` only for deterministic causes (missing baked bundle, webhook certificate generation), and a slow rollout reports `applied`, not `failed`.
Whether AWS lifts these states on its own is unverified (open question 1).
Staging is no longer a failure source, because the listing is a projection of the record.

## 7. Per-field decisions

The catalogue holds five bundles, one version each: `aws-load-balancer-controller` 2.11.0 and `aws-ebs-csi-driver` 1.40.1 (both `RequiresIRSA`), `argocd` 3.0.23, and the hidden `nvidia-device-plugin` 0.17.4 and `spinifex-noop` 0.1.0.
No accepted field may be stored and ignored.
Every rejection uses `InvalidParameterException` (`awserrors.ErrorEKSInvalidParameter`); today's add-on validation returns `InvalidParameterValue`, which is not an EKS error code, and correcting it is part of slice 1.

| Field | Today | Recommendation |
|---|---|---|
| `addonVersion` | Validated against the catalogue; applied. | Keep. Each bundle supports its one version; an unknown version is rejected. A change advances the generation. |
| `serviceAccountRoleArn` | Stored, `iam:PassRole` checked, rendered as web-identity env when the bundle has IRSA markers. | Implement for `aws-load-balancer-controller` and `aws-ebs-csi-driver`. Reject for `argocd`, `nvidia-device-plugin` and `spinifex-noop`, whose bundles have no IRSA markers, so the role would be inert; AWS's behaviour here is unverified (open question 10). An update that changes it advances the generation. |
| `configurationValues` | Stored, staged, transported, discarded by the agent. | Reject a non-empty value for every bundle until that bundle publishes a schema through `DescribeAddonConfiguration` and the agent renders it. Implement per bundle afterwards, `aws-load-balancer-controller` first. Existing stored values are kept and flagged (Section 3). |
| `resolveConflicts` | Accepted and dropped. | Create: accept all three values; with no pre-existing install AWS treats them alike. A v2 agent with `NONE` or `PRESERVE` checks the bundle's objects before the first render and reports `failed` on a conflict; on v1 clusters reject `NONE` and `PRESERVE`. Update: accept `OVERWRITE` (what K3s auto-deploy does); reject `NONE` and `PRESERVE` until field-level drift detection exists, because K3s will overwrite regardless. Persist the value and return it as an update param. |
| `preserve` (delete) | Ignored. | Implement as `relinquish` for v2 guests (Section 4). On v1 clusters reject `preserve=true`, because the v1 agent always runs `kubectl delete`. |
| `podIdentityAssociations` | Accepted and dropped. | Reject a non-empty list; EKS Pod Identity is not implemented. Accept an absent field and, on update, an empty list, which is a correct no-op because no associations exist. Return the field empty. |
| `clientRequestToken` | Dropped. | Implement for create and update (Section 8). |
| `tags` | Stored on create and returned; tag actions on the add-on ARN return `NotImplemented`. | Keep create-time tags. Implement `TagResource`, `UntagResource` and `ListTagsForResource` for add-on ARNs as an owner-provided tag mutation that does not advance the generation, because a Terraform tag change on `aws_eks_addon` otherwise fails. |
| `namespaceConfig` | Not in the pinned SDK model, so silently dropped when the body is decoded. | Reject when present in the raw body, until the model and the bundles support a namespace override. |

## 8. Client token idempotency

The token is stored in the record it created, not in a TTL bucket, so create stays one atomic write.

- `CreateAddon` becomes a CAS-create (`kv.Create`), fixing contract gap 1.
  The record stores the token and a hash of the request without the token.
- A create that loses to an existing record compares tokens: same token and same hash returns the existing add-on as it now stands; same token and a different hash is rejected; a different or absent token is `ResourceInUseException`.
- `UpdateAddon` with a token already held by a retained update of the current incarnation returns that update if the hash matches and is rejected if it does not; the replayed update is not re-applied and does not advance the generation.
- `DeleteAddon` has no token; a repeat while `DELETING` returns the record again, and a repeat once the record is gone is `ResourceNotFoundException`, as today.
- The token lives as long as its record or retained update. A retry after the add-on was deleted creates a new incarnation.
- A token reused with different parameters: recommend `InvalidParameterException`, because `IdempotentParameterMismatch` is not in the EKS error lists, although `CreateCluster` returns it today (open question 4).

## 9. Guest contract evolution and v1 coexistence

`contracts/eks/v1` is frozen; its README already says a generation-aware protocol needs a successor.

- `contracts/eks/v2` adds the directive and report fields of Section 2 and the new phases.
  It is served on a new internal route (for example `GET /clusters/{c}/internal-addons/v2/{account}`) and reports go on a new subject (for example `eks.addon.v2.{account}.{cluster}.status`), so neither side has to sniff payloads.
- A guest proves v2 capability by fetching the v2 route.
  The controller records the cluster's guest contract in `observed.contract` and serves v1 to any guest that asks for v1.
  Following Q-75, the producer never emits v2 to a guest that has not asked for it.
- The v2 agent ships in a new EKS node image, so a cluster stays on v1 until its control-plane VM is replaced; the mixed window can be long.

Controller treatment of v1 clusters during the transition:

| Concern | v1 handling |
|---|---|
| Reports | v1 reports carry no incarnation or generation, so they cannot be fenced. They are applied only when `report.version` equals `desired.addonVersion` and arrive at least a settle window after `desired.acceptedAt`. The window is derived from the agent's sync interval and publish retry budget (30 s plus 30 attempts at 5 s). This is best-effort, not a fence. |
| Same-version updates | A role-only update on a v1 cluster cannot be told apart from the previous generation; it completes on the first matching report after the settle window, and the update's params record that it was completed under v1. |
| Deletion | The v1 agent sends nothing on removal. The owner treats deletion as complete once the internal listing that omits the add-on has been served twice after `DELETING` was set. The agent's loop runs fetch, render, then GC in sequence, so the second fetch implies one GC pass has already issued `kubectl delete --wait=false`. The health message and the delete result state that removal was requested in the guest, not observed. |
| Preserve, `resolveConflicts` `NONE`/`PRESERVE` | Rejected on v1 clusters (Section 7). |
| Conformance | Q-146 conformance is claimed only for clusters whose guest has proven v2. |

v1 is never silently replaced: v1 routes, subject and bodies stay byte-identical, pinned by the existing contract tests.
Removing v1 requires a published retirement, detection that no cluster in the Region still fetches the v1 route, and evidence that every cluster has moved to a v2 image; until then both are served.

## 10. Restart reconciliation

Every obligation is in the record, so restart needs no request memory.
An add-on reconcile pass runs under the cluster's existing lease (`spinifex-eks-leader/{account}/{cluster}`), at start and on a periodic tick, owned by `domains/eks/addon` and given its own host instead of the cluster reconciler's report callback (inventory edge E3).
For each record it:

- re-projects the listing and, during the transition, rewrites a missing or stale manifest key, which re-drives a create or update interrupted after its CAS;
- keeps the `remove` or `relinquish` directive listed for `DELETING` and `DELETE_FAILED` records, which re-drives an interrupted delete;
- applies deadlines (create, update, delete) and the v1 deletion-completion rule; and
- retries nothing that already completed, because each transition is a CAS keyed on generation.

The only window left is a crash before the accepting CAS commits, in which case nothing was accepted and the client's retry with the same token is a new attempt.
Which daemon re-drives: whichever holds the cluster lease after restart, through the existing `SpawnRegisteredReconcilers` respawn; no new process role is introduced.

## 11. Status publications are notifications

Under this design a delivery report is a notification, a hint that observed state may have changed, and never proof of completion by itself: completion is the owner's CAS after the Section 2 rule accepts a report for the current incarnation and generation.
Core NATS is therefore acceptable as the transport:

- **Lost:** the agent re-sends the current phase for every listed directive on every tick, including `removed` for a retained deletion, and the directive stays listed until the owner records completion; a report lost during a restart or with no lease holder is replaced within one tick.
- **Duplicated:** applying the same (`incarnationId`, `generation`, `phase`) twice changes nothing.
- **Delayed or reordered:** a late report from an earlier generation or incarnation fails the comparison rule; within one generation the next tick corrects it.
- **Never arriving:** deadlines turn silence into `CREATE_FAILED`, `UPDATE_FAILED` or a retained `DELETE_FAILED`, so liveness does not depend on delivery.

Correctness rests on durable desired state, periodic guest reporting and restart reconciliation, not on the transport, so no JetStream stream, queue group or delivery change is proposed.
This holds fully for v2 guests; the v1 rules in Section 9 are weaker and are marked as such.

## 12. GPU node-group auto-staging

`stageGPUDeviceAddon` (inventory edge E2) keeps calling the owner and gains the new create path: incarnation, generation 1, no token, no IRSA.
Interactions under this design:

- An existing record in any status, including `DELETING`, remains a no-op for the node group, so a user's delete of `nvidia-device-plugin` is respected and the plugin is not re-staged while it is being removed.
  Whether a later GPU node-group launch should re-create it after removal completes is open question 13; today it does, because the record is then absent.
- `CREATE_FAILED` now persists (Section 6), so a device plugin that failed because the baked bundle was missing stays failed until updated or deleted; node-group readiness does not wait on it, and the failure is visible in `DescribeAddon`.
- Errors stay logged and swallowed, and node-group delete never unstages, until the node-group owner is extracted and the dependency is designed; this design does not change that edge.

## 13. Required evidence

Unit and owner tests (fault injection at the owner's store and installer seams), E2E and Terraform:

1. Interruption before the accepting CAS (nothing accepted; retry with the same token creates once) and after it (reconcile re-projects the directive; no second incarnation).
2. Repeated create (token replay returns the same incarnation), repeated update (same update id, generation advanced once) and repeated delete (same `DELETING` record, no second generation).
3. Stale report after a newer update: a `ready` and a `failed` for generation `G-1` arriving after `G` is accepted leave the add-on `UPDATING`.
4. Stale report after delete and recreate: a report carrying the old `incarnationId` never touches the new record.
5. Controller restart while a create, an update and a delete are each outstanding: every one converges after respawn with no request memory.
6. Unavailable guest during delete: the record stays `DELETING`, becomes `DELETE_FAILED` at the deadline, and the directive stays listed.
7. Eventual guest recovery: a `removed` report after `DELETE_FAILED` completes deletion; an `applied` then `ready` after a long outage completes a pending create or update if its deadline has not passed.
8. v1 coexistence: v1 reports for the matching version apply after the settle window and are ignored before it; v1 deletion completes on the second post-delete listing; v1 body and subject bytes are unchanged.
9. Field decisions: every rejected field returns `InvalidParameterException`; the role reaches the rendered bundle for both IRSA bundles.
10. `DescribeUpdate` and `ListUpdates` for add-ons: `InProgress`, `Successful` and `Failed` with errors.
11. E2E in `tests/e2e/eks`: create, update (version and role), failed update, delete with observed removal, delete with `preserve`, and guest outage across a delete.
12. Terraform `aws_eks_addon`: repeated apply and destroy, an in-place update (version or role) and a tag change, alongside the unit and E2E evidence, as ADR-0003 S5 requires.
    The Terraform provider's create, update and delete waiters are unverified and must be read before the fixture is written.

## 14. Implementation slices

Each slice is small and ordered; behaviour-changing slices are named as such.

| # | Slice | Behaviour change |
|---|---|---|
| 0 | Expand the record: write `schemaVersion`, `incarnationId`, `desired`, `observed` beside the legacy fields; migrate existing records; nothing reads the new fields yet. | None visible; persisted bytes grow. |
| 1 | CAS-create, create token in the record, `InvalidParameterException` for add-on validation errors. | Yes: concurrent and retried creates; error code. |
| 2 | Add-on reconcile pass with its own host under the cluster lease; listing built from records; manifest key kept for rollback. | None visible; clears the create-staging crash window. |
| 3 | Field decisions of Section 7, except `preserve` on v2 and `resolveConflicts` conflict detection. | Yes: previously accepted inputs are rejected. |
| 4 | Durable update operation, `UPDATE_FAILED`, update deadline, `DescribeUpdate`/`ListUpdates` for add-ons, update token, v1 report acceptance rule, sticky failure. | Yes: update response and status. |
| 5 | Durable `DELETING`, `DELETE_FAILED`, delete deadline, recreate blocked while deleting, v1 deletion-completion rule. | Yes: delete response persists and describe no longer returns not found at once. |
| 6 | `contracts/eks/v2`, v2 route and subject, v2 agent in a new node image, incarnation and generation fencing. | Yes, for v2 clusters only. |
| 7 | `preserve` as `relinquish` and create-time conflict detection for v2. | Yes, for v2 clusters only. |
| 8 | Tag actions on add-on ARNs. | Yes: `NotImplemented` becomes supported. |
| 9 | Finalisation: stop writing the manifest key and legacy fields after operator-visible finalisation. | None for current binaries; ends rollback. |

The ARN suffix (option B) and v1 retirement are separate later decisions, not slices of this correction.

## 15. Open questions for the reviewer

1. Should `CREATE_FAILED` and `UPDATE_FAILED` be sticky until a new generation (recommended), or lifted by a later `ready` as today?
2. ARN: keep the name-only ARN for now (option A, recommended) or adopt the per-incarnation suffix with this correction?
3. Which error does an update or delete issued during `CREATING`, `UPDATING` or `DELETING` return: `ResourceInUseException` (recommended) or `InvalidRequestException`?
4. Token reused with different parameters: `InvalidParameterException` (recommended) or `IdempotentParameterMismatch` to match `CreateCluster`, and should `CreateCluster` be revisited?
5. Deadline values for create, update and delete, and whether they are per bundle.
6. Is the v1 deletion-completion rule (second listing served after `DELETING`) acceptable, or should v1 deletions stay `DELETING` until the cluster moves to a v2 image?
7. Is best-effort acceptance of v1 reports (version match plus settle window) acceptable, or should same-version updates be rejected on v1 clusters?
8. Existing stored `configurationValues`: keep and flag (recommended), or reject further updates to such add-ons until cleared?
9. In-flight update when a delete is accepted: `Failed` or `Cancelled`?
10. Reject `serviceAccountRoleArn` for bundles without IRSA markers (recommended), or keep accepting it?
11. How many updates to retain inline per incarnation?
12. The add-on agent runs only on the primary control-plane server; who owns delivery after that server is replaced, and does a missing agent count as an unavailable guest?
13. Should a GPU node-group launch re-create `nvidia-device-plugin` after a user deleted it?
14. Is there an existing cluster-wide release gate for Q-75 finalisation, or does slice 0 need one?
15. Should `namespaceConfig` be rejected now (recommended) or left until the SDK model is updated?
