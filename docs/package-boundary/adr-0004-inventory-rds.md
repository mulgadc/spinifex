# ADR-0004 source inventory — RDS

Code-read inventory of `refactor/spinifex-package-boundaries` at `6c50349bd`.
It is not verified against a live system, and the branch is 59 commits behind `dev` at the time of writing, so recheck any file it cites before acting on it.
Items marked "verify" or "uncertain" are unconfirmed.
Gaps record observed behaviour, not target behaviour.


Repo: `/home/tf-user/Development/mulga/spinifex`, branch `refactor/spinifex-package-boundaries`, paths relative to `spinifex/`.
Inputs read: ADR-0001, ADR-0003, ADR-0004 and Q-78 from `origin/main`.
Method: static reading of production code only; nothing here was verified against a live cluster.
Line references are `file:line` at the time of reading.
"Uncertain" marks a claim inferred from code shape rather than traced end to end.

## 0. Scope findings

`daemon/instance_records.go` (217) and `daemon/instance_records_migrate.go` (361) are **not RDS**.
They own the EC2 per-instance record space `i.<id>` in the EC2 instance-state bucket (`daemon/instance_records.go:25`) and its migrations (`daemon/instance_records_migrate.go:29-70`), and import only `runtime/compute/vm`, `kvstore`, `kvutil`, `migrate`.
They are excluded from the RDS inventory and belong to the EC2 instance owner.

Other production files that wire or consume RDS, found by import search:

- `daemon/daemon.go` (RDS sections only: `:170-171`, `:1171-1206`, `:2046-2078`, `:2306-2312`, `:2374-2382`).
- `daemon/rds_deps.go`.
- `daemon/dns_reconcile.go:71-76` and `:99-104` (RDS bucket watch and RDS DNS desired set).
- `daemon/bedrock_deps.go:43` (Ochre/Bedrock borrows `handlers_rds.NewNATSVolumeAttacher`).
- `handlers/quota/live.go:82-93` (`EnforceRDSInstances`).
- `accountteardown/reapers_rds.go` (instance, subnet-group, parameter-group reapers).
- `gateway/rds.go`, `gateway/operations.go:43-45`, `gateway/gateway.go:302,525-526` (service map and dispatch).
- `bootstrap/config/config.go:338` (`RDSConfig`).
- Test-support (non-`_test.go` but not production): `tests/integration/service_daemon_lite.go:51` reflects every `Service` method onto `rds.<Method>`; `tests/e2e/harness/rds*.go`.
- Counterparty only: `agents/rds/agent/*.go` and `cmd/rds-agent` import `handlers/rds` for wire types.

## 1. File table

Total in-scope production lines: 16,468 (handlers/rds 14,873; gateway/rds 853; gateway/rds.go 95; daemon/rds_deps.go 98; daemon/instance_records* 578, not RDS).

### handlers/rds

| File | Lines | Class | Reason |
|---|---|---|---|
| `store.go` | 451 | temporary residue (prerequisite: per-resource packages exist; removal: each resource owns its own key helpers and bucket view) | One file holds every resource's key space, bucket configs, migrations hook, leader bucket and cross-tenant enumeration (`:20-47`, `:39-50` key list). **Mixed**: split into instance (`db-instances/`, `bootstrap-payloads/`, `instance-index/`, leader bucket), snapshot (`db-snapshots/`, `backups/`, `retained-volumes/`), subnetgroup (`db-subnet-groups/`), parametergroup (`db-parameter-groups/`), events (`events/`), plus a domain-root bucket factory. |
| `records.go` | 501 | temporary residue (same prerequisite) | All durable record structs in one file. **Mixed**: `DBInstanceRecord`/`PendingModifiedValues`/`ModifyLease`/`BootstrapState`/`AgentState`/`SnapshotOperation` → instance; `DBSnapshotRecord`/`AutomatedBackupRecord`/`RetainedVolumeRecord` → snapshot; `DBSubnetGroupRecord` → subnetgroup; `DBParameterGroupRecord`/`DBParameterRecord` → parametergroup; `Parameter`, `EngineHealth`, `Heartbeat*` → guest/controller wire contract. |
| `service.go` | 314 | temporary residue (prerequisite: resource owners; removal: each owner holds its own deps, domain root holds composition) | Single `Service` with one `Deps` bag for every resource (`:19-45` in stripped view), per-account bucket cache and generic JSON CAS helpers. **Mixed**: `Deps` splits by resource; CAS helpers could move to `foundation/state/kvstore` (already exists) rather than a new common package. |
| `service_nats.go` | 189 | temporary residue (prerequisite: awsapi calls owners in-process or via a domain contract; removal: no AWS-SDK-typed NATS client used as internal API) | AWS-SDK-shaped NATS client used by the gateway, quota (`handlers/quota/live.go:89`) and account teardown (`accountteardown/reapers_rds.go:19`). It is currently the de facto internal API, which ADR-0004 S3/S4 forbid for quota. |
| `subjects.go` | 90 | **Mixed**: guest/controller wire contract (`rds.bus.*`, `CommandQueueGroup`, `:6-19`, `:65-90`) + temporary residue (Layer-1 `rds.<Action>` gateway→daemon subjects, `:21-63`) | Bus subjects are the real agent-path process boundary; the per-action subjects exist only because the gateway and daemon are separate dispatch hops. |
| `arn.go` | 92 | AWS protocol adapter (with per-resource kinds) | `FormatARN`/`ParseARN`/`ResourceKind` (`:15-40`) are used for projection, tags and gateway authz scopes. Kinds could live with each resource; parsing belongs in awsapi. |
| `filters.go` | 68 | AWS protocol adapter | Generic AWS `Filter` parsing (`ReadFilters`, `:38`), used by gateway catalogs and describes. |
| `paging.go` | 74 | AWS protocol adapter | AWS `MaxRecords`/`Marker` paging (`:23`) and per-type page keys (`:52-74`). |
| `validate.go` | 271 | resource owner (instance) — partly AWS protocol adapter | `CreateDBInstance` request validation (`:69`), identifier rules (`:196`), unimplemented-parameter rejection (`:225`). **Mixed**: SDK input parsing → awsapi; identifier/engine/class invariants → instance. |
| `status.go` | 69 | resource owner (instance) | DB instance status enum and transition table (`:7-60`). |
| `create.go` | 305 | resource owner (instance) | `CreateDBInstance` orchestration, record reservation, rollback (`:29-147`, `:195-305`). |
| `describe.go` | 298 | **Mixed**: resource owner (instance) query + AWS protocol adapter (projection to `rds.DBInstance`, `:145-298`) | Projection is awsapi; record read/filter is the owner's query. |
| `lifecycle.go` | 454 | resource owner (instance) — **contains an EC2 adapter** | Reboot/Stop/Start (`:57-162`), transition helpers (`:276-405`). `natsInstanceCommander` (`:407-454`) sends `ec2v1` instance commands and `ec2.start`; that is an EC2-owned capability implemented inside RDS (residue). |
| `launch.go` | 629 | cross-resource orchestration (instance realization over EC2/VPC/EBS/IAM) — **contains an EC2 adapter** | `LaunchDBInstanceVM` (`:178-366`) composes system instance, two ENIs, data volume, AMI lookup. `natsVolumeAttacher` (`:560-629`) is an EC2 volume-attach NATS client that Bedrock also imports (`daemon/bedrock_deps.go:43`); it is EC2/EBS residue living in RDS. |
| `network.go` | 200 | resource owner (instance) with cross-resource read of subnetgroup | Endpoint placement (`:36-75`) reads the subnet-group record directly (`:43`) and EC2 VPC/subnet/SG via `networkResolver` (`:16-20`). |
| `vpc.go` | 163 | cross-resource orchestration (system VPC/SG realization for instances) | `EnsureSystemVPC`/`EnsureSystemSecurityGroup` (`:78`, `:104`) over `domains/network/systemvpc`. |
| `storage.go` | 155 | resource owner (instance) | Storage grow via `volumeResizer` (EC2 `DescribeVolumes`/`ModifyVolume`, `:23-26`). |
| `modify.go` | 528 | resource owner (instance) | `ModifyDBInstance` plan/immediate/pending paths (`:76-198`, `:199-528`). |
| `modify_lease.go` | 173 | resource owner (instance) | Per-instance modify lease stored on the instance record (`:24-173`). |
| `pending.go` | 256 | resource owner (instance) — with parametergroup orchestration | Applies `PendingModifiedValues` (`:28`), parameter-group apply to an instance (`:149`), filesystem grow finish (`:240`). `applyParameterGroup` is the instance side of parameter-group propagation. |
| `replace.go` | 252 | resource owner (instance) — replacement | VM replace for class/storage change (`:43-166`), index rewrite (`:200`). |
| `recovery.go` | 220 | resource owner (instance) — failure detection | Health classification (`:54`, `:76`); reports failure, does not repair (`:19-21`). |
| `reconciler.go` | 896 | **Mixed**: resource owner (instance reconcile, `:238-660`, `:797-866`) + snapshot reconcile (`:674-796`) + foundation wiring (leader lease, `:83-188`) | Split: instance reconciler, snapshot creating-resolver, domain-root leader/loop. `describeInstanceState` (`:867-896`) is an EC2 projection adapter. |
| `window.go` | 356 | resource owner (instance) — maintenance/backup windows | Pure window arithmetic for an instance's backup and maintenance windows. |
| `backup.go` | 597 | **Mixed**: resource owner (instance: retention, windows, `runBackupWindow` `:206`, `runMaintenanceWindow` `:340`) + snapshot (automated snapshot identity `:385-394`) + AWS adapter (`DescribeDBInstanceAutomatedBackups` `:396-536`) | Automated backup is an instance obligation that produces snapshot records. |
| `backup_sweep.go` | 340 | resource owner (snapshot) — retention reaper | `BackupRetentionReaper` (`:24-112`) deletes automated snapshots and reclaims retained volumes (`:310`); implements `vm.Reaper` from `runtime/compute/vm` (`:31`). |
| `snapshot.go` | 768 | resource owner (snapshot) — tightly coupled to instance | Create/Describe/Delete (`:49`, `:224`, `:299`); create CASes the instance into `backing-up` (`:672-723`) and quiesces the engine. |
| `restore.go` | 411 | cross-resource orchestration (snapshot → new instance) | `RestoreDBInstanceFromDBSnapshot` (`:24-162`) reads a snapshot record, creates a volume from it and runs the instance create path. |
| `delete.go` | 548 | resource owner (instance) — with snapshot orchestration | `DeleteDBInstance` (`:34`), teardown (`:178-227`), final snapshot (`:247-447`), retained data volume (`:449-535`). |
| `agent.go` | 667 | **Mixed**: guest/controller wire contract (IO types, `Command`, `CommandReply`, `InstanceIndexEntry`, `:30-160`, `:628-636`) + resource owner (instance agent-path handlers `:161-627`) | Split: `contracts/rds/v1` vocabulary vs instance owner handlers. |
| `commands.go` | 239 | **Mixed**: guest/controller wire contract (command types and params, `:12-40`) + resource owner (instance: `issueCommand` issuer, `:86-230`) | Issuer is instance realization; constants are wire vocabulary. |
| `bootstrap.go` | 280 | resource owner (instance) — bootstrap secret | Encrypted master-password payload (`:31-280`), encrypted via `handlers_iam.EncryptSecret` (`:105`, `:199`). |
| `servingcert.go` | 109 | resource owner (instance) — realization helper | Per-instance serving cert minted from cluster CA (`:46`). Uncertain whether this should sit with a PKI owner instead. |
| `userdata.go` | 68 | guest/controller wire contract | Agent user-data rendering (`:36`), the boot-time contract with the DB VM. |
| `iam.go` | 83 | **Mixed**: resource owner (instance — instance profile ensure, `:56-83`) + guest/controller wire contract (`InternalAgentActions`, `:24-30`) | The internal-action list is shared by the gateway gate (`gateway/rds/authz.go:52-63`) and the role grant. |
| `dns.go` | 72 | resource owner (instance) — publishes a projection | `DesiredDNSChanges` (`:25-51`) is an RDS-owned projection consumed by the daemon DNS reconcile. |
| `events.go` | 312 | cross-resource orchestration (event journal) — uncertain final home | Per-source event rings for instance and snapshot (`:88-141`) plus `DescribeEvents` (`:159`). Not in the ADR-0004 resource table; candidate for the domain root, not a `common` package. |
| `tags.go` | 280 | cross-resource orchestration + AWS adapter | One tag implementation dispatching on ARN kind over all four record types (`:51-73`, `:202-230`). Split: each owner exposes a tag mutation on its own record; awsapi routes by ARN. |
| `engine.go` | 287 | resource owner (engine) — read-only catalogue | Engine registry, version/username/dbname/password rules (`:15-287`); consumed by the agent (`LookupEngine`). |
| `engine_postgres.go` | 505 | resource owner (engine) | Postgres engine catalogue and parameter specs/validators. |
| `engine_mariadb.go` | 503 | resource owner (engine) | MariaDB engine catalogue and parameter specs/validators. |
| `paramcatalog.go` | 449 | resource owner (engine) | Parameter spec model, size-derived defaults, `ResolveEffectiveParameters` (`:354`); imports `domains/ec2/instancetypes` for class memory (`:219`). |
| `catalog.go` | 214 | **Mixed**: resource owner (engine — `EngineVersions`, `OrderableOptions` `:102-141`) + AWS protocol adapter (filter value sets, SDK projection `:142-214`) | |
| `sizing.go` | 57 | resource owner (engine/instance-class facade) | `db.*` → EC2 instance type map (`:14-21`), validated against `domains/ec2/instancetypes` (`:30`). |
| `subnetgroup.go` | 340 | resource owner (subnetgroup) | Create/Describe/Delete (`:39`, `:94`, `:138`), subnet resolution via EC2 (`:176`). Reads instance records for in-use check (`:258-277`). |
| `parametergroup.go` | 599 | resource owner (parametergroup) — with instance orchestration | CRUD + DescribeDBParameters (`:29-380`); `propagateParameterGroup` (`:210-236`) pushes changes into instances; `resolveGroupParameters` (`:549`) is called by instance create/modify/restore. |

### gateway/rds and gateway/rds.go

| File | Lines | Class | Reason |
|---|---|---|---|
| `gateway/rds.go` | 95 | AWS protocol adapter (ingress glue) | Query parse, action check, caller derivation, authz order, quota injection and `ExpectedNodes` injection (`:16-75`). Imports `bluebottle/pkg/auth.ParseRoleARN` (`:87`). |
| `gateway/rds/handler.go` | 220 | AWS protocol adapter | Action inventory (registered/internal/unsupported, `:96-169`), typed Query→XML adapter (`:47-79`), `Env{ExpectedNodes, QuotaCheck}` (`:33-43`). |
| `gateway/rds/authz.go` | 112 | AWS protocol adapter | Principal-class gate and per-action resource ARN scopes (`:20-112`). |
| `gateway/rds/instance.go` | 50 | AWS protocol adapter | Instance actions + `DescribeEvents`; runs `QuotaCheck` before forwarding create (`:13-19`). |
| `gateway/rds/snapshot.go` | 35 | AWS protocol adapter | Forwards snapshot/restore/automated-backup actions. |
| `gateway/rds/subnetgroup.go` | 23 | AWS protocol adapter | Forwards subnet-group actions. |
| `gateway/rds/parametergroup.go` | 31 | AWS protocol adapter | Forwards parameter-group actions. |
| `gateway/rds/tags.go` | 23 | AWS protocol adapter | Forwards tag actions. |
| `gateway/rds/enginecatalog.go` | 184 | **Mixed**: AWS protocol adapter (engine catalogue actions) + temporary residue (prerequisite: EC2-owned instance-type-availability projection; removal: `clusterRunnableTypes` replaced by that projection) | `clusterRunnableTypes` (`:146-174`) calls the EC2 AWS adapter `ec2instanceapi.DescribeInstanceTypes` with `env.ExpectedNodes`. |
| `gateway/rds/agent.go` | 287 | **Mixed**: guest/controller wire contract (agent-facing internal actions, long poll, reply relay `:146-287`) + AWS protocol adapter (agent identity gate `:36-111`) | Reads the `rds-system` bucket directly via JetStream (`:72-100`), i.e. the gateway reads RDS private state. |

### daemon and other wiring

| File | Lines | Class | Reason |
|---|---|---|---|
| `daemon/rds_deps.go` | 98 | cross-resource orchestration (composition root) — contains residue | Binds EC2/VPC/IGW/RT/NGW/EIP/image/volume/snapshot services, IAM ensurer, master key and config into `handlers_rds.Deps` (`:20-85`). `describeInstancesFanOut` (`:90-97`) calls the EC2 AWS adapter with `len(clusterConfig.Nodes)` as expected-node count (residue; same blocker family as `ExpectedNodes`). Loads `bluebottle/pkg/masterkey` (`:52`). |
| `daemon/daemon.go` (RDS sections) | n/a | cross-resource orchestration (composition root) | Subscriptions (`:1171-1206`), service init (`:2046-2066`), reconciler start (`:2070-2078`), backup reaper registration (`:2310-2312`), and the RDS reconciler lease reused as the cluster-wide GC election (`:2374-2382`). The last is residue: an RDS lease elects unrelated reapers. |
| `daemon/dns_reconcile.go` (`:71-76`, `:99-104`) | n/a | cross-resource orchestration | Watches RDS per-account buckets via `handlers_rds.AccountWatchBuckets` and consumes `DesiredDNSChanges`. Bucket watch is a read of RDS private storage by the daemon. |
| `daemon/bedrock_deps.go:43` | n/a | temporary residue (prerequisite: EC2/EBS-owned attach capability; removal: Bedrock and RDS both bind the EC2 one) | Another domain imports RDS for an EC2 volume-attach client. |
| `handlers/quota/live.go:82-93` | n/a | temporary residue (prerequisite: RDS-owned count projection per ADR-0004 S4; removal: quota no longer calls `NATSService.DescribeDBInstances`) | Quota counts instances through the AWS-shaped RDS API. |
| `accountteardown/reapers_rds.go` | n/a | temporary residue / cross-domain consumer | Drives instance/subnet-group/parameter-group deletion through `NATSService` AWS-shaped calls (`:41`, `:76`, `:87`, `:109`, `:125`, `:139`, `:159`). Account teardown is explicitly out of ADR-0004 scope. |
| `daemon/instance_records.go`, `daemon/instance_records_migrate.go` | 217 / 361 | not RDS | EC2 instance record space; excluded. |

## 2. Served AWS actions and protocol

Protocol: AWS Query in (form `Action=`), XML out using the IAM-style `<ActionResponse><ActionResult>` envelope (`gateway/rds.go:14-26`, `gateway/rds/handler.go:59-75`).
Order: unknown action → `InvalidAction` before policy (`gateway/rds.go:29-32`); principal-class gate before policy (`:41-43`); resource ARN derivation and policy (`:45-51`); dispatch (`:57-64`).
Unsupported actions return `OperationNotSupported` from `Dispatch` (`gateway/rds/handler.go:182-185`).
Path: gateway adapter → NATS request `rds.<Action>` (queue group `spinifex-workers`) → daemon `Service` method (`daemon/daemon.go:1173-1206`), except where noted.

| Action | Status | Owning resource | Notes |
|---|---|---|---|
| CreateDBInstance | registered | instance | `QuotaCheck` in gateway before forward (`gateway/rds/instance.go:13-19`); 5 min budget (`service_nats.go:16`). |
| DescribeDBInstances | registered | instance | |
| ModifyDBInstance | registered, scoped | instance | Also binds parametergroup. |
| DeleteDBInstance | registered, scoped | instance | Orchestrates final snapshot. |
| RebootDBInstance / StartDBInstance / StopDBInstance | registered, scoped | instance | |
| CreateDBSnapshot | registered, scopes instance + snapshot | snapshot (orchestrates instance) | |
| DescribeDBSnapshots | registered | snapshot | |
| DeleteDBSnapshot | registered, scoped | snapshot | |
| RestoreDBInstanceFromDBSnapshot | registered, scopes snapshot + instance | cross-resource (snapshot → instance) | |
| DescribeDBInstanceAutomatedBackups | registered | instance (automated backup) / snapshot | |
| CreateDBSubnetGroup / DescribeDBSubnetGroups | registered | subnetgroup | |
| DeleteDBSubnetGroup | registered, scoped | subnetgroup | |
| CreateDBParameterGroup / DescribeDBParameterGroups | registered | parametergroup | |
| ModifyDBParameterGroup / DescribeDBParameters / DeleteDBParameterGroup | registered, scoped | parametergroup | Modify propagates to instances. |
| AddTagsToResource / RemoveTagsFromResource / ListTagsForResource | registered, ARN scope | per ARN kind (all four) | `tags.go:51-73`. |
| DescribeEvents | registered | cross-resource (events) | |
| DescribeDBEngineVersions | registered | engine | Served **in the gateway process** with no NATS hop (`gateway/rds/enginecatalog.go:41-73`). |
| DescribeOrderableDBInstanceOptions | registered (`typedEnv`) | engine (+ EC2 availability) | Served in the gateway; NATS fan-out to EC2 `DescribeInstanceTypes` using `ExpectedNodes` (`gateway/rds/enginecatalog.go:146-174`). |
| RegisterDBInstance, SubmitDBStateChange | registered, internal | instance (agent path) | Gateway → bus `rds.bus.{acct}.{db}.register/health` (`service_nats.go:49-57`). |
| GetDBBootstrapConfig, AcknowledgeDBBootstrap | registered, internal | instance (bootstrap) | Gateway → `rds.GetDBBootstrapConfig` / `rds.AcknowledgeDBBootstrap`. |
| PollDBCommands | registered, internal | instance (command channel) | Served **in the gateway**: queue-subscribes the bus command subject; no daemon handler (`gateway/rds/agent.go:182-247`). |
| CreateDBInstanceReadReplica, PromoteReadReplica, CreateDBCluster, ModifyDBCluster, DeleteDBCluster, DescribeDBClusters, FailoverDBCluster, CreateOptionGroup, ModifyOptionGroup, DeleteOptionGroup, DescribeOptionGroups, RestoreDBInstanceToPointInTime | unsupported (12) | n/a | `gateway/rds/handler.go:157-168`. |

Totals: 26 customer actions registered, 5 internal agent actions, 12 unsupported, 43 in the inventory (`ActionNames`/`UnsupportedActionNames` feed `gateway/operations.go:43-45`).
Daemon subscribes 29 `rds.*` subjects (`daemon/daemon.go:1175-1205`); the two catalogue actions and `PollDBCommands` have no daemon subject.
Any action outside the table and not in the map is `InvalidAction`.

## 3. Durable records and KV buckets

| Bucket | Key | Record | Owning resource (proposed) | Writers | Readers (flag = outside RDS owner) |
|---|---|---|---|---|---|
| `rds-account-{accountID}` (history 1, version 1, lazily created; `store.go:20-38`, `:201-213`) | `db-instances/{id}` | `DBInstanceRecord` | instance | create, restore, lifecycle, modify, pending, replace, agent, delete, reconciler, recovery, backup, snapshot (`backing-up` CAS), tags | subnetgroup (`subnetgroup.go:258-277`), parametergroup (`parametergroup.go:211`, `:342`), dns.go, **daemon DNS reconcile via bucket watch** (`daemon/dns_reconcile.go:75`), **quota** indirectly via `DescribeDBInstances` (`handlers/quota/live.go:89`), **account teardown** via `NATSService` |
| same | `bootstrap-payloads/{id}` | `BootstrapPayloadEnvelope` (encrypted) | instance | create, restore, replace (via bootstrap.go) | agent.go (`GetDBBootstrapConfig`, `AcknowledgeDBBootstrap`), reconciler (stale pending report) |
| same | `db-snapshots/{id}` (colon → `.`, `store.go:84-97`) | `DBSnapshotRecord` (manual and automated) | snapshot | snapshot.go, delete.go (final snapshot), backup.go (automated) | restore.go, backup_sweep.go, reconciler snapshot pass, describe, tags |
| same | `backups/{dbId}/automated/{ts}` | `AutomatedBackupRecord` | snapshot (automated-backup index) — instance obligation | backup.go | backup_sweep.go, DescribeDBInstanceAutomatedBackups |
| same | `retained-volumes/{volumeID}` | `RetainedVolumeRecord` | snapshot (cleanup obligation for a former instance's volume) | delete.go `retainDataVolume` (`:484`) | snapshot.go `releaseRetainedVolume`/`reclaimRetainedVolume` (`:414`, `:430`), backup_sweep `reclaimOrphanedVolumes` (`:310`) |
| same | `db-subnet-groups/{name}` | `DBSubnetGroupRecord` | subnetgroup | subnetgroup.go, tags.go | network.go `resolvePlacement` (`:43`, instance create/restore), describe, **account teardown** via `NATSService` |
| same | `db-parameter-groups/{name}/meta` | `DBParameterGroupRecord` | parametergroup | parametergroup.go, tags.go | instance create/modify/restore via `resolveGroupParameters` (`parametergroup.go:549`), pending.go |
| same | `db-parameter-groups/{name}/params/{key}` | `DBParameterRecord` | parametergroup | parametergroup.go (`:188`, delete `:361`) | `ListDBParameterOverrides` (`store.go`), instance apply paths |
| same | `events/{sourceType}/{sourceId}` | event ring (bounded, 14-day trim) | events (cross-resource) | `RecordEvent` from every resource (`events.go:88`) | `DescribeEvents` |
| `rds-system` (history 1, version 1; `store.go:24-26`, `:184-195`) | `instance-index/{ec2InstanceID}` | `InstanceIndexEntry{AccountID, DBInstanceIdentifier, VMGeneration}` (`agent.go:628-636`) | instance (agent identity binding) | create (`create.go:124`), restore (`restore.go:142`), replace (`replace.go:200-215`), delete teardown | **gateway reads JetStream directly** (`gateway/rds/agent.go:72-100`); `Service.LookupInstanceIndex` (`agent.go:637`) |
| `spinifex-rds-leader` (TTL 60s; `store.go:28-33`, `:216-223`) | `reconciler` | kvlease record | domain-root reconciler leadership | `kvlease` via `Reconciler` (`reconciler.go:104-118`) | **daemon GC uses it as the cluster-wide reaper election** (`daemon/daemon.go:2374-2382`) |

Non-durable state that affects decisions: in-memory heartbeat liveness map per daemon (`service.go` `liveness`, `agent.go:284-321`), persisted to KV only on change or every `HeartbeatPersistFloor` (5 beats, `records.go` constants).
The reconciler leader reads this node-local map (`recovery.go:191`), so a beat answered by another node's queue-group member is seen only via the KV floor (uncertain impact; observation, not intent).

## 4. Identity and scope per resource

| Resource | Identifier | Immutable internal ID | ARN | Account | Region | AZ |
|---|---|---|---|---|---|---|
| instance | `DBInstanceIdentifier`, account-unique via `createJSONRevision` (`create.go:62`), reusable after delete | `DbiResourceID` `db-…` (`create.go:153`) | `arn:aws:rds:{region}:{acct}:db:{id}` (`arn.go:15-27`) | customer account in bucket name and record; VM, system ENI and IAM profile live in the system account `GlobalAccountID` (`create.go:51-55`, `delete.go:544-546`) | not stored; taken from `Service.region` at projection (`service.go`) | not recorded on the instance; derived only via subnet (uncertain whether projected at all; no `AvailabilityZone` reference in `describe.go`) |
| snapshot | `DBSnapshotIdentifier`; automated ones `rds:{db}-{ts}` (`backup.go:385`) | none beyond backing EC2 `SnapshotID` | `…:snapshot:{id}` | customer | from service | none |
| subnetgroup | `DBSubnetGroupName` (case-sensitive key; `default` rejected, `subnetgroup.go:318`) | none | `…:subgrp:{name}` | customer | from service | each member subnet carries its AZ (`records.go` `DBSubnetGroupSubnet`) |
| parametergroup | `DBParameterGroupName`; `default.*` synthesized per engine, not stored (`parametergroup.go:24`, `:456-482`) | none | `…:pg:{name}` | customer | from service | n/a |
| engine | engine name + version | n/a (static) | none | global | global | n/a |
| agent identity | EC2 instance ID as `RoleSessionName` of the system instance role (`gateway/rds/agent.go:36-69`) | `VMGeneration` in index | n/a | system account principal mapped to customer account via index | n/a | n/a |

## 5. Lifecycle paths per resource

### instance

- Create: validate (`validate.go:69`) → placement reads subnet group / default VPC (`network.go:36`) → resolve parameter group (`create.go:47`) → ensure IAM instance profile in system account (`create.go:51`) → reserve record at `creating` with `createJSONRevision` (`create.go:62`) → stage encrypted bootstrap payload (`create.go:83`) → `LaunchDBInstanceVM` inline in the request (`create.go:87`) → `recordLaunch` CAS (`create.go:115`, `:195-244`) → write instance index (`create.go:124`) → event, return `creating`.
- Create failure: deferred rollback deletes payload and CAS-deletes the record if `DbiResourceID` unchanged (`create.go:246-295`); post-launch failures run `Unwind` (`create.go:297-305`).
- Readiness: reconciler `reconcileCreating` moves `creating → available` on first healthy agent beat (`reconciler.go:448-464`); `failed` after `BootstrapTimeout` (default 20 min, `reconciler.go:30`).
- Steady state: `reconcileHealth` (`recovery.go:76-101`) records/clears failure; `StatusRecovering` exists in the table (`status.go:22`, `:34`, `:43`, `:47`) but **no code transitions into it** (only `status.go` references it), so there is no automatic repair.
- Reboot/Start: `beginTransition` CAS (`lifecycle.go:65`, `:143`) then reconciler `reconcileRestarting` (`reconciler.go:479`).
- Stop: CAS to `stopping`, inline engine stop + VM stop, inline `completeTransition` to `stopped` before responding (`lifecycle.go:103-133`); reconciler resumes a dead stop (`reconciler.go:584-605`).
- Modify: immediate non-disruptive in-request; disruptive writes `PendingModifiedValues` (`modify.go:153`), then either waits for the maintenance window (`backup.go:340`) or runs under the modify lease (`modify.go:176`); reconciler resumes under the same lease (`reconciler.go:510-583`); class/storage change replaces the VM (`pending.go:80`, `replace.go:43-166`).
- Replace: launch new VM (`replace.go:113`), rewrite index (`:125`), CAS record with `VMGeneration++` (`:137`), rollback on error (`:218`).
- Delete: validate, deletion protection, reserve final snapshot (`delete.go:68`), CAS to `deleting` (`delete.go:73-91`), inline teardown (`delete.go:178-227`): stop engine, terminate VM, final snapshot, purge automated backups, release or retain data volume, delete ENIs (best-effort), delete index, delete payload, delete record last; reconciler replays teardown (`reconciler.go:611-628`) and marks `failed` after `transitionTimeout` 10 min.
- Fencing: KV revision CAS on the record everywhere; per-instance `ModifyLease` on the record (`modify_lease.go:24-173`); cluster `kvlease` for the reconciler (`reconciler.go:104-118`); `VMGeneration` binds bootstrap payloads and acknowledgements (`agent.go:345`, `:470`, `:552-553`) and the index.
- No desired generation exists on `DBInstanceRecord` (`records.go`); `VMGeneration` is a realization generation, not desired-intent generation.
- Idempotency: AWS `CreateDBInstance` has no client token, and a repeat returns `DBInstanceAlreadyExists` (`create.go:64-66`); delete retry re-runs teardown and must repeat the snapshot choice (`delete.go:57-63`); register is idempotent (`agent.go:159-160`).

### snapshot

- Create: check name free (`snapshot.go:92`) → CAS instance to `backing-up` with `SnapshotOperation` (`:672-705`) → create record `creating` (`:109-115`) → quiesce + EC2 snapshot inline (`:144-188`) → `putJSON` record `available` (`:126-130`) → restore instance status (`:706-723`).
- Readiness: synchronous; the API returns `available`.
- Repair: reconciler resolves records stuck `creating` past 15 min against EC2 (`reconciler.go:674-751`, `findEC2SnapshotFor` `:752`); returns instances stuck in `backing-up` (`reconciler.go:630-652`).
- Delete: `removeDBSnapshot` deletes EC2 snapshot then record and releases a retained volume when last holder (`snapshot.go:330-470`).
- Automated: window-driven `takeAutomatedBackup` (`backup.go:270`), stamped on instance record so a leader change cannot double-fire (`reconciler.go:654-660` comment); retention sweep `BackupRetentionReaper` via the daemon GC backstop (`backup_sweep.go:45-112`, `daemon/daemon.go:2310`).
- Fencing: instance-record CAS into `backing-up`; `createJSON` on snapshot key; no snapshot generation.

### subnetgroup

- Create: validate, resolve subnets through EC2 `DescribeSubnets` (`subnetgroup.go:176-239`), `createJSON` (`:78`); synchronous; status always `Complete` (`:31`, `:287`).
- Describe: KV read and list (`:94-133`).
- Delete: read, scan all instance records for `DBSubnetGroupName == name` (`:154-163`), plain `kv.Delete` (`:165`).
- No modify action is served (`ModifyDBSubnetGroup` is neither registered nor unsupported, so it returns `InvalidAction`).
- No reconciliation; no generation; tags via `tags.go` CAS.

### parametergroup

- Create: validate family against engine registry, `createJSON` meta (`parametergroup.go:29-84`); synchronous.
- Modify: validate whole batch, then `putJSON` one key per parameter (`:186-196`), then synchronously push to every attached instance via agent `apply-params` (`:198`, `:210-236`, `pending.go:149`).
- Delete: refuse `default.*`, scan instances for current or pending use (`:342-352`), delete params then meta (`:354-367`).
- `default.*` groups are synthesized from the engine catalogue (`:456-482`).
- No generation; no reconciliation of a failed propagation beyond `ParameterApplyFailed` on the instance (`pending.go:214`).

### engine

- Static, process-local catalogue validated at daemon boot (`engine.go:69`, `daemon/daemon.go:2055`); no lifecycle; ADR-0003 S1 excludes pure read-only catalogues from the lifecycle contract.

## 6. NATS subjects and controller-to-agent messages

Gateway → daemon (request/reply, queue group `spinifex-workers`): `rds.<Action>` for the 24 customer actions listed in `subjects.go:21-63`, plus `rds.GetDBBootstrapConfig` and `rds.AcknowledgeDBBootstrap` (`subjects.go:17-19`).
Budgets: default 30 s, create 5 min, lifecycle 4 min, delete 6 min, modify 6 min, snapshot 6 min, snapshot delete 4 min, restore 8 min (`service_nats.go:12-36`).
A gateway timeout does not cancel the daemon's inline orchestration (uncertain; inferred from request/reply shape), so the client may see an error while the create continues.

Agent path (agent speaks SigV4 Query to awsgw over the mgmt bridge, `daemon/rds_deps.go:71-73`):

- `RegisterDBInstance` → gateway → `rds.bus.{acct}.{db}.register` → daemon `SubjectRegisterWildcard` (`subjects.go:12-13`, `:65-67`).
- `SubmitDBStateChange` (heartbeat folded into state, 30 s interval, stale after 90 s; `records.go` constants) → `rds.bus.{acct}.{db}.health`.
- `GetDBBootstrapConfig` / `AcknowledgeDBBootstrap` → Layer-1 subjects; replay is allowed until acknowledged for the bound `VMGeneration` (`gateway/rds/agent.go:150-180`).
- `PollDBCommands` long poll (1-20 s) queue-subscribes `rds.bus.{acct}.{db}.command` with group `spinifex-rds-agents` and publishes carried replies to `rds.bus.{acct}.{db}.command-reply` before subscribing (`gateway/rds/agent.go:198-287`).
- Command types: `set-password`, `apply-params`, `stop-engine`, `grow-filesystem`, `quiesce`, `unquiesce` (`commands.go:12-23`); params `MasterUsername`, `MasterUserPassword`, `QuiesceLabel`, `QuiesceDeadlineSeconds` (`commands.go:27-35`).
- Issuer: `issueCommand` subscribes the reply subject, then republishes the same `CommandID` every 2 s until reply or budget (`commands.go:144-199`, `:72`); core NATS, not durable, by design for the password (`subjects.go:77-80`).
- Agent retry: results are retained and re-carried until a poll succeeds (`agents/rds/agent/command.go:100-133`).
- Agent dedupe: the agent executes every delivered command and has **no CommandID dedupe** (`agents/rds/agent/command.go:128-131`, `:140-157`); a republish delivered after execution but before the issuer sees the reply can execute twice (uncertain; depends on timing between reply relay and the 2 s republish).
- Commands carry no `VMGeneration` (`agent.go:134-139`), so the fence against a superseded VM is the bus subject plus the gateway's index lookup rather than a generation in the message.
- `LookupEngine`, `Engine`, `ParamType*`, `ApplyType*`, `Parameter`, `EngineHealth*`, `HeartbeatInterval`, `BootstrapMode*`, `ParameterRollbackMessage`, `SupportedInstanceClasses`, `SmallestInstanceClass` are imported by the agent from `handlers/rds` (full list from import scan); this is the S5 "accidental import of a domain monolith".

RDS → EC2 subjects used as capabilities: `ec2v1.InstanceCommandSubject(id)` for stop/reboot/attach (`lifecycle.go:434`, `launch.go:583`), `ec2.start` (`lifecycle.go:451`), `ec2.DescribeStoppedInstances` (`launch.go:616`), plus EC2 `DescribeInstances` fan-out (`daemon/rds_deps.go:90-97`) and `DescribeInstanceTypes` fan-out (`gateway/rds/enginecatalog.go:147`).

Recovery/redelivery: there is no durable work queue; recovery relies on the reconciler re-reading record status (`reconciler.go:238-282`) under `kvlease`, a 5 min resync and KV watch wakeups (`reconciler.go:120-145`).

## 7. Boundary-crossing imports

### Outbound from RDS (handlers/rds, gateway/rds, gateway/rds.go, daemon/rds_deps.go)

- **Quota**: none from `handlers/rds`; the gateway injects `gw.Quota.EnforceRDSInstances` as `Env.QuotaCheck` (`gateway/rds.go:59-62`).
- **ExpectedNodes / EC2 instance availability**: `gateway/rds/enginecatalog.go` → `domains/ec2/awsapi/instance.DescribeInstanceTypes` with `env.ExpectedNodes` (`:147`); `daemon/rds_deps.go` → `domains/ec2/awsapi/instance.DescribeInstances` with node count (`:96`).
- **EC2 instance**: `domains/ec2/systeminstance` (`launch.go:99-100`, `delete.go:236`), `domains/ec2/instance` (`lifecycle.go`, `StartStoppedInstanceOutput`), `contracts/ec2/v1` (`launch.go`, `lifecycle.go`), `runtime/compute/vm` (`agent.go`, `create.go` `vm.VolumeSerial`, `backup_sweep.go` `vm.Reaper`), `foundation/aws/ami`, `domains/ec2/instancetypes` (`sizing.go:30`, `paramcatalog.go:219`).
- **VPC/network**: `domains/network/systemvpc` (`vpc.go`, `launch.go`, `daemon/rds_deps.go`), EC2 SDK VPC/subnet/SG/ENI calls through narrow interfaces `networkResolver` (`network.go:16`) and `launchVPCProvisioner` (`launch.go:76-96`); `foundation/aws/tags` (`ManagedByRDS`).
- **Storage/EBS**: EC2 SDK `CreateVolume`/`DeleteVolume` (`launch.go:107-111`), `DescribeVolumes`/`ModifyVolume` (`storage.go:23-26`), EC2 snapshot provider (`snapshot.go:25`), volume attach over EC2 NATS (`launch.go:560-629`).
- **IAM**: `handlers/iam` for `SystemInstanceRoleEnsurer`/`EnsureSystemInstanceProfile` (`iam.go:56-77`) and `EncryptSecret`/`DecryptSecret` for bootstrap payloads (`bootstrap.go:105`, `:199`).
- **DNS**: `domains/dns` (`dns.go:17`, `dns.RDSName`, `dns.Change`).
- **Bluebottle**: `bluebottle/pkg/auth.ParseRoleARN` (`gateway/rds.go:87`), `bluebottle/pkg/masterkey.ReadShared` (`daemon/rds_deps.go:52`); `handlers/rds` imports no Bluebottle package.
- **Ingress/foundation**: `ingress/aws/query`, `foundation/aws/xml`, `foundation/aws/errors`, `foundation/aws/identifiers`, `foundation/state/{kvstore,kvlease,kvutil,migrate}`, `foundation/lifecycle/reconciler`, `foundation/messaging/nats`, `foundation/pki`, `bootstrap/config`.

### Inbound into RDS (who imports `handlers/rds` or `gateway/rds`)

- `handlers/quota/live.go:14,89` → `NewNATSService(...).DescribeDBInstances` (quota reads RDS through the AWS-shaped API).
- `accountteardown/reapers_rds.go:9` → `NATSService` describe/modify/delete for instances, subnet groups, parameter groups.
- `daemon/daemon.go:78` → `Service`, `Reconciler`, all `Subject*`, `ValidateEngineRegistry`, `NewService`, `NewReconciler`, `NewBackupRetentionReaper`.
- `daemon/rds_deps.go` → `Deps`, `LaunchDeps`, `BackupPolicy`, `NewNATSVolumeAttacher`, `NewDescribeInstanceState`, `NewNATSInstanceCommander`.
- `daemon/dns_reconcile.go:16,75` → `AccountWatchBuckets` (raw bucket handles).
- `daemon/bedrock_deps.go:10,43` → `NewNATSVolumeAttacher` (Bedrock/Ochre depends on RDS).
- `gateway/operations.go:8,44-45` → `gateway_rds.ActionNames`, `UnsupportedActionNames`.
- `gateway/rds/*` → `KVBucketRDSSystem`, `InstanceIndexKey`, `InstanceIndexEntry`, `InstanceRoleName`, `InternalAgentActions`, `ResourceKind*`, `FormatARN`, `ParseARN`, catalogue functions, `NATSService`, wire types.
- `agents/rds/agent/*` → wire types and engine catalogue (section 6).
- `tests/integration/service_daemon_lite.go:51`, `tests/e2e/harness/rds.go` (test support).

## 8. Known gaps against Q-78 / ADR-0003 (observed in code, not live-verified)

1. No desired generation on any RDS record (`records.go`); mutations advance only the KV revision, and `PendingModifiedValues` is overwritten by a later modify (`modify.go:153`), so Q-78 "atomically commits a monotonically increasing desired generation" is not met.
2. Desired and observed state share one record: `Status`, `Agent`, `UnhealthySince`, `FailureReason` sit beside requested class/storage on `DBInstanceRecord` (`records.go`).
3. Create performs all external realization inline in the request (`create.go:87`); if the daemon dies after `LaunchDBInstanceVM` returns and before `recordLaunch` (`create.go:115`), the record stays `creating` with no `InstanceID`, times out to `failed` (`reconciler.go:458-462`), and a later delete has no IDs to clean (`delete.go:229-233`, `:537-547`); the VM, ENIs and volume are tagged `ManagedBy=rds` (`launch.go:260`, `:314`, `:400`) but no RDS orphan reaper for them was found.
4. The same interruption window exists for restore (`restore.go:111` → `:136`) and replace (`replace.go:113` → `:137`); replace's rollback runs only on error return, not on process death.
5. Delete removes the only ownership record (`delete.go:217`) after ENI deletion that is best-effort and only logs failures (`delete.go:203`, `launch.go:510-520`), contrary to Q-78 "a handler never removes the only reconstructible ownership of required cleanup".
6. `deleting` that exceeds 10 min becomes `failed` (`reconciler.go:621-624`); a `failed` instance is then handled by `reconcileHealth` rather than continuing teardown, so incomplete cleanup is not shown as a distinct state (uncertain; inferred from the `switch` at `reconciler.go:256-272`).
7. `StatusRecovering` is declared and reachable in the transition table but never entered, so there is no repair path (`status.go:22`; `recovery.go:19-21` says v1.0 reports rather than repairs).
8. Synchronous completion where AWS is asynchronous: AWS `CreateDBSnapshot` returns `Status=creating`, here it returns `available` after the snapshot is taken inline (`snapshot.go:126-130`); AWS `StopDBInstance` returns `stopping`, here the handler completes to `stopped` inline (`lifecycle.go:126`); AWS `DeleteDBInstance` leaves the instance describable as `deleting`, here the record is deleted before the response when teardown succeeds inline (`delete.go:97-104`, `:217`).
9. DB subnet group and parameter group deletes are check-then-delete across keys with no fence against a concurrent `CreateDBInstance` that has already read the group (`subnetgroup.go:151-165`, `parametergroup.go:339-367`; `network.go:43`, `create.go:47`), so an instance can reference a deleted group.
10. `ModifyDBParameterGroup` writes one key per parameter with no batch atomicity (`parametergroup.go:186-196`); a KV failure mid-loop leaves a half-applied batch despite the comment at `:140-143`, and propagation to instances is synchronous best-effort (`:210-236`).
11. Quota admission is a non-atomic count before create (`gateway/rds/instance.go:13-19`, `handlers/quota/live.go:85-93`), so concurrent creates can exceed the cap; it counts `len(DescribeDBInstances)` which pages at 100 by default (`paging.go:14`, `describe.go:71`), so an account above 100 instances is under-counted (uncertain whether quota limits ever exceed 100).
12. Agent commands are republished every 2 s with no agent-side `CommandID` dedupe and no generation (section 6), so internal redelivery is not proven idempotent for `grow-filesystem`, `apply-params` or `set-password`.
13. Heartbeat liveness lives partly in per-node process memory (`agent.go:284-321`) and the leader judges health from its own map plus a KV floor (`recovery.go:191-209`), which Q-78 discourages for recovery decisions (observation-side, lower severity).
14. The RDS reconciler lease doubles as the cluster-wide GC election for unrelated reapers (`daemon/daemon.go:2374-2382`); without RDS service init every cluster-wide reaper is skipped.
15. No lifecycle contract record exists for any RDS resource (ADR-0003 S1); nothing in `handlers/rds` states readiness predicates or deletion obligations as a contract.

## 9. First-resource candidates

| Rank | Resource | Size (prod lines) | Private state | Dependencies | Tests | Extractable without quota / ExpectedNodes / IAM / Bluebottle? |
|---|---|---|---|---|---|---|
| 1 | **subnetgroup** | ~340 (`subnetgroup.go`) + its slice of `store.go`, `records.go`, `tags.go`, `arn.go`; gateway 23 | `db-subnet-groups/{name}` only | EC2 `DescribeSubnets` through a narrow interface already (`network.go:16-20`, `subnetgroup.go:189`); reverse dependency: needs an instance-owned query "instances referencing subnet group X" to replace `instancesUsingGroup` (`subnetgroup.go:154`); forward dependency: instance placement reads the group record directly (`network.go:43`), which must become a subnetgroup query | `handlers/rds/subnetgroup_test.go` (338), `tags_test.go`, `gateway/rds/handler_test.go`/`authz_test.go`, e2e `tests/e2e/rds/groups_test.go` (`TestSubnetAndParameterGroups`), `isolation_test.go`, `api_matrix_test.go` (uncertain coverage depth for the last two) | **Yes.** No quota, ExpectedNodes, IAM, Bluebottle, agent or EC2-launch dependency. Synchronous AWS semantics (status `Complete`) match the code. Gap 9 (delete race) should be recorded, not fixed, during the move. |
| 2 | **parametergroup** | ~600 (`parametergroup.go`) + slice of store/records/tags; gateway 31 | `db-parameter-groups/{name}/meta`, `…/params/{key}` | Engine catalogue (`engineForFamily`, `LookupParameter`, `ResolveEffectiveParameters`) — owned by **engine**; `SmallestInstanceClass` (EC2 `instancetypes`); cluster CA availability (`checkTLSEnforceable`, `parametergroup.go:581`); instance records for in-use check; **instance orchestration** in Modify (`propagateParameterGroup` → `applyParameterGroup` → agent `apply-params`) | `parametergroup_test.go` (806), `paramcatalog_test.go`, `enginefamily_test.go`, `tlsenforcement_test.go` (uncertain scope), `pending_test.go` (propagation), e2e `groups_test.go` | **Mostly.** No quota/ExpectedNodes/IAM/Bluebottle, but Create/Describe/Delete are synchronous while Modify crosses into instance realization and the agent wire contract; extraction requires engine first (or a narrow engine capability) and moving propagation to explicit cross-resource orchestration. |
| 3 | **engine** | ~1,960 (`engine.go`, `engine_postgres.go`, `engine_mariadb.go`, `paramcatalog.go`, `catalog.go`, `sizing.go`) | none (static catalogue) | `domains/ec2/instancetypes`; consumed by the agent, parametergroup, instance | `engine_test.go`, `engine_mariadb_test.go`, `enginefamily_test.go`, `paramcatalog_test.go`, `catalog_test.go`, `sizing_test.go`, `gateway/rds/enginecatalog_test.go` | **Package: yes**; it is the natural S5 agent capability. **awsapi action `DescribeOrderableDBInstanceOptions`: no**, blocked on the EC2 instance-type-availability projection (`gateway/rds/enginecatalog.go:146-174`). Not a durable resource under ADR-0003 S1, so it is weak evidence for ADR-0004 rollout item 2. |
| 4 | **snapshot** | ~1,700 (`snapshot.go`, `backup_sweep.go`, share of `backup.go`, `restore.go`, `reconciler.go:674-796`) | `db-snapshots/`, `backups/…/automated/`, `retained-volumes/` | Instance record CAS into `backing-up`, engine quiesce over agent commands, EC2 snapshot/volume services, instance delete (final snapshot, retained volumes), restore runs the instance create path | `snapshot_test.go` (879), `restore_test.go`, `backup_test.go` (1013), `backup_sweep_test.go`, `delete_test.go`; e2e `snapshot_restore_test.go`, `automated_backup_test.go` | No quota/ExpectedNodes/IAM/Bluebottle on snapshot itself, but restore inherits instance create (IAM, launch); snapshot creation needs instance-owned quiesce/hold capabilities first. Not a first candidate. |
| 5 | **instance** | ~8,000+ (create, describe, lifecycle, launch, network, vpc, storage, modify, modify_lease, pending, replace, recovery, reconciler, window, backup, delete, agent, commands, bootstrap, servingcert, userdata, iam, dns, validate, status) | `db-instances/`, `bootstrap-payloads/`, `rds-system` `instance-index/`, `spinifex-rds-leader` | Quota admission, ExpectedNodes-style EC2 fan-out (`daemon/rds_deps.go:90-97`), IAM ensurer and secret crypto, Bluebottle master key (via composition), EC2 system instance / ENI / volume / snapshot / attach, system VPC, DNS, agent wire contract | `create_test.go`, `launch_test.go`, `lifecycle_test.go`, `delete_test.go`, `modify_test.go`, `modify_lease_test.go`, `pending_test.go`, `replace_test.go`, `reconciler_test.go`, `recovery_test.go`, `revisit_test.go`, `agent_test.go`, `bootstrap_test.go`, `commands_test.go`, `storage_test.go`, `network_test.go`, `vpc_test.go`, `describe_test.go`, `status_test.go`, `iam_test.go`, `userdata_test.go`, `watch_source_test.go`, `gateway/rds/agent_test.go`, `authz_test.go`; e2e `create_describe`, `lifecycle`, `modify`, `delete`, `failure`, `graceful_reboot`, `connect`, `cross_subnet`, `isolation`, `authz`, `mariadb*` | **No.** Every listed blocker applies. Extract last. |

Cross-resource files to settle alongside the first extraction: `tags.go` (per-ARN dispatch), `events.go` (journal), `arn.go`/`filters.go`/`paging.go` (awsapi), `store.go`/`records.go`/`service.go` (split per owner).

Recommendation: extract **subnetgroup** first, with two narrow capabilities defined by the consumer: an instance-owned "references to subnet group" query for delete, and a subnetgroup-owned "resolve placement subnets" query for instance create.
Then do **engine** (package and agent capability, leaving the orderable action on the legacy path until the EC2 projection exists), then **parametergroup** with propagation made explicit orchestration.
Test coverage for subnetgroup is unit plus one e2e test; ADR-0003 S5 failure/recovery evidence for its delete race does not exist yet.
