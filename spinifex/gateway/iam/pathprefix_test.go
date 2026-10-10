package gateway_iam_test

import (
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/iam"
	"github.com/mulgadc/spinifex/spinifex/foundation/aws/errors"
	gateway_iam "github.com/mulgadc/spinifex/spinifex/gateway/iam"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The messages are the ones AWS returned for the same PathPrefix.
const (
	badPathPrefix  = "The specified value for pathPrefix is invalid. It must begin with the / character and contain only alphanumeric characters and/or / characters."
	longPathPrefix = "1 validation error detected: Value at 'pathPrefix' failed to satisfy constraint: Member must have length less than or equal to 512"
)

// Each list is refused before the service runs; the stub panics if called.
func TestListCalls_RejectPathPrefixBeforeListing(t *testing.T) {
	lists := map[string]func(prefix string) error{
		"ListUsers": func(p string) error {
			_, err := gateway_iam.ListUsers(testAccountID, &iam.ListUsersInput{PathPrefix: aws.String(p)}, &listStub{})
			return err
		},
		"ListRoles": func(p string) error {
			_, err := gateway_iam.ListRoles(testAccountID, &iam.ListRolesInput{PathPrefix: aws.String(p)}, &listStub{})
			return err
		},
		"ListGroups": func(p string) error {
			_, err := gateway_iam.ListGroups(testAccountID, &iam.ListGroupsInput{PathPrefix: aws.String(p)}, &listStub{})
			return err
		},
		"ListInstanceProfiles": func(p string) error {
			_, err := gateway_iam.ListInstanceProfiles(testAccountID, &iam.ListInstanceProfilesInput{PathPrefix: aws.String(p)}, &listStub{})
			return err
		},
		"ListPolicies": func(p string) error {
			_, err := gateway_iam.ListPolicies(testAccountID, &iam.ListPoliciesInput{PathPrefix: aws.String(p)}, &listStub{})
			return err
		},
		"ListEntitiesForPolicy": func(p string) error {
			_, err := gateway_iam.ListEntitiesForPolicy(testAccountID, &iam.ListEntitiesForPolicyInput{
				PolicyArn: aws.String("arn:aws:iam::aws:policy/ReadOnlyAccess"), PathPrefix: aws.String(p),
			}, &listStub{})
			return err
		},
		"ListAttachedUserPolicies": func(p string) error {
			_, err := gateway_iam.ListAttachedUserPolicies(testAccountID, &iam.ListAttachedUserPoliciesInput{UserName: aws.String("u"), PathPrefix: aws.String(p)}, &listStub{})
			return err
		},
		"ListAttachedRolePolicies": func(p string) error {
			_, err := gateway_iam.ListAttachedRolePolicies(testAccountID, &iam.ListAttachedRolePoliciesInput{RoleName: aws.String("r"), PathPrefix: aws.String(p)}, &listStub{})
			return err
		},
		"ListAttachedGroupPolicies": func(p string) error {
			_, err := gateway_iam.ListAttachedGroupPolicies(testAccountID, &iam.ListAttachedGroupPoliciesInput{GroupName: aws.String("g"), PathPrefix: aws.String(p)}, &listStub{})
			return err
		},
	}
	cases := []struct {
		name    string
		prefix  string
		wantMsg string
	}{
		{"empty", "", badPathPrefix},
		{"no leading slash", "awsdiff", badPathPrefix},
		{"space", "/bad path/", badPathPrefix},
		{"over 512 characters", "/" + strings.Repeat("a", 511) + "/", longPathPrefix},
	}
	for op, list := range lists {
		for _, tc := range cases {
			t.Run(op+"/"+tc.name, func(t *testing.T) {
				err := list(tc.prefix)
				require.Error(t, err)
				code, msg, ok := awserrors.ResolveErrorDetail(err)
				require.True(t, ok, "error must carry a registered code: %v", err)
				assert.Equal(t, awserrors.ErrorValidationError, code)
				assert.Equal(t, tc.wantMsg, msg)
			})
		}
	}
}

func TestListUsers_PathPrefixAccepted(t *testing.T) {
	for _, prefix := range []string{"/", "/~/", "/team", "/" + strings.Repeat("a", 511)} {
		svc := &listStub{users: []string{"a"}}
		_, err := gateway_iam.ListUsers(testAccountID, &iam.ListUsersInput{PathPrefix: aws.String(prefix)}, svc)
		require.NoError(t, err, prefix)
		assert.Equal(t, 1, svc.calls)
	}
}

// Policy paths end in a slash, so a prefix without one breaks the pattern
// before its length is checked.
func TestListPolicies_PathPrefixPatternBeforeLength(t *testing.T) {
	_, err := gateway_iam.ListPolicies(testAccountID, &iam.ListPoliciesInput{PathPrefix: aws.String("/" + strings.Repeat("a", 512))}, &listStub{})
	_, msg, ok := awserrors.ResolveErrorDetail(err)
	require.True(t, ok, "error must carry a registered code: %v", err)
	assert.Equal(t, badPathPrefix, msg)
}

// The messages are the ones AWS returned for a 19- and a 2049-character ARN.
func TestListEntitiesForPolicy_PolicyArnLength(t *testing.T) {
	cases := map[string]string{
		"arn:aws:iam::aws:x": "1 validation error detected: Value at 'policyArn' failed to satisfy constraint: Member must have length greater than or equal to 20",
		"arn:aws:iam::aws:policy/" + strings.Repeat("p", 2025): "1 validation error detected: Value at 'policyArn' failed to satisfy constraint: Member must have length less than or equal to 2048",
	}
	for arn, want := range cases {
		_, err := gateway_iam.ListEntitiesForPolicy(testAccountID, &iam.ListEntitiesForPolicyInput{PolicyArn: aws.String(arn)}, &listStub{})
		code, msg, ok := awserrors.ResolveErrorDetail(err)
		require.True(t, ok, "error must carry a registered code: %v", err)
		assert.Equal(t, awserrors.ErrorValidationError, code)
		assert.Equal(t, want, msg)
	}
}
