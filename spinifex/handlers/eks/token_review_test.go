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

func TestEffectiveGroups_EachPolicyProducesItsGroup(t *testing.T) {
	cases := []struct {
		policyARN string
		want      string
	}{
		{"arn:aws:eks::aws:cluster-access-policy/AmazonEKSClusterAdminPolicy", "system:masters"},
		{"arn:aws:eks::aws:cluster-access-policy/AmazonEKSAdminPolicy", "mulga:eks-admin"},
		{"arn:aws:eks::aws:cluster-access-policy/AmazonEKSEditPolicy", "mulga:eks-edit"},
		{"arn:aws:eks::aws:cluster-access-policy/AmazonEKSViewPolicy", "mulga:eks-view"},
	}
	for _, tc := range cases {
		t.Run(tc.want, func(t *testing.T) {
			rec := &AccessEntryRecord{
				AssociatedPolicies: []AssociatedAccessPolicy{
					{PolicyARN: tc.policyARN, AccessScope: AccessScope{Type: accessScopeCluster}},
				},
			}
			assert.Equal(t, []string{tc.want}, effectiveGroups(rec))
		})
	}
}

func TestEffectiveGroups_TwoAssociationsProduceBothGroups(t *testing.T) {
	rec := &AccessEntryRecord{
		AssociatedPolicies: []AssociatedAccessPolicy{
			{PolicyARN: "arn:aws:eks::aws:cluster-access-policy/AmazonEKSEditPolicy", AccessScope: AccessScope{Type: accessScopeCluster}},
			{PolicyARN: "arn:aws:eks::aws:cluster-access-policy/AmazonEKSViewPolicy", AccessScope: AccessScope{Type: accessScopeCluster}},
		},
	}
	assert.Equal(t, []string{"mulga:eks-edit", "mulga:eks-view"}, effectiveGroups(rec))
}

// A creator seeded system:masters directly (CreateCluster bootstrap) who is
// also associated to AmazonEKSClusterAdminPolicy must not see the group twice.
func TestEffectiveGroups_DedupesSeededAndAssociatedClusterAdmin(t *testing.T) {
	rec := &AccessEntryRecord{
		KubernetesGroups: []string{"system:masters"},
		AssociatedPolicies: []AssociatedAccessPolicy{
			{PolicyARN: "arn:aws:eks::aws:cluster-access-policy/AmazonEKSClusterAdminPolicy", AccessScope: AccessScope{Type: accessScopeCluster}},
		},
	}
	assert.Equal(t, []string{"system:masters"}, effectiveGroups(rec))
}

// Records written before namespace scope was rejected at association time may
// still carry one; it must not silently widen to a cluster-scope grant.
func TestEffectiveGroups_NamespaceScopeContributesNothing(t *testing.T) {
	rec := &AccessEntryRecord{
		AssociatedPolicies: []AssociatedAccessPolicy{
			{PolicyARN: "arn:aws:eks::aws:cluster-access-policy/AmazonEKSViewPolicy", AccessScope: AccessScope{Type: accessScopeNamespace, Namespaces: []string{"team-a"}}},
		},
	}
	assert.Empty(t, effectiveGroups(rec))
}

func TestEffectiveGroups_UnrecognizedPolicyDoesNotPanic(t *testing.T) {
	rec := &AccessEntryRecord{
		AssociatedPolicies: []AssociatedAccessPolicy{
			{PolicyARN: "arn:aws:eks::aws:cluster-access-policy/MadeUpPolicy", AccessScope: AccessScope{Type: accessScopeCluster}},
		},
	}
	assert.NotPanics(t, func() { effectiveGroups(rec) })
	assert.Empty(t, effectiveGroups(rec))
}

// End-to-end through Authenticate: an associated policy's group must reach the
// TokenReview result, not just the internal helper.
func TestAuthenticate_ProjectsAssociatedPolicyGroup(t *testing.T) {
	lookup := func(arn string) (*AccessEntryRecord, error) {
		return &AccessEntryRecord{
			KubernetesUsername: testARN,
			AssociatedPolicies: []AssociatedAccessPolicy{
				{PolicyARN: "arn:aws:eks::aws:cluster-access-policy/AmazonEKSViewPolicy", AccessScope: AccessScope{Type: accessScopeCluster}},
			},
		}, nil
	}

	res := Authenticate(validToken("https://sts.amazonaws.com/?Action=GetCallerIdentity"), okVerify, lookup)

	require.True(t, res.Authenticated)
	assert.Equal(t, []string{"mulga:eks-view"}, res.Groups)
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
