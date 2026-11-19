-------------------------------------------------- MODULE Snapshots ---------------------------------------------------
EXTENDS EBSProvider

-----------------------------------------------------------------------------------------------------------------------
\* Interface methods

CreateSnapshotRequests == [snapshotId : SnapshotIds,
                           volumeId   : VolumeIds]

CreateSnapshot(req) ==
  LET snapId == req.snapshotId
      volId  == req.volumeId

      \* The set of errors that this call may return in the current system state. If there are multiple possible
      \* errors for a single call, the exact one that is returned is left to the implementation.
      Errors ==
        LET checks == [alreadyExists   |-> /\ KeyExists(snapshots, snapId)
                                           /\ snapshots[snapId].sourceVolumeId /= volId,
                       \* Only checked for a new snapshot, so a retry outlives the volume.
                       notFound        |-> /\ ~KeyExists(snapshots, snapId)
                                           /\ ~KeyExists(volumes, volId)]
        IN {e \in DOMAIN checks : checks[e]}

     \* The operation was refused, or the provider failed to answer.
  IN \/ /\ val' = None
        /\ err' \in Errors \union ProviderErrors
        /\ UNCHANGED state

     \* The operation was successful.
     \/ /\ Errors = {}
           \* A retry returns the existing snapshot.
        /\ \/ /\ KeyExists(snapshots, snapId)
              /\ UNCHANGED snapshots

           \/ /\ ~KeyExists(snapshots, snapId)
              \* The creation time and metadata are determined by the implementation.
              /\ \E md \in Optional(Metadata),
                    t \in Timestamps :
                   /\ snapshots' = InsertKey(snapshots, snapId, [sizeBytes      |-> volumes[volId].sizeBytes,
                                                                 createdAt      |-> t,
                                                                 sourceVolumeId |-> volId,
                                                                 metadata       |-> md])
        /\ val' = SnapshotView(snapId)'
        /\ err' = None
        /\ UNCHANGED <<volumes, publications>>


CopySnapshotRequests == [sourceSnapshotId      : SnapshotIds,
                         destinationSnapshotId : SnapshotIds,
                         volumeId              : VolumeIds]

CopySnapshot(req) ==
  LET srcSnapId == req.sourceSnapshotId
      dstSnapId == req.destinationSnapshotId
      volId     == req.volumeId

      \* The set of errors that this call may return in the current system state. If there are multiple possible
      \* errors for a single call, the exact one that is returned is left to the implementation.
      Errors ==
        LET checks == [invalidArgument       |-> \/ srcSnapId = dstSnapId
                                                 \* Only compared, not looked up, since a snapshot outlives its volume.
                                                 \/ /\ KeyExists(snapshots, srcSnapId)
                                                    /\ snapshots[srcSnapId].sourceVolumeId /= volId,
                       alreadyExists         |-> KeyExists(snapshots, dstSnapId),
                       notFound              |-> ~KeyExists(snapshots, srcSnapId),
                       unsupportedCapability |-> ~Capabilities.snapshotCopy]
        IN {e \in DOMAIN checks : checks[e]}

     \* The operation was refused, or the provider failed to answer.
  IN \/ /\ val' = None
        /\ err' \in Errors \union ProviderErrors
        /\ UNCHANGED state

     \* The operation was successful.
     \/ /\ Errors = {}
        \* The creation time and metadata are determined by the implementation.
        /\ \E md \in Optional(Metadata),
              t \in Timestamps :
             /\ snapshots' = InsertKey(snapshots, dstSnapId, [createdAt |-> t,
                                                              metadata  |-> md]
                                                              @@ snapshots[srcSnapId])
        /\ val' = SnapshotView(dstSnapId)'
        /\ err' = None
        /\ UNCHANGED <<volumes, publications>>


ListSnapshotsRequests == [maxResults    : -1..(MaxListResults + 1),
                          startingToken : Optional(SUBSET SnapshotIds),
                          snapshotId    : Optional(SnapshotIds)]

\* The ref that ListSnapshots returns for a snapshot.
SnapshotRef(id) == [id             |-> id,
                    sourceVolumeId |-> snapshots[id].sourceVolumeId,
                    metadata       |-> snapshots[id].metadata]

ListSnapshots(req) ==
  LET snapId == req.snapshotId

      \* The set of errors that this call may return in the current system state. If there are multiple possible
      \* errors for a single call, the exact one that is returned is left to the implementation.
      Errors ==
        \* A call that names a snapshot reads only that one, so it needs no enumeration.
        LET checks == [unsupportedCapability |-> snapId = None /\ ~Capabilities.snapshotEnumeration]
        IN {e \in DOMAIN checks : checks[e]}

     \* The operation was refused, or the provider failed to answer.
  IN \/ /\ val' = None
        /\ err' \in Errors \union ProviderErrors
        /\ UNCHANGED state

     \* The request named one snapshot, so paging is ignored.
     \/ /\ Errors = {}
        /\ snapId /= None
        /\ LET refs == IF KeyExists(snapshots, snapId) THEN {SnapshotRef(snapId)} ELSE {}
           IN val' = [snapshots |-> refs, nextToken |-> None]
        /\ err' = None
        /\ UNCHANGED state

     \* The operation was successful.
     \/ /\ Errors = {}
        /\ snapId = None
        /\ LET covered   == IF req.startingToken = None THEN {} ELSE req.startingToken
               remaining == DOMAIN snapshots \ covered
               size      == PageSize(req.maxResults)
           \* The page and the next token are determined by the implementation.
           IN \E ids  \in SUBSET remaining,
                 gone \in SUBSET (SnapshotIds \ DOMAIN snapshots) :
                /\ Cardinality(ids) <= size
                /\ Cardinality(ids) < size => ids = remaining
                /\ val' = [snapshots |-> {SnapshotRef(id) : id \in ids},
                           nextToken |-> IF remaining \ ids = {}
                                         THEN None
                                         ELSE covered \union ids \union gone]
                /\ err' = None
        /\ UNCHANGED state


DeleteSnapshotRequests == [snapshotId : SnapshotIds]

DeleteSnapshot(req) ==
  LET snapId == req.snapshotId

     \* The provider failed to answer.
  IN \/ /\ val' = None
        /\ err' \in ProviderErrors
        /\ UNCHANGED state

     \* The operation was successful, and record removal is idempotent so that retries succeed.
     \/ /\ snapshots' = RemoveKey(snapshots, snapId)
        /\ val' = None
        /\ err' = None
        /\ UNCHANGED <<volumes, publications>>

=======================================================================================================================
