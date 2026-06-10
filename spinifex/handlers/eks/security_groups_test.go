package handlers_eks

import (
	"errors"
	"testing"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/mulgadc/spinifex/spinifex/awserrors"
	"github.com/mulgadc/spinifex/spinifex/tags"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type fakeSGProvisioner struct {
	createCalls    []*ec2.CreateSecurityGroupInput
	describeCalls  []*ec2.DescribeSecurityGroupsInput
	deleteCalls    []*ec2.DeleteSecurityGroupInput
	authorizeCalls []*ec2.AuthorizeSecurityGroupIngressInput

	// existing maps "name|vpcId" → groupId for DescribeSecurityGroups lookup.
	existing map[string]string

	// nextCreateID is returned by the next CreateSecurityGroup call. Tests can
	// pre-seed a queue via createIDs to differentiate the control-plane vs
	// nodegroup SG IDs.
	createIDs []string

	createErr    error
	describeErr  error
	deleteErr    error
	authorizeErr error
}

var _ sgProvisioner = (*fakeSGProvisioner)(nil)

func newFakeSGProvisioner() *fakeSGProvisioner {
	return &fakeSGProvisioner{existing: map[string]string{}}
}

func (f *fakeSGProvisioner) CreateSecurityGroup(input *ec2.CreateSecurityGroupInput, _ string) (*ec2.CreateSecurityGroupOutput, error) {
	f.createCalls = append(f.createCalls, input)
	if f.createErr != nil {
		return nil, f.createErr
	}
	id := "sg-default"
	if len(f.createIDs) > 0 {
		id = f.createIDs[0]
		f.createIDs = f.createIDs[1:]
	}
	if input.GroupName != nil && input.VpcId != nil {
		f.existing[*input.GroupName+"|"+*input.VpcId] = id
	}
	return &ec2.CreateSecurityGroupOutput{GroupId: aws.String(id)}, nil
}

func (f *fakeSGProvisioner) DescribeSecurityGroups(input *ec2.DescribeSecurityGroupsInput, _ string) (*ec2.DescribeSecurityGroupsOutput, error) {
	f.describeCalls = append(f.describeCalls, input)
	if f.describeErr != nil {
		return nil, f.describeErr
	}
	var name, vpc string
	for _, filt := range input.Filters {
		if filt == nil || filt.Name == nil || len(filt.Values) == 0 || filt.Values[0] == nil {
			continue
		}
		switch *filt.Name {
		case "group-name":
			name = *filt.Values[0]
		case "vpc-id":
			vpc = *filt.Values[0]
		}
	}
	id, ok := f.existing[name+"|"+vpc]
	out := &ec2.DescribeSecurityGroupsOutput{}
	if ok {
		out.SecurityGroups = []*ec2.SecurityGroup{{GroupId: aws.String(id), GroupName: aws.String(name), VpcId: aws.String(vpc)}}
	}
	return out, nil
}

func (f *fakeSGProvisioner) DeleteSecurityGroup(input *ec2.DeleteSecurityGroupInput, _ string) (*ec2.DeleteSecurityGroupOutput, error) {
	f.deleteCalls = append(f.deleteCalls, input)
	if f.deleteErr != nil {
		return nil, f.deleteErr
	}
	return &ec2.DeleteSecurityGroupOutput{}, nil
}

func (f *fakeSGProvisioner) AuthorizeSecurityGroupIngress(input *ec2.AuthorizeSecurityGroupIngressInput, _ string) (*ec2.AuthorizeSecurityGroupIngressOutput, error) {
	f.authorizeCalls = append(f.authorizeCalls, input)
	if f.authorizeErr != nil {
		return nil, f.authorizeErr
	}
	return &ec2.AuthorizeSecurityGroupIngressOutput{}, nil
}

func TestEnsureClusterSGs_EmptyInputsRejected(t *testing.T) {
	sgp := newFakeSGProvisioner()

	_, _, err := EnsureClusterSGs(sgp, "111122223333", "", "vpc-aaa")
	require.Error(t, err)

	_, _, err = EnsureClusterSGs(sgp, "111122223333", "alpha", "")
	require.Error(t, err)

	assert.Empty(t, sgp.createCalls)
	assert.Empty(t, sgp.describeCalls)
}

func TestEnsureClusterSGs_FreshCreatesBoth(t *testing.T) {
	sgp := newFakeSGProvisioner()
	sgp.createIDs = []string{"sg-cp-001", "sg-ng-002"}

	cpID, ngID, err := EnsureClusterSGs(sgp, "111122223333", "alpha", "vpc-aaa")
	require.NoError(t, err)
	assert.Equal(t, "sg-cp-001", cpID)
	assert.Equal(t, "sg-ng-002", ngID)

	require.Len(t, sgp.createCalls, 2)

	assertSGCreateTagged(t, sgp.createCalls[0], "eks-cluster-alpha-control-plane-sg", "vpc-aaa", "alpha", clusterEKSRoleControlPlane)
	assertSGCreateTagged(t, sgp.createCalls[1], "eks-cluster-alpha-nodegroup-sg", "vpc-aaa", "alpha", clusterEKSRoleNodegroup)
}

func TestEnsureClusterSGs_IdempotentReusesExisting(t *testing.T) {
	sgp := newFakeSGProvisioner()
	sgp.existing["eks-cluster-alpha-control-plane-sg|vpc-aaa"] = "sg-existing-cp"
	sgp.existing["eks-cluster-alpha-nodegroup-sg|vpc-aaa"] = "sg-existing-ng"

	cpID, ngID, err := EnsureClusterSGs(sgp, "111122223333", "alpha", "vpc-aaa")
	require.NoError(t, err)
	assert.Equal(t, "sg-existing-cp", cpID)
	assert.Equal(t, "sg-existing-ng", ngID)
	assert.Empty(t, sgp.createCalls, "no create calls expected when SGs already exist")
}

func TestEnsureClusterSGs_MixedExistenceCreatesMissing(t *testing.T) {
	sgp := newFakeSGProvisioner()
	sgp.existing["eks-cluster-alpha-control-plane-sg|vpc-aaa"] = "sg-existing-cp"
	sgp.createIDs = []string{"sg-new-ng"}

	cpID, ngID, err := EnsureClusterSGs(sgp, "111122223333", "alpha", "vpc-aaa")
	require.NoError(t, err)
	assert.Equal(t, "sg-existing-cp", cpID)
	assert.Equal(t, "sg-new-ng", ngID)

	require.Len(t, sgp.createCalls, 1, "only nodegroup SG should be created")
	assert.Equal(t, "eks-cluster-alpha-nodegroup-sg", aws.StringValue(sgp.createCalls[0].GroupName))
}

func TestEnsureClusterSGs_CreateErrorSurfacedFromControlPlane(t *testing.T) {
	sgp := newFakeSGProvisioner()
	sgp.createErr = errors.New("vpcd unavailable")

	_, _, err := EnsureClusterSGs(sgp, "111122223333", "alpha", "vpc-aaa")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "create SG eks-cluster-alpha-control-plane-sg")
}

func TestDeleteClusterSGs_DeletesBothExisting(t *testing.T) {
	sgp := newFakeSGProvisioner()
	sgp.existing["eks-cluster-alpha-control-plane-sg|vpc-aaa"] = "sg-existing-cp"
	sgp.existing["eks-cluster-alpha-nodegroup-sg|vpc-aaa"] = "sg-existing-ng"

	err := DeleteClusterSGs(sgp, "111122223333", "alpha", "vpc-aaa")
	require.NoError(t, err)

	require.Len(t, sgp.deleteCalls, 2)
	assert.Equal(t, "sg-existing-cp", aws.StringValue(sgp.deleteCalls[0].GroupId))
	assert.Equal(t, "sg-existing-ng", aws.StringValue(sgp.deleteCalls[1].GroupId))
}

func TestDeleteClusterSGs_MissingSGsNoOp(t *testing.T) {
	sgp := newFakeSGProvisioner()

	err := DeleteClusterSGs(sgp, "111122223333", "alpha", "vpc-aaa")
	require.NoError(t, err)
	assert.Empty(t, sgp.deleteCalls, "no delete calls expected when SGs already absent")
}

func TestDeleteClusterSGs_FirstErrorSurfacedSweepContinues(t *testing.T) {
	sgp := newFakeSGProvisioner()
	sgp.existing["eks-cluster-alpha-control-plane-sg|vpc-aaa"] = "sg-existing-cp"
	sgp.existing["eks-cluster-alpha-nodegroup-sg|vpc-aaa"] = "sg-existing-ng"
	sgp.deleteErr = errors.New("DependencyViolation")

	err := DeleteClusterSGs(sgp, "111122223333", "alpha", "vpc-aaa")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "sg-existing-cp")
	assert.Len(t, sgp.deleteCalls, 2, "both delete calls should be attempted despite first error")
}

func TestEnsureControlPlaneIngress_AuthorizesAPIServerFromVPCCIDR(t *testing.T) {
	sgp := newFakeSGProvisioner()

	err := EnsureControlPlaneIngress(sgp, "111122223333", "sg-cp-001", "10.0.0.0/16")
	require.NoError(t, err)

	require.Len(t, sgp.authorizeCalls, 1)
	in := sgp.authorizeCalls[0]
	assert.Equal(t, "sg-cp-001", aws.StringValue(in.GroupId))
	require.Len(t, in.IpPermissions, 1)
	perm := in.IpPermissions[0]
	assert.Equal(t, "tcp", aws.StringValue(perm.IpProtocol))
	assert.Equal(t, k3sAPIServerPort, aws.Int64Value(perm.FromPort))
	assert.Equal(t, k3sAPIServerPort, aws.Int64Value(perm.ToPort))
	require.Len(t, perm.IpRanges, 1)
	assert.Equal(t, "10.0.0.0/16", aws.StringValue(perm.IpRanges[0].CidrIp))
}

func TestEnsureControlPlaneIngress_EmptyInputsRejected(t *testing.T) {
	sgp := newFakeSGProvisioner()

	require.Error(t, EnsureControlPlaneIngress(sgp, "111122223333", "", "10.0.0.0/16"))
	require.Error(t, EnsureControlPlaneIngress(sgp, "111122223333", "sg-cp-001", ""))
	assert.Empty(t, sgp.authorizeCalls)
}

func TestEnsureControlPlaneIngress_DuplicateTolerated(t *testing.T) {
	sgp := newFakeSGProvisioner()
	sgp.authorizeErr = errors.New(awserrors.ErrorInvalidPermissionDuplicate)

	err := EnsureControlPlaneIngress(sgp, "111122223333", "sg-cp-001", "10.0.0.0/16")
	require.NoError(t, err, "a duplicate rule on re-run must be treated as success")
}

func TestEnsureControlPlaneIngress_AuthorizeErrorSurfaced(t *testing.T) {
	sgp := newFakeSGProvisioner()
	sgp.authorizeErr = errors.New("vpcd unavailable")

	err := EnsureControlPlaneIngress(sgp, "111122223333", "sg-cp-001", "10.0.0.0/16")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "sg-cp-001")
}

func TestEnsureControlPlaneHAIngress_AuthorizesEtcdAndKubeletSelfReferenced(t *testing.T) {
	sgp := newFakeSGProvisioner()

	err := EnsureControlPlaneHAIngress(sgp, "111122223333", "sg-cp-001")
	require.NoError(t, err)

	require.Len(t, sgp.authorizeCalls, 2, "etcd peer/client + kubelet")

	// Collect the authorized (proto, from, to) tuples and assert each rule is
	// self-referencing on the control-plane SG (source == target group).
	type portRange struct {
		proto    string
		from, to int64
	}
	got := map[portRange]bool{}
	for _, in := range sgp.authorizeCalls {
		assert.Equal(t, "sg-cp-001", aws.StringValue(in.GroupId))
		require.Len(t, in.IpPermissions, 1)
		perm := in.IpPermissions[0]
		assert.Empty(t, perm.IpRanges, "HA rules are group-referenced, not CIDR")
		require.Len(t, perm.UserIdGroupPairs, 1)
		assert.Equal(t, "sg-cp-001", aws.StringValue(perm.UserIdGroupPairs[0].GroupId),
			"control-plane SG must reference itself so the rule survives CP churn")
		got[portRange{
			aws.StringValue(perm.IpProtocol),
			aws.Int64Value(perm.FromPort),
			aws.Int64Value(perm.ToPort),
		}] = true
	}

	// 2380 (etcd peer) is the port whose absence silently breaks the join quorum.
	assert.True(t, got[portRange{"tcp", 2379, 2380}], "etcd client+peer 2379-2380 must be opened")
	assert.True(t, got[portRange{"tcp", 10250, 10250}], "kubelet 10250 must be opened")
}

func TestEnsureControlPlaneHAIngress_EmptyInputRejected(t *testing.T) {
	sgp := newFakeSGProvisioner()

	require.Error(t, EnsureControlPlaneHAIngress(sgp, "111122223333", ""))
	assert.Empty(t, sgp.authorizeCalls)
}

func TestEnsureControlPlaneHAIngress_DuplicateTolerated(t *testing.T) {
	sgp := newFakeSGProvisioner()
	sgp.authorizeErr = errors.New(awserrors.ErrorInvalidPermissionDuplicate)

	err := EnsureControlPlaneHAIngress(sgp, "111122223333", "sg-cp-001")
	require.NoError(t, err, "a duplicate rule on re-run must be treated as success")
}

func TestEnsureControlPlaneHAIngress_AuthorizeErrorSurfaced(t *testing.T) {
	sgp := newFakeSGProvisioner()
	sgp.authorizeErr = errors.New("vpcd unavailable")

	err := EnsureControlPlaneHAIngress(sgp, "111122223333", "sg-cp-001")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "sg-cp-001")
}

func assertSGCreateTagged(t *testing.T, in *ec2.CreateSecurityGroupInput, name, vpcID, clusterName, role string) {
	t.Helper()
	require.NotNil(t, in)
	assert.Equal(t, name, aws.StringValue(in.GroupName))
	assert.Equal(t, vpcID, aws.StringValue(in.VpcId))
	require.Len(t, in.TagSpecifications, 1)
	spec := in.TagSpecifications[0]
	require.NotNil(t, spec)
	assert.Equal(t, "security-group", aws.StringValue(spec.ResourceType))

	got := map[string]string{}
	for _, tg := range spec.Tags {
		if tg == nil || tg.Key == nil || tg.Value == nil {
			continue
		}
		got[*tg.Key] = *tg.Value
	}
	assert.Equal(t, tags.ManagedByEKS, got[tags.ManagedByKey])
	assert.Equal(t, clusterName, got[clusterEKSClusterTagKey])
	assert.Equal(t, role, got[clusterEKSRoleTagKey])
}
