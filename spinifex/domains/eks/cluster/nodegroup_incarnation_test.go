package cluster

import (
	"context"
	"testing"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/eks"
	"github.com/mulgadc/spinifex/spinifex/foundation/aws/errors"
	"github.com/mulgadc/spinifex/spinifex/foundation/lifecycle/incarnation"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// recreatedNodegroup deletes c1/ng1 and creates it again under the same name,
// returning the ARNs of the deleted and the replacing incarnation.
func recreatedNodegroup(t *testing.T, f *eksServiceFixture) (earlier, current string) {
	t.Helper()
	seedActiveClusterWithToken(t, f, "c1")
	createReadyNodegroup(t, f, "c1", "ng1", 2)
	first, err := GetNodegroupRecord(t.Context(), f.kv, "c1", "ng1")
	require.NoError(t, err)
	_, err = deleteNodegroupCall(f, "c1", "ng1")
	require.NoError(t, err)
	sweepNodegroups(t, f)
	createReadyNodegroup(t, f, "c1", "ng1", 2)
	second, err := GetNodegroupRecord(t.Context(), f.kv, "c1", "ng1")
	require.NoError(t, err)
	require.NotEqual(t, first.Arn, second.Arn)
	return first.Arn, second.Arn
}

// A request authorized against a deleted group, or against no group, must not
// read or change the same-name group that exists when the daemon runs it.
func TestNodegroupOps_ActOnlyOnTheAuthorizedIncarnation(t *testing.T) {
	t.Parallel()
	f := newEKSServiceFixture(t)
	earlier, current := recreatedNodegroup(t, f)
	before, err := GetNodegroupRecord(t.Context(), f.kv, "c1", "ng1")
	require.NoError(t, err)
	terminatesBefore := len(f.worker.terminateCalls)

	for _, ctx := range []context.Context{
		incarnation.With(context.Background(), earlier),
		incarnation.With(context.Background(), ""),
	} {
		_, err := f.svc.DescribeNodegroup(ctx, &eks.DescribeNodegroupInput{
			ClusterName: aws.String("c1"), NodegroupName: aws.String("ng1"),
		}, testAccountID)
		requireErrorCode(t, err, awserrors.ErrorEKSResourceNotFound)

		_, err = f.svc.UpdateNodegroupConfig(ctx, &eks.UpdateNodegroupConfigInput{
			ClusterName: aws.String("c1"), NodegroupName: aws.String("ng1"),
			ScalingConfig: &eks.NodegroupScalingConfig{DesiredSize: aws.Int64(1)},
		}, testAccountID)
		requireErrorCode(t, err, awserrors.ErrorEKSResourceNotFound)

		_, err = f.svc.TagResource(ctx, &eks.TagResourceInput{
			ResourceArn: aws.String(current), Tags: aws.StringMap(map[string]string{"team": "x"}),
		}, testAccountID)
		requireErrorCode(t, err, awserrors.ErrorEKSResourceNotFound)

		_, err = f.svc.ListTagsForResource(ctx, &eks.ListTagsForResourceInput{ResourceArn: aws.String(current)}, testAccountID)
		requireErrorCode(t, err, awserrors.ErrorEKSResourceNotFound)

		_, err = f.svc.DeleteNodegroup(ctx, &eks.DeleteNodegroupInput{
			ClusterName: aws.String("c1"), NodegroupName: aws.String("ng1"),
		}, testAccountID)
		requireErrorCode(t, err, awserrors.ErrorEKSResourceNotFound)
	}

	after, err := GetNodegroupRecord(t.Context(), f.kv, "c1", "ng1")
	require.NoError(t, err)
	assert.Equal(t, before, after, "the replacing group must be untouched")
	assert.Len(t, f.worker.terminateCalls, terminatesBefore, "no worker of the replacing group may be terminated")

	// The authorized incarnation itself is still served.
	out, err := f.svc.DescribeNodegroup(incarnation.With(context.Background(), current), &eks.DescribeNodegroupInput{
		ClusterName: aws.String("c1"), NodegroupName: aws.String("ng1"),
	}, testAccountID)
	require.NoError(t, err)
	assert.Equal(t, current, aws.StringValue(out.Nodegroup.NodegroupArn))
}

// A worker launched for a group that was replaced mid-scale must not join the
// replacement, and must not be left running unrecorded.
func TestRecordLaunchedWorker_RefusesAReplacedIncarnation(t *testing.T) {
	t.Parallel()
	f := newEKSServiceFixture(t)
	earlier, _ := recreatedNodegroup(t, f)
	before, err := GetNodegroupRecord(t.Context(), f.kv, "c1", "ng1")
	require.NoError(t, err)

	recorded, err := f.svc.recordLaunchedWorker(t.Context(), f.kv, testAccountID, "c1", "ng1", earlier, "i-late")
	requireErrorCode(t, err, awserrors.ErrorEKSResourceNotFound)
	assert.False(t, recorded)

	after, err := GetNodegroupRecord(t.Context(), f.kv, "c1", "ng1")
	require.NoError(t, err)
	assert.Equal(t, before.InstanceIDs, after.InstanceIDs)
	require.NotEmpty(t, f.worker.terminateCalls)
	assert.Equal(t, []string{"i-late"}, f.worker.terminateCalls[len(f.worker.terminateCalls)-1])
}
