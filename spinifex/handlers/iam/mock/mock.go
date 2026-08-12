// Package mock provides an in-memory implementation of
// handlers_iam.SystemInstanceRoleEnsurer for tests.
package mock

import (
	"errors"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/iam"
	"github.com/mulgadc/spinifex/spinifex/awserrors"
	handlers_iam "github.com/mulgadc/spinifex/spinifex/handlers/iam"
)

// SystemInstanceRoleEnsurer is an in-memory
// handlers_iam.SystemInstanceRoleEnsurer. Roles and instance profiles are
// stored keyed by name; Get* miss with NoSuchEntity until the matching
// Create* call, and a repeat Create* reports EntityAlreadyExists, mirroring
// the live IAM API.
type SystemInstanceRoleEnsurer struct {
	Roles    map[string]*iam.Role
	Profiles map[string]*iam.InstanceProfile

	// RolePolicies holds the most recent PutRolePolicy document per role
	// name; PolicyCalls logs every call in order for tests asserting a
	// policy is re-asserted on every launch.
	RolePolicies map[string]string
	PolicyCalls  []iam.PutRolePolicyInput

	// Call counters.
	CreateRoleCalls               int
	CreateInstanceProfileCalls    int
	AddRoleToInstanceProfileCalls int

	// LastTrustDoc captures the AssumeRolePolicyDocument of the most recent
	// CreateRole call.
	LastTrustDoc string
	// LastRoleAcct / LastProfileAcct capture the accountID of the most
	// recent CreateRole / CreateInstanceProfile call.
	LastRoleAcct    string
	LastProfileAcct string

	// PutRolePolicyErr, when set, is returned by PutRolePolicy after the
	// call is recorded, letting tests inject a mid-flow failure.
	PutRolePolicyErr error
	// CreateInstanceProfileErr, when set, is returned by
	// CreateInstanceProfile instead of creating a row.
	CreateInstanceProfileErr error
	// AddRoleToInstanceProfileErr, when set, is returned by
	// AddRoleToInstanceProfile instead of attaching.
	AddRoleToInstanceProfileErr error
	// EmptyInstanceProfileARN makes CreateInstanceProfile return an empty
	// ARN, for exercising callers that must reject it.
	EmptyInstanceProfileARN bool
}

var _ handlers_iam.SystemInstanceRoleEnsurer = (*SystemInstanceRoleEnsurer)(nil)

// New returns an empty SystemInstanceRoleEnsurer ready for use.
func New() *SystemInstanceRoleEnsurer {
	return &SystemInstanceRoleEnsurer{
		Roles:        make(map[string]*iam.Role),
		Profiles:     make(map[string]*iam.InstanceProfile),
		RolePolicies: make(map[string]string),
	}
}

func (e *SystemInstanceRoleEnsurer) GetRole(_ string, in *iam.GetRoleInput) (*iam.GetRoleOutput, error) {
	r, ok := e.Roles[aws.StringValue(in.RoleName)]
	if !ok {
		return nil, errors.New(awserrors.ErrorIAMNoSuchEntity)
	}
	return &iam.GetRoleOutput{Role: r}, nil
}

func (e *SystemInstanceRoleEnsurer) CreateRole(accountID string, in *iam.CreateRoleInput) (*iam.CreateRoleOutput, error) {
	e.CreateRoleCalls++
	e.LastTrustDoc = aws.StringValue(in.AssumeRolePolicyDocument)
	e.LastRoleAcct = accountID
	name := aws.StringValue(in.RoleName)
	if _, ok := e.Roles[name]; ok {
		return nil, errors.New(awserrors.ErrorIAMEntityAlreadyExists)
	}
	r := &iam.Role{
		RoleName: aws.String(name),
		Arn:      aws.String("arn:aws:iam::" + accountID + ":role/" + name),
	}
	e.Roles[name] = r
	return &iam.CreateRoleOutput{Role: r}, nil
}

func (e *SystemInstanceRoleEnsurer) PutRolePolicy(_ string, in *iam.PutRolePolicyInput) (*iam.PutRolePolicyOutput, error) {
	e.PolicyCalls = append(e.PolicyCalls, *in)
	e.RolePolicies[aws.StringValue(in.RoleName)] = aws.StringValue(in.PolicyDocument)
	if e.PutRolePolicyErr != nil {
		return nil, e.PutRolePolicyErr
	}
	return &iam.PutRolePolicyOutput{}, nil
}

func (e *SystemInstanceRoleEnsurer) GetInstanceProfile(_ string, in *iam.GetInstanceProfileInput) (*iam.GetInstanceProfileOutput, error) {
	p, ok := e.Profiles[aws.StringValue(in.InstanceProfileName)]
	if !ok {
		return nil, errors.New(awserrors.ErrorIAMNoSuchEntity)
	}
	return &iam.GetInstanceProfileOutput{InstanceProfile: p}, nil
}

func (e *SystemInstanceRoleEnsurer) CreateInstanceProfile(accountID string, in *iam.CreateInstanceProfileInput) (*iam.CreateInstanceProfileOutput, error) {
	e.CreateInstanceProfileCalls++
	e.LastProfileAcct = accountID
	if e.CreateInstanceProfileErr != nil {
		return nil, e.CreateInstanceProfileErr
	}
	name := aws.StringValue(in.InstanceProfileName)
	if _, ok := e.Profiles[name]; ok {
		return nil, errors.New(awserrors.ErrorIAMEntityAlreadyExists)
	}
	arn := "arn:aws:iam::" + accountID + ":instance-profile/" + name
	if e.EmptyInstanceProfileARN {
		arn = ""
	}
	p := &iam.InstanceProfile{
		InstanceProfileName: aws.String(name),
		Arn:                 aws.String(arn),
	}
	e.Profiles[name] = p
	return &iam.CreateInstanceProfileOutput{InstanceProfile: p}, nil
}

func (e *SystemInstanceRoleEnsurer) AddRoleToInstanceProfile(_ string, in *iam.AddRoleToInstanceProfileInput) (*iam.AddRoleToInstanceProfileOutput, error) {
	e.AddRoleToInstanceProfileCalls++
	if e.AddRoleToInstanceProfileErr != nil {
		return nil, e.AddRoleToInstanceProfileErr
	}
	name := aws.StringValue(in.InstanceProfileName)
	p, ok := e.Profiles[name]
	if !ok {
		return nil, errors.New(awserrors.ErrorIAMNoSuchEntity)
	}
	if len(p.Roles) > 0 {
		return nil, errors.New(awserrors.ErrorIAMLimitExceeded)
	}
	p.Roles = []*iam.Role{{RoleName: in.RoleName}}
	return &iam.AddRoleToInstanceProfileOutput{}, nil
}
