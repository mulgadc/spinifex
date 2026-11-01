package cluster

import (
	"context"
	"testing"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/eks"
	"github.com/mulgadc/spinifex/spinifex/foundation/aws/errors"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func managedWorkers(t *testing.T, f *eksServiceFixture, cluster string) []string {
	t.Helper()
	out, err := f.svc.ListManagedWorkers(context.Background(), &ListManagedWorkersInput{ClusterName: cluster}, testAccountID)
	require.NoError(t, err)
	return out.InstanceIDs
}

// A worker leaves the managed set exactly when its nodegroup terminates it, by
// scale-down or DeleteNodegroup, so the guest node-gc deletes its Node then and
// leaves every worker still owned by a nodegroup alone.
func TestListManagedWorkers_DropsWorkersTheNodegroupTerminated(t *testing.T) {
	t.Parallel()
	f := newEKSServiceFixture(t)
	seedActiveClusterWithToken(t, f, "c1")
	for _, ng := range []string{"ng1", "ng2"} {
		_, err := f.svc.CreateNodegroup(context.Background(), createNGInput("c1", ng, 2), testAccountID)
		require.NoError(t, err)
		markWorkersReady(t, f, "c1", ng, 2)
		f.svc.WaitLaunches()
	}
	ng1, err := GetNodegroupRecord(t.Context(), f.kv, "c1", "ng1")
	require.NoError(t, err)
	ng2, err := GetNodegroupRecord(t.Context(), f.kv, "c1", "ng2")
	require.NoError(t, err)
	require.Len(t, ng1.InstanceIDs, 2)
	assert.ElementsMatch(t, append(append([]string{}, ng1.InstanceIDs...), ng2.InstanceIDs...), managedWorkers(t, f, "c1"))

	_, err = f.svc.UpdateNodegroupConfig(context.Background(), &eks.UpdateNodegroupConfigInput{
		ClusterName:   aws.String("c1"),
		NodegroupName: aws.String("ng1"),
		ScalingConfig: &eks.NodegroupScalingConfig{DesiredSize: aws.Int64(1)},
	}, testAccountID)
	require.NoError(t, err)
	require.Len(t, f.worker.terminateCalls, 1)
	scaledIn := f.worker.terminateCalls[0]
	got := managedWorkers(t, f, "c1")
	assert.Len(t, got, 3)
	for _, id := range scaledIn {
		assert.NotContains(t, got, id, "a worker terminated by scale-down must leave the managed set")
	}

	_, err = f.svc.DeleteNodegroup(context.Background(), &eks.DeleteNodegroupInput{
		ClusterName: aws.String("c1"), NodegroupName: aws.String("ng2"),
	}, testAccountID)
	require.NoError(t, err)
	got = managedWorkers(t, f, "c1")
	assert.Len(t, got, 1)
	for _, id := range ng2.InstanceIDs {
		assert.NotContains(t, got, id, "a deleted nodegroup's workers must leave the managed set")
	}
}

func TestListManagedWorkers_UnknownClusterIsNotFound(t *testing.T) {
	t.Parallel()
	f := newEKSServiceFixture(t)

	_, err := f.svc.ListManagedWorkers(context.Background(), &ListManagedWorkersInput{ClusterName: "ghost"}, testAccountID)
	code, ok := awserrors.ResolveErrorCode(err)
	require.True(t, ok)
	assert.Equal(t, awserrors.ErrorEKSResourceNotFound, code)
}
