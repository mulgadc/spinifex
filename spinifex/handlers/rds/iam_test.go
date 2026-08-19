package handlers_rds

import (
	"encoding/json"
	"errors"
	"slices"
	"testing"

	"github.com/aws/aws-sdk-go/aws"
	handlers_iam "github.com/mulgadc/spinifex/spinifex/handlers/iam"
	iammock "github.com/mulgadc/spinifex/spinifex/handlers/iam/mock"
	"github.com/mulgadc/spinifex/spinifex/utils"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func testIAMProvider(f *iammock.SystemInstanceRoleEnsurer) IAMProvider {
	return func() handlers_iam.SystemInstanceRoleEnsurer { return f }
}

// rdsInstanceProfileARN mirrors the ARN ensureInstanceProfile computes, for
// assertions comparing against a launch input rather than the mock directly.
func rdsInstanceProfileARN(accountID string) string {
	return "arn:aws:iam::" + accountID + ":instance-profile/" + InstanceRoleName
}

// A Postgres RCE on a DB VM inherits this role, so the grant must be the four
// internal actions and nothing wildcarded that could reach the customer surface.
func TestInstanceRolePolicy_GrantsOnlyTheInternalActions(t *testing.T) {
	t.Parallel()
	policy, err := instanceRoleInlinePolicy()
	require.NoError(t, err)
	var doc handlers_iam.PolicyDocument
	require.NoError(t, json.Unmarshal([]byte(policy), &doc))
	require.Len(t, doc.Statement, 1)

	stmt := doc.Statement[0]
	assert.Equal(t, handlers_iam.PolicyEffectAllow, stmt.Effect)

	want := make([]string, 0, len(InternalAgentActions))
	for _, action := range InternalAgentActions {
		want = append(want, "rds:"+action)
	}
	granted := slices.Clone([]string(stmt.Action))
	slices.Sort(granted)
	slices.Sort(want)
	assert.Equal(t, want, granted)

	for _, action := range granted {
		assert.NotContains(t, action, "*", "the instance role must name its actions, never wildcard them")
	}
}

// The role is created in the system account: IMDS resolves an instance profile
// under the VM's own account, so a role in the customer account would be
// invisible to the agent and it would get no credentials at all.
func TestEnsureInstanceProfile_CreatesInSystemAccount(t *testing.T) {
	t.Parallel()
	f := iammock.New()
	arn, err := ensureInstanceProfile(func() handlers_iam.SystemInstanceRoleEnsurer { return f }, utils.GlobalAccountID)
	require.NoError(t, err)

	assert.Equal(t, "arn:aws:iam::"+utils.GlobalAccountID+":instance-profile/"+InstanceRoleName, arn)
	assert.Equal(t, utils.GlobalAccountID, f.LastRoleAcct)
	assert.Equal(t, utils.GlobalAccountID, f.LastProfileAcct)
	assert.Equal(t, handlers_iam.EC2InstanceTrustPolicy, f.LastTrustDoc,
		"AssumeRoleForInstance only mints credentials for the EC2 trust document")
}

// Every launch calls this, so a second call must converge on the existing role
// rather than create a second one, and must re-assert the same grant.
func TestEnsureInstanceProfile_IsIdempotent(t *testing.T) {
	t.Parallel()
	f := iammock.New()
	provider := func() handlers_iam.SystemInstanceRoleEnsurer { return f }

	first, err := ensureInstanceProfile(provider, utils.GlobalAccountID)
	require.NoError(t, err)
	second, err := ensureInstanceProfile(provider, utils.GlobalAccountID)
	require.NoError(t, err)

	assert.Equal(t, first, second)
	assert.Equal(t, 1, f.CreateRoleCalls, "the second launch must find the role, not create it")
	assert.Equal(t, 1, f.CreateInstanceProfileCalls, "the second launch must find the instance profile")
	require.Len(t, f.PolicyCalls, 2, "the grant is re-asserted on every launch")
	policy, err := instanceRoleInlinePolicy()
	require.NoError(t, err)
	for _, put := range f.PolicyCalls {
		assert.Equal(t, instanceRoleInlinePolicyName, aws.StringValue(put.PolicyName))
		assert.Equal(t, policy, aws.StringValue(put.PolicyDocument))
	}
}

// Without IAM the agent has no gateway credentials at all, so provisioning
// must fail instead of launching a VM that can never register.
func TestEnsureInstanceProfile_RejectsUnavailableIAM(t *testing.T) {
	tests := map[string]IAMProvider{
		"nil provider": nil,
		"nil service":  func() handlers_iam.SystemInstanceRoleEnsurer { return nil },
	}
	for name, provider := range tests {
		t.Run(name, func(t *testing.T) {
			arn, err := ensureInstanceProfile(provider, utils.GlobalAccountID)
			require.Error(t, err)
			assert.Empty(t, arn)
			assert.Contains(t, err.Error(), "IAM")
		})
	}
}

func TestEnsureInstanceProfile_PreservesEnsureFailure(t *testing.T) {
	t.Parallel()
	ensureErr := errors.New("IAM storage unavailable")
	f := iammock.New()
	f.PutRolePolicyErr = ensureErr

	arn, err := ensureInstanceProfile(
		func() handlers_iam.SystemInstanceRoleEnsurer { return f }, utils.GlobalAccountID)

	require.Error(t, err)
	assert.ErrorIs(t, err, ensureErr)
	assert.Empty(t, arn)
	assert.Contains(t, err.Error(), InstanceRoleName)
}

func TestEnsureInstanceProfile_RejectsEmptyARN(t *testing.T) {
	t.Parallel()
	f := iammock.New()
	f.EmptyInstanceProfileARN = true

	arn, err := ensureInstanceProfile(
		func() handlers_iam.SystemInstanceRoleEnsurer { return f }, utils.GlobalAccountID)

	require.Error(t, err)
	assert.Empty(t, arn)
	assert.Contains(t, err.Error(), "empty profile ARN")
}
