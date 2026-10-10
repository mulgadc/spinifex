# ADR-0004 source inventory — ECS

Code-read inventory of `refactor/spinifex-package-boundaries` at `6c50349bd`.
It is not verified against a live system, and the branch is 59 commits behind `dev` at the time of writing, so recheck any file it cites before acting on it.
Items marked "verify" or "uncertain" are unconfirmed.
Gaps record observed behaviour, not target behaviour.


Repo: `spinifex` submodule, branch `refactor/spinifex-package-boundaries`; paths below are relative to `spinifex/spinifex/` unless stated otherwise.
Binding inputs: ADR-0001..0004 and Q-78 at `origin/main`.
This inventory reports what the code does; it accepts or closes no gap.
"Verify" marks claims about AWS behaviour, or runtime behaviour, that a test or live system has not confirmed here.

## 1. File table

Totals: `handlers/ecs` is 7,760 production lines over 28 files, `gateway/ecs` + `gateway/ecs.go` is 1,236 lines, and `daemon/ecs_deps.go` + `accountteardown/reapers_ecs.go` is 132 lines.

| File | Lines | Class | Reason / split |
|---|---|---|---|
| `handlers/ecs/bus/messages.go` | 173 | guest/controller wire contract (part temporary residue) | `Assign`, `AssignContainer`, `StopDirective`, `TaskState`, `ContainerStatus`, `InstanceCapacity`, `PortMapping` and `SystemControl` are live payloads of the agent and gateway path. `RegisterInstance` (:19) and `Heartbeat` (:33) have no production publisher, so they are residue. Split: live types go to a versioned `contracts/ecs/v1`; the residue types are deleted once the scheduler bus subscriptions are removed. |
| `handlers/ecs/bus/subjects.go` | 45 | temporary residue (prerequisite: drop scheduler bus subs; removal: delete unused builders) | `RegisterSubject`, `HeartbeatSubject`, `AssignSubject` and `TaskStateSubject` (:19-38) have no production publisher. `ServiceReconcileSubject` (:43) is live and is an internal cross-node wake owned by the service reconcile. |
| `handlers/ecs/capacity.go` | 223 | cross-resource orchestration (container-instance capacity provisioning) | `ProvisionCapacity` (:70) is a non-AWS action. It composes an IAM system role, EC2 AMI lookup and EC2 `RunInstances`, and records nothing durable in ECS. |
| `handlers/ecs/capacity_providers.go` | 233 | resource owner (`capacityprovider`), mixed | Create, Describe and Delete belong to `capacityprovider`. `PutClusterCapacityProviders` (:16-40) mutates the cluster record and belongs to `cluster`. |
| `handlers/ecs/cluster_teardown.go` | 171 | mixed: cross-resource orchestration + resource owner (`containerinstance`) | `DeleteCluster` (:24-53) is a cascade over every child, which is orchestration. `DeregisterContainerInstance` (:60), `UpdateContainerInstancesState` (:103), `instanceActiveTasks` (:143) and `drainInstanceServiceTasks` (:160) are container-instance commands that also stop tasks. Split three ways: cluster delete orchestration, container-instance owner, and task-stop calls through a task capability. |
| `handlers/ecs/conv.go` | 291 | mixed | `ClusterShortName`, `ContainerInstanceShortID`, `TaskShortID` and `CapacityProviderShortName` (:27-60) are per-resource identity parsers that the gateway authz also uses. The `containerDefsFromAWS`, `runtimePlatformFromAWS` and `ContainerDef.toAWS` group (:87-265) is the `taskdefinition` projection. `toAssignContainer` (:268) is wire-contract projection. |
| `handlers/ecs/deployments.go` | 267 | resource owner (`service`) | This is pure deployment, rollout and circuit-breaker logic on `ServiceRecord`, with no KV I/O. |
| `handlers/ecs/eni.go` | 155 | cross-resource orchestration (task realization, network capability) | The consumer-owned `eniController` interface (:37) belongs with `task`. `natsENIController` (:46-128) binds it to EC2 subjects and belongs at the composition root. |
| `handlers/ecs/gpu_report.go` | 63 | resource owner (`task`), mixed with wire contract | `ReportTaskGPU` (:31) writes the task record. `ReportTaskGPUInput`, `ContainerGPUReport` and `ReportTaskGPUOutput` (:10-25) are agent wire shapes. |
| `handlers/ecs/instancerole.go` | 48 | cross-resource orchestration (identity for container instances) | It ensures and converges the `ecsInstanceRole` through `handlers_iam` (:37-48). |
| `handlers/ecs/instances.go` | 532 | mixed: resource owner (`containerinstance`) + `task` + orchestration | Register, Describe, List, `upsertInstance`, `recordRegister` and `recordHeartbeat` (:19-357) belong to `containerinstance`. `recordTaskState` (:361) and `SubmitTaskStateChange` (:445) belong to `task`. `releaseReservation` (:505) is placement against the instance record. `recordDeploymentFailure` (:481) belongs to `service`. |
| `handlers/ecs/netcfg.go` | 78 | resource owner (`task`) | It resolves the network mode and parses awsvpc input for task launch. |
| `handlers/ecs/placement.go` | 144 | cross-resource orchestration (placement) | It contains pure fit and strategy selection over container-instance capacity, with no I/O. |
| `handlers/ecs/poll.go` | 107 | guest/controller wire contract | It holds the agent inbox drain and ack (`PollAssignments` :37), and the task-owned inbox reclaim (:93-107). |
| `handlers/ecs/records.go` | 525 | mixed (all resources' records) | It holds the status constants, ARN builders and every record type for every resource. Split each record, its ARN builder and its statuses into the owning resource. |
| `handlers/ecs/scheduler.go` | 449 | cross-resource orchestration + temporary residue | Lease, service reconcile loop, instance reaper and role converge are orchestration. `onRegister`, `onHeartbeat` and `onTaskState` (:236-320) are residue because nothing publishes those subjects. |
| `handlers/ecs/service.go` | 706 | mixed: facade + `cluster` + `taskdefinition` + shared KV helpers | `ECSService` (:27-81) is an AWS-shaped facade that also serves as an internal API. `Deps` (:98) is composition. Cluster CRUD (:231-411) belongs to `cluster`. Task-definition CRUD and `ParseTaskDefRef` (:416-706) belong to `taskdefinition`. `getJSON`, `putJSON` and `keysWithPrefix` (:164-225) are shared persistence helpers. |
| `handlers/ecs/service_nats.go` | 177 | AWS protocol adapter (gateway→daemon NATS hop) | `NATSECSService` forwards 34 methods onto `ecs.<Action>`. It is also used as an internal API by `accountteardown` and by gateway PassRole resolution. |
| `handlers/ecs/services.go` | 786 | mixed: resource owner (`service`) + orchestration bindings | Service CRUD and reconcile (:128-665) belong to `service`. `targetRegistrar` / `natsTargetRegistrar` (:28-59, ELBv2) and `eipManager` / `natsEIPManager` (:64-121, EC2 EIP) are consumer-owned capabilities, but their NATS bindings belong at the composition root. `listTaskRecords` (:605) reads task records directly. |
| `handlers/ecs/store.go` | 227 | temporary residue (shared account bucket; prerequisite: decide per-owner key namespaces; removal: key helpers move to owners) | One bucket holds the keys of all six resources (:36-150). It also holds the leader bucket factory (:221) and the account enumeration used by every sweep (:186). |
| `handlers/ecs/tags.go` | 277 | cross-resource orchestration (tag dispatch) | It parses the ARN and then reads and writes each resource's record directly (:76-203). It should become one tag command per owner behind ARN dispatch. |
| `handlers/ecs/task_stop.go` | 301 | mixed: resource owner (`task`) + orchestration | `StopTask`, `StartTask`, `reserveOnInstance`, `requestStopTask` and `forceStopTask` (:28-209) belong to `task`, with placement mixed into `reserveOnInstance`. `applyServiceTargets` (:235) and `assignTaskPublicIP` / `releaseTaskPublicIP` (:264-301) are ELBv2 and EIP orchestration. |
| `handlers/ecs/tasks.go` | 438 | mixed: resource owner (`task`) + placement orchestration + wire | `RunTask`, Describe, List and the record/projection code belong to `task`. `reservePlacement` (:180) does a CAS on the container-instance record. `publishAssign` (:249) writes the agent inbox. |
| `handlers/ecs/tasks_gc.go` | 130 | resource owner (`task`) reconciliation | It retains STOPPED tasks, retries owed ENI releases and prunes old tasks. It is hosted on `Scheduler` (:21). |
| `handlers/ecs/userdata.go` | 58 | guest/controller wire contract | It writes the cloud-config that seeds the agent env (`ECS_GATEWAY_URL`, `ECS_GATEWAY_CA`, `ECS_REGION`, `ECS_CLUSTER`) (:31-58). |
| `gateway/ecs.go` | 80 | AWS protocol adapter | `ECS_Request` (:26) does target parsing, the scope gate (:54-60), the PassRole closure (:65-67) and response encoding. |
| `gateway/ecs/handler.go` | 165 | AWS protocol adapter | It holds the authoritative action table (:50-112), `StubbedActionNames` (:115), the raw-JSON action list (:131) and the JSON 1.1 writers. |
| `gateway/ecs/actions.go` | 452 | AWS protocol adapter (mixed: PassRole resolution) | Each action decodes and forwards over NATS. `checkTaskLaunchRoles` (:44) queries `DescribeTaskDefinition` over the ECS NATS facade to find role ARNs. |
| `gateway/ecs/authz.go` | 350 | AWS protocol adapter | The per-action resource-ARN scope table (:57-124) and resolver (:139-350) are built on the `handlers_ecs` parsers and ARN builders. |
| `gateway/ecs/azrebalancing.go` | 103 | AWS protocol adapter | It rejects `availabilityZoneRebalancing=ENABLED` (:28) and adds the SDK-gap field to service responses (:54). |
| `gateway/ecs/clienttoken.go` | 86 | AWS protocol adapter holding durable retry state (flag) | It holds the RunTask ClientToken store `spinifex-ecs-clienttokens` (:22). ADR-0003 S3 says an adapter does not own retry bookkeeping, so this belongs with `task`. |
| `daemon/ecs_deps.go` | 63 | composition root (nearest class: cross-resource orchestration) | It binds IAM, images, `RunWorkerInstance` and the gateway URL/CA into `handlers_ecs.Deps` (:17-63). |
| `daemon/daemon.go` (ECS sections only) | n/a | AWS protocol adapter transport + composition | Import at :74, fields at :168-169, 34 `ecs.<Action>` subscriptions at :1131-1167, construction at :2023-2043. |
| `accountteardown/reapers_ecs.go` | 69 | cross-resource orchestration (account teardown) | It has one cluster reaper that calls the ECS AWS facade over NATS (:20-58). |
| `gateway/operations.go` (:7, :30-32) | n/a | AWS protocol adapter | It registers the ECS operation inventory from `gateway_ecs.Actions` and `StubbedActionNames`. |
| `gateway/gateway.go` (:301, :403, :523-524, :639) | n/a | AWS protocol adapter | Service routing to `ECS_Request` and the `ecs-tasks.amazonaws.com` PassRole principal constant live here. |

Other ECS-aware code outside scope, with no import of ECS:
`handlers/sts/assume_role.go:47-55,350-355` duplicates the `ecsInstanceRole` name and the `ecs-tasks.amazonaws.com` principal, and documents that it cannot import `handlers_ecs`.
`foundation/aws/tags/tags.go:24` defines `ManagedByECS` for AMI resolution.
`operator/imagecatalog/images.go:611-637` defines the `spinifex-ecs-node` and `spinifex-ecs-node-gpu` system AMIs.
`domains/acm/renewal.go:169` is a comment-only reference.
`../internal/ecsgw/client.go` is the agent's SigV4 gateway client, with target prefix at :29 and :101.

Guest agent counterparty (`agents/ecs/agent/`), not inventoried file by file:
It reaches the control plane only through signed gateway HTTP (`controlplane.go:18-35`), not NATS.
It imports both `handlers/ecs/bus` and the `handlers_ecs` implementation package (`registration.go:37-38`, `controlplane.go:128,142,150`, `taskrun.go:416-419`).
That is the ADR-0004 S5 debt: the agent imports a domain implementation package to get wire types.

## 2. Served AWS actions and protocol

Protocol: AWS JSON 1.1, with the action taken from the `X-Amz-Target` suffix (`gateway/ecs.go:15-20`, any prefix accepted).
Requests use the canonical prefix `AmazonEC2ContainerServiceV20141113` (`gateway/ecs/handler.go:26`).
Responses are encoded with `jsonutil` (epoch-second times), except `PollAssignments`, which uses `encoding/json` (RFC3339 times) (`handler.go:131-157`).
Errors are `awserrors` codes rendered by the shared ErrorHandler.
An action missing from the table returns `InvalidAction` (`gateway/ecs.go:32-36`), and a stub returns `NotImplemented` (501) (`handler.go:43-45`).
Request order is: account check (:40-46), bounded body (:48), `ResourceARNs` scope derivation (:54), `checkPolicyResources(r, "ecs", action, …)` (:58), handler with PassRole closure (:69), then encode.
Gateway to daemon is a NATS request on `ecs.<Action>` with a 30 s timeout (`handlers/ecs/service_nats.go:12,29-177`), served by queue group `spinifex-workers` (`daemon/daemon.go:1131-1167`).
`gateway/operations.go:30-32` publishes Registered = all 39 table keys and Stubbed = 5.
`ecsScopes` (`authz.go:57-124`) has an entry for every table action, and `ResourceARNs` fails closed if an entry is missing (:140-144).

| Action | Status | Resource | Handler (daemon) |
|---|---|---|---|
| CreateCluster | registered | cluster | `service.go:231` |
| DescribeClusters | registered | cluster (+counts from instance/task/service) | `service.go:268` |
| ListClusters | registered | cluster | `service.go:338` |
| DeleteCluster | registered | cluster (cascade orchestration) | `cluster_teardown.go:24` |
| UpdateCluster | stubbed | cluster | — |
| PutClusterCapacityProviders | registered | cluster (references capacityprovider) | `capacity_providers.go:16` |
| CreateCapacityProvider | registered | capacityprovider | `capacity_providers.go:44` |
| DescribeCapacityProviders | registered | capacityprovider | `capacity_providers.go:80` |
| DeleteCapacityProvider | registered | capacityprovider | `capacity_providers.go:125` |
| RegisterTaskDefinition | registered (PassRole on task/execution role, `actions.go:99`) | taskdefinition | `service.go:416` |
| DeregisterTaskDefinition | registered | taskdefinition | `service.go:546` |
| DescribeTaskDefinition | registered | taskdefinition | `service.go:571` |
| ListTaskDefinitions | registered | taskdefinition | `service.go:586` |
| ListTaskDefinitionFamilies | stubbed | taskdefinition | — |
| RunTask | registered (PassRole via taskdef lookup, ClientToken) | task (+placement, containerinstance) | `tasks.go:28` |
| StartTask | registered (PassRole via taskdef lookup) | task (+containerinstance) | `task_stop.go:53` |
| StopTask | registered | task | `task_stop.go:28` |
| DescribeTasks | registered | task | `tasks.go:273` |
| ListTasks | registered | task | `tasks.go:299` |
| SubmitTaskStateChange | registered (AWS action; used by agent) | task | `instances.go:445` |
| PollAssignments | registered (non-AWS, agent-only, raw JSON) | task inbox (wire) | `poll.go:37` |
| ReportTaskGPU | registered (non-AWS, agent-only) | task | `gpu_report.go:31` |
| CreateService | registered (PassRole via taskdef lookup; AZ-rebalancing gate) | service | `services.go:128` |
| UpdateService | registered (same) | service | `services.go:209` |
| DeleteService | registered | service | `services.go:267` |
| DescribeServices | registered | service | `services.go:302` |
| ListServices | registered | service | `services.go:326` |
| ListServicesByNamespace | stubbed | service | — |
| RegisterContainerInstance | registered (agent boot + 30 s liveness) | containerinstance | `instances.go:65` |
| DeregisterContainerInstance | registered | containerinstance | `cluster_teardown.go:60` |
| UpdateContainerInstancesState | registered | containerinstance | `cluster_teardown.go:103` |
| DescribeContainerInstances | registered | containerinstance | `instances.go:127` |
| ListContainerInstances | registered | containerinstance | `instances.go:167` |
| ProvisionCapacity | registered (non-AWS) | containerinstance capacity (EC2 launch orchestration) | `capacity.go:70` |
| PutAccountSetting | stubbed | account setting | — |
| ListAccountSettings | stubbed | account setting | — |
| TagResource | registered | cross-resource (by ARN) | `tags.go:227` |
| UntagResource | registered | cross-resource (by ARN) | `tags.go:254` |
| ListTagsForResource | registered | cross-resource (by ARN) | `tags.go:207` |

Unsupported actions are any ECS action absent from the table, and they return `InvalidAction`.
Examples include task sets, `ExecuteCommand`, `UpdateCapacityProvider`, `DeleteTaskDefinitions`, attribute APIs and service deployments/revisions.
Three agent-facing actions (`PollAssignments`, `ReportTaskGPU`, `ProvisionCapacity`) are non-AWS shapes on the AWS ECS signing surface.
`SubmitTaskStateChange` is an AWS action that our agent uses as its state-report channel.

## 3. Durable records / KV buckets

| Bucket | Key shape | Record type | Owning resource | Writers | Readers |
|---|---|---|---|---|---|
| `ecs-account-<accountID>` (history 1, schema v1, created lazily, `store.go:27-29,207-217`) | `clusters/<name>/meta` | `ClusterRecord` (`records.go:155`) | cluster | CreateCluster `service.go:259`; PutClusterCapacityProviders `capacity_providers.go:36`; tags `tags.go:152`; DeleteCluster removes the subtree `cluster_teardown.go:47` | cluster CRUD; RunTask/StartTask/CreateService existence checks (`tasks.go:36`, `task_stop.go:60`, `services.go:148`) |
| same | `clusters/<c>/instances/<id>` | `InstanceRecord` (`records.go:333`) | containerinstance | `upsertInstance` plain Put `instances.go:215`; `recordHeartbeat` `:356`; UpdateContainerInstancesState `cluster_teardown.go:131`; reaper `scheduler.go:390`; tags `tags.go:200`; CAS reserve/release `tasks.go:212`, `task_stop.go:148`, `instances.go:527`; Deregister deletes `cluster_teardown.go:90` | placement (`tasks.go:182`), cluster counts (`service.go:301`), reaper, describe/list |
| same | `clusters/<c>/tasks/<taskID>` | `TaskRecord` (`records.go:389`) | task | RunTask `tasks.go:84`; StartTask `task_stop.go:104`; stop/force-stop `task_stop.go:171,199`; recordTaskState `instances.go:409,433`; EIP assign `task_stop.go:284`; ENI rollback `tasks.go:168`; GPU report `gpu_report.go:58`; sweep `tasks_gc.go:98,119`; tags `tags.go:189` | service reconcile (`services.go:589-622`), cluster/instance counts, reaper, sweep |
| same | `clusters/<c>/assignments/<ciID>/<taskID>` | `bus.Assign` | task (agent inbox) | `publishAssign` `tasks.go:269` | agent via `PollAssignments` (`poll.go:58`); acked and reclaimed via `poll.go:49,97` |
| same | `clusters/<c>/stops/<ciID>/<taskID>` | `bus.StopDirective` | task (agent inbox) | `requestStopTask` `task_stop.go:176` | agent via `PollAssignments` (`poll.go:74`); acked and reclaimed via `poll.go:55,106` |
| same | `clusters/<c>/services/<name>` | `ServiceRecord` (`records.go:454`) incl. `Deployments`, `Events` | service | Create/Update/Delete `services.go:198,256,295`; reconcile `services.go:433`; deployment-failure count `instances.go:496`; tags `tags.go:178` | task paths read it for LB and EIP config (`task_stop.go:241,273`) |
| same | `taskdef-families/<family>/latest-rev`, `taskdef-families/<family>/revs/<rev>` | `int`, `TaskDefRecord` (`records.go:280`) | taskdefinition | Register `service.go:452,455`; Deregister `:564`; tags `tags.go:167` | `resolveTaskDef` (`service.go:622`) for RunTask/StartTask/Create/UpdateService; gateway PassRole via NATS `DescribeTaskDefinition` |
| same | `capacity-providers/<name>` | `CapacityProviderRecord` (`records.go:200`) | capacityprovider | Create `capacity_providers.go:72`; Delete `:142` | Describe only |
| `spinifex-ecs-leader` (TTL 60 s, `store.go:31-33,221`) | `scheduler` (`scheduler.go:24`) | kvlease lease | scheduler (orchestration) | `kvlease` in `Scheduler` (`scheduler.go:96-109`) | the same |
| `spinifex-ecs-clienttokens` (TTL 15 min, `gateway/ecs/clienttoken.go:22-26`) | `<accountID>.<token>` (`foundation/lifecycle/idempotency/store.go:91-93`, no namespace) | `idempotency` entry with `ecs.RunTaskOutput` | task (currently held by the adapter) | gateway `runTaskWithClientToken` (`clienttoken.go:71-86`) | the same |

`store.go:23-25`, `:134-138` and `LeaderLeaseKey` describe per-cluster `<accountID>/<clusterName>` lease keys, but the code uses one global key `scheduler`, and `LeaderLeaseKey` has no production caller.
No reader outside ECS opens any of these buckets.
A Go grep for bucket names and helpers outside `handlers/ecs` and `gateway/ecs` found only RDS's own `AccountBucketName`.
`accountteardown` and the agent reach ECS state only through the ECS API (NATS facade or gateway), not through KV.
No KV migration is registered for `ecs-account-*`; it runs `migrate.DefaultRegistry.RunKV` at version 1 (`store.go:213`).
No production code was found that deletes an `ecs-account-*` bucket.

## 4. Identity and scope

The partition is hard-coded to `aws`.
The Region is the daemon `config.Region` captured at construction (`daemon/daemon.go:2029`).
The account is the SigV4 caller's account, carried on every NATS request.
All records live in the caller's per-account bucket, so the account scope is structural.

| Resource | Identifier | ARN (`records.go:107-135`) | Scope |
|---|---|---|---|
| cluster | name; omitted means `default` (`conv.go:27-36`, `service.go:233-235`) | `arn:aws:ecs:<region>:<acct>:cluster/<name>` | account + Region; no AZ |
| taskdefinition | `family:revision`; revision is `latest-rev + 1` (`service.go:531-541`) | `…:task-definition/<family>:<rev>` | account + Region |
| service | name unique per cluster | `…:service/<cluster>/<name>` | account + Region; subnets/SGs stored, not validated |
| task | UUIDv4 (`tasks.go:65`, `task_stop.go:85`) | `…:task/<cluster>/<taskID>` | account + Region; AZ is implicit through its container instance and is not stored on the task |
| containerinstance | the `InstanceIdentityDocument` string supplied by the agent (in practice the EC2 instance ID), with fallback `ci-<UTC timestamp>` (`instances.go:67-70`) | `…:container-instance/<cluster>/<id>` | account + Region; AZ from the registration attribute (`instances.go:100-101,331`) |
| capacityprovider | name | `…:capacity-provider/<name>` | account + Region |

AWS parity note (verify): AWS mints a UUID container-instance ID that is distinct from `ec2InstanceId`, and AWS expects `instanceIdentityDocument` to be the signed IMDS JSON document.
Ours uses the supplied string as both values (`instances.go:276`).
Identifier reuse: DeleteCluster removes every record (`cluster_teardown.go:47`), so a cluster name and its child names are reusable immediately.
There is no tombstone or identifier-reuse rule (Q-43/Q-44).
Task-definition revisions are not reserved against reuse after a concurrent collision (see gap G9).
Container instances authenticate as the account's `ecsInstanceRole` (`instancerole.go:12-30`).
`PollAssignments`, `SubmitTaskStateChange` and `ReportTaskGPU` check only account scope plus IAM policy on the named instance or task ARN (`authz.go:83-95`).
The handlers do not bind the caller to the specific container instance (`poll.go:37-56`, `instances.go:445-475`, `gpu_report.go:31`).

## 5. Lifecycle paths per resource

There are no desired or observed generation fields on any ECS record (`records.go:155-493`).
The only fencing mechanisms are:
- one global scheduler lease (`scheduler.go:96-109`), checked only at the start of each pass (`:170`, `:189`);
- CAS (`kv.Update` with revision) on container-instance reservations (`tasks.go:212`, `task_stop.go:148`, `instances.go:527`).

Every other write is a read-modify-`Put` without revision (`service.go:179-188`).
ClientToken idempotency exists only for RunTask (`gateway/ecs/actions.go:203-238`).

### cluster
Create is `CreateCluster` (`service.go:231-264`): get, then a plain Put if absent.
A repeat create by name returns the existing record without comparing parameters; AWS also treats it as idempotent (verify).
Readiness: the cluster is `ACTIVE` on create (`:250`), synchronously.
There is no reconcile loop.
Delete is `DeleteCluster` (`cluster_teardown.go:24-53`): it force-stops every task (:43-45), deletes the whole `clusters/<n>/` subtree (:47), and returns a synthetic `INACTIVE` (:51).
No durable deletion intent is written.
On teardown it is driven by `accountteardown/reapers_ecs.go:53-58`.

### taskdefinition
Create is `RegisterTaskDefinition` (`service.go:416-459`): it validates (`:472-511`), computes `rev = latest + 1` without CAS (`:530-541`), Puts the revision (`:452`), then Puts `latest-rev` (`:455`).
There is no client token, and AWS has none for this action.
Readiness: the definition is `ACTIVE` synchronously.
Delete is `DeregisterTaskDefinition` (`:546-568`), which sets `INACTIVE` and retains the record.
There is no hard delete (`DeleteTaskDefinitions` is not served).
`resolveTaskDef` (`:622-647`) does not check status, so RunTask, StartTask and CreateService accept an `INACTIVE` revision.
AWS refuses new runs and services on an INACTIVE revision after a short window (verify).

### service
Create is `CreateService` (`services.go:128-206`): an `ACTIVE` service with the same name is returned as-is (:159-160), and `ClientToken` is ignored.
It Puts the record with a PRIMARY deployment (:196-198), then runs `reconcileService` synchronously on the serving worker (:202).
Readiness: the service record is `ACTIVE` immediately.
Rollout state moves `IN_PROGRESS` → `COMPLETED` when the primary running count reaches the desired count and no old-deployment tasks remain (`deployments.go:170-196`).
The circuit breaker sets `FAILED` after 3 failed starts (`deployments.go:222-243`, `records.go:54`), with optional rollback (`:247-267`).
Launch backoff is 15 s to 5 min (`deployments.go:206-216`).
Reconcile has two triggers:
- the leader loop `ecs/services` (`scheduler.go:143-148`) runs `reconcileAllServices` (`services.go:445-476`) on a wake from `ecs.bus.<acct>.<cluster>.service-reconcile` (`instances.go:472`), a 10 s revisit while unsettled, and a 5 min resync;
- `CreateService` and `UpdateService` also reconcile inline on the API worker (`services.go:202`, `:259`).
The reconcile launches through `s.RunTask` without a client token (`services.go:495-525`) and stops surplus tasks (`:531-581`).
Delete is `DeleteService` (`:267-299`): it requests a stop for each task (:288-289), then sets `INACTIVE` immediately (:291).
`ServiceStatusDraining` (`records.go:30`) is never written.
There is no `force` or desired-count precondition check, and the INACTIVE record is never pruned.

### task
Create is `RunTask` (`tasks.go:28-93`) or `StartTask` (`task_stop.go:53-113`), in this order:
1. Reserve capacity by CAS on the container instance (`tasks.go:180-218` / `task_stop.go:118-153`).
2. Build the record in memory (`tasks.go:221-243`; desired `RUNNING`, last `PENDING`).
3. For awsvpc, create and attach the ENI through EC2 (`tasks.go:131-176`).
4. Make the first durable task write (`tasks.go:84` / `task_stop.go:104`).
5. Write the assignment inbox (`tasks.go:249-270`); a failure there is only logged (`:87-89`).
Readiness comes from the agent's `SubmitTaskStateChange`, which goes through `recordTaskState` (`instances.go:361-439`).
`RUNNING` sets `StartedAt`, registers ELBv2 targets and assigns an EIP when the service asks for it (:415-418).
`STOPPED` releases the target, EIP, inboxes, ENI and reservation (:422-436).
Stop has two paths:
- `StopTask` calls `requestStopTask` (`task_stop.go:162-179`), which sets desired `STOPPED` and posts a stop directive for the agent.
- `forceStopTask` (`:186-209`) is used by the cluster delete, deregister, drain and reaper paths; it marks the task `STOPPED` without telling the agent.
Repair: the leader `ecs/timers` loop runs `sweepStoppedTasks` (`tasks_gc.go:21-105`).
That sweep retries owed ENI releases (at most 20 per pass, 30 s to 15 min backoff) and deletes STOPPED tasks after 1 h.
Idempotency: RunTask honours ClientToken through `idempotency.Do` (`gateway/ecs/clienttoken.go:71-86`); a partial placement is finalized and replayed.
StartTask has no token, and AWS has none for it (verify).
Internal launch from the service reconcile is not idempotent per logical launch.

### containerinstance
Create is `RegisterContainerInstance` (`instances.go:65-124`), which is an upsert with a plain Put (`:195-219`).
The agent calls it at boot and on every 30 s heartbeat (`agents/ecs/agent/controlplane.go:22-25`).
Readiness: the instance is `ACTIVE` on first registration (`:207`).
A zero reported capacity drains it (`Reaped=true`), and returning capacity restores it (`:44-60`).
`AgentConnected` is derived from `Status == ACTIVE` (`:278`), not from liveness.
Repair: the leader reaper (`scheduler.go:340-396`) marks an instance `DRAINING` with `Reaped=true` after 90 s without `LastSeen`, and force-stops its tasks (`:400-416`).
`recordRegister` and `recordHeartbeat` (`instances.go:325-357`) are fed only by bus subjects that nothing publishes.
Operator state: `UpdateContainerInstancesState` (`cluster_teardown.go:103-140`) sets the status.
`DRAINING` force-stops service tasks immediately (`:160-171`).
Delete is `DeregisterContainerInstance` (`:60-97`): it refuses if tasks are active unless `force`, deletes the assignments inbox and the record (the stops inbox is left), and does not touch the EC2 instance.
Capacity is provisioned by `ProvisionCapacity` (`capacity.go:70-170`), which calls `RunInstances` directly with no ECS-side intent record and no idempotency.

### capacityprovider
Create is `CreateCapacityProvider` (`capacity_providers.go:44-76`): an existing name returns the existing record without comparing parameters, and the ASG ARN is stored without validation.
Readiness: it is `ACTIVE` immediately and inert, with no scheduler coupling (`service.go:74-76`).
Delete is a hard key delete (`:125-146`), with no check that a cluster references the provider.

## 6. NATS subjects and guest/controller messages

| Subject | Direction / type | Payload | Retry / redelivery |
|---|---|---|---|
| `ecs.<Action>` ×34 (`service_nats.go:29-177`; subs `daemon.go:1134-1167`) | gateway (and `accountteardown`, gateway PassRole lookup) → daemon, request/reply, queue `spinifex-workers` | aws-sdk-go ECS input/output or `PollAssignments*`, `ReportTaskGPU*`, `ProvisionCapacity*` structs | 30 s timeout. There is no retry in `NATSECSService`, and `natsmsg.NATSRequest` (`foundation/messaging/nats/nats.go:269`) retry behaviour was not verified here. |
| `ecs.bus.<acct>.<cluster>.service-reconcile` (`bus/subjects.go:43`) | any worker → leader, core publish | none | Fire-and-forget (`instances.go:472-474`); loss is covered by the 10 s revisit and 5 min resync (`scheduler.go:30-58`). |
| `ecs.bus.*.*.instance-register.*`, `…instance-heartbeat.*`, `…task-state.*` (`scheduler.go:236-245`) | subscribed by leader | `bus.RegisterInstance`, `bus.Heartbeat`, `bus.TaskState` | No production publisher (residue). Core NATS with no ack or redelivery, and messages are dropped while there is no leader. |
| `ecs.bus.<acct>.<cluster>.assign.<id>` (`bus/subjects.go:30`) | unused | — | Residue; assignment moved to the KV inbox. |
| `ec2.CreateNetworkInterface`, `ec2.DeleteNetworkInterface` (`eni.go:64,119`) | ECS → EC2 AWS-action subjects | aws-sdk-go EC2 types | 30 s timeout (`eni.go:19`). NotFound counts as converged (`:148-155`). A failed release is retried by the sweep. |
| `ec2v1.InstanceCommandSubject(<instanceID>)` (`eni.go:126-128`) | ECS → owning node, `contracts/ec2/v1` | `EC2InstanceCommand{AttachENI or DetachENI}` | Versioned contract; detach is forced (`:109`). |
| `ec2.AllocateAddress`, `ec2.AssociateAddress`, `ec2.DescribeAddresses`, `ec2.DisassociateAddress`, `ec2.ReleaseAddress` via `ec2eip.NewNATSEIPService` (`services.go:81-121`; subjects in `domains/ec2/eip/service_nats.go:26-34`) | ECS → EC2 EIP AWS adapter | aws-sdk-go EC2 types | A failed associate releases immediately (`services.go:91-96`). A failed release is logged, and the fields are cleared anyway (`task_stop.go:296-300`). |
| ELBv2 `RegisterTargets` / `DeregisterTargets` via `handlers_elbv2.NewNATSELBv2Service` (`services.go:45-58`) | ECS → ELBv2 AWS adapter | aws-sdk-go ELBv2 types | Best effort and log-only (`task_stop.go:252-255`), with no retry. The exact subject string was not verified. |
| `ec2.RunInstances.<type>.<node>` | not used by ECS | — | ECS passes no node, so `RunWorkerInstance` runs the local in-process `instanceService.RunInstances` (`daemon/eks_worker_launch.go:29-51`). |

Guest protocol: agent ↔ gateway HTTP, SigV4-signed with the instance role through `internal/ecsgw`; it is not NATS.
- `RegisterContainerInstance`: boot, then every 30 s as liveness.
- `PollAssignments`: delivery is at-least-once. The inboxes are KV keys, and an entry is redelivered on every poll until the agent acks it in `AckTaskIDs` or `AckStopIDs` on the next poll (`poll.go:45-56`), or until the task stops (`instances.go:430-431`, `task_stop.go:204-205`).
- `SubmitTaskStateChange`: duplicate reports are tolerated only through edge detection on `prev != status` (`instances.go:415,422`). There is no ordering or monotonicity guard and no check of the reporting instance (`:372-373`).
- `ReportTaskGPU`.
Bootstrap contract: cloud-config writes `/etc/spinifex-ecs/agent.env` and the gateway CA (`userdata.go:31-58`).

## 7. Boundary-crossing imports

ECS → other packages (non-stdlib, non-foundation first):

| From | Imports | Symbol(s) | Flag |
|---|---|---|---|
| `handlers/ecs/service.go:19`, `instancerole.go:6` | `handlers/iam` | `SystemInstanceRoleEnsurer` (`service.go:92`), `EnsureSystemInstanceProfile` (`instancerole.go:38`), `ConvergeSystemRolePolicy` (`:46`) | IAM |
| `handlers/ecs/services.go:20` | `handlers/elbv2` | `NewNATSELBv2Service().RegisterTargets/DeregisterTargets` | other domain's AWS adapter used as an internal API |
| `handlers/ecs/services.go:18` | `domains/ec2/eip` | `NewNATSEIPService` (Allocate/Associate/Describe/Disassociate/Release) | EC2 / network; AWS adapter as internal API |
| `handlers/ecs/eni.go:13` | `contracts/ec2/v1` | `EC2InstanceCommand`, `EC2CommandAttributes`, `AttachENIData`, `DetachENIData`, `InstanceCommandSubject` | EC2 instance and network (versioned contract) |
| `handlers/ecs/eni.go:64,119` | string subjects `ec2.CreateNetworkInterface` / `ec2.DeleteNetworkInterface` | — | network; EC2 AWS-action subject used as an internal API |
| `handlers/ecs/capacity.go:11-12` | `domains/ec2/ebs/metadata`, `domains/ec2/instancetypes` | `RootDeviceName`; `IsGPUTypeName`, `GPUVendorForType` | EC2 |
| `handlers/ecs/placement.go:9` | `domains/ec2/instancetypes` | `HotPlugENISlotsForType` | EC2 |
| `handlers/ecs/service.go:16`, `capacity.go:10`, `eni.go:12`, `services.go:15-17` | aws-sdk-go `service/ec2`, `service/elbv2` | `RunInstancesInput`, `DescribeImages*`, ENI/EIP/target types | EC2 / ELBv2 types |
| `handlers/ecs/*` | foundation | `messaging/nats.NATSRequest`, `state/kvlease`, `state/kvutil`, `state/migrate`, `lifecycle/reconciler`, `aws/errors`, `aws/arn`, `aws/ami`, `aws/tags` | — |
| `gateway/ecs/authz.go:11-12` | `ingress/aws/bodyscope`, `ingress/aws/query` | `bodyscope.Parse`, `query.MaxSliceLen` | ingress |
| `gateway/ecs/clienttoken.go:13` | `foundation/lifecycle/idempotency` | `OpenStore`, `Do`, `ParamHash`, `ErrParamMismatch`, `ErrUnavailable` | — |
| `daemon/ecs_deps.go:8` | `github.com/mulgadc/bluebottle/pkg/masterkey` | `ReadShared` (:39) | Bluebottle (daemon wiring only; `handlers/ecs` itself does not import Bluebottle) |
| `daemon/ecs_deps.go:10,50-52` | `handlers/iam` | `NewIAMServiceImpl`, `*IAMServiceImpl` | IAM |
| `daemon/ecs_deps.go:29,32` | EKS-named daemon helpers | `resolveGatewayBaseURL` (`daemon/eks_deps.go:164`), `RunWorkerInstance` (`daemon/eks_worker_launch.go:29`) | EC2 instance launch shared with EKS |

Packages that import ECS:

| Importer | Imports | Symbol(s) |
|---|---|---|
| `gateway/ecs/actions.go:12` | `handlers/ecs` | `NewNATSECSService`, `ECSService`, `ProvisionCapacityInput`, `PollAssignmentsInput`, `ReportTaskGPUInput` |
| `gateway/ecs/authz.go:10` | `handlers/ecs` | `ClusterShortName`, `ServiceShortName`, `TaskShortID`, `ContainerInstanceShortID`, `CapacityProviderShortName`, `ParseTaskDefRef`, `ResourceARNSegment`, `ClusterARN`, `ServiceARN`, `TaskARN`, `ContainerInstanceARN`, `CapacityProviderARN`, `TaskDefARN`, `TaskDefRefARN` |
| `gateway/ecs.go:10`, `gateway/operations.go:7` | `gateway/ecs` | `Actions`, `ResourceARNs`, `RawJSONActions`, `WriteJSONResponse`, `WriteRawJSONResponse`, `StubbedActionNames` |
| `daemon/daemon.go:74`, `daemon/ecs_deps.go:9` | `handlers/ecs` | `Service`, `Scheduler`, `NewService`, `WithDeps`, `InitLeaderBucket`, `NewScheduler`, `Deps` |
| `accountteardown/reapers_ecs.go:9` | `handlers/ecs` | `NewNATSECSService`, `ECSService` (`ListClusters`, `DeleteCluster`) |
| `agents/ecs/agent/{registration,controlplane,taskrun}.go` | `handlers/ecs` (implementation package) | `AttrInstanceType`, `AttrAvailabilityZone`, `PollAssignmentsInput/Output`, `ReportTaskGPUInput`, `ContainerGPUReport` |
| `agents/ecs/agent/{agent,controlplane,reconcile,registration,taskrun}.go` | `handlers/ecs/bus` | `Assign`, `StopDirective`, `TaskState`, `ContainerStatus`, `InstanceCapacity`, status constants |

Explicit flags:
- **IAM / PassRole.** PassRole is enforced only in the gateway (`gateway/ecs.go:65-67`, through `gw.checkPassRole` at `gateway/gateway.go:645-648` with `iam:PassedToService=ecs-tasks.amazonaws.com`).
  RegisterTaskDefinition checks the role ARNs in the body (`actions.go:99`).
  RunTask, StartTask, CreateService and UpdateService first fetch the task definition through the ECS NATS facade (`actions.go:44-56`).
  `ProvisionCapacity` attaches the system `ecsInstanceRole` instance profile (`capacity.go:103,141`) and launches through `RunWorkerInstance`, which goes straight to `d.instanceService.RunInstances` (`daemon/eks_worker_launch.go:51`).
  That bypasses the EC2 gateway's PassRole closure (`gateway/ec2.go:199`).
  `handlers/sts/assume_role.go:52-55` duplicates `ecsInstanceRole`, which is implicit coupling with no import.
- **Quota.** `handlers/ecs` has no quota import.
  `ProvisionCapacity` appears to bypass `gw.Quota.EnforceLaunch` and `ChargeLaunch` (`gateway/ec2.go:196-205`), because it calls the in-process instance service (verify whether `instanceService.RunInstances` enforces quota itself).
  ECS resources themselves have no quota.
- **EC2 instance.** `RunInstances` (via `Deps.RunInstances`), `DescribeImages` (via `Deps.Images` = `d.imageService`), `instancetypes`, EBS root-device metadata, and the `contracts/ec2/v1` instance command for ENI hot-plug.
- **Network.** ENI create/delete through EC2 AWS subjects, EIP through the `domains/ec2/eip` NATS adapter, subnets and SGs passed through unvalidated.
- **Bluebottle.** Only `masterkey.ReadShared` in `daemon/ecs_deps.go:39`, used to build the IAM service.
  Without it, `ProvisionCapacity` is disabled and the scheduler refuses leadership (`scheduler.go:226-231`).

## 8. Known gaps against Q-78 / ADR-0003 (observed)

- **G1. No generations.** No ECS record has a desired or observed generation (`records.go:155-493`). Reconcilers act on whole records with plain Put (`service.go:179-188`).
- **G2. Realization before intent.** RunTask and StartTask reserve capacity and create/attach an ENI before the task record exists (`tasks.go:66-84`, `task_stop.go:86-104`). A crash in that window leaves a reservation (`PlacedTasks` naming an unknown task) and possibly an ENI with no owning record. No reconciler targets orphan reservations; `tasks_gc.go` handles only owed ENI release on STOPPED tasks.
- **G3. Lost-update window on instance reservations.** Reservations use CAS (`tasks.go:212`, `task_stop.go:148`, `instances.go:527`), but the same key is rewritten with a plain read-modify-Put every 30 s by agent registration (`instances.go:195-215`). The same happens in UpdateContainerInstancesState (`cluster_teardown.go:131`), the reaper (`scheduler.go:390`) and tags (`tags.go:200`), so a CAS commit between their Get and Put can be overwritten. `handlers/ecs/task_accounting_test.go` (`TestDescribeContainerInstances_StalePlacedTasksDoNotInflateCount`) acknowledges stale `PlacedTasks`.
- **G4. Unfenced concurrent writers.** The leader lease is checked only at pass start (`scheduler.go:170,189`). KV writes are not fenced by the lease. API workers also run `reconcileService` inline (`services.go:202,259`) while the leader runs `reconcileAllServices` (`scheduler.go:177`), so two writers can launch through `RunTask` for the same service. This is the observed code shape; no test was found covering it.
- **G5. Cascade delete removes cleanup ownership.** `DeleteCluster` (`cluster_teardown.go:43-47`) deletes the whole subtree, including STOPPED tasks whose `ENIReleased=false` or whose EIP or target cleanup failed. That removes the only record of owed cleanup, which Q-78 forbids. There is no deletion intent or incomplete-cleanup state. AWS refuses DeleteCluster while services, tasks or registered instances exist (verify against AWS docs and a test).
- **G6. Force-stop does effects before persisting.** `forceStopTask` releases the EIP and ENI before persisting STOPPED (`task_stop.go:197-199`). A failed EIP release is logged and its allocation ID cleared (`:296-300`), so it cannot be retried. Target deregistration is best effort with no retry (`:252-255`). `forceStopTask` also posts no stop directive, so whether a live agent stops its containers is unverified.
- **G7. EIP before record.** `assignTaskPublicIP` allocates and associates the EIP before persisting its allocation ID (`task_stop.go:277-286`).
- **G8. Silent reservation leak.** `releaseReservation` gives up silently after 5 CAS conflicts and returns nil (`instances.go:506-531`), which leaks capacity with no reconciler.
- **G9. Non-atomic revision allocation.** `RegisterTaskDefinition` computes the revision as read + 1 with no CAS (`service.go:430,531-541`). It writes the revision before `latest-rev` (`:452-455`), so concurrent registers can produce the same revision and overwrite it.
- **G10. No guard on task-state reports.** `recordTaskState` sets `LastStatus` unconditionally (`instances.go:372-373`), so a late RUNNING after STOPPED regresses the state. The reporting `InstanceID` is ignored.
- **G11. DeleteService is immediate.** It sets INACTIVE directly, with no DRAINING (`services.go:291`; `records.go:30` is unused). It has no `force` or desired-count check, AWS requires one (verify), and the INACTIVE record is never pruned.
- **G12. Create idempotency without comparison.** CreateService ignores `ClientToken` and returns an existing ACTIVE service without comparing parameters (`services.go:159-160`). CreateCapacityProvider (`capacity_providers.go:60-61`) and CreateCluster (`service.go:246-262`) behave the same way.
- **G13. Retry bookkeeping in the adapter.** The RunTask idempotency store lives in the AWS adapter (`gateway/ecs/clienttoken.go:22-56`). ADR-0003 S3 says retry bookkeeping is not the adapter's.
- **G14. ProvisionCapacity.** It has no durable intent and no idempotency, and it silently caps count at 10 (`capacity.go:89-91,158`). Launched instances are linked only by the tag `spinifex:ecs-cluster` (`:150`).
- **G15. Liveness derived from status.** Container-instance `AgentConnected` is derived from Status, not liveness (`instances.go:278`). Registration makes an instance ACTIVE immediately (`:207`).
- **G16. Residue subscriptions.** The scheduler subscribes to three bus subjects that nothing publishes (`scheduler.go:236-245`).
- **G17. AWS adapters as internal APIs.** Account teardown uses the ECS facade (`accountteardown/reapers_ecs.go:21`), and gateway PassRole resolution calls `DescribeTaskDefinition` over NATS (`actions.go:48`). ECS calls the ELBv2 and EIP adapters and the `ec2.*NetworkInterface` subjects (`services.go:46,82`, `eni.go:64,119`). ADR-0003 S4 and ADR-0004 S4 forbid an AWS adapter used as an internal service API.
- **G18. Incomplete account teardown.** It reaps only clusters (`reapers_ecs.go:20-22`). Task definitions, capacity providers and the `ecs-account-*` bucket remain; no deleting code was found.
- **G19. No pagination.** List APIs ignore `nextToken` and `maxResults` (`service.go:338,586`, `tasks.go:299`, `services.go:326`, `instances.go:167`). Q-89 requires listing tests that exceed one page.
- **G20. Status not checked on launch.** RunTask, StartTask and CreateService accept an INACTIVE task-definition revision (`service.go:622-647`), but AWS refuses (verify).
- **G21. Capacity providers not taggable.** Capacity-provider ARNs are rejected by the tag parser (`tags.go:52-71`), although create accepts tags.
- **G22. Delete leaves stop inboxes.** `DeregisterContainerInstance` deletes the assignments inbox but not the stops inbox (`cluster_teardown.go:87-90`).

## 9. First-resource candidates (ranked)

1. **taskdefinition.**
   Size: about 300 lines in `service.go:416-706` plus about 180 in `conv.go:70-265`, plus `TaskDefRecord` and `ContainerDef` (`records.go:211-328`).
   Private state: `taskdef-families/*` only.
   Dependencies: none at the handler. PassRole is checked in the gateway on role ARN strings, so extraction does not require deciding IAM authority. It has no EC2, network or Bluebottle dependency.
   Consumers needing a narrow query: task launch, service create/update (`resolveTaskDef`), gateway PassRole lookup, and tags.
   Tests: `handlers/ecs/taskdef_validation_test.go`, `service_test.go` (`RegisterTaskDefinition_*`, `ListTaskDefinitions_*`, `DescribeTaskDefinition_Unknown`), `cluster_teardown_test.go` (`DeregisterTaskDefinition_*`), `conv_test.go`, `records_test.go`, `tags_test.go` (`TaskDefinitionRoundTrip`).
   Known gaps: G9 and G20.
   Assessment: the best first slice. Its `resolveTaskDef` edge also gives the ADR-0004 rollout item 3 capability seam.
2. **capacityprovider.**
   Size: about 220 lines in `capacity_providers.go` plus records.
   Private state: `capacity-providers/*`.
   Dependencies: none; the provider is inert, and the ASG ARN is opaque.
   Prerequisite split: `PutClusterCapacityProviders` must move to `cluster`.
   Tests: `handlers/ecs/capacity_providers_test.go`.
   Gaps: G12, G21, and an unchecked delete.
   Assessment: the smallest and safest slice, with no IAM, EC2 or Bluebottle dependency. It has almost no consumers, so it demonstrates little about cross-resource capabilities.
3. **cluster.**
   Size: CRUD of about 180 lines (`service.go:231-411`) plus `PutClusterCapacityProviders`.
   Private state: `clusters/<n>/meta`.
   Dependencies: counts read instance, task and service records (`service.go:300-335`), and its existence is checked by task and service launch. The delete cascade is orchestration over every child (G5).
   Tests: `service_test.go` (`CreateCluster_Idempotent`, `DescribeClusters_*`, `ListClusters_*`), `cluster_teardown_test.go` (`DeleteCluster_*`), `capacity_providers_test.go`, `tags_test.go`, `accountteardown/reapers_ecs_test.go`.
   Assessment: a CRUD-only slice is feasible without IAM or Bluebottle. DeleteCluster has to stay as domain-root orchestration until the children have owners.
4. **containerinstance.**
   Size: about 450 lines across `instances.go`, part of `cluster_teardown.go` and `placement.go`.
   Private state: `instances/*` plus the reservation fields, which placement and task CAS-mutate.
   Dependencies: the agent wire contract, EC2 (`instancetypes`; `RunInstances` via `ProvisionCapacity`) and IAM (`ecsInstanceRole`).
   Tests: `instance_recovery_test.go`, `instance_list_status_test.go`, `placement_test.go`, `eni_placement_test.go`, `capacity_test.go`, `scheduler_test.go`, `task_accounting_test.go`, `service_test.go` (GPU/register/heartbeat/reaper).
   Assessment: G3 must be characterized first, and reservation ownership needs a decision. It is not a first slice.
5. **service.**
   Size: about 1,050 lines (`services.go`, `deployments.go`).
   Dependencies: task launch and stop, taskdefinition, ELBv2 targets and EC2 EIP. Its reconcile runs on both the leader and the API workers (G4).
   Tests: `services_test.go`, `deployments_test.go`, `revisit_test.go`, `eip_test.go`, `services_eip_nats_test.go`, gateway `actions_test.go` and `azrebalancing_test.go`.
   Assessment: it depends on the task and taskdefinition owners existing.
6. **task.**
   Size: about 1,300 lines (`tasks.go`, `task_stop.go`, `tasks_gc.go`, `poll.go`, `eni.go`, `netcfg.go`, `gpu_report.go`, part of `instances.go`).
   Dependencies: EC2 ENI and EIP, ELBv2, container-instance reservations, the agent contract, the ClientToken store and gateway PassRole.
   Tests: `eni_test.go`, `eni_placement_test.go`, `tasks_gc_test.go`, `task_accounting_test.go`, `poll_test.go`, `gpu_report_test.go`, `service_test.go` (`RunTask_*`, `RecordTaskState_*`), gateway `clienttoken_test.go`, `actions_test.go`, and e2e `tests/e2e/ecs/ecs_test.go` and `tests/e2e/gpu/ecs_gpu_test.go` in the mulga root.
   Assessment: it carries most of G2, G3, G6, G7, G8 and G10, so it should be extracted last.
