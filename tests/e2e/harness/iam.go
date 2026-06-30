//go:build e2e

package harness

import (
	"testing"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/iam"
	"github.com/aws/aws-sdk-go/service/sts"
	"github.com/stretchr/testify/require"
)

// IAMRoleARN constructs arn:aws:iam::<account>:role/<name>.
func IAMRoleARN(account, name string) string {
	return "arn:aws:iam::" + account + ":role/" + name
}

// IAMPolicyARN builds the canonical policy ARN for a given account + policy
// "key" (a bare name, or "path/name" without the leading slash).
func IAMPolicyARN(account, key string) string {
	return "arn:aws:iam::" + account + ":policy/" + key
}

// IAMGroupARN constructs arn:aws:iam::<account>:group/<name> (default path /).
func IAMGroupARN(account, name string) string {
	return "arn:aws:iam::" + account + ":group/" + name
}

// IAMAccountID returns the active account ID via STS GetCallerIdentity. Unlike
// deriving it from a created IAM user's ARN, this is side-effect-free, so suites
// sharing one account can call it concurrently without minting colliding users.
func IAMAccountID(t *testing.T, c *AWSClient) string {
	t.Helper()
	out, err := c.STS.GetCallerIdentity(&sts.GetCallerIdentityInput{})
	require.NoError(t, err, "sts:GetCallerIdentity")
	require.NotEmpty(t, aws.StringValue(out.Account), "empty account in GetCallerIdentity response")
	return aws.StringValue(out.Account)
}

// IAMDeleteRoleAndProfilesBestEffort tears down every fragment of a role +
// instance-profile graph a suite might have left behind. Each step swallows
// errors so a missing fragment never cascades; usable both as a pre-test sweep
// and as a fixture-teardown cleanup. Order matters: unbind role-from-profile
// before deleting the profile, detach policies before deleting the role.
func IAMDeleteRoleAndProfilesBestEffort(c *AWSClient, roleName string, profileNames []string, policyARNs ...string) {
	for _, p := range profileNames {
		_, _ = c.IAM.RemoveRoleFromInstanceProfile(&iam.RemoveRoleFromInstanceProfileInput{
			InstanceProfileName: aws.String(p),
			RoleName:            aws.String(roleName),
		})
		_, _ = c.IAM.DeleteInstanceProfile(&iam.DeleteInstanceProfileInput{
			InstanceProfileName: aws.String(p),
		})
	}
	for _, arn := range policyARNs {
		_, _ = c.IAM.DetachRolePolicy(&iam.DetachRolePolicyInput{
			RoleName:  aws.String(roleName),
			PolicyArn: aws.String(arn),
		})
	}
	// Defensive: pull any other attached policies so the final DeleteRole isn't
	// blocked by a stray attach from a partial run.
	if attached, err := c.IAM.ListAttachedRolePolicies(&iam.ListAttachedRolePoliciesInput{
		RoleName: aws.String(roleName),
	}); err == nil {
		for _, p := range attached.AttachedPolicies {
			_, _ = c.IAM.DetachRolePolicy(&iam.DetachRolePolicyInput{
				RoleName:  aws.String(roleName),
				PolicyArn: p.PolicyArn,
			})
		}
	}
	_, _ = c.IAM.DeleteRole(&iam.DeleteRoleInput{RoleName: aws.String(roleName)})
}

// IAMDeleteGroupBestEffort tears down a group: removes any members, detaches all
// attached policies, then deletes the group. Each step swallows errors so a
// missing fragment never cascades; usable both as a pre-test sweep and as a
// fixture-teardown cleanup. Order matters: a group cannot be deleted while it
// still has members or attached policies.
func IAMDeleteGroupBestEffort(c *AWSClient, groupName string, memberUsers []string, policyARNs ...string) {
	for _, u := range memberUsers {
		_, _ = c.IAM.RemoveUserFromGroup(&iam.RemoveUserFromGroupInput{
			GroupName: aws.String(groupName),
			UserName:  aws.String(u),
		})
	}
	for _, arn := range policyARNs {
		_, _ = c.IAM.DetachGroupPolicy(&iam.DetachGroupPolicyInput{
			GroupName: aws.String(groupName),
			PolicyArn: aws.String(arn),
		})
	}
	// Defensive: pull any other attached policies so the final DeleteGroup isn't
	// blocked by a stray attach from a partial run.
	if attached, err := c.IAM.ListAttachedGroupPolicies(&iam.ListAttachedGroupPoliciesInput{
		GroupName: aws.String(groupName),
	}); err == nil {
		for _, p := range attached.AttachedPolicies {
			_, _ = c.IAM.DetachGroupPolicy(&iam.DetachGroupPolicyInput{
				GroupName: aws.String(groupName),
				PolicyArn: p.PolicyArn,
			})
		}
	}
	// Defensive: drop any members the caller didn't pass in (partial run could
	// have added one), so the non-empty guard can't block the delete.
	if got, err := c.IAM.GetGroup(&iam.GetGroupInput{GroupName: aws.String(groupName)}); err == nil {
		for _, u := range got.Users {
			_, _ = c.IAM.RemoveUserFromGroup(&iam.RemoveUserFromGroupInput{
				GroupName: aws.String(groupName),
				UserName:  u.UserName,
			})
		}
	}
	_, _ = c.IAM.DeleteGroup(&iam.DeleteGroupInput{GroupName: aws.String(groupName)})
}
