----------------------------------------------------- MODULE Base -----------------------------------------------------
(*********************************************************************************************************************)
(* The state that a provider holds, and the types and constants that every other module shares.                      *)
(*                                                                                                                   *)
(* Open questions:                                                                                                   *)
(* - Are internal and unavailable the only errors a provider may return for reasons unrelated to the                 *)
(*   request?                                                                                                        *)
(* - Must a present but malformed ID be refused with invalid_argument? IDs are opaque here, so only                  *)
(*   an omitted one is refused.                                                                                      *)
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
  SnapshotIDs, SnapshotHandles,
  AvailabilityZones, Timestamps,
  Parameters, ProviderData,
  \* The optional behaviour that the provider advertises. It describes the provider rather than its
  \* state, so it never changes.
  Capabilities,
  \* Whether faults were injected into the provider, so that it may fail to answer.
  FaultsInjected,
  \* Stands in for an optional field that a request or response omitted.
  None

ASSUME MaxBytes \in Nat \ {0}
ASSUME VolumeIDs /= {} /\ VolumeHandles /= {}
ASSUME SnapshotIDs /= {} => SnapshotHandles /= {} /\ Timestamps /= {}
ASSUME None \notin UNION {VolumeIDs, VolumeHandles, SnapshotIDs, SnapshotHandles,
                          AvailabilityZones, Timestamps, Parameters, ProviderData}
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
ASSUME FaultsInjected \in BOOLEAN

-----------------------------------------------------------------------------------------------------------------------
\* Variables

VARIABLES
  \* Every volume that the provider holds, keyed by volume ID.
  volumes,
  \* Every snapshot that the provider holds, keyed by snapshot ID.
  snapshots

vars == <<volumes, snapshots>>

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

\* The errors that a provider may return for reasons unrelated to the request. Without injected faults
\* there are none, so a provider cannot conform by refusing to answer.
FaultErrors == IF FaultsInjected THEN {"internal", "unavailable"} ELSE {}

CapacityRanges == [required_bytes : Bytes, limit_bytes : Bytes]

VolumeStates == {"available", "in-use"}

Volumes == [capacity_bytes     : 1..MaxBytes,
            state              : VolumeStates,
            handle             : VolumeHandles,
            availability_zone  : Optional(AvailabilityZones),
            \* The source snapshot and parameters are never returned, but are kept so that a retried
            \* request can be told apart from a conflicting one.
            source_snapshot_id : Optional(SnapshotIDs),
            parameters         : Optional(Parameters),
            provider_data      : Optional(ProviderData)]

\* The part of a volume that the provider returns.
VolumeView(volID, v) ==
  ("id" :> volID) @@ Restrict(v, {"capacity_bytes",
                                  "state",
                                  "handle",
                                  "availability_zone",
                                  "provider_data"})

SnapshotStates == {"pending", "completed", "error"}

Snapshots == [size_bytes       : 1..MaxBytes,
              created_at       : Timestamps,
              state            : SnapshotStates,
              handle           : SnapshotHandles,
              source_volume_id : VolumeIDs]

\* The part of a snapshot that the provider returns.
SnapshotView(snapID, s) == ("id" :> snapID) @@ s

-----------------------------------------------------------------------------------------------------------------------
\* State helpers

-----------------------------------------------------------------------------------------------------------------------
\* Invariants

TypeOK ==
  /\ IsPartialFn(volumes, VolumeIDs, Volumes)
  /\ IsPartialFn(snapshots, SnapshotIDs, Snapshots)

-----------------------------------------------------------------------------------------------------------------------
\* Initial state

Init ==
  /\ volumes   = <<>>
  /\ snapshots = <<>>

=======================================================================================================================
