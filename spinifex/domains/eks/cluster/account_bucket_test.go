package cluster

import (
	"testing"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/eks"
	"github.com/mulgadc/spinifex/internal/testkit"
	"github.com/mulgadc/spinifex/spinifex/foundation/aws/errors"
	"github.com/nats-io/nats.go/jetstream"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Handlers that skip requireOrchestrationDeps must still fail cleanly, not
// panic inside jetstream.New, when the service has no NATS connection.
func TestAccountBucket_NilNATSConnReturnsError(t *testing.T) {
	s := &EKSServiceImpl{}

	_, err := s.DescribeCluster(t.Context(), &eks.DescribeClusterInput{Name: aws.String("c")}, testAccountID)
	require.Error(t, err)

	_, err = s.DescribeNodegroup(t.Context(), &eks.DescribeNodegroupInput{
		ClusterName: aws.String("c"), NodegroupName: aws.String("ng"),
	}, testAccountID)
	require.Error(t, err)
}

// A lost account bucket holds no cluster: reads answer as AWS does for a
// missing cluster or an empty list, and none of them recreates the bucket.
func TestReadsAfterBucketLossLeaveBucketAbsent(t *testing.T) {
	t.Parallel()
	svc := setupTestService(t)
	seedTestCluster(t, svc, "c1")
	js := testutil.NewJetStream(t, svc.deps.NATSConn)
	require.NoError(t, js.DeleteKeyValue(t.Context(), AccountBucketName(testAccountID)))

	ctx := t.Context()
	list, err := svc.ListClusters(ctx, &eks.ListClustersInput{}, testAccountID)
	require.NoError(t, err)
	require.NotNil(t, list.Clusters)
	assert.Empty(t, list.Clusters)

	notFound := map[string]error{}
	_, notFound["DescribeCluster"] = svc.DescribeCluster(ctx, &eks.DescribeClusterInput{Name: aws.String("c1")}, testAccountID)
	_, notFound["DescribeNodegroup"] = svc.DescribeNodegroup(ctx, &eks.DescribeNodegroupInput{
		ClusterName: aws.String("c1"), NodegroupName: aws.String("ng"),
	}, testAccountID)
	_, notFound["ListNodegroups"] = svc.ListNodegroups(ctx, &eks.ListNodegroupsInput{ClusterName: aws.String("c1")}, testAccountID)
	_, notFound["ListAccessEntries"] = svc.ListAccessEntries(ctx, &eks.ListAccessEntriesInput{ClusterName: aws.String("c1")}, testAccountID)
	_, notFound["ListTagsForResource"] = svc.ListTagsForResource(ctx, &eks.ListTagsForResourceInput{
		ResourceArn: aws.String("arn:aws:eks:ap-southeast-2:" + testAccountID + ":cluster/c1"),
	}, testAccountID)
	for op, err := range notFound {
		assert.Equal(t, awserrors.ErrorEKSResourceNotFound, awserrors.ValidErrorCodeFromError(err), op)
	}

	_, err = js.KeyValue(ctx, AccountBucketName(testAccountID))
	require.ErrorIs(t, err, jetstream.ErrBucketNotFound, "a read must not recreate the account bucket")
}
