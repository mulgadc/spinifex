--------------------------------------------------- MODULE Volumes ----------------------------------------------------
(*********************************************************************************************************************)
(* The lifecycle of a volume, from its creation to its deletion.                                                     *)
(*                                                                                                                   *)
(* Open questions:                                                                                                   *)
(* - Who decides whether a volume is in-use? An implementation may track attachments itself, or derive the           *)
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

PublishVolumeRequests == [volume_id : Optional(VolumeIDs),
                          handle    : Optional(VolumeHandles),
                          node_id   : Optional(NodeIDs),
                          read_only : BOOLEAN]

UnpublishVolumeRequests == [volume_id : Optional(VolumeIDs),
                            handle    : Optional(VolumeHandles),
                            node_id   : Optional(NodeIDs)]

-----------------------------------------------------------------------------------------------------------------------
\* Control plane operations

CreateVolume(req, val, err) ==
  (*******************************************************************************************************************)
  (* A volume of the requested capacity is created, either blank or holding a snapshot's data, optionally            *)
  (* with seed data written at its start. A retry of a call that already succeeded returns the same volume.          *)
  (*                                                                                                                 *)
  (* Open questions:                                                                                                 *)
  (* - May the capacity be anywhere in the requested range, as CSI allows? An implementation that rounds up          *)
  (*   would then fail the comparison that tells a retry from a conflicting request.                                 *)
  (* - May seed data be combined with a source snapshot? The viperblock implementation refuses, the memory           *)
  (*   implementation accepts.                                                                                       *)
  (* - Must the source snapshot be completed? AWS refuses one that is not.                                           *)
  (* - Must an implementation store the availability zone, parameters and source snapshot, so that it can            *)
  (*   detect a conflicting retry? The viperblock implementation does not.                                           *)
  (* - Should a retry be compared against the original request rather than the current volume? A retry after         *)
  (*   ExpandVolume conflicts on capacity.                                                                           *)
  (* - Should a retry with a different source snapshot volume ID conflict? It is not compared.                       *)
  (* - Is byte equality the meaning of equal parameters, or may an implementation compare them semantically?         *)
  (* - Should MaxSeedBytes be modelled? Seed data larger than it is refused with invalid_argument.                   *)
  (*******************************************************************************************************************)
  LET volID     == req.volume_id
      snapID    == req.source_snapshot_id
      snapVolID == req.source_snapshot_volume_id
      capacity  == req.capacity_range

      \* Any warranted error may be returned, since the order of checks is not part of the contract.
      Errors ==
        LET checks == [invalid_argument       |-> \/ volID = None
                                                  \/ capacity.required_bytes = 0
                                                  \/ /\ capacity.limit_bytes > 0
                                                     /\ capacity.required_bytes > capacity.limit_bytes
                                                  \/ req.seed_data > capacity.required_bytes
                                                  \/ snapID /= None /\ snapVolID = None
                                                  \* Only checked for a new volume, so a retry outlives the snapshot.
                                                  \/ /\ ~KeyExists(volumes, volID)
                                                     /\ KeyExists(snapshots, snapID)
                                                     /\ \/ capacity.required_bytes < snapshots[snapID].size_bytes
                                                        \/ snapshots[snapID].source_volume_id /= snapVolID,
                       \* Seed data is not compared, since it only applies on creation.
                       already_exists         |-> /\ KeyExists(volumes, volID)
                                                  /\ \/ volumes[volID].capacity_bytes     /= capacity.required_bytes
                                                     \/ volumes[volID].availability_zone  /= req.availability_zone
                                                     \/ volumes[volID].source_snapshot_id /= snapID
                                                     \/ volumes[volID].parameters         /= req.parameters,
                       not_found              |-> /\ ~KeyExists(volumes, volID)
                                                  /\ snapID /= None
                                                  /\ ~KeyExists(snapshots, snapID),
                       unsupported_capability |-> /\ req.seed_data > 0
                                                  /\ ~Capabilities.volume_seeding]
        IN {e \in DOMAIN checks : checks[e]}

     \* The request was rejected, or the provider failed to answer.
  IN \/ /\ val = None
        /\ err \in Errors \union ProviderErrors
        /\ UNCHANGED vars

     \* The request was accepted.
     \/ /\ Errors = {}
           \* A retry returns the existing volume.
        /\ \/ /\ KeyExists(volumes, volID)
              /\ UNCHANGED volumes

           \/ /\ ~KeyExists(volumes, volID)
              \* The handle and provider data are the implementation's choice.
              /\ \E h \in VolumeHandles \ {volumes[x].handle : x \in DOMAIN volumes},
                    pd \in Optional(ProviderData) :
                   volumes' = InsertKey(volumes, volID, [capacity_bytes     |-> capacity.required_bytes,
                                                         state              |-> "available",
                                                         handle             |-> h,
                                                         availability_zone  |-> req.availability_zone,
                                                         source_snapshot_id |-> snapID,
                                                         parameters         |-> req.parameters,
                                                         provider_data      |-> pd])
        /\ val = VolumeView(volID)'
        /\ err = None
        /\ UNCHANGED <<publications, snapshots>>

GetVolume(req, val, err) ==
  (*******************************************************************************************************************)
  (* A volume's current capacity, state and handle are read, and nothing changes.                                    *)
  (*                                                                                                                 *)
  (* Open questions:                                                                                                 *)
  (* - Must an implementation compare the handle? The viperblock implementation ignores it. And is not_found         *)
  (*   the right error for a volume that was deleted and recreated under the same ID?                                *)
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
        /\ err \in Errors \union ProviderErrors
        /\ UNCHANGED vars

     \* The request was accepted.
     \/ /\ Errors = {}
        /\ val = VolumeView(volID)
        /\ err = None
        /\ UNCHANGED vars

PublishVolume(req, val, err) ==
  (*******************************************************************************************************************)
  (* A volume's data becomes reachable on a node, through an export that a VM there can attach, either an NBD        *)
  (* URI or a host block device. Reads and writes through the export reach the volume, and a read-only export        *)
  (* refuses writes. Only one node at a time may reach a volume.                                                     *)
  (*                                                                                                                 *)
  (* Open questions:                                                                                                 *)
  (* - Should volume_in_use depend on the exclusion scope? An implementation whose scope is none lets two            *)
  (*   writers in, and one whose scope is node cannot see another node, yet both are refused here.                   *)
  (* - Must a republish to the same node with a different read_only be refused? The viperblock implementation        *)
  (*   refuses it with volume_in_use, while the memory implementation ignores the flag.                              *)
  (* - May two volumes on one node share an NBD URI or a device path?                                                *)
  (* - Must a block export's size equal the volume's capacity?                                                       *)
  (*******************************************************************************************************************)
  LET volID  == req.volume_id
      nodeID == req.node_id

      Errors ==
        LET checks == [invalid_argument       |-> \/ volID = None
                                                  \/ nodeID = None,
                       not_found              |-> \/ ~KeyExists(volumes, volID)
                                                  \/ /\ req.handle /= None
                                                     /\ req.handle /= volumes[volID].handle,
                       unsupported_capability |-> /\ req.read_only
                                                  /\ ~Capabilities.read_only_publish,
                       volume_in_use          |-> \E n \in NodeIDs \ {nodeID} :
                                                    KeyExists(publications, <<volID, n>>)]
        IN {e \in DOMAIN checks : checks[e]}

     \* The request was rejected, or the provider failed to answer.
  IN \/ /\ val = None
        /\ err \in Errors \union ProviderErrors
        /\ UNCHANGED vars

     \* The request was accepted.
     \/ /\ Errors = {}
           \* A retry returns the existing publication.
        /\ \/ /\ KeyExists(publications, <<volID, nodeID>>)
              /\ UNCHANGED publications

           \/ /\ ~KeyExists(publications, <<volID, nodeID>>)
              \* The type of export is the implementation's choice.
              /\ \E x \in Exports :
                   publications' = InsertKey(publications, <<volID, nodeID>>, [export    |-> x,
                                                                               read_only |-> req.read_only])
        /\ volumes' = [volumes EXCEPT ![volID].state = "in-use"]
        /\ val = PublicationView(volID, nodeID)'
        /\ err = None
        /\ UNCHANGED snapshots

UnpublishVolume(req, err) ==
  (*******************************************************************************************************************)
  (* A volume's export on a node is torn down, so that its data is no longer reachable there. Once no node           *)
  (* can reach the volume, it is available again.                                                                    *)
  (*                                                                                                                 *)
  (* Open questions:                                                                                                 *)
  (* - Must an implementation compare the handle? Both the viperblock and memory implementations ignore it.          *)
  (* - May an implementation whose provider failed to seal the volume answer internal without injected               *)
  (*   faults? The viperblock implementation does, and leaves the volume published.                                  *)
  (* - Should a claim that lapses through its TTL be modelled, as a step that happens without a call?                *)
  (*******************************************************************************************************************)
  LET volID  == req.volume_id
      nodeID == req.node_id

      Errors ==
        LET checks == [invalid_argument |-> \/ volID = None
                                            \/ nodeID = None]
        IN {e \in DOMAIN checks : checks[e]}

     \* The request was rejected, or the provider failed to answer.
  IN \/ /\ err \in Errors \union ProviderErrors
        /\ UNCHANGED vars

     \* The request was accepted.
     \/ /\ Errors = {}
        \* Record removal is idempotent so that retries succeed.
        /\ publications' = RemoveKey(publications, <<volID, nodeID>>)
        \* The volume is available again once no node holds it.
        /\ volumes' = IF \E n \in NodeIDs : KeyExists(publications', <<volID, n>>)
                      THEN volumes
                      ELSE [volumes EXCEPT ![volID].state = "available"]
        /\ err = None
        /\ UNCHANGED snapshots

-----------------------------------------------------------------------------------------------------------------------
\* Specification

=======================================================================================================================
