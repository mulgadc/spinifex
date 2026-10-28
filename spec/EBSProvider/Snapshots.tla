-------------------------------------------------- MODULE Snapshots ---------------------------------------------------
(*********************************************************************************************************************)
(* The lifecycle of a snapshot, from its creation to its deletion.                                                   *)
(*********************************************************************************************************************)
EXTENDS Base

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
  (* A copy of a snapshot was requested, as a second, independently addressable snapshot over the                    *)
  (* same data.                                                                                                      *)
  (*                                                                                                                 *)
  (* Open questions:                                                                                                 *)
  (* - Should a retried copy return the existing copy, as a retried CreateVolume does, rather than                   *)
  (*   already_exists?                                                                                               *)
  (* - May a source that is not completed be copied? AWS refuses until it is.                                        *)
  (* - What state does a copy start in? The memory provider always returns completed, while AWS starts               *)
  (*   a copy as pending.                                                                                            *)
  (*******************************************************************************************************************)
  LET srcSnapID == req.source_snapshot_id
      dstSnapID == req.destination_snapshot_id
      volID     == req.volume_id

      Errors ==
        LET checks == [invalid_argument |-> \/ srcSnapID = None
                                            \/ dstSnapID = None
                                            \/ volID = None
                                            \/ srcSnapID = dstSnapID
                                            \* The volume is only compared, not looked up, since a snapshot
                                            \* outlives the volume it was taken from.
                                            \/ /\ KeyExists(snapshots, srcSnapID)
                                               /\ snapshots[srcSnapID].source_volume_id /= volID,
                       already_exists   |-> KeyExists(snapshots, dstSnapID),
                       not_found        |-> /\ srcSnapID /= None
                                            /\ ~KeyExists(snapshots, srcSnapID)]
        IN {e \in DOMAIN checks : checks[e]}

     \* The request was rejected, or the provider failed to answer.
  IN \/ /\ val = None
        /\ err \in Errors \union FaultErrors
        /\ UNCHANGED vars

     \* The request was accepted.
     \/ /\ Errors = {}
        /\ \E h \in SnapshotHandles \ {snapshots[x].handle : x \in DOMAIN snapshots},
              time \in Timestamps :
             /\ snapshots' = InsertKey(snapshots, dstSnapID,
                                       [created_at |-> time, handle |-> h] @@ snapshots[srcSnapID])
             /\ val = SnapshotView(dstSnapID, snapshots'[dstSnapID])
             /\ err = None
             /\ UNCHANGED volumes

DeleteSnapshot(req, err) ==
  (*******************************************************************************************************************)
  (* The deletion of a snapshot was requested, optionally naming the handle it is expected to have.                  *)
  (*                                                                                                                 *)
  (* Open questions:                                                                                                 *)
  (* - Is the handle check part of the contract? Viperblock ignores the handle and deletes anyway.                   *)
  (* - May a pending snapshot be deleted? AWS allows it.                                                             *)
  (* - What does success promise? The Go docs say the snapshot is gone, but a snapshot whose handle did              *)
  (*   not match is left in place.                                                                                   *)
  (*******************************************************************************************************************)
  LET snapID == req.snapshot_id

      Errors ==
        LET checks == [invalid_argument |-> snapID = None]
        IN {e \in DOMAIN checks : checks[e]}

     \* The request was rejected, or the provider failed to answer.
  IN \/ /\ err \in Errors \union FaultErrors
        /\ UNCHANGED vars

     \* The request was accepted. Deleting an absent snapshot succeeds, so a retry does too. A snapshot
     \* whose handle does not match is left in place, since it is not the one the request meant.
     \/ /\ Errors = {}
        /\ err = None
        /\ snapshots' = IF \/ ~KeyExists(snapshots, snapID)
                           \/ req.handle /= None /\ snapshots[snapID].handle /= req.handle
                        THEN snapshots
                        ELSE RemoveKey(snapshots, snapID)
        /\ UNCHANGED volumes

=======================================================================================================================
