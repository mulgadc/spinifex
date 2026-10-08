# EKS managed add-on: delivery executor and fencing inventory

Code read at 3a55e2ee1; not verified live.

This inventory supports the "Delivery executor" section (Section 10) and review decision R6 of `docs/package-boundary/eks-addon-lifecycle-design.md`, and the draft proposal PROP-SPINIFEXARCH-007 (`docs/development/proposals/spinifex-architecture/0007-contract-guest-and-persisted-state-evolution.md` in the mulga repo), which is not accepted.
It records what the code does today and which evidence exists for choosing and fencing a guest-side delivery executor; it proposes no design.
Paths are relative to `spinifex/` unless they start with `scripts/`, `images/`, `contracts/` or `tests/`, which are relative to the spinifex repo root.

## 1. Which guest process delivers add-ons

| Item | Present behaviour | Evidence |
|---|---|---|
| Delivery process | `mulga-eks-addon-sync` (shell loop): fetch staged set, render each bundle into `/var/lib/rancher/k3s/server/manifests/spinifex-addon-<name>.yaml`, report a phase per add-on, GC rendered files whose add-on is no longer staged with `kubectl delete --wait=false`. | `scripts/images/eks-node/mulga-eks-addon-sync.sh:341-405` (sync), `:80-254` (render), `:383-404` (GC) |
| Loop and cadence | Every `ADDON_SYNC_INTERVAL` (30 s) forever; one-shot when unset. | `mulga-eks-addon-sync.sh:409-417`, `mulga-eks-addon-sync.initd:18`, `images/mkosi.profiles/eks-server/mkosi.extra/etc/systemd/system/mulga-eks-addon-sync.service:13` |
| Fetch failure | Logs and keeps the current render; no GC runs that tick, so already-rendered files stay in the auto-deploy dir. | `mulga-eks-addon-sync.sh:344-348` |
| Report | `printf` JSON `{addon,version,phase,message,ts}` piped to `eks-gateway-publish -channel addon`; failure ignored. No executor identity, instance ID or epoch in the payload. | `mulga-eks-addon-sync.sh:68-73`, `contracts/eks/v1/addon_status.go:31-37` |
| Local state | Per-add-on sentinels in `/tmp` (`.addon-firstapply-*`, `.addon-diag-*`) and webhook certs in `/var/lib/spinifex-eks/webhook-certs`; no record of which generation or executor rendered a file. | `mulga-eks-addon-sync.sh:47`, `:279-293`, `:326-328` |
| Credentials | Sources `/etc/spinifex-eks/first-boot.env`; signs with `EKS_ACCESS_KEY`/`EKS_SECRET_KEY` when present, else the SDK chain reads IMDS instance-role credentials. | `mulga-eks-addon-sync.sh:19-33`, `handlers/eks/k3s_server_vm.go:549-557` |
| OpenRC unit | `/etc/init.d/mulga-eks-addon-sync`, `after k3s`, refuses to start without `first-boot.env`. | `scripts/images/eks-node/mulga-eks-addon-sync.initd:1-30`, installed by `scripts/images/eks-node/manifest.conf:43-44` |
| systemd unit | `mulga-eks-addon-sync.service`, `After=k3s.service`, `WantedBy=multi-user.target`; baked disabled by preset. | `images/mkosi.profiles/eks-server/mkosi.extra/etc/systemd/system/mulga-eks-addon-sync.service:1-17`, `images/mkosi.profiles/eks-server/mkosi.extra/usr/lib/systemd/system-preset/40-mulga-eks-role.preset:73` |
| Bake-time enablement | OpenRC image enables only `crond eks-node-role mulga-ebs-byid mulga-eks-provider-id mulga-vpc-mtu`; systemd image enables only `eks-node-role` and the snapshot/otel units. | `scripts/images/eks-node/manifest.conf:83`, `40-mulga-eks-role.preset:59-75` |

## 2. How a VM decides it is the primary

| Item | Present behaviour | Evidence |
|---|---|---|
| Role source | Host renders `SPINIFEX_K3S_ROLE=server` when `ServerURL` is empty and `server-join` otherwise; workers get `agent`. | `handlers/eks/k3s_server_vm.go:527-545`, `handlers/eks/nodegroup_userdata.go:64` |
| Who gets `server` | Create: the first VM of the spread (`nodes[0]`) or the single CP; every other spread member gets `ServerURL` and so `server-join`. | `handlers/eks/k3s_ha_control_plane.go:290-323`, `:272-282`; test `k3s_ha_control_plane_test.go` `TestPlaceControlPlane_SpreadFirstInitsRestJoin` |
| Selector | `eks-node-role.sh` runs once at first boot, writes `/etc/spinifex-eks/role`, enables and starts role services, then disables itself; a later boot exits early if the role file exists. | `scripts/images/eks-node/eks-node-role.sh:90-96`, `:118-122`, `:177` |
| Primary-only service | Only the `server` branch enables and starts `mulga-eks-addon-sync` (and `k3s-first-boot`); `server-join` does not. | `eks-node-role.sh:124-150` vs `:151-166`; `scripts/images/eks-node/eks-node-role_test.sh:129`, `:162` |
| Primacy is static | "Primary" is a first-boot, per-VM, irreversible choice; nothing in the guest re-evaluates it, and no kube Lease, k3s leader or etcd leader is consulted. | `eks-node-role.sh:90-96`; no lease/leader reference in `scripts/images/eks-node/*.sh` |
| Can a non-primary run it | Not by any code path read: `server-join` never enables it, the preset and OpenRC runlevel bake it disabled. Only manual enablement inside the VM would start a second instance, and the gateway would admit it (Section 4). | `eks-node-role.sh:151-166`, `40-mulga-eks-role.preset:73`, `manifest.conf:83` |
| K3s auto-deploy in HA | Each K3s server watches its own local `server/manifests` dir; whether the auto-deploy controller applies on every server or only on the elected K3s leader is unverified. If it runs only on the leader, a primary that is not the K3s leader would not apply. | not verified; `mulga-eks-addon-sync.sh:261-273` relies on the `Addon` CR the controller creates |

## 3. Control-plane membership on the host

| Item | Present behaviour | Evidence |
|---|---|---|
| Record | `ClusterMeta` at `clusters/{name}/meta` in `eks-account-{account}` (history 1). | `handlers/eks/store.go:28-40`, `handlers/eks/cluster_state.go:64-201` |
| Member list | `ControlPlaneNodes []ControlPlaneNode{NodeID, InstanceID, ENIID, ENIIP, MgmtIP}`; scalar `ControlPlaneInstanceID` etc. mirror `[0]`. No role, boot incarnation, executor or epoch field. | `cluster_state.go:109-125`, `:203-212` |
| "Primary" in meta | Index `[0]` by comment and by the create path; the scalars mirror it. | `cluster_state.go:119-124`, `handlers/eks/service_impl.go:941-949` |
| Membership predicate | `IsControlPlaneMember(instanceID)`: any entry in `ControlPlaneNodes` or the scalar. | `cluster_state.go:238-251` |
| Template | `ControlPlaneTemplate` persisted with `ServerURL`, creds and per-node fields cleared, replayed for replacements. | `cluster_state.go:128-133`, `service_impl.go:951-963` |
| Meta revision | Written by every `casUpdateMeta` caller, including health and node-count changes, so its KV revision is not a membership counter. | `cluster_state.go:276`, `:416-436`, `cluster_reconciler.go:620`, `:635` |

## 4. Gateway authentication of the internal routes

| Item | Present behaviour | Evidence |
|---|---|---|
| CP principal | Assumed-role session of role `spinifex-eks-server` in the system account; IMDS mints it with `RoleSessionName` = instance ID. | `handlers/eks/k3s_server_role.go:14`, `handlers/sts/assume_role.go:136-161`, `domains/ec2/guestmetadata/metadata.go:476-511` |
| Role policy | `eks:PublishInternal`, `eks:ListInternalAddons`, `eks:WebhookTokenReview`, `eks:GetRecoveryDirective` on `Resource: "*"`; one role shared by every cluster's CP VMs. | `k3s_server_role.go:27`, `:42-43` |
| Caller mapping | `eksCaller`: `SessionName` from `ctxIdentity`, role name from the underlying role ARN, not the session name. | `gateway/eks.go:307-320` |
| `AuthorizeInternal` | Requires the CP principal class, then `meta.IsControlPlaneMember(caller.SessionName)` for the named cluster; `GetRecoveryDirective` additionally requires the path instance ID to equal the caller's. | `gateway/eks/internal_authz.go:44-96`; tests `gateway/eks_authz_test.go` `TestEKSRequest_InternalRoutesAdmitTheCPAgentPrincipal`, `gateway/eks/internal_authz_test.go` `TestAuthorizeInternal_RejectsClusterTheCallerDoesNotServe` |
| Which routes it gates | Only actions scoped `sourceInternalCluster`: `ListInternalAddons` and `GetRecoveryDirective`. `PublishInternal` and `WebhookTokenReview` are `sourceBodyAccountCluster`, so `IsInternalAction` is false and only the IAM policy check applies. | `gateway/eks/authz.go:52-60`, `gateway/eks/internal_authz.go:37-39`, `gateway/eks.go:216-220`, `:248` |
| Member vs primary | The gateway can tell members apart (instance ID) and bind a fetch to a cluster, but cannot identify the delivery executor: meta has no role field, and `[0]` stops being the `server`-role VM after `[0]` is replaced (Section 6). | `internal_authz.go:74`, `cluster_state.go:463-490` |
| Static-cred fallback | When the instance profile cannot be ensured, the VM gets the shared system access key; it has no instance identity, so `ListInternalAddons` is denied for the VM's life while `PublishInternal` remains policy-gated only. | `k3s_server_role.go:30-50`, `k3s_ha_control_plane.go:161-166`, `k3s_server_vm.go:549-557` |
| Handler-side identity | `ListStagedAddonManifests` receives only the cluster name; no caller identity reaches the daemon. | `gateway/eks/internal_addons.go:20-33`, `handlers/eks/addons.go:117-128`, `daemon/daemon.go:1116` |

## 5. Status relay path

| Hop | Present behaviour | Evidence |
|---|---|---|
| Agent | `eks-gateway-publish` wraps `{accountId, channel, kind, payload}` and POSTs `/clusters/{cluster}/internal-publish`, 30 attempts. | `agents/eks/gatewaypublish/publish.go:29`, `:57-114` |
| Gateway authz | IAM policy against `arn:aws:eks:{region}:{bodyAccount}:cluster/{name}`; with the role's `Resource: "*"`, any CP-role session of any cluster can publish to any cluster's add-on subject. | `gateway/eks/authz.go:155-160`, `k3s_server_role.go:27` |
| Relay | `PublishInternal` publishes the raw payload to core NATS `eks.addon.{account}.{cluster}.status`; only a trace header is added. No caller instance ID, session or epoch is attached. | `gateway/eks/internal_publish.go:48-91`, `contracts/eks/v1/addon_status.go:10-12` |
| Subscriber | Lease holder's `ClusterReconciler.Run` subscribes plainly; decodes the report and calls `Owner.ApplyReport` with addon, phase and message only (version and ts dropped). | `handlers/eks/cluster_reconciler.go:447-461`, `handlers/eks/addon_status_reconciler.go:22-31` |
| Apply | Revision CAS on the add-on record; no check of sender, membership, version or epoch. | `domains/eks/addon/owner.go:140-168`, `domains/eks/addon/store.go:198` |
| State reports | `mulga-eks-state-report` runs on both server roles and uses the same unbound relay; the report carries no member identity and the reconciler keeps the newest. | `eks-node-role.sh:137`, `:160`, `handlers/eks/state_report.go:24-42`, `cluster_reconciler.go:535` |

## 6. Primary failure, replacement and rebuild today

| Event | What happens to add-on delivery | Evidence |
|---|---|---|
| Primary stopped or errored | Reconciler restarts it in place after `restartGrace` (same instance ID, same root volume); role file and enabled units persist, so `mulga-eks-addon-sync` resumes with its rendered files. | `cluster_reconciler.go:708-771`, `:927-937` |
| Etcd reform (HA, all VMs running) | `members[0]` gets `cluster-reset`, others `wipe-rejoin`; members are stopped and restarted. Wipe removes only `server/db/etcd`, so rendered manifests survive. | `cluster_reconciler.go:780-880`, `scripts/images/eks-node/mulga-eks-k3s-recovery.sh:38`, `:183-186` |
| HA member lost (terminated, gone, or describe error) | `ProvisionReplacementCP` launches with `ServerURL` set, so the replacement is `server-join` and never runs `mulga-eks-addon-sync`; `SwapControlPlaneMember` puts it at the dead member's index. | `cluster_reconciler.go:1010-1096`, `k3s_ha_control_plane.go:142-187`, `cluster_state.go:463-490` |
| HA primary (`[0]`) lost | Same path: after the swap, `[0]` is a `server-join` VM and no member runs delivery. Staged manifests stay in KV, but nothing fetches, renders, GCs or reports until an operator intervenes; add-ons keep their last status. | as above; no other enable path for the service |
| Describe error treated as lost | A member whose state query fails counts as lost and can be replaced after `replaceGrace` while still running; if it was the primary, it keeps rendering and reporting (Section 7). | `cluster_reconciler.go:982-1007`; test `cluster_reconciler_replace_test.go` `TestMaybeReplaceCP_DescribeErrorMemberTreatedAsLost` |
| Single-CP total loss | Operator `RestoreSnapshot` launches a fresh `server` VM (`ServerURL` empty), so delivery resumes on the new VM from the KV-staged set; meta is committed before the old CP is terminated, and termination is retried 3 times. | `handlers/eks/restore_snapshot.go:217-353`, `k3s_ha_control_plane.go:206-246`, `restore_snapshot.go:28`, `:185-212` |
| Cluster create timeout or bootstrap failure | Marked `FAILED` without purging; CP VMs keep running `mulga-eks-addon-sync`, `ListInternalAddons` still admits them (membership only), and the reconciler has exited, so reports are dropped. | `cluster_reconciler.go:1151-1167`, `service_impl.go:2138`, `cluster_reconciler.go:645-648` |
| Async launch failure | `failClusterLaunch` purges infra (terminates CP VMs) and marks `FAILED`, keeping meta. | `service_impl.go:701-717` |
| Reclaim of a `FAILED` name | CAS `FAILED`→`CREATING`, purge old infra, overwrite meta; add-on keys are not purged, so the new primary renders the inherited staged set. | `service_impl.go:1203-1261` |
| Delete | `purgeClusterInfra` stops the reconciler and terminates the CP VMs. | `service_impl.go:1271-1272` |
| Resume from durable state | A fresh `server` VM resumes because the staged set is durable KV and the listing has no per-executor cursor; a `server-join` replacement cannot resume because it never runs the agent. | `handlers/eks/addons.go:117-128`, `eks-node-role.sh:151-166` |

## 7. Existing leases, epochs and generations

| Mechanism | Scope | What it proves | Evidence |
|---|---|---|---|
| ClusterReconciler lease | Host daemon per cluster; key `{account}/{cluster}` in `spinifex-eks-leader`, value = daemon `HolderID`, CAS create, renew by revision CAS every 20 s, bucket TTL 60 s, `Lost()` ends `Run`. | One daemon reconciles at a time. The revision is private (no accessor) and is not passed to report apply or guest routes. Unrelated to the guest executor. | `cluster_reconciler.go:26`, `:391-418`, `:463-470`; `foundation/state/kvlease/lease.go:96-178`, `:201-225`; `store.go:38-40` |
| Lease takeover | `SpawnRegisteredReconcilers` runs only at daemon start; a lease loser is not retried elsewhere in the code read. | Reports for a cluster whose lease holder died are dropped until some daemon restarts. | `daemon/daemon.go:2209`, `service_impl.go:385-428`, `handlers/eks/reconciler_registry.go:12-14`, `:166-171` |
| Recovery directive epoch | Per member instance, `epoch = prev+1` on each set; guest applies once and records the applied epoch locally. | At-most-once boot-time directive. Not a fence: it is pulled only at boot and has no `disable delivery` action. | `handlers/eks/recovery_directive.go:30-43`, `:90-107`; `mulga-eks-k3s-recovery.sh:131-197` |
| Per-cluster epoch or generation | None in `ClusterMeta`; add-on records have no generation. | Nothing. | `cluster_state.go:64-201`, `docs/package-boundary/eks-addon-lifecycle.md` row "Generation" |
| K3s server roles | `server` (cluster-init) and `server-join`; fixed at first boot. | Identifies the delivery VM only until `[0]` is replaced. | `k3s_server_vm.go:527-531`, `:585-589` |
| Etcd / kube leader election | K3s embedded etcd and kube controllers elect internally; nothing in Spinifex reads them. | Not used. | no reference in `scripts/images/eks-node/*.sh` or `handlers/eks` |

## 8. Candidate fencing evidence available today

"Fences" means the old executor provably cannot render or apply, and cannot have its reports accepted.

| Evidence | Available how | Proves old executor cannot render/apply? | Gap and what would be needed |
|---|---|---|---|
| EC2 state `terminated` or instance not found | `cpControlAdapter.InstanceState` (system account `DescribeInstances`); `TerminateK3sServerVM` treats not-found as success. | Yes for render and apply once the VM is gone, if the instance record is authoritative; host-partition behaviour of the state record is unverified. | Reports already queued or retried by `eks-gateway-publish` are still accepted. Needs: state proven authoritative under host loss, and report acceptance bound to the current executor. `service_impl.go:153-170`, `k3s_server_vm.go:292-330` |
| EC2 state `stopped` / `error` | Same query. | Only momentarily: the reconciler's restart ladder starts stopped/error members automatically, and the VM resumes delivery with the same identity. | Needs a stop that nothing restarts, or a guest-side disable that survives restart. `cluster_reconciler.go:708-771`, `:927-937` |
| Describe error | Treated as lost. | No: the VM may be running. | Must not count as fencing evidence. `cluster_reconciler.go:982-1007` |
| Removal from `ControlPlaneNodes` | `SwapControlPlaneMember`, `replaceControlPlaneForRestore`. | Partly: `ListInternalAddons` is denied, but the agent keeps its current render on fetch failure, K3s keeps applying those files, and `PublishInternal` still accepts its reports. | Needs: publish gated by membership/executor and identity attached on relay; a guest rule that a denied fetch stops delivery. `internal_authz.go:74`, `mulga-eks-addon-sync.sh:344-348` |
| Instance ID / incarnation | Session name = instance ID; stable across stop/start. | No: there is no per-boot incarnation, so a restarted VM is indistinguishable from before. | Needs an executor epoch recorded host-side and checked per request; instance ID alone identifies, it does not fence. `assume_role.go:136-161` |
| KV revisions | Meta revision, add-on record revision, lease revision. | No: meta revision churns with health writes, add-on CAS ignores sender, lease revision is host-only and private. | Needs a dedicated, monotonic executor epoch written only on reassignment. `cluster_state.go:416-436`, `kvlease/lease.go:44-52` |
| IMDS credential expiry | Instance-role creds last 3600 s, re-minted 5 min early. | No: up to an hour of validity after the last mint, and a running VM keeps re-minting. | Not a fence on its own. `domains/ec2/guestmetadata/credentials.go:14-20` |
| Credential revocation | `RevokeRoleSessions` deletes sessions by role (called on role delete); no per-instance revocation. | No per-executor effect: the role is shared by every CP VM of every cluster. | Needs a per-session-name revocation, and proof the gateway rejects a revoked session; a running VM would still re-mint via IMDS unless IMDS also refuses it. `handlers/sts/session_credentials.go:212-219`, `handlers/iam/roles.go:221` |
| Static system keys | Shared system access key on fallback. | No: shared, unrevocable per VM, no identity. | Fallback VMs cannot be executors under any identity-based rule. `k3s_server_role.go:30-50` |
| K3s node removal / etcd member prune | `EKS_ETCD_PRUNE_PEER_IP` handled in `k3s-first-boot.sh`. | Would stop apply through the shared datastore if the member is removed, but not render to the VM's own disk. The prune path appears unreachable: the variable is set only on replacements, which are `server-join`, and `k3s-first-boot` runs only for `server`. | Needs a reachable prune, verification that a removed member's apiserver cannot write, and still the report gate. `k3s-first-boot.sh:165-187`, `k3s_ha_control_plane.go:153`, `eks-node-role.sh:136`, `:151-166` |
| Guest service disable | None remotely; the only host-to-guest instruction channel is the boot-time recovery directive. | No. | Needs an online directive the agent polls, plus an acknowledgement the host can verify; an acknowledgement from the old executor proves only that it was alive to send it. `recovery_directive.go:12-28` |
| Restore fence | `confirmOldCPTerminated` after meta commit. | Yes for the old VM once terminate succeeds, but it runs after the new VM was launched and committed. | Ordering is successor-first; R6 requires fence-first. `restore_snapshot.go:262-336` |

## 9. Evidence each design option would need

No option is chosen here; each row lists only what it would rely on.

| Option | Evidence it needs that does not exist today |
|---|---|
| Host-assigned executor `{instanceId, epoch}` on the cluster record | An executor field and epoch advanced by CAS; `AuthorizeInternal`-style gating on `PublishInternal` (today policy-only); the gateway attaching the caller instance ID (and epoch) to the relayed message; report apply checking it; a reassignment trigger that does not fire on describe errors; fence evidence from Section 8 rows that say "Yes". |
| Executor follows meta `[0]` | A guest that can become executor after first boot (today role is fixed and replacements are `server-join`); the same gateway and relay identity checks; `[0]` reassignment tied to fencing of the old `[0]`. |
| Run the agent on every server with guest-side leader election (kube Lease) | Host-visible proof of the guest leader (the host cannot read the kube Lease today); verification of K3s auto-deploy behaviour across servers; reports still need gateway-attached identity, since a partitioned old leader can still publish. |
| Disable-on-handover directive | An online directive channel and agent polling; a rule that a fetch denial or missing directive stops rendering; an acknowledgement or instance-state proof before the successor acts. |
| Credential-based fence | Per-instance session revocation, IMDS refusing to re-mint for a fenced instance, and gateway rejection of revoked sessions; still insufficient for files already rendered to the old VM's disk. |

## 10. Gaps

1. Delivery runs only on the first-boot `server` VM; replacing HA `[0]` leaves no member running `mulga-eks-addon-sync`, with no detection or recovery.
2. `PublishInternal` is not gated by `AuthorizeInternal`; with the CP role's `Resource: "*"`, any CP-role session can publish add-on (and state) reports for any cluster and account.
3. The relay attaches no caller identity, and the payload carries none, so the owner cannot tell which member, executor or epoch produced a report; this conflicts with the direction in draft PROP-SPINIFEXARCH-007 S3, which is not accepted.
4. `ClusterMeta` has no role, executor or epoch field; `[0]` as "primary" is a convention that diverges from the guest role after a swap.
5. A member removed from meta is denied fetches but keeps applying its last render and keeps having its reports accepted.
6. Describe errors count as member loss, so a running primary can be replaced without fencing.
7. The restore path launches and commits the successor before fencing the old CP.
8. No per-instance credential revocation; the CP role and instance profile are shared across all clusters.
9. `FAILED` clusters (create timeout, bootstrap failure) keep running CP VMs whose agent is still admitted while reports are dropped.
10. The etcd peer prune for replacements appears unreachable (`server-join` does not run `k3s-first-boot`); not verified live.
11. Whether K3s auto-deploy applies from a non-leader server's manifests dir is unverified, which bears on any multi-server option.
12. No lease takeover for a cluster whose reconciler holder died until a daemon restarts, so reports are dropped meanwhile.
