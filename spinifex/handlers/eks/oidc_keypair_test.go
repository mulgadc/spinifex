package handlers_eks

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"testing"

	"github.com/nats-io/nats.go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

var testMasterKey = func() []byte {
	k := make([]byte, 32)
	if _, err := rand.Read(k); err != nil {
		panic(err)
	}
	return k
}()

func TestGenerateClusterOIDCKeypair_PersistsJWKSAndEncryptedPrivateKey(t *testing.T) {
	kv := newClusterStateTestKV(t)

	_, jwksBytes, err := GenerateClusterOIDCKeypair(kv, "alpha", testMasterKey)
	require.NoError(t, err)
	require.NotEmpty(t, jwksBytes)

	jwksEntry, err := kv.Get(OIDCJWKSKey("alpha"))
	require.NoError(t, err)
	assert.Equal(t, jwksBytes, jwksEntry.Value())

	keyEntry, err := kv.Get(OIDCSigningKeyKey("alpha"))
	require.NoError(t, err)
	assert.NotEmpty(t, keyEntry.Value())
	assert.NotContains(t, string(keyEntry.Value()), "BEGIN PRIVATE KEY",
		"encrypted blob must not contain a PEM header")
}

// The generator returns the plaintext private-key PEM directly so CreateCluster
// avoids a second KV read + decrypt; it must match the key persisted in KV.
func TestGenerateClusterOIDCKeypair_ReturnsPrivateKeyPEMMatchingStored(t *testing.T) {
	kv := newClusterStateTestKV(t)

	privPEM, _, err := GenerateClusterOIDCKeypair(kv, "alpha", testMasterKey)
	require.NoError(t, err)
	require.Contains(t, privPEM, "BEGIN PRIVATE KEY", "returned PEM must be plaintext")

	block, _ := pem.Decode([]byte(privPEM))
	require.NotNil(t, block)
	returned, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	require.NoError(t, err)
	returnedEC, ok := returned.(*ecdsa.PrivateKey)
	require.True(t, ok)

	stored, err := LoadClusterOIDCPrivateKey(kv, "alpha", testMasterKey)
	require.NoError(t, err)
	assert.True(t, stored.Equal(returnedEC), "returned PEM must match the persisted key")
}

func TestGenerateClusterOIDCKeypair_JWKSShapeIsRFC7517EC_P256(t *testing.T) {
	kv := newClusterStateTestKV(t)

	_, jwksBytes, err := GenerateClusterOIDCKeypair(kv, "alpha", testMasterKey)
	require.NoError(t, err)

	var doc oidcJWKS
	require.NoError(t, json.Unmarshal(jwksBytes, &doc))
	require.Len(t, doc.Keys, 1)

	k := doc.Keys[0]
	assert.Equal(t, "EC", k.Kty)
	assert.Equal(t, "P-256", k.Crv)
	assert.Equal(t, "ES256", k.Alg)
	assert.Equal(t, "sig", k.Use)
	assert.NotEmpty(t, k.Kid)

	xBytes, err := base64.RawURLEncoding.DecodeString(k.X)
	require.NoError(t, err)
	assert.Len(t, xBytes, p256CoordLen, "JWK x must be 32-byte SEC1 coordinate")
	yBytes, err := base64.RawURLEncoding.DecodeString(k.Y)
	require.NoError(t, err)
	assert.Len(t, yBytes, p256CoordLen, "JWK y must be 32-byte SEC1 coordinate")
}

func TestGenerateClusterOIDCKeypair_TwoCallsProduceDistinctKeys(t *testing.T) {
	kv := newClusterStateTestKV(t)

	_, a, err := GenerateClusterOIDCKeypair(kv, "alpha", testMasterKey)
	require.NoError(t, err)
	_, b, err := GenerateClusterOIDCKeypair(kv, "beta", testMasterKey)
	require.NoError(t, err)
	assert.NotEqual(t, a, b, "JWKS for distinct clusters must differ")
}

func TestGenerateClusterOIDCKeypair_EmptyArgsRejected(t *testing.T) {
	kv := newClusterStateTestKV(t)

	_, _, err := GenerateClusterOIDCKeypair(kv, "", testMasterKey)
	require.Error(t, err)
	_, _, err = GenerateClusterOIDCKeypair(kv, "alpha", nil)
	require.Error(t, err)
}

func TestLoadClusterOIDCPrivateKey_RoundTripMatchesJWKSPublic(t *testing.T) {
	kv := newClusterStateTestKV(t)

	_, jwksBytes, err := GenerateClusterOIDCKeypair(kv, "alpha", testMasterKey)
	require.NoError(t, err)

	priv, err := LoadClusterOIDCPrivateKey(kv, "alpha", testMasterKey)
	require.NoError(t, err)
	require.NotNil(t, priv)
	assert.Equal(t, elliptic.P256(), priv.Curve)

	var doc oidcJWKS
	require.NoError(t, json.Unmarshal(jwksBytes, &doc))
	require.Len(t, doc.Keys, 1)

	xExpected, err := base64.RawURLEncoding.DecodeString(doc.Keys[0].X)
	require.NoError(t, err)
	yExpected, err := base64.RawURLEncoding.DecodeString(doc.Keys[0].Y)
	require.NoError(t, err)

	assert.Equal(t, padCoord(priv.X.Bytes()), xExpected)
	assert.Equal(t, padCoord(priv.Y.Bytes()), yExpected)
}

func TestLoadClusterOIDCPrivateKey_WrongMasterKeyFails(t *testing.T) {
	kv := newClusterStateTestKV(t)
	_, _, err := GenerateClusterOIDCKeypair(kv, "alpha", testMasterKey)
	require.NoError(t, err)

	other := make([]byte, 32)
	other[0] = 0xff
	_, err = LoadClusterOIDCPrivateKey(kv, "alpha", other)
	require.Error(t, err)
}

func TestLoadClusterOIDCPrivateKey_MissingReturnsErrClusterNotFound(t *testing.T) {
	kv := newClusterStateTestKV(t)
	_, err := LoadClusterOIDCPrivateKey(kv, "ghost", testMasterKey)
	require.ErrorIs(t, err, ErrClusterNotFound)
}

func TestZeroizeClusterOIDCKey_DeletesKeyAndLeavesSiblingsIntact(t *testing.T) {
	kv := newClusterStateTestKV(t)
	_, _, err := GenerateClusterOIDCKeypair(kv, "alpha", testMasterKey)
	require.NoError(t, err)

	require.NoError(t, ZeroizeClusterOIDCKey(kv, "alpha"))

	_, err = kv.Get(OIDCSigningKeyKey("alpha"))
	assert.True(t, errors.Is(err, nats.ErrKeyNotFound))

	jwks, err := kv.Get(OIDCJWKSKey("alpha"))
	require.NoError(t, err, "JWKS must survive zeroize (it carries no secret material)")
	assert.NotEmpty(t, jwks.Value())
}

func TestZeroizeClusterOIDCKey_MissingIsNoop(t *testing.T) {
	kv := newClusterStateTestKV(t)
	require.NoError(t, ZeroizeClusterOIDCKey(kv, "ghost"))
}

func TestZeroizeClusterOIDCKey_EmptyNameRejected(t *testing.T) {
	kv := newClusterStateTestKV(t)
	require.Error(t, ZeroizeClusterOIDCKey(kv, ""))
}

func TestMarshalJWKS_DeterministicForSameKey(t *testing.T) {
	priv, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	require.NoError(t, err)

	a, err := marshalJWKS(&priv.PublicKey)
	require.NoError(t, err)
	b, err := marshalJWKS(&priv.PublicKey)
	require.NoError(t, err)
	assert.Equal(t, a, b)
}

func TestPadCoord_LeftPadsShortBigIntBytes(t *testing.T) {
	got := padCoord([]byte{0x01, 0x02})
	require.Len(t, got, p256CoordLen)
	assert.Equal(t, byte(0x01), got[p256CoordLen-2])
	assert.Equal(t, byte(0x02), got[p256CoordLen-1])
	for _, b := range got[:p256CoordLen-2] {
		assert.Zero(t, b)
	}
}
