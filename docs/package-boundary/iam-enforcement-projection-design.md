# IAM enforcement projection: v1 contract design

**Status:** Draft for review; contract design only, nothing implemented.

This is the minimum versioned Bluebottle IAM enforcement projection required by ADR-0005 S1 and S6(2), designed from the consumer inventory in `iam-enforcement-inventory.md` (cited below as S*n*, P*n* and G*n* rows).
ADR-0005 and ADR-0006 are accepted and are cited as decisions; nothing here changes either.
Clauses are numbered P1, P2… and are appended, never renumbered.
Decisions are D1 to D12, each recorded once in the decision register (Section 14); a D-number anywhere else in either document means that register entry and nothing more.
**Phase 1** means the first step of the code stack in Section 13; Phase 0 is the inventory and the AWSGW characterisation tests in `gateway/auth_current_behaviour_test.go`.
Spinifex paths are relative to `spinifex/spinifex/`; other repos are prefixed as in the inventory.

## 1. Purpose and standing

P1. The projection is the set of signed, versioned facts a local enforcement point needs to verify a SigV4 credential and authorize a request without reading IAM persistence (ADR-0005 S1 "first required shared identity boundary").

P2. Its semantic owner is the Spinifex IAM authority (today `handlers/iam` with `handlers/sts` for sessions; `domains/iam` once extracted).
Bluebottle owns only its vocabulary, evaluation semantics and conformance fixtures (Section 10).

P3. "Minimum" means every fact below has a named consumer in the matrix (Section 11), and no fact is included for a consumer that does not need it.

## 2. Non-goals

| Non-goal | Consequence |
|---|---|
| Moving `handlers/iam`, `handlers/sts` or IAM records | Records, keys, buckets and migrations stay where they are; the projection is produced from them. |
| A broad `IAMService` replacement | Consumers get the projection or a narrow verification capability, never a wider service (ADR-0005 S6 last paragraph). |
| Raw-KV, global-singleton or permissive fallback | No consumer may fall back to reading `spinifex-iam-*`, to a process-global credential table, or to allow-on-error while the projection is absent or stale. |
| Predastore migration or production auth change | This document changes no code; Predastore's current flow (inventory 4.3) stays until its migration PR (Section 13). |
| ECS/EKS `iam:PassRole` migration | ADR-0005 S3 and S6(6) work; role resolution is not a projection fact (P9). |
| Transport, storage and key-custody design | Stated as responsibilities only (Section 10). |
| EKS OIDC discovery | ADR-0005 S4, planned in `sts-oidc-discovery-projection-plan.md`. |

## 3. Facts

P4. Every fact carries a header: `contract` (`"mulga.iam.enforcement"`), `major` (`1`), `kind`, `key`, `generation` (per-key, strictly increasing), `rev` (authority-wide sequence at which it was emitted), `issuedAt` (authority clock), `critical` (list of required extension names, normally empty) and `sig` (Section 4).

P5. **AccountFact**, key `accountID`.
Fields: `status` (`ACTIVE`, `SUSPENDED`, `TERMINATING`).
Consumers: AWSGW (S13, S17) and Predastore (D7); Goanna as a deferred future consumer (D6, D7).

P6. **PrincipalFact**, key `(accountID, principalID)` where `principalID` is the immutable IAM unique ID (`AIDA…` user, `AROA…` role).
Fields: `type` (`user`, `role`), `arn` (stored ARN including path), `name`, `status` (`present`, `deleted`), `policyGeneration`.
A role-session principal is not a PrincipalFact; it is described by its CredentialFact and refers to its role's PrincipalFact.
Consumers: AWSGW (S8, S14), Predastore (P4, P5), STS presigned verify (S9).

P7. **CredentialFact**, key `accessKeyID`.
Fields: `kind` (`long-term`, `session`), `accountID`, `principalID` (the user or role it was issued for), `status` (`Active`, `Inactive`, `Deleted`), `issuedAt`, `expiresAt` (sessions only), `sessionName` and `assumedRoleArn` (role sessions only, for `aws:userid` and caller ARN), `sessionTokenDigest` (sessions only, P14), `verifier` (Section 4.2), and optional `sourceCredentialID` (GetSessionToken sessions only: the long-term AKID that minted the session).
`sourceCredentialID` is informational in major 1: no consumer enforces it, and it does not change revocation behaviour until D11 is decided.
Consumers: AWSGW (S11, S12, S17), Predastore (P3, P4), STS (S9, S10); Goanna (G3, G4) as a deferred future consumer (D6).

P8. **PolicySetFact**, key `(accountID, principalID)`.
Fields: `policyGeneration` and `documents`, the ordered list of `iampolicy.PolicyDocument` that IAM resolves for that principal today (user inline, attached managed default versions, group inline and group managed; or role inline and attached managed), exactly the set `GetUserPolicies` and `GetRolePolicies` return (`handlers/iam/service_impl.go:1997`, `handlers/iam/roles.go:579`).
Resolution, including the missing-group and unmodelled-AWS-managed-policy rules, happens once at the authority; consumers never resolve groups or managed ARNs (ADR-0005 S1 requires Predastore to stop reimplementing group resolution).
A principal whose policies cannot be resolved is published with `documents` absent and `unresolvable: true`, and every consumer treats it as Malformed (P24), never as an empty grant.
Consumers: AWSGW (S15), Predastore (P5, P6); Goanna once it adopts the projection, because it must then enforce IAM identity policy (D6).

P9. Excluded facts and why:

| Excluded | Reason |
|---|---|
| Role trust policies, `MaxSessionDuration` | Only STS issuance (S5, S6) needs them, and STS sits with the authority; a role resolver capability serves it (ADR-0005 S2). |
| Instance profiles | Served to vpcd by the existing RPC (S3, S21); not an enforcement input. |
| OIDC provider records | STS-only (S4); a narrow IAM capability, not a broadcast fact. |
| Tags, paths beyond the ARN, creation times, policy versions other than default | No enforcement consumer reads them. |
| Group facts | Folded into PolicySetFact (P8). |

## 4. Integrity and secret handling

### 4.1 Authenticity

P10. Every fact is signed by the IAM authority with an Ed25519 signature (D2) over its canonical encoding, which includes every header field in P4.
Consumers hold only the authority's public verification keys, identified by `keyID`, from their own configured trust bundle.
A fact whose signature does not verify, whose `keyID` is unknown, or whose canonical encoding does not round-trip is Malformed (P24).

P11. The signature, not transport access, is the trust root.
Today any holder of the shared NATS token can write IAM buckets (inventory 6); a signed fact cannot be forged by a consumer that can write the transport.

P12. Replay: a consumer accepts a fact for a key only if its `generation` is greater than the one it holds, so an old Active fact cannot be replayed over a later Inactive or Deleted one.
Freshness (Section 5) bounds replay of a whole stale view.

### 4.2 Credential verification material

P13. SigV4 is a symmetric HMAC scheme, so verifying it needs key material derived from the secret.
The projection never carries the long-term secret or the session secret, in plaintext or under the master key.
Under D1 (Accepted in principle), the `verifier` carries SigV4 derived signing keys, `kSigning = HMAC chain(secret, date, region, service)`, one per (scope date, region, service) the consumer is entitled to, each encrypted to that consumer's own public key (audience-bound), and covered by the fact signature.
A derived signing key is a sensitive signing capability, not a public verifier: its holder can sign any request within its date, Region and service scope.
A Predastore view receives only `s3` keys for its Region (and a future Goanna view only `monitoring` keys), so a compromised consumer can forge requests only for its own service, Region and the dates it holds.
Recipient-key rotation and scope issuance need implementation review before this material is used (D1 gate).
Dates published: the current and next UTC date for header-signed requests; for presigned URLs, every date back to the consumer's maximum presign age (`bluebottle:pkg/sigv4/sigv4.go:23` sets 7 days) and never earlier than the credential's `issuedAt`.
Bluebottle's `sigv4` needs a verify-by-signing-key entry point for this; today `Verify` takes only the plaintext secret (`bluebottle:pkg/sigv4/verify.go:24`).

P14. Session tokens: `sessionTokenDigest` is SHA-256 of the wire token.
Tokens are 32 random bytes (`handlers/sts/assume_role.go:68`), so an unkeyed digest cannot be inverted or brute-forced, and consumers need no shared HMAC key.
The authority keeps its existing master-key HMAC in its private record; the digest is an additional projection field.

P15. Must never appear in the projection: long-term or session secrets in any encoding; the master key or any key-encryption key; master-key ciphertexts (`secret_access_key`, `secret_encrypted`); session-token plaintext or the master-key HMAC; the authority's signing private key; IAM bucket names, KV keys or persistence record encodings; trust policies; bootstrap credentials.

## 5. Freshness

P16. The authority emits a signed **Watermark** fact at a fixed interval H = 5 s (D3) carrying `rev` (the highest `rev` emitted) and `issuedAt`.

P17. A consumer's view is **current** when it has applied every fact up to the last watermark's `rev`, that watermark was received within S of the consumer's monotonic clock, and its `issuedAt` is within S plus the declared maximum clock skew of the consumer's wall clock.
S is the staleness bound: 30 s normally, set per consumer profile, with a hard maximum of 60 s (D3; 60 s is today's Predastore AKIA cache TTL, inventory P3).

P18. A view is built from a signed **Snapshot** (all live facts at a `rev`, terminated by a watermark at that `rev`) followed by incremental facts.
Absence of a key in a current view means NotFound only if the view was built from a complete snapshot; a consumer that has not completed its first snapshot is Unavailable (P24).

P19. When stale, a consumer refuses every request that depends on the projection (P26), and never extends a cached answer past S.

## 6. Revocation

P20. Access-key deactivation or deletion, principal deletion, account suspension and session revocation each publish a new fact generation (`Inactive`, `Deleted`, `deleted`, `SUSPENDED`) before the authority's API call returns success.
Effect at every current consumer is bounded by S plus one watermark interval H.

P21. Session revocation by principal deletion (`RevokeUserSessions`, `RevokeRoleSessions`, `handlers/sts/session_credentials.go:201,212`) publishes `Deleted` CredentialFacts for every affected session, and the principal's own `deleted` PrincipalFact independently invalidates any session the sweep misses (P23 check 3).
This clause describes how today's delete-the-record mechanism would be projected; whether that mechanism stays or is replaced by an AWS-style deny on token issue time is D12 (Open), and who publishes session-validity facts is D9 (Open).
A GetSessionToken session today outlives deletion of its source access key (`TestCurrentBehaviour_Session_GetSessionTokenOutlivesMintingKey`); whether it should is D11 (Open).

P22. Deleted-key and deleted-session facts are retained as tombstones until the next snapshot; a consumer that misses a tombstone recovers by resynchronising to a snapshot in which the key is absent.

## 7. Replacement and identity reuse

P23. All facts bind to immutable IDs, never names.
A consumer accepts a credential only if:

1. the CredentialFact is `Active` and, for sessions, unexpired and its token digest matches;
2. its `accountID` has an `ACTIVE` AccountFact;
3. its `principalID` has a `present` PrincipalFact in the same account; and
4. it evaluates the PolicySetFact keyed by that same `principalID`.

A user deleted and recreated under the same name receives a new `principalID`, so the old key or session fails check 3 and can never reach the new user's PolicySetFact.
AKIDs are never reused by the authority.

Current-code evidence that this is a change, not a restatement:

- The long-term `AccessKey` record holds only `user_name`, not the user ID (`handlers/iam/types.go:68-75`), and every verifier resolves the user by name (`gateway/auth.go:375-405`, S9, S17, P3).
- `DeleteAccessKey` removes the key from the user record before deleting the access-key record (`handlers/iam/service_impl.go:757-770`), and `CreateAccessKey` writes the key before the user record (665-683); the two writes are not transactional.
  That an interruption between them leaves an `Active` access-key record the user no longer lists is INFERRED from the code; no failure-injection test interrupts either call.
- The resulting state is reproduced directly: `TestCurrentBehaviour_Revocation_OrphanedKeyAuthenticatesAsRecreatedUser` rewrites the user record so it no longer lists the key, and shows that the key still authenticates, that `DeleteUser` then succeeds because it checks only the user's list (585), and that after a same-name user is recreated the key authenticates as the new user (new `UserId`, same ARN).
- `TestCurrentBehaviour_Revocation_OrphanedKeyPermissionsAfterRecreation` shows the policy consequence through the real router and dispatcher policy check: after recreation, the orphaned key is allowed an action only the replacement user's policy grants (`iam:GetUser`) and denied the action only the original user's policy granted (`iam:ListUsers`).
  The orphaned key therefore receives the replacement user's permissions and loses the original user's.
- Session records already bind `RoleID`/`UserID` and AWSGW re-checks them (S8, pinned by `TestCurrentBehaviour_Revocation_UserDeletedAndRecreated` and `TestCurrentBehaviour_Revocation_RoleDeletedAndRecreated`); Predastore does not (P4), from code reading.

The producer PR therefore needs an expand migration (ADR-0006 S4) adding the user ID to every `AccessKey` record before it can publish P7's `principalID` for long-term keys.

## 8. Failure classification

P24. Each consumer classifies a lookup into exactly one class.
The AWSGW column is pinned by the tests named in inventory Section 7; the Predastore column is from code reading (inventory 4.3).
Goanna is not characterised in Phase 0 and has no column (D6).

| Class | Meaning | Fail-closed | AWSGW today | Predastore today | Target wire response |
|---|---|---|---|---|---|
| NotFound | Current, complete view has no fact for the AKID | yes | 403 `InvalidClientTokenId` | 403 `InvalidAccessKeyId` | unchanged |
| Invalid | Key `Inactive`/`Deleted`; principal deleted or replaced; account not ACTIVE before signature; token digest mismatch or missing | yes | `InvalidClientTokenId` (account: `AccessDenied` after signature) | inactive: 500 `InternalError`; no token or principal check | same code as NotFound; account check stays after the signature |
| Expired | Session past `expiresAt` | yes | 400 `ExpiredToken`, before the session token and the signature are checked | 403 `InvalidAccessKeyId` | `ExpiredToken`, only after the token digest matches and the SigV4 proof verifies (D10, P25) |
| Denied | Policy evaluation or ownership denies | yes | `AccessDenied` | 403 `AccessDenied` | unchanged |
| Unavailable | No completed snapshot, or transport down | yes | 503 `ServiceUnavailable` (gateway NATS down) or `InternalError` | 500 `InternalError` | retryable 503 `ServiceUnavailable` (D5) |
| Stale | View older than S (P17) | yes | n/a | n/a (60 s cache) | as Unavailable (D3, D5) |
| Malformed | Bad signature, unknown `keyID`, unknown `major`, unknown `critical` entry, `unresolvable` policy set | yes, and alarms | `InternalError` | `ErrPrincipalConfig` → 403 `AccessDenied`, else 500 | 500 `InternalError`; never reported as NotFound |

P25. Ordering preserved from AWSGW (inventory 4.1): NotFound and Invalid share one code, and checks that depend on mutable principal or account state run after the signature.
Ordering changed from AWSGW (D10): Expired is reported only after both the session token digest matches and the SigV4 proof verifies, so a caller holding only an ASIA key ID no longer learns that an expired session existed.
AWS's own ordering of these checks is unverified, so D10 is a target chosen on its own merits, not an AWS-parity claim.
Policy facts are evaluated only after the signature verifies, so an unauthenticated caller cannot drive policy evaluation (Predastore today resolves policies before the signature, inventory 4.3).

P26. No class other than a verified Allow lets a request proceed, and no consumer may answer from a fact older than S.

## 9. Compatibility (ADR-0006)

P27. `major` is explicit on every fact; a receiver rejects an unknown major as Malformed and changes no state (ADR-0006 S3).
Additive optional fields stay on major 1 and are ignored by receivers that do not know them; a new field that changes enforcement meaning is listed in `critical`, and a receiver that does not know a listed name treats the fact as Malformed.

P28. AWSGW, STS and the IAM authority are one coordinated Regional release: N and N-1 interoperate in both rollout orders (ADR-0006 S2).

P29. Predastore, and Goanna once it adopts the projection (D6), are independently released: each declares the projection majors it supports in its release notes, and the authority publishes the oldest major any registered consumer has proved, through an authenticated capability declaration, not a version-like field or timing (ADR-0006 S3).
An AWSGW restart or upgrade does not require a Predastore restart while their declared major is still published (ADR-0006 S2 example).

P30. An incompatible change is a successor contract `major: 2` published alongside major 1 until every consumer has proved major 2; major 1 retires through ADR-0006 S5 finalisation with operator notice.

## 10. Ownership boundary

| Concern | Owner | Not owned by |
|---|---|---|
| Fact types, canonical encoding, signature verification, freshness predicate, failure classes, `iampolicy` evaluation semantics, verify-by-signing-key, conformance fixtures | Bluebottle (new package beside `pkg/iampolicy`) | — |
| Producing facts, signing key custody and rotation, snapshot and watermark emission, revocation publication, the access-key ID migration | Spinifex IAM authority | Bluebottle |
| Transport (stream or KV layout, subjects, NATS permissions), view storage and caching, per-consumer encryption keys | The IAM authority (publish side) and each consumer (subscribe side) | Bluebottle |
| Request-specific authorization (S3 ownership, service conditions) | Each consumer | Bluebottle, IAM |

P31. Bluebottle does not import Spinifex, hold deployment state, open NATS, name a bucket or subject, hold a credential or run a service.
Fixture signing uses a test-only key generated in the fixtures; production signing lives only in the authority.

P32. Transport must give consumers read-only access to the projection channel only, separate from the IAM private buckets; how is the authority's design, constrained by P11.

## 11. Consumer matrix

| Consumer | Needs from projection | Currently reads directly | Must stop reading | Failure behaviour required | Migration blockers |
|---|---|---|---|---|---|
| AWSGW SigV4 ingress | Account, Principal, Credential, PolicySet, unless D4 (Open) permits an in-process synchronous path while co-located with the authority | `gateway/auth.go:375-520`, `gateway/auth_accountstatus.go:67`, `gateway/gateway.go:771,809` via `IAMService`/`STSService` | `LookupAccessKey`, `DecryptSecret`, `LookupSessionCredential`, `GetUserPolicies`, `GetRolePolicies`, `GetAccount` on the auth path | P24 table; NATS-down 503 preserved | D4 decided; AKID user-ID migration (P23); the audit and context-key boundary still in `gateway` (`PACKAGE_BOUNDARY_MIGRATION.md`, ingress pipeline debt) |
| AWSGW ECR registry path | Account, Principal, Credential | `gateway/ecr_principal.go:97-250` | Same reads as above | 401 Bearer for Invalid, 503 for Unavailable or Stale (present split, `classifyIAMLookupErr` 67) | Same as AWSGW; not covered by the Phase 0 tests |
| STS presigned verify and `GetAccessKeyInfo` | Credential (verifier for `sts`), Principal | `handlers/sts/presigned_url_verify.go:150-200`, `get_access_key_info.go:73` | Raw `DecryptSecret(…, s.masterKey)` and `LookupAccessKey` | `InvalidIdentityToken`/`ExpiredToken` as today; Unavailable is an error, never a token verdict | STS extraction (ADR-0005 S6(4)); whether STS or the IAM authority publishes session CredentialFacts is D9 (Open) |
| Predastore | Account (D7), Principal, Credential (verifier for `s3`), PolicySet | `predastore:internal/gate/auth/auth.go:84-998` (P1-P5) | Every `spinifex-iam-*` bucket, `master.key` for IAM, duplicated record structs, group resolution | P24 table; inactive key → 403 not 500; token digest and principal checks added; policy only after signature | Independently released (P29); config `[[auth]]` keys (P7) may not override IAM or bypass ownership or policy (D8), so any bootstrap exception needs a separately designed, bounded bootstrap principal; Predastore characterisation tests |
| Goanna (future, deferred: D6) | Credential (verifier for `monitoring`), Principal, Account (D7), PolicySet | `goanna:internal/auth/provider.go:33-260` (G1-G4), code reading only | Both IAM buckets and `master.key` | P24 table, plus IAM identity-policy enforcement it does not have today | Deferred (D6); not characterised in Phase 0; before production use it must adopt this projection and enforce IAM identity policy, not keep its account-only authentication |
| Daemon system-role ensurer | None: it writes IAM, so it needs an IAM lifecycle capability, not the projection | `daemon/eks_deps.go:27-48`, `daemon/ecs_deps.go:39-51` | Second `IAMServiceImpl` and `master.key` | Fail closed instead of static-credential fallback (ADR-0005 S5, Q-25) | ADR-0005 S6(3) capability extraction |
| vpcd guest metadata | None: role resolution over the existing RPC | `domains/ec2/guestmetadata/nats_clients.go:42-95` | `handlers_iam.InstanceProfile` type over NATS | unchanged | ADR-0005 S6(3) |

## 12. AWS parity

Each AWS claim cites an official AWS documentation page or is marked unverified.
Citations:

- [AWS1] *Troubleshoot IAM*, section "Changes that I make are not always immediately visible", https://docs.aws.amazon.com/IAM/latest/UserGuide/troubleshoot_general.html#troubleshoot_general_eventual-consistency
- [AWS2] *GetAccessKeyInfo*, AWS STS API Reference, https://docs.aws.amazon.com/STS/latest/APIReference/API_GetAccessKeyInfo.html
- [AWS3] *Common Error Types*, AWS STS API Reference, https://docs.aws.amazon.com/STS/latest/APIReference/CommonErrors.html
- [AWS4] *IAM identifiers*, section "Unique identifiers", https://docs.aws.amazon.com/IAM/latest/UserGuide/reference_identifiers.html#identifiers-unique-ids
- [AWS5] *Revoke IAM role temporary security credentials*, https://docs.aws.amazon.com/IAM/latest/UserGuide/id_roles_use_revoke-sessions.html
- [AWS6] *IAM policy elements: Variables and tags*, table "Principal key values", https://docs.aws.amazon.com/IAM/latest/UserGuide/reference_policies_variables.html#principaltable

| Behaviour | What AWS does | This design | Status of the AWS claim |
|---|---|---|---|
| Propagation of IAM changes | IAM is eventually consistent; changes take time to become visible from all endpoints, and caching can add time [AWS1] | Bounded by S + H (P20); deliberately stricter | Documented [AWS1]; that page states no numeric bound |
| Access-key deactivation or deletion | Unverified | Bounded by S + H (P20) | Unverified |
| Unknown key vs wrong signature | Wire codes unverified ([AWS3] lists neither `InvalidClientTokenId` nor `SignatureDoesNotMatch`); `GetAccessKeyInfo` returns the account for an access key ID and does not indicate whether the key is active, inactive or deleted [AWS2] | Preserved (P25) | `GetAccessKeyInfo` documented [AWS2]; error codes unverified |
| Inactive vs unknown key | Unverified | Same code (P24) | Unverified |
| Expired session | [AWS3] lists `ExpiredTokenException` with HTTP 403; the Query-protocol code, its status and its ordering against token and signature checks are unverified | `ExpiredToken` only after token and SigV4 proof (D10, P25) | Partly documented [AWS3]; ordering unverified |
| Role session after the role's policies change | Unverified | PolicySetFact is keyed by role `principalID` and updated in place | Unverified |
| User deleted and recreated with the same name | A recreated user receives a different unique ID from the deleted one [AWS4] | Credentials and sessions of the old principal fail (P23) | Unique-ID rule documented [AWS4]; behaviour of an old key or session against a recreated user or role is unverified |
| Revoking active role sessions | IAM attaches an inline deny policy (`AWSRevokeOlderSessions`) to the role, conditioned on `aws:TokenIssueTime` [AWS5] | Undecided: today sessions are deleted (P21); D12 is Open | Documented [AWS5] |
| GetSessionToken session after its source key is deleted | Unverified | `sourceCredentialID` recorded, not enforced (P7, D11) | Unverified |
| AKIA credentials sent with `X-Amz-Security-Token` | Unverified | Not addressed; current behaviour recorded in inventory Section 7 | Unverified |
| `aws:userid` value | Account root user: the account ID; IAM user: the user's unique ID [AWS6] | PrincipalFact carries the immutable `principalID` (P6) | Documented [AWS6] |
| Suspended or closed account | Unverified | AccountFact enforced by every production consumer (D7) | Unverified |
| CloudWatch authorization | Unverified | Goanna must enforce identity policy before production use (D6) | Unverified |

## 13. Later code stack

Each step is its own PR, in order; none starts before ADR-0005 S6(1) characterisation tests for its consumer have landed.
Phase 1 begins at step 1, and it may not begin while D4 or D9 is Open.

| # | Change | Entry criteria | Exit criteria |
|---|---|---|---|
| 1 | Bluebottle: projection v1 types, canonical encoding, Ed25519 verify, freshness predicate, failure classes, `sigv4` verify-by-signing-key, conformance fixtures (valid, tampered, wrong key, unknown major, unknown critical, stale, replayed generation, replaced principal, expired, missing token, unresolvable policy set) | This design accepted; D4 and D9 decided | Fixtures pass in Bluebottle; AWSGW and Predastore test suites both run the fixtures without importing Spinifex (ADR-0005 evidence 2); no Spinifex import in Bluebottle |
| 2 | Spinifex IAM authority: add user ID to `AccessKey` (expand, migrate per ADR-0006 S4); produce, sign and publish snapshot, incremental facts and watermarks; signing key custody; publication on every revocation path | PR 1 released; transport design reviewed; D1 gate met (recipient-key rotation and scope issuance reviewed); D12 decided before any change to session revocation | For the characterisation corpus, the projection's decision equals `IAMService`'s on every case; revocation visible within S + H in a test; no consumer changed |
| 3a | AWSGW ingress and ECR path consume the projection, or the in-process path if D4 permits it | PR 2 deployed; AWSGW characterisation green | No `LookupAccessKey`, `DecryptSecret`, session lookup or policy resolution on the auth path; ADR-0005 evidence 3 tests pass |
| 3b | STS presigned verify and `GetAccessKeyInfo` consume the projection | 3a merged | No raw master-key decrypt in `presigned_url_verify.go` |
| 3c | Predastore consumes the projection | PR 2 published at a major Predastore declares; Predastore characterisation green; D8 bootstrap principal designed if any exception is kept | No `spinifex-iam-*` read, IAM struct copy or IAM `master.key` load; no `[[auth]]` override of IAM; ADR-0005 evidence 7 tests pass; compatibility matrix per P29 |
| 3d | Goanna consumes the projection | D6 deferral lifted; Goanna characterisation tests written and green | No IAM bucket or IAM `master.key` access; IAM identity policy enforced on every action; account status enforced (D7) |

Removing the daemon's second `IAMServiceImpl` (inventory S20) and the operator CLI's direct construction (S22) belongs to ADR-0005 S6(3) and S6(5) capability work, not this stack.

## 14. Decision register

Each entry stands alone.
Status values: Accepted, Accepted in principle (direction fixed, implementation gated), Rejected, Deferred, Open (not decided).
An implementation gate names what must exist or be reviewed before code that depends on the decision lands.

### D1: SigV4 verification without the long-lived secret

- **Question:** How does a consumer verify a SigV4 signature without holding the long-lived or session secret access key?
- **Status:** Accepted in principle.
- **Decision:** The CredentialFact `verifier` carries SigV4 derived signing keys, each scoped to one date, Region and service, and each encrypted to the receiving consumer's own public key (audience-encrypted).
  Derived signing keys are sensitive signing capabilities: a holder can sign any request within the key's date, Region and service scope, so they are protected and handled as secrets, never as public verification material.
- **Rationale:** SigV4 is a symmetric HMAC scheme, so verification needs key material derived from the secret; date, Region and service scoping limits a compromised consumer to forging requests for its own service, Region and held dates, and audience encryption stops one consumer reading another's keys.
- **Implementation gate:** Recipient-key rotation and scope issuance (which dates, Regions and services each consumer receives) need implementation review before any derived key is issued or used.
  Bluebottle `sigv4` needs a verify-by-signing-key entry point (`bluebottle:pkg/sigv4/verify.go:24` takes only the plaintext secret today).
- **Links:** P13, P15, P7; inventory 2.4 (`bluebottle:pkg/sigv4`), S11, S12, P3, P4.

### D2: Integrity of projection facts

- **Question:** How are projection facts protected against forgery and tampering by any party that can write the transport?
- **Status:** Accepted.
- **Decision:** Every fact is signed with Ed25519 by the IAM authority over its canonical encoding, including every header field; consumers hold only the authority's public verification keys from their own trust bundle.
- **Rationale:** Any holder of today's shared NATS token can write the IAM buckets (inventory Section 6), so transport access cannot be the trust root; an asymmetric signature means a consumer that can verify facts cannot forge them.
- **Implementation gate:** Signing-key custody and rotation at the authority, and the trust-bundle distribution to consumers, are reviewed before the producer (Section 13 step 2) publishes.
- **Links:** P4, P10, P11, P12, P31; inventory Section 6.

### D3: Freshness bounds

- **Question:** What heartbeat interval and staleness bounds define a current projection view?
- **Status:** Accepted.
- **Decision:** Heartbeat (watermark interval) H = 5 s; normal staleness bound S = 30 s; hard maximum S = 60 s, which no consumer profile may exceed.
- **Rationale:** Revocation then takes effect at every current consumer within S + H; 60 s matches today's longest IAM cache in a consumer (Predastore's AKIA cache), so no consumer becomes weaker than today.
- **Implementation gate:** The Bluebottle freshness predicate and its stale-view fixtures (Section 13 step 1) encode these values before any consumer relies on them.
- **Links:** P16, P17, P19, P20, P24 (Stale); inventory P3.

### D4: AWSGW in-process synchronous path

- **Question:** May AWSGW, co-located with the IAM authority, verify and authorize through an in-process synchronous capability instead of consuming the projection?
- **Status:** Open; not decided.
- **Context:** ADR-0005 S1 allows a synchronous verification capability only for a consumer profile that explicitly accepts its availability dependency; this design does not make that choice for AWSGW.
- **Implementation gate:** D4 must be explicitly decided before Phase 1 (Section 13 step 1) begins.
- **Links:** Section 11 (AWSGW rows), Section 13 step 3a; inventory S11-S15, 4.1.

### D5: Failure class when projection state is unavailable or stale

- **Question:** What does a consumer return when it has no completed snapshot, its transport is down, or its view is older than S?
- **Status:** Accepted.
- **Decision:** Fail closed with a retryable HTTP 503 `ServiceUnavailable`.
- **Rationale:** No request may proceed on missing or stale identity state, and a retryable 503 tells clients the failure is transient rather than a credential verdict; AWSGW already answers 503 when its NATS connection is down (`TestCurrentBehaviour_Auth_BackingStateUnavailable`).
- **Implementation gate:** Each consumer's characterisation tests pin its current unavailable-state outcome before it migrates, so the change to 503 is deliberate and visible.
- **Links:** P19, P24 (Unavailable, Stale), P26; inventory 4.1, 4.3.

### D6: Goanna

- **Question:** Is Goanna in scope for the Phase 0 characterisation and the first projection rollout?
- **Status:** Deferred.
- **Decision:** Goanna is not characterised in Phase 0 and is not part of the first rollout.
  Any future production Goanna must consume the projection and enforce IAM identity policy; it must not keep its current account-only authentication model.
- **Rationale:** Goanna reads the IAM buckets directly and evaluates no IAM policy today (inventory 2.3, 4.4, from code reading), so adopting the projection without adding policy enforcement would carry an authorization gap forward.
- **Implementation gate:** Before Goanna is used in production: Goanna characterisation tests, adoption of the projection, and IAM identity-policy enforcement (Section 13 step 3d).
- **Links:** P5, P7, P8, P29, Section 11 (Goanna row); inventory G1-G4, 4.4.

### D7: Account-status enforcement

- **Question:** Which consumers enforce account status (ACTIVE, SUSPENDED, TERMINATING)?
- **Status:** Accepted.
- **Decision:** Every production consumer enforces account status from the AccountFact.
- **Rationale:** A suspended account must be refused everywhere; today only AWSGW checks it (inventory S13), Predastore does not (inventory 4.3).
- **Implementation gate:** Each consumer's migration PR includes a test that a SUSPENDED account is refused after the signature.
- **Links:** P5, P23 check 2, Section 11; inventory S13, 4.1 step 9, 4.3.

### D8: Predastore config `[[auth]]` credentials

- **Question:** May Predastore's config-file `[[auth]]` credentials keep overriding IAM?
- **Status:** Rejected.
- **Decision:** Config credentials may not override IAM or bypass bucket ownership or policy evaluation.
  Any bootstrap exception needs a separately designed, bounded bootstrap principal.
- **Rationale:** Today `[[auth]]` keys are checked before IAM and returned with `SkipPolicyCheck: true` (inventory P7), a permanent authority bypass that ADR-0005 S1 excludes from the target.
- **Implementation gate:** If any bootstrap exception is kept, the bounded bootstrap principal is designed and reviewed before Predastore migrates (Section 13 step 3c).
- **Links:** Section 11 (Predastore row), Section 13 step 3c; inventory P7, S24, 4.3.

### D9: Owner and publisher of session-validity facts

- **Question:** Which component owns and publishes session CredentialFacts: the IAM authority, or the STS session store that physically holds session records today?
- **Status:** Open; not decided.
- **Context:** ADR-0005 S2 makes IAM the logical authority for session-credential validity and revocation, while STS writes and reads `spinifex-iam-session-credentials` today (inventory S2).
- **Implementation gate:** D9 must be explicitly decided before Phase 1 (Section 13 step 1) begins.
- **Links:** P2, P7, P20, P21, Section 11 (STS row); inventory S2, S12.

### D10: Error ordering for expired sessions

- **Question:** At what point in request verification is `ExpiredToken` reported for an expired session?
- **Status:** Accepted (target).
- **Decision:** Emit `ExpiredToken` only after both session-token validation and SigV4 proof succeed.
- **Rationale:** Today AWSGW reports expiry before checking the session token or the signature, so holding only an ASIA key ID reveals that an expired session existed (`TestCurrentBehaviour_Session_Expired`, `TestCurrentBehaviour_AntiEnumeration_RejectionShapes`).
  AWS's actual ordering is unverified, so this is a chosen target, not a parity claim.
- **Implementation gate:** A conformance fixture and an AWSGW test show an expired session with a missing or wrong token, or a bad signature, does not receive `ExpiredToken`; the current-behaviour tests are updated in the same change.
- **Links:** P24 (Expired), P25, Section 12; inventory 4.1 steps 6b-6c, Section 7 finding F2.

### D11: GetSessionToken session after its source key is deleted or deactivated

- **Question:** Does a GetSessionToken session survive deletion or deactivation of the long-term access key that minted it?
- **Status:** Open.
- **Decision so far:** The future contract's session fact model gains an optional `sourceCredentialID`; revocation behaviour does not change now.
- **Context:** Today the session survives both (`TestCurrentBehaviour_Session_GetSessionTokenOutlivesMintingKey`); AWS behaviour is unverified.
- **Implementation gate:** Decided before any consumer enforces `sourceCredentialID`.
- **Links:** P7, P21, Section 12; inventory Section 7 finding F5.

### D12: Session revocation mechanism

- **Question:** Is a session revoked by deleting its session record, as today, or by an AWS-style deny conditioned on token issue time?
- **Status:** Open; not decided.
- **Context:** AWS attaches an inline deny policy conditioned on `aws:TokenIssueTime` to the role [AWS5]; Spinifex deletes session records on principal deletion (P21).
- **Implementation gate:** Decided before session revocation is implemented in the projection producer (Section 13 step 2).
- **Links:** P20, P21, P22, Section 12; inventory S2.
