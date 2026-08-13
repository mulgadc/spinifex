// package handlers_iam_test, not handlers_iam: an in-package test importing
// the shared mock package would create an import cycle, since the mock
// package itself imports handlers_iam for the interface compile check.
package handlers_iam_test

import (
	"testing"

	handlers_iam "github.com/mulgadc/spinifex/spinifex/handlers/iam"
	iammock "github.com/mulgadc/spinifex/spinifex/handlers/iam/mock"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	testRoleAcct   = "000000000007"
	testRoleName   = "spinifex-system-role"
	testPolicyName = "spinifex-system-internal"
	testPolicyDoc  = `{"Version":"2012-10-17","Statement":[{"Effect":"Allow","Action":["svc:DoThing"],"Resource":"*"}]}`
)

// TestEnsureSystemInstanceProfile_CreatesAll asserts the role (EC2 trust), inline
// policy, and instance profile are all created and the profile ARN is returned.
func TestEnsureSystemInstanceProfile_CreatesAll(t *testing.T) {
	f := iammock.New()
	arn, err := handlers_iam.EnsureSystemInstanceProfile(f, testRoleAcct, testRoleName, testPolicyName, testPolicyDoc)
	require.NoError(t, err)
	assert.Equal(t, "arn:aws:iam::"+testRoleAcct+":instance-profile/"+testRoleName, arn)

	assert.Equal(t, 1, f.CreateRoleCalls)
	assert.Equal(t, 1, f.CreateInstanceProfileCalls)
	assert.Equal(t, 1, f.AddRoleToInstanceProfileCalls)
	assert.Contains(t, f.LastTrustDoc, "ec2.amazonaws.com")
	assert.Equal(t, testPolicyDoc, f.RolePolicies[testRoleName])
	require.NotNil(t, f.Profiles[testRoleName])
	assert.Len(t, f.Profiles[testRoleName].Roles, 1)
}

// TestEnsureSystemInstanceProfile_Idempotent asserts a re-run against existing
// role+profile creates nothing new but still re-asserts the inline policy.
func TestEnsureSystemInstanceProfile_Idempotent(t *testing.T) {
	f := iammock.New()
	_, err := handlers_iam.EnsureSystemInstanceProfile(f, testRoleAcct, testRoleName, testPolicyName, testPolicyDoc)
	require.NoError(t, err)

	_, err = handlers_iam.EnsureSystemInstanceProfile(f, testRoleAcct, testRoleName, testPolicyName, testPolicyDoc)
	require.NoError(t, err)
	assert.Equal(t, 1, f.CreateRoleCalls, "role created once")
	assert.Equal(t, 1, f.CreateInstanceProfileCalls, "profile created once")
	assert.Equal(t, 1, f.AddRoleToInstanceProfileCalls, "role attached once")
}
