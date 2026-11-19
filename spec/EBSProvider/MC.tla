------------------------------------------------------ MODULE MC ------------------------------------------------------
EXTENDS Capabilities, Volumes, Snapshots

MCCapabilities == [onlineExpansion         |-> TRUE,
                   sparseExtentReporting   |-> TRUE,
                   crashConsistentSnapshot |-> TRUE,
                   readOnlyPublish         |-> TRUE,
                   volumeEnumeration       |-> TRUE,
                   snapshotEnumeration     |-> TRUE,
                   snapshotCopy            |-> TRUE,
                   ownerRouting            |-> TRUE,
                   exclusion               |-> [scope           |-> "cluster",
                                                claimTtlSeconds |-> 0,
                                                fencesLostClaim |-> TRUE]]

MCSymmetry == Permutations(VolumeIds) \union Permutations(NodeIds) \union Permutations(SnapshotIds)

MCCreateVolumeRequests == {r \in CreateVolumeRequests : r.sizeBytes > 0}

MCExpandVolumeRequests == {r \in ExpandVolumeRequests : r.sizeBytes > 0}

MCPublishVolumeRequests == {r \in PublishVolumeRequests : r.readOnly => Capabilities.readOnlyPublish}

MCCopySnapshotRequests == IF Capabilities.snapshotCopy
                          THEN {r \in CopySnapshotRequests : r.sourceSnapshotId /= r.destinationSnapshotId}
                          ELSE {}

Next ==
  /\ err' = None
  /\ \/ \E req \in MCCreateVolumeRequests  : Call("CreateVolume",    req, CreateVolume)
     \/ \E req \in MCExpandVolumeRequests  : Call("ExpandVolume",    req, ExpandVolume)
     \/ \E req \in MCPublishVolumeRequests : Call("PublishVolume",   req, PublishVolume)
     \/ \E req \in UnpublishVolumeRequests : Call("UnpublishVolume", req, UnpublishVolume)
     \/ \E req \in DeleteVolumeRequests    : Call("DeleteVolume",    req, DeleteVolume)

     \/ \E req \in CreateSnapshotRequests  : Call("CreateSnapshot",  req, CreateSnapshot)
     \/ \E req \in MCCopySnapshotRequests  : Call("CopySnapshot",    req, CopySnapshot)
     \/ \E req \in DeleteSnapshotRequests  : Call("DeleteSnapshot",  req, DeleteSnapshot)

Spec == Init /\ [][Next]_vars

=======================================================================================================================
