package handlers_elbv2

import (
	"errors"
	"testing"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/iam"
	"github.com/mulgadc/spinifex/spinifex/awserrors"
	handlers_iam "github.com/mulgadc/spinifex/spinifex/handlers/iam"
	"github.com/mulgadc/spinifex/spinifex/utils"
	"github.com/stretchr/testify/assert"
)

// fakeLBEnsurer is a minimal SystemInstanceRoleEnsurer recording the account the
// role and instance profile are created under. GetRole/GetInstanceProfile always
// miss so EnsureSystemInstanceProfile takes the create path.
type fakeLBEnsurer struct {
	roleAcct    string
	profileAcct string
}

func (f *fakeLBEnsurer) GetRole(_ string, _ *iam.GetRoleInput) (*iam.GetRoleOutput, error) {
	return nil, errors.New(awserrors.ErrorIAMNoSuchEntity)
}

func (f *fakeLBEnsurer) CreateRole(acct string, in *iam.CreateRoleInput) (*iam.CreateRoleOutput, error) {
	f.roleAcct = acct
	name := aws.StringValue(in.RoleName)
	return &iam.CreateRoleOutput{Role: &iam.Role{
		RoleName: aws.String(name),
		Arn:      aws.String("arn:aws:iam::" + acct + ":role/" + name),
	}}, nil
}

func (f *fakeLBEnsurer) PutRolePolicy(_ string, _ *iam.PutRolePolicyInput) (*iam.PutRolePolicyOutput, error) {
	return &iam.PutRolePolicyOutput{}, nil
}

func (f *fakeLBEnsurer) GetInstanceProfile(_ string, _ *iam.GetInstanceProfileInput) (*iam.GetInstanceProfileOutput, error) {
	return nil, errors.New(awserrors.ErrorIAMNoSuchEntity)
}

func (f *fakeLBEnsurer) CreateInstanceProfile(acct string, in *iam.CreateInstanceProfileInput) (*iam.CreateInstanceProfileOutput, error) {
	f.profileAcct = acct
	name := aws.StringValue(in.InstanceProfileName)
	return &iam.CreateInstanceProfileOutput{InstanceProfile: &iam.InstanceProfile{
		InstanceProfileName: aws.String(name),
		Arn:                 aws.String("arn:aws:iam::" + acct + ":instance-profile/" + name),
	}}, nil
}

func (f *fakeLBEnsurer) AddRoleToInstanceProfile(_ string, _ *iam.AddRoleToInstanceProfileInput) (*iam.AddRoleToInstanceProfileOutput, error) {
	return &iam.AddRoleToInstanceProfileOutput{}, nil
}

var _ handlers_iam.SystemInstanceRoleEnsurer = (*fakeLBEnsurer)(nil)

// TestEnsureLBInstanceProfile_UsesLazyProvider asserts the LB profile resolves via
// the lazy IAMProvider when the eager IAM field is unset — the daemon wires only
// the provider so the IAM build cannot race the NATS KV backend at startup.
func TestEnsureLBInstanceProfile_UsesLazyProvider(t *testing.T) {
	f := &fakeLBEnsurer{}
	s := &ELBv2ServiceImpl{IAMProvider: func() handlers_iam.SystemInstanceRoleEnsurer { return f }}

	arn := s.ensureLBInstanceProfile(utils.GlobalAccountID)
	assert.Equal(t, "arn:aws:iam::"+utils.GlobalAccountID+":instance-profile/"+lbAgentSystemRoleName, arn)
}

// TestEnsureLBInstanceProfile_CreatesInSystemAccount asserts the role and profile
// are created under the account the LB VM runs in (the system account). IMDS
// resolves the profile under the instance's account, so a role created in the LB
// owner account would be invisible to it and the lb-agent would get no creds.
func TestEnsureLBInstanceProfile_CreatesInSystemAccount(t *testing.T) {
	f := &fakeLBEnsurer{}
	s := &ELBv2ServiceImpl{IAM: f}

	s.ensureLBInstanceProfile(utils.GlobalAccountID)
	assert.Equal(t, utils.GlobalAccountID, f.roleAcct)
	assert.Equal(t, utils.GlobalAccountID, f.profileAcct)
}

// TestEnsureLBInstanceProfile_NotReadyFallsBack asserts an unwired IAM (no field,
// provider returns nil because the KV backend is not up yet) yields "" so the LB
// VM falls back to baked static creds and retries on the next launch.
func TestEnsureLBInstanceProfile_NotReadyFallsBack(t *testing.T) {
	s := &ELBv2ServiceImpl{IAMProvider: func() handlers_iam.SystemInstanceRoleEnsurer { return nil }}
	assert.Equal(t, "", s.ensureLBInstanceProfile(utils.GlobalAccountID))
}
