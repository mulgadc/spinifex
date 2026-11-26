------------------------------------------------- MODULE Provider --------------------------------------------------

EXTENDS
  Naturals,
  Sequences,
  FiniteSets,
  Functions,
  TLC

\* Every thread/process that may invoke the implementation's methods, and observe the results.
CONSTANT Callers
ASSUME Callers /= {}

\* Every possible snapshot creation time.
CONSTANT Timestamps
ASSUME Timestamps /= {}

\* Opaque values that the contract passes through without interpreting.
CONSTANT Metadata

VARIABLE pending

VARIABLES
  results,
  \* Every volume and snapshot that an implementation is able to observe at any given moment.
  volumes,
  snapshots

\* The record with the given ID, or NoValue if there is none. An omitted ID, such as NoValue, names no record.
Volume(id)   == IF \E v \in volumes : v.id = id   THEN CHOOSE v \in volumes   : v.id = id ELSE NoValue
Snapshot(id) == IF \E s \in snapshots : s.id = id THEN CHOOSE s \in snapshots : s.id = id ELSE NoValue

\* Whether the implementation currently exports the volume, and the export that it holds for it.
IsExported(volId) == \E x \in exports : x.volumeId = volId
ExportOf(volId)   == CHOOSE x \in exports : x.volumeId = volId

Errors(req) == CASE req.op = "CreateVolume"    -> [invalidArgument |-> \/ req.sizeBytes = 0
                                                                       \/ /\ Volume(req.volumeId) = NoValue
                                                                          /\ Snapshot(req.sourceSnapshotId) /= NoValue
                                                                          /\ req.sizeBytes > Snapshot(req.sourceSnapshotId).size,
                                                   alreadyExists   |-> /\ Volume(req.volumeId) /= NoValue
                                                                       \* Only refuse retried calls that would conflict with the existing volume.
                                                                       /\ \/ req.sizeBytes > Volume(req.volumeId).size
                                                                          \/ req.sourceSnapshotId /= Volume(req.volumeId).snapId
                                                                          \* Volume parameters are compared to determine semantic "compatibility".
                                                                          \* Byte equivalence may, or may not, be part of that check. It is up to
                                                                          \* each implementation how exactly this comparison is carried out.
                                                                          \/ req.parameters /= Volume(req.volumeId).parameters,
                                                   notFound        |-> /\ Volume(req.volumeId) = NoValue
                                                                       /\ req.sourceSnapshotId /= NoValue
                                                                       /\ req.sourceSnapshotId \notin {s.id : s \in snapshots}]
                 [] req.op = "GetVolume"       -> [notFound              |-> Volume(req.volumeId) = NoValue]
                 [] req.op = "ListVolumes"     -> [unsupportedCapability |-> ~Capabilities.volumeEnumeration]
                 [] req.op = "ExpandVolume"    -> [invalidArgument       |-> req.sizeBytes = 0,
                                                   notFound              |-> Volume(req.volumeId) = NoValue,
                                                   volumeInUse           |-> /\ ~Capabilities.onlineExpansion
                                                                             /\ IsExported(req.volumeId)
                                                                             /\ req.sizeBytes > Volume(req.volumeId).size]
                 [] req.op = "PublishVolume"   -> [notFound              |-> Volume(req.volumeId) = NoValue,
                                                   unsupportedCapability |-> /\ req.readOnly
                                                                             /\ ~Capabilities.readOnlyPublish,
                                                   \* A retry may not change how the volume is published.
                                                   volumeInUse           |-> /\ IsExported(req.volumeId)
                                                                             /\ req.readOnly /= ExportOf(req.volumeId).readOnly]
                 \* Only a failed provider refuses an unpublish, and a volume that does not exist is not refused.
                 [] req.op = "UnpublishVolume" -> <<>>
                 \* A volume that does not exist is not refused, so that retries succeed.
                 [] req.op = "DeleteVolume"    -> [volumeInUse           |-> IsExported(req.volumeId)]

Invoke(c, m) ==
  /\ pending[c] = NoValue /\ inflight[c] = NoValue
  /\ \E req \in Params(m) : pending' = [pending EXCEPT ![c] = req]
  /\ UNCHANGED <<inflight, returned>>

Caller == \E c \in Callers, m \in Methods : Invoke(c, m)

Volumes == [id     : VolumeIds,
            snapId : SnapshotIds \union {NoValue},
            size   : Sizes,
            \* Caller-provided configuration parameters passed to the implementation at volume creation. Opaque to the
            \* contract.
            params : Parameters \union {NoValue},
            meta   : Metadata \union {NoValue}]

CreateVolume(req) ==
  /\ \/ /\ Volume(req.volumeId) = NoValue
        /\ \E v \in Volumes : 
             /\ v.id = req.volumeId
             /\ v.snapId = req.sourceSnapshotId
             /\ v.size = req.sizeBytes
             /\ v.params = req.parameters
             /\ volumes' = volumes \union {v}
     \/ /\ Volume(req.volumeId) /= NoValue
        /\ UNCHANGED volumes
  /\ UNCHANGED snapshots

ExpandVolume(req) ==
  /\ LET v == Volume(req.volumeId)
     IN /\ v /= NoValue
        /\ volumes' = volumes \ {v}
  /\ UNCHANGED snapshots

DeleteVolume(req) ==
  /\ LET v == Volume(req.volumeId)
     IN /\ v /= NoValue
        /\ volumes' = volumes \ {v}
  /\ UNCHANGED snapshots




Snapshots == [id        : SnapshotIds,
              volId     : VolumeIds,
              size      : Sizes,
              createdAt : Timestamps,
              metadata  : Metadata \union {NoValue}]

VARIABLES
  exports,
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

\* The errors that a call may return.
ErrorCodes == {"invalidArgument", "alreadyExists", "notFound", "unsupportedCapability", "volumeInUse",
               "internal", "unavailable"}



\* How a published volume is reached on its node, either over NBD or as a host block device.
Exports == [kind      : {"nbd", "block"},
            path      : ExportPaths,
            sizeBytes : 1..MaxBytes]

\* A volume published to a node, and whether it was published read-only.
Publications == [export   : Exports,
                 readOnly : BOOLEAN]

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
                            "availabilityZone",
                            "metadata"})

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
    Cardinality(PublishedNodes(volId)) = 0

ExportPathsAreUniquePerNode ==
  \A i, j \in DOMAIN publications :
    i[2] = j[2] /\ publications[i].export.path = publications[j].export.path => i = j

ExportSizeMatchesVolume ==
  \A p \in DOMAIN publications :
    publications[p].export.sizeBytes = volumes[p[1]].sizeBytes

=======================================================================================================================
