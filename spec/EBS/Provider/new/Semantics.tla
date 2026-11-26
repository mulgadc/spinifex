---- MODULE Semantics ----

EXTENDS Types, FiniteSets

\* ==== Helpers ====

Select(S, P(_)) ==
  IF \E x \in S : P(x)
  THEN CHOOSE x \in S : P(x)
  ELSE None

\* The set S with the element old swapped for new.
Replace(S, old, new) == (S \ {old}) \union {new}

\* The view that the implementation returns for the abstract volume v.
VolumeView(v) == [id       |-> v.id,
                  status   |-> IF v.inUse THEN "in-use" ELSE "available",
                  size     |-> v.size,
                  metadata |-> v.metadata]

\* The view that the implementation returns for the abstract snapshot s.
SnapshotView(s) == [id        |-> s.id,
                    size      |-> s.size,
                    volumeId  |-> s.volumeId,
                    createdAt |-> s.createdAt,
                    metadata  |-> s.metadata]

\* Every page that a list call may return over the present ids, and the token for the next page. A token holds the
\* ids that earlier pages have seen, and may also name ids that do not exist, such as ids that were deleted.
Pages(S, tokens, startToken, maxResults) ==
  LET \* An omitted or oversized maxResults is clamped to the maximum.
      size      == IF maxResults <= 0 \/ maxResults > MaxListResults THEN MaxListResults ELSE maxResults
      Remaining == S \ Seen(startToken)
      Choices   == {p \in SUBSET Remaining : /\ Cardinality(p) <= size
                                             /\ Cardinality(p) < size => p = Remaining}
      \* The next token covers this page and every earlier one, plus any ids that do not exist.
      NextTokens(p) == IF Remaining \ p = {} THEN {None}
                       ELSE {t \in tokens : /\ Seen(startToken) \union p \subseteq Seen(t)
                                            /\ Seen(t) \intersect Remaining = p}
  IN UNION {{[page |-> p, nextToken |-> t] : t \in NextTokens(p)} : p \in Choices}

\* ==== Method Outcomes ====

GetCapabilitiesOutcomes(state, args) ==
  {Err(state, e) : e \in ProviderErrs} \union
  {Ok(state, Capabilities)}

CreateVolumeOutcomes(state, args) ==
  LET volume   == Select(state.volumes, LAMBDA v : args.id = v.id)
      snapshot == Select(state.snapshots, LAMBDA s : args.snapshotId = s.id)

      Errs ==
        LET raised == [InvalidArgument |-> \/ args.size = 0
                                           \/ /\ volume = None
                                              /\ snapshot /= None
                                              /\ args.size < snapshot.size,
                       AlreadyExists   |-> /\ volume /= None
                                           \* Only refuse retried calls that would conflict with the existing volume.
                                           /\ \/ args.size > volume.size
                                              \/ args.snapshotId /= volume.snapshotId
                                              \* Volume parameters are compared to determine semantic "compatibility".
                                              \* Byte equivalence may, or may not, be part of that check. It is up to
                                              \* each implementation how exactly this comparison is carried out.
                                              \/ args.parameters /= volume.parameters,
                       NotFound        |-> /\ volume = None
                                           /\ args.snapshotId /= None
                                           /\ snapshot = None]
        IN {e \in DOMAIN raised : raised[e]}

  IN {Err(state, e) : e \in ProviderErrs} \union
     CASE Errs /= {}     -> {Err(state, e) : e \in Errs}
       [] volume /= None -> {Ok(state, VolumeView(volume))}
       [] OTHER          -> {Ok([state EXCEPT !.volumes = @ \union {v}], VolumeView(v))
                              : v \in [id         : {args.id},
                                       size       : {args.size},
                                       snapshotId : {args.snapshotId},
                                       parameters : {args.parameters},
                                       metadata   : Metadata \union {None},
                                       inUse      : {FALSE}]}

GetVolumeOutcomes(state, args) ==
  LET volume == Select(state.volumes, LAMBDA v : args.id = v.id)

  IN {Err(state, e) : e \in ProviderErrs} \union
     CASE volume = None -> {Err(state, "NotFound")}
       [] OTHER         -> {Ok(state, VolumeView(volume))}

ListVolumesOutcomes(state, args) ==
  LET Errs ==
        LET raised == [UnsupportedCapability |-> ~Capabilities.volumeEnumeration]
        IN {e \in DOMAIN raised : raised[e]}

  IN {Err(state, e) : e \in ProviderErrs} \union
     CASE Errs /= {} -> {Err(state, e) : e \in Errs}
       [] OTHER      -> {Ok(state, [results   |-> {[id |-> v.id, metadata |-> v.metadata]
                                                      : v \in {v \in state.volumes : v.id \in r.page}},
                                    nextToken |-> r.nextToken])
                          : r \in Pages({v.id : v \in state.volumes}, Tokens(VolumeIds), args.startToken,
                                        args.maxResults)}

ExpandVolumeOutcomes(state, args) ==
  LET volume == Select(state.volumes, LAMBDA v : args.id = v.id)

      Errs ==
        LET raised == [InvalidArgument |-> args.size = 0,
                       NotFound        |-> volume = None,
                       VolumeInUse     |-> /\ volume /= None
                                           /\ ~Capabilities.onlineExpansion
                                           /\ volume.inUse
                                           /\ args.size > volume.size]
        IN {e \in DOMAIN raised : raised[e]}

  IN {Err(state, e) : e \in ProviderErrs} \union
     CASE Errs /= {}               -> {Err(state, e) : e \in Errs}
       \* A target size at or below the current size is a no-op, so that retries succeed.
       [] args.size <= volume.size -> {Ok(state, VolumeView(volume))}
       [] OTHER                    -> LET v == [volume EXCEPT !.size = args.size]
                                      IN {Ok([state EXCEPT !.volumes = Replace(@, volume, v)], VolumeView(v))}

PublishVolumeOutcomes(state, args) ==
  LET volume == Select(state.volumes, LAMBDA v : args.id = v.id)
      export == Select(state.exports, LAMBDA x : args.id = x.volumeId)

      Errs ==
        LET raised == [NotFound              |-> volume = None,
                       UnsupportedCapability |-> /\ args.readOnly
                                                 /\ ~Capabilities.readOnlyPublish,
                       VolumeInUse           |-> /\ volume /= None
                                                 \* A volume in use without our export is in use by someone else.
                                                 /\ \/ /\ export = None
                                                       /\ volume.inUse
                                                    \* A retry may not change how the volume is published.
                                                    \/ /\ export /= None
                                                       /\ args.readOnly /= export.readOnly]
        IN {e \in DOMAIN raised : raised[e]}

  IN {Err(state, e) : e \in ProviderErrs} \union
     CASE Errs /= {}     -> {Err(state, e) : e \in Errs}
       \* A retry returns the existing export.
       [] export /= None -> {Ok(state, export)}
       \* The kind and path of the export are left to the implementation, but no two exports share a path.
       [] OTHER          -> {Ok([state EXCEPT !.volumes = Replace(@, volume, [volume EXCEPT !.inUse = TRUE]),
                                              !.exports = @ \union {x}],
                                x)
                              : x \in {x \in Exports : /\ x.volumeId = args.id
                                                       /\ x.readOnly = args.readOnly
                                                       /\ x.path \notin {o.path : o \in state.exports}}}

UnpublishVolumeOutcomes(state, args) ==
  LET volume == Select(state.volumes, LAMBDA v : args.id = v.id)
      export == Select(state.exports, LAMBDA x : args.id = x.volumeId)

  IN {Err(state, e) : e \in ProviderErrs} \union
     \* Without our export there is nothing to remove, and a volume in use by someone else stays in use.
     CASE export = None -> {Ok(state, None)}
       [] OTHER         -> {Ok([state EXCEPT !.volumes = Replace(@, volume, [volume EXCEPT !.inUse = FALSE]),
                                             !.exports = @ \ {export}],
                               None)}

DeleteVolumeOutcomes(state, args) ==
  LET volume == Select(state.volumes, LAMBDA v : args.id = v.id)

      Errs ==
        LET raised == [VolumeInUse |-> /\ volume /= None
                                       /\ volume.inUse]
        IN {e \in DOMAIN raised : raised[e]}

  IN {Err(state, e) : e \in ProviderErrs} \union
     CASE Errs /= {}    -> {Err(state, e) : e \in Errs}
       \* A volume that does not exist is not refused, so that retries succeed.
       [] volume = None -> {Ok(state, None)}
       [] OTHER         -> {Ok([state EXCEPT !.volumes = @ \ {volume}], None)}

CreateSnapshotOutcomes(state, args) ==
  LET volume   == Select(state.volumes, LAMBDA v : args.volumeId = v.id)
      snapshot == Select(state.snapshots, LAMBDA s : args.id = s.id)

      Errs ==
        LET raised == [AlreadyExists |-> /\ snapshot /= None
                                         /\ args.volumeId /= snapshot.volumeId,
                       \* Only checked for a new snapshot, so a retry outlives the volume.
                       NotFound      |-> /\ snapshot = None
                                         /\ volume = None]
        IN {e \in DOMAIN raised : raised[e]}

  IN {Err(state, e) : e \in ProviderErrs} \union
     CASE Errs /= {}       -> {Err(state, e) : e \in Errs}
       \* A retry returns the existing snapshot.
       [] snapshot /= None -> {Ok(state, SnapshotView(snapshot))}
       \* The creation time and metadata are left to the implementation.
       [] OTHER            -> {Ok([state EXCEPT !.snapshots = @ \union {s}], SnapshotView(s))
                                : s \in [id        : {args.id},
                                         size      : {volume.size},
                                         volumeId  : {args.volumeId},
                                         createdAt : Timestamps,
                                         metadata  : Metadata \union {None}]}

CopySnapshotOutcomes(state, args) ==
  LET source      == Select(state.snapshots, LAMBDA s : args.sourceId = s.id)
      destination == Select(state.snapshots, LAMBDA s : args.destinationId = s.id)

      Errs ==
        LET raised == [InvalidArgument       |-> \/ args.sourceId = args.destinationId
                                                 \* Only compared, not looked up, since a snapshot outlives its volume.
                                                 \/ /\ source /= None
                                                    /\ args.volumeId /= source.volumeId,
                       AlreadyExists         |-> destination /= None,
                       NotFound              |-> source = None,
                       UnsupportedCapability |-> ~Capabilities.snapshotCopy]
        IN {e \in DOMAIN raised : raised[e]}

  IN {Err(state, e) : e \in ProviderErrs} \union
     CASE Errs /= {} -> {Err(state, e) : e \in Errs}
       \* The creation time and metadata are left to the implementation.
       [] OTHER      -> {Ok([state EXCEPT !.snapshots = @ \union {s}], SnapshotView(s))
                          : s \in [id        : {args.destinationId},
                                   size      : {source.size},
                                   volumeId  : {source.volumeId},
                                   createdAt : Timestamps,
                                   metadata  : Metadata \union {None}]}

GetSnapshotOutcomes(state, args) ==
  LET snapshot == Select(state.snapshots, LAMBDA s : args.id = s.id)

  IN {Err(state, e) : e \in ProviderErrs} \union
     CASE snapshot = None -> {Err(state, "NotFound")}
       [] OTHER           -> {Ok(state, SnapshotView(snapshot))}

ListSnapshotsOutcomes(state, args) ==
  LET Errs ==
        LET raised == [UnsupportedCapability |-> ~Capabilities.snapshotEnumeration]
        IN {e \in DOMAIN raised : raised[e]}

  IN {Err(state, e) : e \in ProviderErrs} \union
     CASE Errs /= {} -> {Err(state, e) : e \in Errs}
       [] OTHER      -> {Ok(state, [results   |-> {[id |-> s.id, volumeId |-> s.volumeId, metadata |-> s.metadata]
                                                      : s \in {s \in state.snapshots : s.id \in r.page}},
                                    nextToken |-> r.nextToken])
                          : r \in Pages({s.id : s \in state.snapshots}, Tokens(SnapshotIds), args.startToken,
                                        args.maxResults)}

DeleteSnapshotOutcomes(state, args) ==
  LET snapshot == Select(state.snapshots, LAMBDA s : args.id = s.id)

  IN {Err(state, e) : e \in ProviderErrs} \union
     \* A snapshot that does not exist is not refused, so that retries succeed.
     CASE snapshot = None -> {Ok(state, None)}
       [] OTHER           -> {Ok([state EXCEPT !.snapshots = @ \ {snapshot}], None)}

Outcomes(state, call) ==
  CASE call.method = "GetCapabilities" -> GetCapabilitiesOutcomes(state, call.args)
    [] call.method = "CreateVolume"    -> CreateVolumeOutcomes(state, call.args)
    [] call.method = "GetVolume"       -> GetVolumeOutcomes(state, call.args)
    [] call.method = "ListVolumes"     -> ListVolumesOutcomes(state, call.args)
    [] call.method = "ExpandVolume"    -> ExpandVolumeOutcomes(state, call.args)
    [] call.method = "PublishVolume"   -> PublishVolumeOutcomes(state, call.args)
    [] call.method = "UnpublishVolume" -> UnpublishVolumeOutcomes(state, call.args)
    [] call.method = "DeleteVolume"    -> DeleteVolumeOutcomes(state, call.args)
    [] call.method = "CreateSnapshot"  -> CreateSnapshotOutcomes(state, call.args)
    [] call.method = "CopySnapshot"    -> CopySnapshotOutcomes(state, call.args)
    [] call.method = "GetSnapshot"     -> GetSnapshotOutcomes(state, call.args)
    [] call.method = "ListSnapshots"   -> ListSnapshotsOutcomes(state, call.args)
    [] call.method = "DeleteSnapshot"  -> DeleteSnapshotOutcomes(state, call.args)

====
