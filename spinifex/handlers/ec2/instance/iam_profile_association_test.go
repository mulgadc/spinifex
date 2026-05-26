package handlers_ec2_instance

import (
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/mulgadc/spinifex/spinifex/awserrors"
	spxtypes "github.com/mulgadc/spinifex/spinifex/types"
	"github.com/mulgadc/spinifex/spinifex/vm"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	testIAMAccount  = "111122223333"
	testIAMOther    = "999988887777"
	testProfileArn1 = "arn:aws:iam::111122223333:instance-profile/app-profile"
	testProfileArn2 = "arn:aws:iam::111122223333:instance-profile/other-profile"
)

func vmWithAccount(id, accountID string) *vm.VM {
	return &vm.VM{ID: id, AccountID: accountID, Status: vm.StateRunning}
}

func TestAssociateIamInstanceProfile_Success(t *testing.T) {
	v := vmWithAccount("i-assoc1", testIAMAccount)
	svc := &InstanceServiceImpl{vmMgr: mgrWith(map[string]*vm.VM{v.ID: v})}

	cmd := spxtypes.EC2InstanceCommand{
		ID:                        v.ID,
		Attributes:                spxtypes.EC2CommandAttributes{AssociateIamInstanceProfile: true},
		IamProfileAssociationData: &spxtypes.IamProfileAssociationData{InstanceProfileArn: testProfileArn1},
	}
	result, err := svc.AssociateIamInstanceProfile(v, cmd)
	require.NoError(t, err)
	require.NotNil(t, result)
	assert.True(t, result.Found)
	assert.Equal(t, testProfileArn1, result.InstanceProfileArn)
	assert.True(t, strings.HasPrefix(result.AssociationId, "iip-assoc-"))
	assert.False(t, result.Timestamp.IsZero())

	assert.Equal(t, testProfileArn1, v.IamInstanceProfileArn)
	assert.Equal(t, result.AssociationId, v.IamInstanceProfileAssociationId)
}

func TestAssociateIamInstanceProfile_AlreadyAssociated(t *testing.T) {
	v := vmWithAccount("i-assoc2", testIAMAccount)
	v.IamInstanceProfileArn = testProfileArn1
	v.IamInstanceProfileAssociationId = "iip-assoc-deadbeef00000001"
	svc := &InstanceServiceImpl{vmMgr: mgrWith(map[string]*vm.VM{v.ID: v})}

	cmd := spxtypes.EC2InstanceCommand{
		ID:                        v.ID,
		Attributes:                spxtypes.EC2CommandAttributes{AssociateIamInstanceProfile: true},
		IamProfileAssociationData: &spxtypes.IamProfileAssociationData{InstanceProfileArn: testProfileArn2},
	}
	_, err := svc.AssociateIamInstanceProfile(v, cmd)
	require.Error(t, err)
	assert.Equal(t, awserrors.ErrorIamInstanceProfileAlreadyAssociated, err.Error())

	// The original association is preserved when the conflict check fires.
	assert.Equal(t, testProfileArn1, v.IamInstanceProfileArn)
	assert.Equal(t, "iip-assoc-deadbeef00000001", v.IamInstanceProfileAssociationId)
}

func TestAssociateIamInstanceProfile_MissingData(t *testing.T) {
	v := vmWithAccount("i-assoc3", testIAMAccount)
	svc := &InstanceServiceImpl{vmMgr: mgrWith(map[string]*vm.VM{v.ID: v})}

	cmd := spxtypes.EC2InstanceCommand{ID: v.ID, Attributes: spxtypes.EC2CommandAttributes{AssociateIamInstanceProfile: true}}
	_, err := svc.AssociateIamInstanceProfile(v, cmd)
	require.Error(t, err)
	assert.Equal(t, awserrors.ErrorMissingParameter, err.Error())
}

func TestDisassociateIamProfileAssociation_Success(t *testing.T) {
	const assocID = "iip-assoc-aaaaaaaaaaaaaaa01"
	v := vmWithAccount("i-dis1", testIAMAccount)
	v.IamInstanceProfileArn = testProfileArn1
	v.IamInstanceProfileAssociationId = assocID
	svc := &InstanceServiceImpl{vmMgr: mgrWith(map[string]*vm.VM{v.ID: v})}

	result, err := svc.DisassociateIamProfileAssociation(
		spxtypes.IamProfileDisassociateRequest{AssociationId: assocID}, testIAMAccount)
	require.NoError(t, err)
	require.NotNil(t, result)
	assert.True(t, result.Found)
	assert.Equal(t, v.ID, result.InstanceId)
	assert.Equal(t, testProfileArn1, result.InstanceProfileArn)
	assert.Empty(t, v.IamInstanceProfileArn)
	assert.Empty(t, v.IamInstanceProfileAssociationId)
}

func TestDisassociateIamProfileAssociation_NotFound(t *testing.T) {
	svc := &InstanceServiceImpl{vmMgr: mgrWith(map[string]*vm.VM{})}
	result, err := svc.DisassociateIamProfileAssociation(
		spxtypes.IamProfileDisassociateRequest{AssociationId: "iip-assoc-missing"}, testIAMAccount)
	require.NoError(t, err)
	assert.False(t, result.Found)
}

func TestDisassociateIamProfileAssociation_MissingID(t *testing.T) {
	svc := &InstanceServiceImpl{vmMgr: mgrWith(map[string]*vm.VM{})}
	_, err := svc.DisassociateIamProfileAssociation(
		spxtypes.IamProfileDisassociateRequest{}, testIAMAccount)
	require.Error(t, err)
	assert.Equal(t, awserrors.ErrorMissingParameter, err.Error())
}

func TestDisassociateIamProfileAssociation_CrossAccountIsNoOp(t *testing.T) {
	const assocID = "iip-assoc-bbbbbbbbbbbbbbb02"
	v := vmWithAccount("i-dis-cross", testIAMOther)
	v.IamInstanceProfileArn = testProfileArn1
	v.IamInstanceProfileAssociationId = assocID
	svc := &InstanceServiceImpl{vmMgr: mgrWith(map[string]*vm.VM{v.ID: v})}

	// Caller from a different account must not match — even though the
	// association ID is correct, the VM belongs to testIAMOther.
	result, err := svc.DisassociateIamProfileAssociation(
		spxtypes.IamProfileDisassociateRequest{AssociationId: assocID}, testIAMAccount)
	require.NoError(t, err)
	assert.False(t, result.Found, "cross-account disassociate must NoOp")
	assert.Equal(t, testProfileArn1, v.IamInstanceProfileArn, "other-account VM must be untouched")
}

func TestReplaceIamProfileAssociation_Success(t *testing.T) {
	const oldID = "iip-assoc-ccccccccccccccc03"
	v := vmWithAccount("i-rep1", testIAMAccount)
	v.IamInstanceProfileArn = testProfileArn1
	v.IamInstanceProfileAssociationId = oldID
	svc := &InstanceServiceImpl{vmMgr: mgrWith(map[string]*vm.VM{v.ID: v})}

	result, err := svc.ReplaceIamProfileAssociation(spxtypes.IamProfileReplaceRequest{
		AssociationId:      oldID,
		InstanceProfileArn: testProfileArn2,
	}, testIAMAccount)
	require.NoError(t, err)
	require.NotNil(t, result)
	assert.True(t, result.Found)
	assert.NotEqual(t, oldID, result.AssociationId, "Replace must generate a fresh association ID")
	assert.True(t, strings.HasPrefix(result.AssociationId, "iip-assoc-"))
	assert.Equal(t, testProfileArn2, result.InstanceProfileArn)

	assert.Equal(t, testProfileArn2, v.IamInstanceProfileArn)
	assert.Equal(t, result.AssociationId, v.IamInstanceProfileAssociationId)
}

func TestReplaceIamProfileAssociation_StaleID(t *testing.T) {
	const currentID = "iip-assoc-ddddddddddddddd04"
	v := vmWithAccount("i-rep2", testIAMAccount)
	v.IamInstanceProfileArn = testProfileArn1
	v.IamInstanceProfileAssociationId = currentID
	svc := &InstanceServiceImpl{vmMgr: mgrWith(map[string]*vm.VM{v.ID: v})}

	result, err := svc.ReplaceIamProfileAssociation(spxtypes.IamProfileReplaceRequest{
		AssociationId:      "iip-assoc-stale-doesnotexist",
		InstanceProfileArn: testProfileArn2,
	}, testIAMAccount)
	require.NoError(t, err)
	assert.False(t, result.Found)
	assert.Equal(t, testProfileArn1, v.IamInstanceProfileArn, "binding must be unchanged on stale ID")
	assert.Equal(t, currentID, v.IamInstanceProfileAssociationId)
}

func TestReplaceIamProfileAssociation_MissingParams(t *testing.T) {
	svc := &InstanceServiceImpl{vmMgr: mgrWith(map[string]*vm.VM{})}
	_, err := svc.ReplaceIamProfileAssociation(spxtypes.IamProfileReplaceRequest{InstanceProfileArn: testProfileArn1}, testIAMAccount)
	require.Error(t, err)
	assert.Equal(t, awserrors.ErrorMissingParameter, err.Error())

	_, err = svc.ReplaceIamProfileAssociation(spxtypes.IamProfileReplaceRequest{AssociationId: "iip-assoc-x"}, testIAMAccount)
	require.Error(t, err)
	assert.Equal(t, awserrors.ErrorMissingParameter, err.Error())
}

func TestDescribeIamProfileAssociations_FilterAndOrdering(t *testing.T) {
	withProfile := vmWithAccount("i-with", testIAMAccount)
	withProfile.IamInstanceProfileArn = testProfileArn1
	withProfile.IamInstanceProfileAssociationId = "iip-assoc-with-profile-001"

	noProfile := vmWithAccount("i-without", testIAMAccount)

	otherTenant := vmWithAccount("i-other-tenant", testIAMOther)
	otherTenant.IamInstanceProfileArn = testProfileArn2
	otherTenant.IamInstanceProfileAssociationId = "iip-assoc-other-tenant001"

	svc := &InstanceServiceImpl{vmMgr: mgrWith(map[string]*vm.VM{
		withProfile.ID: withProfile,
		noProfile.ID:   noProfile,
		otherTenant.ID: otherTenant,
	})}

	out := svc.DescribeIamProfileAssociations(spxtypes.IamProfileDescribeRequest{}, testIAMAccount)
	require.Len(t, out.Associations, 1)
	assert.Equal(t, withProfile.ID, out.Associations[0].InstanceId)
	assert.Equal(t, ec2.IamInstanceProfileAssociationStateAssociated, out.Associations[0].State)

	// Filter by instance ID excludes everything.
	out = svc.DescribeIamProfileAssociations(spxtypes.IamProfileDescribeRequest{InstanceIds: []string{"i-not-here"}}, testIAMAccount)
	assert.Empty(t, out.Associations)

	// Filter by association ID picks the one we want.
	out = svc.DescribeIamProfileAssociations(spxtypes.IamProfileDescribeRequest{AssociationIds: []string{"iip-assoc-with-profile-001"}}, testIAMAccount)
	require.Len(t, out.Associations, 1)

	// Filter by state excludes when "associated" isn't requested.
	out = svc.DescribeIamProfileAssociations(spxtypes.IamProfileDescribeRequest{States: []string{"disassociating"}}, testIAMAccount)
	assert.Empty(t, out.Associations)

	// Cross-tenant caller sees the other tenant's row when their own.
	out = svc.DescribeIamProfileAssociations(spxtypes.IamProfileDescribeRequest{}, testIAMOther)
	require.Len(t, out.Associations, 1)
	assert.Equal(t, otherTenant.ID, out.Associations[0].InstanceId)
}

func TestStopOrTerminateInstance_AutoDisassociatesProfileOnTerminate(t *testing.T) {
	v := vmWithAccount("i-term-prof", testIAMAccount)
	v.IamInstanceProfileArn = testProfileArn1
	v.IamInstanceProfileAssociationId = "iip-assoc-clearonterm0001"
	v.Status = vm.StateRunning
	v.RunInstancesInput = &ec2.RunInstancesInput{InstanceType: aws.String("t3.micro")}

	svc := &InstanceServiceImpl{
		vmMgr: mgrWith(map[string]*vm.VM{v.ID: v}),
	}

	cmd := spxtypes.EC2InstanceCommand{
		ID:         v.ID,
		Attributes: spxtypes.EC2CommandAttributes{TerminateInstance: true},
	}
	// Run synchronously up to the lock-protected attribute stamp; the
	// goroutine that follows mutates a fake-less Manager which is fine
	// (Terminate is a no-op in this minimal fixture but may log).
	err := svc.StopOrTerminateInstance(v, cmd)
	// In this minimal fixture the goroutine path's Terminate will fail
	// without a full vm.Manager fixture; the synchronous return either
	// completes or surfaces IncorrectInstanceState depending on
	// transition validation. Either is acceptable — we only assert the
	// auto-disassociate side effect happened under the lock.
	_ = err
	assert.Empty(t, v.IamInstanceProfileArn, "terminate must clear profile ARN")
	assert.Empty(t, v.IamInstanceProfileAssociationId, "terminate must clear association ID")
}
