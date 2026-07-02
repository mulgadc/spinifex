package gateway_ecrauth

import (
	"crypto/ecdsa"
	"crypto/rand"
	"strings"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/mulgadc/spinifex/spinifex/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const testAudience = "ecr.ap-southeast-2.spinifex.internal"

var testMasterKey = func() []byte {
	k := make([]byte, 32)
	if _, err := rand.Read(k); err != nil {
		panic(err)
	}
	return k
}()

func samplePrincipal() Principal {
	return Principal{
		AccountID:   "000000000001",
		ARN:         "arn:aws:iam::000000000001:user/dev",
		Type:        "IAMUser",
		AccessKeyID: "AKIAVAOB203DGNBR04XP",
	}
}

func TestLoadOrCreateSigningKey_CreatesThenReloads(t *testing.T) {
	_, _, js := testutil.StartTestJetStream(t)

	key1, verify1, err := LoadOrCreateSigningKey(js, testMasterKey, 1)
	require.NoError(t, err)
	require.NotEmpty(t, key1.Kid)
	require.Contains(t, verify1, key1.Kid)

	// Second call must reload the same persisted key, not mint a new one.
	key2, verify2, err := LoadOrCreateSigningKey(js, testMasterKey, 1)
	require.NoError(t, err)
	assert.Equal(t, key1.Kid, key2.Kid, "persisted signing key must be reused")
	assert.Len(t, verify2, 1)
}

func TestLoadOrCreateSigningKey_StoresEncrypted(t *testing.T) {
	_, _, js := testutil.StartTestJetStream(t)
	key, _, err := LoadOrCreateSigningKey(js, testMasterKey, 1)
	require.NoError(t, err)

	kv, err := js.KeyValue(SigningBucket)
	require.NoError(t, err)
	entry, err := kv.Get(signingKeyName(key.Kid))
	require.NoError(t, err)
	assert.NotContains(t, string(entry.Value()), "PRIVATE KEY",
		"signing key must be encrypted at rest")
}

func TestLoadOrCreateSigningKey_EmptyMasterKeyRejected(t *testing.T) {
	_, _, js := testutil.StartTestJetStream(t)
	_, _, err := LoadOrCreateSigningKey(js, nil, 1)
	require.Error(t, err)
}

func TestIssuerVerifier_RoundTrip(t *testing.T) {
	_, _, js := testutil.StartTestJetStream(t)
	key, verify, err := LoadOrCreateSigningKey(js, testMasterKey, 1)
	require.NoError(t, err)

	iss := NewIssuer(key, testAudience)
	tok, exp, err := iss.Mint(samplePrincipal())
	require.NoError(t, err)
	assert.WithinDuration(t, time.Now().Add(DefaultTokenTTL), exp, time.Minute)

	claims, err := NewVerifier(verify, testAudience).Verify(tok)
	require.NoError(t, err)
	assert.Equal(t, "000000000001", claims.AccountID)
	assert.Equal(t, "arn:aws:iam::000000000001:user/dev", claims.Subject)
	assert.Equal(t, "IAMUser", claims.PrincipalType)
}

func TestIssuer_MintRequiresAccountID(t *testing.T) {
	_, _, js := testutil.StartTestJetStream(t)
	key, _, err := LoadOrCreateSigningKey(js, testMasterKey, 1)
	require.NoError(t, err)

	p := samplePrincipal()
	p.AccountID = ""
	_, _, err = NewIssuer(key, testAudience).Mint(p)
	require.Error(t, err)
}

func TestVerifier_RejectsWrongAudience(t *testing.T) {
	_, _, js := testutil.StartTestJetStream(t)
	key, verify, err := LoadOrCreateSigningKey(js, testMasterKey, 1)
	require.NoError(t, err)

	tok, _, err := NewIssuer(key, testAudience).Mint(samplePrincipal())
	require.NoError(t, err)

	_, err = NewVerifier(verify, "ecr.us-east-1.spinifex.internal").Verify(tok)
	require.Error(t, err)
}

func TestVerifier_RejectsUnknownKid(t *testing.T) {
	_, _, js := testutil.StartTestJetStream(t)
	key, _, err := LoadOrCreateSigningKey(js, testMasterKey, 1)
	require.NoError(t, err)

	tok, _, err := NewIssuer(key, testAudience).Mint(samplePrincipal())
	require.NoError(t, err)

	// Verifier with an empty key set cannot resolve the kid.
	_, err = NewVerifier(map[string]*ecdsa.PublicKey{}, testAudience).Verify(tok)
	require.Error(t, err)
}

func TestVerifier_RejectsExpiredToken(t *testing.T) {
	_, _, js := testutil.StartTestJetStream(t)
	key, verify, err := LoadOrCreateSigningKey(js, testMasterKey, 1)
	require.NoError(t, err)

	// Hand-mint an already-expired token with the same signing key.
	claims := Claims{
		RegisteredClaims: jwt.RegisteredClaims{
			Issuer:    tokenIssuer,
			Audience:  jwt.ClaimStrings{testAudience},
			IssuedAt:  jwt.NewNumericDate(time.Now().Add(-2 * time.Hour)),
			ExpiresAt: jwt.NewNumericDate(time.Now().Add(-time.Hour)),
		},
		AccountID: "000000000001",
	}
	raw := jwt.NewWithClaims(jwt.SigningMethodES256, claims)
	raw.Header["kid"] = key.Kid
	signed, err := raw.SignedString(key.priv)
	require.NoError(t, err)

	_, err = NewVerifier(verify, testAudience).Verify(signed)
	require.Error(t, err)
}

func TestVerifier_RejectsNonES256(t *testing.T) {
	_, _, js := testutil.StartTestJetStream(t)
	_, verify, err := LoadOrCreateSigningKey(js, testMasterKey, 1)
	require.NoError(t, err)

	// HS256 token must be refused regardless of signature.
	claims := Claims{
		RegisteredClaims: jwt.RegisteredClaims{
			Issuer:    tokenIssuer,
			Audience:  jwt.ClaimStrings{testAudience},
			ExpiresAt: jwt.NewNumericDate(time.Now().Add(time.Hour)),
		},
		AccountID: "000000000001",
	}
	raw := jwt.NewWithClaims(jwt.SigningMethodHS256, claims)
	signed, err := raw.SignedString([]byte(strings.Repeat("k", 32)))
	require.NoError(t, err)

	_, err = NewVerifier(verify, testAudience).Verify(signed)
	require.Error(t, err)
}
