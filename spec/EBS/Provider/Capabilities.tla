------------------------------------------------- MODULE Capabilities -------------------------------------------------
EXTENDS EBSProvider

-----------------------------------------------------------------------------------------------------------------------
\* Interface methods

GetCapabilities(req) ==
  (*******************************************************************************************************************)
  (* The implementation reports which optional behaviour it supports.                                                *)
  (*******************************************************************************************************************)
     \* The provider failed to answer.
  \/ /\ val' = None
     /\ err' \in ProviderErrors
     /\ UNCHANGED state

     \* The operation was successful.
     \* Every answer is the same, so an earlier one may be cached.
  \/ /\ val' = Capabilities
     /\ err' = None
     /\ UNCHANGED state

=======================================================================================================================
