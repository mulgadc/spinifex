------------------------------------------------------ MODULE MC ------------------------------------------------------
EXTENDS Capabilities, Volumes, Snapshots

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

MCSymmetry == Permutations(VolumeIds) \union Permutations(VolumeHandles) \union Permutations(NodeIds)
              \union Permutations(SnapshotIds) \union Permutations(SnapshotHandles)

ValidCapacity(c) == /\ c.requiredBytes > 0
                    /\ c.limitBytes = 0 \/ c.requiredBytes <= c.limitBytes

MCCreateVolumeRequests == {r \in CreateVolumeRequests :
                             /\ ValidCapacity(r.capacityRange)
                             /\ r.seedData <= r.capacityRange.requiredBytes
                             /\ r.sourceSnapshotId /= None => r.sourceSnapshotVolumeId /= None
                             /\ r.seedData > 0 => Capabilities.volumeSeeding}

MCExpandVolumeRequests == {r \in ExpandVolumeRequests : ValidCapacity(r.capacityRange)}

MCPublishVolumeRequests == {r \in PublishVolumeRequests : r.readOnly => Capabilities.readOnlyPublish}

MCCopySnapshotRequests == IF Capabilities.snapshotCopy
                          THEN {r \in CopySnapshotRequests : r.sourceSnapshotId /= r.destinationSnapshotId}
                          ELSE {}

Caller ==
  /\ err' = None
  /\ \/ \E req \in MCCreateVolumeRequests  : Call("CreateVolume",    req, CreateVolume)
     \/ \E req \in MCExpandVolumeRequests  : Call("ExpandVolume",    req, ExpandVolume)
     \/ \E req \in MCPublishVolumeRequests : Call("PublishVolume",   req, PublishVolume)
     \/ \E req \in UnpublishVolumeRequests : Call("UnpublishVolume", req, UnpublishVolume)
     \/ \E req \in DeleteVolumeRequests    : Call("DeleteVolume",    req, DeleteVolume)

     \/ \E req \in CreateSnapshotRequests  : Call("CreateSnapshot",  req, CreateSnapshot)
     \/ \E req \in MCCopySnapshotRequests  : Call("CopySnapshot",    req, CopySnapshot)
     \/ \E req \in DeleteSnapshotRequests  : Call("DeleteSnapshot",  req, DeleteSnapshot)

Provider ==
  /\ NoCall
  /\ \E snapId \in SnapshotIds : SettleSnapshot(snapId)

Next == Caller \/ Provider

Spec == Init /\ [][Next]_vars

=======================================================================================================================
