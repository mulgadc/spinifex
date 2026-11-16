-------------------------------------------------- MODULE Snapshots ---------------------------------------------------
(*********************************************************************************************************************)
(* The lifecycle of a snapshot, from its creation to its deletion.                                                   *)
(*********************************************************************************************************************)
EXTENDS EBSProvider

-----------------------------------------------------------------------------------------------------------------------
\* Interface methods

CreateSnapshotRequests == [snapshotId   : SnapshotIds,
                           volumeId     : VolumeIds,
                           volumeHandle : Optional(VolumeHandles)]

CreateSnapshot(req) ==
  (*******************************************************************************************************************)
  (* A volume's data is frozen as a snapshot, from which new volumes can be restored. The snapshot holds the         *)
  (* volume's data as it was when the call returned, even while it is pending, and a pending snapshot settles later  *)
  (* without a call (SettleSnapshot). A retry of a call that already succeeded returns the same snapshot.            *)
  (*******************************************************************************************************************)
  LET snapId == req.snapshotId
      volId  == req.volumeId

      Errors ==
        LET checks == [alreadyExists   |-> /\ KeyExists(snapshots, snapId)
                                           /\ snapshots[snapId].sourceVolumeId /= volId,
                       \* Only checked for a new snapshot, so a retry outlives the volume.
                       notFound        |-> /\ ~KeyExists(snapshots, snapId)
                                           /\ \/ ~KeyExists(volumes, volId)
                                              \/ req.volumeHandle \notin {None, volumes[volId].handle}]
        IN {e \in DOMAIN checks : checks[e]}

     \* The request was rejected, or the provider failed to answer.
  IN \/ /\ val' = None
        /\ err' \in Errors \union ProviderErrors
        /\ UNCHANGED state

     \* The request was accepted.
     \/ /\ Errors = {}
           \* A retry returns the existing snapshot.
        /\ \/ /\ KeyExists(snapshots, snapId)
              /\ UNCHANGED snapshots

           \/ /\ ~KeyExists(snapshots, snapId)
              \* The handle, creation time and state are the implementation's choice.
              /\ \E h \in SnapshotHandles \ {snapshots[x].handle : x \in DOMAIN snapshots},
                    t \in Timestamps,
                    s \in {"pending", "completed"} :
                   snapshots' = InsertKey(snapshots, snapId, [sizeBytes      |-> volumes[volId].capacityBytes,
                                                              createdAt      |-> t,
                                                              state          |-> s,
                                                              handle         |-> h,
                                                              sourceVolumeId |-> volId])
        /\ val' = SnapshotView(snapId)'
        /\ err' = None
        /\ UNCHANGED <<volumes, publications>>


CopySnapshotRequests == [sourceSnapshotId      : SnapshotIds,
                         destinationSnapshotId : SnapshotIds,
                         volumeId              : VolumeIds]

CopySnapshot(req) ==
  (*******************************************************************************************************************)
  (* A snapshot is copied under a new ID, sharing its frozen data, and the copy can be deleted independently         *)
  (* of it.                                                                                                          *)
  (*******************************************************************************************************************)
  LET srcSnapId == req.sourceSnapshotId
      dstSnapId == req.destinationSnapshotId
      volId     == req.volumeId

      Errors ==
        LET checks == [invalidArgument       |-> \/ srcSnapId = dstSnapId
                                                 \* Only compared, not looked up, since a snapshot outlives its volume.
                                                 \/ /\ KeyExists(snapshots, srcSnapId)
                                                    /\ snapshots[srcSnapId].sourceVolumeId /= volId,
                       alreadyExists         |-> KeyExists(snapshots, dstSnapId),
                       notFound              |-> ~KeyExists(snapshots, srcSnapId),
                       unsupportedCapability |-> ~Capabilities.snapshotCopy]
        IN {e \in DOMAIN checks : checks[e]}

     \* The request was rejected, or the provider failed to answer.
  IN \/ /\ val' = None
        /\ err' \in Errors \union ProviderErrors
        /\ UNCHANGED state

     \* The request was accepted.
     \/ /\ Errors = {}
        \* The handle and creation time are the implementation's choice.
        /\ \E h \in SnapshotHandles \ {snapshots[x].handle : x \in DOMAIN snapshots},
              t \in Timestamps :
             snapshots' = InsertKey(snapshots, dstSnapId, [createdAt |-> t,
                                                           handle    |-> h]
                                                           @@ snapshots[srcSnapId])
        /\ val' = SnapshotView(dstSnapId)'
        /\ err' = None
        /\ UNCHANGED <<volumes, publications>>


ListSnapshotsRequests == [maxResults    : -1..(MaxListResults + 1),
                          startingToken : Optional(ListTokens),
                          snapshotId    : Optional(SnapshotIds)]

\* The ref that ListSnapshots returns for a snapshot, carrying its current state.
SnapshotRef(id) == [id             |-> id,
                    sourceVolumeId |-> snapshots[id].sourceVolumeId,
                    handle         |-> snapshots[id].handle,
                    state          |-> snapshots[id].state]

ListSnapshots(req) ==
  (*******************************************************************************************************************)
  (* Some of the snapshots are listed, a page at a time, and nothing changes. A call with no starting token that     *)
  (* returns no next token lists every snapshot. A call that names a snapshot returns that snapshot in its current   *)
  (* state, or nothing when it does not exist, so an accepted snapshot is never reported absent while it is pending. *)
  (*******************************************************************************************************************)
  LET snapId == req.snapshotId

      Errors ==
        \* A call that names a snapshot reads only that one, so it needs no enumeration.
        LET checks == [unsupportedCapability |-> snapId = None /\ ~Capabilities.snapshotEnumeration]
        IN {e \in DOMAIN checks : checks[e]}

     \* The request was rejected, or the provider failed to answer.
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

     \* The request was accepted.
     \/ /\ Errors = {}
        /\ snapId = None
        /\ LET refs == {SnapshotRef(id) : id \in DOMAIN snapshots}
           \* The page and the next token are the implementation's choice.
           IN \E page \in SUBSET refs, next \in Optional(ListTokens) :
                /\ Cardinality(page) <= PageSize(req.maxResults)
                /\ next /= None => Cardinality(page) = PageSize(req.maxResults)
                /\ req.startingToken = None /\ next = None => page = refs
                /\ val' = [snapshots |-> page, nextToken |-> next]
                /\ err' = None
        /\ UNCHANGED state


DeleteSnapshotRequests == [snapshotId : SnapshotIds,
                           handle     : Optional(SnapshotHandles)]

DeleteSnapshot(req) ==
  (*******************************************************************************************************************)
  (* A snapshot is deleted, so that no new volume can be restored from it. Volumes already restored from it,         *)
  (* and copies of it, are unaffected.                                                                               *)
  (*******************************************************************************************************************)
  LET snapId == req.snapshotId

     \* The provider failed to answer.
  IN \/ /\ val' = None
        /\ err' \in ProviderErrors
        /\ UNCHANGED state

     \* The request was accepted.
     \/ /\ snapshots' = IF /\ KeyExists(snapshots, snapId)
                           /\ req.handle \notin {None, snapshots[snapId].handle}
                        THEN snapshots
                        \* Record removal is idempotent so that retries succeed.
                        ELSE RemoveKey(snapshots, snapId)
        /\ val' = None
        /\ err' = None
        /\ UNCHANGED <<volumes, publications>>

-----------------------------------------------------------------------------------------------------------------------
\* Steps without a call

SettleSnapshot(snapId) ==
  (*******************************************************************************************************************)
  (* A pending snapshot settles as completed or error, as the provider finishes it. Its data is unchanged, since it  *)
  (* was fixed when CreateSnapshot returned, and a snapshot that has settled never changes state again.              *)
  (*******************************************************************************************************************)
  /\ KeyExists(snapshots, snapId)
  /\ snapshots[snapId].state = "pending"
  /\ \E s \in {"completed", "error"} :
       snapshots' = [snapshots EXCEPT ![snapId].state = s]
  /\ UNCHANGED <<volumes, publications>>

=======================================================================================================================
