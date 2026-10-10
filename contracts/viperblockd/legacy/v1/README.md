# Viperblockd legacy contracts, version 1

This package owns the deployed, transitional configuration-update and
state-reload request/reply protocols between Spinifex's legacy control plane
and `viperblockd`. It makes those existing compatibility boundaries explicit
while the generic `providers/ebs` contract is adopted; it is not the EBS
provider abstraction and new provider integrations must not depend on it.

## Configuration update routes

`ConfigUpdateSubject` is `ebs.config`. Every `viperblockd` daemon subscribes
to it in the `spinifex-workers` queue group. It is the fallback path for an
encrypted volume with no live mount: the selected daemon opens the volume
exclusively and reseals its state.

`VolumeConfigUpdateSubject(volume)` is `ebs.config.<volume>`. The daemon with
the live mounted volume subscribes directly so the sole holder of the volume's
state sequence number applies the update.

Both routes carry `EBSConfigUpdateRequest` and respond with
`EBSConfigUpdateResponse`. The JSON field spelling, route shapes, and
request/reply semantics are deployed compatibility behaviour. Incompatible
changes require a new contract version or an explicit migration plan.

## State reload route

`SyncSubject` is `ebs.sync`. After a successful EC2 `ModifyVolume`, the daemon
uses request/reply to ask a `spinifex-workers` queue-group member to reload the
volume's state from its backend. The selected Viperblockd worker responds with
`EBSSyncResponse`; it returns an error when it does not own a live mount of the
volume. The daemon records that error as a warning after the API response has
already been returned. This is deployed legacy behaviour, not a guarantee that
every modification has reached its owning mount.

The route carries `EBSSyncRequest` and `EBSSyncResponse`. Their JSON shape and
the queue-group route are compatibility behaviour.

## Volume lifecycle routes

`MountSubject(node)` and `UnmountSubject(node)` retain the deployed mount and
unmount routes. With a node name they address that node's Viperblockd process
(`ebs.<node>.mount` or `ebs.<node>.unmount`); without one they retain the
single-node `spinifex-workers` queue-group routes. Both carry `EBSRequest`.
`EBSMountResponse` supplies the exported NBD URI, while
`EBSUnMountResponse` reports the seal result. The response broadcasts remain
`ebs.mount.response` and `ebs.unmount.response` alongside ordinary
request/reply replies.

`DeleteSubject` retains the `ebs.delete` queue-group route. It carries
`EBSDeleteRequest` and responds with `EBSDeleteResponse` after Viperblockd has
removed its local state and export. These routes and JSON shapes are deployed
legacy compatibility behaviour, not a generic EBS-provider interface.

## Fence route

`VolumeFencedSubject(node)` is `ebs.<node>.fenced`, or `ebs.fenced` with no
node for a single-node daemon. The Viperblock adapter publishes
`VolumeFencedEvent` on it after tearing down an export whose volume lease
moved to another node; the local daemon subscribes to stop the guest that
was using it. Node-addressed, like mount and unmount: the guest is local, and
no other node's daemon has anything to stop.

## Abandon route

`VolumeAbandonSubject(node)` is `ebs.<node>.abandon`. The daemon uses
request/reply to ask the Viperblock adapter on that node to tear an export
down without sealing, because the instance using it belongs to another node
now. The route carries `VolumeAbandonRequest` and responds with
`VolumeAbandonResponse`. `Abandoned` is false with no error when the volume
was not exported there, which is the ordinary case: the request is
idempotent by design, since the caller is reconciling rather than
transacting.

## Dirty-marker bucket

`DirtyBucket` is the JetStream KV bucket `VIPERBLOCK_VOLUME_DIRTY`, keyed by
volume name under `VolumeKeyPattern`, storing `DirtyRecord` JSON. It records
which node holds writes for a volume that the backend may not have.

Owner: `providers/ebs/viperblock` owns the dirty-volume state and its
transitions. `services/viperblockd` was only its transitional custodian.

Producers: only the Viperblock adapter writes, clears or changes a dirty
record.

Consumers: Viperblock recovery and mount logic on any node. `spx admin` may
obtain a read-only operational view.

Storage contract: the bucket name, `VolumeKeyPattern` and the `DirtyRecord`
JSON shape are this package's legacy v1 contract. There is no TTL; a record
persists until an explicit adapter-owned recovery or unseal transition
removes it.

Compatibility: this is a deployed legacy v1 contract, and coordinated
writers and readers support N/N-1. No renames, type changes or semantic
reinterpretation of existing fields; a breaking change needs a successor
contract and a migration.

CLI authority: the CLI is never a writer and must not perform arbitrary
bucket operations.

This package does not authorize requests, validate a volume ID, store volume
state, or define the generic storage-provider API. Those concerns remain with
the consuming daemon, the EC2 domain, and `providers/ebs` respectively.
