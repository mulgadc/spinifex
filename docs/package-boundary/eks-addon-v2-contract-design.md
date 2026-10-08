# EKS managed add-on: v2 guest contract design

**Status:** Draft for review; contract design only, nothing implemented.

This is the guest/controller wire contract that `docs/package-boundary/eks-addon-lifecycle-design.md` (Section 9, slice 6) calls `contracts/eks/v2`.
It applies that design's review decisions R1 to R16 and does not reopen them.
It defines payloads, routes, subjects, authorization binding, capability, executor identity and compatibility; the owner transitions, deadlines, record migration and agent implementation stay in the lifecycle design.
Present-state claims cite code read on this branch; `docs/package-boundary/eks-addon-lifecycle.md` stays authoritative for what the code does today.
Paths are relative to `spinifex/` unless they start with `scripts/`, `contracts/` or `tests/`, which are relative to the spinifex repo root.

## Standing

Q-75 (contract evolution) and Q-76 (asynchronous message semantics) in `docs/specs/requirements/resource-lifecycle.md` (mulga repo) are Decided requirements with human provenance and normative effect (R1); they are not ADRs.
Q-146 and C-82 (`docs/specs/requirements/managed-services.md`) are open, and this document closes neither.
The draft proposal PROP-SPINIFEXARCH-007 (`docs/development/proposals/spinifex-architecture/0007-contract-guest-and-persisted-state-evolution.md`, mulga repo) is unaccepted.
This design is written to be consistent with it (explicit majors, no payload-asserted authority, Prepare/Activate/Finalise, guest legacy mode), but nothing here depends on it being accepted, and nothing here cites it as a decision.
Where this design needs a mechanism that draft leaves to implementation (the release-membership record, the activation gate), it states the dependency in Section 3.

## 1. Purpose and non-goals

Purpose: give the add-on owner reports it can fence by executor, incarnation and generation, and observed proof of removal, without changing a byte of v1.

| Non-goal | Consequence |
|---|---|
| NATS or JetStream redesign | Reports stay on core NATS as notifications (Q-76); correctness rests on durable desired state, level-triggered re-sending and owner deadlines (lifecycle design Section 12). |
| Changing v1 | `contracts/eks/v1` route, subject, both JSON forms and Go field names stay byte-identical and readable, pinned by `contracts/eks/v1/addon_status_test.go`, `addon_manifest_test.go` and `v1_compat_test.go`. |
| Timing heuristics | No settle window, poll count, retry budget, credential expiry or deadline is used as a fence, capability proof or completion condition (R2, Q-75). |
| Universal framework | No generic versioning, capability-negotiation or guest-protocol library; every type here is EKS add-on specific (ADR-0003 S2 rules out a shared one without two consumers). |
| Lifecycle implementation | Owner state machine, deadlines, record migration, token idempotency and agent code are the lifecycle design's slices, not this contract. |
| Per-incarnation ARN | The external ARN stays name-only (R4); `incarnationId` is internal. |

## 2. Version and capability negotiation

### 2.1 Versions

| Item | Rule |
|---|---|
| Major | Every v2 body carries `"contractVersion": 2`. A receiver rejects any other value with reason `UnknownContractMajor` and changes no state (Q-75). |
| Separation | v2 has its own routes and subject (Section 7); no receiver sniffs payloads to tell v1 from v2. |
| Extensibility | Declared per body below: added optional fields may be ignored; an unknown value of a closed enum (`mode`, `phase`, `reason`, object `state`) is rejected for that entry (Q-75 "reject unknown required semantics or enum values"). |
| Producer rule | The host never emits a v2 directive to a caller that has not called the v2 route as the authenticated active executor; v1 callers get v1 only (Q-75 "oldest active compatible representation"). |

### 2.2 What N and N-1 mean here

| Party | N-1 | N |
|---|---|---|
| Host release (daemon, awsgw, add-on owner, cluster owner) | Today's release: v1 route and subject only. | Serves v1 unchanged plus the v2 routes and subject, the executor record and the v1 compatibility gate. |
| Guest image (control-plane VM agent) | v1 agent: `mulga-eks-addon-sync.sh` with `eks-gateway-fetch`/`eks-gateway-publish`. | v2 agent shipped in a new EKS node image. |

The host window is N and N-1 in both rollout orders.
The guest image is a long-lived consumer outside the Regional upgrade (a cluster keeps its image until its control-plane VM is replaced), so the v1 guest mode publishes its own support and retirement lifecycle as Q-75 requires; N+1 does not drop v1 merely because one release passed.
New EKS images are picked newest-AMI-wins (`handlers/eks/k3s_server_vm.go:353-363`, release inventory Section 5), so an N-1 host can launch a v2 image; Section 8 covers that row.

### 2.3 How capability is established

Capability is never read from a guest payload, image tag, user-data value or AMI name.

1. The caller authenticates with the control-plane instance role session that IMDS mints with `RoleSessionName` = instance ID (`handlers/sts/assume_role.go:136-148`); the role trusts only `ec2.amazonaws.com` (`handlers/iam/system_role.go:14`), and the gateway takes the role name from the underlying role ARN, never the session name (`gateway/eks.go:307-320`).
2. The gateway runs the existing class and membership checks (`gateway/eks/internal_authz.go:44-96`), then the executor check (Section 4.3).
3. The gateway relays the fetch to the daemon with the derived executor identity attached; the cluster owner re-checks it against the executor record and, in the same CAS, marks that epoch `addonContract: 2` if it is not already marked.
4. The add-on owner reads capability only through the narrow interface in Section 4.2.

A new epoch starts unmarked, so the compatibility gate re-closes on every executor change until the new executor fetches v2 (lifecycle design Section 9).
Marking on fetch rather than on the first accepted report is the lifecycle design's choice; open question 3 asks whether to require the report round trip too.

## 3. Prerequisites: what must exist before v2 activates

The release inventory found no release-membership record and no activation gate (`docs/package-boundary/release-and-version-inventory.md` Section 7).
v2 can ship in the Prepare stage without them, but the v2 routes answer `NotActivated` and the gate stays open (old semantics for everyone) until each item below exists.

| # | Prerequisite | Gap it closes | Why v2 needs it |
|---|---|---|---|
| P1 | An authoritative, cluster-visible record of the release each daemon and awsgw process runs | G1, G10 | Activation must prove no N-1 writer remains: N-1 `casUpdate` writers DROP added record fields and N-1 `Owner.Delete` ERASEs without observed removal (`eks-addon-v1-compatibility.md` "Record and manifest writers"). awsgw processes count, because an N-1 gateway serves v1 membership-only authorization and has no v2 route. |
| P2 | An operator-visible activation record for the add-on lifecycle change, written only from P1 evidence | G2 | The v2 routes and the v1 compatibility gate key on it. It is narrow and EKS-owned unless a platform gate exists first (lifecycle design open question 4). |
| P3 | Record expand complete: `schemaVersion`, `incarnationId`, `desired`, `observed` written beside legacy fields by an idempotent, resumable pass | G7, G8 | Directives carry `incarnationId` and `generation`; a record without them cannot be listed in v2 (Section 5.4). |
| P4 | The cluster-owned executor record and the bound v2 report route (Sections 4 and 7) | executor inventory gaps 2 to 4 | Without it no report can be bound to a caller, cluster or executor. |
| P5 | A documented controlled-rollback procedure for the activated state | G4 | An N-1 binary that returns after activation must not complete or reinterpret v2 obligations (lifecycle design open question 9). |

The per-account bucket stamp is not the fence: bumping `KVBucketEKSAccountVersion` (`handlers/eks/store.go:29`) makes every N-1 binary refuse the whole `eks-account-{id}` bucket with `SchemaAheadError` (`foundation/state/migrate/kv.go:55-67`), taking down every EKS action in the account, not just add-ons.
The per-record `schemaVersion` plus P2 replaces it.

## 4. Executor identity and epoch

### 4.1 Shape and owner

The cluster owner owns a new key `clusters/{cluster}/delivery-executor` in `eks-account-{account}`, beside `meta` rather than inside it.
A separate key keeps the epoch off the meta revision, which churns on every health write (`handlers/eks/cluster_state.go:416-436`), and away from N-1 `casUpdateMeta` writers, which would DROP an added meta field.
N-1 binaries ignore the key: cluster listing selects only keys ending `/meta` (`handlers/eks/service_impl.go:2422`), and `DeleteClusterPrefix` erases it with the cluster, which is correct.

```json
{
  "schemaVersion": 1,
  "instanceId": "i-0a1b2c3d4e5f60718",
  "epoch": 5,
  "assignedAt": "2026-10-09T10:00:00Z",
  "addonContract": 2,
  "addonContractProvenAt": "2026-10-09T10:00:31Z",
  "fence": {
    "previousInstanceId": "i-09f8e7d6c5b4a3921",
    "previousEpoch": 4,
    "evidence": "instance-terminated",
    "observedAt": "2026-10-09T09:59:58Z"
  }
}
```

| Field | Rule |
|---|---|
| `instanceId` | The internal EC2 instance ID of a current control-plane member; empty means no executor is assigned (an unavailable guest). |
| `epoch` | Unsigned, starts at 1, advanced by exactly one in the CAS that changes `instanceId`; never decreases while the key exists. |
| `addonContract` | `0` (unproven) or `2`; reset to `0` in the CAS that advances `epoch`. |
| `fence` | The evidence that let the epoch advance (Section 4.4); absent for epoch 1. |

### 4.2 Interface the add-on side declares

The add-on package declares, and the cluster owner implements, one read:

```go
// In domains/eks/addon.
type DeliveryExecutor struct {
    InstanceID    string
    Epoch         uint64
    AddonContract int // 0 or 2
}

type DeliveryExecutors interface {
    ActiveDeliveryExecutor(ctx context.Context, account, cluster string) (DeliveryExecutor, error)
}
```

The implementation carries the compile-time check (`var _ addon.DeliveryExecutors = (*Impl)(nil)`).
The add-on package does not read `ClusterMeta`, the executor key or the gateway caller (R6).
Assignment, reassignment and capability marking are cluster-owner methods the add-on side never calls.

### 4.3 Gateway derivation and rejection rule

The executor identity is `Caller.SessionName` after `requireCPAgent` passes (`gateway/eks/internal_authz.go:85-96`); nothing in the path, query or body names it.
Static-credential fallback VMs have no session name and can never be executors (`handlers/eks/k3s_server_role.go:30-50`).

| Order | Check | v2 reason | HTTP |
|---|---|---|---|
| 1 | SigV4, IAM policy, CP-agent class | existing gateway envelope (`AccessDenied`); these run before the v2 handler | 403 |
| 2 | Activation record present (P2) | `NotActivated` | 503 |
| 3 | Caller is a member of the named cluster in the named account (`IsControlPlaneMember`, `internal_authz.go:74`) | `NotClusterMember` | 403 |
| 4 | Executor record names an instance | `NoActiveExecutor` | 503 |
| 5 | `Caller.SessionName == executor.instanceId` | `NotActiveExecutor` | 403 |
| 6 | Report only: body `executorEpoch == executor.epoch` | `StaleExecutorEpoch` | 409 |
| 7 | Body decodes against v2 with `contractVersion == 2` | `UnknownContractMajor` or `MalformedReport` | 400 |

The daemon re-runs checks 4 to 6 against the record it reads, and the owner re-checks the epoch inside its CAS loop (lifecycle design Section 2 rule 1), because the epoch can advance between the gateway check and the write.

### 4.4 Fencing prerequisites and evidence options

The epoch may advance only after the previous executor provably cannot render, apply, or have reports accepted (R6).
Report acceptance is covered by checks 5 and 6 once the epoch advances; render and apply need evidence about the old VM.
The options below come from the executor inventory (`docs/package-boundary/eks-addon-executor-inventory.md` Sections 8 and 9); the reviewer chooses.

| Option | Evidence it requires | Exists today |
|---|---|---|
| A. Instance terminated or not found | EC2 instance record `terminated` or absent, proven authoritative under host partition; reassignment never triggered by a describe error (`cluster_reconciler.go:982-1007`) | Query exists; authority under host loss unverified; describe-error trigger must be excluded |
| B. Instance stopped and pinned stopped | `stopped` plus a durable "do not restart" marker the reconciler's restart ladder honours (`cluster_reconciler.go:708-771`) | No |
| C. Disable-on-handover directive | An online host-to-guest directive the agent polls, an acknowledgement, and a guest rule that a denied fetch stops rendering; an acknowledgement proves only that the old VM was alive to send it | No (only the boot-time recovery directive) |
| D. Credential-based | Per-session revocation, IMDS refusing to re-mint for the fenced instance, gateway rejecting revoked sessions; still leaves files already rendered on the old disk | No |
| E. Guest leader election (kube Lease) | Host-visible proof of the guest leader, verified K3s auto-deploy behaviour across servers; reports still need checks 5 and 6 | No |
| F. Operator assertion | An explicit, audited operator action naming the old instance, recorded as `fence.evidence: "operator-asserted"` | No |

**Recommendation (reviewer decides):** A as the only automatic evidence, F as the manual path when A cannot be obtained, and the guest stand-by rule of Section 6.4 as defence in depth that is never counted as evidence.
B to E each need new machinery and either prove less than A or prove only liveness.
Under A, the restore path's successor-first ordering (`restore_snapshot.go:262-336`) and the unreachable etcd prune (executor inventory gap 10) must be resolved before the restore path may advance an epoch.

## 5. Directive (fetch) payload v2

### 5.1 Body

Response to `GET /clusters/{clusterName}/internal-addons-v2/{accountId}` (Section 7), `Content-Type: application/json`:

```json
{
  "contractVersion": 2,
  "cluster": "prod",
  "executor": {"instanceId": "i-0a1b2c3d4e5f60718", "epoch": 5},
  "issuedAt": "2026-10-09T10:01:00Z",
  "directives": [
    {
      "addonName": "argocd",
      "incarnationId": "0b7e4f7a-2d55-4f0e-9a51-6f1fd1c0e2a4",
      "generation": 4,
      "mode": "remove",
      "spec": {"addonVersion": "3.0.23"},
      "deletion": {"requestedAt": "2026-10-09T09:58:00Z", "preserve": false},
      "deadline": "2026-10-09T10:28:00Z"
    },
    {
      "addonName": "aws-load-balancer-controller",
      "incarnationId": "6f1c2a4e-8b0d-4c3e-a1f2-93b7d4e5c6a8",
      "generation": 3,
      "mode": "install",
      "spec": {
        "addonVersion": "2.11.0",
        "serviceAccountRoleArn": "arn:aws:iam::123456789012:role/lbc-irsa"
      },
      "deadline": "2026-10-09T10:20:00Z"
    }
  ]
}
```

| Field | Type | Rule |
|---|---|---|
| `contractVersion` | int | Always 2. |
| `cluster` | string | The path cluster, echoed for logs. |
| `executor.instanceId`, `executor.epoch` | string, uint64 | From the executor record, never from the request; the guest echoes `epoch` in every report. |
| `issuedAt` | RFC 3339 UTC | Diagnostic only. |
| `directives` | array | One entry per add-on record of the cluster, sorted by `addonName`; `[]`, never `null`, when empty. |
| `addonName`, `incarnationId`, `generation` | string, UUID string, uint64 | Identity and the desired generation this entry represents. |
| `mode` | enum `install`, `remove`, `relinquish` | Closed set. |
| `spec.addonVersion` | string | Catalogue version; for `remove` and `relinquish`, the last installed spec, so a guest without local state can still render the object list. |
| `spec.serviceAccountRoleArn` | string, omitted when unset | An IAM role ARN; see 5.3. |
| `spec.configurationValues` | string, omitted | Never emitted until a bundle publishes a schema and a renderer (R9); a guest that receives it for a bundle without a renderer reports `failed`/`UnsupportedConfiguration`. |
| `deletion` | object, omitted unless `mode` is `remove` or `relinquish` | `requestedAt` and `preserve`, the obligation being discharged. |
| `deadline` | RFC 3339 UTC | The persisted operation deadline (R12), informational; the guest must not change what it reports, stop reporting or infer completion from it. The owner alone enforces it. |

Extensibility: the guest ignores unknown optional fields at any level; it rejects an unknown `mode` for that entry by reporting `failed`/`InvalidDirective`.

### 5.2 Rendering and the v1 quirk

v1's body is written by `WriteJSONResponse`, which calls the SDK REST-JSON marshaller `jsonutil.BuildJSON` (`gateway/eks/handler.go:19-22`); it ignores `json` tags, so the deployed v1 body uses Go field names (`{"Addons":[{"AddonName",...}]}`) and the guest matches it only through `encoding/json`'s case-insensitive decoding (`contracts/eks/v1/README.md`).
v2 avoids this by construction:

- The v2 route has its own writer that calls `encoding/json` (`json.Marshal`, default HTML escaping) and sets `application/json`; it never goes through `WriteJSONResponse`.
- v2 types carry only `json` tags, no `locationName`, and the literal-byte tests (Section 9) pin camelCase keys, so a regression to `BuildJSON` fails a test.
- Optional fields use `omitempty`; arrays that must be present (`directives`, `reports`, `objects`) are initialised non-nil.
- The guest decodes v2 strictly by key case (a Go decoder that rejects a case-mismatched key, for example by comparing a re-marshal), so the two forms can never be confused again.

### 5.3 Validation the host applies before listing

| Value | Rule | On violation |
|---|---|---|
| `serviceAccountRoleArn` | `arn:{partition}:iam::{12 digits}:role/{path}{name}` with IAM's role-name character set; already `iam:PassRole`-checked at the adapter (R10) and rejected for bundles with no IRSA marker (R14). | The AWS adapter returns `InvalidParameterException`, so it never reaches a record; a stored legacy value that fails is listed without the field and the owner records a health issue. |
| `addonVersion` | In the catalogue. | Same. |
| `configurationValues` | Never listed (R9). | Stored legacy values stay flagged on the record (lifecycle design Section 3). |

The guest re-validates the role ARN with the same pattern before rendering, because the v1 renderer substitutes it raw into `sed` and YAML (`eks-addon-bundle-inventory.md` X9); a failure is `failed`/`InvalidDirective`.

### 5.4 Records that cannot be listed

A record without `incarnationId` or `desired.generation` (expand not yet run, or rewritten by an N-1 writer before activation) is omitted from the v2 listing and counted in a metric; the add-on reconcile pass re-expands it.
After activation no N-1 writer exists (P1), so the omission is transient.

## 6. Report payload v2

### 6.1 Guest body

`POST /clusters/{clusterName}/internal-addons-v2/{accountId}/reports`, one batch per tick:

```json
{
  "contractVersion": 2,
  "executorEpoch": 5,
  "reports": [
    {
      "addonName": "aws-load-balancer-controller",
      "incarnationId": "6f1c2a4e-8b0d-4c3e-a1f2-93b7d4e5c6a8",
      "generation": 3,
      "phase": "degraded",
      "reason": "InsufficientReplicas",
      "message": "deployment kube-system/aws-load-balancer-controller available 0/1",
      "observedAt": "2026-10-09T10:05:00Z",
      "objects": [
        {"group": "apps", "kind": "Deployment", "namespace": "kube-system", "name": "aws-load-balancer-controller", "state": "present", "detail": "available=0/1"}
      ]
    },
    {
      "addonName": "argocd",
      "incarnationId": "0b7e4f7a-2d55-4f0e-9a51-6f1fd1c0e2a4",
      "generation": 4,
      "phase": "removing",
      "reason": "FinalizerBlocked",
      "message": "namespace argocd Terminating",
      "observedAt": "2026-10-09T10:05:00Z",
      "objects": [
        {"group": "", "kind": "Namespace", "namespace": "", "name": "argocd", "state": "terminating", "detail": "finalizers=kubernetes"},
        {"group": "apiextensions.k8s.io", "kind": "CustomResourceDefinition", "namespace": "", "name": "applications.argoproj.io", "state": "absent", "detail": ""}
      ]
    }
  ]
}
```

| Field | Rule |
|---|---|
| `executorEpoch` | The epoch from the last successful directive fetch; one per batch. |
| `reports[]` | At most one entry per `addonName`; a duplicate rejects the batch as `MalformedReport`. |
| `phase` | Closed enum, Section 6.2. |
| `reason` | Closed enum, Section 6.3; required for `failed`, `degraded` and `removing`, optional otherwise. |
| `message` | Free text, at most 1024 bytes after truncation by the guest. |
| `objects[]` | Per-object observations (Sections 6.6 and 6.7); required for `removing`, `removed` and `ready`, optional otherwise; bounded to the bundle's declared set. |
| `ts` | Not present; `observedAt` replaces v1's unused `ts`. |

The guest builds the body with a JSON encoder (a Go helper in the image, as `eks-gateway-publish` already is), never `printf`.
v1's `printf` (`scripts/images/eks-node/mulga-eks-addon-sync.sh:67-72`) does not escape `message`, so a quote or backslash in a message produces invalid JSON; v2 removes that class of defect rather than escaping in shell.

### 6.2 Phases

| Phase | Valid in mode | Meaning |
|---|---|---|
| `applied` | `install` | Rendered for this generation; readiness (Section 6.6) not yet met. Never a failure. |
| `ready` | `install` | Readiness definition for this bundle met for this generation. |
| `degraded` | `install` | Was ready for this generation and no longer meets the definition. |
| `failed` | `install` | A deterministic cause prevents realising this generation (`reason` required). |
| `removing` | `remove` | Some object of the removal set still exists. |
| `removed` | `remove` | Every object of the removal set observed absent. |
| `relinquished` | `relinquish` | The rendered file is gone and the agent no longer manages the add-on. |

`degraded` is an addition to the phase list in the lifecycle design Section 2: without it the guest must choose between `applied` (which cannot move `ACTIVE` to `DEGRADED`) and `failed` (which is terminal while `CREATING` or `UPDATING`, R3), and it cannot see the host status to choose.
With it, the owner maps `ACTIVE` plus `degraded` to `DEGRADED` and records it as a health issue without a transition in `CREATING` or `UPDATING`, where the deadline governs; open question 5.

### 6.3 Reason codes

Reasons are internal contract values, not AWS error codes (R7); the owner maps some onto `AddonIssue.code` (pinned SDK `AddonIssueCode*`, `aws-sdk-go` v1.55.8 `service/eks/api.go`) when it builds `health`.

| Reason | Phases | `AddonIssue.code` | Meaning |
|---|---|---|---|
| `BundleMissing` | `failed` | `InternalFailure` | No baked bundle for name and version in this image. |
| `InvalidDirective` | `failed` | `InternalFailure` | Unknown mode, invalid role ARN, or a field the guest cannot realise. |
| `UnsupportedConfiguration` | `failed` | `InternalFailure` | `configurationValues` present for a bundle without a renderer. |
| `RenderFailed` | `failed` | `InternalFailure` | Template or webhook certificate generation failed. |
| `ApplyRejected` | `failed`, `degraded` | `AdmissionRequestDenied` | The apiserver or an admission webhook rejected an object of the bundle. |
| `ObjectConflict` | `failed` | `ConfigurationConflict` | `resolveConflicts` `NONE`/`PRESERVE` and an object of the bundle exists without the add-on's ownership labels. |
| `ObjectMissing` | `degraded` | `K8sResourceNotFound` | An object of the readiness set is absent after it was applied. |
| `InsufficientReplicas` | `degraded` | `InsufficientNumberOfReplicas` | A workload of the readiness set is below its replica target. |
| `WebhookUnavailable` | `degraded` | `AdmissionRequestDenied` | A fail-closed webhook of the bundle has no ready endpoint. |
| `CredentialFailure` | `degraded` | `AccessDenied` | Only where a bundle's readiness definition observes it; none does yet. |
| `FinalizerBlocked` | `removing` | none | An object of the removal set is `Terminating` behind a finalizer. |
| `DeletePending` | `removing` | none | Delete issued; objects still present without a finalizer cause. |

No reason exists for an unreachable apiserver: the guest sends nothing for that add-on, as the lifecycle design specifies, and silence is handled by deadlines.
The gateway-side rejection reasons of Section 4.3 (`NotActivated`, `NotClusterMember`, `NoActiveExecutor`, `NotActiveExecutor`, `StaleExecutorEpoch`, `UnknownContractMajor`, `MalformedReport`) are response reasons, never report reasons.

### 6.4 Gateway response and guest stand-by rule

The v2 routes answer with `application/json` `{"contractVersion":2,"error":{"reason":"...","message":"..."}}`, rendered with `encoding/json`, instead of the AWS error envelope (R7); only check 1 of Section 4.3 keeps the gateway's existing envelope, because it runs before the route handler.
A successful report POST returns `{"contractVersion":2,"accepted":N}` where `accepted` counts entries relayed, not entries applied: acceptance by the owner is a separate CAS that the guest never learns of (Q-76: a transport acknowledgement never implies domain completion).

On `NotActiveExecutor`, `NotClusterMember` or `StaleExecutorEpoch`, the v2 agent stops delivery: it removes its rendered `spinifex-addon-*.yaml` files without `kubectl delete`, sends no reports and re-fetches on its next tick.
This complements, but never substitutes for, the fence evidence of Section 4.4.

### 6.5 Host-internal envelope and subject

The gateway does not relay the guest bytes verbatim, unlike v1's `PublishInternal` (`gateway/eks/internal_publish.go:47-91`).
It decodes the batch strictly, then publishes one host-internal envelope rendered with `encoding/json`:

```json
{
  "contractVersion": 2,
  "account": "123456789012",
  "cluster": "prod",
  "executor": {"instanceId": "i-0a1b2c3d4e5f60718", "epoch": 5},
  "receivedAt": "2026-10-09T10:05:01Z",
  "reports": [ ... the decoded entries, re-encoded ... ]
}
```

`executor.instanceId` comes from the authenticated caller and `executor.epoch` is the value check 6 verified; no guest field reaches `executor`.
Subject: `eks.addon.v2.{account}.{cluster}.status`, core NATS, no queue group, subscribed by the add-on reconcile pass under the cluster lease (lifecycle design Section 11).
The account and cluster tokens come from the route captures after check 3, so they are bound to a cluster the caller serves; v1's subject is built from the body `accountId` (`internal_publish.go:63-76`).

### 6.6 Readiness evidence per bundle

v1 reports `ready` when K3s records the `addon.k3s.cattle.io/gvks` annotation, which proves only that K3s applied some GVKs, says nothing about workloads, and may reflect the previous generation (`eks-addon-bundle-inventory.md` Section 3.3, X1 to X3).
v2 drops the annotation as a readiness source entirely and defines ready per bundle.

Generation binding: the v2 renderer stamps every rendered object with annotations `eks.spinifex.io/addon-incarnation` and `eks.spinifex.io/addon-generation` and labels `app.kubernetes.io/managed-by: spinifex-addon` and `eks.spinifex.io/addon-name`.
An object counts toward readiness only if it is present and carries the current incarnation and generation, so a report can never describe a previous apply; this also gives a stable ownership selector (X6).

Common rule: every object of the bundle's readiness set is read individually (no `--ignore-not-found` aggregation); a missing one is `ObjectMissing`.

| Bundle | Ready when, for the current generation |
|---|---|
| `spinifex-noop` | Namespace `spinifex-noop` and ConfigMap `spinifex-noop/spinifex-noop` present. |
| `argocd` | Namespace `argocd` present; CRDs `applications`, `applicationsets`, `appprojects.argoproj.io` `Established`; all 6 Deployments `observedGeneration >= generation`, `updatedReplicas == availableReplicas == spec.replicas`; StatefulSet `argocd-application-controller` `updatedReplicas == readyReplicas == spec.replicas`. |
| `aws-ebs-csi-driver` | All rendered objects present; Deployment `ebs-csi-controller` available equals `spec.replicas` (2); DaemonSet `ebs-csi-node` `numberReady == updatedNumberScheduled == desiredNumberScheduled`, where `desiredNumberScheduled == 0` is ready with a `message` saying no eligible node. |
| `aws-load-balancer-controller` | CRDs `ingressclassparams` and `targetgroupbindings.elbv2.k8s.aws` `Established`; Deployment available equals `spec.replicas`; Service `aws-load-balancer-webhook-service` has at least one ready endpoint (EndpointSlice), because the bundle's six webhooks are `failurePolicy: Fail`; both webhook configurations present. |
| `nvidia-device-plugin` | DaemonSet `nvidia-device-plugin-daemonset` `numberReady == updatedNumberScheduled == desiredNumberScheduled`; `desiredNumberScheduled == 0` is ready with a no-GPU-node message. |

Not part of any ready definition: IRSA credential success, GPU allocatable exposure and data-path checks; open question 7.

### 6.7 Removal evidence per bundle

`removed` means every object of the bundle's removal set is observed NotFound by an individual `get` for each object (namespaced and cluster-scoped), not by listing a namespace.
The removal set is the rendered object list of the last installed spec, minus objects shared with another add-on that is still listed (below).
`removing` reports carry `objects[]` with `present`, `terminating` or `absent` for every member of the set, so `DescribeAddon` health can show what blocks.

| Bundle | Removal set (all NotFound) | Blockers reported as `FinalizerBlocked` | Residue outside the set |
|---|---|---|---|
| `spinifex-noop` | Namespace, ConfigMap | Namespace `kubernetes` finalizer (aggregated API availability) | none |
| `argocd` | Namespace `argocd`, 3 CRDs, 3 ClusterRoles, 3 ClusterRoleBindings, and the namespaced objects | `Application` `resources-finalizer.argocd.argoproj.io`; CRD and namespace `Terminating` | user workloads Argo CD deployed |
| `aws-ebs-csi-driver` | 20 rendered objects, `kube-system` or cluster-scoped | PV and VolumeAttachment CSI finalizers | PVs, VolumeAttachments, Spinifex EBS volumes |
| `aws-load-balancer-controller` | 2 CRDs plus 13 rendered objects, webhook configurations included | Ingress, Service and TargetGroupBinding controller finalizers; CRD `Terminating` | Spinifex ELBv2 load balancers, target groups, security groups |
| `nvidia-device-plugin` | 1 DaemonSet | none | host CDI files |

Residue is reported in the final `removed` report's `objects[]` with `state: "residue"` where the guest can see it (PVs with `spec.csi.driver == ebs.csi.aws.com`); out-of-cluster residue is invisible to the guest.
Whether `removed` must wait for in-cluster residue is a lifecycle decision (open question 8); this contract only carries the observations.

Shared `kube-system/spinifex-gateway-ca` Secret (bundle inventory X5): both IRSA bundles render it under one name, so either removal deletes the other's dependency and a NotFound check on it never completes while the other re-creates it.
The contract rule: an object rendered by more than one listed add-on is excluded from every removal set while more than one `install` directive renders it, and the guest deletes it only with the last one.
**Recommendation (reviewer decides):** additionally rename it per bundle (`spinifex-gateway-ca-<addon>`) in the v2 image so the overlap disappears for new installs, keeping the shared-object rule for clusters migrating from v1 renders; open question 9.

## 7. Routes, subjects and authorization binding

| Direction | v1 (unchanged) | v2 |
|---|---|---|
| Fetch route | `GET /clusters/{clusterName}/internal-addons/{accountId}`, action `ListInternalAddons` (`gateway/eks.go:55-57`) | `GET /clusters/{clusterName}/internal-addons-v2/{accountId}`, action `ListInternalAddonDirectives` |
| Report route | `POST /clusters/{clusterName}/internal-publish`, channel `addon`, action `PublishInternal` (`gateway/eks.go:39-41`) | `POST /clusters/{clusterName}/internal-addons-v2/{accountId}/reports`, action `ReportInternalAddons` |
| Gateway to daemon | NATS `eks.ListStagedAddonManifests` | NATS `eks.ListAddonDirectives`, request carries the gateway-derived executor identity |
| Report subject | `eks.addon.{account}.{cluster}.status` | `eks.addon.v2.{account}.{cluster}.status` |
| Response renderer | `jsonutil.BuildJSON` | `encoding/json` |
| Scope | `ListInternalAddons` is `sourceInternalCluster`; `PublishInternal` is `sourceBodyAccountCluster` (`gateway/eks/authz.go:55,59`) | Both `sourceInternalCluster`, so `IsInternalAction` is true and `AuthorizeInternal` runs (`internal_authz.go:37-39`) |
| IAM | Role policy `Resource: "*"` (`handlers/eks/k3s_server_role.go:27`) | The two new actions added to the same inline policy; the binding comes from `AuthorizeInternal` plus Section 4.3, not from the policy resource |

Binding finding carried forward: today `PublishInternal` is not gated by `AuthorizeInternal`, so with the role's `Resource: "*"` any control-plane session of any cluster can publish add-on reports for any account and cluster, and the relay attaches no caller identity (executor inventory gaps 2 and 3).
v2 does not reuse `PublishInternal`; its report route takes the cluster and account from the path, requires membership and the active executor, and attaches identity.
Binding v1's `PublishInternal` to the caller's cluster is still worth doing as an authorization-only change with unchanged bytes, but v2 does not depend on it, because post-activation v1 reports are diagnostic-only (lifecycle design Section 9).

After an executor is assigned for a cluster (P4), the N gateway also applies check 5 to the v1 fetch route for that cluster, so a v2 agent on a non-executor member cannot fall back to v1 and become a second renderer; v1 bytes are unchanged and the change is authorization-only (open question 4).

## 8. Compatibility matrix

"Pre-activation" means P2 has not been recorded; "post-activation" means it has, which by P1 implies no N-1 host process remains.

| Guest | Host | Fetch | Reports | Lifecycle mutations | Conformance |
|---|---|---|---|---|---|
| v1 | N-1 | v1 | v1, applied as today | Today's semantics | None claimed |
| v1 | N, pre-activation | v1, byte-identical | v1, applied as today | Today's semantics | None claimed |
| v1 | N, post-activation | v1 projection: records in `install` mode only | v1, recorded diagnostically in `observed`, never complete an operation | Gated (below) | None claimed |
| v2 | N-1 | v2 answered `InvalidAction` (`gateway/eks.go:196-199`); agent uses legacy v1 mode that tick | v1 | Today's semantics | None claimed |
| v2 | N, pre-activation | v2 answered `NotActivated`; legacy v1 mode that tick | v1 | Today's semantics | None claimed |
| v2, active executor | N, post-activation | v2; epoch marked `addonContract: 2` | v2, fenced | Ungated once marked | Q-146 claimed for this cluster only |
| v2, not executor | N, post-activation | `NotActiveExecutor`; stand-by (Section 6.4); v1 fetch also denied | none | n/a | n/a |

Legacy-mode fallback is permitted only on `InvalidAction` from the fetch route or `NotActivated`; any other v2 rejection forbids it, because falling back on `NotActiveExecutor` or `StaleExecutorEpoch` would bypass the fence.
This is a guest protocol fallback for an N-1 or not-yet-activated host, not a lifecycle fallback, and it never opens the gate; open question 2 asks the reviewer to confirm it does not conflict with R5.

### 8.1 Gated mutations

Per R5 and the lifecycle design Section 9, on a cluster whose current epoch is not marked `addonContract: 2`:

| Action | Gated | Reason |
|---|---|---|
| `CreateAddon` (including GPU auto-staging through the owner) | Yes | `ACTIVE` needs a fenced report for the current incarnation and generation. |
| `UpdateAddon` | Yes | Same. |
| `DeleteAddon`, with or without `preserve` | Yes | Needs observed removal or relinquish. |
| `DescribeAddon`, `ListAddons`, `DescribeUpdate`, `ListUpdates`, `DescribeAddonVersions`, `DescribeAddonConfiguration`, tag actions, cluster-teardown removal | No | No guest realisation required. |

Response: `InvalidRequestException`, HTTP 400, message (proposed exact text):

- epoch unmarked: `Add-on lifecycle operations on cluster {cluster} require an upgraded control-plane image. The cluster's add-on delivery agent has not established add-on contract v2. Upgrade the cluster's control-plane image and retry.`
- no executor assigned: `Add-on lifecycle operations on cluster {cluster} are unavailable: the cluster has no active add-on delivery agent. Retry after the control plane recovers.`

Pre-activation, nothing is gated; the gate exists only once P2 is recorded (lifecycle design slice 7).

### 8.2 What old binaries do with new records

From `docs/package-boundary/eks-addon-v1-compatibility.md`:

| N-1 path | Effect on a v2 record | Why it forces ordering |
|---|---|---|
| `Owner.Update`, `Owner.ApplyReport` with a change, `markFailed` via `casUpdate` (`domains/eks/addon/store.go:198-219`) | DROP: re-encodes v1 `Record` and erases `incarnationId`, `desired`, `observed` | Expand must precede activation, and v2 semantics stay off while any N-1 writer can serve the cluster (P1, P2). |
| `StagingInstaller.Install` | OVERWRITE the manifest from four v1 fields | The manifest key is a projection only until finalisation (R16). |
| `Owner.Delete`, `DeleteClusterPrefix` | ERASE regardless of content | An N-1 delete would complete a v2 delete without observed removal; only activation prevents it. |
| Readers (`Get`, `List`, `ListManifests`) | IGNORE added fields; FAIL the whole listing on a type change (`store.go:78,108`) | No existing field may change JSON type. |
| Reconciler on the v1 subject | IGNORE added report fields; never subscribes to the v2 subject | v2 reports reach only an N lease holder; they are notifications, so a drop is repaired next tick. |
| Bucket stamp | Bumping it makes N-1 refuse the whole account bucket | Use per-record `schemaVersion`, not the stamp. |

Type-change hazard in the lifecycle design's record sketch: today `health` is a string (`domains/eks/addon/record.go:30`), and the sketch shows `health` as an array of issues.
Writing that would make every N-1 `ListAddons` and `DescribeAddon` on the cluster fail to decode; the issue list needs a new field (for example `healthIssues`) with `health` kept as a string (open question 10).

## 9. Compatibility tests the v2 contract needs

| # | Test | Pins |
|---|---|---|
| T1 | Literal-byte encode of a directive response and of a report batch, including empty `directives`, empty `objects`, omitted optional fields and HTML-escaped characters | camelCase keys, field order, `[]` not `null`, no `BuildJSON` |
| T2 | Literal-byte decode of the same bytes into the v2 types, both directions (host encode to guest decode, guest encode to host decode) | Round trip without loss |
| T3 | Unknown optional fields at top level, per directive, per report and per object are ignored | Declared extensibility |
| T4 | Unknown `mode`, `phase`, `reason`, object `state`, and `contractVersion` 1 or 3 are rejected with the named reason and change no state | Q-75 closed enums and majors |
| T5 | Case-mismatched keys (`AddonName`, `Directives`) are rejected by the guest decoder | The v1 quirk cannot recur |
| T6 | Directives sorted by `addonName`; duplicate `addonName` in a report batch rejects the batch | Ordering and uniqueness |
| T7 | A `message` containing quote, backslash, newline and non-ASCII round-trips through the guest encoder and the gateway envelope | No `printf` JSON |
| T8 | The gateway envelope's `executor.instanceId` equals the authenticated session name even when the guest body carries `executor`, `instanceId` or `account` fields | No payload-asserted identity |
| T9 | Each rejection row of Section 4.3, one test per row, including a member of another cluster and a non-executor member | Authorization binding |
| T10 | A v1 body, v1 subject and v1 report remain byte-identical after v2 is added; the existing `contracts/eks/v1` tests run unchanged | v1 frozen |
| T11 | A v2 report on the v1 subject and a v1 report on the v2 subject are both rejected or ignored without state change | No cross-major interpretation |
| T12 | Guest fallback: `InvalidAction` and `NotActivated` select v1 mode for one tick; every other reason does not | Fallback cannot bypass the fence |
| T13 | An N-1 decoder (`contracts/eks/v1` types) reading a v2 directive or report body fails or ignores safely, never yields a populated v1 value that would be applied | Mis-routing safety |
| T14 | Readiness and removal fixtures per bundle: objects with a previous generation annotation never produce `ready`; a shared Secret is excluded while another add-on renders it | Section 6.6 and 6.7 |

## 10. Open questions for the reviewer

1. Route and subject names: `internal-addons-v2` and `eks.addon.v2.{account}.{cluster}.status` as proposed, or another spelling?
2. Guest legacy-mode fallback on `InvalidAction` or `NotActivated` (Section 8): accept as a guest protocol fallback that never opens the gate, or forbid it, in which case a v2 image launched by an N-1 host delivers nothing until the host upgrades (and image import must then be ordered after host activation)?
3. Mark the epoch v2-capable on the first authenticated v2 fetch (lifecycle design) or only after the first accepted v2 report as well?
4. Apply the active-executor check to the v1 fetch route once an executor is assigned (authorization-only change, bytes unchanged)?
5. Add the `degraded` phase (Section 6.2), or encode health loss another way?
6. Reason codes are a closed enum under Q-75: is adding one a new major, or should the directive response advertise the host's accepted reason set so a newer guest can stay within it?
7. Should readiness for the IRSA bundles include a credential check, and for `nvidia-device-plugin` a GPU allocatable check, or stay as defined in Section 6.6?
8. Does `removed` wait for in-cluster residue (PVs, finalizer-held Ingresses and Services), or is residue reported and the record deleted once the removal set is gone?
9. Shared `spinifex-gateway-ca`: shared-object rule only, or also rename per bundle in the v2 image (recommended)?
10. Record `health` type: keep it a string and add `healthIssues` (recommended), given the lifecycle design's sketch would break N-1 readers?
11. Fence evidence: option A plus F (recommended), or another combination from Section 4.4?
12. Report batching: one batch per tick (proposed) or one POST per add-on as in v1?
13. Should the directive carry `deadline` at all, given the guest must ignore it for behaviour, or only in diagnostics?
14. v1 guest support lifecycle: what retirement notice and detection (no cluster fetching the v1 route in the Region) is published, and from which release is it counted?
