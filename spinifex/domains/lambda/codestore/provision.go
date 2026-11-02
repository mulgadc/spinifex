// Package codestore keeps Lambda code copies in the Region's system-account
// bucket, reached only through a short-lived session of the Region's scoped
// code-store role. The composition layer owns every privileged client.
package codestore

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
)

const (
	// OwnerTagKey marks the role and bucket Lambda intends to own. It names the
	// intended owner; any principal able to tag could also set it.
	OwnerTagKey = "spinifex:owner"
	// OwnerTagValue is the owner tag's value.
	OwnerTagValue = "lambda-code-store"
	// PolicyName is the role's only inline policy.
	PolicyName = "lambda-code-store"
)

// TrustPolicy lets only the in-process lambda.amazonaws.com path assume the role.
const TrustPolicy = `{"Version":"2012-10-17","Statement":[{"Effect":"Allow","Principal":{"Service":"lambda.amazonaws.com"},"Action":"sts:AssumeRole"}]}`

var (
	// ErrExists is returned by an admin Create when the resource already exists
	// under the system account.
	ErrExists = errors.New("already exists")
	// ErrNotFound is returned by RoleAdmin.Describe for an absent role.
	ErrNotFound = errors.New("not found")
	// ErrOwnedElsewhere is returned by BucketAdmin.Create when another account
	// owns the bucket name.
	ErrOwnedElsewhere = errors.New("owned by another account")
	// ErrNotOwned is a refusal that persists until an operator resolves it.
	ErrNotOwned = errors.New("lambda code store: existing resource is not Lambda's")
)

// BucketName is the Region's code bucket in the system account.
func BucketName(region string) string {
	return "lambda-code-" + region
}

type policyStatement struct {
	Effect   string   `json:"Effect"`
	Action   []string `json:"Action"`
	Resource string   `json:"Resource"`
}

type policyDocument struct {
	Version   string            `json:"Version"`
	Statement []policyStatement `json:"Statement"`
}

// RolePolicy grants object access to the Region's code bucket and nothing else.
func RolePolicy(region string) (string, error) {
	b := "arn:aws:s3:::" + BucketName(region)
	doc, err := json.Marshal(policyDocument{
		Version: "2012-10-17",
		Statement: []policyStatement{
			{Effect: "Allow", Action: []string{"s3:PutObject", "s3:GetObject", "s3:DeleteObject"}, Resource: b + "/*"},
			{Effect: "Allow", Action: []string{"s3:ListBucket"}, Resource: b},
		},
	})
	if err != nil {
		return "", fmt.Errorf("lambda code store policy: %w", err)
	}
	return string(doc), nil
}

// RoleState is what provisioning reads back about an existing role.
type RoleState struct {
	TrustPolicy      string
	Tags             map[string]string
	InlinePolicies   []string
	AttachedPolicies int
}

// RoleAdmin is bound by the composition layer to the one code-store role.
type RoleAdmin interface {
	Create(trustPolicy string, tags map[string]string) error
	Describe() (*RoleState, error)
	PutPolicy(name, document string) error
}

// BucketAdmin is bound by the composition layer to the one code bucket.
// Create tags the bucket only when it created it, never an existing one.
type BucketAdmin interface {
	Create(ctx context.Context, tags map[string]string) error
	Tags(ctx context.Context) (map[string]string, error)
}

func ownerTags() map[string]string {
	return map[string]string{OwnerTagKey: OwnerTagValue}
}

// Provision creates or verifies the Region's role, policy and bucket. A retry
// completes Lambda's own partial run; any state it cannot attribute to Lambda
// is refused with ErrNotOwned and left untouched.
func Provision(ctx context.Context, region string, role RoleAdmin, bucket BucketAdmin) error {
	if err := ensureRole(role); err != nil {
		return err
	}
	policy, err := RolePolicy(region)
	if err != nil {
		return err
	}
	if err := role.PutPolicy(PolicyName, policy); err != nil {
		return fmt.Errorf("put code-store role policy: %w", err)
	}
	return ensureBucket(ctx, region, bucket)
}

func ensureRole(role RoleAdmin) error {
	err := role.Create(TrustPolicy, ownerTags())
	if err == nil {
		return nil
	}
	if !errors.Is(err, ErrExists) {
		return fmt.Errorf("create code-store role: %w", err)
	}
	st, err := role.Describe()
	if err != nil {
		return fmt.Errorf("describe code-store role: %w", err)
	}
	return checkRole(st)
}

// checkRole accepts only Lambda's own shape: owner tag, exact trust, no attached
// policies, and no inline policy but Lambda's. No inline policy at all is the
// state a crash between CreateRole and PutRolePolicy leaves, so it is retried.
func checkRole(st *RoleState) error {
	switch {
	case st.Tags[OwnerTagKey] != OwnerTagValue:
		return fmt.Errorf("%w: role has no %s=%s tag", ErrNotOwned, OwnerTagKey, OwnerTagValue)
	case !sameJSON(st.TrustPolicy, TrustPolicy):
		return fmt.Errorf("%w: role trust policy differs from the code-store trust policy", ErrNotOwned)
	case st.AttachedPolicies > 0:
		return fmt.Errorf("%w: role has %d attached managed policies", ErrNotOwned, st.AttachedPolicies)
	}
	for _, name := range st.InlinePolicies {
		if name != PolicyName {
			return fmt.Errorf("%w: role has inline policy %q", ErrNotOwned, name)
		}
	}
	return nil
}

func ensureBucket(ctx context.Context, region string, bucket BucketAdmin) error {
	err := bucket.Create(ctx, ownerTags())
	if err == nil {
		return nil
	}
	if errors.Is(err, ErrOwnedElsewhere) {
		return fmt.Errorf("%w: bucket %s is owned by another account", ErrNotOwned, BucketName(region))
	}
	if !errors.Is(err, ErrExists) {
		return fmt.Errorf("create code bucket: %w", err)
	}
	tags, err := bucket.Tags(ctx)
	if err != nil {
		return fmt.Errorf("read code bucket tags: %w", err)
	}
	if tags[OwnerTagKey] != OwnerTagValue {
		return fmt.Errorf("%w: bucket %s exists without the %s=%s tag; an operator must tag or delete it",
			ErrNotOwned, BucketName(region), OwnerTagKey, OwnerTagValue)
	}
	return nil
}

func sameJSON(a, b string) bool {
	var x, y any
	if json.Unmarshal([]byte(a), &x) != nil || json.Unmarshal([]byte(b), &y) != nil {
		return false
	}
	return reflect.DeepEqual(x, y)
}
