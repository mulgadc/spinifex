-------------------------------------------------- MODULE Snapshots ---------------------------------------------------
(*********************************************************************************************************************)
(* The lifecycle of a snapshot, from its creation to its deletion.                                                   *)
(*********************************************************************************************************************)
EXTENDS EBSProvider

-----------------------------------------------------------------------------------------------------------------------
\* Interface methods

CreateSnapshotRequests == [snapshot_id   : Optional(SnapshotIDs),
                           volume_id     : Optional(VolumeIDs),
                           volume_handle : Optional(VolumeHandles)]

CreateSnapshot(req, val, err) ==
  (*******************************************************************************************************************)
  (* A volume's data is frozen as a snapshot, from which new volumes can be restored. A retry of a call that         *)
  (* already succeeded returns the same snapshot.                                                                    *)
  (*******************************************************************************************************************)
  LET snapID == req.snapshot_id
      volID  == req.volume_id

      Errors ==
        LET checks == [invalid_argument |-> None \in {snapID, volID},
                       already_exists   |-> /\ KeyExists(snapshots, snapID)
                                            /\ snapshots[snapID].source_volume_id /= volID,
                       \* Only checked for a new snapshot, so a retry outlives the volume.
                       not_found        |-> /\ ~KeyExists(snapshots, snapID)
                                            /\ \/ ~KeyExists(volumes, volID)
                                               \/ req.volume_handle \notin {None, volumes[volID].handle}]
        IN {e \in DOMAIN checks : checks[e]}

     \* The request was rejected, or the provider failed to answer.
  IN \/ /\ val = None
        /\ err \in Errors \union ProviderErrors
        /\ UNCHANGED vars

     \* The request was accepted.
     \/ /\ Errors = {}
           \* A retry returns the existing snapshot.
        /\ \/ /\ KeyExists(snapshots, snapID)
              /\ UNCHANGED snapshots

           \/ /\ ~KeyExists(snapshots, snapID)
              \* The handle, creation time and state are the implementation's choice.
              /\ \E h \in SnapshotHandles \ {snapshots[x].handle : x \in DOMAIN snapshots},
                    t \in Timestamps,
                    s \in {"pending", "completed"} :
                   snapshots' = InsertKey(snapshots, snapID, [size_bytes       |-> volumes[volID].capacity_bytes,
                                                              created_at       |-> t,
                                                              state            |-> s,
                                                              handle           |-> h,
                                                              source_volume_id |-> volID])
        /\ val = SnapshotView(snapID)'
        /\ err = None
        /\ UNCHANGED <<volumes, publications>>


CopySnapshotRequests == [source_snapshot_id      : Optional(SnapshotIDs),
                         destination_snapshot_id : Optional(SnapshotIDs),
                         volume_id               : Optional(VolumeIDs)]

CopySnapshot(req, val, err) ==
  (*******************************************************************************************************************)
  (* A snapshot is copied under a new ID, sharing its frozen data, and the copy can be deleted independently         *)
  (* of it.                                                                                                          *)
  (*******************************************************************************************************************)
  LET srcSnapID == req.source_snapshot_id
      dstSnapID == req.destination_snapshot_id
      volID     == req.volume_id

      Errors ==
        LET checks == [invalid_argument |-> \/ None \in {srcSnapID, dstSnapID, volID}
                                            \/ srcSnapID = dstSnapID
                                            \* Only compared, not looked up, since a snapshot outlives its volume.
                                            \/ /\ KeyExists(snapshots, srcSnapID)
                                               /\ snapshots[srcSnapID].source_volume_id /= volID,
                       already_exists   |-> KeyExists(snapshots, dstSnapID),
                       not_found        |-> srcSnapID \notin Optional(DOMAIN snapshots)]
        IN {e \in DOMAIN checks : checks[e]}

     \* The request was rejected, or the provider failed to answer.
  IN \/ /\ val = None
        /\ err \in Errors \union ProviderErrors
        /\ UNCHANGED vars

     \* The request was accepted.
     \/ /\ Errors = {}
        \* The handle and creation time are the implementation's choice.
        /\ \E h \in SnapshotHandles \ {snapshots[x].handle : x \in DOMAIN snapshots},
              t \in Timestamps :
             snapshots' = InsertKey(snapshots, dstSnapID, [created_at |-> t,
                                                           handle     |-> h]
                                                           @@ snapshots[srcSnapID])
        /\ val = SnapshotView(dstSnapID)'
        /\ err = None
        /\ UNCHANGED <<volumes, publications>>


DeleteSnapshotRequests == [snapshot_id : Optional(SnapshotIDs),
                           handle      : Optional(SnapshotHandles)]

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
                           /\ req.handle \notin {None, snapshots[snapID].handle}
                        THEN snapshots
                        \* Record removal is idempotent so that retries succeed.
                        ELSE RemoveKey(snapshots, snapID)
        /\ err = None
        /\ UNCHANGED <<volumes, publications>>

=======================================================================================================================
