package handlers_iam

import (
	"errors"
	"testing"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/iam"
	"github.com/mulgadc/spinifex/spinifex/awserrors"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// fakeSystemRoleEnsurer is an in-memory SystemInstanceRoleEnsurer that records
// calls and serves role/profile stores keyed by name.
type fakeSystemRoleEnsurer struct {
	roles           map[string]*iam.Role
	profiles        map[string]*iam.InstanceProfile
	policies        map[string]string
	createRoleCalls int
	createProfCalls int
	addRoleCalls    int
	lastTrustDoc    string
}

func newFakeSystemRoleEnsurer() *fakeSystemRoleEnsurer {
	return &fakeSystemRoleEnsurer{
		roles:    map[string]*iam.Role{},
		profiles: map[string]*iam.InstanceProfile{},
		policies: map[string]string{},
	}
}

func (f *fakeSystemRoleEnsurer) GetRole(_ string, in *iam.GetRoleInput) (*iam.GetRoleOutput, error) {
	r, ok := f.roles[aws.StringValue(in.RoleName)]
	if !ok {
		return nil, errors.New(awserrors.ErrorIAMNoSuchEntity)
	}
	return &iam.GetRoleOutput{Role: r}, nil
}

func (f *fakeSystemRoleEnsurer) CreateRole(acct string, in *iam.CreateRoleInput) (*iam.CreateRoleOutput, error) {
	f.createRoleCalls++
	f.lastTrustDoc = aws.StringValue(in.AssumeRolePolicyDocument)
	name := aws.StringValue(in.RoleName)
	if _, ok := f.roles[name]; ok {
		return nil, errors.New(awserrors.ErrorIAMEntityAlreadyExists)
	}
	r := &iam.Role{RoleName: aws.String(name), Arn: aws.String("arn:aws:iam::" + acct + ":role/" + name)}
	f.roles[name] = r
	return &iam.CreateRoleOutput{Role: r}, nil
}

func (f *fakeSystemRoleEnsurer) PutRolePolicy(_ string, in *iam.PutRolePolicyInput) (*iam.PutRolePolicyOutput, error) {
	f.policies[aws.StringValue(in.RoleName)] = aws.StringValue(in.PolicyDocument)
	return &iam.PutRolePolicyOutput{}, nil
}

func (f *fakeSystemRoleEnsurer) GetInstanceProfile(_ string, in *iam.GetInstanceProfileInput) (*iam.GetInstanceProfileOutput, error) {
	p, ok := f.profiles[aws.StringValue(in.InstanceProfileName)]
	if !ok {
		return nil, errors.New(awserrors.ErrorIAMNoSuchEntity)
	}
	return &iam.GetInstanceProfileOutput{InstanceProfile: p}, nil
}

func (f *fakeSystemRoleEnsurer) CreateInstanceProfile(acct string, in *iam.CreateInstanceProfileInput) (*iam.CreateInstanceProfileOutput, error) {
	f.createProfCalls++
	name := aws.StringValue(in.InstanceProfileName)
	if _, ok := f.profiles[name]; ok {
		return nil, errors.New(awserrors.ErrorIAMEntityAlreadyExists)
	}
	p := &iam.InstanceProfile{
		InstanceProfileName: aws.String(name),
		Arn:                 aws.String("arn:aws:iam::" + acct + ":instance-profile/" + name),
	}
	f.profiles[name] = p
	return &iam.CreateInstanceProfileOutput{InstanceProfile: p}, nil
}

func (f *fakeSystemRoleEnsurer) AddRoleToInstanceProfile(_ string, in *iam.AddRoleToInstanceProfileInput) (*iam.AddRoleToInstanceProfileOutput, error) {
	f.addRoleCalls++
	p, ok := f.profiles[aws.StringValue(in.InstanceProfileName)]
	if !ok {
		return nil, errors.New(awserrors.ErrorIAMNoSuchEntity)
	}
	p.Roles = []*iam.Role{{RoleName: in.RoleName}}
	return &iam.AddRoleToInstanceProfileOutput{}, nil
}

const (
	testRoleAcct   = "000000000007"
	testRoleName   = "spinifex-system-role"
	testPolicyName = "spinifex-system-internal"
	testPolicyDoc  = `{"Version":"2012-10-17","Statement":[{"Effect":"Allow","Action":["svc:DoThing"],"Resource":"*"}]}`
)

// TestEnsureSystemInstanceProfile_CreatesAll asserts the role (EC2 trust), inline
// policy, and instance profile are all created and the profile ARN is returned.
func TestEnsureSystemInstanceProfile_CreatesAll(t *testing.T) {
	f := newFakeSystemRoleEnsurer()
	arn, err := EnsureSystemInstanceProfile(f, testRoleAcct, testRoleName, testPolicyName, testPolicyDoc)
	require.NoError(t, err)
	assert.Equal(t, "arn:aws:iam::"+testRoleAcct+":instance-profile/"+testRoleName, arn)

	assert.Equal(t, 1, f.createRoleCalls)
	assert.Equal(t, 1, f.createProfCalls)
	assert.Equal(t, 1, f.addRoleCalls)
	assert.Contains(t, f.lastTrustDoc, "ec2.amazonaws.com")
	assert.Equal(t, testPolicyDoc, f.policies[testRoleName])
	require.NotNil(t, f.profiles[testRoleName])
	assert.Len(t, f.profiles[testRoleName].Roles, 1)
}

// TestEnsureSystemInstanceProfile_Idempotent asserts a re-run against existing
// role+profile creates nothing new but still re-asserts the inline policy.
func TestEnsureSystemInstanceProfile_Idempotent(t *testing.T) {
	f := newFakeSystemRoleEnsurer()
	_, err := EnsureSystemInstanceProfile(f, testRoleAcct, testRoleName, testPolicyName, testPolicyDoc)
	require.NoError(t, err)

	_, err = EnsureSystemInstanceProfile(f, testRoleAcct, testRoleName, testPolicyName, testPolicyDoc)
	require.NoError(t, err)
	assert.Equal(t, 1, f.createRoleCalls, "role created once")
	assert.Equal(t, 1, f.createProfCalls, "profile created once")
	assert.Equal(t, 1, f.addRoleCalls, "role attached once")
}

var _ SystemInstanceRoleEnsurer = (*fakeSystemRoleEnsurer)(nil)
