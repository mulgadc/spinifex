package handlers_eks

import (
	"errors"
	"testing"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/iam"
	"github.com/mulgadc/bluebottle/pkg/auth"
	"github.com/mulgadc/spinifex/spinifex/awserrors"
	iammock "github.com/mulgadc/spinifex/spinifex/handlers/iam/mock"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// newFakeEnsurer returns the shared SystemInstanceRoleEnsurer mock, used
// across this package's tests wherever an EKSServiceDeps.IAM is needed.
func newFakeEnsurer() *iammock.SystemInstanceRoleEnsurer {
	return iammock.New()
}

const testNodeRoleARN = "arn:aws:iam::000000000001:role/eks-quickstart-node-role"

// fixtureNodeRoleARN is the node role the shared service fixture's nodegroups
// launch with; resolution requires it to exist in the mock's IAM.
const fixtureNodeRoleARN = "arn:aws:iam::111122223333:role/eks-node"

// newEnsurerWithRole returns a mock holding roleName with the canonical ARN IAM
// would have stored for it, which node role resolution verifies against.
func newEnsurerWithRole(roleName, roleARN string) *iammock.SystemInstanceRoleEnsurer {
	f := newFakeEnsurer()
	f.Roles[roleName] = &iam.Role{RoleName: aws.String(roleName), Arn: aws.String(roleARN)}
	return f
}

func TestEnsureNodeInstanceProfile_CreatesAndAttaches(t *testing.T) {
	f := newEnsurerWithRole("eks-quickstart-node-role", testNodeRoleARN)
	s := &EKSServiceImpl{deps: EKSServiceDeps{IAM: f}}

	arn, err := s.ensureNodeInstanceProfile("000000000001", testNodeRoleARN)
	require.NoError(t, err)
	assert.Equal(t, "arn:aws:iam::000000000001:instance-profile/eks-quickstart-node-role", arn)
	assert.Equal(t, 1, f.CreateInstanceProfileCalls)
	assert.Equal(t, 1, f.AddRoleToInstanceProfileCalls)
	assert.Len(t, f.Profiles["eks-quickstart-node-role"].Roles, 1)
}

func TestEnsureNodeInstanceProfile_ExistingWithRoleIsNoop(t *testing.T) {
	f := newEnsurerWithRole("eks-quickstart-node-role", testNodeRoleARN)
	f.Profiles["eks-quickstart-node-role"] = &iam.InstanceProfile{
		InstanceProfileName: aws.String("eks-quickstart-node-role"),
		Arn:                 aws.String("arn:aws:iam::000000000001:instance-profile/eks-quickstart-node-role"),
		Roles:               []*iam.Role{{RoleName: aws.String("eks-quickstart-node-role")}},
	}
	s := &EKSServiceImpl{deps: EKSServiceDeps{IAM: f}}

	arn, err := s.ensureNodeInstanceProfile("000000000001", testNodeRoleARN)
	require.NoError(t, err)
	assert.Equal(t, "arn:aws:iam::000000000001:instance-profile/eks-quickstart-node-role", arn)
	assert.Zero(t, f.CreateInstanceProfileCalls)
	assert.Zero(t, f.AddRoleToInstanceProfileCalls)
}

func TestEnsureNodeInstanceProfile_ExistingWithoutRoleAttaches(t *testing.T) {
	f := newEnsurerWithRole("eks-quickstart-node-role", testNodeRoleARN)
	f.Profiles["eks-quickstart-node-role"] = &iam.InstanceProfile{
		InstanceProfileName: aws.String("eks-quickstart-node-role"),
		Arn:                 aws.String("arn:aws:iam::000000000001:instance-profile/eks-quickstart-node-role"),
	}
	s := &EKSServiceImpl{deps: EKSServiceDeps{IAM: f}}

	_, err := s.ensureNodeInstanceProfile("000000000001", testNodeRoleARN)
	require.NoError(t, err)
	assert.Zero(t, f.CreateInstanceProfileCalls)
	assert.Equal(t, 1, f.AddRoleToInstanceProfileCalls)
}

func TestEnsureNodeInstanceProfile_AddRoleLimitExceededIsSuccess(t *testing.T) {
	f := newEnsurerWithRole("eks-quickstart-node-role", testNodeRoleARN)
	f.AddRoleToInstanceProfileErr = errors.New(awserrors.ErrorIAMLimitExceeded)
	s := &EKSServiceImpl{deps: EKSServiceDeps{IAM: f}}

	arn, err := s.ensureNodeInstanceProfile("000000000001", testNodeRoleARN)
	require.NoError(t, err)
	assert.NotEmpty(t, arn)
}

// A path-bearing role ARN names its profile after the final segment only; the
// path is not a legal instance-profile name character.
func TestEnsureNodeInstanceProfile_PathBearingRoleUsesFinalSegment(t *testing.T) {
	f := newEnsurerWithRole("Worker", "arn:aws:iam::000000000001:role/team/Worker")
	s := &EKSServiceImpl{deps: EKSServiceDeps{IAM: f}}

	arn, err := s.ensureNodeInstanceProfile("000000000001", "arn:aws:iam::000000000001:role/team/Worker")
	require.NoError(t, err)
	assert.Equal(t, "arn:aws:iam::000000000001:instance-profile/Worker", arn)
	assert.Len(t, f.Profiles["Worker"].Roles, 1)
	assert.Equal(t, "Worker", aws.StringValue(f.Profiles["Worker"].Roles[0].RoleName))
}

func TestEnsureNodeInstanceProfile_RejectsInvalidARNs(t *testing.T) {
	cases := map[string]string{
		"empty":             "",
		"not an ARN":        "not-an-arn",
		"bare role suffix":  "garbage:role/Worker",
		"non-IAM service":   "arn:aws:sts::000000000001:role/Worker",
		"non-role resource": "arn:aws:iam::000000000001:user/bob",
		"empty role name":   "arn:aws:iam::000000000001:role/",
		"empty account":     "arn:aws:iam:::role/Worker",
	}
	for name, arn := range cases {
		t.Run(name, func(t *testing.T) {
			f := newEnsurerWithRole("Worker", "arn:aws:iam::000000000001:role/Worker")
			s := &EKSServiceImpl{deps: EKSServiceDeps{IAM: f}}
			_, err := s.ensureNodeInstanceProfile("000000000001", arn)
			require.Error(t, err)
			assert.Zero(t, f.CreateInstanceProfileCalls)
			assert.Zero(t, f.AddRoleToInstanceProfileCalls)
		})
	}
}

// A role another account owns is rejected by the account guard alone: the mock
// holds it under the ARN the caller supplied, so the canonical comparison would
// pass and only the guard can refuse it.
func TestEnsureNodeInstanceProfile_RejectsCrossAccountRole(t *testing.T) {
	const crossAccountARN = "arn:aws:iam::000000000002:role/Worker"
	f := newEnsurerWithRole("Worker", crossAccountARN)
	s := &EKSServiceImpl{deps: EKSServiceDeps{IAM: f}}

	_, err := s.ensureNodeInstanceProfile("000000000001", crossAccountARN)
	require.Error(t, err)
	assert.NotErrorIs(t, err, auth.ErrRoleARNMismatch, "the guard must reject, not the ARN comparison")
	assert.ErrorContains(t, err, "is not in account 000000000001")
	assert.Zero(t, f.CreateInstanceProfileCalls)
	assert.Zero(t, f.AddRoleToInstanceProfileCalls)
}

// A node role ARN carrying a path the stored role does not have must not bind a
// profile to the role its final segment names.
func TestEnsureNodeInstanceProfile_RejectsFabricatedPath(t *testing.T) {
	f := newEnsurerWithRole("eks-quickstart-node-role", testNodeRoleARN)
	s := &EKSServiceImpl{deps: EKSServiceDeps{IAM: f}}

	_, err := s.ensureNodeInstanceProfile("000000000001",
		"arn:aws:iam::000000000001:role/decoy/eks-quickstart-node-role")
	require.ErrorIs(t, err, auth.ErrRoleARNMismatch)
	assert.Zero(t, f.CreateInstanceProfileCalls)
	assert.Zero(t, f.AddRoleToInstanceProfileCalls)
}

// A pathed role is likewise not reachable by its pathless ARN.
func TestEnsureNodeInstanceProfile_RejectsStrippedPath(t *testing.T) {
	f := newEnsurerWithRole("Worker", "arn:aws:iam::000000000001:role/team/Worker")
	s := &EKSServiceImpl{deps: EKSServiceDeps{IAM: f}}

	_, err := s.ensureNodeInstanceProfile("000000000001", "arn:aws:iam::000000000001:role/Worker")
	require.ErrorIs(t, err, auth.ErrRoleARNMismatch)
	assert.Zero(t, f.CreateInstanceProfileCalls)
}

// A node role that does not exist in IAM cannot be resolved, so no profile is
// created for it.
func TestEnsureNodeInstanceProfile_RejectsUnknownRole(t *testing.T) {
	f := newFakeEnsurer()
	s := &EKSServiceImpl{deps: EKSServiceDeps{IAM: f}}

	_, err := s.ensureNodeInstanceProfile("000000000001", testNodeRoleARN)
	require.Error(t, err)
	assert.Zero(t, f.CreateInstanceProfileCalls)
}
