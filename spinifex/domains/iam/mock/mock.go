// Package mock provides an in-memory implementation of
// iamdomain.SystemInstanceRoleEnsurer for tests.
package mock

import (
	"errors"
	"sync"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/iam"
	iamdomain "github.com/mulgadc/spinifex/spinifex/domains/iam"
	"github.com/mulgadc/spinifex/spinifex/foundation/aws/errors"
)

// SystemInstanceRoleEnsurer is an in-memory
// iamdomain.SystemInstanceRoleEnsurer. Roles and instance profiles are
// stored keyed by name; Get* miss with NoSuchEntity until the matching
// Create* call, and a repeat Create* reports EntityAlreadyExists, mirroring
// the live IAM API. Every method is safe for concurrent use, so callers that
// converge on one profile from several goroutines can be exercised under the
// race detector; exported fields must only be touched while no call is in
// flight.
type SystemInstanceRoleEnsurer struct {
	mu sync.Mutex

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
	TagInstanceProfileCalls       int
	RemoveRoleCalls               int
	DeleteInstanceProfileCalls    int

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
	// RemoveRoleFromInstanceProfileErr and DeleteInstanceProfileErr, when set,
	// are returned by the matching call instead of acting.
	RemoveRoleFromInstanceProfileErr error
	DeleteInstanceProfileErr         error
	// EmptyInstanceProfileARN makes CreateInstanceProfile return an empty
	// ARN, for exercising callers that must reject it.
	EmptyInstanceProfileARN bool
}

var _ iamdomain.SystemInstanceRoleEnsurer = (*SystemInstanceRoleEnsurer)(nil)

// New returns an empty SystemInstanceRoleEnsurer ready for use.
func New() *SystemInstanceRoleEnsurer {
	return &SystemInstanceRoleEnsurer{
		Roles:        make(map[string]*iam.Role),
		Profiles:     make(map[string]*iam.InstanceProfile),
		RolePolicies: make(map[string]string),
	}
}

// copyRole and copyProfile hand callers a snapshot rather than a pointer into
// the mock's state, so a concurrent AddRoleToInstanceProfile cannot mutate a
// row another goroutine is still reading.
func copyRole(r *iam.Role) *iam.Role {
	c := *r
	return &c
}

func copyProfile(p *iam.InstanceProfile) *iam.InstanceProfile {
	c := *p
	c.Roles = append([]*iam.Role(nil), p.Roles...)
	c.Tags = append([]*iam.Tag(nil), p.Tags...)
	return &c
}

func (e *SystemInstanceRoleEnsurer) GetRole(_ string, in *iam.GetRoleInput) (*iam.GetRoleOutput, error) {
	e.mu.Lock()
	defer e.mu.Unlock()

	r, ok := e.Roles[aws.StringValue(in.RoleName)]
	if !ok {
		return nil, awserrors.Errorf(awserrors.ErrorIAMNoSuchEntity, "The role with name %s cannot be found.", aws.StringValue(in.RoleName))
	}
	return &iam.GetRoleOutput{Role: copyRole(r)}, nil
}

func (e *SystemInstanceRoleEnsurer) CreateRole(accountID string, in *iam.CreateRoleInput) (*iam.CreateRoleOutput, error) {
	e.mu.Lock()
	defer e.mu.Unlock()

	e.CreateRoleCalls++
	e.LastTrustDoc = aws.StringValue(in.AssumeRolePolicyDocument)
	e.LastRoleAcct = accountID
	name := aws.StringValue(in.RoleName)
	if _, ok := e.Roles[name]; ok {
		return nil, awserrors.Errorf(awserrors.ErrorIAMEntityAlreadyExists, "Role with name %s already exists.", name)
	}
	r := &iam.Role{
		RoleName: aws.String(name),
		Arn:      aws.String("arn:aws:iam::" + accountID + ":role/" + name),
	}
	e.Roles[name] = r
	return &iam.CreateRoleOutput{Role: copyRole(r)}, nil
}

func (e *SystemInstanceRoleEnsurer) PutRolePolicy(_ string, in *iam.PutRolePolicyInput) (*iam.PutRolePolicyOutput, error) {
	e.mu.Lock()
	defer e.mu.Unlock()

	e.PolicyCalls = append(e.PolicyCalls, *in)
	e.RolePolicies[aws.StringValue(in.RoleName)] = aws.StringValue(in.PolicyDocument)
	if e.PutRolePolicyErr != nil {
		return nil, e.PutRolePolicyErr
	}
	return &iam.PutRolePolicyOutput{}, nil
}

func (e *SystemInstanceRoleEnsurer) GetInstanceProfile(_ string, in *iam.GetInstanceProfileInput) (*iam.GetInstanceProfileOutput, error) {
	e.mu.Lock()
	defer e.mu.Unlock()

	p, ok := e.Profiles[aws.StringValue(in.InstanceProfileName)]
	if !ok {
		return nil, awserrors.Errorf(awserrors.ErrorIAMNoSuchEntity, "Instance Profile %s cannot be found.", aws.StringValue(in.InstanceProfileName))
	}
	return &iam.GetInstanceProfileOutput{InstanceProfile: copyProfile(p)}, nil
}

func (e *SystemInstanceRoleEnsurer) CreateInstanceProfile(accountID string, in *iam.CreateInstanceProfileInput) (*iam.CreateInstanceProfileOutput, error) {
	e.mu.Lock()
	defer e.mu.Unlock()

	e.CreateInstanceProfileCalls++
	e.LastProfileAcct = accountID
	if e.CreateInstanceProfileErr != nil {
		return nil, e.CreateInstanceProfileErr
	}
	name := aws.StringValue(in.InstanceProfileName)
	if _, ok := e.Profiles[name]; ok {
		return nil, awserrors.Errorf(awserrors.ErrorIAMEntityAlreadyExists, "Instance Profile %s already exists.", name)
	}
	arn := "arn:aws:iam::" + accountID + ":instance-profile/" + name
	if e.EmptyInstanceProfileARN {
		arn = ""
	}
	p := &iam.InstanceProfile{
		InstanceProfileName: aws.String(name),
		Arn:                 aws.String(arn),
		Tags:                append([]*iam.Tag(nil), in.Tags...),
	}
	e.Profiles[name] = p
	return &iam.CreateInstanceProfileOutput{InstanceProfile: copyProfile(p)}, nil
}

func (e *SystemInstanceRoleEnsurer) AddRoleToInstanceProfile(_ string, in *iam.AddRoleToInstanceProfileInput) (*iam.AddRoleToInstanceProfileOutput, error) {
	e.mu.Lock()
	defer e.mu.Unlock()

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
		return nil, awserrors.Errorf(awserrors.ErrorIAMLimitExceeded, "Cannot exceed quota for InstanceSessionsPerInstanceProfile: 1")
	}
	p.Roles = []*iam.Role{{RoleName: in.RoleName}}
	return &iam.AddRoleToInstanceProfileOutput{}, nil
}

func (e *SystemInstanceRoleEnsurer) TagInstanceProfile(_ string, in *iam.TagInstanceProfileInput) (*iam.TagInstanceProfileOutput, error) {
	e.mu.Lock()
	defer e.mu.Unlock()

	e.TagInstanceProfileCalls++
	p, ok := e.Profiles[aws.StringValue(in.InstanceProfileName)]
	if !ok {
		return nil, awserrors.Errorf(awserrors.ErrorIAMNoSuchEntity, "Instance Profile %s cannot be found.", aws.StringValue(in.InstanceProfileName))
	}
	for _, t := range in.Tags {
		replaced := false
		for i, have := range p.Tags {
			if aws.StringValue(have.Key) == aws.StringValue(t.Key) {
				p.Tags[i] = t
				replaced = true
				break
			}
		}
		if !replaced {
			p.Tags = append(p.Tags, t)
		}
	}
	return &iam.TagInstanceProfileOutput{}, nil
}

func (e *SystemInstanceRoleEnsurer) UntagInstanceProfile(_ string, in *iam.UntagInstanceProfileInput) (*iam.UntagInstanceProfileOutput, error) {
	e.mu.Lock()
	defer e.mu.Unlock()

	p, ok := e.Profiles[aws.StringValue(in.InstanceProfileName)]
	if !ok {
		return nil, awserrors.Errorf(awserrors.ErrorIAMNoSuchEntity, "Instance Profile %s cannot be found.", aws.StringValue(in.InstanceProfileName))
	}
	drop := map[string]bool{}
	for _, k := range in.TagKeys {
		drop[aws.StringValue(k)] = true
	}
	kept := p.Tags[:0:0]
	for _, t := range p.Tags {
		if !drop[aws.StringValue(t.Key)] {
			kept = append(kept, t)
		}
	}
	p.Tags = kept
	return &iam.UntagInstanceProfileOutput{}, nil
}

func (e *SystemInstanceRoleEnsurer) ListInstanceProfileTags(_ string, in *iam.ListInstanceProfileTagsInput) (*iam.ListInstanceProfileTagsOutput, error) {
	e.mu.Lock()
	defer e.mu.Unlock()

	p, ok := e.Profiles[aws.StringValue(in.InstanceProfileName)]
	if !ok {
		return nil, awserrors.Errorf(awserrors.ErrorIAMNoSuchEntity, "Instance Profile %s cannot be found.", aws.StringValue(in.InstanceProfileName))
	}
	return &iam.ListInstanceProfileTagsOutput{Tags: append([]*iam.Tag(nil), p.Tags...)}, nil
}

func (e *SystemInstanceRoleEnsurer) RemoveRoleFromInstanceProfile(_ string, in *iam.RemoveRoleFromInstanceProfileInput) (*iam.RemoveRoleFromInstanceProfileOutput, error) {
	e.mu.Lock()
	defer e.mu.Unlock()

	e.RemoveRoleCalls++
	if e.RemoveRoleFromInstanceProfileErr != nil {
		return nil, e.RemoveRoleFromInstanceProfileErr
	}
	p, ok := e.Profiles[aws.StringValue(in.InstanceProfileName)]
	if !ok {
		return nil, awserrors.Errorf(awserrors.ErrorIAMNoSuchEntity, "Instance Profile %s cannot be found.", aws.StringValue(in.InstanceProfileName))
	}
	if len(p.Roles) == 0 || aws.StringValue(p.Roles[0].RoleName) != aws.StringValue(in.RoleName) {
		return nil, awserrors.Errorf(awserrors.ErrorIAMNoSuchEntity, "The role with name %s cannot be found.", aws.StringValue(in.RoleName))
	}
	p.Roles = nil
	return &iam.RemoveRoleFromInstanceProfileOutput{}, nil
}

// DeleteInstanceProfile refuses while a role is attached, as IAM does.
func (e *SystemInstanceRoleEnsurer) DeleteInstanceProfile(_ string, in *iam.DeleteInstanceProfileInput) (*iam.DeleteInstanceProfileOutput, error) {
	e.mu.Lock()
	defer e.mu.Unlock()

	e.DeleteInstanceProfileCalls++
	if e.DeleteInstanceProfileErr != nil {
		return nil, e.DeleteInstanceProfileErr
	}
	name := aws.StringValue(in.InstanceProfileName)
	p, ok := e.Profiles[name]
	if !ok {
		return nil, awserrors.Errorf(awserrors.ErrorIAMNoSuchEntity, "Instance Profile %s cannot be found.", name)
	}
	if len(p.Roles) > 0 {
		return nil, awserrors.Errorf(awserrors.ErrorIAMDeleteConflict, "Cannot delete entity, must remove roles from instance profile first.")
	}
	delete(e.Profiles, name)
	return &iam.DeleteInstanceProfileOutput{}, nil
}
