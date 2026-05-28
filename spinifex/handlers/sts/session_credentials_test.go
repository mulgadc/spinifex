package handlers_sts

import (
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/mulgadc/spinifex/spinifex/testutil"
	"github.com/mulgadc/spinifex/spinifex/utils"
	"github.com/nats-io/nats.go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func setupBucket(t *testing.T) nats.KeyValue {
	t.Helper()
	_, _, js := testutil.StartTestJetStream(t)
	kv, err := initSessionCredentialsBucket(js, 1)
	require.NoError(t, err)
	return kv
}

func newTestSessionCredential(akid string) *SessionCredential {
	now := time.Now().UTC()
	return &SessionCredential{
		AccessKeyID:       akid,
		SecretEncrypted:   "ciphertext-base64",
		SessionTokenHMAC:  "hmac-base64",
		AccountID:         "000000000000",
		AssumedRoleARN:    "arn:aws:sts::000000000000:assumed-role/app-role/session-1",
		UnderlyingRoleARN: "arn:aws:iam::000000000000:role/app-role",
		RoleID:            "AROAEXAMPLEAAAAAA",
		AssumedRoleID:     "AROAEXAMPLEAAAAAA:session-1",
		SessionName:       "session-1",
		ExpiresAt:         now.Add(1 * time.Hour),
		CreatedAt:         now,
	}
}

func TestInitSessionCredentialsBucket_StampsVersion(t *testing.T) {
	_, _, js := testutil.StartTestJetStream(t)

	kv, err := initSessionCredentialsBucket(js, 1)
	require.NoError(t, err)

	version, err := utils.ReadVersion(kv)
	require.NoError(t, err)
	assert.Equal(t, KVBucketSessionCredentialsVersion, version)
}

func TestInitSessionCredentialsBucket_Idempotent(t *testing.T) {
	_, _, js := testutil.StartTestJetStream(t)

	kv1, err := initSessionCredentialsBucket(js, 1)
	require.NoError(t, err)

	// Reopen — must return the same bucket without error.
	kv2, err := initSessionCredentialsBucket(js, 1)
	require.NoError(t, err)
	assert.Equal(t, kv1.Bucket(), kv2.Bucket())
}

func TestPutSessionCredential_RoundTrip(t *testing.T) {
	bucket := setupBucket(t)
	cred := newTestSessionCredential("ASIAEXAMPLEAAAAAAAAA")

	require.NoError(t, putSessionCredential(bucket, cred))

	entry, err := bucket.Get(cred.AccessKeyID)
	require.NoError(t, err)

	var got SessionCredential
	require.NoError(t, json.Unmarshal(entry.Value(), &got))
	assert.Equal(t, cred.AccessKeyID, got.AccessKeyID)
	assert.Equal(t, cred.AssumedRoleARN, got.AssumedRoleARN)
	assert.Equal(t, cred.SessionTokenHMAC, got.SessionTokenHMAC)
	assert.Equal(t, cred.SessionName, got.SessionName)
	assert.True(t, got.ExpiresAt.Equal(cred.ExpiresAt))
}

func TestPutSessionCredential_RejectsNonASIAPrefix(t *testing.T) {
	bucket := setupBucket(t)

	cases := []struct {
		name string
		akid string
	}{
		{"akia_long_lived_prefix", "AKIAEXAMPLEAAAAAAAAA"},
		{"empty", ""},
		{"lowercase_asia", "asiaEXAMPLEAAAAAAAAA"},
		{"unknown_prefix", "TESTEXAMPLEAAAAAAAAA"},
		{"truncated_prefix", "ASI"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cred := newTestSessionCredential(tc.akid)
			err := putSessionCredential(bucket, cred)
			require.Error(t, err)
			assert.Contains(t, err.Error(), "ASIA")
		})
	}

	// Bucket must contain no credentials — every put failed before reaching
	// bucket.Create, so no AKID should be present.
	keys, err := bucket.Keys()
	if !errors.Is(err, nats.ErrNoKeysFound) {
		require.NoError(t, err)
		for _, k := range keys {
			if k == utils.VersionKey {
				continue
			}
			t.Fatalf("unexpected key written to session bucket: %q", k)
		}
	}
}

func TestPutSessionCredential_NilCredential(t *testing.T) {
	bucket := setupBucket(t)
	err := putSessionCredential(bucket, nil)
	require.Error(t, err)
}

func TestPutSessionCredential_CollisionReturnsKeyExists(t *testing.T) {
	bucket := setupBucket(t)
	cred := newTestSessionCredential("ASIACOLLISIONAAAAAAA")

	require.NoError(t, putSessionCredential(bucket, cred))

	// Second create with the same AKID must surface nats.ErrKeyExists so the
	// mint helper can retry with a freshly generated AKID.
	err := putSessionCredential(bucket, cred)
	require.Error(t, err)
	assert.ErrorIs(t, err, nats.ErrKeyExists)
}

func TestVerifySessionToken_MatchAndMismatch(t *testing.T) {
	svc, _ := newTestSetup(t)

	const wireToken = "the-original-wire-token"
	cred := &SessionCredential{
		AccessKeyID:      "ASIAVERIFYAAAAAAAAAA",
		SessionTokenHMAC: computeTokenHMAC(svc.masterKey, wireToken),
	}

	assert.True(t, svc.VerifySessionToken(cred, wireToken),
		"matching wire token must verify under the master key")
	assert.False(t, svc.VerifySessionToken(cred, "tampered-token"),
		"mismatched wire token must reject")
	assert.False(t, svc.VerifySessionToken(cred, ""),
		"empty wire token must reject without comparing")
	assert.False(t, svc.VerifySessionToken(nil, wireToken),
		"nil cred must reject without panicking")
}

func TestVerifySessionToken_CorruptStoredHMACRejects(t *testing.T) {
	// A SessionTokenHMAC field that fails base64 decode is data corruption.
	// VerifySessionToken must reject rather than fall through with a zero
	// expected slice and accidentally match an empty HMAC.
	svc, _ := newTestSetup(t)
	cred := &SessionCredential{
		AccessKeyID:      "ASIACORRUPTHMACAAAAA",
		SessionTokenHMAC: "!!!not-base64!!!",
	}
	assert.False(t, svc.VerifySessionToken(cred, "any-token"))
}
