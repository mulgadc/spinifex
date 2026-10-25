package handlers_sts

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/iam"
	"github.com/aws/aws-sdk-go/service/sts"
	"github.com/golang-jwt/jwt/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	iamdomain "github.com/mulgadc/spinifex/spinifex/domains/iam"
	awserrors "github.com/mulgadc/spinifex/spinifex/foundation/aws/errors"
	"github.com/mulgadc/spinifex/spinifex/foundation/state/kvutil"
)

// The TestCurrentBehaviour_ tests in this file pin today's AssumeRoleWithWebIdentity
// outcomes as seen through the ClusterJWKSResolver contract, not the target design.
// They use a fakeJWKSResolver (fake_jwks_resolver_test.go) so no EKS package
// is involved — the resolver's own storage-layer behaviour is pinned in the EKS package.

func charRole(t *testing.T, svc *STSServiceImpl, f *webIdentityFixture) *iam.Role {
	t.Helper()
	return createRoleForFixture(t, svc, f.accountID, "irsa-char", webIdentityTrustPolicyNoCondition(f.federatedARN))
}

func assumeWebIdentity(svc *STSServiceImpl, role *iam.Role, token string) (*sts.AssumeRoleWithWebIdentityOutput, error) {
	return svc.AssumeRoleWithWebIdentity(&sts.AssumeRoleWithWebIdentityInput{
		RoleArn:          role.Arn,
		RoleSessionName:  aws.String("char"),
		WebIdentityToken: aws.String(token),
	})
}

// registerIssuer records issuer as an OIDC provider in the role account's IAM bucket.
func registerIssuer(t *testing.T, svc *STSServiceImpl, roleAccountID, issuer string) {
	t.Helper()
	iamKV, err := kvutil.GetOrCreateBucket(t.Context(), svc.js,
		iamdomain.IAMAccountBucketName(roleAccountID), iamdomain.KVBucketIAMAccountVersion)
	require.NoError(t, err)
	_, err = iamKV.Put(t.Context(), iamdomain.OIDCProviderKey(issuer), []byte(`{"registered":true}`))
	require.NoError(t, err)
}

func charIssuer(accountID, cluster string) string {
	return fmt.Sprintf("https://gw.%s/oidc/eks/%s/%s/%s", testWebSuffix, testWebRegion, accountID, cluster)
}

func newKeyJWK(t *testing.T, kid string) (*ecdsa.PrivateKey, JWK) {
	t.Helper()
	priv, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	require.NoError(t, err)
	jwk := signingJWK(t, &priv.PublicKey)
	jwk.Alg, jwk.Use, jwk.Kid = "ES256", "sig", kid
	return priv, *jwk
}

// seedJWKS seeds the fake resolver for (accountID, clusterName) with keys.
func seedJWKS(t *testing.T, svc *STSServiceImpl, accountID, clusterName string, keys ...JWK) {
	t.Helper()
	raw, err := json.Marshal(&JWKS{Keys: keys})
	require.NoError(t, err)
	fakeResolver(t, svc).seed(accountID, clusterName, raw)
}

func signWith(t *testing.T, priv *ecdsa.PrivateKey, kid string, claims jwt.MapClaims) string {
	t.Helper()
	tok := jwt.NewWithClaims(jwt.SigningMethodES256, claims)
	tok.Header["kid"] = kid
	s, err := tok.SignedString(priv)
	require.NoError(t, err)
	return s
}

func requireInvalidIdentityToken(t *testing.T, out *sts.AssumeRoleWithWebIdentityOutput, err error) {
	t.Helper()
	require.Error(t, err)
	assert.Nil(t, out)
	assert.Equal(t, awserrors.ErrorInvalidIdentityToken, err.Error())
}

// The IAM provider-registration check runs before any JWKS resolver call, so
// an unregistered issuer never reaches the resolver.
func TestCurrentBehaviour_UnregisteredIssuerNeverCallsResolver(t *testing.T) {
	svc, _ := newTestSetup(t)
	f := newWebIdentityFixture(t, svc, testCallerAccountID)
	role := charRole(t, svc, f)

	claims := f.defaultClaims()
	claims["iss"] = charIssuer(testCallerAccountID, "unregistered")
	out, err := assumeWebIdentity(svc, role, f.signToken(claims))
	requireInvalidIdentityToken(t, out, err)

	assert.Empty(t, fakeResolver(t, svc).lookups(), "resolver must not be called before issuer registration is checked")
}

// JWKS is resolved against the issuer's account, not the role's account.
func TestCurrentBehaviour_CrossAccountIssuerQueriesIssuerAccount(t *testing.T) {
	svc, _ := newTestSetup(t)
	const issuerAccount = "111122223333"
	issuer := charIssuer(issuerAccount, "remote")
	registerIssuer(t, svc, testCallerAccountID, issuer)
	priv, jwk := newKeyJWK(t, "remote-kid")
	seedJWKS(t, svc, issuerAccount, "remote", jwk)

	federated := iamdomain.OIDCProviderARN(testCallerAccountID, strings.TrimPrefix(issuer, "https://"))
	role := createRoleForFixture(t, svc, testCallerAccountID, "irsa-xacct", webIdentityTrustPolicyNoCondition(federated))

	f := &webIdentityFixture{issuer: issuer}
	out, err := assumeWebIdentity(svc, role, signWith(t, priv, "remote-kid", f.defaultClaims()))
	require.NoError(t, err)
	require.NotNil(t, out.Credentials)

	assert.True(t, fakeResolver(t, svc).calledWith(issuerAccount, "remote"),
		"resolver must be queried with the issuer's account, not the role's")
	assert.False(t, fakeResolver(t, svc).calledWith(testCallerAccountID, "remote"))
}

// A resolver "not found" result — whatever storage-layer reason produced it,
// pinned separately in the EKS package — fails closed as InvalidIdentityToken.
func TestCurrentBehaviour_ClusterMissingIsInvalidIdentityToken(t *testing.T) {
	svc, _ := newTestSetup(t)
	f := newWebIdentityFixture(t, svc, testCallerAccountID)
	role := charRole(t, svc, f)

	issuer := charIssuer(testCallerAccountID, "ghost")
	registerIssuer(t, svc, testCallerAccountID, issuer)
	claims := f.defaultClaims()
	claims["iss"] = issuer
	out, err := assumeWebIdentity(svc, role, f.signToken(claims))
	requireInvalidIdentityToken(t, out, err)
}

// Every malformed JWKS value fails closed with the same error code, whether
// the failure is FetchClusterJWKS's own JSON decode or the later JWK-to-key
// conversion.
func TestCurrentBehaviour_JWKSMalformedIsInvalidIdentityToken(t *testing.T) {
	cases := map[string]string{
		"not_json":    `not-json`,
		"json_null":   `null`,
		"empty_keys":  `{"keys":[]}`,
		"wrong_shape": `{"keys":"x"}`,
		"bad_coords":  `{"keys":[{"kty":"EC","kid":"test-kid-1","crv":"P-256","x":"!!","y":"!!"}]}`,
		"rsa_kty":     `{"keys":[{"kty":"RSA","kid":"test-kid-1","n":"AQAB","e":"AQAB"}]}`,
	}
	for name, value := range cases {
		t.Run(name, func(t *testing.T) {
			svc, _ := newTestSetup(t)
			f := newWebIdentityFixture(t, svc, testCallerAccountID)
			role := charRole(t, svc, f)
			fakeResolver(t, svc).seed(f.accountID, f.clusterName, []byte(value))

			out, err := assumeWebIdentity(svc, role, f.signToken(f.defaultClaims()))
			requireInvalidIdentityToken(t, out, err)
		})
	}
}

// Any resolver dependency failure (KV unavailable, timeout, ...) maps to the
// same InvalidIdentityToken as a not-found result; STS cannot distinguish
// them without leaking internal storage detail to an anonymous caller.
func TestCurrentBehaviour_ResolverErrorIsInvalidIdentityToken(t *testing.T) {
	svc, _ := newTestSetup(t)
	f := newWebIdentityFixture(t, svc, testCallerAccountID)
	role := charRole(t, svc, f)
	fakeResolver(t, svc).seedErr(f.accountID, f.clusterName, context.DeadlineExceeded)

	out, err := assumeWebIdentity(svc, role, f.signToken(f.defaultClaims()))
	requireInvalidIdentityToken(t, out, err)
}

// No caching: each call re-reads the JWKS, so an overwrite takes effect on the
// next request and a key dropped from the set stops verifying immediately.
func TestCurrentBehaviour_JWKSRotationIsReadFreshEachRequest(t *testing.T) {
	svc, _ := newTestSetup(t)
	f := newWebIdentityFixture(t, svc, testCallerAccountID)
	role := charRole(t, svc, f)
	oldJWK := f.jwk
	oldToken := f.signToken(f.defaultClaims())
	newPriv, newJWK := newKeyJWK(t, "rotated-kid")
	newToken := signWith(t, newPriv, "rotated-kid", f.defaultClaims())

	_, err := assumeWebIdentity(svc, role, oldToken)
	require.NoError(t, err)
	out, err := assumeWebIdentity(svc, role, newToken)
	requireInvalidIdentityToken(t, out, err)

	// Both keys published: either verifies.
	seedJWKS(t, svc, f.accountID, f.clusterName, oldJWK, newJWK)
	_, err = assumeWebIdentity(svc, role, oldToken)
	require.NoError(t, err)
	_, err = assumeWebIdentity(svc, role, newToken)
	require.NoError(t, err)

	// Old key dropped: the old token fails on the very next request.
	seedJWKS(t, svc, f.accountID, f.clusterName, newJWK)
	out, err = assumeWebIdentity(svc, role, oldToken)
	requireInvalidIdentityToken(t, out, err)
	_, err = assumeWebIdentity(svc, role, newToken)
	require.NoError(t, err)
}
