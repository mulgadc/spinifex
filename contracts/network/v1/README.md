# Network realisation contract, version 1

This package owns the deployed wire vocabulary for the `vpc.*`
control-plane-to-network-realisation boundary: subjects, payload JSON, the
`{success,error}` acknowledgement envelope, delivery mode and the
`vpcd-workers` queue group. It does not move `vpcd`, perform any NATS I/O, or
change any lifecycle or reliability semantics. It is a characterization of
what is deployed today, split one route family per file, not a redesign.

## Ownership

Network owns the meaning and wire contract of every route below, because it
owns OVN and host realisation. EC2 and the other managed-service domains own
their own AWS-visible state transitions and decide when, and whether, to
issue a projection onto one of these subjects; this package does not grant
them authority over a route's semantics, only a stable shape to publish
against.

## Route table

| Subject | Payload | Reply | Delivery | Queue group |
|---|---|---|---|---|
| `vpc.create` | `VPCEvent` | none | fire-and-forget | `vpcd-workers` |
| `vpc.delete` | `VPCEvent` | none | fire-and-forget | `vpcd-workers` |
| `vpc.create-subnet` | `SubnetEvent` | none | fire-and-forget | `vpcd-workers` |
| `vpc.delete-subnet` | `SubnetEvent` | none | fire-and-forget | `vpcd-workers` |
| `vpc.create-port` | `PortEvent` | `AckEnvelope` | request/reply, 5s | `vpcd-workers` |
| `vpc.delete-port` | `PortEvent` | `AckEnvelope` | request/reply, 5s; caller treats failure as non-fatal | `vpcd-workers` |
| `vpc.update-port-sgs` | `PortSecurityGroupsUpdateEvent` | `AckEnvelope` | request/reply, 5s | `vpcd-workers` |
| `vpc.igw-attach` | `InternetGatewayEvent` | none | fire-and-forget, non-fatal | `vpcd-workers` |
| `vpc.igw-detach` | `InternetGatewayEvent` | none | fire-and-forget, non-fatal | `vpcd-workers` |
| `vpc.add-nat` | `NATEvent` | `AckEnvelope` | request/reply, 45s (OVN flows barrier); some callers fire-and-forget | `vpcd-workers` |
| `vpc.delete-nat` | `NATEvent` | `AckEnvelope` | request/reply, 15s, retried on `ErrNoResponders` (3 attempts, 500ms apart) | `vpcd-workers` |
| `vpc.add-nat-gateway` | `NATGatewayEvent` | none | fire-and-forget | `vpcd-workers` |
| `vpc.delete-nat-gateway` | `NATGatewayEvent` | none | fire-and-forget | `vpcd-workers` |
| `vpc.add-igw-route` | `IGWRouteEvent` | none | fire-and-forget | `vpcd-workers` |
| `vpc.delete-igw-route` | `IGWRouteEvent` | none | fire-and-forget | `vpcd-workers` |
| `vpc.gate-subnet-egress` | `SubnetEgressGateEvent` | none | fire-and-forget | `vpcd-workers` |
| `vpc.ungate-subnet-egress` | `SubnetEgressUngateEvent` | none | fire-and-forget | `vpcd-workers` |
| `vpc.add-system-egress` | `SystemEgressEvent` | none | fire-and-forget | `vpcd-workers` |
| `vpc.delete-system-egress` | `SystemEgressEvent` | none | fire-and-forget | `vpcd-workers` |
| `vpc.create-sg` | `SecurityGroupEvent` | `AckEnvelope` | request/reply, 5s | `vpcd-workers` |
| `vpc.delete-sg` | `SecurityGroupEvent` | `AckEnvelope` | request/reply, 5s, idempotent | `vpcd-workers` |
| `vpc.update-sg` | `SecurityGroupEvent` | `AckEnvelope` | request/reply, 5s | `vpcd-workers` |

Producer for every route above is the daemon (`spinifex-daemon`), through the
typed client in `domains/network/projection`; `vpc.add-system-egress` has no
current producer.
Consumer for every route is `vpcd`, by way of `domains/network/subscribers`.
This package is the sole declared owner of every subject and type above,
including `vpc.igw-attach`/`vpc.igw-detach`, which were previously also
declared in `contracts/ec2/v1`; that duplicate has been removed.

## Compatibility rules

This is the deployed v1 contract. Changes follow N/N-1: a subject, JSON field
name, JSON field type, or the ack envelope shape must not be renamed,
retyped, or reinterpreted to mean something else. A breaking change needs a
new, explicitly versioned successor package, not an edit here. Every subject
and golden JSON literal in this package is pinned by an external `_test`
package; a failing pin means the change broke compatibility, not that the
test is stale.

## What this package does not do

It does not move `vpcd` or any subscriber out of its current package. It does
not change any route's delivery mode, timeout, retry budget, queue group, or
the NAT add barrier.
