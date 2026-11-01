------------------------------------------------- MODULE EBSProvider --------------------------------------------------
(*********************************************************************************************************************)
(* The state that an implementation exposes, and the types and constants that every other module shares.             *)
(*********************************************************************************************************************)
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
  VolumeIDs, VolumeHandles,
  NodeIDs, NBDURIs, DevicePaths,
  SnapshotIDs, SnapshotHandles,
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
ASSUME VolumeIDs /= {} /\ VolumeHandles /= {}
ASSUME NodeIDs /= {} => NBDURIs /= {} \/ DevicePaths /= {}
ASSUME SnapshotIDs /= {} => SnapshotHandles /= {} /\ Timestamps /= {}
ASSUME ListTokens /= {}
ASSUME MaxListResults \in Nat \ {0}
ASSUME None \notin UNION {VolumeIDs, VolumeHandles, NodeIDs, SnapshotIDs, SnapshotHandles,
                          AvailabilityZones, Timestamps, Parameters, ProviderData, ListTokens}
ASSUME Capabilities \in [online_expansion          : BOOLEAN,
                         sparse_extent_reporting   : BOOLEAN,
                         crash_consistent_snapshot : BOOLEAN,
                         volume_seeding            : BOOLEAN,
                         read_only_publish         : BOOLEAN,
                         volume_enumeration        : BOOLEAN,
                         snapshot_enumeration      : BOOLEAN,
                         owner_routing             : BOOLEAN,
                         exclusion                 : [scope             : {"none", "node", "cluster"},
                                                      claim_ttl_seconds : Nat,
                                                      fences_lost_claim : BOOLEAN]]
ASSUME ProviderMayFail \in BOOLEAN

-----------------------------------------------------------------------------------------------------------------------
\* Variables

VARIABLES
  \* Every volume that the implementation exposes, keyed by volume ID.
  volumes,
  \* Every publication that the implementation exposes, keyed by the volume and the node it is published to.
  publications,
  \* Every snapshot that the implementation exposes, keyed by snapshot ID.
  snapshots

vars == <<volumes, publications, snapshots>>

-----------------------------------------------------------------------------------------------------------------------
\* Miscellaneous

IsPartialFn(f, D, R) == DOMAIN f \subseteq D /\ \A x \in DOMAIN f : f[x] \in R

Optional(S) == S \union {None}

InsertKey(f, x, y) == f @@ (x :> y)

RemoveKey(f, x) == RestrictDomain(f, LAMBDA x_2 : x_2 /= x)

KeyExists(f, x) == x \in DOMAIN f

-----------------------------------------------------------------------------------------------------------------------
\* Types

Bytes == 0..MaxBytes

\* The errors that an implementation may return when its provider fails to answer.
ProviderErrors == IF ProviderMayFail
                  THEN {"internal", "unavailable"}
                  \* Otherwise an implementation could conform by refusing to answer.
                  ELSE {}

CapacityRanges == [required_bytes : Bytes, limit_bytes : Bytes]

VolumeStates == {"available", "in-use"}

Volumes == [capacity_bytes     : 1..MaxBytes,
            state              : VolumeStates,
            handle             : VolumeHandles,
            availability_zone  : Optional(AvailabilityZones),

            \* Never returned, but kept to tell a retry from a conflicting request.
            source_snapshot_id : Optional(SnapshotIDs),
            parameters         : Optional(Parameters),
            provider_data      : Optional(ProviderData)]

\* How a published volume is reached on its node, either over NBD or as a host block device.
Exports == [kind    : {"nbd"},
            nbd_uri : NBDURIs]
           \union
           [kind        : {"block"},
            device_path : DevicePaths,
            size_bytes  : 1..MaxBytes]

\* A volume published to a node, and whether it was published read-only.
Publications == [export    : Exports,
                 read_only : BOOLEAN]

SnapshotStates == {"pending", "completed", "error"}

Snapshots == [size_bytes       : 1..MaxBytes,
              created_at       : Timestamps,
              state            : SnapshotStates,
              handle           : SnapshotHandles,
              source_volume_id : VolumeIDs]

-----------------------------------------------------------------------------------------------------------------------
\* State helpers

\* The part of a volume that the implementation returns.
VolumeView(volID) ==
  ("id" :> volID) @@ Restrict(volumes[volID], {"capacity_bytes",
                                               "state",
                                               "handle",
                                               "availability_zone",
                                               "provider_data"})

\* The nodes that a volume is published to.
PublishedNodes(volID) == {n \in NodeIDs : KeyExists(publications, <<volID, n>>)}

\* The part of a publication that the implementation returns.
PublicationView(volID, nodeID) == [volume_id |-> volID,
                                   node_id   |-> nodeID,
                                   export    |-> publications[volID, nodeID].export]

\* The part of a snapshot that the implementation returns.
SnapshotView(snapID) == ("id" :> snapID) @@ snapshots[snapID]

\* The number of refs that a page may hold, with an omitted or oversized request clamped to the maximum.
PageSize(m) == IF m <= 0 \/ m > MaxListResults THEN MaxListResults ELSE m

-----------------------------------------------------------------------------------------------------------------------
\* Invariants

TypeOK ==
  /\ IsPartialFn(volumes, VolumeIDs, Volumes)
  /\ IsPartialFn(publications, VolumeIDs \X NodeIDs, Publications)
  /\ IsPartialFn(snapshots, SnapshotIDs, Snapshots)

-----------------------------------------------------------------------------------------------------------------------
\* Initial state

Init ==
  /\ volumes      = <<>>
  /\ publications = <<>>
  /\ snapshots    = <<>>

=======================================================================================================================
