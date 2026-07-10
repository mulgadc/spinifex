package handlers_ecs

import (
	"context"
	"testing"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ecs"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestService_CreateCapacityProvider_Idempotent(t *testing.T) {
	svc, _ := newTestService(t)
	ctx := context.Background()

	in := &ecs.CreateCapacityProviderInput{
		Name: aws.String("cp-1"),
		AutoScalingGroupProvider: &ecs.AutoScalingGroupProvider{
			AutoScalingGroupArn: aws.String("arn:aws:autoscaling:r:a:autoScalingGroup:x:autoScalingGroupName/asg-1"),
			ManagedScaling: &ecs.ManagedScaling{
				Status:                 aws.String(ecs.ManagedScalingStatusEnabled),
				TargetCapacity:         aws.Int64(90),
				MinimumScalingStepSize: aws.Int64(1),
				MaximumScalingStepSize: aws.Int64(10),
			},
		},
		Tags: []*ecs.Tag{{Key: aws.String("team"), Value: aws.String("infra")}},
	}
	out, err := svc.CreateCapacityProvider(ctx, in, testAccountID)
	require.NoError(t, err)
	require.NotNil(t, out.CapacityProvider)
	assert.Equal(t, "cp-1", aws.StringValue(out.CapacityProvider.Name))
	assert.Equal(t, CapacityProviderStatusActive, aws.StringValue(out.CapacityProvider.Status))
	assert.Equal(t, CapacityProviderARN(testRegion, testAccountID, "cp-1"), aws.StringValue(out.CapacityProvider.CapacityProviderArn))
	require.NotNil(t, out.CapacityProvider.AutoScalingGroupProvider)
	require.NotNil(t, out.CapacityProvider.AutoScalingGroupProvider.ManagedScaling)
	assert.Equal(t, int64(90), aws.Int64Value(out.CapacityProvider.AutoScalingGroupProvider.ManagedScaling.TargetCapacity))
	require.Len(t, out.CapacityProvider.Tags, 1)

	// Re-creating the same name returns the existing record, not a duplicate.
	again, err := svc.CreateCapacityProvider(ctx, in, testAccountID)
	require.NoError(t, err)
	assert.Equal(t, aws.StringValue(out.CapacityProvider.CapacityProviderArn), aws.StringValue(again.CapacityProvider.CapacityProviderArn))

	list, err := svc.DescribeCapacityProviders(ctx, &ecs.DescribeCapacityProvidersInput{}, testAccountID)
	require.NoError(t, err)
	assert.Len(t, list.CapacityProviders, 1)
}

func TestService_CreateCapacityProvider_InvalidParameter(t *testing.T) {
	svc, _ := newTestService(t)
	ctx := context.Background()

	_, err := svc.CreateCapacityProvider(ctx, &ecs.CreateCapacityProviderInput{}, testAccountID)
	require.Error(t, err)

	_, err = svc.CreateCapacityProvider(ctx, &ecs.CreateCapacityProviderInput{Name: aws.String("cp")}, testAccountID)
	require.Error(t, err)
}

func TestService_DescribeCapacityProviders_NamedAndMissing(t *testing.T) {
	svc, _ := newTestService(t)
	ctx := context.Background()

	_, err := svc.CreateCapacityProvider(ctx, &ecs.CreateCapacityProviderInput{
		Name: aws.String("cp-1"),
		AutoScalingGroupProvider: &ecs.AutoScalingGroupProvider{
			AutoScalingGroupArn: aws.String("arn:aws:autoscaling:r:a:autoScalingGroup:x:autoScalingGroupName/asg-1"),
		},
	}, testAccountID)
	require.NoError(t, err)

	out, err := svc.DescribeCapacityProviders(ctx, &ecs.DescribeCapacityProvidersInput{
		CapacityProviders: []*string{aws.String("cp-1"), aws.String("cp-missing")},
	}, testAccountID)
	require.NoError(t, err)
	require.Len(t, out.CapacityProviders, 1)
	require.Len(t, out.Failures, 1)
	assert.Equal(t, "cp-missing", aws.StringValue(out.Failures[0].Arn))
}

func TestService_DeleteCapacityProvider(t *testing.T) {
	svc, _ := newTestService(t)
	ctx := context.Background()

	_, err := svc.CreateCapacityProvider(ctx, &ecs.CreateCapacityProviderInput{
		Name: aws.String("cp-1"),
		AutoScalingGroupProvider: &ecs.AutoScalingGroupProvider{
			AutoScalingGroupArn: aws.String("arn:aws:autoscaling:r:a:autoScalingGroup:x:autoScalingGroupName/asg-1"),
		},
	}, testAccountID)
	require.NoError(t, err)

	out, err := svc.DeleteCapacityProvider(ctx, &ecs.DeleteCapacityProviderInput{CapacityProvider: aws.String("cp-1")}, testAccountID)
	require.NoError(t, err)
	assert.Equal(t, "cp-1", aws.StringValue(out.CapacityProvider.Name))

	list, err := svc.DescribeCapacityProviders(ctx, &ecs.DescribeCapacityProvidersInput{}, testAccountID)
	require.NoError(t, err)
	assert.Empty(t, list.CapacityProviders)

	_, err = svc.DeleteCapacityProvider(ctx, &ecs.DeleteCapacityProviderInput{CapacityProvider: aws.String("cp-1")}, testAccountID)
	require.Error(t, err)
}

// TestService_PutClusterCapacityProviders_PersistsStrategy asserts the
// strategy round-trips onto the cluster record and is returned by
// DescribeClusters, without any scheduler/placement coupling (inert in v1).
func TestService_PutClusterCapacityProviders_PersistsStrategy(t *testing.T) {
	svc, _ := newTestService(t)
	ctx := context.Background()

	_, err := svc.CreateCluster(ctx, &ecs.CreateClusterInput{ClusterName: aws.String("web")}, testAccountID)
	require.NoError(t, err)

	out, err := svc.PutClusterCapacityProviders(ctx, &ecs.PutClusterCapacityProvidersInput{
		Cluster:           aws.String("web"),
		CapacityProviders: []*string{aws.String("cp-1"), aws.String("cp-2")},
		DefaultCapacityProviderStrategy: []*ecs.CapacityProviderStrategyItem{
			{CapacityProvider: aws.String("cp-1"), Weight: aws.Int64(1), Base: aws.Int64(2)},
			{CapacityProvider: aws.String("cp-2"), Weight: aws.Int64(3)},
		},
	}, testAccountID)
	require.NoError(t, err)
	require.Len(t, out.Cluster.CapacityProviders, 2)
	require.Len(t, out.Cluster.DefaultCapacityProviderStrategy, 2)
	assert.Equal(t, "cp-1", aws.StringValue(out.Cluster.DefaultCapacityProviderStrategy[0].CapacityProvider))
	assert.Equal(t, int64(2), aws.Int64Value(out.Cluster.DefaultCapacityProviderStrategy[0].Base))

	// Persisted onto the cluster record itself, visible via DescribeClusters.
	described, err := svc.DescribeClusters(ctx, &ecs.DescribeClustersInput{Clusters: []*string{aws.String("web")}}, testAccountID)
	require.NoError(t, err)
	require.Len(t, described.Clusters, 1)
	require.Len(t, described.Clusters[0].CapacityProviders, 2)
	require.Len(t, described.Clusters[0].DefaultCapacityProviderStrategy, 2)
}

func TestService_PutClusterCapacityProviders_UnknownCluster(t *testing.T) {
	svc, _ := newTestService(t)
	_, err := svc.PutClusterCapacityProviders(context.Background(), &ecs.PutClusterCapacityProvidersInput{
		Cluster:           aws.String("ghost"),
		CapacityProviders: []*string{aws.String("cp-1")},
	}, testAccountID)
	require.Error(t, err)
}
