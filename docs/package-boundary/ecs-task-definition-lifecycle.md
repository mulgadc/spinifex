# ECS task definition: lifecycle contract

Present contract under ADR-0003 S1, recorded before the resource moves.
It describes the code as it stands, so it claims no conformance; the gaps below are observed behaviour, not targets.
Paths are relative to `spinifex/` unless they start with `tests/`.

## Scope

A task definition revision has no realization.
Register and deregister complete synchronously within the request; tasks and services that use a revision are realized by their own owners.
A revision is immutable after register except for its status and tags.

## Contract

| Item | Present behaviour | Evidence |
|---|---|---|
| Identity | `family:revision`, with the revision allocated by register. ARN `arn:aws:ecs:{region}:{account}:task-definition/{family}:{revision}`. References accept a bare family (latest revision), `family:revision` or the ARN. | `handlers/ecs/taskdef_lifecycle_test.go` `TestDescribeTaskDefinition_ResolvesEveryReferenceForm`, `handlers/ecs/service_test.go` `TestService_RegisterTaskDefinition_RevisionBump` |
| Scope | Account and Region, not cluster: keys live in the account's ECS bucket. | `TestTaskDefinition_PersistedKeysAndFieldNames` |
| Durable owner and record | `handlers/ecs/service.go` writes `taskdef-families/{family}/revs/{revision}` (`TaskDefRecord`) and `taskdef-families/{family}/latest-rev` (an integer). `handlers/ecs/tags.go` also writes the revision record directly. | `TestTaskDefinition_PersistedKeysAndFieldNames`, `handlers/ecs/records_test.go` `TestTaskDefRecord_DecodesPreExistingJSON`, `handlers/ecs/tags_test.go` `TestService_Tags_TaskDefinitionRoundTrip` |
| Desired state | Containers, network mode, task-level CPU and memory, task and execution role ARNs, compatibilities, runtime platform and tags. Register rejects container features the agent cannot apply. | `handlers/ecs/taskdef_validation_test.go` |
| Generation | None. The revision number is identity, not a generation. | |
| Observed state | Not tracked separately; status is `ACTIVE` or `INACTIVE`. | `TestService_ListTaskDefinitions_StatusFilter` |
| Readiness | Usable as soon as register returns. | `TestService_RunTask_PlacesAndAssigns` |
| Idempotency | No client token, as in AWS: every register allocates a new revision. Deregister is repeatable. Internal NATS redelivery of a register allocates another revision. | `TestService_RegisterTaskDefinition_RevisionBump`, `TestDeregisterTaskDefinition_RepeatSucceedsAndUnknownIsRefused` |
| Dependants | RunTask and StartTask resolve the reference themselves and copy the containers, reserved CPU, memory and GPU, network mode and both role ARNs onto the task record and the agent's assignment. A service pins the revision ARN it resolved at create or update; scheduler launches and circuit-breaker rollback use the pinned ARN. | `TestRunTask_BareFamilyRunsTheLatestRevision`, `TestCreateService_PinsTheResolvedRevision`, `TestService_RunTask_AssignCarriesTaskRole`, `taskdef_validation_test.go` `TestRunTask_AssignCarriesExecutionRoleAndLogDriver` |
| Authorization | `iam:PassRole` is enforced only in the gateway adapter (`gateway/ecs/actions.go`): on the request's role ARNs at register, and on the resolved revision's roles at RunTask, StartTask, CreateService and UpdateService, looked up through the public `DescribeTaskDefinition` over NATS. Scheduler and rollback launches inside the daemon are not re-checked. | `gateway/ecs/actions_test.go` `TestCheckTaskDefinitionRoles_*`, `TestRegisterTaskDefinition_*PassRole`, `gateway/ecs/authz_test.go` |
| Deletion ordering | No hard delete: deregister sets `INACTIVE` and the revision stays describable. A deregistered revision still launches tasks. No account-teardown reaper covers task definitions (`accountteardown/reapers_ecs.go` reaps clusters only). | `TestService_DeregisterTaskDefinition_InactivatesRevision`, `TestRunTask_AcceptsAnInactiveRevision` |
| Restart and interruption | Register is two writes: the revision, then `latest-rev`. An interruption between them leaves a stored revision that a bare-family reference does not reach and the next register overwrites. | none |
| End-to-end | `tests/e2e/ecs/ecs_test.go`, `tests/e2e/gpu/ecs_gpu_test.go`, `tests/integration/cross_tenant_test.go`. No Terraform apply/destroy evidence for `aws_ecs_task_definition`. | |

## Gaps

1. Revision allocation reads `latest-rev` and adds one with no compare-and-set, so concurrent registers of one family can allocate the same revision and the later write replaces the earlier one (`service.go` `nextRevision`).
2. An interrupted register can leave an orphaned revision that the next register overwrites (see restart row).
3. Launch resolution does not check status, so RunTask, StartTask and CreateService accept an `INACTIVE` revision. AWS documents that an inactive revision cannot start new tasks or create new services; not verified against live AWS.
4. Security: for a bare-family reference, the gateway's `iam:PassRole` check and the daemon's launch resolve "latest" separately. A revision registered between the two runs with roles the caller was never checked against. Exploiting it needs another principal in the same account able to register that revision. Recorded, not fixed: it touches the IAM boundary.
5. The gateway resolves roles through the public `DescribeTaskDefinition` action over NATS, an AWS adapter used as an internal API (ADR-0003 S4).
6. `handlers/ecs/tags.go` reads and writes the revision record directly, bypassing the owner.
7. No task-definition cleanup at account teardown, and no ADR-0003 S5 failure or recovery evidence.
