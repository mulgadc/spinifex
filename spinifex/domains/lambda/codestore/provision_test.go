package codestore

import (
	"context"
	"errors"
	"slices"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const testRegion = "ap-southeast-2"

type fakeRole struct {
	state    *RoleState
	created  int
	policies map[string]string
}

func (f *fakeRole) Create(trust string, tags map[string]string) error {
	if f.state != nil {
		return ErrExists
	}
	f.created++
	f.state = &RoleState{TrustPolicy: trust, Tags: tags}
	return nil
}

func (f *fakeRole) Describe() (*RoleState, error) {
	if f.state == nil {
		return nil, ErrNotFound
	}
	return f.state, nil
}

func (f *fakeRole) PutPolicy(name, doc string) error {
	if f.policies == nil {
		f.policies = map[string]string{}
	}
	f.policies[name] = doc
	if !slices.Contains(f.state.InlinePolicies, name) {
		f.state.InlinePolicies = append(f.state.InlinePolicies, name)
	}
	return nil
}

type fakeBucket struct {
	exists, foreign bool
	tags            map[string]string
	tagErr          error
	tagWrites       int
}

func (f *fakeBucket) Create(_ context.Context, tags map[string]string) error {
	switch {
	case f.foreign:
		return ErrOwnedElsewhere
	case f.exists:
		return ErrExists
	}
	f.exists = true
	if f.tagErr != nil {
		return f.tagErr
	}
	f.tagWrites++
	f.tags = tags
	return nil
}

func (f *fakeBucket) Tags(context.Context) (map[string]string, error) { return f.tags, nil }

func mustPolicy(t *testing.T) string {
	t.Helper()
	p, err := RolePolicy(testRegion)
	require.NoError(t, err)
	return p
}

func ownRole(inline ...string) *fakeRole {
	return &fakeRole{state: &RoleState{TrustPolicy: TrustPolicy, Tags: ownerTags(), InlinePolicies: inline}}
}

func TestProvision_FreshCreatesTaggedRolePolicyAndBucket(t *testing.T) {
	role, bucket := &fakeRole{}, &fakeBucket{}

	require.NoError(t, Provision(t.Context(), testRegion, role, bucket))

	assert.Equal(t, 1, role.created)
	assert.Equal(t, OwnerTagValue, role.state.Tags[OwnerTagKey])
	assert.JSONEq(t, mustPolicy(t), role.policies[PolicyName])
	assert.Equal(t, OwnerTagValue, bucket.tags[OwnerTagKey])
}

// A crash between CreateRole and PutRolePolicy leaves Lambda's tagged role with
// no inline policy; the next run adds it.
func TestProvision_RetriesOwnRoleWithoutPolicy(t *testing.T) {
	role, bucket := ownRole(), &fakeBucket{}

	require.NoError(t, Provision(t.Context(), testRegion, role, bucket))

	assert.Zero(t, role.created)
	assert.JSONEq(t, mustPolicy(t), role.policies[PolicyName])
}

func TestProvision_RestoresDriftedOwnPolicy(t *testing.T) {
	role, bucket := ownRole(PolicyName), &fakeBucket{}
	role.policies = map[string]string{PolicyName: `{"Version":"2012-10-17","Statement":[{"Effect":"Allow","Action":"s3:*","Resource":"*"}]}`}

	require.NoError(t, Provision(t.Context(), testRegion, role, bucket))
	assert.JSONEq(t, mustPolicy(t), role.policies[PolicyName])
}

func TestProvision_RefusesRoleNotLambdas(t *testing.T) {
	cases := map[string]*RoleState{
		"no owner tag":          {TrustPolicy: TrustPolicy},
		"other trust":           {TrustPolicy: `{"Version":"2012-10-17","Statement":[{"Effect":"Allow","Principal":{"AWS":"arn:aws:iam::000000000000:root"},"Action":"sts:AssumeRole"}]}`, Tags: ownerTags()},
		"attached policy":       {TrustPolicy: TrustPolicy, Tags: ownerTags(), AttachedPolicies: 1},
		"foreign inline policy": {TrustPolicy: TrustPolicy, Tags: ownerTags(), InlinePolicies: []string{PolicyName, "admin"}},
	}
	for name, st := range cases {
		t.Run(name, func(t *testing.T) {
			role, bucket := &fakeRole{state: st}, &fakeBucket{}

			err := Provision(t.Context(), testRegion, role, bucket)

			require.ErrorIs(t, err, ErrNotOwned)
			assert.Nil(t, role.policies, "no policy written to a role that is not Lambda's")
			assert.False(t, bucket.exists, "provisioning stops before the bucket")
		})
	}
}

func TestProvision_AcceptsOwnTaggedBucket(t *testing.T) {
	bucket := &fakeBucket{exists: true, tags: ownerTags()}

	require.NoError(t, Provision(t.Context(), testRegion, &fakeRole{}, bucket))
	assert.Zero(t, bucket.tagWrites)
}

// A crash after CreateBucket and before its tags leaves an untagged bucket that
// cannot be told apart from another system component's, so it is refused.
func TestProvision_RefusesUntaggedBucketAfterPartialCreate(t *testing.T) {
	bucket := &fakeBucket{tagErr: errors.New("connection reset")}
	require.Error(t, Provision(t.Context(), testRegion, &fakeRole{}, bucket))

	bucket.tagErr = nil
	err := Provision(t.Context(), testRegion, &fakeRole{}, bucket)

	require.ErrorIs(t, err, ErrNotOwned)
	assert.Contains(t, err.Error(), "lambda-code-ap-southeast-2 exists without the spinifex:owner=lambda-code-store tag")
	assert.Zero(t, bucket.tagWrites, "an existing bucket is never tagged into use")
}

func TestProvision_RefusesBucketOwnedElsewhere(t *testing.T) {
	err := Provision(t.Context(), testRegion, &fakeRole{}, &fakeBucket{foreign: true})
	require.ErrorIs(t, err, ErrNotOwned)
}

func TestRolePolicy_ScopesToRegionalBucket(t *testing.T) {
	assert.JSONEq(t, `{"Version":"2012-10-17","Statement":[
		{"Effect":"Allow","Action":["s3:PutObject","s3:GetObject","s3:DeleteObject"],"Resource":"arn:aws:s3:::lambda-code-ap-southeast-2/*"},
		{"Effect":"Allow","Action":["s3:ListBucket"],"Resource":"arn:aws:s3:::lambda-code-ap-southeast-2"}]}`,
		mustPolicy(t))
}
