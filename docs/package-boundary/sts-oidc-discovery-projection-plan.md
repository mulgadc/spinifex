# STS OIDC discovery projection plan

**Status: Proposed** — scope is ADR-0005 S4 only; the Q-22 cut uses an EKS-owned in-process projection, not a wire contract.

## Current state

- `handlers/sts/oidc_jwks.go:14` imports `handlers/eks` for exactly two symbols: `AccountBucketName` (line 101) and `OIDCJWKSKey` (line 110).
- No EKS store type, record or `ClusterMeta` is used by STS production code; the JetStream access is `s.js.KeyValue("eks-account-{issuerAccount}")` then `kv.Get("clusters/{cluster}/oidc-jwks.json")` (`oidc_jwks.go:93-126`).
- The only caller is the `AssumeRoleWithWebIdentity` key function (`handlers/sts/assume_role_with_web_identity.go:159-193`): parse the `iss` claim (`ParseEKSIssuerURL`, line 173), check the IAM OIDC-provider registration in the role account (line 177), read the JWKS from the issuer account (line 180), select by `kid` (line 187).
- Every failure in the key function becomes `InvalidIdentityToken` (HTTP 400) at line 71; there is no cache, so each request reads KV afresh.
- The public `/oidc/eks/{region}/{accountID}/{clusterName}/.well-known/openid-configuration` and `/keys` routes are served by the root `gateway` package (`gateway/gateway.go:351-352`, `gateway/oidc_discovery.go`), not by STS, and are outside the Q-22 rule because the root gateway package has no domain.
- The composition root is `runtime/roles/awsgw/awsgw.go:222` (`NewSTSServiceImpl`), which already wires one cross-service capability at line 228 (`SetSessionRevoker`).
- The JWKS key is written once per cluster launch (`handlers/eks/service_impl.go:874` → `oidc_keypair.go:94`) as a single-key document, read by the EKS bootstrap cross-check (`nats_bootstrap.go:283`), and removed only by `DeleteClusterPrefix` when teardown completes (`service_impl.go:1442`); there is no rotation path.

## Resolver (consumer-owned, in `handlers/sts`)

```go
// ClusterJWKSResolver returns the published JWKS document for an EKS cluster issuer.
type ClusterJWKSResolver interface {
	LookupClusterJWKS(ctx context.Context, accountID, clusterName string) (jwks []byte, found bool, err error)
}
```

| Today's outcome | Resolver result | STS result (unchanged) |
| --- | --- | --- |
| Account bucket missing (`ErrBucketNotFound`) | `nil, false, nil` | `InvalidIdentityToken` |
| Cluster missing, key absent, key deleted by teardown | `nil, false, nil` | `InvalidIdentityToken` |
| Bucket open or key read fails (timeout, unavailable) | `nil, false, err` | `InvalidIdentityToken` |
| Key present, any bytes | `raw, true, nil` | STS decodes; malformed or empty set is `InvalidIdentityToken` |

- The resolver returns raw bytes so JSON decoding, the empty-set rule and `JWK`/`JWKS` types stay in STS, and the implementation never imports `handlers/sts`.
- `FetchClusterJWKS` keeps its `(nil, nil)` / error contract, taking the resolver instead of `jetstream.JetStream`; `ParseEKSIssuerURL` and the registration-before-lookup ordering stay in STS.
- `NewSTSServiceImpl` takes the resolver as a required argument and rejects nil, mirroring the nil-IAM check, so a missing binding fails at start-up rather than as a permissive path.

## Placement: `handlers/eks`, not a new domain package

- Rejected: a new `domains/eks/projection` package. `Domains_ImportNoAgentsIngressOrLegacy` (`architecture/layering/rules_test.go:130`) forbids any new-path `domains/*` package from importing a `handlers/*` legacy root, and the bucket/key layout lives only in `handlers/eks` today, so a projection package could not read it without that forbidden edge.
- Instead: `handlers/eks` implements the resolver itself, beside the layout it already owns. `OIDCJWKSReader` (`handlers/eks/oidc_jwks_reader.go`), built with `NewOIDCJWKSReader(js jetstream.JetStream)`, does the same bucket-open-then-`Get`, the same timeouts and the same not-found classification `handlers/sts/oidc_jwks.go` used, next to `AccountBucketName` and `OIDCJWKSKey` in `store.go`.
- This is transitional: `handlers/eks` is the current owner of the layout, not a permanent home for a cross-domain adapter. When `handlers/eks` is extracted to a `domains/eks` package, the resolver implementation moves with it.
- `handlers/eks` must not import `handlers/sts` (that edge is still forbidden), so the compile-time interface check `var _ handlers_sts.ClusterJWKSResolver = (*handlers_eks.OIDCJWKSReader)(nil)` lives in the composition root, `runtime/roles/awsgw`, not beside the implementation.

## Wiring

- `runtime/roles/awsgw/awsgw.go:222` builds `handlers_eks.NewOIDCJWKSReader(js)` and passes it to `NewSTSServiceImpl` as a required, nil-checked argument.
- `tests/integration/harness.go:150` is the only other production-shaped STS construction site and is wired identically.

## Imports and allowlist

- Disappears: `handlers/sts/oidc_jwks.go` `handlers_eks` import, the last non-test STS import of any EKS package.
- Goes stale and is deleted: `{From: "handlers/sts", To: "handlers/eks"}` (`architecture/layering/allowlist_test.go:161`); the layering test fails on a stale entry, so it cannot be left in place.
- Stays: `handlers/sts → handlers/iam` and `gateway/sts → handlers/iam`, because the IAM registration read, trust policy and session crypto are out of scope.
- Test files that exercise the real KV layout move to `handlers/eks`, next to the layout they pin.
- Every remaining `handlers/sts` test, including the web-identity fixtures and the `newTestSetup` constructor wiring, uses a test-owned `fakeJWKSResolver` and imports no EKS package.
- The `Q22_STSImportsNoEKSImplementation` rule stays and now passes with no exception: STS imports no EKS package at all, not even through an allowlist entry, and that holds for test files too (`grep -rn 'handlers/eks' spinifex/handlers/sts/` is empty).

## Test placement (post-cut)

- `handlers/eks/oidc_jwks_reader_test.go`: the raw KV layout and literal bucket/key pin (`TestOIDCJWKSReader_UsesLiteralBucketAndKey`), missing bucket (`TestOIDCJWKSReader_AccountBucketMissingIsNotFound`), dependency failures and a tombstoned key (`TestOIDCJWKSReader_DependencyFailureSemantics`), input validation (`TestOIDCJWKSReader_NilJetStream`, `TestOIDCJWKSReader_EmptyArgs`), cluster status never consulted (`TestCurrentBehaviour_ClusterRecordStateIsNotConsulted`, moved from `handlers/sts`), and a deleted cluster prefix reading as not found (`TestCurrentBehaviour_ClusterPrefixDeletedIsNotFound`, moved from `handlers/sts`).
- `handlers/sts/fake_jwks_resolver_test.go`: the `fakeJWKSResolver` test double — a map keyed by `(accountID, clusterName)` with seed/seedErr and call recording — used by every STS test in this package.
- `handlers/sts/oidc_jwks_test.go`: `FetchClusterJWKS` unit tests against the fake, including `TestFetchClusterJWKS_PropagatesResolverErrorUnchanged` (renamed from `..._ResolverErrorIsWrapped`), which pins that a resolver error passes through unchanged via `assert.Same` and `errors.Is`.
- `handlers/sts/oidc_jwks_current_behaviour_test.go`: the STS-owned outcomes — registration runs before any resolver call (`TestCurrentBehaviour_UnregisteredIssuerNeverCallsResolver`), the issuer's account is queried, not the role's (`TestCurrentBehaviour_CrossAccountIssuerQueriesIssuerAccount`), every resolver outcome (not-found, malformed JWKS, dependency error) maps to `InvalidIdentityToken` (`TestCurrentBehaviour_ClusterMissingIsInvalidIdentityToken`, `TestCurrentBehaviour_JWKSMalformedIsInvalidIdentityToken`, `TestCurrentBehaviour_ResolverErrorIsInvalidIdentityToken`), and rotation is read fresh each request (`TestCurrentBehaviour_JWKSRotationIsReadFreshEachRequest`).
- `handlers/sts/service_impl_test.go` and `assume_role_with_web_identity_test.go`: `newTestSetup` wires a `*fakeJWKSResolver`, and the web-identity fixture seeds it directly instead of writing EKS KV records.
- `tests/integration/sts_eks_oidc_issuer_test.go`: the one test that deliberately imports both `handlers/eks` and `handlers/sts` — `TestEKSClusterOIDCIssuer_RoundTripsThroughSTSParseEKSIssuerURL` proves a `ClusterOIDCIssuer` URL parses through `ParseEKSIssuerURL` to the same `(accountID, clusterName)`.
- `gateway/oidc_discovery_current_behaviour_test.go` is unchanged: `TestCurrentBehaviour_WebIdentityJWKSMissingWireResponse` still builds a real `OIDCJWKSReader` plus a real STS service and pins the wire HTTP 400 with `<Code>InvalidIdentityToken</Code>`. The root `gateway` package has no domain classification and composes both packages intentionally, so this is not a package-boundary violation.
- `gateway/oidc_discovery_current_behaviour_test.go` also pins the public discovery/JWKS endpoints: 404/500 bodies, verbatim JWKS bytes and `Cache-Control: public, max-age=300`, unaffected by this cut.

## Non-goals

- Moving IAM state, the OIDC-provider registry read or `handlers_iam` imports.
- Redesigning web-identity verification, issuer parsing, audience or trust-policy handling, or fetching JWKS over HTTPS as AWS STS does.
- Moving `handlers/sts` to `domains/sts`, or changing the cluster-status blindness, DELETING/FAILED key retention, or adding rotation.

## Open questions

- Should the root-gateway discovery handlers (`gateway/oidc_discovery.go:38,93`) consume an EKS-owned resolver in a later change, the same pattern as STS? The root gateway's `handlers/eks` import has no allowlist entry today because the root gateway has no domain classification, so it is tracked as debt rather than fixed here; S4 is about STS only.
