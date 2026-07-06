package handlers_eks

import (
	"context"
	"errors"
	"testing"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/aws/aws-sdk-go/service/eks"
	"github.com/mulgadc/spinifex/spinifex/admin"
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

	// CreateCluster accepts the request (CREATING) and launches asynchronously;
	// the launch failure surfaces as FAILED status, not the call's return value.
	_, err := f.svc.CreateCluster(context.Background(), createInput("alpha"), testAccountID, "")
	require.NoError(t, err)
	f.svc.WaitLaunches()

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

	_, err := f.svc.CreateCluster(context.Background(), createInput("alpha"), testAccountID, "")
	require.NoError(t, err)
	f.svc.WaitLaunches()

	// The NLB the partial create provisioned exists in the fake.
	require.NotEmpty(t, f.nlb.createLBCalls, "create provisioned an NLB")

	_, err = f.svc.DeleteCluster(context.Background(), deleteInput("alpha"), testAccountID)
	require.NoError(t, err)

	assert.NotEmpty(t, f.nlb.deleteLBCalls, "DeleteCluster must tear down the NLB recorded by the failed create")
	_, getErr := GetClusterMeta(f.kv, "alpha")
	assert.ErrorIs(t, getErr, ErrClusterNotFound)
}

// A post-placement failure (NLB target registration) must leave the just-launched
// control-plane VM refs persisted in the FAILED meta. Otherwise the live
// sys.medium VMs are unrecorded and neither DeleteCluster nor the FAILED-cluster
// reclaim can reach them — they leak.
func TestCreateCluster_PostPlacementFailureLeavesControlPlaneRecorded(t *testing.T) {
	f := newEKSServiceFixture(t)

	// Placement succeeds and launches the CP VM, then target registration fails.
	f.nlb.registerErr = errors.New("TargetGroupNotFound")

	_, err := f.svc.CreateCluster(context.Background(), createInput("alpha"), testAccountID, "")
	require.NoError(t, err)
	f.svc.WaitLaunches()
	require.NotEmpty(t, f.inst.launchCalls, "control-plane VM was launched before the register-targets step")

	meta, getErr := GetClusterMeta(f.kv, "alpha")
	require.NoError(t, getErr, "meta must remain so the leaked CP VM has an owning record")
	assert.Equal(t, ClusterStatusFailed, meta.Status)
	assert.NotEmpty(t, meta.ControlPlaneInstanceID, "CP instance ID must be persisted before the register-targets step")
	assert.NotEmpty(t, meta.ControlPlaneNodes, "CP node list must be persisted before the register-targets step")
}

// End-to-end: a create that fails after placement, then delete-cluster, must
// terminate the control-plane VM the partial create launched — driven by the CP
// refs the early persist recorded.
func TestCreateCluster_PostPlacementFailedThenDeleteTerminatesControlPlane(t *testing.T) {
	f := newEKSServiceFixture(t)
	f.nlb.registerErr = errors.New("TargetGroupNotFound")

	_, err := f.svc.CreateCluster(context.Background(), createInput("alpha"), testAccountID, "")
	require.NoError(t, err)
	f.svc.WaitLaunches()
	require.NotEmpty(t, f.inst.launchCalls, "create launched a CP VM")

	_, err = f.svc.DeleteCluster(context.Background(), deleteInput("alpha"), testAccountID)
	require.NoError(t, err)

	assert.NotEmpty(t, f.inst.terminateCalls, "DeleteCluster must terminate the CP VM recorded by the failed create")
	_, getErr := GetClusterMeta(f.kv, "alpha")
	assert.ErrorIs(t, getErr, ErrClusterNotFound)
}

// A create whose async launch fails after the NLB is provisioned must eagerly
// tear down the infra it already built — EKS names are unique per create, so no
// same-name retry ever fires the FAILED-cluster reclaim, and the internet-facing
// LB VM would otherwise keep its allocated+associated EIP indefinitely. The
// FAILED meta is retained for observability; purge is idempotent so a later
// DeleteCluster re-purge is a no-op.
func TestCreateCluster_FailedLaunchEagerlyPurgesInfra(t *testing.T) {
	f := newEKSServiceFixture(t)
	f.inst.launchErr = errors.New("no capacity")

	_, err := f.svc.CreateCluster(context.Background(), createInput("alpha"), testAccountID, "")
	require.NoError(t, err)
	f.svc.WaitLaunches()

	require.NotEmpty(t, f.nlb.createLBCalls, "launch provisioned an NLB before failing")
	// No DeleteCluster here: the failure handler itself must reap the NLB/LB VM.
	assert.NotEmpty(t, f.nlb.deleteLBCalls, "failed launch must eagerly tear down the NLB it provisioned")

	meta, getErr := GetClusterMeta(f.kv, "alpha")
	require.NoError(t, getErr, "FAILED meta is retained for observability")
	assert.Equal(t, ClusterStatusFailed, meta.Status)
}

// A live (CREATING/ACTIVE) cluster of the same name blocks create with a
// cluster-scoped ResourceInUseException, not the ELBv2 target-group message.
func TestCreateCluster_ExistingClusterReturnsResourceInUse(t *testing.T) {
	f := newEKSServiceFixture(t)

	meta := sampleClusterMeta("alpha")
	meta.Status = ClusterStatusActive
	require.NoError(t, PutClusterMeta(f.kv, meta))

	_, err := f.svc.CreateCluster(context.Background(), createInput("alpha"), testAccountID, "")
	require.EqualError(t, err, awserrors.ErrorEKSResourceInUse)
}

// A FAILED cluster from a prior attempt must not block a retry: create reclaims
// it (tearing down recorded resources) and proceeds to a fresh CREATING cluster.
func TestCreateCluster_FailedClusterIsReclaimedOnRetry(t *testing.T) {
	f := newEKSServiceFixture(t)

	// First attempt fails at VM launch, leaving a FAILED meta with NLB ARNs.
	f.inst.launchErr = errors.New("no capacity")
	_, err := f.svc.CreateCluster(context.Background(), createInput("alpha"), testAccountID, "")
	require.NoError(t, err)
	f.svc.WaitLaunches()
	failed, err := GetClusterMeta(f.kv, "alpha")
	require.NoError(t, err)
	require.Equal(t, ClusterStatusFailed, failed.Status)

	// Retry now succeeds: the reclaim (in the synchronous claim phase) purges the
	// failed attempt's NLB and the create starts clean. The relaunch then runs to
	// completion, leaving the cluster CREATING (ACTIVE is the reconciler's job).
	f.inst.launchErr = nil
	_, err = f.svc.CreateCluster(context.Background(), createInput("alpha"), testAccountID, "")
	require.NoError(t, err)
	f.svc.WaitLaunches()

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

	desc, err := f.svc.DescribeCluster(context.Background(), &eks.DescribeClusterInput{Name: aws.String("alpha")}, testAccountID)
	require.NoError(t, err)
	assert.Equal(t, eks.ClusterStatusFailed, aws.StringValue(desc.Cluster.Status))

	list, err := f.svc.ListClusters(context.Background(), &eks.ListClustersInput{}, testAccountID)
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

	desc, err := f.svc.DescribeCluster(context.Background(), &eks.DescribeClusterInput{Name: aws.String("alpha")}, testAccountID)
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

	desc, err := f.svc.DescribeCluster(context.Background(), &eks.DescribeClusterInput{Name: aws.String("alpha")}, testAccountID)
	require.NoError(t, err)
	assert.Nil(t, desc.Cluster.Health, "healthy cluster must not report a ClusterHealth issue")
}

// A missing eks-server AMI is an operator/config gap surfaced during the async
// launch: CreateCluster accepts the request (CREATING) and the background launch
// marks the half-built cluster FAILED so the reclaim path can retry (bead 165.4).
func TestCreateCluster_MissingAMIReturnsServiceUnavailable(t *testing.T) {
	f := newEKSServiceFixture(t)
	f.ami.describeOut = &ec2.DescribeImagesOutput{} // no images tagged managed-by=eks

	_, err := f.svc.CreateCluster(context.Background(), createInput("alpha"), testAccountID, "")
	require.NoError(t, err)
	f.svc.WaitLaunches()

	meta, getErr := GetClusterMeta(f.kv, "alpha")
	require.NoError(t, getErr)
	assert.Equal(t, ClusterStatusFailed, meta.Status)
}

func TestCreateCluster_HappyPathPersistsActiveCreatingMeta(t *testing.T) {
	f := newEKSServiceFixture(t)

	out, err := f.svc.CreateCluster(context.Background(), createInput("alpha"), testAccountID, "")
	require.NoError(t, err)
	require.NotNil(t, out)
	f.svc.WaitLaunches()

	meta, err := GetClusterMeta(f.kv, "alpha")
	require.NoError(t, err)
	assert.Equal(t, ClusterStatusCreating, meta.Status)
	assert.NotEmpty(t, meta.NLBArn)
	assert.NotEmpty(t, meta.ControlPlaneInstanceID)
	assert.NotEmpty(t, meta.ControlPlaneENIID)
}

// bootstrapClusterCreatorAdminPermissions defaults true: a successful create
// with a known caller principal ARN mints a system:masters AccessEntry for the
// caller, keyed by their exact ARN (the token webhook looks it up by the same).
func TestCreateCluster_SeedsCreatorAdminAccessEntry(t *testing.T) {
	f := newEKSServiceFixture(t)
	const caller = "arn:aws:iam::111122223333:role/admin"

	_, err := f.svc.CreateCluster(context.Background(), createInput("alpha"), testAccountID, caller)
	require.NoError(t, err)
	f.svc.WaitLaunches()

	rec, err := GetAccessEntryRecord(f.kv, "alpha", caller)
	require.NoError(t, err)
	assert.Equal(t, []string{"system:masters"}, rec.KubernetesGroups)
	assert.Equal(t, caller, rec.KubernetesUsername)
	assert.Equal(t, AccessEntryTypeStandard, rec.Type)
}

// With the bootstrap flag explicitly false, no creator-admin entry is minted.
func TestCreateCluster_SkipsCreatorAdminWhenDisabled(t *testing.T) {
	f := newEKSServiceFixture(t)
	const caller = "arn:aws:iam::111122223333:role/admin"
	in := createInput("alpha")
	in.AccessConfig = &eks.CreateAccessConfigRequest{
		BootstrapClusterCreatorAdminPermissions: aws.Bool(false),
	}

	_, err := f.svc.CreateCluster(context.Background(), in, testAccountID, caller)
	require.NoError(t, err)
	f.svc.WaitLaunches()

	_, err = GetAccessEntryRecord(f.kv, "alpha", caller)
	assert.ErrorIs(t, err, ErrAccessEntryNotFound)
}

// CreateCluster builds the managed control-plane VPC ("Set B") under the system
// account — the AWS-managed-account analogue the customer never provisions. The
// control-plane runs in its private subnet and egresses via the CP VPC's NAT
// gateway (no per-cluster hidden-pool EIP), so the persisted meta carries the CP
// VPC refs and the NAT gateway is provisioned exactly once.
func TestCreateCluster_BuildsManagedCPVPCUnderSystemAccount(t *testing.T) {
	f := newEKSServiceFixture(t)

	_, err := f.svc.CreateCluster(context.Background(), createInput("setb"), testAccountID, "")
	require.NoError(t, err)
	f.svc.WaitLaunches()

	meta, err := GetClusterMeta(f.kv, "setb")
	require.NoError(t, err)
	require.NotNil(t, meta.ManagedCPVPC, "managed CP VPC refs must be persisted")
	assert.NotEmpty(t, meta.ManagedCPVPC.VpcId)
	assert.NotEmpty(t, meta.ManagedCPVPC.PublicSubnetId)
	require.NotEmpty(t, meta.ManagedCPVPC.PrivateSubnetIds)
	assert.NotEmpty(t, meta.ManagedCPVPC.NatGatewayId, "private CP subnet egresses via the NAT gateway")

	// The CP VPC is composed under the system account, not the caller's.
	require.NotEmpty(t, f.vpcMgr.createVpcAccts)
	for _, acct := range f.vpcMgr.createVpcAccts {
		assert.Equal(t, admin.SystemAccountID(), acct, "managed CP VPC must be owned by the system account")
	}
	assert.Len(t, f.ngw.createCalls, 1, "exactly one NAT gateway for the CP VPC")

	// No per-cluster hidden-pool egress EIP is allocated anymore; the only EIP is
	// the NAT gateway's, tagged ManagedBy=eks so it stays out of customer listings.
	assert.Empty(t, meta.EgressEIPAllocationID, "hidden-pool egress EIP is gone — egress is via the NAT gateway")
	require.Len(t, f.eip.allocateCalls, 1, "one EIP: the NAT gateway address")
	tagged := false
	for _, spec := range f.eip.allocateCalls[0].TagSpecifications {
		for _, tg := range spec.Tags {
			if aws.StringValue(tg.Key) == tags.ManagedByKey && aws.StringValue(tg.Value) == tags.ManagedByEKS {
				tagged = true
			}
		}
	}
	assert.True(t, tagged, "NAT gateway EIP must carry the ManagedBy=eks tag")
}
