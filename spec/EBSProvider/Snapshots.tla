-------------------------------------------------- MODULE Snapshots ---------------------------------------------------
(*********************************************************************************************************************)
(* The lifecycle of a snapshot, from its creation to its deletion.                                                   *)
(*********************************************************************************************************************)
EXTENDS EBSProvider

-----------------------------------------------------------------------------------------------------------------------
\* Types

CopySnapshotRequests == [source_snapshot_id      : Optional(SnapshotIDs),
                         destination_snapshot_id : Optional(SnapshotIDs),
                         volume_id               : Optional(VolumeIDs)]

DeleteSnapshotRequests == [snapshot_id : Optional(SnapshotIDs),
                           handle      : Optional(SnapshotHandles)]

-----------------------------------------------------------------------------------------------------------------------
\* Interface methods

CopySnapshot(req, val, err) ==
  (*******************************************************************************************************************)
  (* A snapshot is copied under a new ID, sharing its frozen data, and the copy can be deleted independently         *)
  (* of it.                                                                                                          *)
  (*******************************************************************************************************************)
  LET srcSnapID == req.source_snapshot_id
      dstSnapID == req.destination_snapshot_id
      volID     == req.volume_id

      Errors ==
        LET checks == [invalid_argument |-> \/ srcSnapID = None
                                            \/ dstSnapID = None
                                            \/ volID = None
                                            \/ srcSnapID = dstSnapID
                                            \* Only compared, not looked up, since a snapshot outlives its volume.
                                            \/ /\ KeyExists(snapshots, srcSnapID)
                                               /\ snapshots[srcSnapID].source_volume_id /= volID,
                       already_exists   |-> KeyExists(snapshots, dstSnapID),
                       not_found        |-> /\ srcSnapID /= None
                                            /\ ~KeyExists(snapshots, srcSnapID)]
        IN {e \in DOMAIN checks : checks[e]}

     \* The request was rejected, or the provider failed to answer.
  IN \/ /\ val = None
        /\ err \in Errors \union ProviderErrors
        /\ UNCHANGED vars

     \* The request was accepted.
     \/ /\ Errors = {}
        \* The handle and creation time are the implementation's choice.
        /\ \E h \in SnapshotHandles \ {snapshots[x].handle : x \in DOMAIN snapshots},
              time \in Timestamps :
             /\ snapshots' = InsertKey(snapshots, dstSnapID,
                                       [created_at |-> time, handle |-> h] @@ snapshots[srcSnapID])
             /\ val = SnapshotView(dstSnapID)'
             /\ err = None
             /\ UNCHANGED <<volumes, publications>>

DeleteSnapshot(req, err) ==
  (*******************************************************************************************************************)
  (* A snapshot is deleted, so that no new volume can be restored from it. Volumes already restored from it,         *)
  (* and copies of it, are unaffected.                                                                               *)
  (*******************************************************************************************************************)
  LET snapID == req.snapshot_id

      Errors ==
        LET checks == [invalid_argument |-> snapID = None]
        IN {e \in DOMAIN checks : checks[e]}

     \* The request was rejected, or the provider failed to answer.
  IN \/ /\ err \in Errors \union ProviderErrors
        /\ UNCHANGED vars

     \* The request was accepted.
     \/ /\ Errors = {}
        /\ snapshots' = IF /\ KeyExists(snapshots, snapID)
                           /\ req.handle /= None
                           /\ snapshots[snapID].handle /= req.handle
                        THEN snapshots
                        \* Record removal is idempotent so that retries succeed.
                        ELSE RemoveKey(snapshots, snapID)
        /\ err = None
        /\ UNCHANGED <<volumes, publications>>

=======================================================================================================================
