package handlers_sts

import (
	"bytes"
	"encoding/json"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/sts"
	handlers_iam "github.com/mulgadc/spinifex/spinifex/handlers/iam"
	"github.com/mulgadc/spinifex/spinifex/testutil"
	"github.com/nats-io/nats.go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// newTestSetup spins up a NATS+JetStream test server, builds a real
// IAMServiceImpl (cheaper than hand-rolling a 40-method stub), and returns
// the wired-up STSServiceImpl together with the underlying NATS conn for
// tests that need to interact with KV directly.
func newTestSetup(t *testing.T) (*STSServiceImpl, *nats.Conn) {
	t.Helper()
	_, nc, _ := testutil.StartTestJetStream(t)

	masterKey, err := handlers_iam.GenerateMasterKey()
	require.NoError(t, err)

	iamSvc, err := handlers_iam.NewIAMServiceImpl(nc, masterKey, 1)
	require.NoError(t, err)

	stsSvc, err := NewSTSServiceImpl(nc, iamSvc, masterKey, 1)
	require.NoError(t, err)
	return stsSvc, nc
}

func TestNewSTSServiceImpl_RejectsNilNATSConn(t *testing.T) {
	masterKey := bytes.Repeat([]byte{0x01}, masterKeySize)
	_, err := NewSTSServiceImpl(nil, nopIAMService{}, masterKey, 1)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "NATS")
}

func TestNewSTSServiceImpl_RejectsNilIAMService(t *testing.T) {
	_, nc, _ := testutil.StartTestJetStream(t)
	masterKey := bytes.Repeat([]byte{0x01}, masterKeySize)
	_, err := NewSTSServiceImpl(nc, nil, masterKey, 1)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "IAM")
}

func TestNewSTSServiceImpl_RejectsWrongMasterKeySize(t *testing.T) {
	_, nc, _ := testutil.StartTestJetStream(t)
	for _, size := range []int{0, 1, 16, 31, 33, 64} {
		t.Run("size", func(t *testing.T) {
			_, err := NewSTSServiceImpl(nc, nopIAMService{}, bytes.Repeat([]byte{0xaa}, size), 1)
			require.Error(t, err)
			assert.Contains(t, err.Error(), "master key")
		})
	}
}

func TestNewSTSServiceImpl_InitializesBucket(t *testing.T) {
	svc, _ := newTestSetup(t)
	require.NotNil(t, svc.sessionsBucket)
	assert.Equal(t, KVBucketSessionCredentials, svc.sessionsBucket.Bucket())
}

func TestNewSTSServiceImpl_NormalisesNegativeClusterSize(t *testing.T) {
	_, nc, _ := testutil.StartTestJetStream(t)
	masterKey, err := handlers_iam.GenerateMasterKey()
	require.NoError(t, err)
	iamSvc, err := handlers_iam.NewIAMServiceImpl(nc, masterKey, 1)
	require.NoError(t, err)

	svc, err := NewSTSServiceImpl(nc, iamSvc, masterKey, 0)
	require.NoError(t, err)
	require.NotNil(t, svc)
}

func TestSTSServiceImpl_AssumeRoleSkeleton(t *testing.T) {
	svc, _ := newTestSetup(t)
	out, err := svc.AssumeRole("000000000000", "arn:aws:iam::000000000000:user/alice", "alice",
		&sts.AssumeRoleInput{RoleArn: aws.String("arn:aws:iam::000000000000:role/app")})
	require.Error(t, err)
	assert.Nil(t, out)
	assert.ErrorIs(t, err, errSTSSkeleton)
}

func TestSTSServiceImpl_GetCallerIdentitySkeleton(t *testing.T) {
	svc, _ := newTestSetup(t)
	out, err := svc.GetCallerIdentity("000000000000", "arn:aws:iam::000000000000:user/alice", "AIDA...",
		&sts.GetCallerIdentityInput{})
	require.Error(t, err)
	assert.Nil(t, out)
	assert.ErrorIs(t, err, errSTSSkeleton)
}

func TestLookupSessionCredential_NonASIAPrefixReturnsNilNil(t *testing.T) {
	svc, _ := newTestSetup(t)

	cases := []string{
		"AKIAEXAMPLEAAAAAAAAA", // long-lived prefix — must never trigger a lookup
		"",
		"TESTEXAMPLE",
		"asiaEXAMPLEAAAAAAAAA", // lowercase: prefix check is case-sensitive
	}
	for _, akid := range cases {
		got, err := svc.LookupSessionCredential(akid)
		require.NoError(t, err)
		assert.Nil(t, got, "AKID %q should not resolve to a session credential", akid)
	}
}

func TestLookupSessionCredential_MissingASIAReturnsNilNil(t *testing.T) {
	svc, _ := newTestSetup(t)
	got, err := svc.LookupSessionCredential("ASIAMISSINGAAAAAAAAA")
	require.NoError(t, err)
	assert.Nil(t, got)
}

func TestLookupSessionCredential_HitRoundTrips(t *testing.T) {
	svc, _ := newTestSetup(t)

	now := time.Now().UTC().Truncate(time.Second)
	cred := &SessionCredential{
		AccessKeyID:       "ASIAROUNDTRIPAAAAAAA",
		SecretEncrypted:   "ciphertext-base64",
		SessionTokenHMAC:  "hmac-base64",
		AccountID:         "000000000000",
		AssumedRoleARN:    "arn:aws:sts::000000000000:assumed-role/app/sess-1",
		UnderlyingRoleARN: "arn:aws:iam::000000000000:role/app",
		RoleID:            "AROAEXAMPLEAAAAAA",
		AssumedRoleID:     "AROAEXAMPLEAAAAAA:sess-1",
		SessionName:       "sess-1",
		ExpiresAt:         now.Add(time.Hour),
		CreatedAt:         now,
	}
	require.NoError(t, putSessionCredential(svc.sessionsBucket, cred))

	got, err := svc.LookupSessionCredential(cred.AccessKeyID)
	require.NoError(t, err)
	require.NotNil(t, got)
	assert.Equal(t, cred.AccessKeyID, got.AccessKeyID)
	assert.Equal(t, cred.AssumedRoleARN, got.AssumedRoleARN)
	assert.Equal(t, cred.SessionTokenHMAC, got.SessionTokenHMAC)
	assert.True(t, got.ExpiresAt.Equal(cred.ExpiresAt))
}

func TestLookupSessionCredential_UnmarshalFailureSurfacesError(t *testing.T) {
	svc, _ := newTestSetup(t)
	akid := "ASIACORRUPTAAAAAAAAA"
	// Bypass putSessionCredential to inject a deliberately malformed payload —
	// the prefix is valid (so the lookup reaches the bucket) but the JSON
	// body is garbage. This guards against the "lookup returns nil silently
	// on parse failure" silent-failure mode.
	_, err := svc.sessionsBucket.Put(akid, []byte("not json"))
	require.NoError(t, err)

	got, err := svc.LookupSessionCredential(akid)
	require.Error(t, err)
	assert.Nil(t, got)
	assert.Contains(t, err.Error(), "unmarshal session credential")
}

// Sanity check: the production marshaller round-trips through the lookup
// path. Catches struct-tag regressions early.
func TestSessionCredential_JSONRoundTrip(t *testing.T) {
	cred := SessionCredential{
		AccessKeyID:    "ASIAJSONROUNDAAAAAAA",
		AccountID:      "000000000000",
		AssumedRoleARN: "arn:aws:sts::000000000000:assumed-role/app/sess",
		ExpiresAt:      time.Date(2030, 1, 1, 0, 0, 0, 0, time.UTC),
		CreatedAt:      time.Date(2029, 12, 31, 23, 0, 0, 0, time.UTC),
	}
	data, err := json.Marshal(cred)
	require.NoError(t, err)

	var got SessionCredential
	require.NoError(t, json.Unmarshal(data, &got))
	assert.Equal(t, cred, got)
}

// nopIAMService satisfies handlers_iam.IAMService via embedding so the
// constructor's nil-check tests can pass a non-nil interface without
// implementing 40+ methods. Any actual call panics — the Step 2 tests never
// reach that code path.
type nopIAMService struct {
	handlers_iam.IAMService
}
