--------------------------------------------------- MODULE Volumes ----------------------------------------------------
(*********************************************************************************************************************)
(* The lifecycle of a volume, from its creation to its deletion.                                                     *)
(*********************************************************************************************************************)
EXTENDS EBSProvider

-----------------------------------------------------------------------------------------------------------------------
\* Interface methods

CreateVolumeRequests == [volume_id                 : Optional(VolumeIDs),
                         capacity_range            : CapacityRanges,
                         availability_zone         : Optional(AvailabilityZones),
                         source_snapshot_id        : Optional(SnapshotIDs),
                         source_snapshot_volume_id : Optional(VolumeIDs),
                         parameters                : Optional(Parameters),
                         seed_data                 : Bytes]

CreateVolume(req, val, err) ==
  (*******************************************************************************************************************)
  (* A volume of the requested capacity is created, either blank or holding a snapshot's data, optionally            *)
  (* with seed data written at its start. A retry of a call that already succeeded returns the same volume.          *)
  (*******************************************************************************************************************)
  LET volID     == req.volume_id
      snapID    == req.source_snapshot_id
      snapVolID == req.source_snapshot_volume_id
      capacity  == req.capacity_range

      \* Any warranted error may be returned, since the order of checks is not part of the contract.
      Errors ==
        LET checks == [invalid_argument       |-> \/ volID = None
                                                  \/ capacity.required_bytes <= 0
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
                                                  /\ snapID \notin Optional(DOMAIN snapshots),
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


GetVolumeRequests == [volume_id : Optional(VolumeIDs),
                      handle    : Optional(VolumeHandles)]

GetVolume(req, val, err) ==
  (*******************************************************************************************************************)
  (* A volume's current capacity, state and handle are read, and nothing changes.                                    *)
  (*******************************************************************************************************************)
  LET volID == req.volume_id

      Errors ==
        LET checks == [invalid_argument |-> volID = None,
                       not_found        |-> \/ ~KeyExists(volumes, volID)
                                            \/ req.handle \notin {None, volumes[volID].handle}]
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


ListVolumesRequests == [max_results    : -1..(MaxListResults + 1),
                        starting_token : Optional(ListTokens)]

ListVolumes(req, val, err) ==
  (*******************************************************************************************************************)
  (* Some of the volumes are listed, a page at a time, and nothing changes. A call with no starting token that       *)
  (* returns no next token lists every volume.                                                                       *)
  (*******************************************************************************************************************)
  LET Errors ==
    LET checks == [unsupported_capability |-> ~Capabilities.volume_enumeration]
    IN {e \in DOMAIN checks : checks[e]}

     \* The request was rejected, or the provider failed to answer.
  IN \/ /\ val = None
        /\ err \in Errors \union ProviderErrors
        /\ UNCHANGED vars

     \* The request was accepted.
     \/ /\ Errors = {}
        /\ LET refs == {[id |-> id, handle |-> volumes[id].handle] : id \in DOMAIN volumes}
           \* The page and the next token are the implementation's choice.
           IN \E page \in SUBSET refs, next \in Optional(ListTokens) :
                /\ Cardinality(page) <= PageSize(req.max_results)
                /\ next /= None => Cardinality(page) = PageSize(req.max_results)
                /\ req.starting_token = None /\ next = None => page = refs
                /\ val = [volumes |-> page, next_token |-> next]
        /\ err = None
        /\ UNCHANGED vars


ExpandVolumeRequests == [volume_id      : Optional(VolumeIDs),
                         handle         : Optional(VolumeHandles),
                         capacity_range : CapacityRanges]

ExpandVolume(req, val, err) ==
  (*******************************************************************************************************************)
  (* A volume grows to the requested capacity, keeping its data. A published volume grows only if the                *)
  (* implementation supports online expansion, and a request for the current capacity succeeds without change.       *)
  (*******************************************************************************************************************)
  LET volID    == req.volume_id
      capacity == req.capacity_range

      Errors ==
        LET checks == [invalid_argument |-> \/ volID = None
                                            \/ capacity.required_bytes = 0
                                            \/ /\ capacity.limit_bytes > 0
                                               /\ capacity.required_bytes > capacity.limit_bytes
                                            \/ /\ KeyExists(volumes, volID)
                                               /\ capacity.required_bytes < volumes[volID].capacity_bytes,
                       not_found        |-> \/ ~KeyExists(volumes, volID)
                                            \/ req.handle \notin {None, volumes[volID].handle},
                       volume_in_use    |-> /\ PublishedNodes(volID) /= {}
                                            /\ ~Capabilities.online_expansion
                                            /\ capacity.required_bytes > volumes[volID].capacity_bytes]
        IN {e \in DOMAIN checks : checks[e]}

     \* The request was rejected, or the provider failed to answer.
  IN \/ /\ val = None
        /\ err \in Errors \union ProviderErrors
        /\ UNCHANGED vars

     \* The request was accepted.
     \/ /\ Errors = {}
        /\ volumes' = [volumes EXCEPT ![volID].capacity_bytes = capacity.required_bytes]
        /\ val = VolumeView(volID)'
        /\ err = None
        /\ UNCHANGED <<publications, snapshots>>


PublishVolumeRequests == [volume_id : Optional(VolumeIDs),
                          handle    : Optional(VolumeHandles),
                          node_id   : Optional(NodeIDs),
                          read_only : BOOLEAN]

PublishVolume(req, val, err) ==
  (*******************************************************************************************************************)
  (* A volume's data becomes reachable on a node, through an export that a VM there can attach, either an NBD        *)
  (* URI or a host block device. Reads and writes through the export reach the volume, and a read-only export        *)
  (* refuses writes. Only one node at a time may reach a volume.                                                     *)
  (*******************************************************************************************************************)
  LET volID  == req.volume_id
      nodeID == req.node_id

      Errors ==
        LET checks == [invalid_argument       |-> None \in {volID, nodeID},
                       not_found              |-> \/ ~KeyExists(volumes, volID)
                                                  \/ req.handle \notin {None, volumes[volID].handle},
                       unsupported_capability |-> /\ req.read_only
                                                  /\ ~Capabilities.read_only_publish,
                       volume_in_use          |-> PublishedNodes(volID) \ {nodeID} /= {}]
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


UnpublishVolumeRequests == [volume_id : Optional(VolumeIDs),
                            handle    : Optional(VolumeHandles),
                            node_id   : Optional(NodeIDs)]

UnpublishVolume(req, err) ==
  (*******************************************************************************************************************)
  (* A volume's export on a node is torn down, so that its data is no longer reachable there. Once no node           *)
  (* can reach the volume, it is available again.                                                                    *)
  (*******************************************************************************************************************)
  LET volID  == req.volume_id
      nodeID == req.node_id

      Errors ==
        LET checks == [invalid_argument |-> None \in {volID, nodeID}]
        IN {e \in DOMAIN checks : checks[e]}

     \* The request was rejected, or the provider failed to answer.
  IN \/ /\ err \in Errors \union ProviderErrors
        /\ UNCHANGED vars

     \* The request was accepted.
     \/ /\ Errors = {}
        \* Record removal is idempotent so that retries succeed.
        /\ publications' = RemoveKey(publications, <<volID, nodeID>>)
        \* The volume is available again once no node holds it.
        /\ volumes' = IF \/ ~KeyExists(volumes, volID)
                         \/ PublishedNodes(volID)' /= {}
                      THEN volumes
                      ELSE [volumes EXCEPT ![volID].state = "available"]
        /\ err = None
        /\ UNCHANGED snapshots


DeleteVolumeRequests == [volume_id : Optional(VolumeIDs),
                         handle    : Optional(VolumeHandles)]

DeleteVolume(req, err) ==
  (*******************************************************************************************************************)
  (* A volume is deleted, so that its data is gone. Snapshots taken from it are unaffected.                          *)
  (*******************************************************************************************************************)
  LET volID == req.volume_id

      Errors ==
        LET checks == [invalid_argument |-> volID = None,
                       volume_in_use    |-> /\ PublishedNodes(volID) /= {}
                                            /\ req.handle \in {None, volumes[volID].handle}]
        IN {e \in DOMAIN checks : checks[e]}

     \* The request was rejected, or the provider failed to answer.
  IN \/ /\ err \in Errors \union ProviderErrors
        /\ UNCHANGED vars

     \* The request was accepted.
     \/ /\ Errors = {}
        \* Record removal is idempotent so that retries succeed.
        /\ volumes' = IF /\ KeyExists(volumes, volID)
                         /\ req.handle \notin {None, volumes[volID].handle}
                      THEN volumes
                      ELSE RemoveKey(volumes, volID)
        /\ err = None
        /\ UNCHANGED <<publications, snapshots>>

-----------------------------------------------------------------------------------------------------------------------
\* Specification

=======================================================================================================================
