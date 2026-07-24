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

// samplePrincipal returns a Principal shaped like the gateway's user
// principalContext. Type uses the same literal ("user") the gateway package's
// principalTypeUser constant holds; this package can't import gateway to
// reference the constant directly without cycling.
func samplePrincipal() Principal {
	return Principal{
		AccountID:   "000000000001",
		ARN:         "arn:aws:iam::000000000001:user/dev",
		Type:        "user",
		AccessKeyID: "AKIAVAOB203DGNBR04XP",
	}
}

func TestLoadOrCreateSigningKey_CreatesThenReloads(t *testing.T) {
	_, nc, _ := testutil.StartTestJetStream(t)
	js := testutil.NewJetStream(t, nc)

	key1, verify1, err := LoadOrCreateSigningKey(t.Context(), js, testMasterKey, 1)
	require.NoError(t, err)
	require.NotEmpty(t, key1.Kid)
	require.Contains(t, verify1, key1.Kid)

	// Second call must reload the same persisted key, not mint a new one.
	key2, verify2, err := LoadOrCreateSigningKey(t.Context(), js, testMasterKey, 1)
	require.NoError(t, err)
	assert.Equal(t, key1.Kid, key2.Kid, "persisted signing key must be reused")
	assert.Len(t, verify2, 1)
}

func TestLoadOrCreateSigningKey_StoresEncrypted(t *testing.T) {
	_, nc, _ := testutil.StartTestJetStream(t)
	js := testutil.NewJetStream(t, nc)
	key, _, err := LoadOrCreateSigningKey(t.Context(), js, testMasterKey, 1)
	require.NoError(t, err)

	kv, err := js.KeyValue(t.Context(), SigningBucket)
	require.NoError(t, err)
	entry, err := kv.Get(t.Context(), signingKeyName(key.Kid))
	require.NoError(t, err)
	assert.NotContains(t, string(entry.Value()), "PRIVATE KEY",
		"signing key must be encrypted at rest")
}

func TestLoadOrCreateSigningKey_EmptyMasterKeyRejected(t *testing.T) {
	_, nc, _ := testutil.StartTestJetStream(t)
	js := testutil.NewJetStream(t, nc)
	_, _, err := LoadOrCreateSigningKey(t.Context(), js, nil, 1)
	require.Error(t, err)
}

func TestIssuerVerifier_RoundTrip(t *testing.T) {
	_, nc, _ := testutil.StartTestJetStream(t)
	js := testutil.NewJetStream(t, nc)
	key, verify, err := LoadOrCreateSigningKey(t.Context(), js, testMasterKey, 1)
	require.NoError(t, err)

	iss := NewIssuer(key, testAudience)
	tok, exp, err := iss.Mint(samplePrincipal())
	require.NoError(t, err)
	assert.WithinDuration(t, exp, time.Now().Add(DefaultTokenTTL), time.Minute)

	claims, err := NewVerifier(verify, testAudience).Verify(tok)
	require.NoError(t, err)
	assert.Equal(t, "000000000001", claims.AccountID)
	assert.Equal(t, "arn:aws:iam::000000000001:user/dev", claims.Subject)
	assert.Equal(t, "user", claims.PrincipalType)
}

func TestIssuer_MintRequiresAccountID(t *testing.T) {
	_, nc, _ := testutil.StartTestJetStream(t)
	js := testutil.NewJetStream(t, nc)
	key, _, err := LoadOrCreateSigningKey(t.Context(), js, testMasterKey, 1)
	require.NoError(t, err)

	p := samplePrincipal()
	p.AccountID = ""
	_, _, err = NewIssuer(key, testAudience).Mint(p)
	require.Error(t, err)
}

// TestMint_RequiresCompleteIdentityPointer pins the plan's claim contract: a
// token minted with any field missing from the identity pointer (ARN,
// accessKeyID, an unsupported principalType) must fail to mint at all, since
// a token that can't name a lookup key can never be safely rehydrated.
func TestMint_RequiresCompleteIdentityPointer(t *testing.T) {
	_, nc, _ := testutil.StartTestJetStream(t)
	js := testutil.NewJetStream(t, nc)
	key, _, err := LoadOrCreateSigningKey(t.Context(), js, testMasterKey, 1)
	require.NoError(t, err)
	iss := NewIssuer(key, testAudience)

	cases := []struct {
		name   string
		mutate func(p Principal) Principal
	}{
		{"missing ARN", func(p Principal) Principal { p.ARN = ""; return p }},
		{"missing accessKeyID", func(p Principal) Principal { p.AccessKeyID = ""; return p }},
		{"unsupported principalType", func(p Principal) Principal { p.Type = "IAMUser"; return p }},
		{"empty principalType", func(p Principal) Principal { p.Type = ""; return p }},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, _, err := iss.Mint(c.mutate(samplePrincipal()))
			require.Error(t, err)
		})
	}
}

// TestSupportedPrincipalType pins the exact set of principalType claim values
// Verify accepts, mirrored from the gateway package's principalType constants.
func TestSupportedPrincipalType(t *testing.T) {
	assert.True(t, SupportedPrincipalType("user"))
	assert.True(t, SupportedPrincipalType("assumed-role"))
	assert.True(t, SupportedPrincipalType("root"))
	assert.False(t, SupportedPrincipalType("IAMUser"))
	assert.False(t, SupportedPrincipalType(""))
}

func TestVerifier_RejectsWrongAudience(t *testing.T) {
	_, nc, _ := testutil.StartTestJetStream(t)
	js := testutil.NewJetStream(t, nc)
	key, verify, err := LoadOrCreateSigningKey(t.Context(), js, testMasterKey, 1)
	require.NoError(t, err)

	tok, _, err := NewIssuer(key, testAudience).Mint(samplePrincipal())
	require.NoError(t, err)

	_, err = NewVerifier(verify, "ecr.us-east-1.spinifex.internal").Verify(tok)
	require.Error(t, err)
}

func TestVerifier_RejectsUnknownKid(t *testing.T) {
	_, nc, _ := testutil.StartTestJetStream(t)
	js := testutil.NewJetStream(t, nc)
	key, _, err := LoadOrCreateSigningKey(t.Context(), js, testMasterKey, 1)
	require.NoError(t, err)

	tok, _, err := NewIssuer(key, testAudience).Mint(samplePrincipal())
	require.NoError(t, err)

	// Verifier with an empty key set cannot resolve the kid.
	_, err = NewVerifier(map[string]*ecdsa.PublicKey{}, testAudience).Verify(tok)
	require.Error(t, err)
}

func TestVerifier_RejectsExpiredToken(t *testing.T) {
	_, nc, _ := testutil.StartTestJetStream(t)
	js := testutil.NewJetStream(t, nc)
	key, verify, err := LoadOrCreateSigningKey(t.Context(), js, testMasterKey, 1)
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

// TestVerifier_RejectsIncompleteIdentityPointer hand-mints tokens that skip
// Mint's own checks, proving Verify itself refuses a claim set missing any
// part of the identity pointer even if it were signed by some other path.
func TestVerifier_RejectsIncompleteIdentityPointer(t *testing.T) {
	_, nc, _ := testutil.StartTestJetStream(t)
	js := testutil.NewJetStream(t, nc)
	key, verify, err := LoadOrCreateSigningKey(t.Context(), js, testMasterKey, 1)
	require.NoError(t, err)

	base := func() Claims {
		return Claims{
			RegisteredClaims: jwt.RegisteredClaims{
				Issuer:    tokenIssuer,
				Audience:  jwt.ClaimStrings{testAudience},
				Subject:   "arn:aws:iam::000000000001:user/dev",
				ExpiresAt: jwt.NewNumericDate(time.Now().Add(time.Hour)),
			},
			AccountID:     "000000000001",
			PrincipalType: "user",
			AccessKeyID:   "AKIAVAOB203DGNBR04XP",
		}
	}
	sign := func(t *testing.T, c Claims) string {
		t.Helper()
		raw := jwt.NewWithClaims(jwt.SigningMethodES256, c)
		raw.Header["kid"] = key.Kid
		signed, err := raw.SignedString(key.priv)
		require.NoError(t, err)
		return signed
	}

	cases := []struct {
		name   string
		mutate func(c Claims) Claims
	}{
		{"missing subject", func(c Claims) Claims { c.Subject = ""; return c }},
		{"missing accountID", func(c Claims) Claims { c.AccountID = ""; return c }},
		{"missing accessKeyID", func(c Claims) Claims { c.AccessKeyID = ""; return c }},
		{"unsupported principalType", func(c Claims) Claims { c.PrincipalType = "IAMUser"; return c }},
		{"empty principalType", func(c Claims) Claims { c.PrincipalType = ""; return c }},
	}
	v := NewVerifier(verify, testAudience)
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := v.Verify(sign(t, tc.mutate(base())))
			require.Error(t, err)
		})
	}
}

func TestVerifier_RejectsNonES256(t *testing.T) {
	_, nc, _ := testutil.StartTestJetStream(t)
	js := testutil.NewJetStream(t, nc)
	_, verify, err := LoadOrCreateSigningKey(t.Context(), js, testMasterKey, 1)
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
