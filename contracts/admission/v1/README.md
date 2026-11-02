# Admission contract, version 1

This package owns the cross-process contract for the admission domain's
quota override. It is not an in-process service interface or the owner of
quota state.

## Seal account quota override

`SealAccountQuotaOverrideSubject` (`admission.v1.SealAccountQuotaOverride`) is
a request/reply route on which a gateway, in the `awsgw-quota-seal` queue
group, irreversibly seals one account's quota override record.
`domains/admission/quota` owns it: `awsgw` serves it.
`operator/accountlifecycle` is the consumer: it calls this subject to fence every
override writer at the quota owner before the account record is removed.

The request names the account in its body (`account_id`) and may carry an
opaque `correlation_ref` that is echoed for logs.
`awsgw` ignores `X-Account-ID` for this subject: the account sealed is the one
named in the body.
A seal is refused with `IncorrectState` unless IAM already holds the account
as `TERMINATING`; a precondition read failure, or no termination checker
wired, answers `InternalError`.
Both refusals write nothing and never open the override bucket.

A reply is sent only on success: `disposition` is always `sealed` and
`revision` is the stream leader's accepted write for that seal.
A failure is the standard awserrors error payload, not this reply shape.
Sealing is idempotent: a seal on an already-sealed record re-writes the same
value and returns a fresh, higher revision.
Any current gateway may answer; the leader-accepted revision is the evidence
whichever one does, and the override bucket is opened at seal time even on a
gateway whose quotas were disabled at startup.

## Seal-quota-override capability

`AccountQuotaOverrideSealCapabilitySubject(node)` (`admission.v1.AccountQuotaOverrideSealCapability.<node>`) is a per-node, read-only request/reply route on which `operator/accountlifecycle` asks one configured gateway whether it supports the quota override seal, immediately before every seal attempt.
The quota owner (`domains/admission/quota`) owns it: `awsgw` serves it with no queue group, so a request addressed to one node reaches only subscribers configured as that node, and two gateways sharing a node ID both answer.
The consumer's quota seal reaper probes every configured node that runs `awsgw` and sends no seal request unless every one answers compatible.

`AccountQuotaOverrideSealCapabilityVersion` is a claim about the whole binary's writers, not about the responder: version 1 means every quota override writer in the binary is revision-guarded and refuses a sealed record, and the binary serves the seal subject.
A gateway binary without this responder has no subscriber, so the probe for that node reads as incomplete (`no-reply`), the same as a stopped or partitioned node.
The reply proves nothing beyond the moment it was sent: a gateway downgraded or replaced after a successful probe is outside the guarantee.

### Compatibility boundary

The subject name carries the contract major; `account_quota_seal_test.go` and `account_quota_seal_capability_test.go`
preserve the version-1 JSON shapes and the subject builder.
Producer and consumer are in the coordinated Spinifex release, so the window
is N and N-1.
A gateway that predates the subject has no subscription, so the request gets
no responder.
An incompatible change to the subject or fields needs a new major.
