package ecs_test

import (
	"testing"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ecs"
	ecsdomain "github.com/mulgadc/spinifex/spinifex/domains/ecs"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// listInstanceARNs lists a cluster's container instances at one status, or at
// every status when the filter is empty.
func listInstanceARNs(t *testing.T, svc *ecsdomain.Service, cluster, status string) []string {
	t.Helper()
	in := &ecs.ListContainerInstancesInput{Cluster: aws.String(cluster)}
	if status != "" {
		in.Status = aws.String(status)
	}
	out, err := svc.ListContainerInstances(t.Context(), in, ecsdomain.TestAccountID)
	require.NoError(t, err)
	return aws.StringValueSlice(out.ContainerInstanceArns)
}

// A caller that cannot narrow the list has to describe every registration to
// find one that can take work, and a cluster carrying a drained node reads as
// if every node were live.
func TestListContainerInstances_StatusSelectsOnlyThatStatus(t *testing.T) {
	svc, _ := ecsdomain.NewTestService(t)
	_, err := svc.CreateCluster(t.Context(), &ecs.CreateClusterInput{ClusterName: aws.String("web")}, ecsdomain.TestAccountID)
	require.NoError(t, err)
	ecsdomain.RegisterInstance(t, svc, "web", "i-live", 1024, 2048)
	ecsdomain.RegisterInstance(t, svc, "web", "i-drained", 1024, 2048)

	kv, err := svc.Bucket(t.Context(), ecsdomain.TestAccountID)
	require.NoError(t, err)
	drained, err := svc.UpsertInstance(t.Context(), kv, ecsdomain.TestAccountID, "web", "i-drained", func(r *ecsdomain.InstanceRecord) {
		r.Status = ecsdomain.InstanceStatusDraining
	})
	require.NoError(t, err)

	active := listInstanceARNs(t, svc, "web", ecsdomain.InstanceStatusActive)
	assert.Len(t, active, 1)
	assert.NotContains(t, active, drained.ARN)

	assert.Equal(t, []string{drained.ARN}, listInstanceARNs(t, svc, "web", ecsdomain.InstanceStatusDraining))
	assert.Len(t, listInstanceARNs(t, svc, "web", ""), 2)
}

// The statuses a registration passes through in AWS never reach a record here,
// so asking for one is a valid question with an empty answer rather than an
// error.
func TestListContainerInstances_TransitionalStatusMatchesNothing(t *testing.T) {
	svc, _ := ecsdomain.NewTestService(t)
	_, err := svc.CreateCluster(t.Context(), &ecs.CreateClusterInput{ClusterName: aws.String("web")}, ecsdomain.TestAccountID)
	require.NoError(t, err)
	ecsdomain.RegisterInstance(t, svc, "web", "i-live", 1024, 2048)

	for _, status := range []string{"REGISTERING", "DEREGISTERING", "REGISTRATION_FAILED"} {
		assert.Empty(t, listInstanceARNs(t, svc, "web", status), status)
	}
}

func TestListContainerInstances_UnknownStatusIsRefused(t *testing.T) {
	svc, _ := ecsdomain.NewTestService(t)
	_, err := svc.CreateCluster(t.Context(), &ecs.CreateClusterInput{ClusterName: aws.String("web")}, ecsdomain.TestAccountID)
	require.NoError(t, err)

	_, err = svc.ListContainerInstances(t.Context(), &ecs.ListContainerInstancesInput{
		Cluster: aws.String("web"),
		Status:  aws.String("INACTIVE"),
	}, ecsdomain.TestAccountID)
	require.Error(t, err)
}

// A filtered list and a describe of what it returned have to agree, since the
// status the filter selects on is the one describe reports.
func TestListContainerInstances_FilterAgreesWithDescribe(t *testing.T) {
	svc, _ := ecsdomain.NewTestService(t)
	_, err := svc.CreateCluster(t.Context(), &ecs.CreateClusterInput{ClusterName: aws.String("web")}, ecsdomain.TestAccountID)
	require.NoError(t, err)
	ecsdomain.RegisterInstance(t, svc, "web", "i-live", 1024, 2048)
	ecsdomain.RegisterInstance(t, svc, "web", "i-drained", 1024, 2048)

	kv, err := svc.Bucket(t.Context(), ecsdomain.TestAccountID)
	require.NoError(t, err)
	_, err = svc.UpsertInstance(t.Context(), kv, ecsdomain.TestAccountID, "web", "i-drained", func(r *ecsdomain.InstanceRecord) {
		r.Status = ecsdomain.InstanceStatusDraining
	})
	require.NoError(t, err)

	arns := listInstanceARNs(t, svc, "web", ecsdomain.InstanceStatusActive)
	require.NotEmpty(t, arns)
	out, err := svc.DescribeContainerInstances(t.Context(), &ecs.DescribeContainerInstancesInput{
		Cluster:            aws.String("web"),
		ContainerInstances: aws.StringSlice(arns),
	}, ecsdomain.TestAccountID)
	require.NoError(t, err)
	require.Len(t, out.ContainerInstances, len(arns))
	for _, ci := range out.ContainerInstances {
		assert.Equal(t, ecsdomain.InstanceStatusActive, aws.StringValue(ci.Status))
	}
}
