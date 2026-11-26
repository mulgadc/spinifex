---- MODULE Types ----

EXTENDS Integers

\* The sets containing all unique volume and snapshot ids.
CONSTANTS
  VolumeIds,
  SnapshotIds
ASSUME VolumeIds /= {}

\* Every possible volume or snapshot size (in bytes).
CONSTANT Sizes
ASSUME Sizes \subseteq Nat

CONSTANT Timestamps
ASSUME Timestamps \subseteq Nat

\* Opaque values that the contract passes through without interpreting.
CONSTANT Parameters, Metadata

\* Every path at which a node may expose a published volume.
CONSTANT ExportPaths

CONSTANT MaxListResults
ASSUME MaxListResults \in Nat \ {0}

\* Stands in for an optional field that a request or response omitted.
CONSTANT None
ASSUME None \notin UNION {VolumeIds, SnapshotIds, Parameters, Metadata}

\* The optional behaviour that the implementation advertises, which never changes.
CONSTANT Capabilities
ASSUME Capabilities \in [onlineExpansion         : BOOLEAN,
                         sparseExtentReporting   : BOOLEAN,
                         crashConsistentSnapshot : BOOLEAN,
                         readOnlyPublish         : BOOLEAN,
                         volumeEnumeration       : BOOLEAN,
                         snapshotEnumeration     : BOOLEAN,
                         snapshotCopy            : BOOLEAN,
                         ownerRouting            : BOOLEAN,
                         exclusion               : [scope : {"none", "node", "cluster"},
                                                    claimTtlSeconds : Nat,
                                                    fencesLostClaim : BOOLEAN]]

\* Whether the provider may fail to answer at any point in a behaviour.
CONSTANT ProviderMayFail
ASSUME ProviderMayFail \in BOOLEAN

\* ==== Interface Signature ====

\* The errors that an implementation may return when its provider fails to answer.
ProviderErrs == IF ProviderMayFail
                THEN {"Internal", "Unavailable"}
                \* Otherwise an implementation could conform by refusing to answer.
                ELSE {}

\* How a published volume is reached on its node, either over NBD or as a host block device.
Exports == [volumeId : VolumeIds,
            kind     : {"nbd", "block"},
            path     : ExportPaths,
            readOnly : BOOLEAN]

\* An opaque page token, made from the ids that it covers so that a symmetry set on the ids stays sound.
\* Only Seen reads inside a token, and an omitted token has seen nothing.
Token(ids)  == [seen |-> ids]
Seen(t)     == IF t = None THEN {} ELSE t.seen
Tokens(Ids) == {Token(ids) : ids \in SUBSET Ids}

\* The part of a volume that the implementation returns.
VolumeViews   == [id        : VolumeIds,
                  status    : {"deleted", "available", "in-use"},
                  size      : Sizes,
                  metadata  : Metadata \union {None}]

\* The part of a snapshot that the implementation returns.
SnapshotViews == [id        : SnapshotIds,
                  size      : Sizes,
                  volumeId  : VolumeIds,
                  createdAt : Timestamps,
                  metadata  : Metadata \union {None}]

GetCapabilitiesArgs == {<<>>}
GetCapabilitiesErrs == ProviderErrs
GetCapabilitiesVals == {Capabilities}

CreateVolumeArgs    == [id                    : VolumeIds,
                        size                  : Sizes,
                        snapshotId            : SnapshotIds \union {None},
                        parameters            : Parameters \union {None}]
CreateVolumeErrs    == {"InvalidArgument", "AlreadyExists", "NotFound"} \union ProviderErrs
CreateVolumeVals    == VolumeViews

GetVolumeArgs       == [id                    : VolumeIds]
GetVolumeErrs       == {"NotFound"} \union ProviderErrs
GetVolumeVals       == VolumeViews

ListVolumesArgs     == [maxResults            : -1..(MaxListResults + 1),
                        startToken            : Tokens(VolumeIds) \union {None}]
ListVolumesErrs     == {"UnsupportedCapability"} \union ProviderErrs
ListVolumesVals     == [results               : SUBSET [id : VolumeIds,
                                                        metadata : Metadata \union {None}],
                        nextToken             : Tokens(VolumeIds) \union {None}]

ExpandVolumeArgs    == [id                    : VolumeIds,
                        size                  : Sizes]
ExpandVolumeErrs    == {"InvalidArgument", "NotFound", "VolumeInUse"} \union ProviderErrs
ExpandVolumeVals    == VolumeViews

PublishVolumeArgs   == [id                    : VolumeIds,
                        readOnly              : BOOLEAN]
PublishVolumeErrs   == {"NotFound", "UnsupportedCapability", "VolumeInUse"} \union ProviderErrs
PublishVolumeVals   == Exports

UnpublishVolumeArgs == [id                    : VolumeIds]
UnpublishVolumeErrs == ProviderErrs
UnpublishVolumeVals == {None}

DeleteVolumeArgs    == [id                    : VolumeIds]
DeleteVolumeErrs    == {"VolumeInUse"} \union ProviderErrs
DeleteVolumeVals    == {None}

CreateSnapshotArgs  == [id                    : SnapshotIds,
                        volumeId              : VolumeIds]
CreateSnapshotErrs  == {"AlreadyExists", "NotFound"} \union ProviderErrs
CreateSnapshotVals  == SnapshotViews

CopySnapshotArgs    == [sourceId              : SnapshotIds,
                        destinationId         : SnapshotIds,
                        volumeId              : VolumeIds]
CopySnapshotErrs    == {"InvalidArgument", "AlreadyExists", "NotFound", "UnsupportedCapability"} \union ProviderErrs
CopySnapshotVals    == SnapshotViews

GetSnapshotArgs     == [id                    : SnapshotIds]
GetSnapshotErrs     == {"NotFound"} \union ProviderErrs
GetSnapshotVals     == SnapshotViews

ListSnapshotsArgs   == [maxResults            : -1..(MaxListResults + 1),
                        startToken            : Tokens(SnapshotIds) \union {None}]
ListSnapshotsErrs   == {"UnsupportedCapability"} \union ProviderErrs
ListSnapshotsVals   == [results               : SUBSET [id : SnapshotIds,
                                                        volumeId : VolumeIds,
                                                        metadata : Metadata \union {None}],
                        nextToken             : Tokens(SnapshotIds) \union {None}]

DeleteSnapshotArgs  == [id                    : SnapshotIds]
DeleteSnapshotErrs  == ProviderErrs
DeleteSnapshotVals  == {None}

Methods == [GetCapabilities |-> [args |-> GetCapabilitiesArgs,
                                 errs |-> GetCapabilitiesErrs,
                                 vals |-> GetCapabilitiesVals],
            CreateVolume    |-> [args |-> CreateVolumeArgs,
                                 errs |-> CreateVolumeErrs,
                                 vals |-> CreateVolumeVals],
            GetVolume       |-> [args |-> GetVolumeArgs,
                                 errs |-> GetVolumeErrs,
                                 vals |-> GetVolumeVals],
            ListVolumes     |-> [args |-> ListVolumesArgs,
                                 errs |-> ListVolumesErrs,
                                 vals |-> ListVolumesVals],
            ExpandVolume    |-> [args |-> ExpandVolumeArgs,
                                 errs |-> ExpandVolumeErrs,
                                 vals |-> ExpandVolumeVals],
            PublishVolume   |-> [args |-> PublishVolumeArgs,
                                 errs |-> PublishVolumeErrs,
                                 vals |-> PublishVolumeVals],
            UnpublishVolume |-> [args |-> UnpublishVolumeArgs,
                                 errs |-> UnpublishVolumeErrs,
                                 vals |-> UnpublishVolumeVals],
            DeleteVolume    |-> [args |-> DeleteVolumeArgs,
                                 errs |-> DeleteVolumeErrs,
                                 vals |-> DeleteVolumeVals],
            CreateSnapshot  |-> [args |-> CreateSnapshotArgs,
                                 errs |-> CreateSnapshotErrs,
                                 vals |-> CreateSnapshotVals],
            CopySnapshot    |-> [args |-> CopySnapshotArgs,
                                 errs |-> CopySnapshotErrs,
                                 vals |-> CopySnapshotVals],
            GetSnapshot     |-> [args |-> GetSnapshotArgs,
                                 errs |-> GetSnapshotErrs,
                                 vals |-> GetSnapshotVals],
            ListSnapshots   |-> [args |-> ListSnapshotsArgs,
                                 errs |-> ListSnapshotsErrs,
                                 vals |-> ListSnapshotsVals],
            DeleteSnapshot  |-> [args |-> DeleteSnapshotArgs,
                                 errs |-> DeleteSnapshotErrs,
                                 vals |-> DeleteSnapshotVals]]

Calls == UNION {{[method |-> m, args |-> a] : a \in Methods[m].args} : m \in DOMAIN Methods}

Ok(state, val) == [state |-> state, err |-> None, val |-> val]
Err(state, err) == [state |-> state, err |-> err, val |-> None]

\* ==== Provider State ====

Volumes   == [id         : VolumeIds,
              size       : Sizes,
              snapshotId : SnapshotIds \union {None},
              parameters : Parameters \union {None},
              metadata   : Metadata \union {None},
              inUse      : BOOLEAN]

Snapshots == [id         : SnapshotIds,
              size       : Sizes,
              volumeId   : VolumeIds,
              createdAt  : Timestamps,
              metadata   : Metadata \union {None}]

====
