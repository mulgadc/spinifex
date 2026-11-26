--------------------------------------------------- MODULE Volumes ----------------------------------------------------
EXTENDS EBSProvider

CreateVolume(req) ==
  LET volId     == req.volumeId
      snapId    == req.sourceSnapshotId

      \* The set of errors that this call may return in the current system state. If there are multiple possible
      \* errors for a single call, the exact one that is returned is left to the implementation.
      Errors ==
        LET checks == [invalidArgument |-> \/ req.sizeBytes = 0
                                           \/ /\ ~KeyExists(volumes, volId)
                                              /\ KeyExists(snapshots, snapId)
                                              /\ req.sizeBytes < snapshots[snapId].sizeBytes,
                       alreadyExists   |-> /\ KeyExists(volumes, volId)
                                           \* Only refuse retried calls that would conflict with the existing volume.
                                           /\ \/ volumes[volId].sizeBytes < req.sizeBytes
                                              \/ volumes[volId].availabilityZone /= req.availabilityZone
                                              \/ volumes[volId].sourceSnapshotId /= snapId
                                              \* Volume parameters are compared to determine semantic "compatibility".
                                              \* Byte equivalence may, or may not, be part of that check. It is up to
                                              \* each implementation how exactly this comparison is carried out.
                                              \/ volumes[volId].parameters /= req.parameters,
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
              \* The metadata is determined by the implementation.
              /\ \E md \in Optional(Metadata) :
                   \* The volume record must report the exact requested size, but the implementation may allocate more
                   \* backing storage if necessary. To handle OCI's 50GB minimum, for example. 
                   /\ volumes' = InsertKey(volumes, volId, [sizeBytes        |-> req.sizeBytes,
                                                            availabilityZone |-> req.availabilityZone,
                                                            sourceSnapshotId |-> snapId,
                                                            parameters       |-> req.parameters,
                                                            metadata         |-> md])
        \* NOTE: The UNCHANGED assertion is placed before the val' assignment here to prevent TLC from trying to read 
        \* publications' inside VolumeView(id)' before it is assigned a value, in behaviors where CreateVolume is
        \* the first action.
        /\ UNCHANGED <<publications, snapshots>>
        /\ val' = VolumeView(volId)'
        /\ err' = None


GetVolume(req) ==
  LET volId == req.volumeId

      \* The set of errors that this call may return in the current system state. If there are multiple possible
      \* errors for a single call, the exact one that is returned is left to the implementation.
      Errors ==
        LET checks == [notFound |-> ~KeyExists(volumes, volId)]
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


ListVolumes(req) ==
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
        /\ LET covered   == IF req.startingToken = None THEN {} ELSE req.startingToken
               remaining == DOMAIN volumes \ covered
               size      == PageSize(req.maxResults)
           \* The page and the next token are determined by the implementation.
           IN \E ids  \in SUBSET remaining,
                 gone \in SUBSET (VolumeIds \ DOMAIN volumes) :
                /\ Cardinality(ids) <= size
                /\ Cardinality(ids) < size => ids = remaining
                /\ val' = [volumes   |-> {[id |-> id, metadata |-> volumes[id].metadata] : id \in ids},
                           nextToken |-> IF remaining \ ids = {}
                                         THEN None
                                         ELSE covered \union ids \union gone]
                /\ err' = None
        /\ UNCHANGED state


ExpandVolume(req) ==
  LET volId == req.volumeId

      \* The set of errors that this call may return in the current system state. If there are multiple possible
      \* errors for a single call, the exact one that is returned is left to the implementation.
      Errors ==
        LET checks == [invalidArgument |-> req.sizeBytes = 0,
                       notFound        |-> ~KeyExists(volumes, volId),
                       volumeInUse     |-> /\ ~Capabilities.onlineExpansion
                                           /\ PublishedNodes(volId) /= {}
                                           /\ req.sizeBytes > volumes[volId].sizeBytes]
        IN {e \in DOMAIN checks : checks[e]}

     \* The operation was refused, or the provider failed to answer.
  IN \/ /\ val' = None
        /\ err' \in Errors \union ProviderErrors
        /\ UNCHANGED state

     \* The operation was successful.
     \/ /\ Errors = {}
        /\ \/ /\ req.sizeBytes > volumes[volId].sizeBytes
              /\ volumes' = [volumes EXCEPT ![volId].sizeBytes = req.sizeBytes ]
              \* The new volume size propagates through to any nodes it is published on.
              /\ publications' = [p \in DOMAIN publications |->
                                    IF p[1] = volId
                                    THEN [publications[p] EXCEPT !.export.sizeBytes = req.sizeBytes]
                                    ELSE publications[p]]
              /\ val' = VolumeView(volId)'
              /\ err' = None
              /\ UNCHANGED snapshots

           \* A no-op if the target size is less than or equal to the volume's current size, ensuring that this
           \* operation is idempotent.
           \/ /\ req.sizeBytes <= volumes[volId].sizeBytes
              /\ val' = VolumeView(volId)
              /\ err' = None
              /\ UNCHANGED state


PublishVolume(req) ==
  LET volId  == req.volumeId

      \* The set of errors that this call may return in the current system state. If there are multiple possible
      \* errors for a single call, the exact one that is returned is left to the implementation.
      Errors ==
        LET checks == [notFound              |-> ~KeyExists(volumes, volId),
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


UnpublishVolume(req) ==
  LET volId  == req.volumeId

     \* The provider failed to answer.
  IN \/ /\ val' = None
        /\ err' \in ProviderErrors
        /\ UNCHANGED state

     \* The operation was successful, and record removal is idempotent so that retries succeed.
     \/ /\ publications' = RemoveKey(publications, <<volId, nodeId>>)
        /\ val' = None
        /\ err' = None
        /\ UNCHANGED <<volumes, snapshots>>


DeleteVolume(req) ==
  LET volId == req.volumeId

      \* The set of errors that this call may return in the current system state. If there are multiple possible
      \* errors for a single call, the exact one that is returned is left to the implementation.
      Errors ==
        LET checks == [volumeInUse |-> PublishedNodes(volId) /= {}]
        IN {e \in DOMAIN checks : checks[e]}

     \* The operation was refused, or the provider failed to answer.
  IN \/ /\ val' = None
        /\ err' \in Errors \union ProviderErrors
        /\ UNCHANGED state

     \* The operation was successful.
     \/ /\ Errors = {}
        \* Record removal is idempotent so that retries succeed.
        /\ volumes' = RemoveKey(volumes, volId)
        /\ val' = None
        /\ err' = None
        /\ UNCHANGED <<publications, snapshots>>

=======================================================================================================================
