package handlers_eks_test

import (
	"testing"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/eks"
	handlers_eks "github.com/mulgadc/spinifex/spinifex/handlers/eks"
	"github.com/mulgadc/spinifex/spinifex/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The store mechanics are tested in spinifex/idempotency. What is EKS's own is
// the param hash and that the store binds against the cluster-token bucket.

// clusterTokenParamHash ignores ClientRequestToken (same params, different token
// → same hash) but reflects a real parameter change.
func TestClusterTokenParamHash_IgnoresTokenReflectsParams(t *testing.T) {
	t.Parallel()
	base := &eks.CreateClusterInput{
		Name:               aws.String("c1"),
		Version:            aws.String("1.32"),
		RoleArn:            aws.String("arn:aws:iam::000000000000:role/eksrole"),
		ClientRequestToken: aws.String("tok-A"),
	}
	sameParamsDiffToken := *base
	sameParamsDiffToken.ClientRequestToken = aws.String("tok-B")
	diffParams := *base
	diffParams.Version = aws.String("1.31")

	assert.Equal(t, handlers_eks.ClusterTokenParamHash(base), handlers_eks.ClusterTokenParamHash(&sameParamsDiffToken),
		"token must not affect the param hash")
	assert.NotEqual(t, handlers_eks.ClusterTokenParamHash(base), handlers_eks.ClusterTokenParamHash(&diffParams),
		"a real param change must change the hash")
}

// The hash is taken from a copy, so hashing must not clear the caller's token.
func TestClusterTokenParamHash_LeavesTheInputAlone(t *testing.T) {
	t.Parallel()
	input := &eks.CreateClusterInput{Name: aws.String("c1"), ClientRequestToken: aws.String("tok-A")}

	handlers_eks.ClusterTokenParamHash(input)

	assert.Equal(t, "tok-A", aws.StringValue(input.ClientRequestToken),
		"hashing must not strip the token the create still needs")
}

// A cluster name round-trips through the store, which is what a duplicate
// CreateCluster resolves its reply from.
func TestClusterTokenStore_ReplaysTheClusterName(t *testing.T) {
	t.Parallel()
	_, nc, _ := testutil.StartTestJetStream(t)
	store, err := handlers_eks.NewClusterTokenStore(t.Context(), testutil.NewJetStream(t, nc))
	require.NoError(t, err)
	const account, tok, hash = "111122223333", "tok-1", "h"

	_, owned, err := store.Claim(t.Context(), account, tok, hash)
	require.NoError(t, err)
	require.True(t, owned)
	require.NoError(t, store.Finalize(t.Context(), account, tok, hash, "my-cluster"))

	replay, owned2, err := store.Claim(t.Context(), account, tok, hash)
	require.NoError(t, err)
	assert.False(t, owned2)
	require.NotNil(t, replay)
	assert.Equal(t, "my-cluster", *replay)
}
