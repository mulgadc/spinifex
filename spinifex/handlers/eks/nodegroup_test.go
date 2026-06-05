package handlers_eks

import (
	"errors"
	"strconv"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/aws/aws-sdk-go/service/eks"
	"github.com/mulgadc/spinifex/spinifex/awserrors"
	handlers_iam "github.com/mulgadc/spinifex/spinifex/handlers/iam"
	"github.com/mulgadc/spinifex/spinifex/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// fakeWorkerLauncher records RunWorkerInstance / TerminateWorkerInstances calls
// and hands back sequential instance IDs so tests can assert on the IDs a
// nodegroup tracks.
type fakeWorkerLauncher struct {
	runCalls       []*ec2.RunInstancesInput
	terminateCalls [][]string

	nextID  int
	runErr  error
	termErr error
}

var _ WorkerLauncher = (*fakeWorkerLauncher)(nil)

func newFakeWorkerLauncher() *fakeWorkerLauncher {
	return &fakeWorkerLauncher{}
}

func (f *fakeWorkerLauncher) RunWorkerInstance(input *ec2.RunInstancesInput, _ string) (*ec2.Reservation, error) {
	f.runCalls = append(f.runCalls, input)
	if f.runErr != nil {
		return nil, f.runErr
	}
	f.nextID++
	id := "i-worker" + strconv.Itoa(f.nextID)
	return &ec2.Reservation{Instances: []*ec2.Instance{{InstanceId: aws.String(id)}}}, nil
}

func (f *fakeWorkerLauncher) TerminateWorkerInstances(ids []string, _ string) error {
	f.terminateCalls = append(f.terminateCalls, ids)
	return f.termErr
}

// seedActiveClusterWithToken lays down an ACTIVE cluster meta with the control-
// plane ENI IP + VPC populated and an encrypted K3s join token, so the
// nodegroup path can run end-to-end.
func seedActiveClusterWithToken(t *testing.T, f *eksServiceFixture, cluster string) {
	t.Helper()
	meta := &ClusterMeta{
		Name:              cluster,
		Status:            ClusterStatusActive,
		Version:           "1.32",
		ControlPlaneENIIP: "10.0.1.42",
		ResourcesVpcConfig: &ClusterVpcConfig{
			SubnetIds: []string{"subnet-aaa"},
			VpcId:     "vpc-aaa",
		},
	}
	require.NoError(t, PutClusterMeta(f.kv, meta))

	ct, err := handlers_iam.EncryptSecret("K10node-join-token::server:abc", bootstrapTestMasterKey)
	require.NoError(t, err)
	_, err = f.kv.Put(NodeTokenKey(cluster), []byte(ct))
	require.NoError(t, err)
}

func createNGInput(cluster, ng string, desired int64) *eks.CreateNodegroupInput {
	return &eks.CreateNodegroupInput{
		ClusterName:   aws.String(cluster),
		NodegroupName: aws.String(ng),
		NodeRole:      aws.String("arn:aws:iam::111122223333:role/eks-node"),
		Subnets:       aws.StringSlice([]string{"subnet-aaa"}),
		ScalingConfig: &eks.NodegroupScalingConfig{
			MinSize:     aws.Int64(1),
			MaxSize:     aws.Int64(3),
			DesiredSize: aws.Int64(desired),
		},
	}
}

func TestNodegroupRecord_CRUDRoundTrip(t *testing.T) {
	_, _, js := testutil.StartTestJetStream(t)
	kv, err := GetOrCreateAccountBucket(js, testAccountID)
	require.NoError(t, err)

	rec := &NodegroupRecord{
		ClusterName:    "c1",
		Name:           "ng1",
		Arn:            "arn:aws:eks:us-east-1:111122223333:nodegroup/c1/ng1/abc",
		Status:         eks.NodegroupStatusActive,
		Subnets:        []string{"subnet-aaa"},
		InstanceTypes:  []string{"t3.medium"},
		ScalingMin:     1,
		ScalingMax:     3,
		ScalingDesired: 2,
		InstanceIDs:    []string{"i-1", "i-2"},
	}
	require.NoError(t, PutNodegroupRecord(kv, rec))

	got, err := GetNodegroupRecord(kv, "c1", "ng1")
	require.NoError(t, err)
	assert.Equal(t, rec.InstanceIDs, got.InstanceIDs)
	assert.Equal(t, int64(2), got.ScalingDesired)

	list, err := ListNodegroupRecords(kv, "c1")
	require.NoError(t, err)
	require.Len(t, list, 1)
	assert.Equal(t, "ng1", list[0].Name)

	require.NoError(t, DeleteNodegroupRecord(kv, "c1", "ng1"))
	_, err = GetNodegroupRecord(kv, "c1", "ng1")
	assert.ErrorIs(t, err, ErrNodegroupNotFound)

	// Delete of an absent record is idempotent.
	require.NoError(t, DeleteNodegroupRecord(kv, "c1", "ng1"))
}

func TestBuildAgentUserData_Shape(t *testing.T) {
	ud := buildAgentUserData(agentUserDataInput{
		ClusterName:       "c1",
		NodegroupName:     "ng1",
		ControlPlaneENIIP: "10.0.1.42",
		JoinToken:         "K10secret::server:xyz",
		NodeName:          "c1-ng1-abc123de",
	})

	assert.True(t, strings.HasPrefix(ud, "#cloud-config\n"))
	assert.Contains(t, ud, "SPINIFEX_K3S_ROLE=agent")
	assert.Contains(t, ud, "K3S_URL=https://10.0.1.42:6443")
	assert.Contains(t, ud, "K3S_TOKEN=K10secret::server:xyz")
	assert.Contains(t, ud, "K3S_NODE_NAME=c1-ng1-abc123de")
	assert.Contains(t, ud, "eks.amazonaws.com/nodegroup=ng1")
	assert.Contains(t, ud, agentEnvPath)
	// IMDS on-link route line present.
	assert.Contains(t, ud, "ip route replace "+imdsServerIP+"/32")
	// Exactly one write_files block (single "write_files:" key).
	assert.Equal(t, 1, strings.Count(ud, "write_files:"))
}

func TestEnsureNodegroupSGRules_AuthorizesExpectedRules(t *testing.T) {
	sg := newFakeSGProvisioner()

	require.NoError(t, EnsureNodegroupSGRules(sg, testAccountID, "c1", "sg-cp", "sg-ng"))

	require.Len(t, sg.authorizeCalls, 6)

	// Verify the apiserver rule: 6443/tcp on cpSG sourced from ngSG.
	var found6443 bool
	for _, call := range sg.authorizeCalls {
		require.Len(t, call.IpPermissions, 1)
		perm := call.IpPermissions[0]
		if aws.StringValue(call.GroupId) == "sg-cp" &&
			aws.StringValue(perm.IpProtocol) == "tcp" &&
			aws.Int64Value(perm.FromPort) == 6443 {
			require.Len(t, perm.UserIdGroupPairs, 1)
			assert.Equal(t, "sg-ng", aws.StringValue(perm.UserIdGroupPairs[0].GroupId))
			assert.Empty(t, perm.IpRanges, "rules must be group-to-group, not CIDR")
			found6443 = true
		}
	}
	assert.True(t, found6443, "expected agent→apiserver 6443/tcp rule")
}

func TestEnsureNodegroupSGRules_DuplicateRuleTolerated(t *testing.T) {
	sg := newFakeSGProvisioner()
	sg.authorizeErr = errors.New(awserrors.ErrorInvalidPermissionDuplicate)

	require.NoError(t, EnsureNodegroupSGRules(sg, testAccountID, "c1", "sg-cp", "sg-ng"),
		"duplicate-rule error must be treated as success")
}

func TestCreateNodegroup_HappyPath(t *testing.T) {
	f := newEKSServiceFixture(t)
	seedActiveClusterWithToken(t, f, "c1")

	out, err := f.svc.CreateNodegroup(createNGInput("c1", "ng1", 2), testAccountID)
	require.NoError(t, err)
	require.NotNil(t, out.Nodegroup)
	assert.Equal(t, eks.NodegroupStatusActive, aws.StringValue(out.Nodegroup.Status))
	assert.Equal(t, int64(2), aws.Int64Value(out.Nodegroup.ScalingConfig.DesiredSize))
	assert.Contains(t, aws.StringValue(out.Nodegroup.NodegroupArn), ":nodegroup/c1/ng1/")

	// Two workers launched and tracked.
	assert.Len(t, f.worker.runCalls, 2)
	rec, err := GetNodegroupRecord(f.kv, "c1", "ng1")
	require.NoError(t, err)
	assert.Len(t, rec.InstanceIDs, 2)

	// Workers go through the customer path: no ManagedBy tag, SG = nodegroup SG.
	for _, call := range f.worker.runCalls {
		require.NotEmpty(t, call.SecurityGroupIds)
		assert.Equal(t, "subnet-aaa", aws.StringValue(call.SubnetId))
		for _, ts := range call.TagSpecifications {
			for _, tg := range ts.Tags {
				assert.NotEqual(t, "spinifex:managed-by", aws.StringValue(tg.Key))
			}
		}
	}

	// SG ingress rules were authorized.
	assert.NotEmpty(t, f.sg.authorizeCalls)

	// Duplicate create → ResourceInUseException.
	_, err = f.svc.CreateNodegroup(createNGInput("c1", "ng1", 2), testAccountID)
	require.EqualError(t, err, awserrors.ErrorEKSResourceInUse)
}

func TestCreateNodegroup_ClusterNotActive(t *testing.T) {
	f := newEKSServiceFixture(t)
	require.NoError(t, PutClusterMeta(f.kv, &ClusterMeta{
		Name:               "c1",
		Status:             ClusterStatusCreating,
		ControlPlaneENIIP:  "10.0.1.42",
		ResourcesVpcConfig: &ClusterVpcConfig{VpcId: "vpc-aaa"},
	}))

	_, err := f.svc.CreateNodegroup(createNGInput("c1", "ng1", 1), testAccountID)
	require.EqualError(t, err, awserrors.ErrorInvalidRequest)
}

func TestCreateNodegroup_UnknownCluster(t *testing.T) {
	f := newEKSServiceFixture(t)
	_, err := f.svc.CreateNodegroup(createNGInput("ghost", "ng1", 1), testAccountID)
	require.EqualError(t, err, awserrors.ErrorEKSResourceNotFound)
}

func TestDescribeAndListNodegroups(t *testing.T) {
	f := newEKSServiceFixture(t)
	seedActiveClusterWithToken(t, f, "c1")
	_, err := f.svc.CreateNodegroup(createNGInput("c1", "ng1", 1), testAccountID)
	require.NoError(t, err)

	desc, err := f.svc.DescribeNodegroup(&eks.DescribeNodegroupInput{
		ClusterName: aws.String("c1"), NodegroupName: aws.String("ng1"),
	}, testAccountID)
	require.NoError(t, err)
	assert.Equal(t, "ng1", aws.StringValue(desc.Nodegroup.NodegroupName))
	assert.Equal(t, "c1", aws.StringValue(desc.Nodegroup.ClusterName))

	list, err := f.svc.ListNodegroups(&eks.ListNodegroupsInput{ClusterName: aws.String("c1")}, testAccountID)
	require.NoError(t, err)
	assert.Equal(t, []string{"ng1"}, aws.StringValueSlice(list.Nodegroups))

	_, err = f.svc.DescribeNodegroup(&eks.DescribeNodegroupInput{
		ClusterName: aws.String("c1"), NodegroupName: aws.String("ghost"),
	}, testAccountID)
	require.EqualError(t, err, awserrors.ErrorEKSResourceNotFound)
}

func TestUpdateNodegroupConfig_ScaleUpAndDown(t *testing.T) {
	f := newEKSServiceFixture(t)
	seedActiveClusterWithToken(t, f, "c1")
	_, err := f.svc.CreateNodegroup(createNGInput("c1", "ng1", 1), testAccountID)
	require.NoError(t, err)
	require.Len(t, f.worker.runCalls, 1)

	// Scale up 1 → 3: two new workers launched.
	upOut, err := f.svc.UpdateNodegroupConfig(&eks.UpdateNodegroupConfigInput{
		ClusterName:   aws.String("c1"),
		NodegroupName: aws.String("ng1"),
		ScalingConfig: &eks.NodegroupScalingConfig{DesiredSize: aws.Int64(3)},
	}, testAccountID)
	require.NoError(t, err)
	assert.Equal(t, eks.UpdateStatusSuccessful, aws.StringValue(upOut.Update.Status))
	assert.Len(t, f.worker.runCalls, 3)
	rec, err := GetNodegroupRecord(f.kv, "c1", "ng1")
	require.NoError(t, err)
	assert.Len(t, rec.InstanceIDs, 3)

	// Scale down 3 → 1: surplus (last two) terminated.
	_, err = f.svc.UpdateNodegroupConfig(&eks.UpdateNodegroupConfigInput{
		ClusterName:   aws.String("c1"),
		NodegroupName: aws.String("ng1"),
		ScalingConfig: &eks.NodegroupScalingConfig{DesiredSize: aws.Int64(1)},
	}, testAccountID)
	require.NoError(t, err)
	require.Len(t, f.worker.terminateCalls, 1)
	assert.Len(t, f.worker.terminateCalls[0], 2)
	rec, err = GetNodegroupRecord(f.kv, "c1", "ng1")
	require.NoError(t, err)
	assert.Len(t, rec.InstanceIDs, 1)
}

func TestDeleteNodegroup_TerminatesAndIsIdempotent(t *testing.T) {
	f := newEKSServiceFixture(t)
	seedActiveClusterWithToken(t, f, "c1")
	_, err := f.svc.CreateNodegroup(createNGInput("c1", "ng1", 2), testAccountID)
	require.NoError(t, err)

	_, err = f.svc.DeleteNodegroup(&eks.DeleteNodegroupInput{
		ClusterName: aws.String("c1"), NodegroupName: aws.String("ng1"),
	}, testAccountID)
	require.NoError(t, err)
	require.Len(t, f.worker.terminateCalls, 1)
	assert.Len(t, f.worker.terminateCalls[0], 2)

	_, err = GetNodegroupRecord(f.kv, "c1", "ng1")
	assert.ErrorIs(t, err, ErrNodegroupNotFound)

	// Second delete → ResourceNotFoundException (record already gone).
	_, err = f.svc.DeleteNodegroup(&eks.DeleteNodegroupInput{
		ClusterName: aws.String("c1"), NodegroupName: aws.String("ng1"),
	}, testAccountID)
	require.EqualError(t, err, awserrors.ErrorEKSResourceNotFound)
}

func TestUpdateNodegroupVersion_NotImplemented(t *testing.T) {
	f := newEKSServiceFixture(t)
	_, err := f.svc.UpdateNodegroupVersion(&eks.UpdateNodegroupVersionInput{
		ClusterName: aws.String("c1"), NodegroupName: aws.String("ng1"),
	}, testAccountID)
	require.EqualError(t, err, awserrors.ErrorNotImplemented)
}
