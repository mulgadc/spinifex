//go:build e2e

package iam

import (
	"testing"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/aws/aws-sdk-go/service/iam"
	"github.com/mulgadc/spinifex/tests/e2e/harness"
	"github.com/stretchr/testify/require"
)

const (
	usrInlineEnforceUser = "usr-e2e-inl-enf-user"
	usrInlineEnforceName = "usr-e2e-inl-enf-describe-regions"

	// A single-action grant so the positive case proves the user's own inline
	// document was resolved and evaluated — not a blanket allow.
	usrInlinePolicyDoc = `{"Version":"2012-10-17","Statement":[{"Effect":"Allow","Action":"ec2:DescribeRegions","Resource":"*"}]}`
)

// runIAMUserInlineEnforcement proves the user-inline linchpin: a policy embedded
// directly in a user record grants that user its permission with no group or
// managed attachment involved. A user with its own access key is denied a guarded
// action; once an inline policy granting that action is put on the user, the same
// live credentials are allowed (policies resolved per request); after the inline
// policy is deleted the action is denied again, isolating the user's own inline
// document as the sole grant source. Mirrors runIAMGroupInlineEnforcement without
// the group indirection.
func runIAMUserInlineEnforcement(t *testing.T, fix *Fixture) {
	harness.Phase(t, "Single — IAM User inline-policy authorization enforcement")

	// No group or managed policy — the inline doc lives on the user record, so the
	// best-effort sweep clears it via ListUserPolicies/DeleteUserPolicy.
	sweep := func() {
		iamDeleteUserBestEffort(fix, usrInlineEnforceUser)
	}
	sweep()
	fix.Harness.RegisterCleanup(sweep)

	// User with its own static credentials.
	harness.Step(t, "create-user %q + access key", usrInlineEnforceUser)
	_, err := fix.AWS.IAM.CreateUser(&iam.CreateUserInput{UserName: aws.String(usrInlineEnforceUser)})
	require.NoError(t, err, "create-user")
	key, err := fix.AWS.IAM.CreateAccessKey(&iam.CreateAccessKeyInput{UserName: aws.String(usrInlineEnforceUser)})
	require.NoError(t, err, "create-access-key")
	userCli := harness.NewAWSClientWithCreds(t, fix.Env,
		aws.StringValue(key.AccessKey.AccessKeyId),
		aws.StringValue(key.AccessKey.SecretAccessKey))

	// No grant anywhere yet: the active key authenticates, then default-denies.
	harness.Step(t, "describe-regions with no grant (expect AccessDenied)")
	harness.ExpectError(t, "AccessDenied", func() error {
		_, e := userCli.EC2.DescribeRegions(&ec2.DescribeRegionsInput{})
		return e
	})

	// Put an inline policy granting ec2:DescribeRegions directly on the user.
	harness.Step(t, "put-user-policy %q (grants ec2:DescribeRegions)", usrInlineEnforceName)
	_, err = fix.AWS.IAM.PutUserPolicy(&iam.PutUserPolicyInput{
		UserName:       aws.String(usrInlineEnforceUser),
		PolicyName:     aws.String(usrInlineEnforceName),
		PolicyDocument: aws.String(usrInlinePolicyDoc),
	})
	require.NoError(t, err, "put-user-policy")

	// Same live credentials are now allowed — the user's own inline grant applies.
	harness.Step(t, "describe-regions after inline user grant (expect success)")
	_, err = userCli.EC2.DescribeRegions(&ec2.DescribeRegionsInput{})
	require.NoError(t, err, "describe-regions must be allowed once the user inline policy grants it")

	// Delete the inline policy — the grant must evaporate, isolating the inline
	// document as the sole grant source.
	harness.Step(t, "delete-user-policy %s", usrInlineEnforceName)
	_, err = fix.AWS.IAM.DeleteUserPolicy(&iam.DeleteUserPolicyInput{
		UserName:   aws.String(usrInlineEnforceUser),
		PolicyName: aws.String(usrInlineEnforceName),
	})
	require.NoError(t, err, "delete-user-policy")

	harness.Step(t, "describe-regions after inline policy removed (expect AccessDenied)")
	harness.ExpectError(t, "AccessDenied", func() error {
		_, e := userCli.EC2.DescribeRegions(&ec2.DescribeRegionsInput{})
		return e
	})
}
