package cluster

import (
	"testing"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/eks"
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
