--------------------------------------------------- MODULE Volumes ----------------------------------------------------
(*********************************************************************************************************************)
(* The lifecycle of a volume, from its creation to its deletion.                                                     *)
(*                                                                                                                   *)
(* Open questions:                                                                                                   *)
(* - Who decides whether a volume is in-use? A provider may track attachments itself, or derive the                  *)
(*   state from whether a publication claim is held.                                                                 *)
(*********************************************************************************************************************)
EXTENDS Base

-----------------------------------------------------------------------------------------------------------------------
\* Types

CreateVolumeRequests == [volume_id                 : Optional(VolumeIDs),
                         capacity_range            : CapacityRanges,
                         availability_zone         : Optional(AvailabilityZones),
                         source_snapshot_id        : Optional(SnapshotIDs),
                         source_snapshot_volume_id : Optional(VolumeIDs),
                         parameters                : Optional(Parameters),
                         seed_data                 : Bytes]

GetVolumeRequests == [volume_id : Optional(VolumeIDs),
                      handle    : Optional(VolumeHandles)]

-----------------------------------------------------------------------------------------------------------------------
\* State helpers

-----------------------------------------------------------------------------------------------------------------------
\* Interface methods

CreateVolume(req, val, err) ==
  (*******************************************************************************************************************)
  (* A new volume was requested, either blank or restored from a snapshot, and optionally seeded                     *)
  (* with data. A retry of a request that already succeeded returns the same volume.                                 *)
  (*                                                                                                                 *)
  (* Open questions:                                                                                                 *)
  (* - May the capacity be anywhere in the requested range, as CSI allows? A provider that rounds up                 *)
  (*   would then fail the comparison that tells a retry from a conflicting request.                                 *)
  (* - May seed data be combined with a source snapshot? Viperblock refuses, the memory provider                     *)
  (*   accepts.                                                                                                      *)
  (* - Must the source snapshot be completed? AWS refuses one that is not.                                           *)
  (* - Must a provider store the availability zone, parameters and source snapshot, so that it can                   *)
  (*   detect a conflicting retry? Viperblock does not.                                                              *)
  (* - Should a retry be compared against the original request rather than the current volume? A retry               *)
  (*   after ExpandVolume conflicts on capacity.                                                                     *)
  (* - Should a retry with a different source snapshot volume ID conflict? It is not compared.                       *)
  (* - Is byte equality the meaning of equal parameters, or may a provider compare them semantically?                *)
  (* - Should MaxSeedBytes be modelled? Seed data larger than it is refused with invalid_argument.                   *)
  (*******************************************************************************************************************)
  LET volID     == req.volume_id
      snapID    == req.source_snapshot_id
      snapVolID == req.source_snapshot_volume_id
      capacity  == req.capacity_range

      \* Every error that the request warrants. The order checks run in is not part of the contract,
      \* so any of them may be returned.
      Errors ==
        LET checks == [invalid_argument       |-> \/ volID = None
                                                  \/ capacity.required_bytes = 0
                                                  \/ /\ capacity.limit_bytes > 0
                                                     /\ capacity.required_bytes > capacity.limit_bytes
                                                  \/ req.seed_data > capacity.required_bytes
                                                  \/ snapID /= None /\ snapVolID = None
                                                  \* The source snapshot is only checked for a new volume, so a
                                                  \* retry whose snapshot has since been deleted still succeeds.
                                                  \/ /\ ~KeyExists(volumes, volID)
                                                     /\ KeyExists(snapshots, snapID)
                                                     /\ \/ capacity.required_bytes < snapshots[snapID].size_bytes
                                                        \/ snapshots[snapID].source_volume_id /= snapVolID,
                       \* Seed data is not compared, since it only applies to the volume that a request creates.
                       already_exists         |-> /\ KeyExists(volumes, volID)
                                                  /\ \/ volumes[volID].capacity_bytes     /= capacity.required_bytes
                                                     \/ volumes[volID].availability_zone  /= req.availability_zone
                                                     \/ volumes[volID].source_snapshot_id /= snapID
                                                     \/ volumes[volID].parameters         /= req.parameters,
                       not_found              |-> /\ ~KeyExists(volumes, volID)
                                                  /\ snapID /= None
                                                  /\ ~KeyExists(snapshots, snapID),
                       unsupported_capability |-> req.seed_data > 0 /\ ~Capabilities.volume_seeding]
        IN {e \in DOMAIN checks : checks[e]}

     \* The request was rejected, or the provider failed to answer.
  IN \/ /\ val = None
        /\ err \in Errors \union FaultErrors
        /\ UNCHANGED vars

     \* The request repeated one that already succeeded, so the existing volume was returned.
     \/ /\ Errors = {}
        /\ KeyExists(volumes, volID)
        /\ val = VolumeView(volID, volumes[volID])
        /\ err = None
        /\ UNCHANGED vars

     \* The request was accepted.
     \/ /\ Errors = {}
        /\ ~KeyExists(volumes, volID)
        \* Handles and provider data are opaque, so the provider may choose any, as long as no two
        \* volumes share a handle.
        /\ \E h \in VolumeHandles \ {volumes[x].handle : x \in DOMAIN volumes},
              pd \in Optional(ProviderData) :
             /\ volumes' = InsertKey(volumes, volID, [capacity_bytes     |-> capacity.required_bytes,
                                                      state              |-> "available",
                                                      handle             |-> h,
                                                      availability_zone  |-> req.availability_zone,
                                                      source_snapshot_id |-> snapID,
                                                      parameters         |-> req.parameters,
                                                      provider_data      |-> pd])
             /\ val = VolumeView(volID, volumes'[volID])
             /\ err = None
             /\ UNCHANGED snapshots

GetVolume(req, val, err) ==
  (*******************************************************************************************************************)
  (* The description of a volume was requested, optionally naming the handle it is expected to have.                 *)
  (*                                                                                                                 *)
  (* Open questions:                                                                                                 *)
  (* - Must a provider compare the handle? Viperblock ignores it. And is not_found the right error                   *)
  (*   for a volume that was deleted and recreated under the same ID?                                                *)
  (*******************************************************************************************************************)
  LET volID == req.volume_id

      Errors ==
        LET checks == [invalid_argument |-> volID = None,
                       not_found        |-> \/ ~KeyExists(volumes, volID)
                                            \/ /\ req.handle /= None
                                               /\ req.handle /= volumes[volID].handle]
        IN {e \in DOMAIN checks : checks[e]}

     \* The request was rejected, or the provider failed to answer.
  IN \/ /\ val = None
        /\ err \in Errors \union FaultErrors
        /\ UNCHANGED vars

     \* The request was accepted.
     \/ /\ Errors = {}
        /\ val = VolumeView(volID, volumes[volID])
        /\ err = None
        /\ UNCHANGED vars

-----------------------------------------------------------------------------------------------------------------------
\* Specification

=======================================================================================================================
