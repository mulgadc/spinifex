package handlers_rds

import (
	"encoding/json"
	"errors"
	"slices"
	"testing"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/iam"
	"github.com/mulgadc/spinifex/spinifex/awserrors"
	handlers_iam "github.com/mulgadc/spinifex/spinifex/handlers/iam"
	"github.com/mulgadc/spinifex/spinifex/utils"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// fakeRDSEnsurer records every call so both the grant and its idempotency can be
// asserted. The role and profile exist from the second call onward, which is the
// state a re-launch finds.
type fakeRDSEnsurer struct {
	roleAcct     string
	profileAcct  string
	trust        string
	policies     []iam.PutRolePolicyInput
	roleCreates  int
	roleExists   bool
	profileNames []string
}

func (f *fakeRDSEnsurer) GetRole(_ string, _ *iam.GetRoleInput) (*iam.GetRoleOutput, error) {
	if !f.roleExists {
		return nil, errors.New(awserrors.ErrorIAMNoSuchEntity)
	}
	return &iam.GetRoleOutput{Role: &iam.Role{RoleName: aws.String(InstanceRoleName)}}, nil
}

func (f *fakeRDSEnsurer) CreateRole(acct string, in *iam.CreateRoleInput) (*iam.CreateRoleOutput, error) {
	f.roleAcct = acct
	f.roleCreates++
	f.roleExists = true
	f.trust = aws.StringValue(in.AssumeRolePolicyDocument)
	name := aws.StringValue(in.RoleName)
	return &iam.CreateRoleOutput{Role: &iam.Role{
		RoleName: aws.String(name),
		Arn:      aws.String("arn:aws:iam::" + acct + ":role/" + name),
	}}, nil
}

func (f *fakeRDSEnsurer) PutRolePolicy(_ string, in *iam.PutRolePolicyInput) (*iam.PutRolePolicyOutput, error) {
	f.policies = append(f.policies, *in)
	return &iam.PutRolePolicyOutput{}, nil
}

func (f *fakeRDSEnsurer) GetInstanceProfile(_ string, _ *iam.GetInstanceProfileInput) (*iam.GetInstanceProfileOutput, error) {
	if len(f.profileNames) == 0 {
		return nil, errors.New(awserrors.ErrorIAMNoSuchEntity)
	}
	return &iam.GetInstanceProfileOutput{InstanceProfile: &iam.InstanceProfile{
		InstanceProfileName: aws.String(InstanceRoleName),
		Arn:                 aws.String("arn:aws:iam::" + f.profileAcct + ":instance-profile/" + InstanceRoleName),
		Roles:               []*iam.Role{{RoleName: aws.String(InstanceRoleName)}},
	}}, nil
}

func (f *fakeRDSEnsurer) CreateInstanceProfile(acct string, in *iam.CreateInstanceProfileInput) (*iam.CreateInstanceProfileOutput, error) {
	f.profileAcct = acct
	name := aws.StringValue(in.InstanceProfileName)
	f.profileNames = append(f.profileNames, name)
	return &iam.CreateInstanceProfileOutput{InstanceProfile: &iam.InstanceProfile{
		InstanceProfileName: aws.String(name),
		Arn:                 aws.String("arn:aws:iam::" + acct + ":instance-profile/" + name),
	}}, nil
}

func (f *fakeRDSEnsurer) AddRoleToInstanceProfile(_ string, _ *iam.AddRoleToInstanceProfileInput) (*iam.AddRoleToInstanceProfileOutput, error) {
	return &iam.AddRoleToInstanceProfileOutput{}, nil
}

var _ handlers_iam.SystemInstanceRoleEnsurer = (*fakeRDSEnsurer)(nil)

// A Postgres RCE on a DB VM inherits this role, so the grant must be the four
// internal actions and nothing wildcarded that could reach the customer surface.
func TestInstanceRolePolicy_GrantsOnlyTheInternalActions(t *testing.T) {
	var doc handlers_iam.PolicyDocument
	require.NoError(t, json.Unmarshal([]byte(instanceRoleInlinePolicy), &doc))
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
	f := &fakeRDSEnsurer{}
	arn := ensureInstanceProfile(func() handlers_iam.SystemInstanceRoleEnsurer { return f }, utils.GlobalAccountID)

	assert.Equal(t, "arn:aws:iam::"+utils.GlobalAccountID+":instance-profile/"+InstanceRoleName, arn)
	assert.Equal(t, utils.GlobalAccountID, f.roleAcct)
	assert.Equal(t, utils.GlobalAccountID, f.profileAcct)
	assert.Equal(t, handlers_iam.EC2InstanceTrustPolicy, f.trust,
		"AssumeRoleForInstance only mints credentials for the EC2 trust document")
}

// Every launch calls this, so a second call must converge on the existing role
// rather than create a second one, and must re-assert the same grant.
func TestEnsureInstanceProfile_IsIdempotent(t *testing.T) {
	f := &fakeRDSEnsurer{}
	provider := func() handlers_iam.SystemInstanceRoleEnsurer { return f }

	first := ensureInstanceProfile(provider, utils.GlobalAccountID)
	second := ensureInstanceProfile(provider, utils.GlobalAccountID)

	assert.Equal(t, first, second)
	assert.Equal(t, 1, f.roleCreates, "the second launch must find the role, not create it")
	assert.Len(t, f.profileNames, 1, "the second launch must find the instance profile")
	require.Len(t, f.policies, 2, "the grant is re-asserted on every launch")
	for _, put := range f.policies {
		assert.Equal(t, instanceRoleInlinePolicyName, aws.StringValue(put.PolicyName))
		assert.Equal(t, instanceRoleInlinePolicy, aws.StringValue(put.PolicyDocument))
	}
}

// Without IAM the agent has no gateway credentials at all, so the caller has to
// see an empty ARN rather than a launch that looks provisioned.
func TestEnsureInstanceProfile_UnwiredIAMYieldsNoProfile(t *testing.T) {
	assert.Empty(t, ensureInstanceProfile(nil, utils.GlobalAccountID))
	assert.Empty(t, ensureInstanceProfile(
		func() handlers_iam.SystemInstanceRoleEnsurer { return nil }, utils.GlobalAccountID))
}
