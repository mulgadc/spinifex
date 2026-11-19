------------------------------------------------- MODULE EBSProvider --------------------------------------------------
EXTENDS
  Naturals,
  Sequences,
  FiniteSets,
  Functions,
  TLC

-----------------------------------------------------------------------------------------------------------------------
\* Constants

CONSTANTS
  MaxBytes,
  VolumeIds, VolumeHandles,
  NodeIds, ExportPaths,
  SnapshotIds, SnapshotHandles,
  AvailabilityZones, Timestamps,
  Parameters, ProviderData,
  ListTokens, MaxListResults,
  \* The optional behaviour that the implementation advertises, which never changes.
  Capabilities,
  \* Whether the provider may fail to answer at any point in a behaviour.
  ProviderMayFail,
  \* Stands in for an optional field that a request or response omitted.
  None

ASSUME MaxBytes \in Nat \ {0}
ASSUME VolumeIds /= {} /\ VolumeHandles /= {}
ASSUME NodeIds /= {} => ExportPaths /= {}
ASSUME SnapshotIds /= {} => SnapshotHandles /= {} /\ Timestamps /= {}
ASSUME ListTokens /= {}
ASSUME MaxListResults \in Nat \ {0}
ASSUME None \notin UNION {VolumeIds, VolumeHandles, NodeIds, SnapshotIds, SnapshotHandles,
                          AvailabilityZones, Timestamps, Parameters, ProviderData, ListTokens}
ASSUME Capabilities \in [onlineExpansion         : BOOLEAN,
                         sparseExtentReporting   : BOOLEAN,
                         crashConsistentSnapshot : BOOLEAN,
                         readOnlyPublish         : BOOLEAN,
                         volumeEnumeration       : BOOLEAN,
                         snapshotEnumeration     : BOOLEAN,
                         snapshotCopy            : BOOLEAN,
                         ownerRouting            : BOOLEAN,
                         exclusion               : [scope           : {"none", "node", "cluster"},
                                                    claimTtlSeconds : Nat,
                                                    fencesLostClaim : BOOLEAN]]
ASSUME ProviderMayFail \in BOOLEAN

-----------------------------------------------------------------------------------------------------------------------
\* Variables

VARIABLES
  \* Every volume that the implementation exposes, keyed by volume ID.
  volumes,
  \* Every publication that the implementation exposes, keyed by the volume and the node it is published to.
  publications,
  \* Every snapshot that the implementation exposes, keyed by snapshot ID.
  snapshots,
  \* The method that the last step called and the request it was called with, or None for a step without a call.
  method, params,
  \* The value or error that the last step's call returned.
  val, err

\* The state that the implementation exposes, which a refused call leaves unchanged.
state == <<volumes, publications, snapshots>>

\* The call that the last step answered.
call == <<method, params, val, err>>

vars == <<state, call>>

-----------------------------------------------------------------------------------------------------------------------
\* Miscellaneous

IsPartialFn(f, D, R) == DOMAIN f \subseteq D /\ \A x \in DOMAIN f : f[x] \in R

Optional(S) == S \union {None}

InsertKey(f, x, y) == f @@ (x :> y)

RemoveKey(f, x) == RestrictDomain(f, LAMBDA y : y /= x)

KeyExists(f, x) == x \in DOMAIN f

-----------------------------------------------------------------------------------------------------------------------
\* Types

\* The interface methods that a call may name.
Methods == {"GetCapabilities",
            "CreateVolume", "GetVolume", "ListVolumes", "ExpandVolume",
            "PublishVolume", "UnpublishVolume", "DeleteVolume",
            "CreateSnapshot", "CopySnapshot", "ListSnapshots", "DeleteSnapshot"}

\* The errors that a call may return.
ErrorCodes == {"invalidArgument", "alreadyExists", "notFound", "unsupportedCapability", "volumeInUse",
               "internal", "unavailable"}

\* The errors that an implementation may return when its provider fails to answer.
ProviderErrors == IF ProviderMayFail
                  THEN {"internal", "unavailable"}
                  \* Otherwise an implementation could conform by refusing to answer.
                  ELSE {}

Volumes == [handle           : VolumeHandles,
            sizeBytes        : 1..MaxBytes,
            availabilityZone : Optional(AvailabilityZones),
            sourceSnapshotId : Optional(SnapshotIds),
            \* The following fields are opaque to the contract. They may differ in
            \* content/structure between implementations.
            parameters       : Optional(Parameters),
            providerData     : Optional(ProviderData)]

\* How a published volume is reached on its node, either over NBD or as a host block device.
Exports == [kind      : {"nbd", "block"},
            path      : ExportPaths,
            sizeBytes : 1..MaxBytes]

\* A volume published to a node, and whether it was published read-only.
Publications == [export   : Exports,
                 readOnly : BOOLEAN]

Snapshots == [sizeBytes      : 1..MaxBytes,
              createdAt      : Timestamps,
              handle         : SnapshotHandles,
              sourceVolumeId : VolumeIds]

-----------------------------------------------------------------------------------------------------------------------
\* State helpers

\* The nodes that a volume is published to.
PublishedNodes(volId) == {n \in NodeIds : KeyExists(publications, <<volId, n>>)}

\* The part of a volume that the implementation returns.
VolumeView(volId) ==
  [id    |-> volId,
   state |-> CASE volId \notin DOMAIN volumes -> "deleted"
               [] PublishedNodes(volId) = {}  -> "available"
               [] PublishedNodes(volId) /= {} -> "in-use"]
  @@
  Restrict(volumes[volId], {"sizeBytes",
                            "handle",
                            "availabilityZone",
                            "providerData"})

\* The part of a publication that the implementation returns.
PublicationView(volId, nodeId) == [volumeId |-> volId,
                                   nodeId   |-> nodeId,
                                   export   |-> publications[volId, nodeId].export]

\* The part of a snapshot that the implementation returns.
SnapshotView(snapId) == ("id" :> snapId) @@ snapshots[snapId]

\* The number of refs that a page may hold, with an omitted or oversized request clamped to the maximum.
PageSize(m) == IF m <= 0 \/ m > MaxListResults THEN MaxListResults ELSE m

-----------------------------------------------------------------------------------------------------------------------
\* Calls

\* A call to method m with req, which action answers by setting val and err.
Call(m, req, Action(_)) == /\ method' = m
                           /\ params' = req
                           /\ Action(req)

-----------------------------------------------------------------------------------------------------------------------
\* Initial state

Init ==
  /\ volumes      = <<>>
  /\ publications = <<>>
  /\ snapshots    = <<>>
  \* No call has been answered.
  /\ method       = None
  /\ params       = None
  /\ val          = None
  /\ err          = None

-----------------------------------------------------------------------------------------------------------------------
\* Invariants

TypeOK ==
  /\ IsPartialFn(volumes, VolumeIds, Volumes)
  /\ IsPartialFn(publications, VolumeIds \X NodeIds, Publications)
  /\ IsPartialFn(snapshots, SnapshotIds, Snapshots)
  /\ method \in Optional(Methods)
  /\ err \in Optional(ErrorCodes)
  /\ err \in ErrorCodes => val = None

ExistsIfPublished ==
  \A volId \in VolumeIds :
    PublishedNodes(volId) /= {} => volId \in DOMAIN volumes

OnlyPublishedOnce ==
  \A volId \in VolumeIds :
    Cardinality(PublishedNodes(volId)) <= 1

HandlesAreUnique ==
  /\ \A i, j \in DOMAIN volumes :
       volumes[i].handle = volumes[j].handle => i = j
  /\ \A i, j \in DOMAIN snapshots :
       snapshots[i].handle = snapshots[j].handle => i = j

ExportPathsAreUniquePerNode ==
  \A i, j \in DOMAIN publications :
    i[2] = j[2] /\ publications[i].export.path = publications[j].export.path => i = j

ExportSizeMatchesVolume ==
  \A p \in DOMAIN publications :
    publications[p].export.sizeBytes = volumes[p[1]].sizeBytes

=======================================================================================================================
