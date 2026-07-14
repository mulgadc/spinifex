package handlers_eks

import (
	"testing"

	"github.com/mulgadc/spinifex/spinifex/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const testAccountID = "111122223333"

func TestGetOrCreateAccountBucket_Idempotent(t *testing.T) {
	_, _, js := testutil.StartTestJetStream(t)

	kv1, err := GetOrCreateAccountBucket(js, testAccountID, 1)
	require.NoError(t, err)
	require.NotNil(t, kv1)

	kv2, err := GetOrCreateAccountBucket(js, testAccountID, 1)
	require.NoError(t, err)
	require.NotNil(t, kv2)

	assert.Equal(t, AccountBucketName(testAccountID), kv1.Bucket())
	assert.Equal(t, kv1.Bucket(), kv2.Bucket())
}

// TestGetOrCreateAccountBucket_ReplicasClamped confirms the per-account
// bucket is created with the requested replica count, clamped to a minimum
// of 1. The embedded single-node test server rejects Replicas > 1
// ("replicas > 1 not supported in non-clustered mode"), so only the clamping
// path (replicas <= 0 -> 1) is exercisable end-to-end here; multi-node
// replica counts are exercised live (see the associated bug doc).
func TestGetOrCreateAccountBucket_ReplicasClamped(t *testing.T) {
	_, _, js := testutil.StartTestJetStream(t)

	kv, err := GetOrCreateAccountBucket(js, testAccountID, 0)
	require.NoError(t, err)
	require.NotNil(t, kv)

	si, err := js.StreamInfo("KV_" + AccountBucketName(testAccountID))
	require.NoError(t, err)
	assert.Equal(t, 1, si.Config.Replicas)
}

func TestInitLeaderBucket_Idempotent(t *testing.T) {
	_, _, js := testutil.StartTestJetStream(t)

	kv1, err := InitLeaderBucket(js, 1)
	require.NoError(t, err)
	require.NotNil(t, kv1)
	assert.Equal(t, KVBucketEKSLeader, kv1.Bucket())

	kv2, err := InitLeaderBucket(js, 1)
	require.NoError(t, err)
	require.NotNil(t, kv2)
	assert.Equal(t, KVBucketEKSLeader, kv2.Bucket())
}

// TestInitLeaderBucket_ReplicasClamped confirms the leader bucket is created
// with the requested replica count, clamped to a minimum of 1.
func TestInitLeaderBucket_ReplicasClamped(t *testing.T) {
	_, _, js := testutil.StartTestJetStream(t)

	kv, err := InitLeaderBucket(js, -1)
	require.NoError(t, err)
	require.NotNil(t, kv)

	si, err := js.StreamInfo("KV_" + KVBucketEKSLeader)
	require.NoError(t, err)
	assert.Equal(t, 1, si.Config.Replicas)
}

func TestKeyPaths_MatchQ2Spec(t *testing.T) {
	cases := []struct {
		name string
		got  string
		want string
	}{
		{"ClusterMetaKey", ClusterMetaKey("alpha"), "clusters/alpha/meta"},
		{"NodegroupKey", NodegroupKey("alpha", "ng-1"), "clusters/alpha/nodegroups/ng-1"},
		{"AccessEntryKey", AccessEntryKey("alpha", "arn:aws:iam::111122223333:user/dev"), "clusters/alpha/access-entries/" + PrincipalARNHash("arn:aws:iam::111122223333:user/dev")},
		{"OIDCProviderKey", OIDCProviderKey("alpha", "abc123"), "clusters/alpha/oidc-providers/abc123"},
		{"OIDCSigningKeyKey", OIDCSigningKeyKey("alpha"), "clusters/alpha/oidc-signing-key.pem.enc"},
		{"OIDCJWKSKey", OIDCJWKSKey("alpha"), "clusters/alpha/oidc-jwks.json"},
		{"AdminKubeconfigKey", AdminKubeconfigKey("alpha"), "clusters/alpha/admin-kubeconfig.enc"},
		{"NodeTokenKey", NodeTokenKey("alpha"), "clusters/alpha/k3s-node-token.enc"},
		{"EventKey", EventKey("alpha", "1700000000"), "clusters/alpha/events/1700000000"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, tc.got)
		})
	}
}

func TestAccountBucketName(t *testing.T) {
	assert.Equal(t, "eks-account-111122223333", AccountBucketName(testAccountID))
}

func TestNewStore_NilConn(t *testing.T) {
	_, err := NewStore(nil)
	require.Error(t, err)
}
