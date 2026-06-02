package handlers_eks

import (
	"errors"
	"testing"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/eks"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func createInput(name string) *eks.CreateClusterInput {
	return &eks.CreateClusterInput{
		Name:    aws.String(name),
		RoleArn: aws.String("arn:aws:iam::111122223333:role/eks-cluster"),
		Version: aws.String("1.32"),
		ResourcesVpcConfig: &eks.VpcConfigRequest{
			SubnetIds: aws.StringSlice([]string{"subnet-aaa"}),
		},
	}
}

// A create that fails after the NLB is provisioned must leave the NLB ARNs
// persisted on the (now FAILED) meta, otherwise the resources leak with no
// owning record and DeleteCluster cannot reclaim them (bead 165.3).
func TestCreateCluster_NLBArnPersistedBeforeLaterFailure(t *testing.T) {
	f := newEKSServiceFixture(t)

	// Force the K3s VM launch to fail — this is after EnsureClusterNLB and the
	// NLB-arn persist checkpoint, but before the final PutClusterMeta.
	f.inst.runErr = errors.New("no capacity")

	_, err := f.svc.CreateCluster(createInput("alpha"), testAccountID)
	require.Error(t, err)

	meta, getErr := GetClusterMeta(f.kv, "alpha")
	require.NoError(t, getErr, "meta must remain so the leaked NLB has an owning record")
	assert.Equal(t, ClusterStatusFailed, meta.Status)
	assert.NotEmpty(t, meta.NLBArn, "NLB ARN must be persisted before the VM-launch step")
	assert.NotEmpty(t, meta.NLBTargetGroupArn)
}

// End-to-end: a failed create followed by delete-cluster leaves zero orphaned
// NLB resources. The persisted ARNs from the partial create drive teardown.
func TestCreateCluster_FailedCreateThenDeleteReclaimsNLB(t *testing.T) {
	f := newEKSServiceFixture(t)
	f.inst.runErr = errors.New("no capacity")

	_, err := f.svc.CreateCluster(createInput("alpha"), testAccountID)
	require.Error(t, err)

	// The NLB the partial create provisioned exists in the fake.
	require.NotEmpty(t, f.nlb.createLBCalls, "create provisioned an NLB")

	_, err = f.svc.DeleteCluster(deleteInput("alpha"), testAccountID)
	require.NoError(t, err)

	assert.NotEmpty(t, f.nlb.deleteLBCalls, "DeleteCluster must tear down the NLB recorded by the failed create")
	_, getErr := GetClusterMeta(f.kv, "alpha")
	assert.ErrorIs(t, getErr, ErrClusterNotFound)
}

func TestCreateCluster_HappyPathPersistsActiveCreatingMeta(t *testing.T) {
	f := newEKSServiceFixture(t)

	out, err := f.svc.CreateCluster(createInput("alpha"), testAccountID)
	require.NoError(t, err)
	require.NotNil(t, out)

	meta, err := GetClusterMeta(f.kv, "alpha")
	require.NoError(t, err)
	assert.Equal(t, ClusterStatusCreating, meta.Status)
	assert.NotEmpty(t, meta.NLBArn)
	assert.NotEmpty(t, meta.ControlPlaneInstanceID)
	assert.NotEmpty(t, meta.ControlPlaneENIID)
}
