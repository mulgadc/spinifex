package handlers_eks

import (
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/iam"
	handlers_iam "github.com/mulgadc/spinifex/spinifex/handlers/iam"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const testSysAcct = "000000000000"

// TestEnsureK3sServerInstanceProfile_CreatesRolePolicyProfile asserts the CP
// role is created with the EC2 trust policy, carries the internal gateway
// permissions, and its instance profile is returned for launch attachment.
func TestEnsureK3sServerInstanceProfile_CreatesRolePolicyProfile(t *testing.T) {
	f := newFakeEnsurer()
	s := &EKSServiceImpl{deps: EKSServiceDeps{IAM: f}}

	arn := s.ensureCPInstanceProfile(testSysAcct)
	assert.Equal(t, "arn:aws:iam::"+testSysAcct+":instance-profile/"+eksServerSystemRoleName, arn)

	assert.Equal(t, 1, f.createRoleCalls)
	assert.Contains(t, f.lastTrustDoc, "ec2.amazonaws.com")
	assert.Contains(t, f.lastTrustDoc, "sts:AssumeRole")

	policy := f.rolePolicies[eksServerSystemRoleName]
	assert.Contains(t, policy, "eks:PublishInternal")
	assert.Contains(t, policy, "eks:ListInternalAddons")

	require.NotNil(t, f.profiles[eksServerSystemRoleName])
	assert.Len(t, f.profiles[eksServerSystemRoleName].Roles, 1)
}

// TestEnsureK3sServerInstanceProfile_ExistingRoleConverges asserts a re-launch
// against a pre-existing role does not re-create it but still re-asserts the
// inline policy so a stale role converges onto the current permissions.
func TestEnsureK3sServerInstanceProfile_ExistingRoleConverges(t *testing.T) {
	f := newFakeEnsurer()
	f.roles[eksServerSystemRoleName] = &iam.Role{
		RoleName: aws.String(eksServerSystemRoleName),
		Arn:      aws.String("arn:aws:iam::" + testSysAcct + ":role/" + eksServerSystemRoleName),
	}
	s := &EKSServiceImpl{deps: EKSServiceDeps{IAM: f}}

	arn := s.ensureCPInstanceProfile(testSysAcct)
	assert.NotEmpty(t, arn)
	assert.Zero(t, f.createRoleCalls)
	assert.Contains(t, f.rolePolicies[eksServerSystemRoleName], "eks:PublishInternal")
}

// TestEnsureCPInstanceProfile_NilIAMFallsBack asserts an unwired IAM service
// yields "" so the caller falls back to baked static creds rather than failing.
func TestEnsureCPInstanceProfile_NilIAMFallsBack(t *testing.T) {
	s := &EKSServiceImpl{deps: EKSServiceDeps{}}
	assert.Equal(t, "", s.ensureCPInstanceProfile(testSysAcct))
}

// TestEKSServerTrustPolicyIsAssumable guards that the trust doc the CP role
// ships with is exactly the EC2 service-principal shape AssumeRoleForInstance
// matches; a drift here silently breaks IMDS credential minting.
func TestEKSServerTrustPolicyIsAssumable(t *testing.T) {
	assert.True(t, strings.Contains(handlers_iam.EC2InstanceTrustPolicy, `"Service":"ec2.amazonaws.com"`))
	assert.True(t, strings.Contains(handlers_iam.EC2InstanceTrustPolicy, `"Action":"sts:AssumeRole"`))
}
