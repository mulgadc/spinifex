# Account-lifecycle participant contract, version 1

This package owns the cross-process contract between the account-termination
engine in `operator/accountlifecycle` and each participant domain.

## Routes

- `accountlifecycle.v1.participant.<name>.inventory`: read-only. The reply
  reports, for every declared class, a remaining count and a status of
  `clean`, `present` or `incomplete`, read from the owner's authoritative
  stores. A store error, partial scan or unreadable record is `incomplete`,
  never `clean`.
- `accountlifecycle.v1.participant.<name>.terminate`: one pass of the
  participant's termination command. It seals the participant's admission for
  the account, starts the owner drain and returns. It never waits for the
  drain; the engine repeats the command and the inventory until every class
  is `clean` or the participant's drain budget ends.

Each participant serves both routes in one queue group. The request names the
account in its body. `CorrelationRef` is opaque and grants nothing.
`OperationID` and `Fence` are empty in the interim engine.

## Declaration

A participant publishes a `Declaration` in its own `contracts/<domain>/v1`
package: name, declaration version, owned classes, the contract versions it
serves and its drain budget. The engine imports the declaration from that
contract package, never from the domain.

## Compatibility boundary

Subjects, JSON field names, statuses and reason strings are wire
compatibility. Replies carry no resource names, tags or key material.
`participant_test.go` pins the version-1 shapes.
