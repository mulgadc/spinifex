package handlers_eks

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/mulgadc/spinifex/spinifex/testutil"
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func validToken(url string) string {
	return "k8s-aws-v1." + base64.RawURLEncoding.EncodeToString([]byte(url))
}

const testARN = "arn:aws:iam::111122223333:role/admin"

// seedAccountBucket creates an empty per-account bucket on the jetstream API.
// testutil.SeedKV stays on the legacy handle, which cannot be passed to the
// migrated AccessEntry helpers.
func seedAccountBucket(t *testing.T, js jetstream.JetStream, accountID string) jetstream.KeyValue {
	t.Helper()
	kv, err := js.CreateKeyValue(t.Context(), jetstream.KeyValueConfig{
		Bucket:  AccountBucketName(accountID),
		History: KVBucketEKSAccountHistory,
	})
	require.NoError(t, err)
	return kv
}

func okVerify(_ string) (*TokenVerifyResponse, error) {
	return &TokenVerifyResponse{
		AccountID:     "111122223333",
		ARN:           testARN,
		UserID:        "AROAEXAMPLE:session",
		PrincipalType: "AssumedRole",
	}, nil
}

func TestAuthenticate_GrantsWhenEntryExists(t *testing.T) {
	lookup := func(arn string) (*AccessEntryRecord, error) {
		assert.Equal(t, testARN, arn)
		return &AccessEntryRecord{
			KubernetesUsername: testARN,
			KubernetesGroups:   []string{"system:masters"},
		}, nil
	}

	res := Authenticate(validToken("https://sts.amazonaws.com/?Action=GetCallerIdentity"), okVerify, lookup)

	require.True(t, res.Authenticated)
	assert.Equal(t, testARN, res.Username)
	assert.Equal(t, "AROAEXAMPLE:session", res.UID)
	assert.Equal(t, []string{"system:masters"}, res.Groups)
}

func TestAuthenticate_DeniesMalformedToken(t *testing.T) {
	called := false
	verify := func(string) (*TokenVerifyResponse, error) { called = true; return nil, nil }
	lookup := func(string) (*AccessEntryRecord, error) { return nil, nil }

	res := Authenticate("not-a-k8s-aws-token", verify, lookup)

	assert.False(t, res.Authenticated)
	assert.False(t, called, "must not call STS for a token that fails to decode")
}

func TestAuthenticate_DeniesWhenVerifyFails(t *testing.T) {
	verify := func(string) (*TokenVerifyResponse, error) {
		return nil, errors.New("signature mismatch")
	}
	lookup := func(string) (*AccessEntryRecord, error) {
		t.Fatal("lookup must not run when verify fails")
		return nil, nil
	}

	res := Authenticate(validToken("https://sts/?x=1"), verify, lookup)
	assert.False(t, res.Authenticated)
}

func TestAuthenticate_DeniesWhenNoAccessEntry(t *testing.T) {
	lookup := func(string) (*AccessEntryRecord, error) {
		return nil, ErrAccessEntryNotFound
	}

	res := Authenticate(validToken("https://sts/?x=1"), okVerify, lookup)
	assert.False(t, res.Authenticated)
	assert.Empty(t, res.Username)
}

func TestAuthenticate_FallsBackUIDToARN(t *testing.T) {
	verify := func(string) (*TokenVerifyResponse, error) {
		return &TokenVerifyResponse{ARN: testARN}, nil // no UserID
	}
	lookup := func(string) (*AccessEntryRecord, error) {
		return &AccessEntryRecord{KubernetesUsername: testARN, KubernetesGroups: []string{"system:masters"}}, nil
	}

	res := Authenticate(validToken("https://sts/?x=1"), verify, lookup)
	require.True(t, res.Authenticated)
	assert.Equal(t, testARN, res.UID)
}

func TestResolveTokenReview_NilConn(t *testing.T) {
	_, err := ResolveTokenReview(context.Background(), nil, "111122223333", "alpha", "tok", time.Second)
	require.Error(t, err)
}

// A genuine infra fault (account bucket never created) is an error, not a
// silent Authenticated=false — the webhook turns it into a retryable 5xx.
func TestResolveTokenReview_MissingBucketErrors(t *testing.T) {
	_, nc, _ := testutil.StartTestJetStream(t)
	_, err := ResolveTokenReview(context.Background(), nc, "111122223333", "alpha", validToken("https://sts/?x=1"), time.Second)
	require.Error(t, err)
}

func TestResolveTokenReview_AuthenticatesViaVerifyAndKV(t *testing.T) {
	_, nc, _ := testutil.StartTestJetStream(t)
	js := testutil.NewJetStream(t, nc)
	kv := seedAccountBucket(t, js, "111122223333")
	require.NoError(t, PutAccessEntryRecord(t.Context(), kv, &AccessEntryRecord{
		ClusterName:        "alpha",
		PrincipalARN:       testARN,
		KubernetesUsername: testARN,
		KubernetesGroups:   []string{"system:masters"},
		Type:               AccessEntryTypeStandard,
	}))

	// Stand in for the awsgw-hosted STS verify responder.
	sub, err := nc.Subscribe(TokenVerifySubject, func(m *nats.Msg) {
		resp, _ := json.Marshal(TokenVerifyResponse{
			AccountID:     "111122223333",
			ARN:           testARN,
			UserID:        "AROAEXAMPLE:session",
			PrincipalType: "AssumedRole",
		})
		_ = m.Respond(resp)
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = sub.Unsubscribe() })

	res, err := ResolveTokenReview(context.Background(), nc, "111122223333", "alpha", validToken("https://sts/?Action=GetCallerIdentity"), 2*time.Second)
	require.NoError(t, err)
	require.True(t, res.Authenticated)
	assert.Equal(t, testARN, res.Username)
	assert.Equal(t, "AROAEXAMPLE:session", res.UID)
	assert.Equal(t, []string{"system:masters"}, res.Groups)
}

// A valid IAM principal with no AccessEntry resolves to Authenticated=false
// (a clean 401), not an error.
func TestResolveTokenReview_NoAccessEntryDenies(t *testing.T) {
	_, nc, _ := testutil.StartTestJetStream(t)
	js := testutil.NewJetStream(t, nc)
	seedAccountBucket(t, js, "111122223333")

	sub, err := nc.Subscribe(TokenVerifySubject, func(m *nats.Msg) {
		resp, _ := json.Marshal(TokenVerifyResponse{AccountID: "111122223333", ARN: testARN})
		_ = m.Respond(resp)
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = sub.Unsubscribe() })

	res, err := ResolveTokenReview(context.Background(), nc, "111122223333", "alpha", validToken("https://sts/?x=1"), 2*time.Second)
	require.NoError(t, err)
	assert.False(t, res.Authenticated)
}
