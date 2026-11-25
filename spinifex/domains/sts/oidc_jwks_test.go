package sts

import (
	"encoding/json"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestJWKS_FindByKID(t *testing.T) {
	t.Parallel()

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
	t.Parallel()

	jwks, err := FetchClusterJWKS(t.Context(), newFakeJWKSResolver(), "123456789012", "missing-cluster")
	require.NoError(t, err)
	assert.Nil(t, jwks)
}

func TestFetchClusterJWKS_RoundTrip(t *testing.T) {
	t.Parallel()

	want := &JWKS{Keys: []JWK{
		{Kid: "abc123", Kty: "EC", Crv: "P-256", Alg: "ES256", Use: "sig", X: "x-coord", Y: "y-coord"},
	}}
	raw, err := json.Marshal(want)
	require.NoError(t, err)

	resolver := newFakeJWKSResolver()
	resolver.seed("123456789012", "my-cluster", raw)
	got, err := FetchClusterJWKS(t.Context(), resolver, "123456789012", "my-cluster")
	require.NoError(t, err)
	require.NotNil(t, got)
	require.Len(t, got.Keys, 1)
	assert.Equal(t, "abc123", got.Keys[0].Kid)
	assert.Equal(t, "EC", got.Keys[0].Kty)
	assert.Equal(t, "ES256", got.Keys[0].Alg)
}

func TestFetchClusterJWKS_EmptyKeysIsError(t *testing.T) {
	t.Parallel()

	resolver := newFakeJWKSResolver()
	resolver.seed("123456789012", "empty-cluster", []byte(`{"keys":[]}`))
	jwks, err := FetchClusterJWKS(t.Context(), resolver, "123456789012", "empty-cluster")
	require.Error(t, err)
	assert.Nil(t, jwks)
}

// Malformed stored bytes fail JSON decode; this is the STS-owned half of the
// error semantics, the resolver's half is pinned in the EKS package.
func TestFetchClusterJWKS_MalformedIsDecodeError(t *testing.T) {
	t.Parallel()

	resolver := newFakeJWKSResolver()
	resolver.seed("123456789012", "my-cluster", []byte(`not-json`))
	jwks, err := FetchClusterJWKS(t.Context(), resolver, "123456789012", "my-cluster")
	require.Error(t, err)
	assert.Nil(t, jwks)
	assert.Contains(t, err.Error(), "decode JWKS for cluster my-cluster")
}

// A resolver dependency failure is returned unchanged, not wrapped or
// swallowed: the caller sees the exact error value the resolver raised.
func TestFetchClusterJWKS_PropagatesResolverErrorUnchanged(t *testing.T) {
	t.Parallel()

	resolverErr := errors.New("kv dependency failure")
	resolver := newFakeJWKSResolver()
	resolver.seedErr("123456789012", "my-cluster", resolverErr)

	jwks, err := FetchClusterJWKS(t.Context(), resolver, "123456789012", "my-cluster")
	require.Error(t, err)
	assert.Nil(t, jwks)
	assert.Same(t, resolverErr, err, "FetchClusterJWKS must return the resolver's error unchanged, not wrapped")
	require.ErrorIs(t, err, resolverErr)
}

func TestFetchClusterJWKS_NilResolver(t *testing.T) {
	t.Parallel()

	_, err := FetchClusterJWKS(t.Context(), nil, "123456789012", "c1")
	require.Error(t, err)
}

func TestFetchClusterJWKS_EmptyArgs(t *testing.T) {
	t.Parallel()

	resolver := newFakeJWKSResolver()
	_, err := FetchClusterJWKS(t.Context(), resolver, "", "c1")
	require.Error(t, err)
	_, err = FetchClusterJWKS(t.Context(), resolver, "123456789012", "")
	require.Error(t, err)
}

// Sanity check that JWK JSON round-trips through the type without dropping
// the optional RSA fields. The EKS-issued keys are EC P-256, but the decoder
// must remain permissive so a non-EKS issuer registered later still parses.
func TestJWK_RSAFieldsRoundTrip(t *testing.T) {
	t.Parallel()

	src := JWK{Kty: "RSA", Kid: "rsa-1", Use: "sig", Alg: "RS256", N: "modulus-base64", E: "AQAB"}
	raw, err := json.Marshal(src)
	require.NoError(t, err)
	var got JWK
	require.NoError(t, json.Unmarshal(raw, &got))
	assert.Equal(t, src, got)
}
