package handlers_eks

import (
	"errors"
	"testing"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/aws/aws-sdk-go/service/eks"
	"github.com/mulgadc/spinifex/spinifex/awserrors"
	"github.com/mulgadc/spinifex/spinifex/tags"
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
	f.inst.launchErr = errors.New("no capacity")

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
	f.inst.launchErr = errors.New("no capacity")

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

// A live (CREATING/ACTIVE) cluster of the same name blocks create with a
// cluster-scoped ResourceInUseException, not the ELBv2 target-group message.
func TestCreateCluster_ExistingClusterReturnsResourceInUse(t *testing.T) {
	f := newEKSServiceFixture(t)

	meta := sampleClusterMeta("alpha")
	meta.Status = ClusterStatusActive
	require.NoError(t, PutClusterMeta(f.kv, meta))

	_, err := f.svc.CreateCluster(createInput("alpha"), testAccountID)
	require.EqualError(t, err, awserrors.ErrorEKSResourceInUse)
}

// A FAILED cluster from a prior attempt must not block a retry: create reclaims
// it (tearing down recorded resources) and proceeds to a fresh CREATING cluster.
func TestCreateCluster_FailedClusterIsReclaimedOnRetry(t *testing.T) {
	f := newEKSServiceFixture(t)

	// First attempt fails at VM launch, leaving a FAILED meta with NLB ARNs.
	f.inst.launchErr = errors.New("no capacity")
	_, err := f.svc.CreateCluster(createInput("alpha"), testAccountID)
	require.Error(t, err)
	failed, err := GetClusterMeta(f.kv, "alpha")
	require.NoError(t, err)
	require.Equal(t, ClusterStatusFailed, failed.Status)

	// Retry now succeeds: the reclaim path purges the failed attempt's NLB and
	// the create starts clean.
	f.inst.launchErr = nil
	_, err = f.svc.CreateCluster(createInput("alpha"), testAccountID)
	require.NoError(t, err)

	got, err := GetClusterMeta(f.kv, "alpha")
	require.NoError(t, err)
	assert.Equal(t, ClusterStatusCreating, got.Status)
	assert.NotEmpty(t, f.nlb.deleteLBCalls, "reclaim must tear down the failed attempt's NLB")
}

// A FAILED cluster is observable via Describe and List (bead 165.6) — no state
// where create blocks but describe/list show nothing.
func TestCreateCluster_FailedClusterVisibleInDescribeAndList(t *testing.T) {
	f := newEKSServiceFixture(t)

	meta := sampleClusterMeta("alpha")
	meta.Status = ClusterStatusFailed
	meta.StatusReason = "bootstrap failed: boom"
	require.NoError(t, PutClusterMeta(f.kv, meta))

	desc, err := f.svc.DescribeCluster(&eks.DescribeClusterInput{Name: aws.String("alpha")}, testAccountID)
	require.NoError(t, err)
	assert.Equal(t, eks.ClusterStatusFailed, aws.StringValue(desc.Cluster.Status))

	list, err := f.svc.ListClusters(&eks.ListClustersInput{}, testAccountID)
	require.NoError(t, err)
	assert.Contains(t, aws.StringValueSlice(list.Clusters), "alpha")
}

// An ACTIVE cluster whose reconciler recorded a /healthz failure must surface
// that as a ClusterHealth issue in describe-cluster, so a dead control plane is
// visible behind the still-ACTIVE status (bead 165.13).
func TestDescribeCluster_SurfacesHealthIssueForActiveCluster(t *testing.T) {
	f := newEKSServiceFixture(t)

	meta := sampleClusterMeta("alpha")
	meta.Status = ClusterStatusActive
	require.NoError(t, PutClusterMeta(f.kv, meta))
	require.NoError(t, SetClusterHealth(f.kv, "alpha", "healthz request: connection refused"))

	desc, err := f.svc.DescribeCluster(&eks.DescribeClusterInput{Name: aws.String("alpha")}, testAccountID)
	require.NoError(t, err)
	assert.Equal(t, eks.ClusterStatusActive, aws.StringValue(desc.Cluster.Status))
	require.NotNil(t, desc.Cluster.Health)
	require.Len(t, desc.Cluster.Health.Issues, 1)
	assert.Equal(t, eks.ClusterIssueCodeClusterUnreachable, aws.StringValue(desc.Cluster.Health.Issues[0].Code))
	assert.Contains(t, aws.StringValue(desc.Cluster.Health.Issues[0].Message), "connection refused")
}

// A healthy ACTIVE cluster carries no health issue in describe-cluster.
func TestDescribeCluster_HealthyActiveClusterHasNoIssues(t *testing.T) {
	f := newEKSServiceFixture(t)

	meta := sampleClusterMeta("alpha")
	meta.Status = ClusterStatusActive
	require.NoError(t, PutClusterMeta(f.kv, meta))

	desc, err := f.svc.DescribeCluster(&eks.DescribeClusterInput{Name: aws.String("alpha")}, testAccountID)
	require.NoError(t, err)
	assert.Nil(t, desc.Cluster.Health, "healthy cluster must not report a ClusterHealth issue")
}

// A missing eks-server AMI is an operator/config gap, not an unrecoverable
// internal fault: CreateCluster must surface ServiceUnavailable (not a generic
// ServerInternal) and mark the half-built cluster FAILED (bead 165.4).
func TestCreateCluster_MissingAMIReturnsServiceUnavailable(t *testing.T) {
	f := newEKSServiceFixture(t)
	f.ami.describeOut = &ec2.DescribeImagesOutput{} // no images tagged managed-by=eks

	_, err := f.svc.CreateCluster(createInput("alpha"), testAccountID)
	require.EqualError(t, err, awserrors.ErrorServiceUnavailable)

	meta, getErr := GetClusterMeta(f.kv, "alpha")
	require.NoError(t, getErr)
	assert.Equal(t, ClusterStatusFailed, meta.Status)
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

func TestCreateCluster_AllocatesHiddenEgressEIP(t *testing.T) {
	f := newEKSServiceFixture(t)

	_, err := f.svc.CreateCluster(createInput("egress"), testAccountID)
	require.NoError(t, err)

	meta, err := GetClusterMeta(f.kv, "egress")
	require.NoError(t, err)
	assert.Equal(t, f.eip.publicIP, meta.EgressEIPPublicIP)
	assert.Equal(t, f.eip.allocationID, meta.EgressEIPAllocationID)

	require.Len(t, f.eip.allocateCalls, 1)
	// The pool address is tagged ManagedBy=eks so it stays out of customer
	// DescribeAddresses listings.
	tagged := false
	for _, spec := range f.eip.allocateCalls[0].TagSpecifications {
		for _, tg := range spec.Tags {
			if aws.StringValue(tg.Key) == tags.ManagedByKey && aws.StringValue(tg.Value) == tags.ManagedByEKS {
				tagged = true
			}
		}
	}
	assert.True(t, tagged, "egress EIP must carry the ManagedBy=eks tag")
}
