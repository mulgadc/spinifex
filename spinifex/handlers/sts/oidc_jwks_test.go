package handlers_sts

import (
	"encoding/json"
	"testing"

	handlers_eks "github.com/mulgadc/spinifex/spinifex/handlers/eks"
	"github.com/mulgadc/spinifex/spinifex/testutil"
	"github.com/nats-io/nats.go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestParseEKSIssuerURL_Valid(t *testing.T) {
	accountID, clusterName, err := ParseEKSIssuerURL("https://10.0.0.1:9999/oidc/eks/au-mel-1/123456789012/my-cluster")
	require.NoError(t, err)
	assert.Equal(t, "123456789012", accountID)
	assert.Equal(t, "my-cluster", clusterName)
}

// Cross-component contract: a token whose iss is built by ClusterOIDCIssuer
// must parse cleanly via ParseEKSIssuerURL and resolve to the same
// (accountID, clusterName). This is the collision that bead 165.11 fixes.
func TestParseEKSIssuerURL_RoundTripsClusterOIDCIssuer(t *testing.T) {
	issuer, err := handlers_eks.ClusterOIDCIssuer("https://10.0.0.1:9999", "au-mel-1", "123456789012", "my-cluster")
	require.NoError(t, err)

	accountID, clusterName, err := ParseEKSIssuerURL(issuer)
	require.NoError(t, err)
	assert.Equal(t, "123456789012", accountID)
	assert.Equal(t, "my-cluster", clusterName)
}

func TestParseEKSIssuerURL_Invalid(t *testing.T) {
	cases := []struct {
		name   string
		issuer string
	}{
		{"http scheme", "http://10.0.0.1:9999/oidc/eks/au-mel-1/123456789012/c1"},
		{"missing host", "https:///oidc/eks/au-mel-1/123456789012/c1"},
		{"wrong path prefix", "https://10.0.0.1:9999/foo/eks/au-mel-1/123456789012/c1"},
		{"too few segments", "https://10.0.0.1:9999/oidc/eks/au-mel-1/123456789012"},
		{"too many segments", "https://10.0.0.1:9999/oidc/eks/au-mel-1/123456789012/c1/extra"},
		{"empty account segment", "https://10.0.0.1:9999/oidc/eks/au-mel-1//c1"},
		{"empty cluster segment", "https://10.0.0.1:9999/oidc/eks/au-mel-1/123456789012/"},
		{"empty path", "https://10.0.0.1:9999"},
		{"legacy aws two-segment shape", "https://oidc.eks.au-mel-1.mulga.local/123456789012/c1"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, _, err := ParseEKSIssuerURL(tc.issuer)
			require.Error(t, err)
		})
	}
}

func TestJWKS_FindByKID(t *testing.T) {
	jwks := &JWKS{Keys: []JWK{
		{Kid: "key-a", Kty: "EC", Crv: "P-256", X: "x", Y: "y"},
		{Kid: "key-b", Kty: "EC", Crv: "P-256", X: "x2", Y: "y2"},
	}}
	got := jwks.FindByKID("key-b")
	require.NotNil(t, got)
	assert.Equal(t, "key-b", got.Kid)

	assert.Nil(t, jwks.FindByKID("missing"))
	var nilJWKS *JWKS
	assert.Nil(t, nilJWKS.FindByKID("anything"))
}

func TestFetchClusterJWKS_NotFoundReturnsNil(t *testing.T) {
	_, _, js := testutil.StartTestJetStream(t)

	jwks, err := FetchClusterJWKS(js, "123456789012", "missing-cluster")
	require.NoError(t, err)
	assert.Nil(t, jwks)
}

func TestFetchClusterJWKS_RoundTrip(t *testing.T) {
	_, _, js := testutil.StartTestJetStream(t)

	kv, err := handlers_eks.GetOrCreateAccountBucket(js, "123456789012")
	require.NoError(t, err)

	want := &JWKS{Keys: []JWK{
		{Kid: "abc123", Kty: "EC", Crv: "P-256", Alg: "ES256", Use: "sig", X: "x-coord", Y: "y-coord"},
	}}
	raw, err := json.Marshal(want)
	require.NoError(t, err)
	_, err = kv.Put(handlers_eks.OIDCJWKSKey("my-cluster"), raw)
	require.NoError(t, err)

	got, err := FetchClusterJWKS(js, "123456789012", "my-cluster")
	require.NoError(t, err)
	require.NotNil(t, got)
	require.Len(t, got.Keys, 1)
	assert.Equal(t, "abc123", got.Keys[0].Kid)
	assert.Equal(t, "EC", got.Keys[0].Kty)
	assert.Equal(t, "ES256", got.Keys[0].Alg)
}

func TestFetchClusterJWKS_EmptyKeysIsError(t *testing.T) {
	_, _, js := testutil.StartTestJetStream(t)

	kv, err := handlers_eks.GetOrCreateAccountBucket(js, "123456789012")
	require.NoError(t, err)

	_, err = kv.Put(handlers_eks.OIDCJWKSKey("empty-cluster"), []byte(`{"keys":[]}`))
	require.NoError(t, err)

	jwks, err := FetchClusterJWKS(js, "123456789012", "empty-cluster")
	require.Error(t, err)
	assert.Nil(t, jwks)
}

func TestFetchClusterJWKS_NilJetStream(t *testing.T) {
	_, err := FetchClusterJWKS(nil, "123456789012", "c1")
	require.Error(t, err)
}

func TestFetchClusterJWKS_EmptyArgs(t *testing.T) {
	_, _, js := testutil.StartTestJetStream(t)
	_, err := FetchClusterJWKS(js, "", "c1")
	require.Error(t, err)
	_, err = FetchClusterJWKS(js, "123456789012", "")
	require.Error(t, err)
}

// Sanity check that JWK JSON round-trips through the type without dropping
// the optional RSA fields. The EKS-issued keys are EC P-256, but the decoder
// must remain permissive so a non-EKS issuer registered later still parses.
func TestJWK_RSAFieldsRoundTrip(t *testing.T) {
	src := JWK{Kty: "RSA", Kid: "rsa-1", Use: "sig", Alg: "RS256", N: "modulus-base64", E: "AQAB"}
	raw, err := json.Marshal(src)
	require.NoError(t, err)
	var got JWK
	require.NoError(t, json.Unmarshal(raw, &got))
	assert.Equal(t, src, got)
}

// Ensure that a NATS error other than ErrBucketNotFound / ErrKeyNotFound is
// surfaced rather than masked. Without an injected JetStream stub there's no
// fault-injection path here, so the negative-cache cases above cover the two
// expected misses; this test documents the contract by exercising the nil
// pre-conditions only.
var _ = nats.ErrBucketNotFound
