# ADR-0004 source inventory — EKS

Code-read inventory of `refactor/spinifex-package-boundaries` at `6c50349bd`.
It is not verified against a live system, and the branch is 59 commits behind `dev` at the time of writing, so recheck any file it cites before acting on it.
Items marked "verify" or "uncertain" are unconfirmed.
Gaps record observed behaviour, not target behaviour.

Add-on update: every add-on row, the add-on boundary edges (Section 7.1) and the add-on gaps were re-read from the code at `18794e017`, after the add-on owner and guest wire contract were extracted.
That update is a code read only; it is not verified against a live system.
Everything else still describes `6c50349bd`, and line numbers outside the add-on rows may have drifted.
The add-on lifecycle contract and its gap list are in `docs/package-boundary/eks-addon-lifecycle.md`; the proposed lifecycle correction is in `docs/package-boundary/eks-addon-lifecycle-design.md`.


Repo: `spinifex` submodule, branch `refactor/spinifex-package-boundaries`; all paths are relative to `spinifex/spinifex/` unless stated.
Binding inputs: ADR-0001, ADR-0002, ADR-0003, ADR-0004 and Q-78 as on `origin/main` of mulga.
Proposed resource vocabulary (ADR-0004 S1): `cluster`, `nodegroup`, `addon`, `access`.
Line numbers are from the working tree at time of writing; "uncertain" marks inferences not verified against a test or live system.

## 1. File table

Production `.go` files only; `_test.go` excluded.
Line counts are `wc -l`.

### 1.1 `handlers/eks/` (single package `handlers_eks`, 37 files, 12,310 lines at `6c50349bd`)

The add-on files `addon_catalog.go`, `addon_installer.go` and `addon_state.go` have left this package; Section 1.1a lists where they went.
The add-on rows below are at `18794e017`.

| File | Lines | Class | Reason |
|---|---|---|---|
| `access_entry.go` | 255 | resource owner (`access`) | AccessEntry record CRUD/CAS, supported access-policy catalogue, scope validation, ARN/record projection. |
| `addon_status_reconciler.go` | 32 | temporary residue (report relay hosted on `cluster`) | `applyAddonStatusReport` (`:22`) converts a `contracts/eks/v1` `AddonStatusReport` into `addon.Report` and calls the reconciler-declared `addonReports` capability (`:13-15`, bound to `*addon.Owner` at `:17`); `report.Version` and `report.TS` are dropped. Removal: add-on delivery observation gets its own host (Section 7.1, edge E3). |
| `addons.go` | 287 | AWS protocol adapter (`addon`) + internal guest route | Six AWS add-on actions validate against `addon.Lookup`, check cluster existence (`acctKVForCluster`), call `*addon.Owner` and project `addon.Record` to SDK shapes (`addonRecordToAWS` `:222`); `UpdateAddon` builds the `Successful` update with `Id = rec.Arn` (`:192-197`). Internal `ListStagedAddonManifests` (`:117`) copies `addon.Manifest` into `eksv1.StagedAddonManifest`. `addons()` (`:265`) builds a new `Owner` per call; `addonBucket` (`:278`) opens the account bucket for the staging installer. |
| `awsgw_endpoint.go` | 42 | resource owner (`cluster`) | Builds the per-cluster OIDC issuer URL. |
| `claim.go` | 31 | resource owner (`nodegroup`) | CAS-create helper; only caller is `nodegroup.go:123`. |
| `clienttoken.go` | 57 | resource owner (`cluster`) | CreateCluster `clientRequestToken` store (`spinifex-eks-clustertokens`). |
| `cluster_reconciler.go` | 1209 | resource owner (`cluster`), mixed | Cluster readiness/health, CP restart, member replacement, etcd reset; also hosts the add-on status subscription for `addon` (`addonReports` field `:171-176`, `WithAddonStatusSource` `:264-269`, plain subscribe `:447-458`), wired in `service_impl.go:2160-2163` (add-on rows at `18794e017`). |
| `cluster_state.go` | 515 | resource owner (`cluster`) | `ClusterMeta` record, status enum, CAS helpers, delete-reap bookkeeping, `DeleteClusterPrefix` (`:493`, which also erases nodegroup, access and add-on record and manifest keys by prefix without calling the add-on owner). |
| `cp_vpc.go` | 128 | resource owner (`cluster`) | EKS-specific managed CP VPC realization via `domains/network/systemvpc`. |
| `eks_billable_reaper.go` | 133 | resource owner (`cluster`) | Node-local GC backstop for CP VMs whose meta is gone; cites `ADR-0003:S2` as the general lifecycle envelope, while the resource's own contract remains incomplete. |
| `eks_deleting_reaper.go` | 159 | resource owner (`cluster`) | Re-drives DELETING clusters with backoff and exhaustion. |
| `igw.go` | 32 | resource owner (`cluster`) | Legacy-topology IGW ensure/delete via `systemvpc`. |
| `k3s_ha_control_plane.go` | 628 | resource owner (`cluster`), mixed | CP placement, spread, replacement, fresh CP; also contains `natsHostScheduler` (`:461-628`), a NATS fan-out over `contracts/cluster/v1` that is a capability implementation and should move to the composition root or the scheduler owner. |
| `k3s_server_role.go` | 51 | resource owner (`cluster`) | Ensures the system CP instance profile through `handlers_iam.EnsureSystemInstanceProfile` (identity dependency). |
| `k3s_server_vm.go` | 700 | resource owner (`cluster`) + guest wire contract | CP VM/ENI launch/terminate and AMI lookup; `buildK3sUserData` (`:524+`) is the CP guest boot contract; `lookupEKSGPUNodeAMI` is used by `nodegroup`. |
| `nats_bootstrap.go` | 347 | resource owner (`cluster`) + guest wire contract | Bootstrap subject vocabulary and envelope (`:47-57`) are a guest contract; persistence of token/kubeconfig/JWKS/CA is cluster state. |
| `nlb.go` | 591 | resource owner (`cluster`) | EKS-specific NLB realization via an ELBv2-shaped capability; also `ReapLBCLoadBalancers` (`:517`) for LBC-created ALBs in the customer VPC. |
| `nodegroup.go` | 1409 | resource owner (`nodegroup`), mixed | Nodegroup CRUD, scaling CAS, worker launch/terminate; also creates customer IAM roles/instance profiles (`:768-838`), stages a GPU add-on (`stageGPUDeviceAddon` `:405-416`, called from `:473`, through `addons().EnsureGPUDevicePlugin`; at `18794e017`), and decrypts the cluster join token (`:1160`, reads `cluster` secrets). |
| `nodegroup_userdata.go` | 171 | resource owner (`nodegroup`) + guest wire contract | Worker cloud-init (K3S_TOKEN, registry mirrors) is the worker guest boot contract. |
| `oidc_keypair.go` | 202 | resource owner (`cluster`) | Per-cluster OIDC signing key (encrypted) and JWKS. |
| `private_endpoint.go` | 88 | resource owner (`cluster`) | Customer-VPC private-endpoint ENI. |
| `reconciler_registry.go` | 183 | resource owner (`cluster`) | In-process registry of per-cluster reconciler goroutines. |
| `recovery_directive.go` | 144 | resource owner (`cluster`) + guest wire contract | Per-member etcd recovery directive; `RecoveryDirective` is read by the on-VM recovery agent. |
| `restore_snapshot.go` | 355 | resource owner (`cluster`) | Operator-only total-loss DR path; reads etcd snapshots through `providers/objectstore`. |
| `security_groups.go` | 505 | resource owner (`cluster`), mixed | Cluster SG realization in system and customer VPCs; `EnsureNodegroupSGRules` (`:351`) is nodegroup realization. |
| `service.go` | 73 | temporary residue | `EKSService`, one method per AWS action plus internal methods; it is the NATS RPC surface between gateway and daemon and is also consumed by `accountteardown` and the operator CLI; removal condition: per-resource commands behind `domains/eks/awsapi` registration (ADR-0004 S3) with any remaining process boundary versioned under `contracts/eks/v1`. |
| `service_impl.go` | 2518 | mixed: cross-resource orchestration + resource owners | Split: CreateCluster launch sequence and `purgeClusterInfra` (`:504-1013`, `:1268-1452`) are cluster realization orchestration; cluster read/delete (`:1041-1162`) is `cluster`; access-entry actions (`:1527-1816`) are `access`; nodegroup wrappers (`:1465-1525`) are `nodegroup`; Tag actions (`:1842-1992`) span cluster and nodegroup; IdP stubs (`:1818-1832`); `DesiredDNSChanges` (`:2143`) is a DNS projection; `EKSServiceDeps` and capability interfaces (`:39-245`). |
| `service_nats.go` | 190 | temporary residue | Gateway-side NATS client implementing `EKSService` over `eks.<Action>` subjects; removal condition as for `service.go`. |
| `state_report.go` | 75 | guest/controller wire contract (`cluster`) | `eks.state.*` subject and `ServerStateReport`; the add-on subject and report moved to `contracts/eks/v1`, leaving only `unmarshalAddonStatusReport` (`:69`), which decodes into `eksv1.AddonStatusReport` (add-on rows at `18794e017`). |
| `store.go` | 255 | resource owner (`cluster`), mixed | Bucket names, leader bucket, migrations, key helpers for every resource (the add-on key helpers have moved to `domains/eks/addon/store.go`; 217 lines at `18794e017`); split per-resource key helpers into each owner; `AccountWatchBuckets` (`:215`) is effectively a projection consumed by DNS. |
| `token_review.go` | 128 | resource owner (`access`) | Authenticates a `get-token` bearer against STS and maps to AccessEntry groups; `ResolveTokenReview` is called from the gateway process and opens EKS KV there. |
| `token_webhook.go` | 57 | guest/controller wire contract | `eks.VerifyToken` subject and request/response consumed by `runtime/roles/awsgw`. |
| `types.go` | 131 | mixed: `nodegroup` + `access` + residue | `NodegroupRecord` (nodegroup), `AccessEntryRecord`/`AccessScope` (access); `ClusterRecord` (`:19`) and `OIDCProviderConfigRecord` (`:125`) have no production readers or writers (temporary residue; removal: delete). |
| `volume_reclaim.go` | 81 | resource owner (`cluster`) | Surfaces (never deletes) CSI-created EBS volumes at cluster delete. |

### 1.1a Add-on owner and wire contract (at `18794e017`)

| File | Lines | Class | Reason |
|---|---|---|---|
| `domains/eks/addon/record.go` | 52 | resource owner (`addon`) | `Record` and `Manifest` (persisted JSON fields unchanged from the former `AddonRecord`), `Status` (six values; `UPDATE_FAILED` and `DELETE_FAILED` absent), `ErrNotFound`, `ErrExists`. Moved from `addon_state.go`. |
| `domains/eks/addon/store.go` | 237 | resource owner (`addon`) | `Prefix`, `Key`, `ManifestKey`, `Get`, `List`, `ListManifests` (exported) and `put`, `putManifest`, `deleteManifest`, `deleteRecord`, `casUpdate` (private). `put` is unconditional, so create is get-then-put. Moved from `addon_state.go` and the add-on key helpers in `store.go`. |
| `domains/eks/addon/owner.go` | 205 | resource owner (`addon`) | `Owner` is the sole writer: `Create` (`:48`), `EnsureGPUDevicePlugin` (`:79`), `Update` (`:94`), `Delete` (`:122`), `ApplyReport` (`:140`), `markFailed` (`:171`, always `CREATE_FAILED`), `nextStatus` (`:186`). `Desired` and `Change` are the validated inputs. Moved from `addons.go`, `addon_status_reconciler.go` and the former `markAddonFailed`/`nextAddonStatus`. |
| `domains/eks/addon/delivery.go` | 77 | resource owner (`addon`) | `Installer` interface, `StagingInstaller` (writes or deletes the manifest sub-key), `Bucket`, and the owner's own `Phase`/`Report` (no version or timestamp). Moved from `addon_installer.go`. |
| `domains/eks/addon/catalog.go` | 122 | resource owner (`addon`), read-only catalogue | Static bundles `aws-load-balancer-controller` 2.11.0, `argocd` 3.0.23, `aws-ebs-csi-driver` 1.40.1, hidden `nvidia-device-plugin` 0.17.4 and hidden `spinifex-noop` 0.1.0; `Lookup`, `Specs`, `ValidateCatalog`. Moved from `addon_catalog.go`. |
| `contracts/eks/v1/addon_status.go` (repo root) | 37 | guest/controller wire contract | `AddonStatusSubject`, `AddonDeliveryPhase` (`applied`, `ready`, `failed`), `AddonStatusReport` (`addon`, `version`, `phase`, `message`, `ts`). Moved from `state_report.go`. |
| `contracts/eks/v1/addon_manifest.go` (repo root) | 18 | guest/controller wire contract | `StagedAddonManifest` and `InternalAddonsResponse`; the deployed HTTP body uses Go field names because the REST-JSON marshaller ignores the `json` tags, which `addon_manifest_test.go` pins. Moved from `addon_installer.go`, `gateway/eks` (`internalAddonsOutput`) and `agents/eks/gatewayfetch` (`stagedAddon`, `internalAddonsResponse`). |
| `contracts/eks/v1/README.md` | n/a | contract record | Compatibility boundary: a generation-aware protocol needs a successor version, not a v1 edit. |

Guest counterparty: `scripts/images/eks-node/mulga-eks-addon-sync.sh` (417 lines) runs on the primary server only, renders per add-on name, GCs rendered files whose name is no longer staged with `kubectl delete --wait=false`, ignores `configurationValues`, and builds the report with `printf`.

Additional dead code observed in `store.go`: `OIDCProviderKey` (`:95`) and `EventKey` (`:132`) have no production callers.

### 1.2 `gateway/eks.go` and `gateway/eks/` (15 files, 1,473 lines)

| File | Lines | Class | Reason |
|---|---|---|---|
| `gateway/eks.go` | 379 | AWS protocol adapter | REST-JSON route table (`:27-219`), dispatch, authorization ordering, `iam:PassRole` check (`:319-347`), caller identity extraction. |
| `gateway/eks/access.go` | 89 | AWS protocol adapter | Unmarshal and NATS call for access actions. |
| `gateway/eks/addons.go` | 60 | AWS protocol adapter | Same for add-on actions. |
| `gateway/eks/authz.go` | 310 | AWS protocol adapter | Per-action resource ARN derivation (`eksScopes` `:45-109`); imports `handlers_eks.PrincipalARNHash`. |
| `gateway/eks/cluster.go` | 66 | AWS protocol adapter | Cluster action wrappers. |
| `gateway/eks/handler.go` | 47 | AWS protocol adapter | JSON response/error writers. |
| `gateway/eks/internal_addons.go` | 33 | guest/controller wire contract | `GET /clusters/{c}/internal-addons/{acct}` for the addon-sync agent; calls `handlers_eks.NewNATSEKSService(...).ListStagedAddonManifests` and wraps the result as `eksv1.InternalAddonsResponse` (at `18794e017`). |
| `gateway/eks/internal_authz.go` | 129 | AWS protocol adapter + temporary residue | Principal gate for internal routes; `lookupClusterMeta` (`:102-129`) reads `eks-account-*` and `ClusterMeta` directly from the gateway process; removal: owner-published membership query. |
| `gateway/eks/internal_publish.go` | 91 | guest/controller wire contract | HTTP-to-NATS relay of bootstrap/state/addon channels; the `addon` channel relays the raw payload to `eksv1.AddonStatusSubject` (`:75-76`, at `18794e017`). |
| `gateway/eks/internal_recovery.go` | 37 | guest/controller wire contract | `GET .../internal-recovery/{acct}/{instance}`. |
| `gateway/eks/nodegroup.go` | 65 | AWS protocol adapter | Nodegroup wrappers. |
| `gateway/eks/oidc.go` | 45 | AWS protocol adapter | IdP-config wrappers (handlers are stubs). |
| `gateway/eks/passrole.go` | 43 | AWS protocol adapter | Extracts passed role ARNs for CreateCluster, CreateNodegroup, CreateAddon and UpdateAddon. |
| `gateway/eks/tags.go` | 35 | AWS protocol adapter | Tag wrappers. |
| `gateway/eks/token_review.go` | 51 | guest/controller wire contract | `POST /clusters/{c}/token-review` for the in-VM token webhook. |

### 1.3 Other production files wiring or reading EKS

| File | Lines | Class | Reason |
|---|---|---|---|
| `daemon/eks_deps.go` | 220 | cross-resource orchestration (composition-root binding) | Builds `EKSServiceDeps`, binding VPC, ELBv2, image, EIP, IGW, NAT-GW, route-table, placement-group, IAM, EBS and object-store owners. |
| `daemon/eks_adapter.go` | 58 | cross-resource orchestration (composition-root binding) | `SubnetVPCResolver` over the VPC service. |
| `daemon/eks_cp_control.go` | 67 | temporary residue | CP describe/start/stop calls `domains/ec2/awsapi/instance.{DescribeInstances,StartInstances,StopInstances}`, i.e. an EC2 AWS adapter used as an internal API (ADR-0004 S3/S4); removal: EC2-owned instance-control capability. |
| `daemon/eks_worker_launch.go` | 175 | cross-resource orchestration (composition-root binding) | `WorkerLauncher` via `instanceService.RunInstances`, `ec2.RunInstances.{type}.{node}` and `contracts/ec2/v1` instance commands. |
| `daemon/daemon.go` (EKS parts: `:75`, `:167`, `:1085-1128`, `:2018-2019`, `:2175`, `:2302-2304`, `:3017-3018`) | n/a | temporary residue | NATS subscription table for 38 `eks.*` subjects, service init, reconciler spawn, reaper registration, shutdown; comment at `:1085-1087` says every handler returns NotImplemented, which is stale. |
| `daemon/dns_reconcile.go` (`:27`, `:64-68`, `:97-98`) | n/a | temporary residue | Watches every `eks-account-*` bucket via `AccountWatchBuckets` and pulls `DesiredDNSChanges`; removal: EKS-published DNS projection. |
| `gateway/oidc_discovery.go` | 117 | temporary residue | Public OIDC discovery/JWKS endpoints read `eks-account-*`, `GetClusterMeta` and `OIDCJWKSKey` directly; removal: cluster-owned OIDC projection. |
| `handlers/sts/oidc_jwks.go` | 126 | temporary residue (STS-owned file) | `FetchClusterJWKS` reads `eks-account-*`/`OIDCJWKSKey` (`:101-110`); ADR-0001 step 7 names this STS-to-EKS import for removal via an identity-owned contract or EKS projection. |
| `runtime/roles/awsgw/eks_token_verify.go` | 75 | guest/controller wire contract (server) | Queue-subscribes `eks.VerifyToken` (`:30`) and verifies presigned STS URLs. |
| `accountteardown/reapers_eks.go` | 138 | cross-resource orchestration | Account teardown deletes nodegroups then clusters through the `EKSService` NATS client. |
| `operator/cli/admin_eks.go` | 77 | guest/controller wire contract (operator) | `spx admin eks restore-snapshot` over `eks.RestoreSnapshot`. |
| `gateway/gateway.go` (`:300`, `:345-346`, `:403`, `:455`, `:513`), `gateway/auth.go` (`:278-279`), `gateway/operations.go` (`:34-36`) | n/a | AWS protocol adapter (legacy registration residue) | Legacy dispatch, OIDC public routes and inventory; removal per ADR-0002 when `domains/eks/awsapi` registers. |
| `bootstrap/config/config.go:44` | n/a | AWS protocol adapter (config) | `eks` listed in `AWSGWServiceNames`. |

Guest counterparties (not inventoried individually): `agents/eks/{gatewaypublish,gatewayfetch,tokenwebhook,credentialprovider,konnectivitycert,webhookcert}` and `scripts/images/eks-node/*`.
`agents/eks/gatewayfetch/fetch.go` decodes the internal-addons body through `contracts/eks/v1` `InternalAddonsResponse` (at `18794e017`).
`agents/eks/tokenwebhook/webhook.go` imports `handlers_eks.WebhookTokenReviewResult`, so a guest binary imports the domain monolith (ADR-0004 S5 debt).

## 2. Served AWS actions and protocol

Protocol: REST-JSON, method+path routed by `rest.NewRouter("eks", eksRoutes)` (`gateway/eks.go:234`); unknown routes return `InvalidAction` (`:241`).
Authorization order (`gateway/eks.go:237-312`): route lookup, account from auth context, internal-route principal gate, body read, query folding, `ResourceARNs`, `checkPolicyResources`, `checkEKSPassRole`, handler over NATS.
Gateway-to-daemon transport: NATS request/reply `eks.<Action>`, queue group `spinifex-workers` (`daemon/daemon.go:1090-1127`).
`gateway/operations.go:34-36` declares EKS as `Registered: eksActionNames()` with no `Stubbed` or `Unsupported` list, so stubs and internal routes are indistinguishable in the inventory.

| Action | Route | Status | Owner |
|---|---|---|---|
| CreateCluster | POST /clusters | implemented (async) | cluster |
| ListClusters | GET /clusters | implemented | cluster |
| DescribeCluster | GET /clusters/{c} | implemented | cluster |
| DeleteCluster | DELETE /clusters/{c} | implemented (synchronous purge) | cluster |
| UpdateClusterConfig | POST /clusters/{c}/update-config | stub NotImplemented (`service_impl.go:1455`) | cluster |
| UpdateClusterVersion | POST /clusters/{c}/updates | stub NotImplemented (`service_impl.go:1459`) | cluster |
| ListUpdates | GET /clusters/{c}/updates | stub at gateway (`gateway/eks.go:49-52`) | cluster (update records do not exist) |
| DescribeUpdate | GET /clusters/{c}/updates/{id} | stub at gateway (`gateway/eks.go:53-56`) | cluster/nodegroup/addon |
| CreateNodegroup | POST /clusters/{c}/node-groups | implemented (async) | nodegroup |
| ListNodegroups | GET /clusters/{c}/node-groups | implemented | nodegroup |
| DescribeNodegroup | GET .../node-groups/{n} | implemented | nodegroup |
| UpdateNodegroupConfig | POST .../node-groups/{n}/update-config | implemented (synchronous, returns `Successful`) | nodegroup |
| UpdateNodegroupVersion | POST .../node-groups/{n}/update-version | stub NotImplemented (`nodegroup.go:1120-1127`) | nodegroup |
| DeleteNodegroup | DELETE .../node-groups/{n} | implemented (synchronous) | nodegroup |
| CreateAccessEntry | POST /clusters/{c}/access-entries | implemented (STANDARD type only, `service_impl.go:1561-1564`) | access |
| ListAccessEntries | GET /clusters/{c}/access-entries | implemented | access |
| DescribeAccessEntry | GET .../access-entries/{p} | implemented | access |
| UpdateAccessEntry | POST .../access-entries/{p} | implemented | access |
| DeleteAccessEntry | DELETE .../access-entries/{p} | implemented | access |
| AssociateAccessPolicy | POST .../access-entries/{p}/access-policies | implemented (cluster scope only, `service_impl.go:1692`) | access |
| DisassociateAccessPolicy | DELETE .../access-policies/{policy} | implemented | access |
| ListAssociatedAccessPolicies | GET .../access-entries/{p}/access-policies | implemented | access |
| ListAccessPolicies | GET /access-policies | implemented (static catalogue) | access |
| DescribeAddonVersions | GET /addons/supported-versions | implemented (static catalogue) | addon |
| ListAddons | GET /clusters/{c}/addons | implemented | addon |
| CreateAddon | POST /clusters/{c}/addons | implemented (async via guest) | addon |
| DescribeAddon | GET .../addons/{a} | implemented | addon |
| UpdateAddon | POST .../addons/{a}/update | implemented | addon |
| DeleteAddon | DELETE .../addons/{a} | implemented | addon |
| AssociateIdentityProviderConfig | POST .../identity-provider-configs/associate | stub NotImplemented (`service_impl.go:1818`) | cluster (no IdP resource) |
| DescribeIdentityProviderConfig | POST .../identity-provider-configs/describe | stub | cluster |
| DisassociateIdentityProviderConfig | POST .../identity-provider-configs/disassociate | stub | cluster |
| ListIdentityProviderConfigs | GET .../identity-provider-configs | stub | cluster |
| TagResource / UntagResource / ListTagsForResource | POST/DELETE/GET /tags/* | implemented for cluster and nodegroup ARNs; NotImplemented for addon/access-entry ARNs (`service_impl.go:1882,1925,1958`) | cluster, nodegroup |
| PublishInternal | POST /clusters/{c}/internal-publish | internal (not AWS) | cluster guest contract |
| WebhookTokenReview | POST /clusters/{c}/token-review | internal (not AWS) | access guest contract |
| ListInternalAddons | GET /clusters/{c}/internal-addons/{acct} | internal (not AWS) | addon guest contract |
| GetRecoveryDirective | GET /clusters/{c}/internal-recovery/{acct}/{id} | internal (not AWS) | cluster guest contract |

Internal NATS-only methods (no HTTP route): `SetRecoveryDirective`, `RestoreSnapshot`, `ListStagedAddonManifests` (backs ListInternalAddons).
Unregistered AWS EKS operations (InvalidAction today) include Fargate profiles, pod identity associations, encryption config, insights, `RegisterCluster`/`DeregisterCluster`, `DescribeAddonConfiguration`, `DescribeClusterVersions` and EKS Anywhere subscriptions (list from memory of the EKS API, not verified against the pinned model).
`iam:PassRole` is enforced for CreateCluster `roleArn`, CreateNodegroup `nodeRole`, and CreateAddon/UpdateAddon `serviceAccountRoleArn` (`gateway/eks/passrole.go`).

## 3. Durable records and KV buckets

| Bucket | Key | Record | Owner | Writers | Readers outside EKS |
|---|---|---|---|---|---|
| `eks-account-{acct}` (history 1, migration v1, `store.go:30-38,235-245`) | `clusters/{c}/meta` | `ClusterMeta` (`cluster_state.go:65`) | cluster | `service_impl.go` create/launch/delete/tags, `cluster_state.go` CAS helpers, `cluster_reconciler.go`, `restore_snapshot.go:150`, `nats_bootstrap.go:297` (CA) | **`gateway/eks/internal_authz.go:110-124`**, **`gateway/oidc_discovery.go:38,60`** |
| same | `clusters/{c}/nodegroups/{ng}` | `NodegroupRecord` (`types.go:33`) | nodegroup | `nodegroup.go` (claim, CAS, put, delete), `service_impl.go:1964` (tags) | none observed |
| same | `clusters/{c}/access-entries/{sha256(principalARN)}` | `AccessEntryRecord` (`types.go:93`) | access | `service_impl.go:1548-1785`, creator-admin seed `service_impl.go:994-1001` (cluster launch) | **gateway process via `ResolveTokenReview` (`token_review.go:113`)** |
| same | `clusters/{c}/addons/{a}` | `addon.Record` (`domains/eks/addon/record.go`) | addon | `domains/eks/addon/owner.go` only, reached from `addons.go`, `addon_status_reconciler.go` and `nodegroup.go:405-416` (GPU add-on); plus the `DeleteClusterPrefix` sweep (at `18794e017`) | none observed |
| same | `clusters/{c}/addons/{a}/manifest` | `addon.Manifest` (`domains/eks/addon/record.go`) | addon | `domains/eks/addon/delivery.go` `StagingInstaller`, `Owner.Delete`, the `DeleteClusterPrefix` sweep | guest via internal-addons route, as `contracts/eks/v1` `StagedAddonManifest` |
| same | `clusters/{c}/oidc-signing-key.pem.enc` | encrypted ECDSA key | cluster | `oidc_keypair.go:86`, zeroized `service_impl.go:1281` | none |
| same | `clusters/{c}/oidc-jwks.json` | JWKS | cluster | `oidc_keypair.go:94` | **`handlers/sts/oidc_jwks.go:110`**, **`gateway/oidc_discovery.go:93`** |
| same | `clusters/{c}/oidc-jwks-verified` | marker | cluster | `nats_bootstrap.go:291` | none |
| same | `clusters/{c}/admin-kubeconfig.enc` | encrypted kubeconfig | cluster | `nats_bootstrap.go:269` | none observed in Go (uncertain whether a CLI reads it) |
| same | `clusters/{c}/k3s-node-token.enc` | encrypted join token | cluster | `nats_bootstrap.go:251` | read by `nodegroup.go:1164` (cross-resource) |
| same | `clusters/{c}/recovery/{instanceId}` | `RecoveryDirective` | cluster | `recovery_directive.go:102`, reconciler | guest via internal-recovery route |
| same (whole bucket) | all keys | watch | cluster | n/a | **`daemon/dns_reconcile.go:68`** via `AccountWatchBuckets` |
| `spinifex-eks-leader` (TTL 60s, `store.go:40-42,249`) | `{acct}/{c}` reconciler lease; `.../teardown` lease | kvlease | cluster | `cluster_reconciler.go:391-403`, `service_impl.go:1174-1197` | none |
| `spinifex-eks-clustertokens` (TTL 15m, `clienttoken.go:16-21`) | idempotency store keyed by account+token | `idempotency.Store[string]` | cluster | `service_impl.go:529-558,612-619` | none |

Dead key helpers: `OIDCProviderKey` (`store.go:95`), `EventKey` (`store.go:132`).
Secret material in `ClusterMeta`: `ControlPlaneTemplate` is a copy of `K3sServerInput` that clears creds but not `OIDCPrivateKeyPEM` or `JoinToken` (`service_impl.go:945-959`; fields at `k3s_server_vm.go:146,171`), so the OIDC private key and k3s join token appear to be persisted unencrypted in the meta record that the gateway also reads; no scrubbing or custom marshaller was found (verify with a KV dump before acting).
`ClusterMeta` combines desired intent (`Version`, `RoleArn`, `ResourcesVpcConfig`, `Logging`, `Tags`), realization refs (CP nodes, NLB, CP VPC, EIP, ENIs), observations (`HealthIssue`, `NodeCount`, `NodegroupNodeCounts`, `CertificateAuthorityB64`) and delete bookkeeping (`DeletingSince`, `DeleteReapAttempts`).
Hidden realizations outside EKS stores: CP VMs (system account, `ManagedBy=eks`), CP VPC/subnets/NAT-GW/route tables/IGW, NLB and target groups (ELBv2), SGs, private-endpoint ENI, egress EIP, HA placement group, worker instances (customer account), IAM roles/instance profiles, CSI EBS volumes.

## 4. Identity and scope

| Resource | Identifier / ARN | Account | Region | AZ |
|---|---|---|---|---|
| cluster | name; `arn:aws:eks:{region}:{acct}:cluster/{name}` (`foundation/aws/arn/eks.go:21`) | tenant account owns the record; CP VMs, CP VPC and NLB are realized in the system account `awsidentifiers.GlobalAccountID` (`service_impl.go:738`, `:2240-2248`); customer-VPC SGs and the private-endpoint ENI in the tenant account | single Region from `deps.Region`; not stored as a field | Regional; HA CP spread over hosts by AZ (`k3s_ha_control_plane.go:528`) |
| nodegroup | `arn:...:nodegroup/{c}/{ng}/{discriminator}` where the discriminator is derived from (account, cluster, name) (`nodegroup.go:333`), so a recreated nodegroup gets the same ARN, unlike AWS's per-incarnation UUID | tenant | deps.Region | AZs implied by subnets |
| addon | `arn:...:addon/{c}/{a}` (`arn/eks.go` `FormatEKSAddon`, minted in `domains/eks/addon/owner.go` `Create`; the gateway authorizer derives the same string, `gateway/eks/authz.go` `addonARN`); no per-incarnation suffix, whereas the AWS API reference examples show `addon/{c}/{a}/{uuid}` | tenant | owner `region` from deps.Region | n/a |
| access | `arn:...:access-entry/{c}/{sha256(principal)}` (`access_entry.go:244`); AWS's shape differs (verify) | tenant | deps.Region | n/a |

Uniqueness is per account bucket and cluster-name prefix; there is no immutable internal UID distinct from the name for any EKS resource.

## 5. Lifecycle paths per resource

### cluster
Create: `CreateCluster` `service_impl.go:504`; validates, optional token claim (`:529-558`), resolves VPC, builds `ClusterMeta{Status: CREATING}`, `claimClusterName` CAS-create with FAILED-to-CREATING reclaim (`:1200-1265`), finalizes token, then launches `launchClusterInfra` on an in-process goroutine with `context.Background()` (`:625-645`).
Launch stages (`:718-1012`): CP VPC, SGs, CP ingress, customer SGs, private ENI, NLB, OIDC keypair, join token, CP placement/launch, persist CP refs, NLB target registration, persist final meta, creator-admin AccessEntry, `spawnBootstrap`, `spawnReconciler`; any failure calls `failClusterLaunch` (`:696`) which marks FAILED.
Readiness: `ClusterReconciler.reconcileOnce` (`cluster_reconciler.go:594`) flips CREATING to ACTIVE when bootstrap artifacts exist (`:1173-1189`) and the CP state report is healthy; CREATING becomes FAILED after `createTimeout` (`:1151-1171`).
Reconciliation/repair (ACTIVE): health recording, CP restart (`:708`), etcd reset via recovery directives (`:780`), member replacement (`:1010`), all under the per-cluster lease `spinifex-eks-leader/{acct}/{c}` (`:391-418`).
Restart: `SpawnRegisteredReconcilers` (`service_impl.go:342-425`) rescans buckets, reclaims orphaned nodegroups, resubscribes missing bootstrap kinds and respawns reconcilers; it does not resume an interrupted `launchClusterInfra`.
Delete: `DeleteCluster` (`service_impl.go:1104-1155`) returns success for an absent cluster, takes the teardown lease (losers return DELETING), sets DELETING, runs `purgeClusterInfra` synchronously in the request (`:1268-1452`), and only erases `clusters/{c}/` after all blocking steps succeed; `EKSDeletingReaper` re-drives with backoff and exhaustion (`eks_deleting_reaper.go:46-159`); `EKSBillableReaper` terminates CP VMs whose meta is gone (`eks_billable_reaper.go:46-100`).
Fencing: leases only; no desired/observed generation (no `generation` field anywhere in `handlers/eks`).
Idempotency: `clientRequestToken` honoured for CreateCluster only, 15-minute TTL, replays current meta by name.

### nodegroup
Create: `createNodegroup` (`nodegroup.go:205`) requires cluster ACTIVE (`:226-229`), `ClaimNodegroupRecord` CAS-create (`:112-126`, ResourceInUse at `:363`), then background `launchNodegroupInfra` (`:373`, `:449-535`).
Readiness: `waitWorkersReady` (`:537-563`) polls the cluster's `NodegroupNodeCounts` observation until ready count reaches baseline plus desired, then sets ACTIVE (`:521-523`), else CREATE_FAILED.
Update: `UpdateNodegroupConfig` runs `reconcileNodegroup` inline under revision CAS (`:914-979`, `launchWorkersCAS` `:985`, `recordLaunchedWorker` `:1038`) and returns `Update{Status: Successful}` (`:900-906`).
Restart: `reclaimOrphanedNodegroups` (`:1189-1228`) terminates workers of CREATING/partial CREATE_FAILED records and marks them CREATE_FAILED; it does not resume.
Delete: `deleteNodegroup` (`:1129-1158`) terminates workers then deletes the record synchronously and returns a DELETING projection of a record that no longer exists.
Fencing: KV revision CAS; no generation, no lease.
Idempotency: no `clientRequestToken`; duplicate create loses the claim and gets ResourceInUse.

### addon
Paths in this subsection are at `18794e017`.
Create: `CreateAddon` (`addons.go:61`) validates against the catalogue and calls `Owner.Create` (`domains/eks/addon/owner.go:48`), which is get-then-put (not CAS-create) and then stages the manifest (`delivery.go` `StagingInstaller.Install`); a staging failure marks `CREATE_FAILED`; returns CREATING.
Readiness: guest addon-sync agent publishes `eks.addon.{acct}.{c}.status` via internal-publish; `applyAddonStatusReport` (`addon_status_reconciler.go:22`) relays to `Owner.ApplyReport` (`owner.go:140`), whose `nextStatus` (`:186`) maps `ready` to ACTIVE from any status and `failed` to CREATE_FAILED/DEGRADED; `report.Version` and `report.TS` are dropped before the owner sees them.
Update: `UpdateAddon` (`addons.go:163`) calls `Owner.Update` (`owner.go:94`), which CAS-sets UPDATING from any status and restages; the handler returns `Successful` with `Id = rec.Arn` (`addons.go:193-194`); a staging failure marks `CREATE_FAILED`.
Delete: `DeleteAddon` (`addons.go:201`) calls `Owner.Delete` (`owner.go:122`), which unstages and deletes the record and manifest immediately; the DELETING status is only in the response.
Fencing: revision CAS on the record; no generation or incarnation.
Idempotency: none (`clientRequestToken` ignored).

### access
Create: `CreateAccessEntry` (`service_impl.go:1548-1581`) get-then-put (`:1570-1578`), synchronous; creator-admin entry seeded during cluster launch (`:994-1001`).
Mutations: `casUpdateAccessEntry` (`access_entry.go:141`) for update/associate/disassociate; delete via `DeleteAccessEntryRecord`.
Effect: read at authentication time by `Authenticate`/`ResolveTokenReview` (`token_review.go:31-128`); no in-cluster realization or readiness.
Delete with cluster: swept by `DeleteClusterPrefix` (`cluster_state.go:493`).
Idempotency: none (`clientRequestToken` ignored).

## 6. NATS subjects and guest/controller messages

| Subject / route | Direction | Delivery | Defined | Consumer |
|---|---|---|---|---|
| `eks.<Action>` (38 subjects) | gateway (or teardown/CLI) to daemon | core NATS request/reply, queue `spinifex-workers` | `service_nats.go:30-189`, `daemon/daemon.go:1090-1127` | `EKSServiceImpl` |
| `eks.bus.{acct}.{c}.{k3s-bootstrap-token,k3s-admin-kubeconfig,k3s-oidc-jwks,k3s-ca}` | CP VM via gateway relay to daemon | core NATS publish/subscribe, at-most-once | `nats_bootstrap.go:47-57`, subscribe `:197` | `NATSBootstrap` persists to KV with `persistWithRetry` (`:32`) |
| `eks.state.{acct}.{c}.server` | CP VM via relay | core NATS, periodic | `state_report.go:13` | `ClusterReconciler` (`cluster_reconciler.go:432`) |
| `eks.addon.{acct}.{c}.status` | CP VM via relay | core NATS, level-triggered every 30 s per staged add-on | `contracts/eks/v1/addon_status.go:10` (at `18794e017`) | `ClusterReconciler` (`:448`), relayed to `addon.Owner.ApplyReport` |
| `eks.VerifyToken` | EKS (gateway process) to awsgw STS | request/reply, queue `awsgw-eks-token-verify` | `token_webhook.go:13` | `runtime/roles/awsgw/eks_token_verify.go:30` |
| `contracts/cluster/v1` `NodeStatusSubject`, `NodeVMsSubject` | EKS to every node | fan-out request | `k3s_ha_control_plane.go:502,579` | daemons |
| `ec2.RunInstances.{type}.{node}` and `contracts/ec2/v1` instance commands | EKS (daemon binding) to EC2 | request/reply | `daemon/eks_worker_launch.go` | EC2 instance owner |
| HTTP `internal-publish`, `token-review`, `internal-addons`, `internal-recovery` | guest to gateway (SigV4, system CP role) | HTTP | `gateway/eks.go:59-87` | gateway relays/queries |
| HTTP `/oidc/eks/{region}/{acct}/{c}/.well-known/openid-configuration`, `/keys` | public | HTTP, unauthenticated | `gateway/gateway.go:345-346` | `gateway/oidc_discovery.go` |

Retry and recovery paths: guest `eks-gateway-publish` retries until HTTP 2xx (`agents/eks/gatewaypublish/publish.go:26-31,97-114`), but the gateway returns 2xx after a core-NATS publish even with no subscriber (`gateway/eks/internal_publish.go:84-89`), so a bootstrap message published while the daemon subscriber is absent is lost.
On restart, `BootstrapPendingKinds` resubscribes only missing kinds (`nats_bootstrap.go:115-139`, `service_impl.go:410-416`); a lost one-shot message is not replayed, so the cluster reaches FAILED at the create timeout.
A bootstrap subscriber that exits with error marks the cluster FAILED (`service_impl.go:2192-2212`).
SDK retries of `eks.<Action>` land on any node of the queue group; dedup relies on token store, claim CAS, teardown lease or nothing (addon/access create).

## 7. Boundary-crossing imports

### Outbound from `handlers/eks`
- IAM/identity: `handlers/iam` `EncryptSecret`/`DecryptSecret` (`nats_bootstrap.go`, `oidc_keypair.go`, `nodegroup.go`), `EnsureSystemInstanceProfile` (`k3s_server_role.go`), `SystemInstanceRoleEnsurer` (`service_impl.go`); `instanceProfileEnsurer` capability with `GetRole/CreateRole/PutRolePolicy/CreateInstanceProfile/AddRoleToInstanceProfile` (`service_impl.go:214`) means EKS creates IAM roles and instance profiles.
- Bluebottle: `bluebottle/pkg/auth` `ResolveRoleARN` (`nodegroup.go`) and `ParseRoleARN` (`service_impl.go`); daemon binding uses `bluebottle/pkg/masterkey`; gateway uses `bluebottle/pkg/auth` (`gateway/eks.go`).
- STS: token verification over `eks.VerifyToken` (process contract, no import).
- EC2 instance: `domains/ec2/systeminstance` (`SystemInstanceInput/Output`, `ExtraENIInput`, `BootAMI`), `domains/ec2/instancetypes`, `domains/ec2/placementgroup` (alias `ec2placementgroup`), `domains/ec2/ebs/metadata`, `runtime/compute/vm` (reapers), plus `WorkerLauncher`, `k3sInstanceLauncher`, `k3sAMIResolver`, `eipProvisioner`, `csiVolumeReclaimer` capabilities.
- Network: `domains/network/systemvpc` (`Ensure`, `Delete`, `EnsureIGW`, `DeleteIGW`, provisioner interfaces); `sgProvisioner` and `k3sVPCProvisioner` capabilities (EC2 SDK shapes).
- DNS: `domains/dns` `EKSName`, `EKSChanges`, `ResolveBaseDomain`, `Change`, `ActionUpsert` (`service_impl.go`).
- ACM/CA: no import; the platform/NATS CA PEM is passed as `GatewayCACert` from `d.config.NATS.CACert` (`daemon/eks_deps.go:64-71`) and baked into CP user data; the k3s cluster CA is self-generated in the guest and returned on `k3s-ca`.
- ELBv2: `nlbProvisioner` capability with ELBv2 SDK shapes (`nlb.go:19`), bound to `d.elbv2Service`.
- Storage/object: `providers/objectstore` (etcd snapshots), Predastore system creds via deps.
- Contracts: `contracts/cluster/v1`; `contracts/eks/v1` from `addons.go`, `addon_status_reconciler.go`, `state_report.go` and `service_impl.go` (at `18794e017`).
- EKS add-on owner: `domains/eks/addon` from `addons.go`, `addon_status_reconciler.go`, `service_impl.go` and, through `addons()`, `nodegroup.go` (at `18794e017`; edges in Section 7.1).
- Foundation: `kvutil`, `kvstore`, `kvlease`, `migrate`, `idempotency`, `reconciler`, `telemetry`, `aws/{arn,errors,tags,identifiers,ami}`, `messaging/nats`.
- Bootstrap: `bootstrap/config.Config` in `EKSServiceDeps`.

### Inbound to `handlers/eks`
- `gateway/eks/*`: `NewNATSEKSService`, input/output types (including `ListStagedAddonManifestsInput` in `internal_addons.go`), `PrincipalARNHash`, `ClusterMeta`, `ClusterMetaKey`, `AccountBucketName`, `CPInstanceRoleName`, `ResolveTokenReview`, bootstrap/state subject builders; the add-on subject now comes from `contracts/eks/v1` (at `18794e017`).
- `gateway/oidc_discovery.go`: `AccountBucketName`, `GetClusterMeta`, `OIDCJWKSKey`.
- `handlers/sts/oidc_jwks.go`: `AccountBucketName`, `OIDCJWKSKey` (STS reads EKS private KV).
- `runtime/roles/awsgw/eks_token_verify.go`: `TokenVerifySubject`, `TokenVerifyRequest`, `TokenVerifyResponse`.
- `agents/eks/tokenwebhook/webhook.go`: `WebhookTokenReviewResult` (guest imports domain).
- `daemon/*`: `EKSServiceImpl`, `NewEKSServiceImpl`, `EKSServiceDeps`, `NewNATSHostScheduler`, `SubnetVPCResolver`, `WorkerLauncher`, `AccountWatchBuckets`.
- `accountteardown/reapers_eks.go`: `EKSService`, `NewNATSEKSService`.
- `operator/cli/admin_eks.go`: `NewNATSEKSService`, `RestoreSnapshotInput`.

### Reverse knowledge of EKS in other domains (comments or tag conventions, no import)
`domains/ec2/instance/service_impl.go:394-396` (worker tag comment), `domains/ec2/systeminstance/launcher.go:4,25`, `handlers/elbv2/{launcher.go:5,service_impl.go:1232-1250,service_impl_nlb_sg.go:196}`, `domains/ec2/vpc/eni.go:54`, `domains/ec2/guestmetadata/resolver.go:37`, `domains/dns/{naming.go:118-124,reconcile.go:58,303}`, `handlers/iam/managed_policies.go` (EKS managed policies), `accountteardown/reapers_iam.go:26-33` (OIDC providers).

### 7.1 Add-on boundary edges remaining at `18794e017`

Each edge is temporary unless marked otherwise, and each clears only on its stated condition; moving a file does not clear any of them.
E1 to E5 match the `handlers/eks` → `domains/eks/addon` debt entry in `docs/PACKAGE_BOUNDARY_MIGRATION.md`; E6 to E9 match the further-edges entry beside it.

| ID | Edge | Kind | Where | Removal condition |
|---|---|---|---|---|
| E1 | Cluster delete erases add-on records and manifests | handler → owner state (legacy) | `handlers/eks/cluster_state.go:493` `DeleteClusterPrefix` sweeps `clusters/{c}/` without calling `addon.Owner` | Cluster delete invokes an add-on-owned teardown capability. |
| E2 | GPU node-group launch stages `nvidia-device-plugin` | node group → add-on orchestration | `handlers/eks/nodegroup.go:405-416` `stageGPUDeviceAddon`, called at `:473`, through `Owner.EnsureGPUDevicePlugin`; best-effort, errors logged and swallowed, never undone on node-group delete, and any existing record (including `CREATE_FAILED`) blocks re-staging | The node-group owner is extracted and the dependency is designed. |
| E3 | Add-on delivery reports are received by the cluster reconciler | cluster hosts add-on observation | `handlers/eks/cluster_reconciler.go:171-176,264-269,447-458` subscribes; `addon_status_reconciler.go:22` relays through the reconciler-declared `addonReports`; wired in `service_impl.go:2160-2163` | Add-on delivery observation has its own host under the lifecycle correction. |
| E4 | Cluster existence is checked by the handler | handler admission | `addons.go` actions call `acctKVForCluster`, which reads `ClusterMeta` existence only; cluster status is ignored | Moves into the owner when cluster admission is defined. |
| E5 | Guest wire types | versioned contract (not debt) | `contracts/eks/v1` imported directly by `handlers/eks` (`addons.go`, `addon_status_reconciler.go`, `state_report.go`, `service_impl.go`), `gateway/eks` (`internal_addons.go`, `internal_publish.go`) and `agents/eks/gatewayfetch` | Permanent while v1 guests exist; v1 is retired only through a successor version and an explicit retirement, never by editing v1. |
| E6 | The persisted `Record` crosses to the AWS adapter | owner record exposed | `addons.go` `addonRecordToAWS` (`:222`) and `ListAddons`/`DescribeAddon` read `addon.Get`/`addon.List` directly | Replace with an owner-returned view when the add-on AWS adapter moves to `domains/eks/awsapi`. |
| E7 | The guest route reaches the owner through the monolith RPC | gateway → `handlers/eks` | `gateway/eks/internal_addons.go` uses `handlers_eks.NewNATSEKSService` and `ListStagedAddonManifestsInput`; `handlers/eks/addons.go:117` maps `addon.Manifest` to `eksv1.StagedAddonManifest` | As for `service.go`/`service_nats.go`: per-resource commands behind `domains/eks/awsapi` registration, with the internal request versioned with the add-on contract. |
| E8 | Owner and contract keep separate shapes | deliberate duplication (not debt) | `addon.Manifest`/`addon.Report` beside `eksv1.StagedAddonManifest`/`eksv1.AddonStatusReport`; the handler maps between them | None: the owner does not import the wire contract. Revisit only if a successor contract makes the mapping lossy. |
| E9 | The add-on ARN is derived in two places | duplicated identity rule | `domains/eks/addon/owner.go` `Create` and `gateway/eks/authz.go` `addonARN` both call `arn.FormatEKSAddon` from names | Clears when authorization obtains the ARN from the owner, which becomes necessary if an incarnation suffix is adopted. |

## 8. Known gaps against Q-78 / ADR-0003 (observed)

1. No desired or observed generation for any EKS resource; fencing is leases (cluster) or record-revision CAS (nodegroup, addon, access).
2. Intent and observation share `ClusterMeta` (`cluster_state.go:65-190`), and nodegroup readiness reads a cluster observation (`nodegroup.go:537-549`).
3. Cluster realization runs in request-spawned goroutines (`service_impl.go:625-645`); restart does not resume the launch, so an interrupted create ends FAILED after timeout (`cluster_reconciler.go:1151-1171`) rather than converging the same intent.
4. CP VMs are live before their IDs are persisted (`service_impl.go:960-970` comment); a crash in that window leaves VMs reachable only by the billable reaper after meta removal.
5. Bootstrap artifacts travel on at-most-once core NATS (`nats_bootstrap.go:197`, `gateway/eks/internal_publish.go:84`); a lost message is not replayed.
6. DeleteCluster purges synchronously inside the request (`service_impl.go:1141-1154`) rather than accepting asynchronously as AWS does.
7. DeleteCluster does not reject a cluster with nodegroups (AWS returns ResourceInUseException) and `DeleteClusterPrefix` erases nodegroup records while their worker instances keep running; `accountteardown/reapers_eks.go:15-18` documents this; workers carry no `ManagedBy` tag (`nodegroup.go:669-670`) and no reaper reclaims them (uncertain whether the e2e suite exercises this order).
8. DeleteNodegroup is synchronous and removes the record immediately (`nodegroup.go:1145-1157`), so the resource is absent before EC2 termination completes; no durable deletion intent.
9. UpdateNodegroupConfig and UpdateAddon return `Successful` synchronously (`nodegroup.go:900-906`; `addons.go:192-197` at `18794e017`) and `DescribeUpdate`/`ListUpdates` are stubs, so there is no truthful pending update.
10. CreateAddon and CreateAccessEntry use get-then-put (`domains/eks/addon/owner.go` `Owner.Create` at `18794e017`; `service_impl.go:1570-1578`), so concurrent creates can both succeed.
11. Addon status reports are not fenced by version or incarnation (`addon_status_reconciler.go:22` drops `AddonStatusReport.Version` and `TS` at `18794e017`), so a stale report can flip UPDATING to ACTIVE, or a stale `failed` to CREATE_FAILED.
12. DeleteAddon deletes the record and manifest before in-cluster removal is observed (`domains/eks/addon/owner.go` `Owner.Delete` at `18794e017`), and the guest sends no removal report.
13. `clientRequestToken` is honoured only by CreateCluster; CreateNodegroup, CreateAddon, CreateAccessEntry, UpdateNodegroupConfig, UpdateAddon ignore it.
14. Nodegroup and addon ARNs are deterministic by name, so identifiers are reused across incarnations (Q-78 references Q-43 identifier-reuse rules).
15. ADR-0004 S3 inventory: EKS has no `Stubbed`/`Unsupported` classification (`gateway/operations.go:34-36`) although nine actions return NotImplemented, and four internal routes are mixed into the AWS action inventory.
16. ADR-0003 S4 / ADR-0004 S4: three non-owner readers of EKS private KV (`gateway/eks/internal_authz.go`, `gateway/oidc_discovery.go`, `handlers/sts/oidc_jwks.go`) plus `daemon/dns_reconcile.go` watching all EKS buckets.
17. ADR-0004 S3: `daemon/eks_cp_control.go` uses the EC2 AWS adapter package as an internal API.
18. ADR-0004 S5: guest `agents/eks/tokenwebhook` imports `handlers/eks`.
19. Observed security risk outside Q-78: plaintext OIDC private key and k3s join token in `ClusterMeta.ControlPlaneTemplate` (Section 3).
20. Comment hygiene: EKS lifecycle evidence now cites `ADR-0003:S2`; `daemon/daemon.go:1085-1087` is stale.

## 9. First-resource candidates

| Rank | Resource | Approx. size | Private state | Dependencies | Tests | Extractable without EKS identity/IAM/Bluebottle? |
|---|---|---|---|---|---|---|
| 1 | access | about 670 lines (`access_entry.go` 255, `service_impl.go:1527-1816` about 290, `token_review.go` 128, parts of `types.go`) | `clusters/{c}/access-entries/*` only | cluster existence query (`acctKVForCluster` reads `ClusterMeta`), cluster launch seeds creator admin, gateway authz needs `PrincipalARNHash`, gateway token-review calls `ResolveTokenReview`, STS verify contract | `access_entry_test.go`, `service_impl_test.go` (access cases), `token_review_test.go`, `token_webhook_test.go`, `gateway/eks/authz_test.go`, `gateway/eks/token_review_test.go`, e2e `TestEKS/AccessEntry`, `GetToken` | The record CRUD and policy catalogue are extractable now with a narrow cluster-exists query; the token-review part touches STS identity (`eks.VerifyToken` contract) but not IAM or Bluebottle, so it can stay behind its existing NATS contract. |
| 2 | addon (owner extracted) | at `18794e017`: owner 693 lines in `domains/eks/addon`, wire contract 55 lines in `contracts/eks/v1`, adapter and relay 319 lines left in `handlers/eks` (`addons.go`, `addon_status_reconciler.go`) | `clusters/{c}/addons/*` incl. manifests | the edges in Section 7.1 | `domains/eks/addon/*_test.go`, `contracts/eks/v1/*_test.go`, `handlers/eks/addon_lifecycle_test.go`, `addons_test.go`, `addon_status_reconciler_test.go`, `gateway/eks_addon_wire_test.go`, `agents/eks/gatewayfetch/addons_wire_test.go`, e2e `AddonDelivery`, `TestEKSGPUPodExposure` | Extracted without IAM or Bluebottle; the report-relay seam (E3) and the guest contract (E5) exist, and every lifecycle gap remains. |
| 3 | nodegroup | about 1,650 lines (`nodegroup.go`, `nodegroup_userdata.go`, `claim.go`, part of `types.go`, nodegroup SG rules) | `clusters/{c}/nodegroups/*` | cluster meta (ACTIVE, CP ENI IP, VPC, readiness counts), cluster join token decrypt (`handlers_iam.DecryptSecret`), IAM role/profile creation, Bluebottle `ResolveRoleARN`, EC2 worker launch, AMI lookup, cluster SGs, GPU addon | `nodegroup_test.go` (39 tests), `nodegroup_recreate_test.go`, `nodegroup_instance_profile_test.go`, `nodegroup_userdata_test.go`, `daemon/eks_worker_launch_test.go`, e2e IRSA pod path | No; it needs IAM, Bluebottle and cluster secrets capabilities first. |
| 4 | cluster | about 8,500 lines (remainder of `handlers/eks`) | meta, OIDC material, kubeconfig, join token, recovery directives, leader/token buckets | every external owner (EC2, VPC, ELBv2, DNS, IAM, objectstore, placement, scheduler) and all guest contracts | `create_cluster_test.go`, `delete_cluster_test.go`, `cluster_reconciler*_test.go` (5 files), `lifecycle_invariants_test.go` (RLC1-3), reapers, `nats_bootstrap_test.go`, `oidc_keypair_test.go`, `k3s_*_test.go`, `nlb_test.go`, `security_groups_test.go`, `cp_vpc_test.go`, `restore_snapshot_test.go`, e2e `TestEKS` | No; it is the orchestration hub and depends on identity, Bluebottle and the CA. |

Recommendation: extract `access` first, defining a cluster-owned "cluster exists and is not deleting" query and moving `PrincipalARNHash` and the key helper into the access package.
Its shortcomings (gap 10, no client token, no generation) are small and can be recorded in its lifecycle contract without first resolving cluster realization.
