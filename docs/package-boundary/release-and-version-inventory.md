# Release, Version and Upgrade Mechanism Inventory

Code read at `3a55e2ee1`; not verified live.

This is a read-only inventory of the release-membership, rolling-upgrade, rollback, schema-version and guest-image mechanisms that exist today, measured against what the draft Prepare → Activate → Migrate → Finalise sequence and N/N-1 negotiation would need.
The draft proposal (`docs/development/proposals/spinifex-architecture/0007-…`, unaccepted) and Q-75/Q-76 in `docs/specs/requirements/resource-lifecycle.md` are the yardstick only; nothing here treats them as decided.
The open question it answers is the EKS add-on design's question 4 (`eks-addon-lifecycle-design.md:445`): is there an existing Region-wide release gate that can establish "every daemon runs the new release"?
**Short answer: no.**

Path convention: paths without a prefix are relative to the `spinifex` repo root; `mulga/`, `predastore/`, `viperblock/` and `ansible/` paths are relative to the umbrella repo.
Shell scripts, Ansible and docs were read, not run.

## 1. How a node advertises its build version

| Mechanism | Evidence | What it carries | Cluster-visible? |
|---|---|---|---|
| Build-time ldflags | `Makefile:60-62` | `git describe --tags --always --dirty` and short commit into `operator/cli.Version`/`Commit` | No; baked into the binary |
| `spx version` | `spinifex/operator/cli/version.go:12-22` | Prints `Version (Commit) os/arch` of the local binary | No; local process only |
| Gateway `GetVersion` | `spinifex/operator/cli/service.go:613`, `spinifex/runtime/roles/awsgw/awsgw.go:58-71,487-488`, `spinifex/gateway/spinifex.go:61-62`, `spinifex/gateway/spx/spx.go:27-39` | Version of whichever awsgw process answered the request | Only for the answering gateway; no per-node fan-out |
| Daemon heartbeat (KV `spinifex-cluster-state`, key `heartbeat.<node>`) | `spinifex/daemon/jetstream.go:224-236,247`, `spinifex/daemon/heartbeat.go:56-72` | Node, config `Epoch`, services, capacity | Yes, but **no build version field** |
| Node health (`spinifex.admin.<node>.health`, HTTPS `/health`) | `contracts/cluster/v1/health.go:15-24` | `ConfigHash`, `Epoch`, uptime, services | Yes, **no build version** |
| Node status fan-out (`spinifex.node.status`, `spx get nodes`) | `contracts/cluster/v1/status.go:34-68`, `spinifex/gateway/spx/spx.go:47-60` | Capacity, roles, GPUs | Yes, **no build version** |
| Node discovery (`spinifex.nodes.discover`) | `contracts/cluster/v1/discovery.go:14-16` | Node name only | Yes, no version |
| Formation join | `spinifex/runtime/formation/formation.go:25-37,70-80` | Name, IPs, region/AZ, services | Join exchange has **no version or protocol field** |
| `spinifex.toml` `version` / `epoch` | `spinifex/operator/cli/templates/spinifex.toml:2-3`, `spinifex/bootstrap/config/config.go:19-21`, `spinifex/daemon/daemon.go:2743-2757` | Config-format version ("4") and leader-bumped config epoch, hashed into `ConfigHash` | Indirectly via `ConfigHash`; this is config agreement, not binary version |
| Deploy manifest file | `mulga/scripts/update-nodes.sh:165-189,320-321` | Five sub-repo describe strings, written to `/etc/spinifex/deploy-manifest.txt` and the operator's `~/.spinifex/deploy-manifests/` | No; a file on disk, only written by this dev script |

**Finding:** there is no cluster-wide record of which release each member runs.
Every version observation is either local (`spx version`, deploy manifest) or names one process (`GetVersion`).
The only cross-node comparison is done by shell scripts over SSH (Section 2).

## 2. How upgrades are performed today

| Path | Evidence | Ordering | Drain | Health gate | Version gate |
|---|---|---|---|---|---|
| Online installer re-run (`curl … \| bash`) | `docs/admin/update/README.md:31-53`, `scripts/setup.sh:828-847,1187-1194` | Per node, operator-driven; no cluster coordination | Whatever `spinifex.target` stop does | None in the script beyond service restart | Optional `--version` pin, SHA-256 verified; no peer comparison |
| Manual: install binary, `spx admin upgrade`, restart | `docs/admin/update/README.md:64-100`, `spinifex/operator/cli/admin_upgrade.go:55-120` | Per node | None | None | None; applies local config migrations and systemd unit reconcile only |
| `mulga/scripts/update-nodes.sh` (dev/test) | `mulga/scripts/update-nodes.sh:29-38,218-243,329-348,359-377` | **Stop-the-world**: `spx admin cluster shutdown`, update all, start all | Coordinated GATE→DRAIN→STORAGE→PERSIST→INFRA with all-node ACKs | Retries `spx get nodes` until no `NotReady` | Prints `spx version` per host after the fact; header states mixed NATS/predastore/OVN versions are deliberately avoided |
| Ansible `cluster-deploy.yml` (dev clusters) | `ansible/playbooks/cluster-deploy.yml:1-37`, `ansible/roles/deploy/tasks/main.yml:97-109,372,390,443-459,516-534` | **Rolling** `serial: 1`, `max_fail_percentage: 0` | `systemctl stop spinifex.target` (drain via `spinifex-shutdown.service`) | awsgw port, target active, predastore Raft quorum rejoin, stale-inode restart | None; no `spx admin upgrade` step, so the cluster runs mixed builds mid-roll with no compatibility check |
| `spinifex/scripts/install-node.sh` (formation) | `scripts/install-node.sh:9-11,286-304,347-354` | n/a; forms a cluster | n/a | Smoke test | Refuses to form if `spx version` strings differ (exact match, SSH) |
| Coordinated shutdown | `spinifex/operator/cli/admin.go:105-112`, `spinifex/daemon/shutdown.go:29-45` | Phase order with ACKs | Yes | ACK per phase | None |
| Local drain | `spinifex/operator/cli/admin.go:120-133` | Local node | GATE+DRAIN only | n/a | None |
| `spx admin preflight` | `spinifex/operator/cli/admin_preflight.go:15-28`, `docs/admin/update/README.md:156-183` | Local | n/a | Detects a unit still executing a replaced `spx` inode | Local only; not a peer comparison |

The two multi-node paths disagree: `update-nodes.sh` avoids mixed versions by stopping the cluster, while `cluster-deploy.yml` rolls one node at a time and so runs N and N-1 together with nothing proving they interoperate.
A known mixed-version hazard is already documented: an older daemon ignores the `Target` field of `ShutdownRequest` and treats a single-node drain as cluster-wide (`spinifex/daemon/shutdown.go:34-37`, `docs/admin/host-lifecycle/README.md:123`).
RFC2-18 (`mulga/docs/specs/spinifex/rfc2-18_operator-lifecycle.md:34-53,88-104,165-177`, DRAFTING) records the same observations and states that after-the-fact version printing is not a compatibility gate.

## 3. Feature flags, capability negotiation, minimum-version gates

| Mechanism | Evidence | Scope | Shape |
|---|---|---|---|
| EBS provider capabilities | `spinifex/providers/ebs/provider.go:55-70`, `spinifex/providers/ebs/viperblock/provider_handlers.go:236-262`, `spinifex/daemon/daemon.go:2397` | Spinifex ↔ block provider | Boolean feature set fetched at runtime; the only real capability exchange found |
| EBS provider wire version | `spinifex/providers/ebs/provider.go:13-25`, `spinifex/providers/ebs/errors.go:90-95`, `spinifex/providers/ebs/nats.go:212-316` | Every provider request/response | **Exact match** (`!= SchemaVersion` → `ErrUnsupportedVersion`); no N-1 window |
| RDS data-volume contract image tag | `scripts/images/rds-postgres/manifest.conf:49-51`, `scripts/images/rds-mariadb/manifest.conf:59`, `spinifex/handlers/rds/launch.go:39-45,532`, `spinifex/operator/imagecatalog/images.go:666,686` | Control plane → guest image | AMI filter `rds-data-volume-contract=format-auth-v1` excludes older images; a static image-tag capability, not a runtime proof |
| RDS bootstrap envelope | `spinifex/handlers/rds/bootstrap.go:23-36,190-192,212` | Host ↔ RDS guest | `envelopeVersion` exact match |
| KV schema-ahead refusal | `spinifex/foundation/state/migrate/kv.go:55-67,87-89` | Any process opening a versioned bucket | Older build refuses to open a bucket stamped newer; fail-closed, not negotiation |
| systemd unit marker | `spinifex/operator/host/systemd/reconcile.go:28-33,131-140`, `spinifex/operator/host/systemd/units_gen.go` | Host units | `# spinifex-unit-version: N`; replace only when older |

Searches for `featuregate`, `feature_flag`, `minVersion` (non-TLS), `minimumVersion`, `semver` and version handshakes in Spinifex found nothing else.
There is no "all members at version ≥ X" check, no release gate record and no cluster capability set.

## 4. Persisted-state versions, migrations and rollback

### 4.1 KV bucket schema versions

| Item | Evidence | Notes |
|---|---|---|
| Version stamp | `spinifex/foundation/state/kvutil/state.go:3-4`, `spinifex/foundation/state/kvutil/kvutil.go:211-244` | `_version` key per bucket; `WriteVersion` is read-then-unconditional-put, not CAS |
| Migration runner | `spinifex/foundation/state/migrate/kv.go:17-137` | Chain validation, run, stamp after each step; runs at bucket-open time in whichever process opens it |
| Bucket-level versions | e.g. `spinifex/daemon/jetstream.go:53-55,139,154,187`, `spinifex/domains/ec2/vpc/service_impl.go:42-44,171-211`, `spinifex/handlers/rds/store.go:27-38,184,209` | About 40 constants, almost all `1`; IPAM, security groups and static pool are at `2` with **no registered migration** (`spinifex/domains/ec2/vpc/ipam.go:17`, `spinifex/domains/ec2/vpc/security_group.go:128`, `spinifex/domains/network/external/static_pool.go:22`) |
| Registered migrations | `spinifex/daemon/instance_records_migrate.go:31-72`, `spinifex/migrate/VERSIONS.md` | Only `spinifex-instance-state` (1→5) and `spinifex-terminated-instances` (1→3) |
| Catalogue | `spinifex/migrate/VERSIONS.md` | Lists three KV buckets; the other constants are not catalogued |

A bucket stamped `1` whose target became `2` with no registered step fails `RunKV` with "no migrations registered" (`spinifex/foundation/state/migrate/kv.go:110-111`); `VERSIONS.md` says those installs required `spx admin init --force`, which is a hard cutover with no documented Q-75 exception record.

### 4.2 Expand/contract precedent

The instance-record crossing is the only expand–migrate–contract precedent found.
`VERSIONS.md` states version 5 of `spinifex-instance-state` froze the legacy `instance.<id>` and `node.<id>` keys and left them in place so the crossing can be rolled back, and the tests cover idempotent re-run and not overwriting fresher records (`spinifex/daemon/instance_records_transition_test.go:221-315`).
It is not a full Prepare→Activate→Finalise sequence: activation happens when the first upgraded process opens the bucket, the bump itself makes the older build refuse to start (`SchemaAheadError`), and there is no operator finalisation step that deletes the frozen keys.
So "rollback" means a binary downgrade is refused rather than supported; it would need the version stamp to be reset by hand (not documented).

### 4.3 Record- and file-level schema versions

| Format | Evidence | Rule |
|---|---|---|
| EBS volume/snapshot/AMI metadata documents | `spinifex/domains/ec2/ebs/metadata/metadata.go:14-17,166-221` | `schema_version` = 2; decode rejects anything `!= 2` as corrupt, so N-1 cannot read N's documents and vice versa |
| Node local state (disk + KV `node.<id>`) | `spinifex/daemon/local_state.go:14-34,131-133` | Exact match `1` |
| Config files | `spinifex/migrate/VERSIONS.md`, `spinifex/migrate/migrate.go:191-246,327-332` | Per-file version; `RunConfig` backs up before each step; no config migrations registered |
| Object-store migrations | `spinifex/migrate/migrate.go:126-161`, `spinifex/migrate/VERSIONS.md` | Stamp in a KV bucket, read-then-write; none registered |
| Predastore cluster config | `predastore/internal/config/config.go:25-28,358` | `Version = 1`, exact match, "no migration between versions" |
| Predastore placement record | `predastore/internal/gate/handlers/placement_record.go:17-48,85,125-139` | Readers accept v1–v3 and reject unknown; writer always emits v3 (not "oldest compatible") |
| Viperblock WAL/chunk/checkpoint headers | `viperblock/viperblock/snapshot.go:346-349`, `viperblock/viperblock/viperblock.go:3622,4788,6532` | Magic + `Version` uint16, exact match |
| Viperblock flat-section version byte | `viperblock/viperblock/snapshot.go:634,726-729` | Reserved, not checked |

No resource record carries a per-record `schemaVersion` today; the EKS add-on design proposes adding one (`eks-addon-lifecycle-design.md:119,421`).

### 4.4 Rollback

No rollback command, procedure or test exists for the platform release.
`spx admin upgrade` has no reverse mode (`spinifex/operator/cli/admin_upgrade.go:55-71`); a downgraded unit marker is reported as a conflict rather than restored (`spinifex/operator/host/systemd/reconcile.go:133-140`); config backups are files, not managed restore points (RFC2-18 C-127 at `mulga/docs/specs/spinifex/rfc2-18_operator-lifecycle.md:97-104`).
The update runbook covers forward upgrade and stale-binary troubleshooting only (`docs/admin/update/README.md:31-213`).

## 5. Guest images and agents

| Guest | Image version source | Agent version | Control plane records | Upgrade path |
|---|---|---|---|---|
| EKS node (CP and worker) | `scripts/images/eks-node/manifest.conf:2,4,25,87-88`, `scripts/images/eks-node/setup.sh:16` (K3s `v1.32.5+k3s1`), catalogue `spinifex/operator/imagecatalog/images.go:576-590` (Version = Alpine `3.21.7`, mutable URL, sidecar SHA-256) | Guest binaries built without version ldflags; no guest reports one | `ClusterMeta.Version` = requested Kubernetes version, default `1.32`, not validated against the image (`spinifex/handlers/eks/service_impl.go:291,587`); `ControlPlaneInstanceID` (`spinifex/handlers/eks/cluster_state.go:109`); nodegroup `ReleaseVersion` is a fixed placeholder `1.0.0-spinifex-eks-node` (`spinifex/handlers/eks/nodegroup.go:43-46,296-299`); `PlatformVersion` is constant `eks.1` (`spinifex/handlers/eks/service_impl.go:2188-2191`) | None: `UpdateClusterVersion`, `UpdateNodegroupVersion`, `ListUpdates` and `DescribeUpdate` have no gateway route at this revision (`spinifex/gateway/eks.go`, 31 routes, none match); plan only in `mulga/docs/development/feature/eks-cluster-version-upgrade.md` |
| EKS add-on bundles | Baked per version into the image under `addons/<name>/<version>` (`scripts/images/eks-node/mulga-eks-addon-sync.sh:38,81-87`) | Add-on status report carries add-on version only (`contracts/eks/v1/addon_status.go:31-37`) | Add-on record `addonVersion` | A new add-on version needs a new image; v1 guest protocol has no protocol version or capability field |
| ECS container instance | `scripts/images/ecs-agent/manifest.conf:4,23,40-41` | `version = "dev"` unless ldflags set (`cmd/ecs-agent/main.go:18-24`); `Makefile:78` sets it but the image manifest build does not, so imaged agents likely report `dev` (not verified live) | `AgentVersion` stored and echoed in `VersionInfo` (`spinifex/handlers/ecs/records.go:237`, `spinifex/handlers/ecs/instances.go:283,333`); never compared | Relaunch capacity; no agent-update path |
| RDS (postgres, mariadb) | `scripts/images/rds-postgres/manifest.conf:4,19,47-51`, `scripts/images/rds-mariadb/manifest.conf:4,21,55-59`, tags `engine`, `engine-version`, `rds-data-volume-contract` | `version = "dev"` default (`cmd/rds-agent/main.go:21-23,72`); image build sets no ldflags | `Agent.AgentVersion`, `EngineVersion` recorded (`spinifex/handlers/rds/records.go:405-415`, `spinifex/handlers/rds/agent.go:189`); never compared; AMI ID used at launch is logged, not stored on the DB record (`spinifex/handlers/rds/launch.go:189,267,353`) | `ModifyDBInstance EngineVersion` returns unimplemented (`spinifex/handlers/rds/modify.go:477-478`) |

AMI selection for EKS, RDS and ECS takes the newest image matching tags (`spinifex/handlers/eks/k3s_server_vm.go:353-363`, `spinifex/handlers/rds/launch.go:552-558`, `spinifex/handlers/ecs/capacity.go:108-119`), so importing a new image silently changes what new resources receive while existing ones keep the old one.
The control plane can find a guest's AMI only indirectly through the backing EC2 instance; it cannot learn the guest's protocol support from the guest.

## 6. Offline artefacts and backups

| Artefact | Evidence | Version field |
|---|---|---|
| EBS snapshot / AMI metadata | `spinifex/domains/ec2/ebs/metadata/metadata.go:57-58,79-80` | `schema_version` 2, exact match |
| Viperblock snapshot checkpoint | `viperblock/viperblock/snapshot.go:346-349` | Header magic + version, exact match |
| RDS DB snapshot record | `spinifex/handlers/rds/records.go:291-320` | `EngineVersion` only; no record schema version; data lives in an EC2 snapshot |
| EKS etcd backups (`eks-backups-system` bucket) | `spinifex/handlers/eks/restore_snapshot.go:19-22,53-75` | Name-encoded tier and timestamp only; no K3s/etcd version recorded |
| Account export | Not found | n/a |

No artefact declares a support or retirement window, and none records the Spinifex release that wrote it.

## 7. Gaps against Prepare → Activate → Migrate → Finalise and N/N-1

Stated as gaps only; no design is implied.

| # | Need | Current state | Gap |
|---|---|---|---|
| G1 | Authoritative record of each member's release | Heartbeat, health, status and formation carry no build version (Section 1) | No release-membership data source exists; activation evidence ("every writer can preserve the transitional form") cannot be produced |
| G2 | Region-wide activation gate | None; only `install-node.sh` exact string match over SSH at formation | Nothing operator-visible records "activated"; the EKS add-on slice 7 would have to build it |
| G3 | Finalisation step and preflight | Frozen instance keys are never retired; no finalise command | No operator-visible end of rollback for any change |
| G4 | Controlled rollback | Schema-ahead refusal blocks downgrade; no reverse migration, no unit downgrade, no runbook | Rollback is unsupported once any bucket is bumped |
| G5 | N/N-1 wire compatibility | Most contracts unversioned (C-9); versioned ones use exact match (EBS provider, RDS envelope, local state, EBS metadata, viperblock headers) | No contract implements a two-version window or "emit oldest compatible" |
| G6 | Capability exchange | Only EBS provider booleans and an RDS image tag | No authenticated per-peer or per-guest capability record; no invalidation on replacement |
| G7 | Fenced, owned migration | Migrations run on bucket open in any process; version stamp is read-then-put | No migration owner, fence or per-record progress; concurrent runners rely on idempotence |
| G8 | Per-record schema version | Bucket-level only | Lazy per-record migration under a resource owner has no field to key on |
| G9 | Coherent rolling procedure | Stop-the-world script vs `serial: 1` Ansible roll with no version or compatibility check; known mixed-version drain hazard | No supported rolling-upgrade contract with a stop-on-skew rule |
| G10 | Guest version visibility | Agent versions recorded but default `dev` in images and never compared; EKS guests report none | Control plane cannot tell a v1 from a v2-capable guest except by a protocol it does not yet have |
| G11 | Guest upgrade operations | EKS version/nodegroup updates unrouted; RDS engine upgrade unimplemented; newest-AMI-wins selection | No owned path to move a guest to a new image, and image choice is implicit |
| G12 | Release manifest | `update-nodes.sh` writes a dev-only text manifest; installer pins one tarball | No immutable release manifest binding binary, contract versions and supported source versions (RFC2-18 Q-183 target) |
| G13 | Artefact lifecycle | Snapshot/backup formats lack writer release and support window; etcd backups record no K3s version | Retained artefacts cannot be checked for usability before a format is retired |
| G14 | Version catalogue accuracy | `VERSIONS.md` lists 3 of about 40 KV buckets | No single inventory of persisted formats to drive a finalisation preflight |
