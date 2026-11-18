--------------------------------------------------- MODULE Volumes ----------------------------------------------------
EXTENDS EBSProvider

-----------------------------------------------------------------------------------------------------------------------
\* Interface methods

CreateVolumeRequests == [volumeId               : VolumeIds,
                         sizeBytes              : 0..MaxBytes,
                         availabilityZone       : Optional(AvailabilityZones),
                         sourceSnapshotId       : Optional(SnapshotIds),
                         sourceSnapshotVolumeId : Optional(VolumeIds),
                         parameters             : Optional(Parameters)]

CreateVolume(req) ==
  (*******************************************************************************************************************)
  (* A volume of the requested size is created, either blank or holding a snapshot's data. A retry of a              *)
  (* call that already succeeded returns the same volume.                                                            *)
  (*******************************************************************************************************************)
  LET volId     == req.volumeId
      snapId    == req.sourceSnapshotId
      snapVolId == req.sourceSnapshotVolumeId

      \* The set of errors that this call may return in the current system state. If there are multiple possible
      \* errors for a single call, the exact one that is returned is left to the implementation.
      Errors ==
        LET checks == [invalidArgument |-> \/ req.sizeBytes = 0
                                           \/ snapId /= None /\ snapVolId = None
                                           \/ /\ ~KeyExists(volumes, volId)
                                              /\ KeyExists(snapshots, snapId)
                                              /\ \/ req.sizeBytes < snapshots[snapId].sizeBytes
                                                 \/ snapshots[snapId].sourceVolumeId /= snapVolId,
                       alreadyExists   |-> /\ KeyExists(volumes, volId)
                                           \* Only refuse retried calls that would conflict with the existing volume.
                                           /\ \/ volumes[volId].sizeBytes        /= req.sizeBytes
                                              \/ volumes[volId].availabilityZone /= req.availabilityZone
                                              \/ volumes[volId].sourceSnapshotId /= snapId
                                              \/ volumes[volId].parameters       /= req.parameters,
                       notFound        |-> /\ ~KeyExists(volumes, volId)
                                           /\ snapId \notin Optional(DOMAIN snapshots)]
        IN {e \in DOMAIN checks : checks[e]}

     \* The operation was refused, or the provider failed to answer.
  IN \/ /\ val' = None
        /\ err' \in Errors \union ProviderErrors
        /\ UNCHANGED state

     \* The operation was successful.
     \/ /\ Errors = {}
           \* A retry returns the existing volume.
        /\ \/ /\ KeyExists(volumes, volId)
              /\ UNCHANGED volumes

           \/ /\ ~KeyExists(volumes, volId)
              \* The handle and provider data is determined by the implementation.
              /\ \E h \in VolumeHandles,
                    pd \in Optional(ProviderData) :
                   \* No two volumes are given the same handle.
                   /\ h \notin {volumes[v].handle : v \in DOMAIN volumes}
                   /\ volumes' = InsertKey(volumes, volId, [handle           |-> h,
                                                            \* An implementation may allocate more backing storage
                                                            \* if necessary (e.g. to handle OCI's 50GB minimum), but
                                                            \* the volume record must report the exact requested size.
                                                            sizeBytes        |-> req.sizeBytes,
                                                            availabilityZone |-> req.availabilityZone,
                                                            sourceSnapshotId |-> snapId,
                                                            parameters       |-> req.parameters,
                                                            providerData     |-> pd])
        \* NOTE: The UNCHANGED assertion is placed before the val' assignment here to prevent TLC from trying to read 
        \* publications' inside VolumeView(id)' before it is assigned a value, in behaviors where CreateVolume is
        \* the first action.
        /\ UNCHANGED <<publications, snapshots>>
        /\ val' = VolumeView(volId)'
        /\ err' = None


GetVolumeRequests == [volumeId : VolumeIds,
                      handle   : Optional(VolumeHandles)]

GetVolume(req) ==
  (*******************************************************************************************************************)
  (* A volume's current size, state and handle are read, and nothing changes.                                        *)
  (*******************************************************************************************************************)
  LET volId == req.volumeId

      \* The set of errors that this call may return in the current system state. If there are multiple possible
      \* errors for a single call, the exact one that is returned is left to the implementation.
      Errors ==
        LET checks == [notFound |-> \/ ~KeyExists(volumes, volId)
                                    \/ req.handle \notin {None, volumes[volId].handle}]
        IN {e \in DOMAIN checks : checks[e]}

     \* The operation was refused, or the provider failed to answer.
  IN \/ /\ val' = None
        /\ err' \in Errors \union ProviderErrors
        /\ UNCHANGED state

     \* The operation was successful.
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
  \* The set of errors that this call may return in the current system state. If there are multiple possible
  \* errors for a single call, the exact one that is returned is left to the implementation.
  LET Errors ==
    LET checks == [unsupportedCapability |-> ~Capabilities.volumeEnumeration]
    IN {e \in DOMAIN checks : checks[e]}

     \* The operation was refused, or the provider failed to answer.
  IN \/ /\ val' = None
        /\ err' \in Errors \union ProviderErrors
        /\ UNCHANGED state

     \* The operation was successful.
     \/ /\ Errors = {}
        /\ LET refs == {[id |-> id, handle |-> volumes[id].handle] : id \in DOMAIN volumes}
           \* The page and the next token are determined by the implementation.
           IN \E page \in SUBSET refs, next \in Optional(ListTokens) :
                /\ Cardinality(page) <= PageSize(req.maxResults)
                /\ next /= None => Cardinality(page) = PageSize(req.maxResults)
                /\ req.startingToken = None /\ next = None => page = refs
                /\ val' = [volumes |-> page, nextToken |-> next]
                /\ err' = None
        /\ UNCHANGED state


ExpandVolumeRequests == [volumeId  : VolumeIds,
                         handle    : Optional(VolumeHandles),
                         sizeBytes : 0..MaxBytes]

ExpandVolume(req) ==
  (*******************************************************************************************************************)
  (* A volume grows to the requested size, keeping its data. A published volume grows only if the                    *)
  (* implementation supports online expansion, and a request for the current size succeeds without change.           *)
  (*******************************************************************************************************************)
  LET volId == req.volumeId

      \* The set of errors that this call may return in the current system state. If there are multiple possible
      \* errors for a single call, the exact one that is returned is left to the implementation.
      Errors ==
        LET checks == [invalidArgument |-> \/ req.sizeBytes = 0
                                           \/ /\ KeyExists(volumes, volId)
                                              /\ req.sizeBytes < volumes[volId].sizeBytes,
                       notFound        |-> \/ ~KeyExists(volumes, volId)
                                           \/ req.handle \notin {None, volumes[volId].handle},
                       volumeInUse     |-> /\ PublishedNodes(volId) /= {}
                                           /\ ~Capabilities.onlineExpansion
                                           /\ req.sizeBytes > volumes[volId].sizeBytes]
        IN {e \in DOMAIN checks : checks[e]}

     \* The operation was refused, or the provider failed to answer.
  IN \/ /\ val' = None
        /\ err' \in Errors \union ProviderErrors
        /\ UNCHANGED state

     \* The operation was successful.
     \/ /\ Errors = {}
        /\ volumes' = [volumes EXCEPT ![volId].sizeBytes = req.sizeBytes]
        \* The new volume size propagates through to any nodes it is published on via the size field
        \* on the export record. If the target and current volume sizes are equal, which is always the case
        \* if the volume is published and online expansion is not supported, this is a no-op.
        /\ publications' = [p \in DOMAIN publications |->
                              IF p[1] = volId
                              THEN [publications[p] EXCEPT !.export.sizeBytes = req.sizeBytes]
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

      \* The set of errors that this call may return in the current system state. If there are multiple possible
      \* errors for a single call, the exact one that is returned is left to the implementation.
      Errors ==
        LET checks == [notFound              |-> \/ ~KeyExists(volumes, volId)
                                                 \/ req.handle \notin {None, volumes[volId].handle},
                       unsupportedCapability |-> /\ req.readOnly
                                                 /\ ~Capabilities.readOnlyPublish,
                       volumeInUse           |-> \/ PublishedNodes(volId) \ {nodeId} /= {}
                                                 \/ /\ KeyExists(publications, <<volId, nodeId>>)
                                                    /\ req.readOnly /= publications[volId, nodeId].readOnly]
        IN {e \in DOMAIN checks : checks[e]}

     \* The operation was refused, or the provider failed to answer.
  IN \/ /\ val' = None
        /\ err' \in Errors \union ProviderErrors
        /\ UNCHANGED state

     \* The operation was successful.
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
                   /\ x.sizeBytes = volumes[volId].sizeBytes
                   /\ publications' = InsertKey(publications, <<volId, nodeId>>, [export   |-> x,
                                                                                  readOnly |-> req.readOnly])
        /\ val' = PublicationView(volId, nodeId)'
        /\ err' = None
        /\ UNCHANGED <<volumes, snapshots>>


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

     \* The operation was successful, and record removal is idempotent so that retries succeed.
     \/ /\ publications' = RemoveKey(publications, <<volId, nodeId>>)
        /\ val' = None
        /\ err' = None
        /\ UNCHANGED <<volumes, snapshots>>


DeleteVolumeRequests == [volumeId : VolumeIds,
                         handle   : Optional(VolumeHandles)]

DeleteVolume(req) ==
  (*******************************************************************************************************************)
  (* A volume is deleted, so that its data is gone. Snapshots taken from it are unaffected.                          *)
  (*******************************************************************************************************************)
  LET volId == req.volumeId

      \* The set of errors that this call may return in the current system state. If there are multiple possible
      \* errors for a single call, the exact one that is returned is left to the implementation.
      Errors ==
        LET checks == [volumeInUse |-> /\ PublishedNodes(volId) /= {}
                                       /\ req.handle \in {None, volumes[volId].handle}]
        IN {e \in DOMAIN checks : checks[e]}

     \* The operation was refused, or the provider failed to answer.
  IN \/ /\ val' = None
        /\ err' \in Errors \union ProviderErrors
        /\ UNCHANGED state

     \* The operation was successful.
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
