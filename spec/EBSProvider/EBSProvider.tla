------------------------------------------------- MODULE EBSProvider -------------------------------------------------
(*********************************************************************************************************************)
(* The contract that every EBSProvider implementation must meet, composed from every other module.                   *)
(*                                                                                                                   *)
(* Open questions:                                                                                                   *)
(* - What may a failed call change? A failed call leaves the state unchanged, but a timeout can arrive after         *)
(*   the provider acted, and a crash can leave work half done.                                                       *)
(* - Is the schema version part of the contract? Every method refuses an unsupported version with                    *)
(*   unsupported_version, and the conformance suite tests it, but the spec does not model versions.                  *)
(*********************************************************************************************************************)
EXTENDS Capabilities, Volumes

-----------------------------------------------------------------------------------------------------------------------
\* Specification

(*********************************************************************************************************************)
(* Every call is a single atomic step, so an implementation must be linearizable: concurrent calls must              *)
(* behave as if each ran alone, in an order consistent with when they were made and when they returned.              *)
(*********************************************************************************************************************)
Next ==
  \/ VolumesNext

Spec == Init /\ [][Next]_vars

=======================================================================================================================
