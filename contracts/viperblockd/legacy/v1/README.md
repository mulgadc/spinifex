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

This package does not authorize requests, validate a volume ID, store volume
state, or define the generic storage-provider API. Those concerns remain with
the consuming daemon, the EC2 domain, and `providers/ebs` respectively.
