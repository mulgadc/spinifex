# EKS add-on v1 compatibility: who would drop, erase or ignore added fields

Code read at 3a55e2ee1; not verified live.

Question: if a newer binary adds JSON fields (for example `incarnationId`, `generation`, `observedGeneration`, `origin`, `executorEpoch`) to the add-on record `clusters/{c}/addons/{a}`, the staged manifest `clusters/{c}/addons/{a}/manifest`, or a `contracts/eks/v1` payload, what does an older binary still running the code below do with them?

Verdicts:

- **DROP**: decodes into a Go struct and writes the re-encoded struct back, erasing undeclared fields (`encoding/json` keeps no unknown-field bag).
- **OVERWRITE**: writes a freshly built value with no read of the stored bytes; whatever was stored is replaced.
- **ERASE**: deletes the key; content is irrelevant.
- **IGNORE**: read-only decode; unknown fields are skipped and nothing is written back.
- **FAIL**: decode error when a known field's JSON type changes (not an added field).

## Version fences that exist today

| Fence | Where | Effect |
|---|---|---|
| Record schema/version field | none: `spinifex/domains/eks/addon/record.go:24` | No per-record version, so an old binary cannot tell a newer record from a v1 one. |
| Manifest schema/version field | none: `spinifex/domains/eks/addon/record.go:40` | Same for the manifest. |
| Per-account bucket schema stamp | `spinifex/handlers/eks/store.go:29` (`KVBucketEKSAccountVersion = 1`), checked at `store.go:203` | Bumping it makes every older binary refuse the whole `eks-account-{id}` bucket with `SchemaAheadError` (`spinifex/foundation/state/migrate/kv.go:87`), i.e. all EKS calls for that account, not just add-ons. |
| Registered migrations for the bucket | none | `RegisterKV` is keyed by exact bucket name (`migrate/kv.go:48`, lookup at `kv.go:94`) while EKS buckets are named per account (`store.go:146`). |
| Wire version | `contracts/eks/v1/addon_status.go:1-3` package doc | Says generation-aware delivery needs a successor version; nothing on the wire carries a version marker. |

## Record and manifest writers

| Writer | file:line | Record | Manifest |
|---|---|---|---|
| `Owner.Create` (via `CreateAddon` `handlers/eks/addons.go:84`) | `domains/eks/addon/owner.go:48`; `Get` then unconditional `put` `store.go:39,51` | OVERWRITE (the Get/Put pair is not `kv.Create`, so a record created between them is replaced wholesale) | OVERWRITE via `StagingInstaller.Install` `delivery.go:39` → `putManifest` `store.go:153` (built from the four v1 fields) |
| `Owner.EnsureGPUDevicePlugin` (via `nodegroup.go:411`) | `owner.go:79` | IGNORE when a record exists (`ErrExists` swallowed); else as Create | as Create |
| `Owner.Update` (via `UpdateAddon` `addons.go:181`) | `owner.go:94` → `casUpdate` `store.go:198` (Unmarshal `:209`, Marshal `:215`, `kv.Update` `:219`) | DROP | OVERWRITE via Install `owner.go:113` → `putManifest` |
| `Owner.ApplyReport` (via reconciler `handlers/eks/addon_status_reconciler.go:23`) | `owner.go:140` → `casUpdate` | DROP when status or health changes; IGNORE when the report changes nothing (mutate returns false, no write) | untouched |
| `Owner.markFailed` (staging failure in Create/Update) | `owner.go:171` → `casUpdate` | DROP | untouched |
| `Owner.Delete` (via `DeleteAddon` `addons.go:211`) | `owner.go:122` → Uninstall `delivery.go:51`, `deleteRecord` `store.go:181,189` | ERASE | ERASE (`deleteManifest` `store.go:172`) |
| `DeleteClusterPrefix` (DeleteCluster `service_impl.go:1153`, DELETING reaper `eks_deleting_reaper.go:141`, both via `purgeClusterInfra` `service_impl.go:1451`) | `handlers/eks/cluster_state.go:493` | ERASE (every key under `clusters/{c}/`) | ERASE |
| `purgeClusterInfra(deleteMeta=false)` (`service_impl.go:712,1251`) | `service_impl.go:1450` | not touched | not touched |
| Account teardown | `spinifex/accountteardown/reapers_eks.go:103` | ERASE, only indirectly through `DeleteCluster`; no add-on reaper and no EKS bucket delete | ERASE (same path) |
| `TagResource` / `UntagResource` on an add-on ARN | `service_impl.go:1772,1812,1855` | not a writer: add-on ARNs return `NotImplemented`; create-time tags live only in `Record.Tags` | n/a |

## Record and manifest readers

| Reader | file:line | Verdict |
|---|---|---|
| `addon.Get` (DescribeAddon `addons.go:152`, Create existence check, Delete) | `store.go:58,70` | IGNORE; FAIL on a type change |
| `addon.List` (ListAddons `addons.go:26`) | `store.go:78,108` | IGNORE; one undecodable record fails the whole listing |
| `addon.ListManifests` (`ListStagedAddonManifests` `addons.go:125`) | `store.go:119,141` | IGNORE; one undecodable manifest fails delivery for every add-on in the cluster |
| `addonRecordToAWS` | `addons.go:222` | IGNORE (projects v1 fields onto the SDK shape) |
| `ListStagedAddonManifests` reply | `addons.go:129-137` | Projects the four v1 fields field-by-field, so unknown manifest fields never leave the daemon even if `addon.Manifest` gained them |

## contracts/eks/v1 consumers

| Consumer | file:line | Mechanism | Added field in a newer payload |
|---|---|---|---|
| Gateway NATS client for `eks.ListStagedAddonManifests` | `handlers/eks/service_nats.go:128`; `foundation/messaging/nats/nats.go:316` | `encoding/json` into `ListStagedAddonManifestsOutput` | DROP on relay: an older gateway re-renders only the fields its `StagedAddonManifest` declares |
| Gateway `ListInternalAddons` response | `gateway/eks/internal_addons.go:20,32`; route `gateway/eks.go:55`; `gateway/eks/handler.go:21-22` | `jsonutil.BuildJSON` (Go field names, json tags ignored) | Emits whatever fields its build declares; nothing to ignore |
| `eks-gateway-fetch -resource addons` | `agents/eks/gatewayfetch/fetch.go:115-123` | `encoding/json` (case-insensitive) then fixed four-column printf | IGNORE: extra top-level or per-add-on fields never reach the TSV |
| `mulga-eks-addon-sync.sh` sync loop | `scripts/images/eks-node/mulga-eks-addon-sync.sh:357` | `IFS=tab read -r _addon _version _role _cfg_b64` | A fifth TSV column would land inside `_cfg_b64` (last variable takes the rest), which is unused today (`:359`); inserting a column before column 4 would shift positions. Fetch binary and script ship in the same image, so they move together. |
| `mulga-eks-addon-sync.sh` report | `mulga-eks-addon-sync.sh:67-72` | printf of five fixed keys | N/A for added fields: an older guest never emits them |
| Gateway `PublishInternal` relay | `gateway/eks/internal_publish.go:30,82-85` | `Payload json.RawMessage` published verbatim | Passes added report fields through untouched |
| Reconciler report subscriber | `handlers/eks/cluster_reconciler.go:449-455`; `state_report.go:69-72`; `addon_status_reconciler.go:22-27` | `encoding/json` into `AddonStatusReport`, then only `Addon`, `Phase`, `Message` reach the owner | IGNORE: a report carrying `incarnationId`/`generation` is applied exactly as today, so an older reconciler cannot reject a stale incarnation's report |
| Phase / status enums | `owner.go:186-204` | `nextStatus` default branch | An unknown phase changes nothing; an unknown stored status decodes, and a `failed` report moves it to `CREATE_FAILED` |

## Consequences for an expand phase

- Every path that writes the record back (Update, ApplyReport with a change, markFailed) DROPs added fields, so while any older binary can serve an add-on write, added record fields are best-effort and can silently revert to absent.
- Every Install rebuilds the manifest from the record, so an older binary's Create/Update erases added manifest fields too.
- Delete and cluster deletion ERASE regardless of content, so a newer `origin`/`declined` marker would not stop an older binary deleting the record.
- Reads and all wire consumers tolerate added fields; changing the JSON type of an existing field instead fails whole listings on older binaries.
- The only hard fence is the per-account bucket stamp, which is all-or-nothing for EKS in that account.

## Characterisation tests pinning the above

| Test | file |
|---|---|
| `TestOwnerWrites_DropUnknownRecordAndManifestFields` | `spinifex/domains/eks/addon/v1_compat_test.go` |
| `TestOwnerApplyReport_NoOpKeepsUnknownFields` | `spinifex/domains/eks/addon/v1_compat_test.go` |
| `TestOwnerDelete_ErasesRecordAndManifestWithUnknownFields` | `spinifex/domains/eks/addon/v1_compat_test.go` |
| `TestReads_IgnoreUnknownFieldsWithoutRewriting` | `spinifex/domains/eks/addon/v1_compat_test.go` |
| `TestList_OneUndecodableEntryFailsTheListing` | `spinifex/domains/eks/addon/v1_compat_test.go` |
| `TestAddonStatusReport_IgnoresUnknownFields` | `contracts/eks/v1/v1_compat_test.go` |
| `TestInternalAddonsResponse_IgnoresUnknownFields` | `contracts/eks/v1/v1_compat_test.go` |
| `TestEmitAddonsTSV_IgnoresUnknownFields` | `spinifex/agents/eks/gatewayfetch/v1_compat_test.go` |
| `TestAddonStatusReport_UnknownFieldsAreAppliedAsToday` | `spinifex/handlers/eks/addon_v1_compat_test.go` |
| `TestListStagedAddonManifests_ProjectsOnlyV1Fields` | `spinifex/handlers/eks/addon_v1_compat_test.go` |

Already covered elsewhere: cluster-prefix sweep of add-on keys (`handlers/eks/addon_lifecycle_test.go:306`), hand-written v1 record decode (`addon_lifecycle_test.go:85`), unknown phase is a no-op (`domains/eks/addon/owner_test.go:49`).
