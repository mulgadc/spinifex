--------------------------------------------------- MODULE Volumes ----------------------------------------------------
(*********************************************************************************************************************)
(* The lifecycle of a volume, from its creation to its deletion.                                                     *)
(*********************************************************************************************************************)
EXTENDS EBSProvider

-----------------------------------------------------------------------------------------------------------------------
\* Interface methods

CreateVolumeRequests == [volumeId               : VolumeIds,
                         capacityRange          : CapacityRanges,
                         availabilityZone       : Optional(AvailabilityZones),
                         sourceSnapshotId       : Optional(SnapshotIds),
                         sourceSnapshotVolumeId : Optional(VolumeIds),
                         parameters             : Optional(Parameters),
                         seedData               : Bytes]

CreateVolume(req) ==
  (*******************************************************************************************************************)
  (* A volume of the requested capacity is created, either blank or holding a snapshot's data, optionally            *)
  (* with seed data written at its start. A retry of a call that already succeeded returns the same volume.          *)
  (*******************************************************************************************************************)
  LET volId     == req.volumeId
      snapId    == req.sourceSnapshotId
      snapVolId == req.sourceSnapshotVolumeId
      capacity  == req.capacityRange

      \* Any warranted error may be returned, since the order of checks is not part of the contract.
      Errors ==
        LET checks == [invalidArgument       |-> \/ capacity.requiredBytes = 0
                                                 \/ /\ capacity.limitBytes > 0
                                                    /\ capacity.requiredBytes > capacity.limitBytes
                                                 \/ req.seedData > capacity.requiredBytes
                                                 \/ snapId /= None /\ snapVolId = None
                                                 \* Only checked for a new volume, so a retry outlives the snapshot.
                                                 \/ /\ ~KeyExists(volumes, volId)
                                                    /\ KeyExists(snapshots, snapId)
                                                    /\ \/ capacity.requiredBytes < snapshots[snapId].sizeBytes
                                                       \/ snapshots[snapId].sourceVolumeId /= snapVolId,
                       \* Seed data is not compared, since it only applies on creation.
                       alreadyExists         |-> /\ KeyExists(volumes, volId)
                                                 /\ \/ volumes[volId].capacityBytes    /= capacity.requiredBytes
                                                    \/ volumes[volId].availabilityZone /= req.availabilityZone
                                                    \/ volumes[volId].sourceSnapshotId /= snapId
                                                    \/ volumes[volId].parameters       /= req.parameters,
                       notFound              |-> /\ ~KeyExists(volumes, volId)
                                                 /\ snapId \notin Optional(DOMAIN snapshots),
                       unsupportedCapability |-> /\ req.seedData > 0
                                                 /\ ~Capabilities.volumeSeeding]
        IN {e \in DOMAIN checks : checks[e]}

     \* The request was rejected, or the provider failed to answer.
  IN \/ /\ val' = None
        /\ err' \in Errors \union ProviderErrors
        /\ UNCHANGED state

     \* The request was accepted.
     \/ /\ Errors = {}
           \* A retry returns the existing volume.
        /\ \/ /\ KeyExists(volumes, volId)
              /\ UNCHANGED volumes

           \/ /\ ~KeyExists(volumes, volId)
              \* The handle and provider data are the implementation's choice.
              /\ \E h \in VolumeHandles \ {volumes[x].handle : x \in DOMAIN volumes},
                    pd \in Optional(ProviderData) :
                   volumes' = InsertKey(volumes, volId, [capacityBytes    |-> capacity.requiredBytes,
                                                         state            |-> "available",
                                                         handle           |-> h,
                                                         availabilityZone |-> req.availabilityZone,
                                                         sourceSnapshotId |-> snapId,
                                                         parameters       |-> req.parameters,
                                                         providerData     |-> pd])
        /\ val' = VolumeView(volId)'
        /\ err' = None
        /\ UNCHANGED <<publications, snapshots>>


GetVolumeRequests == [volumeId : VolumeIds,
                      handle   : Optional(VolumeHandles)]

GetVolume(req) ==
  (*******************************************************************************************************************)
  (* A volume's current capacity, state and handle are read, and nothing changes.                                    *)
  (*******************************************************************************************************************)
  LET volId == req.volumeId

      Errors ==
        LET checks == [notFound |-> \/ ~KeyExists(volumes, volId)
                                    \/ req.handle \notin {None, volumes[volId].handle}]
        IN {e \in DOMAIN checks : checks[e]}

     \* The request was rejected, or the provider failed to answer.
  IN \/ /\ val' = None
        /\ err' \in Errors \union ProviderErrors
        /\ UNCHANGED state

     \* The request was accepted.
     \/ /\ Errors = {}
        /\ val' = VolumeView(volId)
        /\ err' = None
        /\ UNCHANGED state


ListVolumesRequests == [maxResults    : -1..(MaxListResults + 1),
                        startingToken : Optional(ListTokens)]

ListVolumes(req) ==
  (*******************************************************************************************************************)
  (* Some of the volumes are listed, a page at a time, and nothing changes. A call with no starting token that       *)
  (* returns no next token lists every volume.                                                                       *)
  (*******************************************************************************************************************)
  LET Errors ==
    LET checks == [unsupportedCapability |-> ~Capabilities.volumeEnumeration]
    IN {e \in DOMAIN checks : checks[e]}

     \* The request was rejected, or the provider failed to answer.
  IN \/ /\ val' = None
        /\ err' \in Errors \union ProviderErrors
        /\ UNCHANGED state

     \* The request was accepted.
     \/ /\ Errors = {}
        /\ LET refs == {[id |-> id, handle |-> volumes[id].handle] : id \in DOMAIN volumes}
           \* The page and the next token are the implementation's choice.
           IN \E page \in SUBSET refs, next \in Optional(ListTokens) :
                /\ Cardinality(page) <= PageSize(req.maxResults)
                /\ next /= None => Cardinality(page) = PageSize(req.maxResults)
                /\ req.startingToken = None /\ next = None => page = refs
                /\ val' = [volumes |-> page, nextToken |-> next]
                /\ err' = None
        /\ UNCHANGED state


ExpandVolumeRequests == [volumeId      : VolumeIds,
                         handle        : Optional(VolumeHandles),
                         capacityRange : CapacityRanges]

ExpandVolume(req) ==
  (*******************************************************************************************************************)
  (* A volume grows to the requested capacity, keeping its data. A published volume grows only if the                *)
  (* implementation supports online expansion, and a request for the current capacity succeeds without change.       *)
  (*******************************************************************************************************************)
  LET volId    == req.volumeId
      capacity == req.capacityRange

      Errors ==
        LET checks == [invalidArgument |-> \/ capacity.requiredBytes = 0
                                           \/ /\ capacity.limitBytes > 0
                                              /\ capacity.requiredBytes > capacity.limitBytes
                                           \/ /\ KeyExists(volumes, volId)
                                              /\ capacity.requiredBytes < volumes[volId].capacityBytes,
                       notFound        |-> \/ ~KeyExists(volumes, volId)
                                           \/ req.handle \notin {None, volumes[volId].handle},
                       volumeInUse     |-> /\ PublishedNodes(volId) /= {}
                                           /\ ~Capabilities.onlineExpansion
                                           /\ capacity.requiredBytes > volumes[volId].capacityBytes]
        IN {e \in DOMAIN checks : checks[e]}

     \* The request was rejected, or the provider failed to answer.
  IN \/ /\ val' = None
        /\ err' \in Errors \union ProviderErrors
        /\ UNCHANGED state

     \* The request was accepted.
     \/ /\ Errors = {}
        /\ volumes' = [volumes EXCEPT ![volId].capacityBytes = capacity.requiredBytes]
        \* The new volume size propagates through to any nodes it is published on via the size field
        \* on the export record. If the target and current volume sizes are equal, which is always the case
        \* if the volume is published and online expansion is not supported, this is a no-op.
        /\ publications' = [p \in DOMAIN publications |->
                              IF p[1] = volId
                              THEN [publications[p] EXCEPT !.export.sizeBytes = capacity.requiredBytes]
                              ELSE publications[p]]
        /\ val' = VolumeView(volId)'
        /\ err' = None
        /\ UNCHANGED snapshots


PublishVolumeRequests == [volumeId : VolumeIds,
                          handle   : Optional(VolumeHandles),
                          nodeId   : NodeIds,
                          readOnly : BOOLEAN]

PublishVolume(req) ==
  (*******************************************************************************************************************)
  (* A volume's data becomes reachable on a node, through an export that a VM there can attach, either an NBD        *)
  (* URI or a host block device. Reads and writes through the export reach the volume, and a read-only export        *)
  (* refuses writes. Only one node at a time may reach a volume.                                                     *)
  (*******************************************************************************************************************)
  LET volId  == req.volumeId
      nodeId == req.nodeId

      Errors ==
        LET checks == [notFound              |-> \/ ~KeyExists(volumes, volId)
                                                 \/ req.handle \notin {None, volumes[volId].handle},
                       unsupportedCapability |-> /\ req.readOnly
                                                 /\ ~Capabilities.readOnlyPublish,
                       volumeInUse           |-> PublishedNodes(volId) \ {nodeId} /= {}]
        IN {e \in DOMAIN checks : checks[e]}

     \* The request was rejected, or the provider failed to answer.
  IN \/ /\ val' = None
        /\ err' \in Errors \union ProviderErrors
        /\ UNCHANGED state

     \* The request was accepted.
     \/ /\ Errors = {}
           \* A retry returns the existing publication.
        /\ \/ /\ KeyExists(publications, <<volId, nodeId>>)
              /\ UNCHANGED publications

           \/ /\ ~KeyExists(publications, <<volId, nodeId>>)
              \* The exact kind of export (NBD, block etc) is left up to the implementation.
              /\ \E x \in Exports :
                   \* Each export path must be unique within the target node. To prevent attempts to mount
                   \* two different block devices to the same host file-system location, for example.
                   /\ x.path \notin {publications[p].export.path :
                                       p \in {<<v, n>> \in DOMAIN publications : n = nodeId}}
                   \* The export's backing storage may be larger than the volume we are publishing. We
                   \* attach the size of the volume to the export record so that QEMU may limit the size
                   \* of the virtual disk exposed to the guest.
                   /\ x.sizeBytes = volumes[volId].capacityBytes
                   /\ publications' = InsertKey(publications, <<volId, nodeId>>, [export   |-> x,
                                                                                  readOnly |-> req.readOnly])
        /\ volumes' = [volumes EXCEPT ![volId].state = "in-use"]
        /\ val' = PublicationView(volId, nodeId)'
        /\ err' = None
        /\ UNCHANGED snapshots


UnpublishVolumeRequests == [volumeId : VolumeIds,
                            handle   : Optional(VolumeHandles),
                            nodeId   : NodeIds]

UnpublishVolume(req) ==
  (*******************************************************************************************************************)
  (* A volume's export on a node is torn down, so that its data is no longer reachable there. Once no node           *)
  (* can reach the volume, it is available again.                                                                    *)
  (*******************************************************************************************************************)
  LET volId  == req.volumeId
      nodeId == req.nodeId

     \* The provider failed to answer.
  IN \/ /\ val' = None
        /\ err' \in ProviderErrors
        /\ UNCHANGED state

     \* The request was accepted, and record removal is idempotent so that retries succeed.
     \/ /\ publications' = RemoveKey(publications, <<volId, nodeId>>)
        \* The volume is available again once no node holds it.
        /\ volumes' = IF \/ ~KeyExists(volumes, volId)
                         \/ PublishedNodes(volId)' /= {}
                      THEN volumes
                      ELSE [volumes EXCEPT ![volId].state = "available"]
        /\ val' = None
        /\ err' = None
        /\ UNCHANGED snapshots


DeleteVolumeRequests == [volumeId : VolumeIds,
                         handle   : Optional(VolumeHandles)]

DeleteVolume(req) ==
  (*******************************************************************************************************************)
  (* A volume is deleted, so that its data is gone. Snapshots taken from it are unaffected.                          *)
  (*******************************************************************************************************************)
  LET volId == req.volumeId

      Errors ==
        LET checks == [volumeInUse |-> /\ PublishedNodes(volId) /= {}
                                       /\ req.handle \in {None, volumes[volId].handle}]
        IN {e \in DOMAIN checks : checks[e]}

     \* The request was rejected, or the provider failed to answer.
  IN \/ /\ val' = None
        /\ err' \in Errors \union ProviderErrors
        /\ UNCHANGED state

     \* The request was accepted.
     \/ /\ Errors = {}
        \* Record removal is idempotent so that retries succeed.
        /\ volumes' = IF /\ KeyExists(volumes, volId)
                         /\ req.handle \notin {None, volumes[volId].handle}
                      THEN volumes
                      ELSE RemoveKey(volumes, volId)
        /\ val' = None
        /\ err' = None
        /\ UNCHANGED <<publications, snapshots>>

=======================================================================================================================
