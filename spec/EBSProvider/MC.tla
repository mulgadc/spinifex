------------------------------------------------------ MODULE MC ------------------------------------------------------
(*********************************************************************************************************************)
(* The behaviours that a model explores: any sequence of calls, each answered as its method allows, interleaved with *)
(* the steps that the provider takes on its own. Requests that their method refuses in every state are left out,     *)
(* because they are invalid on their own or ask for a capability that the model does not advertise, so a refusal is  *)
(* explored only where the state warrants it or the provider fails.                                                  *)
(*********************************************************************************************************************)
EXTENDS Capabilities, Volumes, Snapshots

-----------------------------------------------------------------------------------------------------------------------
\* Constants

\* Every optional behaviour advertised, so that every request that a capability gates is explored.
MCCapabilities == [onlineExpansion         |-> TRUE,
                   sparseExtentReporting   |-> TRUE,
                   crashConsistentSnapshot |-> TRUE,
                   volumeSeeding           |-> TRUE,
                   readOnlyPublish         |-> TRUE,
                   volumeEnumeration       |-> TRUE,
                   snapshotEnumeration     |-> TRUE,
                   snapshotCopy            |-> TRUE,
                   ownerRouting            |-> TRUE,
                   exclusion               |-> [scope           |-> "cluster",
                                                claimTtlSeconds |-> 0,
                                                fencesLostClaim |-> TRUE]]

\* IDs and handles are opaque, so states that differ only by renaming them are equivalent.
MCSymmetry == Permutations(VolumeIds) \union Permutations(VolumeHandles) \union Permutations(NodeIds)
              \union Permutations(SnapshotIds) \union Permutations(SnapshotHandles)

-----------------------------------------------------------------------------------------------------------------------
\* Requests

\* A capacity that is positive and within its limit.
ValidCapacity(c) == /\ c.requiredBytes > 0
                    /\ c.limitBytes = 0 \/ c.requiredBytes <= c.limitBytes

MCCreateVolumeRequests == {r \in CreateVolumeRequests :
                             /\ ValidCapacity(r.capacityRange)
                             /\ r.seedData <= r.capacityRange.requiredBytes
                             /\ r.sourceSnapshotId /= None => r.sourceSnapshotVolumeId /= None
                             /\ r.seedData > 0 => Capabilities.volumeSeeding}

MCListVolumesRequests == IF Capabilities.volumeEnumeration THEN ListVolumesRequests ELSE {}

MCExpandVolumeRequests == {r \in ExpandVolumeRequests : ValidCapacity(r.capacityRange)}

MCPublishVolumeRequests == {r \in PublishVolumeRequests : r.readOnly => Capabilities.readOnlyPublish}

MCCopySnapshotRequests == IF Capabilities.snapshotCopy
                          THEN {r \in CopySnapshotRequests : r.sourceSnapshotId /= r.destinationSnapshotId}
                          ELSE {}

MCListSnapshotsRequests == {r \in ListSnapshotsRequests : r.snapshotId = None => Capabilities.snapshotEnumeration}

-----------------------------------------------------------------------------------------------------------------------
\* Specification

\* A call to any method with any request that the model explores.
Caller ==
  \/ \E req \in GetCapabilitiesRequests : Call("GetCapabilities", req, GetCapabilities)

  \/ \E req \in MCCreateVolumeRequests  : Call("CreateVolume",    req, CreateVolume)
  \/ \E req \in GetVolumeRequests       : Call("GetVolume",       req, GetVolume)
  \/ \E req \in MCListVolumesRequests   : Call("ListVolumes",     req, ListVolumes)
  \/ \E req \in MCExpandVolumeRequests  : Call("ExpandVolume",    req, ExpandVolume)
  \/ \E req \in MCPublishVolumeRequests : Call("PublishVolume",   req, PublishVolume)
  \/ \E req \in UnpublishVolumeRequests : Call("UnpublishVolume", req, UnpublishVolume)
  \/ \E req \in DeleteVolumeRequests    : Call("DeleteVolume",    req, DeleteVolume)

  \/ \E req \in CreateSnapshotRequests  : Call("CreateSnapshot",  req, CreateSnapshot)
  \/ \E req \in MCCopySnapshotRequests  : Call("CopySnapshot",    req, CopySnapshot)
  \/ \E req \in MCListSnapshotsRequests : Call("ListSnapshots",   req, ListSnapshots)
  \/ \E req \in DeleteSnapshotRequests  : Call("DeleteSnapshot",  req, DeleteSnapshot)

\* A step that the provider takes on its own, answering no call.
Provider ==
  /\ NoCall
  /\ \E snapId \in SnapshotIds : SettleSnapshot(snapId)

Next == Caller \/ Provider

Spec == Init /\ [][Next]_vars

=======================================================================================================================
