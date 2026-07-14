package handlers_eks

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/eks"
	"github.com/mulgadc/spinifex/spinifex/awserrors"
	"github.com/nats-io/nats.go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestEKSServiceImpl_ListClustersPagination walks the NextToken/MaxResults
// paging path end-to-end: each page honours MaxResults, hands back a NextToken
// pointing at the next name, and the final page carries no token.
func TestEKSServiceImpl_ListClustersPagination(t *testing.T) {
	f := newEKSServiceFixture(t)
	for _, n := range []string{"c01", "c02", "c03", "c04", "c05"} {
		require.NoError(t, PutClusterMeta(f.kv, sampleClusterMeta(n)))
	}

	p1, err := f.svc.ListClusters(context.Background(), &eks.ListClustersInput{MaxResults: aws.Int64(2)}, testAccountID)
	require.NoError(t, err)
	assert.Equal(t, []string{"c01", "c02"}, aws.StringValueSlice(p1.Clusters))
	require.NotNil(t, p1.NextToken)
	assert.Equal(t, "c03", aws.StringValue(p1.NextToken))

	p2, err := f.svc.ListClusters(context.Background(), &eks.ListClustersInput{MaxResults: aws.Int64(2), NextToken: p1.NextToken}, testAccountID)
	require.NoError(t, err)
	assert.Equal(t, []string{"c03", "c04"}, aws.StringValueSlice(p2.Clusters))
	require.NotNil(t, p2.NextToken)
	assert.Equal(t, "c05", aws.StringValue(p2.NextToken))

	p3, err := f.svc.ListClusters(context.Background(), &eks.ListClustersInput{MaxResults: aws.Int64(2), NextToken: p2.NextToken}, testAccountID)
	require.NoError(t, err)
	assert.Equal(t, []string{"c05"}, aws.StringValueSlice(p3.Clusters))
	assert.Nil(t, p3.NextToken, "final page must not advertise a NextToken")
}

// TestEKSServiceImpl_ListClustersMaxResultsClamped confirms an out-of-range
// MaxResults (zero, negative, or above the 100 ceiling) clamps to a single full
// page rather than truncating or erroring.
func TestEKSServiceImpl_ListClustersMaxResultsClamped(t *testing.T) {
	f := newEKSServiceFixture(t)
	want := []string{"c01", "c02", "c03"}
	for _, n := range want {
		require.NoError(t, PutClusterMeta(f.kv, sampleClusterMeta(n)))
	}

	for _, mr := range []int64{0, -5, 250} {
		out, err := f.svc.ListClusters(context.Background(), &eks.ListClustersInput{MaxResults: aws.Int64(mr)}, testAccountID)
		require.NoError(t, err)
		assert.Equal(t, want, aws.StringValueSlice(out.Clusters), "MaxResults=%d must clamp to one full page", mr)
		assert.Nil(t, out.NextToken, "MaxResults=%d single page must not advertise a NextToken", mr)
	}
}

// TestEKSReadUnavailableClassification pins the post-restart behaviour: a
// transient JetStream error (no leader yet) surfaces as a retryable
// ServiceUnavailable, while a genuine fault stays wrapped (the daemon maps it
// to InternalError). Regression guard for list-clusters returning a misleading
// 500 during the KV warmup window.
func TestEKSReadUnavailableClassification(t *testing.T) {
	transient := []error{
		nats.ErrNoResponders,
		nats.ErrTimeout,
		nats.ErrNoStreamResponse,
		nats.ErrConnectionClosed,
		fmt.Errorf("kv keys: %w", nats.ErrNoResponders), // wrapped, as ListClusters wraps it
	}
	for _, e := range transient {
		assert.True(t, natsTransient(e), "%v must be classified transient", e)
		assert.EqualError(t, eksReadUnavailableOr(e, "op"), awserrors.ErrorServiceUnavailable)
	}

	genuine := errors.New("corrupt cluster meta")
	assert.False(t, natsTransient(genuine))
	assert.ErrorContains(t, eksReadUnavailableOr(genuine, "list cluster names"), "list cluster names: corrupt cluster meta")
}
