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
  (* A volume's data is frozen as a snapshot, from which new volumes can be restored. The snapshot holds the         *)
  (* volume's data as it was when the call returned, even while it is pending, and a pending snapshot settles later  *)
  (* without a call (SettleSnapshot). A retry of a call that already succeeded returns the same snapshot.            *)
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
        LET checks == [invalid_argument       |-> \/ None \in {srcSnapID, dstSnapID, volID}
                                                  \/ srcSnapID = dstSnapID
                                                  \* Only compared, not looked up, since a snapshot outlives its volume.
                                                  \/ /\ KeyExists(snapshots, srcSnapID)
                                                     /\ snapshots[srcSnapID].source_volume_id /= volID,
                       already_exists         |-> KeyExists(snapshots, dstSnapID),
                       not_found              |-> srcSnapID \notin Optional(DOMAIN snapshots),
                       unsupported_capability |-> ~Capabilities.snapshot_copy]
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


ListSnapshotsRequests == [max_results    : -1..(MaxListResults + 1),
                          starting_token : Optional(ListTokens),
                          snapshot_id    : Optional(SnapshotIDs)]

\* The ref that ListSnapshots returns for a snapshot, carrying its current state.
SnapshotRef(id) == [id               |-> id,
                    source_volume_id |-> snapshots[id].source_volume_id,
                    handle           |-> snapshots[id].handle,
                    state            |-> snapshots[id].state]

ListSnapshots(req, val, err) ==
  (*******************************************************************************************************************)
  (* Some of the snapshots are listed, a page at a time, and nothing changes. A call with no starting token that     *)
  (* returns no next token lists every snapshot. A call that names a snapshot returns that snapshot in its current   *)
  (* state, or nothing when it does not exist, so an accepted snapshot is never reported absent while it is pending. *)
  (*******************************************************************************************************************)
  LET snapID == req.snapshot_id

      Errors ==
        \* A call that names a snapshot reads only that one, so it needs no enumeration.
        LET checks == [unsupported_capability |-> snapID = None /\ ~Capabilities.snapshot_enumeration]
        IN {e \in DOMAIN checks : checks[e]}

     \* The request was rejected, or the provider failed to answer.
  IN \/ /\ val = None
        /\ err \in Errors \union ProviderErrors
        /\ UNCHANGED vars

     \* The request named one snapshot, so paging is ignored.
     \/ /\ Errors = {}
        /\ snapID /= None
        /\ val = [snapshots  |-> IF KeyExists(snapshots, snapID) THEN {SnapshotRef(snapID)} ELSE {},
                  next_token |-> None]
        /\ err = None
        /\ UNCHANGED vars

     \* The request was accepted.
     \/ /\ Errors = {}
        /\ snapID = None
        /\ LET refs == {SnapshotRef(id) : id \in DOMAIN snapshots}
           \* The page and the next token are the implementation's choice.
           IN \E page \in SUBSET refs, next \in Optional(ListTokens) :
                /\ Cardinality(page) <= PageSize(req.max_results)
                /\ next /= None => Cardinality(page) = PageSize(req.max_results)
                /\ req.starting_token = None /\ next = None => page = refs
                /\ val = [snapshots |-> page, next_token |-> next]
        /\ err = None
        /\ UNCHANGED vars


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

-----------------------------------------------------------------------------------------------------------------------
\* Steps without a call

SettleSnapshot(snapID) ==
  (*******************************************************************************************************************)
  (* A pending snapshot settles as completed or error, as the provider finishes it. Its data is unchanged, since it  *)
  (* was fixed when CreateSnapshot returned, and a snapshot that has settled never changes state again.              *)
  (*******************************************************************************************************************)
  /\ KeyExists(snapshots, snapID)
  /\ snapshots[snapID].state = "pending"
  /\ \E s \in {"completed", "error"} :
       snapshots' = [snapshots EXCEPT ![snapID].state = s]
  /\ UNCHANGED <<volumes, publications>>

=======================================================================================================================
