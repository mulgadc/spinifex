# IAM enforcement inventory

**Status:** Discovery record for ADR-0005 S6(1) and rollout evidence item 1; describes present code only and authorises no change.

This inventory lists every direct consumer of IAM private state across Spinifex, Predastore, Goanna and Bluebottle, and records the current authentication flow at each entry point.
The design that replaces these reads is `iam-enforcement-projection-design.md` in this directory; D-numbers below refer to its decision register (Section 14 of that document).
AWSGW findings pinned by characterisation tests are listed in Section 7.

## Sources and conventions

| Repo | Revision read | Path prefix used below |
|---|---|---|
| Spinifex | `feat/adr-0005-iam-enforcement-projection` at `976df15d7` | relative to `spinifex/spinifex/` |
| Predastore | `origin/dev` at `37ef410` (`predastore-wt-iamchar`) | `predastore:` |
| Goanna | `origin/dev` at `b4c6b31` (`goanna-wt-iamchar`), code reading only; not characterised in Phase 0 (D6) | `goanna:` |
| Bluebottle | `dev` at `f2f4149` | `bluebottle:` |

"IAM private state" means the `spinifex-iam-*`, `spinifex-accounts` and `iam-account-<id>` KV buckets and their records, access-key and session secrets, the master key that encrypts them, stored policy documents, role and trust records, STS session records and tokens, OIDC provider records, and any type that encodes those records.
Locations were found with the allyourbase index, then confirmed by direct reads, because the index was stale for `handlers/iam/service_impl.go` and does not cover the two worktrees.
Line numbers are those at the revisions above.

Process names: **awsgw** is the AWS gateway role (`runtime/roles/awsgw`), which hosts the IAM and STS implementations in-process; **daemon** is the Spinifex node daemon; **vpcd** hosts guest metadata; **spx** is the operator CLI; **s3d** is the Predastore server; **goannad** is the Goanna server.

## 1. IAM private records today

| Bucket | Key | Record (`handlers/iam/types.go` unless stated) | Secret fields |
|---|---|---|---|
| `spinifex-iam-access-keys` | `<AKID>` | `AccessKey` (68): `access_key_id`, `secret_access_key`, `user_name`, `account_id`, `status`, `created_at` | `secret_access_key` is AES-256-GCM under the master key, no associated data |
| `spinifex-iam-users` | `<account>.<userName>` | `User` (36): `user_id`, `arn`, `access_keys`, `attached_policies`, `groups`, `inline_policies` | none |
| `spinifex-iam-groups` | `<account>.<groupName>` | `Group` (53) | none |
| `spinifex-iam-roles` | `<account>.<roleName>` | `Role` (111): `role_id`, `arn`, `assume_role_policy_document`, `max_session_duration`, `attached_policies`, `inline_policies` | none |
| `spinifex-iam-policies` | `<account>.<policyName>` | `Policy` (84): `arn`, `policy_document`, versions | none |
| `spinifex-iam-instance-profiles` | `<account>.<name>` | `InstanceProfile` (128) | none |
| `spinifex-accounts` | `<account>` | `Account` (182): `status` ACTIVE, SUSPENDED or TERMINATING | none |
| `iam-account-<account>` | `OIDCProviderKey(issuer)` (`handlers/iam/oidc_providers.go:42`) | OIDC provider record | none |
| `spinifex-iam-session-credentials` | `<ASIA AKID>` | `SessionCredential` (`handlers/sts/session_credentials.go:40`): principal type, role/user IDs and ARNs, `expires_at` | `secret_encrypted` (AES-GCM, master key); `session_token_hmac` (HMAC-SHA256 keyed by the same master key, `handlers/sts/assume_role.go:613`) |

Bucket constants are declared at `handlers/iam/service_impl.go:38-45` and `handlers/sts/session_credentials.go:23`.
The master key is one 32-byte file, `<config>/master.key`, loaded group-readable with `masterkey.ReadShared` or `LoadShared` (`bluebottle:pkg/masterkey/masterkey.go:50-58`).
It encrypts IAM and session secrets and keys the session-token HMAC, and the same key also seals ACM, ECR, RDS, EKS, Ochre and Bedrock secrets (Section 4).

## 2. Consumer matrix

One classification per row, from: authority, credential verifier, policy evaluator, role resolver, session consumer, bootstrap path, invalid private-store reader.
Rows marked **CLI** or **test** are operator or test-support code.

### 2.1 Spinifex

| # | Location | Process | Reads / writes | Classification |
|---|---|---|---|---|
| S1 | `handlers/iam/service_impl.go:133` (`NewIAMServiceImpl`), all `IAMServiceImpl` methods | awsgw | Creates, migrates, reads and writes every IAM bucket in Section 1 except sessions; holds the master key (210) | authority |
| S2 | `handlers/sts/session_credentials.go:61-230`, `handlers/sts/assume_role.go:543-600` (`mintSession`) | awsgw | Writes and reads `spinifex-iam-session-credentials`; encrypts the session secret and HMACs the token with the raw master key | authority. Borderline: ADR-0005 S2 names IAM the logical authority for session validity, but STS physically owns this store today. |
| S3 | `handlers/iam/imds_rpc.go:34` (`SubscribeIMDSResponders`), `handlers/sts/imds_rpc.go:30` | awsgw | Serves instance-profile, `GetRole` and `AssumeRoleForInstance` over NATS to vpcd | authority |
| S4 | `handlers/sts/assume_role_with_web_identity.go:197-215` (`verifyOIDCProviderRegistered`) | awsgw | Opens `iam-account-<roleAccount>` with its own JetStream handle and reads `OIDCProviderKey(issuer)`, bypassing `IAMService` | invalid private-store reader |
| S5 | `handlers/sts/role_arn.go:27` (`ResolveRoleByARN`), used by `assume_role.go:167`, `assume_role_with_web_identity.go:83`, `gateway/sts.go:108` | awsgw | `IAMService.GetRole`: `RoleId`, `Arn`, `AssumeRolePolicyDocument`, `MaxSessionDuration` | role resolver |
| S6 | `handlers/sts/assume_role.go:248-290`, `web_identity_trust_policy.go:42-61` | awsgw | Parses the role trust policy with `handlers_iam.ValidateTrustPolicyDocument` | policy evaluator |
| S7 | `handlers/sts/get_session_token.go:73` | awsgw | `IAMService.GetUser` to bind the user ID at mint | role resolver. Borderline: it resolves a user, but the capability is the same canonical-principal resolution as roles. |
| S8 | `handlers/sts/session_principal.go:61-125` (`VerifySessionPrincipal`) | awsgw | `GetUser`/`GetRole` by name, compares the live immutable ID with the one stored at mint | credential verifier |
| S9 | `handlers/sts/presigned_url_verify.go:150-200` (`resolvePrincipalForVerify`), exposed over NATS by `runtime/roles/awsgw/eks_token_verify.go:29` | awsgw, for the EKS token webhook agent | `LookupAccessKey`, `DecryptSecret`, `GetUser`; session record via `LookupSessionCredential` and `handlers_iam.DecryptSecret(…, s.masterKey)` | credential verifier |
| S10 | `handlers/sts/get_access_key_info.go:73` | awsgw | `LookupAccessKey` and session lookup to map an AKID to its account | credential verifier. Borderline: needs only the AKID-to-account fact. |
| S11 | `gateway/auth.go:375-405` (`resolveLongLivedAKID`) | awsgw | `LookupAccessKey`, status, `DecryptSecret` | credential verifier |
| S12 | `gateway/auth.go:409-472` (`resolveSessionAKID`), `483-520` (`checkSessionPrincipal`) | awsgw | `LookupSessionCredential`, expiry, `VerifySessionToken`, `DecryptSecret`, then S8 | session consumer |
| S13 | `gateway/auth_accountstatus.go:67-97` (`checkAccountActive`) | awsgw | `GetAccount` status; ACTIVE cached 5 s (`accountActiveTTL`, line 16) | credential verifier. Borderline: account status gates every credential, so it is verification input rather than policy. |
| S14 | `gateway/gateway.go:771` (`principalUser`) → `gateway/sts/GetCallerIdentity.go:61` (`ResolveCallerUser`) | awsgw | `GetUser`/role lookup for `aws:userid` and the user ARN | policy evaluator. Borderline: a principal read, but its only use is condition-key input. |
| S15 | `gateway/gateway.go:809-885` (`evaluatePrincipalPolicyResources`) | awsgw | `GetUserPolicies` (`handlers/iam/service_impl.go:1997`) or `GetRolePolicies` (`handlers/iam/roles.go:579`): user, group, role and policy records; evaluates with `iampolicy.EvaluateWithKeys`; global root bypass at 827 | policy evaluator |
| S16 | `gateway/gateway.go` `checkPassRole`, callers `gateway/ec2.go:194,212,221,477`, `gateway/ecs.go:66`, `gateway/eks.go:296` | awsgw | Evaluates `iam:PassRole` on an ARN; role existence comes from S5-style lookups in the handlers | role resolver. ADR-0005 S3 scope; listed for completeness, not a projection consumer. |
| S17 | `gateway/ecr_principal.go:81-250` (`resolveECRPrincipal` and helpers) | awsgw (OCI registry path) | Rehydrates an ECR bearer token: `LookupAccessKey`, `GetUser`, `GetAccount`, `LookupSessionCredential`, S8 | credential verifier |
| S18 | `gateway/admin_createaccount.go:112,135` | awsgw | Encrypts the new account admin's secret with the IAM master key into the admin idempotency store and decrypts it on replay | bootstrap path. Borderline: it keeps a copy of an IAM secret outside the IAM store. |
| S19 | `runtime/roles/awsgw/awsgw.go:205-241, 272-286` | awsgw | Reads `master.key` raw, builds IAM and STS, wires the revoker, consumes `bootstrap.json` via `SeedBootstrap` (`handlers/iam/service_impl.go:855`) | bootstrap path |
| S20 | `daemon/eks_deps.go:27-48` (`systemRoleEnsurer`), `daemon/ecs_deps.go:39-51` | daemon | Reads `master.key` and constructs a second full `IAMServiceImpl`, which opens and migrates every IAM bucket and writes roles, instance profiles and policies | invalid private-store reader. Borderline: it runs IAM's own code, but in a non-authority process with full store access and the master key. |
| S21 | `domains/ec2/guestmetadata/nats_clients.go:42-95` | vpcd | Receives `handlers_iam.InstanceProfile` and `iam.GetRoleOutput` (including the trust document) over NATS from S3, cached 5 min (line 23); requests session credentials for guests | role resolver |
| S22 | `operator/cli/admin.go:2876-2899` (`initIAMServiceFromConfig`), callers `admin_principal.go:43,116,246,313,476`, `admin_account_delete.go:76`, `admin.go:2918,3046` | spx (**CLI**) | Connects with the cluster NATS token, reads `master.key`, builds a full `IAMServiceImpl` | bootstrap path |
| S23 | `operator/cli/admin.go:3449-3480` (`writeBootstrapFilesWithAdmin`) | spx (**CLI**) | Generates `master.key`, encrypts the system and admin secrets into `bootstrap.json`, writes plaintext `system-credentials.json` (3450) | bootstrap path |
| S24 | `operator/cli/templates/predastore.toml:79-121` | spx (**CLI**) | Renders Predastore's `[iam]` section with the NATS token, `master.key` path and `spinifex-iam-access-keys`, plus `[[auth]]` system and Northstar keys | bootstrap path |
| S25 | `accountteardown/reapers_iam.go:17-330`, used by `gateway/admin_deleteaccount.go:114` and `operator/cli/admin_account_delete.go:93` | awsgw, spx | Deletes IAM entities through `IAMService` methods only | authority. Borderline: a lifecycle caller through the authority's API, not a raw reader. |
| S26 | `handlers/iam/mock/mock.go`, `handlers/sts/*_test.go`, `operator/cli/admin_test.go:1090` | **test** | Mock `IAMService`; tests open the session bucket and assert template bucket names | bootstrap path (test support) |

`IAMService` lifecycle callers that use only the public interface and read no private record are not projection consumers: `handlers/{ecs,eks,rds,elbv2}` system-role ensurers, `domains/ec2/awsapi/{instance,spotinstance}` and the IAM AWS adapter `gateway/iam/*`.

### 2.2 Predastore

| # | Location | Process | Reads / writes | Classification |
|---|---|---|---|---|
| P1 | `predastore:internal/gate/auth/auth.go:84-152` | s3d | Duplicates `iamAccessKey`, `sessionCredential`, `iamUser`, `iamRole`, `iamGroup`, `iamPolicy` JSON shapes | invalid private-store reader |
| P2 | `auth.go:299-356` (`NewNATSIAMProvider`) | s3d | `masterkey.LoadShared(master.key)` (311); NATS with the cluster token | invalid private-store reader |
| P3 | `auth.go:624-725` (`LookupCredentials`) | s3d | Reads `spinifex-iam-access-keys/<AKID>`, checks `status`, decrypts `secret_access_key`; caches 60 s (217) with watchers (395-397) | invalid private-store reader. It is functionally a credential verifier, but every input is a private record. |
| P4 | `auth.go:457-560` (`lookupSessionCredentials`) | s3d | Reads `spinifex-iam-session-credentials/<ASIA>`, expiry, decrypts `secret_encrypted`; never cached | invalid private-store reader |
| P5 | `auth.go:764-998` (`resolveUserPolicies`, `resolveGroupPolicies`, `resolveRolePolicies`, `resolveManagedPolicies`) | s3d | Reads users, groups, roles, policies by `<account>.<name>`; reimplements group and managed-policy resolution | invalid private-store reader |
| P6 | `internal/gate/middleware.go:263-290` | s3d | `iampolicy.EvaluateWithKeys` over P5's documents | policy evaluator |
| P7 | `auth.go:158-180` (`ConfigProvider`), `auth.go:999-1029` (`ChainProvider`), `internal/gate/policy.go:19`, `middleware.go:311,326`, `internal/gate/handlers/list_buckets.go:90` | s3d | Config `[[auth]]` keys checked before IAM, returned with `SkipPolicyCheck: true` | bootstrap path |
| P8 | `predastore:internal/storetest/keys.go:13` | **test** | Deterministic test master key for shard tests, not IAM | bootstrap path (test support) |

### 2.3 Goanna

Not characterised in Phase 0; deferred (D6).
The rows below are inventory facts from code reading, not test evidence.
Goanna is a future consumer of IAM state: it currently reads the IAM buckets directly, and it must adopt the final IAM enforcement projection, including IAM identity-policy enforcement, before production use.

| # | Location | Process | Reads / writes | Classification |
|---|---|---|---|---|
| G1 | `goanna:internal/auth/provider.go:33-76` | goannad | Duplicates the access-key and session JSON shapes and bucket names | invalid private-store reader |
| G2 | `provider.go:113-170` (`NewProvider`) | goannad | `masterkey.LoadShared(master.key)` (128) | invalid private-store reader |
| G3 | `provider.go:174-226` (`lookupAccessKey`) | goannad | Reads `spinifex-iam-access-keys/<AKID>`, status, decrypts; caches 60 s (49) with a watcher | invalid private-store reader |
| G4 | `provider.go:229-260` (`lookupSession`) | goannad | Reads `spinifex-iam-session-credentials/<ASIA>`, expiry, decrypts; uncached | invalid private-store reader |

Goanna evaluates no IAM policy (no `iampolicy` import and no authorization code under `goanna:internal` or `goanna:cmd`), so it reads no policy, user, group or role record.

### 2.4 Bluebottle

Bluebottle reads no IAM state and declares no type that encodes an IAM record.

| Package | Role | Note |
|---|---|---|
| `bluebottle:pkg/iampolicy` | Policy vocabulary and evaluator (`EvaluateWithKeys`, `evaluator.go:36`) | Shared contract, used by S15, P6 and STS; `door_test.go` already fixes the condition-key sets of the gateway and Predastore doors. |
| `bluebottle:pkg/auth/arn.go` | ARN parsing (`ParseRoleARN` 39, `ResolvePolicyARN` 59) | Mechanism only. |
| `bluebottle:pkg/sigv4` | SigV4 parse and verify | `Verify` (`verify.go:24`) takes the plaintext secret; there is no verify-by-derived-signing-key entry point. |
| `bluebottle:pkg/masterkey` | AES-256-GCM key loading | The package comment describes shard encryption, but every IAM consumer uses it; `EncryptBase64`/`DecryptBase64` (135-160) pass no associated data. |

## 3. Holders of the IAM master key that read no IAM record

These are not IAM consumers, but each process holds the key that decrypts every IAM and session secret, and each uses the IAM package's helpers as its crypto boundary.

| Location | Process | Use |
|---|---|---|
| `domains/acm/store.go:224,327` | awsgw | ACM private keys via `handlers_iam.EncryptSecret`/`DecryptSecret` |
| `domains/ecr/auth/signingkey.go:179,206` | awsgw | ECR token signing key |
| `gateway/bedrock/credentials.go:66,87` | awsgw | Bedrock credentials bucket |
| `handlers/rds/bootstrap.go:105,199`, `daemon/rds_deps.go:56` | daemon | RDS bootstrap envelopes |
| `handlers/eks/oidc_keypair.go:82,116`, `nats_bootstrap.go:247,265`, `nodegroup.go:1160`, `daemon/eks_deps.go:52` | daemon | EKS OIDC signing key, join tokens, kubeconfig |
| `domains/ochre/vector/appliance.go:499,514`, `daemon/ochre_deps.go:63`, `operator/cli/admin_ochre.go:1005,1077` | daemon, spx | Ochre appliance passwords |
| `daemon/daemon.go:2003,2138`, `daemon/ecs_deps.go:39` | daemon | Service dependencies |

## 4. Current authentication flows

### 4.1 AWSGW SigV4 (`gateway/auth.go:40-290`)

| Step | Check | Failure response | Rate-limited |
|---|---|---|---|
| 1 | Per-IP lockout (51) | `RequestLimitExceeded` | n/a |
| 2 | `sigv4.Parse`: envelope, scope, skew, payload (57-97) | `MissingAuthenticationToken`, `RequestEntityTooLarge`, `InternalError` (body read), `SignatureDoesNotMatch` (skew), `IncompleteSignature` | skew and malformed only |
| 3 | Credential-scope service served (115-125) | Unknown-service error | no |
| 4 | NATS connected (129) | 503 `ServiceUnavailable` | no |
| 5a | AKIA: KV `Get` (S11) | unknown → `InvalidClientTokenId`; KV error → `InternalError` | unknown only |
| 5b | AKIA: `status == Active` | `InvalidClientTokenId` | yes |
| 5c | AKIA: decrypt secret | `InvalidClientTokenId` | yes |
| 6a | ASIA: session `Get` (S12) | KV error → `InternalError`; absent → `InvalidClientTokenId` | absent only |
| 6b | ASIA: expiry (424) | `ExpiredToken` | yes |
| 6c | ASIA: `X-Amz-Security-Token` present and HMAC matches (431-442) | `InvalidClientTokenId` | yes |
| 6d | ASIA: decrypt secret | `InvalidClientTokenId` | yes |
| 7 | Other prefix (149-154) | `InvalidClientTokenId` | yes |
| 8 | `sig.Verify` (163) | `SignatureDoesNotMatch` | yes |
| 9 | Account ACTIVE (183, S13) | not active → `AccessDenied`; read error → `InternalError` | no |
| 10 | Session principal continuity (191, S8) | replaced, gone or legacy → `InvalidClientTokenId`; fault → `InternalError` | verdicts except legacy |
| 11 | Resolve `aws:userid` (198, S14) | fault → `InternalError`; missing user → key omitted | no |
| 12 | Per action: policy resolution and evaluation (S15) | `AccessDenied`; resolution fault after 3 tries on transient NATS errors → `InternalError` | no |

Caching: none for credentials, sessions or policies; only the ACTIVE account answer for 5 s.
Backing state unavailable: the gateway's NATS connection closed fails fast with 503; a closed IAM/STS connection or a deleted access-key, session or accounts bucket gives `InternalError`, and no request proceeds (`TestCurrentBehaviour_Auth_BackingStateUnavailable`, in-process router over embedded JetStream; not exercised on a deployed cluster).

Anti-enumeration:

- An unknown AKIA, an inactive AKIA, an undecryptable secret and an unknown prefix all return `InvalidClientTokenId`, so status is not revealed.
- A known, active key with a wrong signature returns `SignatureDoesNotMatch`, so key existence is distinguishable from an unknown key; whether AWS returns the same codes is unverified (Section 5).
- Account status, principal continuity and policy reads all happen after the signature, so a caller without the secret cannot probe them (comments at 181 and 476-482).
- An ASIA key past expiry returns `ExpiredToken` (HTTP 400) before the session token or signature is checked, so anyone holding only an expired session AKID learns that it existed (Section 7 F2, D10); records are kept for one hour past expiry (`janitorGracePeriod`, `handlers/sts/session_credentials.go:35`).
- An AKIA key ignores `X-Amz-Security-Token`: the header is signed but never checked (Section 7 F1).
- Timing: an unknown AKIA returns after one KV read, a known key additionally decrypts and HMACs; the error codes already differ, so the timing difference adds nothing for unknown versus bad-signature, and unknown versus inactive both stop after the same KV read. Not measured.

### 4.2 AWSGW anonymous STS (`gateway/sts_anonymous.go:16-55`, `gateway/sts.go:81`)

Only `AssumeRoleWithWebIdentity` is anonymous.
The interceptor runs before the SigV4 middleware (`gateway/gateway.go:347`), so neither the per-IP auth lockout nor the post-auth throttle applies to it.
Order in `handlers/sts/assume_role_with_web_identity.go`:

1. Parameter validation and rejection of session policies (36-50), `ValidationError` or `MissingParameter`.
2. Role ARN syntax to obtain the role account only (55-58).
3. JWT parse, ES256 only, `exp` required (60-72); the key function (159-193) resolves `kid`, `iss`, issuer account and cluster, checks the OIDC provider registration by raw read (S4), and fetches the cluster JWKS through the EKS resolver. Every failure here, including a KV outage, returns `InvalidIdentityToken`.
4. `iss`, `sub` non-empty and `aud` contains `sts.amazonaws.com` (74-81), `InvalidIdentityToken`.
5. Role resolution (83-90): absent, foreign or non-canonical ARN → `AccessDenied`.
6. Duration bound against the role's `MaxSessionDuration` (92-104), `ValidationError`.
7. Trust policy evaluation (114), then mint (119).

The token is verified before any role read, so an unauthenticated caller cannot probe role existence.
A caller holding a valid token from a registered issuer can distinguish an existing role (step 6 `ValidationError` for an over-long duration) from an absent one (step 5 `AccessDenied`); whether AWS orders these checks the same way is unverified.
No caching: every call reads the provider record and the JWKS.

`GetAccessKeyInfo` (S10) is signed but needs no permission and returns the account for any AKIA or ASIA key it holds.

### 4.3 Predastore S3 SigV4 (`predastore:internal/gate/middleware.go:214-330`)

1. Unsigned request: allowed through only to a public bucket's permitted methods, else `AccessDenied`.
2. `sigv4.Parse` errors map in `internal/gate/handlers/respond.go:20-100`.
3. `ChainProvider.LookupCredentials` (P7): config `[[auth]]` keys first, then NATS IAM.
4. AKIA (P3): cache hit returns at once; else KV read, status, empty-account check, decrypt, then user, group and policy resolution (P5) before the signature is verified.
5. ASIA (P4): KV read, expiry, decrypt, then user or role policy resolution; no session-token check and no principal-continuity check.
6. `sig.Verify` (250), then `EvaluateWithKeys` unless `SkipPolicyCheck` (263-290), then bucket ownership (299-322).

| Condition | Response |
|---|---|
| Unknown AKID, or session absent or expired, or a session principal deleted | 403 `InvalidAccessKeyId` |
| Inactive AKIA (685), undecryptable secret, empty account, deleted user behind an AKIA, any KV fault | 500 `InternalError` |
| Malformed or foreign attached policy ARN (`ErrPrincipalConfig`) | 403 `AccessDenied` |
| Wrong signature | 403 `SignatureDoesNotMatch` |
| Policy deny or cross-account bucket | 403 `AccessDenied` |
| IAM buckets not yet created | 403 `InvalidAccessKeyId` (bootstrap) |

Caching: AKIA results for 60 s, invalidated by watchers on the access-key, user, policy and group buckets but not roles; a watcher that dies flushes once and then leaves the 60 s TTL as the only bound.
Sessions are never cached.
Backing state unavailable: KV faults give 500; config keys keep working because they are checked first.

Anti-enumeration: an inactive key (500) is distinguishable from an unknown key (403) before any signature check, and policy resolution runs for any presented AKIA before the signature, so an unauthenticated caller who knows a key ID can drive user, group and policy reads.
No account-status check exists in Predastore.

### 4.4 Goanna CloudWatch SigV4 (`goanna:internal/auth/middleware.go:42-100`)

From code reading only; not characterised in Phase 0; deferred (D6).

1. `sigv4.Parse`; envelope errors map in `writeParseError` (104-128).
2. Credential-scope service must be `monitoring` (59-66).
3. `Provider.Lookup` (68): AKIA via G3 or ASIA via G4.
4. `signed.Verify` (74), `SignatureDoesNotMatch` on failure.
5. The account is attached to the context; no policy or account-status check follows.

Unknown, inactive, empty-account and undecryptable keys all return 403 `InvalidClientTokenId`; an expired session returns 403 `ExpiredToken` before signature verification; a KV fault returns 503 `ServiceUnavailable`.
Caching: AKIA 60 s with a watcher on the access-key bucket; sessions uncached.
No session-token check and no principal-continuity check.

## 5. AWS reference points used above

Each claim cites an official AWS documentation page or is marked unverified; citations are listed in the design document, Section 12.

- Unknown key versus wrong signature: the wire codes AWS returns (`InvalidClientTokenId` or `InvalidAccessKeyId` versus `SignatureDoesNotMatch`) are unverified; the STS *Common Error Types* page (https://docs.aws.amazon.com/STS/latest/APIReference/CommonErrors.html) lists neither code.
- Key-ID existence: AWS documents `sts:GetAccessKeyInfo` as returning the account for an access key ID, and states that it does not indicate whether the key is active, inactive or deleted (*GetAccessKeyInfo*, https://docs.aws.amazon.com/STS/latest/APIReference/API_GetAccessKeyInfo.html).
- Inactive key: unverified.
- Expired session: the STS *Common Error Types* page lists `ExpiredTokenException` with HTTP 403; the Query-protocol code and status, and whether expiry is checked before or after the token and signature, are unverified.
- An ASIA key used without its session token: unverified.
- An AKIA key sent with `X-Amz-Security-Token`: unverified.
- `aws:userid`: the account ID for the account root user and the unique ID for an IAM user (*IAM policy elements: Variables and tags*, "Principal key values", https://docs.aws.amazon.com/IAM/latest/UserGuide/reference_policies_variables.html#principaltable).

## 6. Coverage gaps in this inventory

- The AWSGW flow (4.1) is pinned by the tests in `gateway/auth_current_behaviour_test.go`, which drive the real router over real IAM and STS services on an embedded JetStream; no flow was exercised on a deployed cluster.
- Predastore (4.3) and Goanna (4.4) flows are from code reading; Goanna is deferred (D6) and not characterised in Phase 0.
- The ECR registry path (S17), STS presigned verify (S9) and `GetAccessKeyInfo` (S10) are not covered by the Phase 0 tests.
- NATS subject ACLs: comments call the IMDS subjects "ACL-gated" (`handlers/iam/imds_rpc.go:13`), but the rendered `operator/cli/templates/nats.conf:41-44` defines one shared token with no per-user permissions; deployed NATS configuration was not inspected.
- Northstar, Viperblock and `spinifex/spinifex/agents` were searched for the bucket names and record field names and none reads IAM state; `viperblock/internal/predastoretest/server.go:131` only renders a Predastore test config.
- Viperblock loads its volume key with `masterkey.LoadShared` (`viperblock/viperblock/masterkey_load.go:25`) from a configured path; whether a deployment points that path at the IAM `master.key` was not checked.

## 7. Characterisation findings (AWSGW, current behaviour)

Each finding states what the named test in `gateway/auth_current_behaviour_test.go` proves about current behaviour, and nothing beyond it.

**F1. AKIA credentials with a session-token header are accepted.**
A long-lived AKIA key sent with an `X-Amz-Security-Token` header (here, another session's valid token) authenticates as the key's IAM user; the header is covered by the signature but never checked.
Test: `TestCurrentBehaviour_Session_AssumeRoleCredentials/long_lived_with_session_token`.
AWS behaviour for this request is unverified, so this is recorded as current behaviour only, with no change proposed.

**F2. Session expiry is reported before token validation and SigV4 proof.**
An expired ASIA session answers `ExpiredToken` (HTTP 400 today) whether the request carries the right token, no token, or a tampered token with a wrong secret; with no token, the door decrypts no secret before answering.
A caller holding only the ASIA key ID therefore learns that an expired session existed.
Tests: `TestCurrentBehaviour_Session_Expired` (`valid_token`, `no_token`, `wrong_token`) and `TestCurrentBehaviour_AntiEnumeration_RejectionShapes/session_expired_no_token`.
Target ordering: D10.

**F3. A tenant IAM user named `root` is presented with the account ID as its UserId.**
`GetCallerIdentity` for a tenant user named `root` returns `UserId` (the `aws:userid` value) equal to the account ID instead of the AIDA ID stored on its user record; the ARN is the user's own ARN.
AWS uses the account ID as `aws:userid` only for the account root user (Section 5).
This is an identity-presentation issue: the same user with no policy is implicitly denied `iam:ListUsers`, so it does not take the global-root policy bypass for that action.
Any effect on policies that test `aws:userid` in a condition is untested.
Tests: `TestCurrentBehaviour_Auth_LongLivedKeyResolvesPrincipal/tenant_user_named_root`, `TestCurrentBehaviour_Policy_IdentityPolicyOutcomes/tenant_user_named_root_implicit_deny`.

**F4. An orphaned access key authenticates as, and is authorized as, a same-name replacement user.**
The tests construct the orphaned state directly by rewriting the user record so it no longer lists the key; the access-key record stays `Active`.
They prove that the key still authenticates, that `DeleteUser` then succeeds, that after a same-name user is recreated the key authenticates as the new user (new `UserId`), and that the policy check applies the replacement user's policy: the key is allowed `iam:GetUser`, which only the replacement's policy grants, and denied `iam:ListUsers`, which only the original user's policy granted.
How production reaches the orphaned state is INFERRED from code: `CreateAccessKey` and `DeleteAccessKey` each make two non-transactional writes (`handlers/iam/service_impl.go:665-683`, `757-770`), and an interruption between them would leave the key unlisted; no failure-injection test proves this.
Tests: `TestCurrentBehaviour_Revocation_OrphanedKeyAuthenticatesAsRecreatedUser`, `TestCurrentBehaviour_Revocation_OrphanedKeyPermissionsAfterRecreation`.
Design consequence: P23.

**F5. A GetSessionToken session outlives its minting key.**
Deactivating and then deleting the long-term key that minted a GetSessionToken session leaves the session authenticating as the IAM user.
Test: `TestCurrentBehaviour_Session_GetSessionTokenOutlivesMintingKey`.
Open question: D11.
