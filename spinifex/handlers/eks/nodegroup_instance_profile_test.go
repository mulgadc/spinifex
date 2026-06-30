package handlers_eks

import (
	"errors"
	"testing"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/iam"
	"github.com/mulgadc/spinifex/spinifex/awserrors"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// fakeInstanceProfileEnsurer records calls and serves a small in-memory profile
// store keyed by name.
type fakeInstanceProfileEnsurer struct {
	profiles    map[string]*iam.InstanceProfile
	createErr   error
	addRoleErr  error
	createCalls int
	addCalls    int

	// Role store + capture for the system-role path (ensureSystemRole).
	roles           map[string]*iam.Role
	createRoleCalls int
	lastTrustDoc    string
	rolePolicies    map[string]string // roleName -> last PutRolePolicy document
}

func newFakeEnsurer() *fakeInstanceProfileEnsurer {
	return &fakeInstanceProfileEnsurer{
		profiles:     map[string]*iam.InstanceProfile{},
		roles:        map[string]*iam.Role{},
		rolePolicies: map[string]string{},
	}
}

func (f *fakeInstanceProfileEnsurer) GetRole(accountID string, in *iam.GetRoleInput) (*iam.GetRoleOutput, error) {
	r, ok := f.roles[aws.StringValue(in.RoleName)]
	if !ok {
		return nil, errors.New(awserrors.ErrorIAMNoSuchEntity)
	}
	return &iam.GetRoleOutput{Role: r}, nil
}

func (f *fakeInstanceProfileEnsurer) CreateRole(accountID string, in *iam.CreateRoleInput) (*iam.CreateRoleOutput, error) {
	f.createRoleCalls++
	f.lastTrustDoc = aws.StringValue(in.AssumeRolePolicyDocument)
	name := aws.StringValue(in.RoleName)
	if _, ok := f.roles[name]; ok {
		return nil, errors.New(awserrors.ErrorIAMEntityAlreadyExists)
	}
	r := &iam.Role{
		RoleName: aws.String(name),
		Arn:      aws.String("arn:aws:iam::" + accountID + ":role/" + name),
	}
	f.roles[name] = r
	return &iam.CreateRoleOutput{Role: r}, nil
}

func (f *fakeInstanceProfileEnsurer) PutRolePolicy(_ string, in *iam.PutRolePolicyInput) (*iam.PutRolePolicyOutput, error) {
	f.rolePolicies[aws.StringValue(in.RoleName)] = aws.StringValue(in.PolicyDocument)
	return &iam.PutRolePolicyOutput{}, nil
}

func (f *fakeInstanceProfileEnsurer) GetInstanceProfile(_ string, in *iam.GetInstanceProfileInput) (*iam.GetInstanceProfileOutput, error) {
	p, ok := f.profiles[aws.StringValue(in.InstanceProfileName)]
	if !ok {
		return nil, errors.New(awserrors.ErrorIAMNoSuchEntity)
	}
	return &iam.GetInstanceProfileOutput{InstanceProfile: p}, nil
}

func (f *fakeInstanceProfileEnsurer) CreateInstanceProfile(accountID string, in *iam.CreateInstanceProfileInput) (*iam.CreateInstanceProfileOutput, error) {
	f.createCalls++
	if f.createErr != nil {
		return nil, f.createErr
	}
	name := aws.StringValue(in.InstanceProfileName)
	if _, ok := f.profiles[name]; ok {
		return nil, errors.New(awserrors.ErrorIAMEntityAlreadyExists)
	}
	p := &iam.InstanceProfile{
		InstanceProfileName: aws.String(name),
		Arn:                 aws.String("arn:aws:iam::" + accountID + ":instance-profile/" + name),
	}
	f.profiles[name] = p
	return &iam.CreateInstanceProfileOutput{InstanceProfile: p}, nil
}

func (f *fakeInstanceProfileEnsurer) AddRoleToInstanceProfile(_ string, in *iam.AddRoleToInstanceProfileInput) (*iam.AddRoleToInstanceProfileOutput, error) {
	f.addCalls++
	if f.addRoleErr != nil {
		return nil, f.addRoleErr
	}
	name := aws.StringValue(in.InstanceProfileName)
	p, ok := f.profiles[name]
	if !ok {
		return nil, errors.New(awserrors.ErrorIAMNoSuchEntity)
	}
	if len(p.Roles) > 0 {
		return nil, errors.New(awserrors.ErrorIAMLimitExceeded)
	}
	p.Roles = []*iam.Role{{RoleName: in.RoleName}}
	return &iam.AddRoleToInstanceProfileOutput{}, nil
}

const testNodeRoleARN = "arn:aws:iam::000000000001:role/eks-quickstart-node-role"

func TestEnsureNodeInstanceProfile_CreatesAndAttaches(t *testing.T) {
	f := newFakeEnsurer()
	s := &EKSServiceImpl{deps: EKSServiceDeps{IAM: f}}

	arn, err := s.ensureNodeInstanceProfile("000000000001", testNodeRoleARN)
	require.NoError(t, err)
	assert.Equal(t, "arn:aws:iam::000000000001:instance-profile/eks-quickstart-node-role", arn)
	assert.Equal(t, 1, f.createCalls)
	assert.Equal(t, 1, f.addCalls)
	assert.Len(t, f.profiles["eks-quickstart-node-role"].Roles, 1)
}

func TestEnsureNodeInstanceProfile_ExistingWithRoleIsNoop(t *testing.T) {
	f := newFakeEnsurer()
	f.profiles["eks-quickstart-node-role"] = &iam.InstanceProfile{
		InstanceProfileName: aws.String("eks-quickstart-node-role"),
		Arn:                 aws.String("arn:aws:iam::000000000001:instance-profile/eks-quickstart-node-role"),
		Roles:               []*iam.Role{{RoleName: aws.String("eks-quickstart-node-role")}},
	}
	s := &EKSServiceImpl{deps: EKSServiceDeps{IAM: f}}

	arn, err := s.ensureNodeInstanceProfile("000000000001", testNodeRoleARN)
	require.NoError(t, err)
	assert.Equal(t, "arn:aws:iam::000000000001:instance-profile/eks-quickstart-node-role", arn)
	assert.Zero(t, f.createCalls)
	assert.Zero(t, f.addCalls)
}

func TestEnsureNodeInstanceProfile_ExistingWithoutRoleAttaches(t *testing.T) {
	f := newFakeEnsurer()
	f.profiles["eks-quickstart-node-role"] = &iam.InstanceProfile{
		InstanceProfileName: aws.String("eks-quickstart-node-role"),
		Arn:                 aws.String("arn:aws:iam::000000000001:instance-profile/eks-quickstart-node-role"),
	}
	s := &EKSServiceImpl{deps: EKSServiceDeps{IAM: f}}

	_, err := s.ensureNodeInstanceProfile("000000000001", testNodeRoleARN)
	require.NoError(t, err)
	assert.Zero(t, f.createCalls)
	assert.Equal(t, 1, f.addCalls)
}

func TestEnsureNodeInstanceProfile_AddRoleLimitExceededIsSuccess(t *testing.T) {
	f := newFakeEnsurer()
	f.addRoleErr = errors.New(awserrors.ErrorIAMLimitExceeded)
	s := &EKSServiceImpl{deps: EKSServiceDeps{IAM: f}}

	arn, err := s.ensureNodeInstanceProfile("000000000001", testNodeRoleARN)
	require.NoError(t, err)
	assert.NotEmpty(t, arn)
}

func TestEnsureNodeInstanceProfile_BadARN(t *testing.T) {
	s := &EKSServiceImpl{deps: EKSServiceDeps{IAM: newFakeEnsurer()}}
	_, err := s.ensureNodeInstanceProfile("000000000001", "not-an-arn")
	require.Error(t, err)
}

func TestRoleNameFromARN(t *testing.T) {
	assert.Equal(t, "eks-quickstart-node-role", roleNameFromARN(testNodeRoleARN))
	assert.Equal(t, "", roleNameFromARN("arn:aws:iam::000000000001:user/bob"))
	assert.Equal(t, "", roleNameFromARN(""))
}
