------------------------------------------------- MODULE Capabilities -------------------------------------------------
(*********************************************************************************************************************)
(* The optional behaviour that a provider advertises.                                                                *)
(*********************************************************************************************************************)
EXTENDS Base

-----------------------------------------------------------------------------------------------------------------------
\* Types

GetCapabilitiesRequests == {<<>>}

-----------------------------------------------------------------------------------------------------------------------
\* Interface methods

GetCapabilities(req, val, err) ==
  (*******************************************************************************************************************)
  (* The provider's capabilities were requested.                                                                     *)
  (*                                                                                                                 *)
  (* Open questions:                                                                                                 *)
  (* - Is it a lasting promise that capabilities never change, even when a provider is upgraded or                   *)
  (*   reconfigured after an earlier answer was cached?                                                              *)
  (*******************************************************************************************************************)
     \* The provider failed to answer.
  \/ /\ val = None
     /\ err \in FaultErrors
     /\ UNCHANGED vars

     \* The request was accepted. Every answer is the same, so an earlier answer may be cached.
  \/ /\ val = Capabilities
     /\ err = None
     /\ UNCHANGED vars

=======================================================================================================================
