# ECS agent/scheduler bus contract, version 1

This package owns the deployed wire vocabulary for the `ecs.bus.*`
scheduler-to-agent boundary: subjects, payload JSON and the Layer-2 message
types. It is a characterization of what is deployed today, not a redesign; it
does not move the scheduler, the agent, or perform any NATS I/O itself.

## Route table

| Subject | Payload | Reply | Delivery | Queue group | Producer | Consumer |
|---|---|---|---|---|---|---|
| `ecs.bus.<account>.<cluster>.instance-register.<instanceId>` | `RegisterInstance` | none | fire-and-forget | none | none today | `spinifex_daemon` leader, `handlers/ecs/scheduler.go` `onRegister` |
| `ecs.bus.<account>.<cluster>.instance-heartbeat.<instanceId>` | `Heartbeat` | none | fire-and-forget | none | none today | `spinifex_daemon` leader, `handlers/ecs/scheduler.go` `onHeartbeat` |
| `ecs.bus.<account>.<cluster>.assign.<instanceId>` | `Assign` | none | fire-and-forget | none | none today | none today |
| `ecs.bus.<account>.<cluster>.task-state.<taskId>` | `TaskState` | none | fire-and-forget | none | none today | `spinifex_daemon` leader, `handlers/ecs/scheduler.go` `onTaskState` |
| `ecs.bus.<account>.<cluster>.service-reconcile` | none | none | fire-and-forget | none | `spinifex_daemon`, `handlers/ecs/instances.go` `SubmitTaskStateChange` | `spinifex_daemon` leader, `handlers/ecs/scheduler.go` `onReconcileWake` |

The agent today registers, heartbeats and reports task state through the AWS
gateway's `RegisterContainerInstance`, `PollAssignments` and
`SubmitTaskStateChange` NATS actions instead of publishing on this bus; those
actions are a separate, pre-existing subject family and carry some of the
same message types (`Assign`, `TaskState`, `StopDirective`) as their wire
body. Four of the five subject builders above, every route except
`service-reconcile`, therefore have no current publisher. The scheduler still
subscribes to all four wildcard patterns; only its election leader holds the
subscriptions, which is why no queue group is needed. `service-reconcile` is
the one route with a live producer and consumer: the worker that recorded a
task-state change is rarely the leader, so it publishes a cross-node wake the
leader's own subscription picks up.

`StopDirective`, `ContainerStatus`, `InstanceCapacity`, `PortMapping`,
`AssignContainer` and `SystemControl` are nested payload types carried inside
`Assign` and `TaskState`; they travel on the same routes as their parent type
and are not separately addressed.

## Ownership

The ECS domain owns the meaning of every subject and type in this package.
The ecs-agent binary baked into the ECS node AMI is a deployed consumer: it
decodes `Assign` and `StopDirective` and encodes `TaskState`, all carried
today by the AWS-gateway actions that reuse these types.
It does not encode `Heartbeat` or `RegisterInstance`.

## Compatibility rules

This is the deployed v1 contract. Changes follow N/N-1: a subject, JSON field
name, JSON field type, or message semantics must not be renamed, retyped, or
reinterpreted to mean something else. The AMI-baked agent cannot be upgraded
in lockstep with the daemon, so it must keep decoding the shapes it already
knows; `messages_test.go` pins the older agent's `AssignContainer` shape
against the current one for exactly this reason. A breaking change needs a
new, explicitly versioned successor package, not an edit here.

## What this package does not do

It does not move the scheduler, the agent, or any subscriber out of its
current package, and it changes no ECS lifecycle behaviour: every subject
string, JSON field and message type here is identical to the one the
`handlers/ecs/bus` package declared before this move.
